// Package findings derives rule-based findings from a run's recorded
// evidence. Every finding carries the numbers that triggered it.
package findings

import (
	"fmt"
	"math"
	"sort"
	"time"

	"faultline/internal/fault"
	"faultline/internal/report"
)

const (
	SteadyStateBreach   = "steady_state_breach"
	RetryAmplification  = "retry_amplification"
	RetryWithoutBackoff = "retry_without_backoff"
	RetryAfterIgnored   = "retry_after_ignored"
	MissingTimeout      = "missing_timeout"
	SlowRecovery        = "slow_recovery"
	FaultNotTriggered   = "fault_not_triggered"
)

// TimeoutFault describes a timeout fault of the experiment.
type TimeoutFault struct {
	ID     string
	Route  string
	HoldMs float64
}

// Fault describes any fault of the experiment, for fault_not_triggered.
type Fault struct {
	ID       string
	Route    string
	Match    fault.Match
	Triggers int64
}

type Input struct {
	Hypothesis []report.HypothesisEntry
	// Records are the run's own proxied requests during injection. Only
	// Evaluate reads them; EvaluateFrom takes its Evidence separately.
	Records                     []fault.Record
	RetryAmplificationThreshold float64
	TimeoutFaults               []TimeoutFault
	Faults                      []Fault
	// InjectionCompleted is false for runs that were aborted or never injected.
	InjectionCompleted bool
	// RecoveryMeasured is true when the recovery phase ran to its end.
	RecoveryMeasured bool
	RecoverySeconds  *float64
	RecoveryTimeoutS float64
	// ClearedAt is when the faults were removed. Zero if they never were.
	ClearedAt time.Time
}

func round(v float64, decimals int) float64 {
	m := math.Pow(10, float64(decimals))
	return math.Round(v*m) / m
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	} else {
		return (s[n/2-1] + s[n/2]) / 2
	}
}

func end(r fault.Record) time.Time {
	return r.TS.Add(time.Duration(r.DurationMs * float64(time.Millisecond)))
}

// Evaluate applies every rule to records held in memory.
func Evaluate(in Input) []report.Finding {
	out, _ := EvaluateFrom(in, FromRecords(in.Records))
	return out
}

// EvaluateFrom applies every rule. Findings come out ordered by severity,
// then by rule. If the evidence cannot be read, the rules that need it are
// left out and the error is returned with the findings that remain.
func EvaluateFrom(in Input, ev Evidence) ([]report.Finding, error) {
	out := []report.Finding{}
	var evidenceErr error

	for _, h := range in.Hypothesis {
		if h.Passed {
			continue
		}
		observed := "no data"
		var obs any
		if h.Observed != nil {
			observed, obs = fmt.Sprint(*h.Observed), *h.Observed
		}
		out = append(out, report.Finding{
			ID: SteadyStateBreach, Severity: "high",
			Title:    "Steady state breached: " + h.Name,
			Detail:   fmt.Sprintf("Hypothesis %s has threshold %v; observed %s during injection.", h.Name, h.Threshold, observed),
			Evidence: map[string]any{"name": h.Name, "threshold": h.Threshold, "observed": obs},
		})
	}

	retries, err := retryFindings(in, ev)
	if err != nil {
		evidenceErr = err
	}
	out = append(out, retries...)

	for _, tf := range in.TimeoutFaults {
		var held []float64
		err := ev.Holds(tf.ID, func(h Hold) {
			// A hold that ended because the faults were cleared says nothing
			// about the caller's timeout: it neither gave up nor sat it out.
			cutShort := h.HeldMs < MissingTimeoutFraction*tf.HoldMs
			if cutShort && !in.ClearedAt.IsZero() && !h.End.Before(in.ClearedAt.Add(-ClearToleranceMs*time.Millisecond)) {
				return
			}
			held = append(held, h.HeldMs)
		})
		if err != nil {
			evidenceErr = err
			continue
		}
		if len(held) == 0 {
			continue
		}
		if med := median(held); med >= MissingTimeoutFraction*tf.HoldMs {
			out = append(out, report.Finding{
				ID: MissingTimeout, Severity: "high",
				Title:  "Caller has no effective timeout on route " + tf.Route,
				Detail: fmt.Sprintf("The proxy held requests on route %s for %.0f ms; callers stayed connected for a median of %.0f ms over %d requests instead of giving up.", tf.Route, tf.HoldMs, med, len(held)),
				Evidence: map[string]any{
					"route": tf.Route, "hold_ms": round(tf.HoldMs, 1), "median_client_held_ms": round(med, 1), "samples": len(held),
				},
			})
		}
	}

	if in.RecoveryMeasured {
		slow := in.RecoverySeconds == nil || *in.RecoverySeconds > SlowRecoveryFraction*in.RecoveryTimeoutS
		if slow {
			var secs any
			detail := fmt.Sprintf("The system did not recover within the recovery timeout of %v s.", in.RecoveryTimeoutS)
			if in.RecoverySeconds != nil {
				secs = *in.RecoverySeconds
				detail = fmt.Sprintf("Recovery took %v s, more than %.0f%% of the %v s recovery timeout.", *in.RecoverySeconds, SlowRecoveryFraction*100, in.RecoveryTimeoutS)
			}
			out = append(out, report.Finding{
				ID: SlowRecovery, Severity: "medium",
				Title:    "Slow recovery after faults cleared",
				Detail:   detail,
				Evidence: map[string]any{"recovery_seconds": secs, "timeout_s": in.RecoveryTimeoutS},
			})
		}
	}

	if in.InjectionCompleted {
		for _, f := range in.Faults {
			if f.Triggers > 0 {
				continue
			}
			out = append(out, report.Finding{
				ID: FaultNotTriggered, Severity: "info",
				Title:  "Fault " + f.ID + " never triggered",
				Detail: fmt.Sprintf("No request of this run matched fault %s on route %s. Check the route, the match and that the system under test forwards the scope header.", f.ID, f.Route),
				Evidence: map[string]any{
					"fault_id": f.ID, "route": f.Route, "match": map[string]any{"method": f.Match.Method, "path": f.Match.Path},
				},
			})
		}
	}

	rank := map[string]int{"high": 0, "medium": 1, "low": 2, "info": 3}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Severity] < rank[out[j].Severity] })
	return out, evidenceErr
}

