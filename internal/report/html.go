package report

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"math"
	"strings"
	"time"
)

// The HTML report is one self-contained file: inline CSS, inline JS, inline
// SVG, and no reference to anything outside the file.

type htmlTile struct{ Label, Value, Note string }

type htmlFinding struct {
	Finding
	Keys []htmlKV
}

type htmlKV struct{ Key, Value string }

type htmlRoute struct {
	Name string
	RouteMetrics
}

type htmlPoint struct {
	Time            string
	Requests        int
	Errors          int
	ErrorRate, P95  string
	Faulted         int
	ProbeOK, ProbeF int
}

type htmlData struct {
	Doc         *Document
	Verdict     string
	VerdictMark string
	Reason      string
	Started     string
	Ended       string
	Duration    string
	Tiles       []htmlTile
	Chart       template.HTML
	Hypothesis  []htmlHyp
	Findings    []htmlFinding
	Faults      []htmlFault
	Routes      []htmlRoute
	Points      []htmlPoint
	Headers     string
}

type htmlHyp struct {
	Name, Threshold, Observed, Result string
	Passed                            bool
}

type htmlFault struct {
	FaultResult
	Window string
}

var verdictMarks = map[string]string{
	VerdictPassed: "✓", VerdictFailed: "✕", VerdictInconclusive: "?", VerdictAborted: "■",
}

