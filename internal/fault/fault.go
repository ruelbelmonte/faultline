// Package fault defines the fault spec shared by the control plane and the proxy.
package fault

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"

	"faultline/internal/yamlx"
)

const (
	Latency              = "latency"
	HTTPError            = "http_error"
	ErrorInBody          = "error_in_body"
	Timeout              = "timeout"
	Reset                = "reset"
	SlowBody             = "slow_body"
	CorruptBody          = "corrupt_body"
	LLMRateLimit         = "llm_rate_limit"
	LLMOverloaded        = "llm_overloaded"
	LLMStreamCut         = "llm_stream_cut"
	LLMSlowFirstToken    = "llm_slow_first_token"
	LLMTruncate          = "llm_truncate"
	LLMMalformedToolCall = "llm_malformed_tool_call"
)

var Types = []string{
	Latency, HTTPError, ErrorInBody, Timeout, Reset, SlowBody, CorruptBody,
	LLMRateLimit, LLMOverloaded, LLMStreamCut, LLMSlowFirstToken, LLMTruncate, LLMMalformedToolCall,
}

const (
	Always      = "always"
	Probability = "probability"
	Burst       = "burst"
	EveryNth    = "every_nth"
)

const (
	DefaultErrorBody = `{"error":"faultline injected"}`
	MalformedInput   = `{"query": "de`
)

func KnownType(t string) bool {
	for _, x := range Types {
		if x == t {
			return true
		}
	}
	return false
}

type Match struct {
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
}

type Strategy struct {
	Kind  string  `json:"kind"`
	P     float64 `json:"p,omitempty"`
	Count int     `json:"count,omitempty"`
	N     int     `json:"n,omitempty"`
}

// Spec is the wire shape of a fault, in the experiment file and on the admin API.
type Spec struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Route    string         `json:"route"`
	Match    Match          `json:"match"`
	Strategy Strategy       `json:"strategy"`
	Params   map[string]any `json:"params,omitempty"`
}

// Params holds the validated parameters of every fault type.
type Params struct {
	MS          int
	JitterMS    int
	Status      int
	Body        string
	Headers     map[string]string
	RetryAfter  *int
	ContentType string
	Hold        time.Duration
	BytesPerSec int
	Mode        string
	AtBytes     int
	AfterBytes  int
	DelayMS     int
	MaxChars    int
}

func ValidateStrategy(s *Strategy, path string, errs *yamlx.Errors) {
	if s.Kind == "" {
		s.Kind = Always
	}
	switch s.Kind {
	case Always:
	case Probability:
		if !(s.P > 0 && s.P <= 1) {
			errs.Add(path+".p", "must be greater than 0 and at most 1")
		}
	case Burst:
		if s.Count < 1 {
			errs.Add(path+".count", "must be at least 1")
		}
	case EveryNth:
		if s.N < 1 {
			errs.Add(path+".n", "must be at least 1")
		}
	default:
		errs.Add(path+".kind", "must be one of always, probability, burst, every_nth")
	}
}

type paramReader struct {
	raw  map[string]any
	path string
	errs *yamlx.Errors
	used map[string]bool
}

