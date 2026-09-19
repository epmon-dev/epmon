package sqlite

import (
	"path/filepath"
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

	n, err := st.Purge(ctx, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Errorf("Purge removed %d, want 10 (yesterday's)", n)
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
