package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"faultline/internal/config"
	"faultline/internal/report"
	"faultline/internal/yamlx"
)

const (
	envURL     = "FAULTLINE_URL"
	defaultURL = "http://127.0.0.1:7700"

	exitFailed       = 10
	exitInconclusive = 11
	exitAborted      = 12
)

// runIDRE is the only shape of run id the CLI puts into a URL or a file path.
var runIDRE = regexp.MustCompile(`^run_[a-z2-7]{12}$`)

// errUsage marks a problem with how the command was invoked (exit 2).
var errUsage = errors.New("usage")

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type remote struct {
	base  string
	token string
	http  *http.Client
}

type apiError struct {
	Status int
	Code   string
	Errors yamlx.Errors
}

func (e *apiError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "the control plane rejected the token (401); check FAULTLINE_TOKEN"
	case http.StatusConflict:
		return "conflict (409): " + e.Code
	}
	return fmt.Sprintf("control plane answered %d %s", e.Status, e.Code)
}

func urlFlag(fs *flag.FlagSet) *string {
	def := os.Getenv(envURL)
	if def == "" {
		def = defaultURL
	}
	return fs.String("url", def, "control plane URL (or FAULTLINE_URL)")
}

func (e *env) remote(url string) (*remote, bool) {
	token := os.Getenv(config.EnvToken)
	if token == "" {
		fmt.Fprintf(e.stderr, "faultline: %s is not set\n", config.EnvToken)
		return nil, false
	}
	base := strings.TrimRight(url, "/")
	u, err := neturl.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fmt.Fprintf(e.stderr, "faultline: %q is not an http or https URL\n", url)
		return nil, false
	}
	if u.Scheme == "http" && !loopback(u.Hostname()) {
		fmt.Fprintf(e.stderr, "faultline: warning: sending the API token over plain HTTP to %s; put a TLS terminating proxy in front of the control plane\n", u.Host)
	}
	return &remote{base: base, token: token, http: &http.Client{Timeout: 30 * time.Second}}, true
}

func (r *remote) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return r.http.Do(req)
}

// call performs a JSON request. Non-2xx answers come back as *apiError.
func (r *remote) call(ctx context.Context, method, path string, body, out any) error {
	resp, err := r.request(ctx, method, path, body)
	if err != nil {
		return fmt.Errorf("cannot reach the control plane at %s: %w", r.base, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		ae := &apiError{Status: resp.StatusCode}
		var parsed struct {
			Error  string       `json:"error"`
			Errors yamlx.Errors `json:"errors"`
		}
		if json.Unmarshal(data, &parsed) == nil {
			ae.Code, ae.Errors = parsed.Error, parsed.Errors
		}
		return ae
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// fail prints an error and maps it to an exit code.
func (e *env) fail(err error) int {
	fmt.Fprintf(e.stderr, "faultline: %v\n", err)
	if errors.Is(err, errUsage) {
		return exitUsage
	}
	return exitInternal
}

func (e *env) cmdStatus(args []string) int {
	fs := e.flags("status")
	url := urlFlag(fs)
	if pos, err := parse(fs, args); err != nil || len(pos) > 0 {
		return e.usageErr("status takes no arguments")
	}
	r, ok := e.remote(*url)
	if !ok {
		return exitUsage
	}
	ctx := context.Background()
	var runs []struct {
		RunID      string  `json:"run_id"`
		Experiment string  `json:"experiment"`
		Status     string  `json:"status"`
		Verdict    *string `json:"verdict"`
		StartedAt  string  `json:"started_at"`
	}
	if err := r.call(ctx, http.MethodGet, "/api/v1/runs", nil, &runs); err != nil {
		return e.fail(err)
	}
	fmt.Fprintf(e.stdout, "control: %s ok\n", r.base)
	active := "none"
	for _, run := range runs {
		if !report.Terminal(run.Status) {
			active = fmt.Sprintf("%s (%s) experiment %s, started %s", run.RunID, run.Status, run.Experiment, run.StartedAt)
			break
		}
	}
	fmt.Fprintf(e.stdout, "active run: %s\n", active)
	if len(runs) > 0 && active == "none" {
		last := runs[0]
		verdict := "none"
		if last.Verdict != nil {
			verdict = *last.Verdict
		}
		fmt.Fprintf(e.stdout, "last run: %s (%s, verdict %s) experiment %s\n", last.RunID, last.Status, verdict, last.Experiment)
	}

	var faults struct {
		RunID  *string `json:"run_id"`
		Faults []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Route    string `json:"route"`
			Triggers int64  `json:"triggers"`
		} `json:"faults"`
	}
	if err := r.call(ctx, http.MethodGet, "/api/v1/faults/active", nil, &faults); err != nil {
		fmt.Fprintf(e.stdout, "active faults: unknown (proxy unreachable)\n")
		return exitInternal
	}
	if len(faults.Faults) == 0 {
		fmt.Fprintln(e.stdout, "active faults: none")
	}
	for _, f := range faults.Faults {
		fmt.Fprintf(e.stdout, "active fault: %s %s on route %s, %d triggers\n", f.ID, f.Type, f.Route, f.Triggers)
	}
	return exitOK
}

func (e *env) cmdStop(args []string) int {
	fs := e.flags("stop")
	url := urlFlag(fs)
	if pos, err := parse(fs, args); err != nil || len(pos) > 0 {
		return e.usageErr("stop takes no arguments")
	}
	r, ok := e.remote(*url)
	if !ok {
		return exitUsage
	}
	if err := r.call(context.Background(), http.MethodPost, "/api/v1/kill", nil, nil); err != nil {
		return e.fail(err)
	}
	fmt.Fprintln(e.stdout, "kill switch: all faults cleared, active run aborted")
	return exitOK
}

func validFormat(f string) bool {
	return report.ContentType(f) != ""
}

func (r *remote) report(ctx context.Context, runID, format string) ([]byte, error) {
	resp, err := r.request(ctx, http.MethodGet, "/api/v1/runs/"+runID+"/report?format="+format, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the control plane at %s: %w", r.base, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		ae := &apiError{Status: resp.StatusCode}
		var parsed struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &parsed) == nil {
			ae.Code = parsed.Error
		}
		return nil, ae
	}
	return data, nil
}

func (e *env) cmdReport(args []string) int {
	fs := e.flags("report")
	url := urlFlag(fs)
	format := fs.String("format", "", "json, md, html or junit")
	out := fs.String("out", "", "write to this file instead of stdout")
	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) != 1 {
		return e.usageErr("report needs exactly one run id")
	}
	if !validFormat(*format) {
		return e.usageErr("--format must be one of json, md, html, junit")
	}
	if !runIDRE.MatchString(pos[0]) {
		return e.usageErr("%q is not a run id", pos[0])
	}
	r, ok := e.remote(*url)
	if !ok {
		return exitUsage
	}
	data, err := r.report(context.Background(), pos[0], *format)
	if err != nil {
		return e.fail(err)
	}
	if *out == "" {
		e.stdout.Write(data)
		return exitOK
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return e.fail(err)
	}
	fmt.Fprintf(e.stdout, "wrote %s\n", *out)
	return exitOK
}