func HTML(doc *Document, ts *Timeseries) ([]byte, error) {
	if ts == nil {
		ts = &Timeseries{}
	}
	d := htmlData{Doc: doc, Verdict: verdictOf(doc), Started: doc.StartedAt.UTC().Format("2006-01-02 15:04:05 UTC")}
	d.VerdictMark = verdictMarks[d.Verdict]
	if d.VerdictMark == "" {
		d.VerdictMark = "…"
	}
	if doc.Reason != nil {
		d.Reason = *doc.Reason
	}
	if doc.EndedAt != nil {
		d.Ended = doc.EndedAt.UTC().Format("2006-01-02 15:04:05 UTC")
		d.Duration = doc.EndedAt.Sub(doc.StartedAt.Time).Round(time.Second).String()
	}

	var triggers int64
	for _, f := range doc.Faults {
		triggers += f.Triggers
		d.Faults = append(d.Faults, htmlFault{f, window(f)})
	}
	peak, peakNote := 0.0, "no error rate threshold"
	for _, p := range ts.Series {
		if p.Driver.Requests > 0 {
			peak = math.Max(peak, float64(p.Driver.Errors)/float64(p.Driver.Requests))
		}
	}
	var errLimit *float64
	for _, h := range doc.Hypothesis {
		if h.Name == "error_rate_max" {
			t := h.Threshold
			errLimit = &t
			peakNote = "threshold " + pct(h.Threshold)
		}
		d.Hypothesis = append(d.Hypothesis, htmlHyp{h.Name, num(h.Threshold), optNum(h.Observed, "no data"), passFail(h.Passed), h.Passed})
	}
	recovery, recoveryNote := "not measured", ""
	if doc.RecoverySeconds != nil {
		recovery, recoveryNote = num(*doc.RecoverySeconds)+" s", "after faults cleared"
	}
	d.Tiles = []htmlTile{
		{"Requests", fmt.Sprint(doc.Metrics.Driver.Requests), fmt.Sprintf("%d errors during injection", doc.Metrics.Driver.Errors)},
		{"Faults injected", fmt.Sprint(triggers), fmt.Sprintf("%d fault(s) configured", len(doc.Faults))},
		{"Peak error rate", pct(peak), peakNote},
		{"Recovery time", recovery, recoveryNote},
	}
	for _, f := range doc.Findings {
		hf := htmlFinding{Finding: f}
		for _, k := range evidenceKeys(f.Evidence) {
			hf.Keys = append(hf.Keys, htmlKV{k, evidenceValue(f.Evidence[k])})
		}
		d.Findings = append(d.Findings, hf)
	}
	for _, name := range routeNames(doc.Metrics.Proxy) {
		d.Routes = append(d.Routes, htmlRoute{name, doc.Metrics.Proxy[name]})
	}
	for _, p := range ts.Series {
		rate := "-"
		if p.Driver.Requests > 0 {
			rate = pct(float64(p.Driver.Errors) / float64(p.Driver.Requests))
		}
		d.Points = append(d.Points, htmlPoint{
			Time: p.T.UTC().Format("15:04:05"), Requests: p.Driver.Requests, Errors: p.Driver.Errors,
			ErrorRate: rate, P95: num(p.Driver.P95Ms), Faulted: p.Proxy.Faulted, ProbeOK: p.Probes.OK, ProbeF: p.Probes.Fail,
		})
	}
	d.Headers = strings.Join(doc.Redaction.Headers, ", ")
	d.Chart = template.HTML(chartSVG(ts, errLimit))

	var buf bytes.Buffer
	if err := htmlTemplate.Execute(&buf, d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func pct(v float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.1f", v*100), "0"), ".") + "%"
}

// chartSVG draws error rate and p95 latency over time, with fault windows
// shaded, phase boundaries and abort markers. Lines differ in dash pattern
// as well as colour.
func chartSVG(ts *Timeseries, errLimit *float64) string {
	const (
		w, h         = 960.0, 300.0
		left, right  = 52.0, 60.0
		top, bottom  = 28.0, 34.0
		plotW, plotH = w - left - right, h - top - bottom
	)
	if len(ts.Series) == 0 {
		return `<p class="muted">No timeline data was recorded for this run.</p>`
	}
	t0 := ts.Series[0].T.Unix()
	span := float64(ts.Series[len(ts.Series)-1].T.Unix()-t0) + 1
	x := func(t time.Time) float64 {
		v := (float64(t.Unix()-t0) + 0.5) / span
		return left + math.Min(math.Max(v, 0), 1)*plotW
	}
	edge := func(t time.Time) float64 {
		v := float64(t.Unix()-t0) / span
		return left + math.Min(math.Max(v, 0), 1)*plotW
	}
	maxP95 := 1.0
	for _, p := range ts.Series {
		maxP95 = math.Max(maxP95, p.Driver.P95Ms)
	}
	maxP95 = niceCeil(maxP95)
	yRate := func(v float64) float64 { return top + (1-math.Min(v, 1))*plotH }
	yP95 := func(v float64) float64 { return top + (1-v/maxP95)*plotH }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %.0f %.0f" role="img" aria-labelledby="chart-title chart-desc">`, w, h)
	b.WriteString(`<title id="chart-title">Run timeline</title><desc id="chart-desc">Error rate and p95 latency per second, with fault windows, phase boundaries and abort markers. The same numbers are in the timeline data table below.</desc>`)

	for i := 0; i <= 4; i++ {
		frac := float64(i) / 4
		y := top + (1-frac)*plotH
		fmt.Fprintf(&b, `<line class="grid" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`, left, y, left+plotW, y)
		fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="%.1f" text-anchor="end">%.0f%%</text>`, left-6, y+4, frac*100)
		fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="%.1f">%s ms</text>`, left+plotW+6, y+4, num(frac*maxP95))
	}

	for _, fw := range ts.FaultWindows {
		x1, x2 := edge(fw.Start.Time), edge(fw.End.Time)
		fmt.Fprintf(&b, `<rect class="fault-window" x="%.1f" y="%.1f" width="%.1f" height="%.1f"/>`, x1, top, math.Max(x2-x1, 1), plotH)
		fmt.Fprintf(&b, `<text class="label" x="%.1f" y="%.1f">fault %s</text>`, x1+4, top+12, html.EscapeString(fw.FaultID))
	}
	for i, ph := range ts.Phases {
		px := edge(ph.Start.Time)
		if i > 0 {
			fmt.Fprintf(&b, `<line class="phase" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`, px, top, px, top+plotH)
		}
		fmt.Fprintf(&b, `<text class="label" x="%.1f" y="%.1f">%s</text>`, px+4, h-10, html.EscapeString(ph.Name))
	}
	if errLimit != nil {
		y := yRate(*errLimit)
		fmt.Fprintf(&b, `<line class="limit" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`, left, y, left+plotW, y)
		fmt.Fprintf(&b, `<text class="label" x="%.1f" y="%.1f" text-anchor="end">error rate limit %s</text>`, left+plotW-4, y-4, pct(*errLimit))
	}

	var rate, p95 []string
	for _, p := range ts.Series {
		if p.Driver.Requests == 0 {
			continue
		}
		px := x(p.T.Time)
		rate = append(rate, fmt.Sprintf("%.1f,%.1f", px, yRate(float64(p.Driver.Errors)/float64(p.Driver.Requests))))
		p95 = append(p95, fmt.Sprintf("%.1f,%.1f", px, yP95(p.Driver.P95Ms)))
	}
	if len(rate) > 0 {
		fmt.Fprintf(&b, `<polyline class="line-rate" points="%s"/>`, strings.Join(rate, " "))
		fmt.Fprintf(&b, `<polyline class="line-p95" points="%s"/>`, strings.Join(p95, " "))
	}
	for _, m := range ts.Markers {
		mx := edge(m.T.Time)
		fmt.Fprintf(&b, `<line class="marker" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`, mx, top-8, mx, top+plotH)
		fmt.Fprintf(&b, `<text class="label marker-label" x="%.1f" y="%.1f" text-anchor="end">%s: %s</text>`, mx-4, top-12, html.EscapeString(m.Type), html.EscapeString(m.Detail))
	}

	colW := plotW / span
	for _, p := range ts.Series {
		label := fmt.Sprintf("%s: %d requests, %d errors, p95 %s ms, %d faulted, probes %d ok / %d failed",
			p.T.UTC().Format("15:04:05"), p.Driver.Requests, p.Driver.Errors, num(p.Driver.P95Ms), p.Proxy.Faulted, p.Probes.OK, p.Probes.Fail)
		fmt.Fprintf(&b, `<rect class="col" x="%.1f" y="%.1f" width="%.1f" height="%.1f" data-label="%s"><title>%s</title></rect>`,
			edge(p.T.Time), top, colW, plotH, html.EscapeString(label), html.EscapeString(label))
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// niceCeil rounds up to 1, 2 or 5 times a power of ten.
func niceCeil(v float64) float64 {
	if v <= 0 {
		return 1
	}
	pow := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 5, 10} {
		if v <= m*pow {
			return m * pow
		}
	}
	return 10 * pow
}

var htmlTemplate = template.Must(template.New("report").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Faultline report: {{.Doc.Experiment.Name}}</title>
<style>
:root{--bg:#ffffff;--fg:#1a1d21;--muted:#5b6470;--line:#d5dae0;--card:#f5f7f9;--pass:#17713b;--fail:#b3261e;--warn:#8a5a00;--info:#1f5fa8;--rate:#b3261e;--p95:#1f5fa8}
@media (prefers-color-scheme:dark){:root{--bg:#14171a;--fg:#e8eaed;--muted:#a0a8b3;--line:#39404a;--card:#1d2126;--pass:#6fd08c;--fail:#ff8a80;--warn:#f2c26b;--info:#8ab4f8;--rate:#ff8a80;--p95:#8ab4f8}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
main{max-width:1040px;margin:0 auto;padding:24px 16px 48px}
h1{font-size:1.5rem;margin:0 0 4px}
h2{font-size:1.1rem;margin:32px 0 10px;padding-bottom:6px;border-bottom:1px solid var(--line)}
h3{font-size:1rem;margin:0 0 6px}
p{margin:6px 0}
.muted{color:var(--muted)}
code{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:.92em}
.verdict{display:flex;gap:12px;align-items:center;margin:18px 0;padding:14px 16px;border:2px solid currentColor;border-radius:8px;font-size:1.15rem;font-weight:600}
.verdict .mark{font-size:1.4rem;line-height:1}
.verdict .reason{font-weight:400;font-size:.95rem}
.v-passed{color:var(--pass)}.v-failed{color:var(--fail)}.v-inconclusive{color:var(--warn)}.v-aborted{color:var(--fail)}
dl.meta{display:grid;grid-template-columns:max-content 1fr;gap:4px 16px;margin:0}
dl.meta dt{color:var(--muted)}dl.meta dd{margin:0;overflow-wrap:anywhere}
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(190px,1fr));gap:12px;margin-top:20px}
.tile{background:var(--card);border:1px solid var(--line);border-radius:8px;padding:12px 14px}
.tile .label{color:var(--muted);font-size:.85rem}
.tile .value{font-size:1.6rem;font-weight:600}
.tile .note{color:var(--muted);font-size:.85rem}
.scroll{overflow-x:auto}
table{border-collapse:collapse;width:100%}
th,td{text-align:left;padding:6px 10px;border-bottom:1px solid var(--line);vertical-align:top}
th{color:var(--muted);font-weight:600;font-size:.85rem}
td.n,th.n{text-align:right;font-variant-numeric:tabular-nums}
.pass{color:var(--pass);font-weight:600}.fail{color:var(--fail);font-weight:600}
.finding{background:var(--card);border:1px solid var(--line);border-left:6px solid var(--muted);border-radius:8px;padding:12px 14px;margin:10px 0;break-inside:avoid}
.sev{display:inline-block;border:1px solid currentColor;border-radius:4px;padding:0 6px;margin-right:8px;font-size:.78rem;font-weight:600;text-transform:uppercase;letter-spacing:.03em}
.s-high{border-left-color:var(--fail)}.s-high .sev{color:var(--fail)}
.s-medium{border-left-color:var(--warn)}.s-medium .sev{color:var(--warn)}
.s-low,.s-info{border-left-color:var(--info)}.s-low .sev,.s-info .sev{color:var(--info)}
.finding table{margin-top:8px;width:auto}
.chart{width:100%;height:auto;display:block}
.chart .grid{stroke:var(--line);stroke-width:1}
.chart .axis,.chart .label{fill:var(--muted);font-size:11px}
.chart .fault-window{fill:var(--fail);fill-opacity:.1;stroke:var(--fail);stroke-width:1;stroke-dasharray:4 3}
.chart .phase{stroke:var(--muted);stroke-width:1;stroke-dasharray:2 4}
.chart .limit{stroke:var(--rate);stroke-width:1;stroke-dasharray:1 3}
.chart .line-rate{fill:none;stroke:var(--rate);stroke-width:2}
.chart .line-p95{fill:none;stroke:var(--p95);stroke-width:2;stroke-dasharray:7 4}
.chart .marker{stroke:var(--fg);stroke-width:2}
.chart .marker-label{fill:var(--fg);font-weight:600}
.chart .col{fill:transparent}
.chart .col:hover{fill:var(--fg);fill-opacity:.07}
.legend{display:flex;flex-wrap:wrap;gap:6px 18px;font-size:.85rem;color:var(--muted);margin-top:6px}
.legend svg{vertical-align:middle;margin-right:4px}
#readout{min-height:1.5em;font-size:.85rem;color:var(--muted);font-variant-numeric:tabular-nums}
details{margin-top:8px}summary{cursor:pointer;color:var(--muted)}
footer{margin-top:36px;color:var(--muted);font-size:.85rem}
@media print{body{font-size:11pt}main{max-width:none;padding:0}details{display:none}#readout{display:none}.tile,.finding{background:none}h2{break-after:avoid}}
</style>
</head>
<body>
<main>
<header>
<h1>Faultline report: {{.Doc.Experiment.Name}}</h1>
{{if .Doc.Experiment.Description}}<p class="muted">{{.Doc.Experiment.Description}}</p>{{end}}
</header>

<p class="verdict v-{{.Verdict}}" role="status"><span class="mark" aria-hidden="true">{{.VerdictMark}}</span><span>Verdict: {{.Verdict}}</span>{{if .Reason}}<span class="reason">reason: {{.Reason}}</span>{{end}}</p>

<dl class="meta">
<dt>Run</dt><dd><code>{{.Doc.RunID}}</code></dd>
<dt>Status</dt><dd>{{.Doc.Status}}</dd>
<dt>Environment</dt><dd>{{.Doc.Environment}}</dd>
<dt>Started</dt><dd>{{.Started}}</dd>
{{if .Ended}}<dt>Ended</dt><dd>{{.Ended}} ({{.Duration}})</dd>{{end}}
<dt>Experiment hash</dt><dd><code>{{.Doc.Experiment.YAMLSHA256}}</code></dd>
</dl>

<section class="tiles" aria-label="Key figures">
{{range .Tiles}}<div class="tile"><div class="label">{{.Label}}</div><div class="value">{{.Value}}</div><div class="note">{{.Note}}</div></div>
{{end}}</section>

<section>
<h2>Timeline</h2>
{{.Chart}}
<div class="legend">
<span><svg width="28" height="8" aria-hidden="true"><line x1="0" y1="4" x2="28" y2="4" stroke="currentColor" stroke-width="2" style="color:var(--rate)"/></svg>error rate (solid, left axis)</span>
<span><svg width="28" height="8" aria-hidden="true"><line x1="0" y1="4" x2="28" y2="4" stroke="currentColor" stroke-width="2" stroke-dasharray="7 4" style="color:var(--p95)"/></svg>p95 latency (dashed, right axis)</span>
<span>shaded, dashed box: fault window</span>
<span>dotted vertical line: phase boundary</span>
<span>thick vertical line: abort</span>
</div>
<p id="readout" aria-live="polite"></p>
<details>
<summary>Timeline data</summary>
<div class="scroll"><table>
<thead><tr><th>Time (UTC)</th><th class="n">Requests</th><th class="n">Errors</th><th class="n">Error rate</th><th class="n">p95 ms</th><th class="n">Faulted</th><th class="n">Probes ok</th><th class="n">Probes failed</th></tr></thead>
<tbody>
{{range .Points}}<tr><td>{{.Time}}</td><td class="n">{{.Requests}}</td><td class="n">{{.Errors}}</td><td class="n">{{.ErrorRate}}</td><td class="n">{{.P95}}</td><td class="n">{{.Faulted}}</td><td class="n">{{.ProbeOK}}</td><td class="n">{{.ProbeF}}</td></tr>
{{end}}</tbody>
</table></div>
</details>
</section>

<section>
<h2>Hypothesis</h2>
{{if .Hypothesis}}<div class="scroll"><table>
<thead><tr><th>Threshold</th><th class="n">Limit</th><th class="n">Observed</th><th>Result</th></tr></thead>
<tbody>
{{range .Hypothesis}}<tr><td><code>{{.Name}}</code></td><td class="n">{{.Threshold}}</td><td class="n">{{.Observed}}</td><td class="{{if .Passed}}pass{{else}}fail{{end}}">{{.Result}}</td></tr>
{{end}}</tbody>
</table></div>{{else}}<p class="muted">No hypothesis thresholds were evaluated.</p>{{end}}
</section>

<section>
<h2>Findings</h2>
{{if not .Findings}}<p class="muted">No findings.</p>{{end}}
{{range .Findings}}<article class="finding s-{{.Severity}}">
<h3><span class="sev">{{.Severity}}</span>{{.Title}}</h3>
<p>{{.Detail}}</p>
<table aria-label="Evidence for {{.ID}}">
<thead><tr><th>Evidence (<code>{{.ID}}</code>)</th><th>Value</th></tr></thead>
<tbody>
{{range .Keys}}<tr><td><code>{{.Key}}</code></td><td>{{.Value}}</td></tr>
{{end}}</tbody>
</table>
</article>
{{end}}</section>

<section>
<h2>Faults</h2>
<div class="scroll"><table>
<thead><tr><th>Fault</th><th>Type</th><th>Route</th><th class="n">Triggers</th><th>Window</th></tr></thead>
<tbody>
{{range .Faults}}<tr><td><code>{{.ID}}</code></td><td>{{.Type}}</td><td>{{.Route}}</td><td class="n">{{.Triggers}}</td><td>{{.Window}}</td></tr>
{{end}}</tbody>
</table></div>
</section>

<section>
<h2>Metrics</h2>
<p class="muted">Driver, injection phase only.</p>
<div class="scroll"><table>
<thead><tr><th class="n">Requests</th><th class="n">Errors</th><th class="n">Error rate</th><th class="n">p50 ms</th><th class="n">p95 ms</th></tr></thead>
<tbody><tr><td class="n">{{.Doc.Metrics.Driver.Requests}}</td><td class="n">{{.Doc.Metrics.Driver.Errors}}</td><td class="n">{{.Doc.Metrics.Driver.ErrorRate}}</td><td class="n">{{.Doc.Metrics.Driver.P50Ms}}</td><td class="n">{{.Doc.Metrics.Driver.P95Ms}}</td></tr></tbody>
</table></div>
{{if .Routes}}<p class="muted">Proxy, by route.</p>
<div class="scroll"><table>
<thead><tr><th>Route</th><th class="n">Requests</th><th class="n">Faulted</th></tr></thead>
<tbody>
{{range .Routes}}<tr><td>{{.Name}}</td><td class="n">{{.Requests}}</td><td class="n">{{.Faulted}}</td></tr>
{{end}}</tbody>
</table></div>{{end}}
</section>

<footer>
<p>Generated by Faultline from run <code>{{.Doc.RunID}}</code>, report schema {{.Doc.SchemaVersion}}.{{if .Doc.Redaction.Enabled}} Values of these headers are redacted wherever they could be stored or shown: {{.Headers}}. Request and response bodies are never stored.{{end}}</p>
</footer>
</main>
<script>
(function () {
  var readout = document.getElementById("readout");
  if (!readout) { return; }
  var cols = document.querySelectorAll(".chart .col");
  for (var i = 0; i < cols.length; i++) {
    cols[i].addEventListener("mouseenter", function (e) { readout.textContent = e.target.getAttribute("data-label"); });
  }
})();
</script>
</body>
</html>
`))
