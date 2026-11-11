package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"faultline/internal/fault"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// BearerOK compares the request's bearer token in constant time.
func BearerOK(r *http.Request, token string) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// Admin returns the admin API handler.
func (p *Proxy) Admin(token string) http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !BearerOK(r, token) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("GET /admin/v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	mux.HandleFunc("PUT /admin/v1/faults", auth(func(w http.ResponseWriter, r *http.Request) {
		var req fault.RuleSet
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_json"})
			return
		}
		if errs := p.engine.Install(req); len(errs) > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_rule_set", "errors": errs})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "run_id": req.RunID})
	}))

	mux.HandleFunc("DELETE /admin/v1/faults", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"cleared": p.engine.Clear("cleared")})
	}))

	mux.HandleFunc("POST /admin/v1/lease/renew", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			LeaseID string `json:"lease_id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_json"})
			return
		}
		switch err := p.engine.Renew(req.LeaseID); {
		case errors.Is(err, ErrNoLease):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrLeaseMismatch):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		default:
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		}
	}))

	mux.HandleFunc("GET /admin/v1/faults/active", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.engine.Active())
	}))

	mux.HandleFunc("GET /admin/v1/records", auth(func(w http.ResponseWriter, r *http.Request) {
		var since int64
		if v := r.URL.Query().Get("since"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_since"})
				return
			}
			since = n
		}
		writeJSON(w, http.StatusOK, p.records.since(r.URL.Query().Get("run_id"), since))
	}))

	mux.HandleFunc("GET /admin/v1/stats", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.engine.Stats())
	}))

	// Unknown admin paths must not reveal anything without a token either.
	mux.HandleFunc("/", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}))
	return mux
}
