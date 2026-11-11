// Package experiment parses, validates and redacts experiment files.
package experiment

import (
	"net/url"
	"strings"
	"time"

	"faultline/internal/config"
	"faultline/internal/fault"
	"faultline/internal/yamlx"
)

const (
	ScopeHeader = "header"
	ScopeAll    = "all"
)

type Request struct {
	Method  string
	Path    string
	Headers map[string]string
	Body    string
}

type Traffic struct {
	BaseURL     string
	RPS         float64
	Concurrency int
	Timeout     time.Duration
	Requests    []Request
	Command     string
}

type Probe struct {
	Name         string
	URL          string
	Interval     time.Duration
	Timeout      time.Duration
	ExpectStatus int
}

type Hypothesis struct {
	ErrorRateMax    *float64
	P95LatencyMsMax *float64
	ProbeSuccessMin *float64
}

type Abort struct {
	ErrorRateMax             *float64
	ProbeFailuresConsecutive int
}

type Fault struct {
	fault.Spec
	Start    time.Duration
	Duration time.Duration
	Typed    fault.Params
}

type Experiment struct {
	Name        string
	Description string
	Seed        *int64
	TargetRoute string
	Scope       string
	Duration    time.Duration

	Traffic *Traffic

	PrecheckConsecutiveOK int
	PrecheckTimeout       time.Duration
	Probes                []Probe
	Hypothesis            Hypothesis
	Abort                 Abort
	RecoveryTimeout       time.Duration

	Faults []Fault
}

// Parse validates an experiment against a config. A non-empty error list
// means the experiment must not run.
func Parse(data []byte, cfg *config.Config) (*Experiment, yamlx.Errors) {
	var errs yamlx.Errors
	root := yamlx.Parse(data, &errs)
	if root == nil {
		return nil, errs
	}
	e := &Experiment{
		Scope:                 ScopeHeader,
		PrecheckConsecutiveOK: 3,
		PrecheckTimeout:       15 * time.Second,
		RecoveryTimeout:       30 * time.Second,
	}

	if root.Require("version") {
		if v, ok := root.Int("version"); ok && v != 1 {
			errs.Add("version", "must be 1")
		}
	}
	if root.Require("name") {
		if v, ok := root.Str("name"); ok {
			e.Name = v
			if !config.NameRE.MatchString(v) {
				errs.Add("name", "must match [a-z0-9-]+")
			}
		}
	}
	e.Description, _ = root.Str("description")
	if v, ok := root.Int("seed"); ok {
		e.Seed = &v
	}

	if root.Require("target") {
		if m := root.Map("target"); m != nil {
			if m.Require("route") {
				if v, ok := m.Str("route"); ok {
					e.TargetRoute = v
					if _, found := cfg.Route(v); !found {
						errs.Add(m.Path("route"), "route %q is not defined in the config", v)
					}
				}
			}
			m.Done()
		}
	}

	if v, ok := root.Str("scope"); ok {
		e.Scope = v
		switch v {
		case ScopeHeader:
		case ScopeAll:
			if !cfg.AllowUnscoped {
				errs.Add("scope", "scope all is rejected unless control.allow_unscoped is true")
			}
		default:
			errs.Add("scope", "must be header or all")
		}
	}

	if root.Require("duration") {
		if v, ok := root.Dur("duration"); ok {
			e.Duration = v
			switch {
			case v <= 0:
				errs.Add("duration", "must be greater than 0")
			case v > cfg.MaxExperimentDuration:
				errs.Add("duration", "must not exceed control.max_experiment_duration (%s)", cfg.MaxExperimentDuration)
			}
		}
	}

	if m := root.Map("traffic"); m != nil {
		e.Traffic = parseTraffic(m, &errs)
	}
	if m := root.Map("steady_state"); m != nil {
		parseSteadyState(m, e, &errs)
	}
	if m := root.Map("abort"); m != nil {
		if v, ok := m.Float("error_rate_max"); ok {
			if !(v > 0 && v <= 1) {
				errs.Add(m.Path("error_rate_max"), "must be greater than 0 and at most 1")
			}
			e.Abort.ErrorRateMax = &v
		}
		if v, ok := m.Int("probe_failures_consecutive"); ok {
			if v < 1 {
				errs.Add(m.Path("probe_failures_consecutive"), "must be at least 1")
			}
			e.Abort.ProbeFailuresConsecutive = int(v)
		}
		m.Done()
	}
	if m := root.Map("recovery"); m != nil {
		if v, ok := m.Dur("timeout"); ok {
			if v <= 0 {
				errs.Add(m.Path("timeout"), "must be greater than 0")
			}
			e.RecoveryTimeout = v
		}
		m.Done()
	}

	items := root.Seq("faults")
	if len(items) == 0 {
		errs.Add("faults", "at least one fault is required")
	}
	ids := map[string]bool{}
	for _, it := range items {
		if m := it.Map(); m != nil {
			e.Faults = append(e.Faults, parseFault(m, e, cfg, ids, &errs))
		}
	}
	root.Done()

	if len(errs) > 0 {
		return nil, errs
	}
	return e, nil
}

