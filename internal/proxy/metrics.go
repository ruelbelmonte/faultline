package proxy

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"faultline/internal/fault"
)

// promMetrics are labelled by route name and fault type only, both fixed by
// config and code, so series stay bounded and carry no request data.
type promMetrics struct {
	reg            *prometheus.Registry
	requests       *prometheus.CounterVec
	duration       *prometheus.HistogramVec
	upstreamErrors *prometheus.CounterVec
	faults         *prometheus.CounterVec
	leaseExpired   prometheus.Counter
	recordsDropped prometheus.Counter
}

func newMetrics(routes []string, e *Engine) *promMetrics {
	m := &promMetrics{
		reg: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "faultline_proxy_requests_total", Help: "Requests received per route.",
		}, []string{"route"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "faultline_proxy_request_duration_seconds", Help: "Time to serve a request per route, injected latency included.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"route"}),
		upstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "faultline_proxy_upstream_errors_total", Help: "Requests whose upstream could not be reached, per route.",
		}, []string{"route"}),
		faults: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "faultline_proxy_faults_triggered_total", Help: "Faults injected, per fault type.",
		}, []string{"type"}),
		leaseExpired: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "faultline_proxy_lease_expired_total", Help: "Rule sets cleared because the control plane stopped renewing the lease.",
		}),
		recordsDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "faultline_proxy_records_dropped_total", Help: "Request records overwritten in the ring before the control plane read them.",
		}),
	}
	for _, r := range routes {
		m.requests.WithLabelValues(r)
		m.duration.WithLabelValues(r)
		m.upstreamErrors.WithLabelValues(r)
	}
	for _, t := range fault.Types {
		m.faults.WithLabelValues(t)
	}
	m.reg.MustRegister(m.requests, m.duration, m.upstreamErrors, m.faults, m.leaseExpired, m.recordsDropped,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "faultline_proxy_lease_active", Help: "1 while a rule set is installed.",
		}, func() float64 {
			if active, _ := e.LeaseState(); active {
				return 1
			}
			return 0
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "faultline_proxy_lease_seconds_since_renewal", Help: "Seconds since the active lease was installed or renewed; 0 without one.",
		}, func() float64 {
			_, since := e.LeaseState()
			return since.Seconds()
		}),
	)
	return m
}

func (m *promMetrics) observe(route string, took time.Duration, out outcome, faultType string) {
	m.requests.WithLabelValues(route).Inc()
	m.duration.WithLabelValues(route).Observe(took.Seconds())
	if out.upstreamErr {
		m.upstreamErrors.WithLabelValues(route).Inc()
	}
	if faultType != "" {
		m.faults.WithLabelValues(faultType).Inc()
	}
}
