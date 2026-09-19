package sqlite

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/epmon-dev/epmon/internal/store"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// dayStart returns noon UTC of now-d days ago (noon avoids DST edges).
func dayStart(now time.Time, d int) time.Time {
	y, m, day := now.AddDate(0, 0, -d).UTC().Date()
	return time.Date(y, m, day, 12, 0, 0, 0, time.UTC)
}

func TestChecksAndHistory(t *testing.T) {
	st := openTest(t)
	ctx := t.Context()
	now := time.Now().UTC()

	// Yesterday: 10 up. Today: 8 up + 2 down (=20% -> down day).
	record := func(ts time.Time, up bool, code int, errStr string) {
		t.Helper()
		c := store.Check{ServiceID: "web", TS: ts.Unix(), Up: up, LatencyMs: 5, StatusCode: code, Error: errStr}
		if err := st.RecordCheck(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		record(dayStart(now, 1).Add(time.Duration(i)*time.Minute), true, 200, "")
	}
	for i := 0; i < 8; i++ {
		record(dayStart(now, 0).Add(time.Duration(i)*time.Minute), true, 200, "")
	}
	for i := 0; i < 2; i++ {
		record(dayStart(now, 0).Add(time.Duration(60+i)*time.Minute), false, 500, "status: got 500")
	}

	last, err := st.LastCheck(ctx, "web")
	if err != nil || last == nil || last.Up {
		t.Fatalf("LastCheck should be the newest (down): %+v %v", last, err)
	}
	if none, err := st.LastCheck(ctx, "missing"); err != nil || none != nil {
		t.Fatalf("LastCheck missing = %v, %v", none, err)
	}

	hist, err := st.DailyHistory(ctx, "web", 3, now)
	if err != nil {
		t.Fatal(err)
	}
	byDate := map[string]store.DayBucket{}
	for _, b := range hist {
		byDate[b.Date] = b
	}
	yesterday := dayStart(now, 1).Format("2006-01-02")
	today := dayStart(now, 0).Format("2006-01-02")
	twoAgo := dayStart(now, 2).Format("2006-01-02")
	if b := byDate[yesterday]; b.Up == nil || !*b.Up || b.Checks != 10 {
		t.Errorf("yesterday = %+v, want up/10", b)
	}
	if b := byDate[today]; b.Up == nil || *b.Up || b.Checks != 10 {
		t.Errorf("today = %+v, want down/10", b)
	}
	if b := byDate[twoAgo]; b.Up != nil || b.Checks != 0 {
		t.Errorf("two days ago = %+v, want unknown", b)
	}

	recent, err := st.RecentChecks(ctx, "web", 5)
	if err != nil || len(recent) != 5 {
		t.Fatalf("RecentChecks = %d, %v", len(recent), err)
	}
}

// TestPurgeUsesRollingWindow anchors purge rows to now with relative
// offsets. Purge deletes ts < now-24h*retention regardless of wall-clock
// hour, so calendar-day fixtures (e.g. yesterday noon) make the assertion
// time-of-day dependent — they fail before ~12:09 UTC. Relative offsets
// pass identically at every hour.
func TestPurgeUsesRollingWindow(t *testing.T) {
	st := openTest(t)
	ctx := t.Context()
	now := time.Now().UTC()

	old := now.Add(-25 * time.Hour).Unix()
	fresh := now.Add(-time.Hour).Unix()
	for i := 0; i < 3; i++ {
		c := store.Check{ServiceID: "web", TS: old + int64(i), Up: true, LatencyMs: 5, StatusCode: 200}
		if err := st.RecordCheck(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RecordCheck(ctx, store.Check{ServiceID: "web", TS: fresh, Up: true, LatencyMs: 5, StatusCode: 200}); err != nil {
		t.Fatal(err)
	}

	n, err := st.Purge(ctx, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("Purge removed %d, want 3 (rows older than 24h)", n)
	}
	remaining, err := st.RecentChecks(ctx, "web", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].TS != fresh {
		t.Errorf("Purge kept %+v, want only the fresh row", remaining)
	}
}

func TestIncidents(t *testing.T) {
	st := openTest(t)
	ctx := t.Context()
	now := time.Now().UTC()

	id, err := st.CreateIncident(ctx, "web", "Elevated latency", "minor", now)
	if err != nil || id == 0 {
		t.Fatalf("CreateIncident = %d, %v", id, err)
	}
	if err := st.AddIncidentUpdate(ctx, id, "Investigating.", now); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateIncident(ctx, id, "", "", "resolved", now); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetIncident(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("GetIncident = %+v, %v", got, err)
	}
	if got.State != "resolved" || len(got.Updates) != 1 || got.Updates[0].Text != "Investigating." {
		t.Errorf("incident mismatch: %+v", got)
	}
	if none, err := st.GetIncident(ctx, 9999); err != nil || none != nil {
		t.Errorf("GetIncident missing = %+v, %v", none, err)
	}
	if err := st.UpdateIncident(ctx, 9999, "", "", "resolved", now); err != store.ErrNotFound {
		t.Errorf("UpdateIncident missing = %v, want ErrNotFound", err)
	}
	if err := st.AddIncidentUpdate(ctx, 9999, "x", now); err != store.ErrNotFound {
		t.Errorf("AddIncidentUpdate missing = %v, want ErrNotFound", err)
	}

	all, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil || len(all) != 1 {
		t.Fatalf("ListIncidents = %d, %v", len(all), err)
	}
	open, err := st.ListIncidents(ctx, store.IncidentFilter{State: "investigating"})
	if err != nil || len(open) != 0 {
		t.Errorf("resolved incident should not list as investigating: %d", len(open))
	}
	byService, err := st.ListIncidents(ctx, store.IncidentFilter{ServiceID: "web"})
	if err != nil || len(byService) != 1 {
		t.Errorf("filter by service = %d, %v", len(byService), err)
	}
	byOther, err := st.ListIncidents(ctx, store.IncidentFilter{ServiceID: "other"})
	if err != nil || len(byOther) != 0 {
		t.Errorf("filter by other service = %d, %v", len(byOther), err)
	}
}

func TestIncidentUpdateLimits(t *testing.T) {
	st := openTest(t)
	ctx := t.Context()
	now := time.Now().UTC()

	id, err := st.CreateIncident(ctx, "web", "Limits", "minor", now)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		text string
		ok   bool
	}{
		"empty":                {"", false},
		"one rune":             {"x", true},
		"2000 runes":           {strings.Repeat("a", 2000), true},
		"2001 runes":           {strings.Repeat("a", 2001), false},
		"2000 multibyte runes": {strings.Repeat("é", 2000), true},
		"2001 multibyte runes": {strings.Repeat("é", 2001), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := st.AddIncidentUpdate(ctx, id, tc.text, now)
			if tc.ok && err != nil {
				t.Errorf("AddIncidentUpdate = %v, want nil", err)
			}
			if !tc.ok && !errors.Is(err, store.ErrInvalid) {
				t.Errorf("AddIncidentUpdate = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestIncidentSeverityCritical(t *testing.T) {
	st := openTest(t)
	ctx := t.Context()
	now := time.Now().UTC()

	id, err := st.CreateIncident(ctx, "web", "Outage", "critical", now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetIncident(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("GetIncident = %+v, %v", got, err)
	}
	if got.Severity != "critical" {
		t.Errorf("severity = %q, want critical (must not downgrade to minor)", got.Severity)
	}
	// Unknown severities still normalize to minor at the store boundary.
	other, err := st.CreateIncident(ctx, "web", "Weird", "urgent", now)
	if err != nil {
		t.Fatal(err)
	}
	gotOther, err := st.GetIncident(ctx, other)
	if err != nil || gotOther == nil {
		t.Fatalf("GetIncident = %+v, %v", gotOther, err)
	}
	if gotOther.Severity != "minor" {
		t.Errorf("severity = %q, want minor for unknown input", gotOther.Severity)
	}
}

// TestUpdateIncidentConcurrentFields patches different fields from two
// goroutines: with a read-modify-write implementation the loser's full-row
// rewrite intermittently clobbers the winner's field.
func TestUpdateIncidentConcurrentFields(t *testing.T) {
	st := openTest(t)
	ctx := t.Context()
	now := time.Now().UTC()

	for i := 0; i < 50; i++ {
		id, err := st.CreateIncident(ctx, "web", "Race", "minor", now)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = st.UpdateIncident(ctx, id, "T", "", "", now)
		}()
		go func() {
			defer wg.Done()
			_ = st.UpdateIncident(ctx, id, "", "", "monitoring", now)
		}()
		wg.Wait()
		got, err := st.GetIncident(ctx, id)
		if err != nil || got == nil {
			t.Fatalf("iteration %d: GetIncident = %+v, %v", i, got, err)
		}
		if got.Title != "T" || got.State != "monitoring" {
			t.Fatalf("iteration %d: lost update, got title=%q state=%q", i, got.Title, got.State)
		}
	}
}

// TestUpdateIncidentNoopUpdate asserts a same-value patch still matches
// the row (no ErrNotFound) since SQLite counts matched rows.
func TestUpdateIncidentNoopUpdate(t *testing.T) {
	st := openTest(t)
	ctx := t.Context()
	now := time.Now().UTC()

	id, err := st.CreateIncident(ctx, "web", "Noop", "minor", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateIncident(ctx, id, "", "", "investigating", now); err != nil {
		t.Errorf("same-value update = %v, want nil", err)
	}
}

// TestIncidentStateMachine table-tests the forward-only machine:
// investigating -> monitoring -> resolved. Same-state and forward skips
// are legal; every backward move is rejected with TransitionError.
func TestIncidentStateMachine(t *testing.T) {
	forward := []struct {
		from, to string
		wantErr  bool
	}{
		{"investigating", "investigating", false},
		{"investigating", "monitoring", false},
		{"investigating", "resolved", false}, // skip forward stays legal
		{"monitoring", "monitoring", false},
		{"monitoring", "resolved", false},
		{"monitoring", "investigating", true},
		{"resolved", "resolved", false},
		{"resolved", "monitoring", true},
		{"resolved", "investigating", true},
	}
	for _, tc := range forward {
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			st := openTest(t)
			ctx := t.Context()
			now := time.Now().UTC()

			id, err := st.CreateIncident(ctx, "web", "State", "minor", now)
			if err != nil {
				t.Fatal(err)
			}
			// Walk legally to the `from` state first.
			for _, s := range []string{"investigating", "monitoring", "resolved"} {
				if err := st.UpdateIncident(ctx, id, "", "", s, now); err != nil {
					t.Fatalf("setup move to %s: %v", s, err)
				}
				if s == tc.from {
					break
				}
			}
			err = st.UpdateIncident(ctx, id, "", "", tc.to, now)
			if !tc.wantErr && err != nil {
				t.Errorf("move %s->%s = %v, want nil", tc.from, tc.to, err)
			}
			var terr *store.TransitionError
			if tc.wantErr {
				if !errors.As(err, &terr) {
					t.Errorf("move %s->%s = %v, want TransitionError", tc.from, tc.to, err)
				} else if terr.From != tc.from || terr.To != tc.to {
					t.Errorf("TransitionError = %+v, want from=%q to=%q", terr, tc.from, tc.to)
				}
				// Rejected moves leave the stored state untouched.
				got, _ := st.GetIncident(ctx, id)
				if got == nil || got.State != tc.from {
					t.Errorf("state after rejected move = %+v, want %q", got, tc.from)
				}
			}
		})
	}
}

func TestListIncidentsPaging(t *testing.T) {
	st := openTest(t)
	ctx := t.Context()
	now := time.Now().UTC()

	for _, title := range []string{"T1", "T2", "T3", "T4", "T5"} {
		id, err := st.CreateIncident(ctx, "web", title, "minor", now)
		if err != nil {
			t.Fatal(err)
		}
		if title == "T5" {
			if err := st.AddIncidentUpdate(ctx, id, "note", now); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n, err := st.CountIncidents(ctx, store.IncidentFilter{}); err != nil || n != 5 {
		t.Fatalf("CountIncidents = %d, %v; want 5", n, err)
	}
	page, err := st.ListIncidents(ctx, store.IncidentFilter{Limit: 2, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].Title != "T4" || page[1].Title != "T3" {
		t.Fatalf("page = %+v, want [T4 T3] newest-first", page)
	}
	first, err := st.ListIncidents(ctx, store.IncidentFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].Title != "T5" || len(first[0].Updates) != 1 {
		t.Fatalf("first page = %+v, want T5 with its thread", first)
	}
	// Unbounded direct-store use keeps working.
	all, err := st.ListIncidents(ctx, store.IncidentFilter{})
	if err != nil || len(all) != 5 {
		t.Fatalf("unbounded list = %d, %v; want 5", len(all), err)
	}
}

func TestSyncServices(t *testing.T) {
	st := openTest(t)
	ctx := t.Context()
	now := time.Now().UTC()
	metas := []store.ServiceMeta{{ID: "web", Name: "Web", URL: "https://example.com"}}
	if err := st.SyncServices(ctx, metas, now); err != nil {
		t.Fatal(err)
	}
	// Idempotent re-sync with a rename.
	metas[0].Name = "Website"
	if err := st.SyncServices(ctx, metas, now); err != nil {
		t.Fatal(err)
	}
}