func (e *env) cmdRun(args []string) int {
	fs := e.flags("run")
	url := urlFlag(fs)
	failOnBreach := fs.Bool("fail-on-breach", false, "exit 10 failed, 11 inconclusive, 12 aborted")
	reportDir := fs.String("report-dir", "", "write reports to DIR/<run_id>/")
	formatList := fs.String("format", "", "report formats: json,md,html,junit (default: all four)")
	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) != 1 {
		return e.usageErr("run needs exactly one experiment file or directory")
	}
	formats := report.Formats
	if *formatList != "" {
		if *reportDir == "" {
			return e.usageErr("--format needs --report-dir")
		}
		formats = nil
		for _, f := range strings.Split(*formatList, ",") {
			f = strings.TrimSpace(f)
			if !validFormat(f) {
				return e.usageErr("unknown report format %q; use json, md, html or junit", f)
			}
			formats = append(formats, f)
		}
	}
	files, err := experimentFiles(pos[0])
	if err != nil {
		return e.usageErr("%v", err)
	}
	r, ok := e.remote(*url)
	if !ok {
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	type outcome struct{ file, runID, verdict, reason string }
	var outcomes []outcome
	worst := exitOK
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return e.usageErr("%v", err)
		}
		fmt.Fprintf(e.stdout, "==> %s\n", file)
		var created struct {
			RunID string `json:"run_id"`
		}
		err = r.call(ctx, http.MethodPost, "/api/v1/runs", map[string]string{"experiment_yaml": string(data)}, &created)
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusBadRequest {
			fmt.Fprintf(e.stderr, "faultline: %s is invalid:\n", file)
			for _, v := range ae.Errors {
				fmt.Fprintln(e.stderr, v.String())
			}
			return exitUsage
		}
		if err != nil {
			return e.fail(err)
		}
		if !runIDRE.MatchString(created.RunID) {
			// The id goes into URLs and report paths; its shape is never taken on trust.
			return e.fail(errors.New("the control plane returned a malformed run id"))
		}
		fmt.Fprintf(e.stdout, "run %s created\n", created.RunID)

		doc, err := e.follow(ctx, r, created.RunID)
		if ctx.Err() != nil {
			// Interrupted: do not leave the run injecting behind us.
			abortCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			r.call(abortCtx, http.MethodPost, "/api/v1/runs/"+created.RunID+"/abort", nil, nil)
			cancel()
			fmt.Fprintf(e.stderr, "faultline: interrupted; run %s aborted\n", created.RunID)
			return exitInternal
		}
		if err != nil {
			return e.fail(err)
		}
		if doc.RunID != created.RunID {
			return e.fail(errors.New("the control plane returned a document for a different run"))
		}
		e.summary(doc)

		if *reportDir != "" {
			dir := filepath.Join(*reportDir, doc.RunID)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return e.fail(err)
			}
			for _, f := range formats {
				body, err := r.report(ctx, doc.RunID, f)
				if err != nil {
					return e.fail(err)
				}
				path := filepath.Join(dir, "report."+report.Ext(f))
				if err := os.WriteFile(path, body, 0o644); err != nil {
					return e.fail(err)
				}
				fmt.Fprintf(e.stdout, "report: %s\n", path)
			}
		}

		o := outcome{file: file, runID: doc.RunID, verdict: "unknown"}
		if doc.Verdict != nil {
			o.verdict = *doc.Verdict
		}
		if doc.Reason != nil {
			o.reason = *doc.Reason
		}
		outcomes = append(outcomes, o)
		worst = worse(worst, verdictExit(o.verdict))
	}

	if len(outcomes) > 1 {
		fmt.Fprintln(e.stdout, "\nSummary:")
		for _, o := range outcomes {
			line := fmt.Sprintf("  %-13s %s  %s", o.verdict, o.runID, o.file)
			if o.reason != "" {
				line += " (" + o.reason + ")"
			}
			fmt.Fprintln(e.stdout, line)
		}
	}
	if *failOnBreach {
		return worst
	}
	return exitOK
}

