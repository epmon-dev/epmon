package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// contractCase is one request against the live handler plus the spec
// operation it must satisfy. specPath uses the templated form from
// openapi.yaml (e.g. /api/v1/incidents/{id}).
type contractCase struct {
	method   string
	path     string
	specPath string
	body     string
}

// TestAPIContractFromSpec loads the embedded OpenAPI contract and asserts,
// for every registered route: the spec documents the method, every served
// status code is a documented response, and every 4xx/5xx carries the
// standard error envelope with a vocabulary code. It also asserts the
// reverse: every spec operation has at least one exercising request, so
// contract additions without coverage fail here.
func TestAPIContractFromSpec(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal(openAPIYAML, &doc); err != nil {
		t.Fatalf("embedded spec is not valid YAML: %v", err)
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatal("spec has no paths")
	}
	errCode := func() map[string]bool {
		out := map[string]bool{}
		schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
		errSchema := schemas["Error"].(map[string]any)
		props := errSchema["properties"].(map[string]any)
		errObj := props["error"].(map[string]any)
		codeProp := errObj["properties"].(map[string]any)["code"].(map[string]any)
		for _, c := range codeProp["enum"].([]any) {
			out[c.(string)] = true
		}
		return out
	}()
	if len(errCode) == 0 {
		t.Fatal("spec Error.code enum is empty")
	}

	srv, _ := testServer(t)
	h := srv.Handler()

	// NOTE: incident id 1 is the first row of the fresh test database;
	// keep the create row before the /1 rows.
	table := []contractCase{
		{"GET", "/healthz", "/healthz", ""},
		{"GET", "/api/v1/status", "/api/v1/status", ""},
		{"GET", "/api/v1/services", "/api/v1/services", ""},
		{"GET", "/api/v1/services/web/history", "/api/v1/services/{id}/history", ""},
		{"GET", "/api/v1/services/nope/history", "/api/v1/services/{id}/history", ""},
		{"GET", "/api/v1/incidents", "/api/v1/incidents", ""},
		{"POST", "/api/v1/incidents", "/api/v1/incidents", `{"title":"x"}`},
		{"POST", "/api/v1/incidents", "/api/v1/incidents", `{}`},
		{"GET", "/api/v1/incidents/1", "/api/v1/incidents/{id}", ""},
		{"GET", "/api/v1/incidents/999999", "/api/v1/incidents/{id}", ""},
		{"PATCH", "/api/v1/incidents/1", "/api/v1/incidents/{id}", `{"state":"resolved"}`},
		{"PATCH", "/api/v1/incidents/1", "/api/v1/incidents/{id}", `{"state":"bogus"}`},
		{"PATCH", "/api/v1/incidents/999999", "/api/v1/incidents/{id}", `{"state":"resolved"}`},
		{"POST", "/api/v1/incidents/1/updates", "/api/v1/incidents/{id}/updates", `{"text":"x"}`},
		{"POST", "/api/v1/incidents/1/updates", "/api/v1/incidents/{id}/updates", `{}`},
		{"POST", "/api/v1/incidents/999999/updates", "/api/v1/incidents/{id}/updates", `{"text":"x"}`},
		{"GET", "/api/v1/openapi.yaml", "/api/v1/openapi.yaml", ""},
		{"GET", "/api/v1/openapi.json", "/api/v1/openapi.json", ""},
		{"GET", "/docs", "/docs", ""},
		{"GET", "/nope", "/nope", ""},
	}

	covered := map[string]bool{}
	for _, tc := range table {
		var req *http.Request
		if tc.body == "" {
			req = httptest.NewRequest(tc.method, tc.path, nil)
		} else {
			req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		ops, ok := paths[tc.specPath].(map[string]any)
		if tc.specPath == "/nope" {
			// Unknown routes are not in the spec; they must still use
			// the standard 404 envelope.
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", tc.method, tc.path, rec.Code)
			}
			assertEnvelope(t, tc, rec.Body.Bytes(), errCode)
			continue
		}
		if !ok {
			t.Errorf("%s %s: spec path %q missing", tc.method, tc.path, tc.specPath)
			continue
		}
		op, ok := ops[strings.ToLower(tc.method)].(map[string]any)
		if !ok {
			t.Errorf("%s %s: method missing from spec path %q", tc.method, tc.path, tc.specPath)
			continue
		}
		resps, ok := op["responses"].(map[string]any)
		if !ok {
			t.Errorf("%s %s: spec operation has no responses", tc.method, tc.path)
			continue
		}
		if _, ok := resps[strconv.Itoa(rec.Code)]; !ok {
			t.Errorf("%s %s = %d, undocumented in spec (documents %v)", tc.method, tc.path, rec.Code, keys(resps))
		}
		covered[tc.method+" "+tc.specPath] = true
		if rec.Code >= 400 {
			assertEnvelope(t, tc, rec.Body.Bytes(), errCode)
		}
	}

	for path, rawOps := range paths {
		for method := range rawOps.(map[string]any) {
			key := strings.ToUpper(method) + " " + path
			if !covered[key] {
				t.Errorf("spec operation %s has no exercising contract case", key)
			}
		}
	}
}

func assertEnvelope(t *testing.T, tc contractCase, body []byte, vocab map[string]bool) {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Errorf("%s %s: error body is not JSON: %q", tc.method, tc.path, string(body))
		return
	}
	errObj, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Errorf("%s %s: missing error envelope in %v", tc.method, tc.path, decoded)
		return
	}
	code, _ := errObj["code"].(string)
	if !vocab[code] {
		t.Errorf("%s %s: error code %q not in spec vocabulary", tc.method, tc.path, code)
	}
	if _, ok := errObj["message"].(string); !ok {
		t.Errorf("%s %s: error envelope missing message string", tc.method, tc.path)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
