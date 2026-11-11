package fault

import (
	"encoding/json"
	"testing"

	"faultline/internal/yamlx"
)

func TestGlob(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"/stock/*", "/stock/1", true},
		{"/stock/*", "/stock/1/detail", true},
		{"/stock/*", "/stock", false},
		{"/stock/*", "/price/1", false},
		{"/v1/messages", "/v1/messages", true},
		{"/v1/messages", "/v1/messages/x", false},
		{"*", "/anything", true},
		{"/a/*/c", "/a/b/c", true},
		{"/a/*/c", "/a/b/d", false},
		{"/item/?", "/item/7", true},
		{"/item/?", "/item/77", false},
		{"", "", true},
	}
	for _, tc := range cases {
		if got := Glob(tc.pattern, tc.s); got != tc.want {
			t.Errorf("Glob(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

func TestMatch(t *testing.T) {
	m := Match{Method: "GET", Path: "/stock/*"}
	if !m.Matches("get", "/stock/1") || m.Matches("POST", "/stock/1") || m.Matches("GET", "/x") {
		t.Fatal("method or path matching is wrong")
	}
	if !(Match{}).Matches("DELETE", "/whatever") {
		t.Fatal("an empty match must match everything")
	}
}

func TestParamsFromJSONNumbers(t *testing.T) {
	var raw map[string]any
	json.Unmarshal([]byte(`{"status":503,"retry_after":2}`), &raw)
	var errs yamlx.Errors
	p := ParseParams(HTTPError, raw, "params", &errs)
	if len(errs) > 0 || p.Status != 503 || *p.RetryAfter != 2 || p.Body != DefaultErrorBody {
		t.Fatalf("unexpected result %+v, errors %v", p, errs)
	}
}

func TestStrategyDefaultsToAlways(t *testing.T) {
	var errs yamlx.Errors
	s := Strategy{}
	ValidateStrategy(&s, "strategy", &errs)
	if s.Kind != Always || len(errs) > 0 {
		t.Fatalf("got %+v, %v", s, errs)
	}
}
