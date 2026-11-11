package report

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
)

// Format names accepted by the API and the CLI.
const (
	FormatJSON  = "json"
	FormatMD    = "md"
	FormatHTML  = "html"
	FormatJUnit = "junit"
)

var Formats = []string{FormatJSON, FormatMD, FormatHTML, FormatJUnit}

// Ext is the file extension for a format; JUnit reports are .xml.
func Ext(format string) string {
	if format == FormatJUnit {
		return "xml"
	}
	return format
}

func ContentType(format string) string {
	switch format {
	case FormatJSON:
		return "application/json"
	case FormatMD:
		return "text/markdown; charset=utf-8"
	case FormatHTML:
		return "text/html; charset=utf-8"
	case FormatJUnit:
		return "application/xml"
	}
	return ""
}

// Render produces one report format from the canonical document.
func Render(format string, doc *Document, ts *Timeseries) ([]byte, error) {
	switch format {
	case FormatJSON:
		return JSON(doc)
	case FormatMD:
		return []byte(Markdown(doc)), nil
	case FormatHTML:
		return HTML(doc, ts)
	case FormatJUnit:
		return JUnit(doc)
	}
	return nil, fmt.Errorf("unknown report format %q", format)
}

func JSON(doc *Document) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func verdictOf(doc *Document) string {
	if doc.Verdict == nil {
		return "in progress"
	}
	return *doc.Verdict
}

func num(v float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.4f", v), "0"), ".")
}

func optNum(v *float64, none string) string {
	if v == nil {
		return none
	}
	return num(*v)
}

func passFail(ok bool) string {
	if ok {
		return "pass"
	}
	return "FAIL"
}

// evidenceKeys returns evidence keys in a stable order.
func evidenceKeys(e map[string]any) []string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func evidenceValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "none"
	case string:
		return x
	case float64:
		return num(x)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func window(f FaultResult) string {
	if f.StartAt == nil || f.EndAt == nil {
		return "not installed"
	}
	return f.StartAt.UTC().Format("15:04:05") + " to " + f.EndAt.UTC().Format("15:04:05") + " UTC"
}

func routeNames(m map[string]RouteMetrics) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func mdCell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ", "\r", "").Replace(s)
}

