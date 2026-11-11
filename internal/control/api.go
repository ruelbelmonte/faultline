package control

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"faultline/internal/config"
	"faultline/internal/experiment"
	"faultline/internal/ui"
	"faultline/internal/yamlx"
)

const maxBody = 1 << 20

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func (c *Control) authorised(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(c.token)) == 1
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	return true
}

// Handler serves /healthz, the authenticated API under /api/v1 and the UI.
func (c *Control) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/meta", c.handleMeta)
	api.HandleFunc("GET /api/v1/routes", c.handleRoutes)
	api.HandleFunc("GET /api/v1/experiments", c.handleExperiments)
	api.HandleFunc("GET /api/v1/experiments/{name}", c.handleExperimentGet)
	api.HandleFunc("PUT /api/v1/experiments/{name}", c.handleExperimentPut)
	api.HandleFunc("POST /api/v1/experiments/validate", c.handleValidate)
	api.HandleFunc("POST /api/v1/runs", c.handleRunCreate)
	api.HandleFunc("GET /api/v1/runs", c.handleRuns)
	api.HandleFunc("GET /api/v1/runs/{id}", c.handleRun)
	api.HandleFunc("GET /api/v1/runs/{id}/timeseries", c.handleTimeseries)
	api.HandleFunc("GET /api/v1/runs/{id}/events", c.handleEvents)
	api.HandleFunc("POST /api/v1/runs/{id}/abort", c.handleAbort)
	api.HandleFunc("POST /api/v1/kill", c.handleKill)
	api.HandleFunc("GET /api/v1/faults/active", c.handleFaultsActive)
	api.HandleFunc("GET /api/v1/audit", c.handleAudit)
	api.HandleFunc("GET /api/v1/runs/{id}/report", c.handleReport)
	api.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found")
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if !c.authorised(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		api.ServeHTTP(w, r)
	})
	mux.Handle("/", ui.Handler())
	return mux
}

// handleMeta tells the UI what it is looking at. It is not in the PRD's
// endpoint table; the header's environment badge has no other source.
func (c *Control) handleMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"environment":    c.cfg.Environment,
		"allow_unscoped": c.cfg.AllowUnscoped,
		"scope_header":   c.cfg.ScopeHeader,
	})
}

func (c *Control) handleRoutes(w http.ResponseWriter, r *http.Request) {
	routes := c.cfg.Routes
	if routes == nil {
		routes = []config.Route{}
	}
	writeJSON(w, http.StatusOK, routes)
}

type experimentEntry struct {
	Name  string `json:"name"`
	File  string `json:"file"`
	Valid bool   `json:"valid"`
}

func isYAML(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}

