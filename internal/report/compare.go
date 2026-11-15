package report

import (
	"fmt"
	"sort"
)

// Baseline selections a run can be compared with.
const (
	// BaselinePrevious is the latest finished run of the same experiment
	// name and yaml_sha256. It is the default.
	BaselinePrevious = "previous"
	// BaselineLastPassed is the latest such run whose verdict was passed.
	BaselineLastPassed = "last-passed"
	// BaselineExplicit is reported when the baseline was named by run id.
	BaselineExplicit = "explicit"
)

// Comparison is the run-over-run block of a finished run's document. It holds
// ids, numbers and verdicts only: no finding detail or evidence and no
// experiment text, so it can never carry a value that redaction removed.
type Comparison struct {
	BaselineRunID      string            `json:"baseline_run_id"`
	BaselineSelection  string            `json:"baseline_selection"`
	BaselineVerdict    *string           `json:"baseline_verdict"`
	BaselineStartedAt  Time              `json:"baseline_started_at"`
	SameExperimentYAML bool              `json:"same_experiment_yaml"`
	Hypothesis         []HypothesisDelta `json:"hypothesis"`
	Faults             []FaultDelta      `json:"faults"`
	FindingsAppeared   []FindingRef      `json:"findings_appeared"`
	FindingsResolved   []FindingRef      `json:"findings_resolved"`
	Regression         bool              `json:"regression"`
	RegressionReasons  []string          `json:"regression_reasons"`
}

type HypothesisDelta struct {
	Name             string   `json:"name"`
	BaselineObserved *float64 `json:"baseline_observed"`
	Observed         *float64 `json:"observed"`
	Delta            *float64 `json:"delta"`
	BaselinePassed   *bool    `json:"baseline_passed"`
	Passed           bool     `json:"passed"`
}

type FaultDelta struct {
	ID               string `json:"id"`
	BaselineTriggers *int64 `json:"baseline_triggers"`
	Triggers         int64  `json:"triggers"`
	Delta            *int64 `json:"delta"`
}

// FindingRef identifies a finding by rule id and, when its evidence names
// one, the route it was found on.
type FindingRef struct {
	ID       string `json:"id"`
	Route    string `json:"route,omitempty"`
	Severity string `json:"severity"`
}

func findingRef(f Finding) FindingRef {
	r := FindingRef{ID: f.ID, Severity: f.Severity}
	if route, ok := f.Evidence["route"].(string); ok {
		r.Route = route
	}
	return r
}

func (r FindingRef) key() string { return r.ID + "\x00" + r.Route }

func (r FindingRef) String() string {
	if r.Route == "" {
		return r.ID
	}
	return r.ID + " (" + r.Route + ")"
}

// Compare describes how cur differs from base. selection is BaselinePrevious,
// BaselineLastPassed or the run id the caller asked for (reported as
// BaselineExplicit).
//
// A regression is any of: the baseline passed and cur did not; a hypothesis
// threshold that passed in the baseline fails now; a finding that was not in
// the baseline appears.
func Compare(cur, base *Document, selection string) *Comparison {
	if selection != BaselinePrevious && selection != BaselineLastPassed {
		selection = BaselineExplicit
	}
	c := &Comparison{
		BaselineRunID:      base.RunID,
		BaselineSelection:  selection,
		BaselineVerdict:    base.Verdict,
		BaselineStartedAt:  base.StartedAt,
		SameExperimentYAML: cur.Experiment.YAMLSHA256 == base.Experiment.YAMLSHA256,
		Hypothesis:         []HypothesisDelta{},
		Faults:             []FaultDelta{},
		FindingsAppeared:   []FindingRef{},
		FindingsResolved:   []FindingRef{},
		RegressionReasons:  []string{},
	}
	regress := func(format string, args ...any) {
		c.Regression = true
		c.RegressionReasons = append(c.RegressionReasons, fmt.Sprintf(format, args...))
	}

	if base.Verdict != nil && *base.Verdict == VerdictPassed && (cur.Verdict == nil || *cur.Verdict != VerdictPassed) {
		regress("verdict passed -> %s", verdictOf(cur))
	}

	baseHyp := map[string]HypothesisEntry{}
	for _, h := range base.Hypothesis {
		baseHyp[h.Name] = h
	}
	for _, h := range cur.Hypothesis {
		d := HypothesisDelta{Name: h.Name, Observed: h.Observed, Passed: h.Passed}
		if b, ok := baseHyp[h.Name]; ok {
			passed := b.Passed
			d.BaselineObserved, d.BaselinePassed = b.Observed, &passed
			if b.Observed != nil && h.Observed != nil {
				delta := *h.Observed - *b.Observed
				d.Delta = &delta
			}
			if b.Passed && !h.Passed {
				regress("hypothesis %s passed in %s and fails now", h.Name, base.RunID)
			}
		}
		c.Hypothesis = append(c.Hypothesis, d)
	}

	baseFaults := map[string]int64{}
	for _, f := range base.Faults {
		baseFaults[f.ID] = f.Triggers
	}
	for _, f := range cur.Faults {
		d := FaultDelta{ID: f.ID, Triggers: f.Triggers}
		if b, ok := baseFaults[f.ID]; ok {
			delta := f.Triggers - b
			d.BaselineTriggers, d.Delta = &b, &delta
		}
		c.Faults = append(c.Faults, d)
	}

	curSet, baseSet := map[string]FindingRef{}, map[string]FindingRef{}
	for _, f := range cur.Findings {
		r := findingRef(f)
		curSet[r.key()] = r
	}
	for _, f := range base.Findings {
		r := findingRef(f)
		baseSet[r.key()] = r
	}
	for k, r := range curSet {
		if _, ok := baseSet[k]; !ok {
			c.FindingsAppeared = append(c.FindingsAppeared, r)
		}
	}
	for k, r := range baseSet {
		if _, ok := curSet[k]; !ok {
			c.FindingsResolved = append(c.FindingsResolved, r)
		}
	}
	sortRefs(c.FindingsAppeared)
	sortRefs(c.FindingsResolved)
	for _, r := range c.FindingsAppeared {
		regress("finding %s appeared", r)
	}
	return c
}

func sortRefs(refs []FindingRef) {
	sort.Slice(refs, func(i, j int) bool { return refs[i].key() < refs[j].key() })
}