func verdictExit(verdict string) int {
	switch verdict {
	case report.VerdictPassed:
		return exitOK
	case report.VerdictFailed:
		return exitFailed
	case report.VerdictInconclusive:
		return exitInconclusive
	case report.VerdictAborted:
		return exitAborted
	}
	return exitInternal
}

// worse orders exit codes 12 > 10 > 11 > 0; an internal error outranks all.
func worse(a, b int) int {
	rank := map[int]int{exitOK: 0, exitInconclusive: 1, exitFailed: 2, exitAborted: 3, exitInternal: 4}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// follow prints progress from the run's event stream until it ends, then
// returns the final document. If the stream breaks it falls back to polling.
func (e *env) follow(ctx context.Context, r *remote, runID string) (*report.Document, error) {
	if resp, err := (&remote{base: r.base, token: r.token, http: &http.Client{}}).request(ctx, http.MethodGet, "/api/v1/runs/"+runID+"/events", nil); err == nil {
		if resp.StatusCode == http.StatusOK {
			e.printEvents(resp.Body)
		}
		resp.Body.Close()
	}
	for {
		var doc report.Document
		if err := r.call(ctx, http.MethodGet, "/api/v1/runs/"+runID, nil, &doc); err != nil {
			return nil, err
		}
		if report.Terminal(doc.Status) {
			return &doc, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (e *env) printEvents(body io.Reader) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	event, lastStatus := "", ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			switch event {
			case "status":
				var s struct {
					Status string `json:"status"`
				}
				if json.Unmarshal([]byte(data), &s) == nil && s.Status != lastStatus {
					lastStatus = s.Status
					fmt.Fprintf(e.stdout, "  %s\n", s.Status)
				}
			case "metrics":
				var p report.Point
				if json.Unmarshal([]byte(data), &p) == nil && p.Driver.Requests > 0 {
					fmt.Fprintf(e.stdout, "  %s  %4d requests  %4d errors  p95 %7.1f ms  %4d faulted\n",
						p.T.UTC().Format("15:04:05"), p.Driver.Requests, p.Driver.Errors, p.Driver.P95Ms, p.Proxy.Faulted)
				}
			}
		}
	}
}

func (e *env) summary(doc *report.Document) {
	verdict := "unknown"
	if doc.Verdict != nil {
		verdict = *doc.Verdict
	}
	line := fmt.Sprintf("verdict: %s", verdict)
	if doc.Reason != nil {
		line += " (" + *doc.Reason + ")"
	}
	fmt.Fprintln(e.stdout, line)
	for _, h := range doc.Hypothesis {
		observed := "no data"
		if h.Observed != nil {
			observed = fmt.Sprint(*h.Observed)
		}
		result := "pass"
		if !h.Passed {
			result = "FAIL"
		}
		fmt.Fprintf(e.stdout, "  hypothesis %-20s limit %-8v observed %-8s %s\n", h.Name, h.Threshold, observed, result)
	}
	for _, f := range doc.Faults {
		fmt.Fprintf(e.stdout, "  fault %s (%s on %s): %d triggers\n", f.ID, f.Type, f.Route, f.Triggers)
	}
	for _, f := range doc.Findings {
		fmt.Fprintf(e.stdout, "  finding [%s] %s: %s\n", f.Severity, f.ID, f.Title)
	}
	if doc.RecoverySeconds != nil {
		fmt.Fprintf(e.stdout, "  recovery: %v s\n", *doc.RecoverySeconds)
	}
}
