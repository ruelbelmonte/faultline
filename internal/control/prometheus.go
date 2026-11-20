package control

import (
	"github.com/prometheus/client_golang/prometheus"

	"faultline/internal/report"
)

type promMetrics struct {
	reg         *prometheus.Registry
	transitions *prometheus.CounterVec
}

func newPromMetrics() *promMetrics {
	m := &promMetrics{
		reg: prometheus.NewRegistry(),
		transitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "faultline_control_run_status_transitions_total", Help: "Runs entering each status.",
		}, []string{"status"}),
	}
	for _, s := range []string{report.StatusPrechecking, report.StatusInjecting, report.StatusRecovering, report.StatusCompleted, report.StatusAborted} {
		m.transitions.WithLabelValues(s)
	}
	m.reg.MustRegister(m.transitions)
	return m
}