func parseTraffic(m *yamlx.Map, errs *yamlx.Errors) *Traffic {
	t := &Traffic{Concurrency: 1, Timeout: 30 * time.Second}
	hasRequests, hasCommand := m.Has("requests"), m.Has("command")
	if hasRequests == hasCommand {
		errs.Add(m.Path("requests"), "exactly one of requests or command is required")
	}
	if v, ok := m.Str("base_url"); ok {
		t.BaseURL = strings.TrimRight(v, "/")
		if !validURL(v) {
			errs.Add(m.Path("base_url"), "must be an http or https URL")
		}
	} else if hasRequests {
		errs.Add(m.Path("base_url"), "is required with requests")
	}
	if v, ok := m.Float("rps"); ok {
		t.RPS = v
		if v <= 0 {
			errs.Add(m.Path("rps"), "must be greater than 0")
		}
	} else if hasRequests {
		errs.Add(m.Path("rps"), "is required with requests")
	}
	if v, ok := m.Int("concurrency"); ok {
		t.Concurrency = int(v)
		if v < 1 {
			errs.Add(m.Path("concurrency"), "must be at least 1")
		}
	}
	if v, ok := m.Dur("timeout"); ok {
		t.Timeout = v
		if v <= 0 {
			errs.Add(m.Path("timeout"), "must be greater than 0")
		}
	}
	items := m.Seq("requests")
	if hasRequests && len(items) == 0 {
		errs.Add(m.Path("requests"), "must contain at least one request")
	}
	for _, it := range items {
		rm := it.Map()
		if rm == nil {
			continue
		}
		r := Request{Method: "GET"}
		if v, ok := rm.Str("method"); ok {
			r.Method = strings.ToUpper(v)
			if v == "" {
				errs.Add(rm.Path("method"), "must not be empty")
			}
		}
		if rm.Require("path") {
			if v, ok := rm.Str("path"); ok {
				r.Path = v
				if !strings.HasPrefix(v, "/") {
					errs.Add(rm.Path("path"), "must start with /")
				}
			}
		}
		r.Headers = rm.StrMap("headers")
		r.Body, _ = rm.Str("body")
		rm.Done()
		t.Requests = append(t.Requests, r)
	}
	if v, ok := m.Str("command"); ok {
		t.Command = v
		if strings.TrimSpace(v) == "" {
			errs.Add(m.Path("command"), "must not be empty")
		}
	}
	m.Done()
	return t
}

func parseSteadyState(m *yamlx.Map, e *Experiment, errs *yamlx.Errors) {
	if pm := m.Map("precheck"); pm != nil {
		if v, ok := pm.Int("consecutive_ok"); ok {
			e.PrecheckConsecutiveOK = int(v)
			if v < 1 {
				errs.Add(pm.Path("consecutive_ok"), "must be at least 1")
			}
		}
		if v, ok := pm.Dur("timeout"); ok {
			e.PrecheckTimeout = v
			if v <= 0 {
				errs.Add(pm.Path("timeout"), "must be greater than 0")
			}
		}
		pm.Done()
	}
	names := map[string]bool{}
	for _, it := range m.Seq("probes") {
		pm := it.Map()
		if pm == nil {
			continue
		}
		p := Probe{Interval: time.Second, Timeout: 2 * time.Second, ExpectStatus: 200}
		if pm.Require("name") {
			if v, ok := pm.Str("name"); ok {
				p.Name = v
				if v == "" {
					errs.Add(pm.Path("name"), "must not be empty")
				} else if names[v] {
					errs.Add(pm.Path("name"), "duplicate probe name %q", v)
				}
				names[v] = true
			}
		}
		if pm.Require("url") {
			if v, ok := pm.Str("url"); ok {
				p.URL = v
				if !validURL(v) {
					errs.Add(pm.Path("url"), "must be an http or https URL")
				}
			}
		}
		if v, ok := pm.Dur("interval"); ok {
			p.Interval = v
			if v < 100*time.Millisecond {
				errs.Add(pm.Path("interval"), "must be at least 100ms")
			}
		}
		if v, ok := pm.Dur("timeout"); ok {
			p.Timeout = v
			if v <= 0 {
				errs.Add(pm.Path("timeout"), "must be greater than 0")
			}
		}
		if v, ok := pm.Int("expect_status"); ok {
			p.ExpectStatus = int(v)
			if v < 100 || v > 599 {
				errs.Add(pm.Path("expect_status"), "must be an HTTP status code")
			}
		}
		pm.Done()
		e.Probes = append(e.Probes, p)
	}
	if hm := m.Map("hypothesis"); hm != nil {
		if v, ok := hm.Float("error_rate_max"); ok {
			if v < 0 || v > 1 {
				errs.Add(hm.Path("error_rate_max"), "must be between 0 and 1")
			}
			e.Hypothesis.ErrorRateMax = &v
		}
		if v, ok := hm.Float("p95_latency_ms_max"); ok {
			if v <= 0 {
				errs.Add(hm.Path("p95_latency_ms_max"), "must be greater than 0")
			}
			e.Hypothesis.P95LatencyMsMax = &v
		}
		if v, ok := hm.Float("probe_success_min"); ok {
			if v < 0 || v > 1 {
				errs.Add(hm.Path("probe_success_min"), "must be between 0 and 1")
			}
			e.Hypothesis.ProbeSuccessMin = &v
		}
		hm.Done()
	}
	m.Done()
}

