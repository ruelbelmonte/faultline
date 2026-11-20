// Package metrics serves a Prometheus registry behind a read-only bearer token.
package metrics

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Handler serves reg in the Prometheus text format. It has its own token so
// that a scraper never holds a token that can change faults or start runs.
func Handler(reg *prometheus.Registry, token string) http.Handler {
	h := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}` + "\n"))
			return
		}
		h.ServeHTTP(w, r)
	})
}
