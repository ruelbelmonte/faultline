// Package report defines the canonical run document and renders it as
// JSON, Markdown, HTML and JUnit XML.
package report

import (
	"encoding/json"
	"time"
)

const SchemaVersion = "1"

const (
	StatusPending     = "pending"
	StatusPrechecking = "prechecking"
	StatusInjecting   = "injecting"
	StatusRecovering  = "recovering"
	StatusCompleted   = "completed"
	StatusAborted     = "aborted"

	VerdictPassed       = "passed"
	VerdictFailed       = "failed"
	VerdictInconclusive = "inconclusive"
	VerdictAborted      = "aborted"
)

func Terminal(status string) bool {
	return status == StatusCompleted || status == StatusAborted
}

// Time marshals as RFC 3339 in UTC with second precision.
type Time struct{ time.Time }

func At(t time.Time) Time { return Time{t.UTC().Truncate(time.Second)} }

func AtPtr(t time.Time) *Time {
	v := At(t)
	return &v
}

func (t Time) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.UTC().Format(time.RFC3339))
}

func (t *Time) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	t.Time = v
	return nil
}

type ExperimentInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	YAMLSHA256  string `json:"yaml_sha256"`
}

type HypothesisEntry struct {
	Name      string   `json:"name"`
	Threshold float64  `json:"threshold"`
	Observed  *float64 `json:"observed"`
	Passed    bool     `json:"passed"`
}

type FaultResult struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Route    string `json:"route"`
	Triggers int64  `json:"triggers"`
	StartAt  *Time  `json:"start_at"`
	EndAt    *Time  `json:"end_at"`
}

type Finding struct {
	ID       string         `json:"id"`
	Severity string         `json:"severity"`
	Title    string         `json:"title"`
	Detail   string         `json:"detail"`
	Evidence map[string]any `json:"evidence"`
}

type DriverMetrics struct {
	Requests  int     `json:"requests"`
	Errors    int     `json:"errors"`
	ErrorRate float64 `json:"error_rate"`
	P50Ms     float64 `json:"p50_ms"`
	P95Ms     float64 `json:"p95_ms"`
}

type RouteMetrics struct {
	Requests int `json:"requests"`
	Faulted  int `json:"faulted"`
}

type Metrics struct {
	Driver DriverMetrics           `json:"driver"`
	Proxy  map[string]RouteMetrics `json:"proxy"`
}

type Redaction struct {
	Enabled bool     `json:"enabled"`
	Headers []string `json:"headers"`
}

// Document is the canonical run document: the API's run resource and the JSON report.
type Document struct {
	SchemaVersion   string            `json:"schema_version"`
	RunID           string            `json:"run_id"`
	Experiment      ExperimentInfo    `json:"experiment"`
	Environment     string            `json:"environment"`
	Status          string            `json:"status"`
	Verdict         *string           `json:"verdict"`
	Reason          *string           `json:"reason"`
	StartedAt       Time              `json:"started_at"`
	EndedAt         *Time             `json:"ended_at"`
	Hypothesis      []HypothesisEntry `json:"hypothesis"`
	Faults          []FaultResult     `json:"faults"`
	Findings        []Finding         `json:"findings"`
	Metrics         Metrics           `json:"metrics"`
	RecoverySeconds *float64          `json:"recovery_seconds"`
	Redaction       Redaction         `json:"redaction"`
}

// Point is one second of the run timeline.
type Point struct {
	T      Time `json:"t"`
	Driver struct {
		Requests int     `json:"requests"`
		Errors   int     `json:"errors"`
		P95Ms    float64 `json:"p95_ms"`
	} `json:"driver"`
	Proxy  RouteMetrics `json:"proxy"`
	Probes struct {
		OK   int `json:"ok"`
		Fail int `json:"fail"`
	} `json:"probes"`
}

type Phase struct {
	Name  string `json:"name"`
	Start Time   `json:"start"`
	End   *Time  `json:"end"`
}

type FaultWindow struct {
	FaultID string `json:"fault_id"`
	Start   Time   `json:"start"`
	End     Time   `json:"end"`
}

type Marker struct {
	Type   string `json:"type"`
	T      Time   `json:"t"`
	Detail string `json:"detail"`
}

// Timeseries is the body of GET /api/v1/runs/{id}/timeseries.
type Timeseries struct {
	BucketSeconds int           `json:"bucket_seconds"`
	Series        []Point       `json:"series"`
	Phases        []Phase       `json:"phases"`
	FaultWindows  []FaultWindow `json:"fault_windows"`
	Markers       []Marker      `json:"markers"`
}
