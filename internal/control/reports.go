package control

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"faultline/internal/report"
)

// writeReports renders every format into {data_dir}/reports/{run_id}/.
func (c *Control) writeReports(runID string) {
	doc, _, err := c.store.Run(runID)
	if err != nil {
		c.log.Error("writing reports failed", "run_id", runID, "error", err.Error())
		return
	}
	ts, err := c.timeseries(runID)
	if err != nil {
		c.log.Error("writing reports failed", "run_id", runID, "error", err.Error())
		return
	}
	dir := filepath.Join(c.cfg.DataDir, "reports", runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		c.log.Error("writing reports failed", "run_id", runID, "error", err.Error())
		return
	}
	for _, format := range report.Formats {
		data, err := report.Render(format, doc, ts)
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, "report."+report.Ext(format)), data, 0o600)
		}
		if err != nil {
			c.log.Error("writing report failed", "run_id", runID, "format", format, "error", err.Error())
		}
	}
}

func (c *Control) handleReport(w http.ResponseWriter, r *http.Request) {
	id, format := r.PathValue("id"), r.URL.Query().Get("format")
	if report.ContentType(format) == "" {
		writeError(w, http.StatusBadRequest, "invalid_format")
		return
	}
	doc, _, err := c.document(id)
	if err != nil {
		c.notFoundOr500(w, err)
		return
	}
	if !report.Terminal(doc.Status) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "run_in_progress", "status": doc.Status})
		return
	}
	ts, err := c.timeseries(id)
	if err != nil {
		c.notFoundOr500(w, err)
		return
	}
	data, err := report.Render(format, doc, ts)
	if err != nil {
		c.log.Error("rendering report failed", "run_id", id, "format", format, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	c.audit(ActorToken, "report_downloaded", id, map[string]any{"format": format})
	w.Header().Set("Content-Type", report.ContentType(format))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-report.%s"`, id, report.Ext(format)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The HTML report is a download, never a page of this origin.
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Write(data)
}