func parseFault(m *yamlx.Map, e *Experiment, cfg *config.Config, ids map[string]bool, errs *yamlx.Errors) Fault {
	f := Fault{}
	if m.Require("id") {
		if v, ok := m.Str("id"); ok {
			f.ID = v
			if v == "" {
				errs.Add(m.Path("id"), "must not be empty")
			} else if ids[v] {
				errs.Add(m.Path("id"), "duplicate fault id %q", v)
			}
			ids[v] = true
		}
	}
	typeOK := false
	if m.Require("type") {
		if v, ok := m.Str("type"); ok {
			f.Type = v
			typeOK = fault.KnownType(v)
			if !typeOK {
				errs.Add(m.Path("type"), "unknown fault type %q", v)
			}
		}
	}
	f.Route = e.TargetRoute
	if v, ok := m.Str("route"); ok {
		f.Route = v
		if _, found := cfg.Route(v); !found {
			errs.Add(m.Path("route"), "route %q is not defined in the config", v)
		}
	}
	if mm := m.Map("match"); mm != nil {
		if v, ok := mm.Str("method"); ok {
			f.Match.Method = strings.ToUpper(v)
		}
		if v, ok := mm.Str("path"); ok {
			f.Match.Path = v
			if !strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "*") {
				errs.Add(mm.Path("path"), "must start with / or *")
			}
		}
		mm.Done()
	}
	if sm := m.Map("strategy"); sm != nil {
		f.Strategy.Kind, _ = sm.Str("kind")
		if !sm.Has("kind") {
			errs.Add(sm.Path("kind"), "is required")
			f.Strategy.Kind = fault.Always
		} else {
			if v, ok := sm.Float("p"); ok {
				f.Strategy.P = v
			}
			if v, ok := sm.Int("count"); ok {
				f.Strategy.Count = int(v)
			}
			if v, ok := sm.Int("n"); ok {
				f.Strategy.N = int(v)
			}
			fault.ValidateStrategy(&f.Strategy, m.Path("strategy"), errs)
		}
		sm.Done()
	} else {
		f.Strategy.Kind = fault.Always
	}

	f.Start, _ = m.Dur("start")
	if v, ok := m.Dur("duration"); ok {
		f.Duration = v
		if v <= 0 {
			errs.Add(m.Path("duration"), "must be greater than 0")
		}
	}
	if e.Duration > 0 {
		switch {
		case f.Start >= e.Duration:
			errs.Add(m.Path("start"), "must be less than the experiment duration (%s)", e.Duration)
		case f.Start+f.Duration > e.Duration:
			errs.Add(m.Path("duration"), "start + duration must not exceed the experiment duration (%s)", e.Duration)
		}
		if f.Duration == 0 {
			f.Duration = e.Duration - f.Start
		}
	}

	raw, _ := m.Any("params")
	if raw == nil {
		raw = map[string]any{}
	}
	f.Params = raw
	if typeOK {
		f.Typed = fault.ParseParams(f.Type, raw, m.Path("params"), errs)
	}
	m.Done()
	return f
}

func validURL(v string) bool {
	u, err := url.Parse(v)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