func (r *paramReader) get(key string) (any, bool) {
	r.used[key] = true
	v, ok := r.raw[key]
	if !ok || v == nil {
		return nil, false
	}
	return v, true
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func (r *paramReader) intMin(key string, min int, required bool, def int) int {
	v, ok := r.get(key)
	if !ok {
		if required {
			r.errs.Add(r.path+"."+key, "is required")
		}
		return def
	}
	f, ok := number(v)
	if !ok || f != math.Trunc(f) {
		r.errs.Add(r.path+"."+key, "must be an integer")
		return def
	}
	if int(f) < min {
		r.errs.Add(r.path+"."+key, "must be at least %d", min)
		return def
	}
	return int(f)
}

func (r *paramReader) str(key string, required bool, def string) string {
	v, ok := r.get(key)
	if !ok {
		if required {
			r.errs.Add(r.path+"."+key, "is required")
		}
		return def
	}
	s, ok := v.(string)
	if !ok {
		r.errs.Add(r.path+"."+key, "must be a string")
		return def
	}
	return s
}

func (r *paramReader) done() {
	var unknown []string
	for k := range r.raw {
		if !r.used[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		r.errs.Add(r.path+"."+k, "unknown parameter for this fault type")
	}
}

// ParseParams validates raw params for a fault type and applies defaults.
func ParseParams(typ string, raw map[string]any, path string, errs *yamlx.Errors) Params {
	r := &paramReader{raw: raw, path: path, errs: errs, used: map[string]bool{}}
	var p Params
	switch typ {
	case Latency:
		p.MS = r.intMin("ms", 0, true, 0)
		p.JitterMS = r.intMin("jitter_ms", 0, false, 0)
	case HTTPError:
		p.Status = r.intMin("status", 200, true, 0)
		if p.Status > 599 {
			errs.Add(path+".status", "must be at most 599")
		}
		p.Body = r.str("body", false, DefaultErrorBody)
		if v, ok := r.get("headers"); ok {
			m, ok := v.(map[string]any)
			if !ok {
				errs.Add(path+".headers", "must be a mapping")
			}
			p.Headers = map[string]string{}
			for k, hv := range m {
				s, ok := hv.(string)
				if !ok {
					errs.Add(path+".headers."+k, "must be a string")
					continue
				}
				p.Headers[k] = s
			}
		}
		if _, ok := raw["retry_after"]; ok {
			v := r.intMin("retry_after", 0, false, 0)
			p.RetryAfter = &v
		}
	case ErrorInBody:
		p.Body = r.str("body", true, "")
		p.ContentType = r.str("content_type", false, "application/json")
	case Timeout:
		p.Hold = 30 * time.Second
		if s := r.str("hold", false, ""); s != "" {
			d, err := time.ParseDuration(s)
			if err != nil || d <= 0 {
				errs.Add(path+".hold", "must be a duration such as 30s")
			} else {
				p.Hold = d
			}
		}
	case Reset, LLMOverloaded, LLMMalformedToolCall:
	case SlowBody:
		p.BytesPerSec = r.intMin("bytes_per_sec", 1, true, 1)
	case CorruptBody:
		p.Mode = r.str("mode", true, "")
		switch p.Mode {
		case "truncate":
			p.AtBytes = r.intMin("at_bytes", 0, true, 0)
		case "garble":
		case "":
		default:
			errs.Add(path+".mode", "must be truncate or garble")
		}
	case LLMRateLimit:
		v := r.intMin("retry_after", 0, false, 5)
		p.RetryAfter = &v
	case LLMStreamCut:
		p.AfterBytes = r.intMin("after_bytes", 0, true, 0)
	case LLMSlowFirstToken:
		p.DelayMS = r.intMin("delay_ms", 0, true, 0)
	case LLMTruncate:
		p.MaxChars = r.intMin("max_chars", 0, false, 50)
	default:
		return p
	}
	r.done()
	return p
}

// Glob matches s against a pattern where * is any run of characters
// (including /) and ? is exactly one character.
func Glob(pattern, s string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if pattern == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if Glob(pattern, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
		default:
			if s == "" || s[0] != pattern[0] {
				return false
			}
		}
		pattern, s = pattern[1:], s[1:]
	}
	return s == ""
}

func (m Match) Matches(method, path string) bool {
	if m.Method != "" && !strings.EqualFold(m.Method, method) {
		return false
	}
	return m.Path == "" || Glob(m.Path, path)
}

// Install is one fault as sent to the proxy: a Spec plus its absolute window.
type Install struct {
	Spec
	StartAt *time.Time `json:"start_at"`
	EndAt   *time.Time `json:"end_at"`
}

// RuleSet is the body of PUT /admin/v1/faults.
type RuleSet struct {
	RunID     string    `json:"run_id"`
	ScopeMode string    `json:"scope_mode"`
	LeaseID   string    `json:"lease_id"`
	Seed      *int64    `json:"seed,omitempty"`
	Faults    []Install `json:"faults"`
}

// Active is the body of GET /admin/v1/faults/active.
type Active struct {
	RunID   *string       `json:"run_id"`
	LeaseID string        `json:"lease_id,omitempty"`
	Faults  []ActiveFault `json:"faults"`
}

type ActiveFault struct {
	ID       string    `json:"id"`
	Type     string    `json:"type"`
	Route    string    `json:"route"`
	StartAt  time.Time `json:"start_at"`
	EndAt    time.Time `json:"end_at"`
	Triggers int64     `json:"triggers"`
}

// Record is one proxied request observed during an active run.
type Record struct {
	Seq          int64     `json:"seq"`
	TS           time.Time `json:"ts"`
	Route        string    `json:"route"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	Status       int       `json:"status"`
	DurationMs   float64   `json:"duration_ms"`
	Scoped       bool      `json:"scoped"`
	Correlation  string    `json:"correlation"`
	FaultID      *string   `json:"fault_id"`
	FaultType    *string   `json:"fault_type"`
	RetryAfter   *int      `json:"retry_after"`
	ClientHeldMs *float64  `json:"client_held_ms"`
}

// MaxRecordsPage is the most records one GET /admin/v1/records call returns.
const MaxRecordsPage = 5000

type RecordPage struct {
	Records []Record `json:"records"`
	Next    int64    `json:"next"`
}

type RouteCounters struct {
	Requests int64 `json:"requests"`
	Faulted  int64 `json:"faulted"`
}

type Event struct {
	TS    time.Time `json:"ts"`
	Type  string    `json:"type"`
	RunID string    `json:"run_id"`
}

// Stats is the body of GET /admin/v1/stats. Routes count every request since
// the proxy started; Faults holds triggers of the current or most recent run.
type Stats struct {
	RunID  *string                  `json:"run_id"`
	Active bool                     `json:"active"`
	Routes map[string]RouteCounters `json:"routes"`
	Faults map[string]int64         `json:"faults"`
	Events []Event                  `json:"events"`
}