// retryStats is what the retry rules keep per faulted route.
type retryStats struct {
	total, maxAttempts, correlations int
	noBackoffGaps, ignoredGaps       []float64
	ignoredRetryAfter                int
}

// retryFindings looks at how callers retried on each faulted route.
func retryFindings(in Input, ev Evidence) ([]report.Finding, error) {
	routes, err := ev.FaultedRoutes()
	if err != nil {
		return nil, err
	}
	sort.Strings(routes)
	stats := map[string]*retryStats{}
	for _, route := range routes {
		stats[route] = &retryStats{ignoredRetryAfter: math.MaxInt}
	}
	err = ev.Attempts(func(route string, attempts []Attempt) {
		s := stats[route]
		if s == nil {
			return
		}
		s.correlations++
		s.total += len(attempts)
		s.maxAttempts = max(s.maxAttempts, len(attempts))
		for i := 0; i+1 < len(attempts); i++ {
			prev, next := attempts[i], attempts[i+1]
			if !prev.Faulted {
				continue
			}
			gap := float64(next.TS.Sub(prev.end()).Microseconds()) / 1000
			if gap < -OverlapToleranceMs {
				continue
			}
			gap = math.Max(gap, 0)
			if gap < BackoffMinGapMs {
				s.noBackoffGaps = append(s.noBackoffGaps, gap)
			}
			if prev.RetryAfter != nil && gap < RetryAfterTolerance*float64(*prev.RetryAfter)*1000 {
				s.ignoredGaps = append(s.ignoredGaps, gap)
				s.ignoredRetryAfter = min(s.ignoredRetryAfter, *prev.RetryAfter)
			}
		}
	})
	if err != nil {
		return nil, err
	}

	var out []report.Finding
	for _, route := range routes {
		s := stats[route]
		total, maxAttempts, correlations := s.total, s.maxAttempts, s.correlations
		noBackoffGaps, ignoredGaps, ignoredRetryAfter := s.noBackoffGaps, s.ignoredGaps, s.ignoredRetryAfter

		if correlations > 0 {
			mean := float64(total) / float64(correlations)
			if mean >= in.RetryAmplificationThreshold {
				severity := "medium"
				if mean >= AmplificationHighFactor*in.RetryAmplificationThreshold {
					severity = "high"
				}
				out = append(out, report.Finding{
					ID: RetryAmplification, Severity: severity,
					Title:  "Retries amplify load on route " + route,
					Detail: fmt.Sprintf("Each logical request made %.2f calls to route %s on average (max %d) across %d correlation ids while it was failing. The threshold is %v.", mean, route, maxAttempts, correlations, in.RetryAmplificationThreshold),
					Evidence: map[string]any{
						"route": route, "mean_attempts": round(mean, 2), "max_attempts": maxAttempts, "correlations": correlations,
					},
				})
			}
		}
		if len(noBackoffGaps) > 0 {
			out = append(out, report.Finding{
				ID: RetryWithoutBackoff, Severity: "medium",
				Title:  "Retries without backoff on route " + route,
				Detail: fmt.Sprintf("%d retries reached route %s less than %.0f ms after a faulted response (median gap %.1f ms).", len(noBackoffGaps), route, BackoffMinGapMs, median(noBackoffGaps)),
				Evidence: map[string]any{
					"route": route, "occurrences": len(noBackoffGaps), "median_gap_ms": round(median(noBackoffGaps), 1),
				},
			})
		}
		if len(ignoredGaps) > 0 {
			out = append(out, report.Finding{
				ID: RetryAfterIgnored, Severity: "medium",
				Title:  "Retry-After ignored on route " + route,
				Detail: fmt.Sprintf("%d retries reached route %s earlier than %.0f%% of the Retry-After of %d s the proxy sent (median gap %.1f ms).", len(ignoredGaps), route, RetryAfterTolerance*100, ignoredRetryAfter, median(ignoredGaps)),
				Evidence: map[string]any{
					"route": route, "violations": len(ignoredGaps), "retry_after_s": ignoredRetryAfter, "median_gap_ms": round(median(ignoredGaps), 1),
				},
			})
		}
	}
	return out, nil
}
