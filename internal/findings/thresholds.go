package findings

// Rule thresholds. The retry amplification threshold is configurable
// (findings.retry_amplification_threshold); everything else is tuned here.
const (
	// AmplificationHighFactor raises retry_amplification to high severity
	// when mean attempts reach this multiple of the configured threshold.
	AmplificationHighFactor = 2.0

	// BackoffMinGapMs: a retry arriving sooner than this after a faulted
	// response counts as having no backoff.
	BackoffMinGapMs = 50.0

	// RetryAfterTolerance: a retry must wait at least this fraction of the
	// Retry-After value the proxy sent.
	RetryAfterTolerance = 0.9

	// MissingTimeoutFraction: a caller that holds a timeout fault for at
	// least this fraction of the hold has no effective timeout of its own.
	MissingTimeoutFraction = 0.9

	// SlowRecoveryFraction: recovery taking more than this fraction of
	// recovery.timeout is slow.
	SlowRecoveryFraction = 0.5

	// ClearToleranceMs: a hold that ended within this long before the
	// faults were cleared is treated as ended by the clear.
	ClearToleranceMs = 300

	// OverlapToleranceMs: two requests of one correlation that overlap by
	// more than this are parallel calls, not an attempt and its retry.
	OverlapToleranceMs = 5.0
)