// Markdown renders a report suitable as a pull request comment.
func Markdown(doc *Document) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Faultline report: %s\n\n", mdCell(doc.Experiment.Name))
	fmt.Fprintf(&b, "**Verdict: %s**", strings.ToUpper(verdictOf(doc)))
	if doc.Reason != nil {
		fmt.Fprintf(&b, " (%s)", mdCell(*doc.Reason))
	}
	b.WriteString("\n\n")
	if doc.Experiment.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", mdCell(doc.Experiment.Description))
	}
	fmt.Fprintf(&b, "| | |\n|---|---|\n| Run | `%s` |\n| Status | %s |\n| Environment | %s |\n| Started | %s |\n",
		doc.RunID, doc.Status, mdCell(doc.Environment), doc.StartedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
	if doc.EndedAt != nil {
		fmt.Fprintf(&b, "| Ended | %s |\n", doc.EndedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
	}
	fmt.Fprintf(&b, "| Recovery | %s |\n", optNum(doc.RecoverySeconds, "not measured")+recoveryUnit(doc))

	b.WriteString("\n## Hypothesis\n\n")
	if len(doc.Hypothesis) == 0 {
		b.WriteString("No hypothesis thresholds were evaluated.\n")
	} else {
		b.WriteString("| Threshold | Limit | Observed | Result |\n|---|---|---|---|\n")
		for _, h := range doc.Hypothesis {
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", h.Name, num(h.Threshold), optNum(h.Observed, "no data"), passFail(h.Passed))
		}
	}

	b.WriteString("\n## Findings\n\n")
	if len(doc.Findings) == 0 {
		b.WriteString("No findings.\n")
	}
	for _, f := range doc.Findings {
		fmt.Fprintf(&b, "### [%s] %s\n\n%s\n\n", f.Severity, mdCell(f.Title), mdCell(f.Detail))
		fmt.Fprintf(&b, "Evidence (`%s`):\n\n", f.ID)
		for _, k := range evidenceKeys(f.Evidence) {
			fmt.Fprintf(&b, "- %s: %s\n", k, mdCell(evidenceValue(f.Evidence[k])))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n## Faults\n\n| Fault | Type | Route | Triggers | Window |\n|---|---|---|---|---|\n")
	for _, f := range doc.Faults {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %s |\n", mdCell(f.ID), f.Type, f.Route, f.Triggers, window(f))
	}

	d := doc.Metrics.Driver
	b.WriteString("\n## Metrics\n\nDriver, injection phase:\n\n")
	fmt.Fprintf(&b, "| Requests | Errors | Error rate | p50 | p95 |\n|---|---|---|---|---|\n| %d | %d | %s | %s ms | %s ms |\n",
		d.Requests, d.Errors, num(d.ErrorRate), num(d.P50Ms), num(d.P95Ms))
	if len(doc.Metrics.Proxy) > 0 {
		b.WriteString("\nProxy, by route:\n\n| Route | Requests | Faulted |\n|---|---|---|\n")
		for _, name := range routeNames(doc.Metrics.Proxy) {
			fmt.Fprintf(&b, "| %s | %d | %d |\n", name, doc.Metrics.Proxy[name].Requests, doc.Metrics.Proxy[name].Faulted)
		}
	}
	if doc.Redaction.Enabled {
		fmt.Fprintf(&b, "\nHeader values redacted: %s. Bodies are never stored.\n", strings.Join(doc.Redaction.Headers, ", "))
	}
	return b.String()
}

func recoveryUnit(doc *Document) string {
	if doc.RecoverySeconds == nil {
		return ""
	}
	return " s"
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
}

type junitSuite struct {
	XMLName   xml.Name    `xml:"testsuite"`
	Name      string      `xml:"name,attr"`
	Tests     int         `xml:"tests,attr"`
	Failures  int         `xml:"failures,attr"`
	Errors    int         `xml:"errors,attr"`
	Skipped   int         `xml:"skipped,attr"`
	Time      string      `xml:"time,attr"`
	Timestamp string      `xml:"timestamp,attr"`
	Cases     []junitCase `xml:"testcase"`
}

// JUnit renders one testsuite: a testcase per hypothesis entry, one per
// fault, and a failing "run not aborted" case for aborted runs.
func JUnit(doc *Document) ([]byte, error) {
	class := "faultline." + doc.Experiment.Name
	duration := 0.0
	if doc.EndedAt != nil {
		duration = doc.EndedAt.Sub(doc.StartedAt.Time).Seconds()
	}
	s := junitSuite{
		Name: doc.Experiment.Name, Time: fmt.Sprintf("%.0f", duration),
		Timestamp: doc.StartedAt.UTC().Format("2006-01-02T15:04:05"),
	}
	add := func(name string, f *junitFailure) {
		s.Cases = append(s.Cases, junitCase{Name: name, Classname: class, Time: "0", Failure: f})
		s.Tests++
		if f != nil {
			s.Failures++
		}
	}
	for _, h := range doc.Hypothesis {
		var f *junitFailure
		if !h.Passed {
			msg := fmt.Sprintf("%s: observed %s, threshold %s", h.Name, optNum(h.Observed, "no data"), num(h.Threshold))
			f = &junitFailure{Message: msg, Type: "steady_state_breach", Text: msg + " (run " + doc.RunID + ")"}
		}
		add(h.Name, f)
	}
	for _, ft := range doc.Faults {
		var f *junitFailure
		if ft.Triggers == 0 {
			msg := fmt.Sprintf("fault %s (%s on route %s) had zero triggers", ft.ID, ft.Type, ft.Route)
			f = &junitFailure{Message: msg, Type: "fault_not_triggered", Text: msg + " (run " + doc.RunID + ")"}
		}
		add("fault "+ft.ID+" triggered", f)
	}
	if doc.Status == StatusAborted {
		reason := "unknown"
		if doc.Reason != nil {
			reason = *doc.Reason
		}
		add("run not aborted", &junitFailure{Message: "run aborted: " + reason, Type: "aborted", Text: "run " + doc.RunID + " was aborted: " + reason})
	}
	out, err := xml.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(out, '\n')...), nil
}
