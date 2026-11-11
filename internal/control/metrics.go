package control

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"faultline/internal/report"
)

// rollingWindow is the window for abort and recovery error rates.
const rollingWindow = 5 * time.Second

type sample struct {
	start  time.Time
	end    time.Time
	ms     float64
	status int
}

func (s sample) failed() bool { return s.status == 0 || s.status >= 500 }

type probeCheck struct {
	at time.Time
	ok bool
}

// Bucket is one second of metrics, as persisted.
type Bucket struct {
	T      report.Time `json:"t"`
	Driver struct {
		Requests int     `json:"requests"`
		Errors   int     `json:"errors"`
		P50Ms    float64 `json:"p50_ms"`
		P95Ms    float64 `json:"p95_ms"`
	} `json:"driver"`
	Routes map[string]report.RouteMetrics `json:"routes"`
	Probes struct {
		OK   int `json:"ok"`
		Fail int `json:"fail"`
	} `json:"probes"`

	latencies []float64
}

func (b *Bucket) Point() report.Point {
	var p report.Point
	p.T = b.T
	p.Driver.Requests, p.Driver.Errors, p.Driver.P95Ms = b.Driver.Requests, b.Driver.Errors, b.Driver.P95Ms
	for _, r := range b.Routes {
		p.Proxy.Requests += r.Requests
		p.Proxy.Faulted += r.Faulted
	}
	p.Probes.OK, p.Probes.Fail = b.Probes.OK, b.Probes.Fail
	return p
}

// percentile uses the nearest-rank method on a sorted slice.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

func round(v float64, decimals int) float64 {
	m := math.Pow(10, float64(decimals))
	return math.Round(v*m) / m
}

func sortedCopy(v []float64) []float64 {
	out := append([]float64(nil), v...)
	sort.Float64s(out)
	return out
}

var runIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newRunID returns run_ followed by 12 lowercase base32 characters.
func newRunID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "run_" + strings.ToLower(runIDEncoding.EncodeToString(b))[:12]
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