func (c *Control) handleExperiments(w http.ResponseWriter, r *http.Request) {
	out := []experimentEntry{}
	entries, err := os.ReadDir(c.cfg.ExperimentsDir)
	if err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, "experiments_dir_unreadable")
		return
	}
	for _, ent := range entries {
		if ent.IsDir() || !isYAML(ent.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(c.cfg.ExperimentsDir, ent.Name()))
		valid := false
		if err == nil {
			_, errs := experiment.Parse(data, c.cfg)
			valid = len(errs) == 0
		}
		out = append(out, experimentEntry{Name: strings.TrimSuffix(ent.Name(), filepath.Ext(ent.Name())), File: ent.Name(), Valid: valid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	writeJSON(w, http.StatusOK, out)
}

func (c *Control) handleExperimentGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !config.NameRE.MatchString(name) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	for _, ext := range []string{".yaml", ".yml"} {
		data, err := os.ReadFile(filepath.Join(c.cfg.ExperimentsDir, name+ext))
		if err != nil {
			continue
		}
		// Shown as stored on disk except for sensitive header values.
		writeJSON(w, http.StatusOK, map[string]string{
			"name": name, "file": name + ext,
			"yaml": string(experiment.Redact(data, c.cfg.RedactionHeaders)),
		})
		return
	}
	writeError(w, http.StatusNotFound, "not_found")
}

func (c *Control) handleExperimentPut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !config.NameRE.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_name")
		return
	}
	var body struct {
		YAML string `json:"yaml"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if _, errs := experiment.Parse([]byte(body.YAML), c.cfg); len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_experiment", "errors": errs})
		return
	}
	if err := os.MkdirAll(c.cfg.ExperimentsDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "write_failed")
		return
	}
	file := name + ".yaml"
	// Control never writes sensitive header values to disk, here included.
	data := experiment.Redact([]byte(body.YAML), c.cfg.RedactionHeaders)
	if err := os.WriteFile(filepath.Join(c.cfg.ExperimentsDir, file), data, 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, "write_failed")
		return
	}
	c.audit(ActorToken, "experiment_saved", "", map[string]any{"name": name, "file": file})
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "file": file})
}

func (c *Control) handleValidate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		YAML string `json:"yaml"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	_, errs := experiment.Parse([]byte(body.YAML), c.cfg)
	if errs == nil {
		errs = yamlx.Errors{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": len(errs) == 0, "errors": errs})
}

func (c *Control) handleRunCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExperimentYAML string `json:"experiment_yaml"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	id, err := c.CreateRun([]byte(body.ExperimentYAML))
	var verrs yamlx.Errors
	switch {
	case errors.As(err, &verrs):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_experiment", "errors": verrs})
	case errors.Is(err, ErrRunActive):
		resp := map[string]string{"error": "run_active"}
		if a := c.activeRun(); a != nil {
			resp["run_id"] = a.id
		}
		writeJSON(w, http.StatusConflict, resp)
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusCreated, map[string]string{"run_id": id})
	}
}

func (c *Control) handleRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := c.store.Runs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (c *Control) notFoundOr500(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "run_not_found")
		return
	}
	c.log.Error("request failed", "error", err.Error())
	writeError(w, http.StatusInternalServerError, "internal")
}

func (c *Control) handleRun(w http.ResponseWriter, r *http.Request) {
	doc, _, err := c.document(r.PathValue("id"))
	if err != nil {
		c.notFoundOr500(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func (c *Control) handleTimeseries(w http.ResponseWriter, r *http.Request) {
	ts, err := c.timeseries(r.PathValue("id"))
	if err != nil {
		c.notFoundOr500(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

func (c *Control) handleAbort(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run := c.activeRun()
	if run == nil || run.id != id {
		doc, _, err := c.store.Run(id)
		if err != nil {
			c.notFoundOr500(w, err)
			return
		}
		writeJSON(w, http.StatusConflict, map[string]string{"error": "run_not_active", "status": doc.Status})
		return
	}
	run.abort(ReasonManual, ActorToken)
	select {
	case <-run.done:
	case <-time.After(20 * time.Second):
		writeError(w, http.StatusInternalServerError, "abort_timeout")
		return
	}
	doc, _ := run.Snapshot()
	writeJSON(w, http.StatusOK, map[string]string{"status": doc.Status})
}

// handleKill is the kill switch: the proxy is cleared first, then any active
// run is aborted.
func (c *Control) handleKill(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	clearErr := c.proxy.Clear(ctx)
	cancel()
	runID := ""
	if run := c.abortActive(ReasonKillSwitch, ActorToken); run != nil {
		runID = run.id
	}
	c.audit(ActorToken, "kill_switch", runID, map[string]any{"cleared": clearErr == nil})
	if clearErr != nil {
		c.log.Error("kill switch could not reach the proxy", "error", clearErr.Error())
		writeJSON(w, http.StatusBadGateway, map[string]any{"cleared": false, "error": "proxy_unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"cleared": true})
}

func (c *Control) handleFaultsActive(w http.ResponseWriter, r *http.Request) {
	raw, err := c.proxy.Active(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "proxy_unreachable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(raw)
}

func (c *Control) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid_limit")
			return
		}
		limit = min(n, 1000)
	}
	entries, err := c.store.AuditLog(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleEvents streams status changes, metrics buckets and findings as
// Server-Sent Events. The stream ends when the run does.
func (c *Control) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	doc, _, err := c.document(id)
	if err != nil {
		c.notFoundOr500(w, err)
		return
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(event string, data []byte) bool {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	sendJSON := func(event string, v any) bool {
		data, _ := json.Marshal(v)
		return send(event, data)
	}

	var ch chan sseMsg
	cancel := func() {}
	initial := statusEvent{doc.RunID, doc.Status, doc.Verdict, doc.Reason}
	if run := c.activeRun(); run != nil && run.id == id {
		ch, initial, cancel = run.subscribe()
	}
	defer cancel()

	if ch == nil {
		// Finished: replay the outcome from the stored document.
		if final, _, err := c.store.Run(id); err == nil {
			doc = final
			initial = statusEvent{doc.RunID, doc.Status, doc.Verdict, doc.Reason}
		}
		for _, f := range doc.Findings {
			if !sendJSON("finding", f) {
				return
			}
		}
		sendJSON("status", initial)
		return
	}
	if !sendJSON("status", initial) {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case msg, open := <-ch:
			if !open {
				return
			}
			if !send(msg.event, msg.data) {
				return
			}
		}
	}
}
