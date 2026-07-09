package observability

import (
	"encoding/json"
	"html/template"
	"net/http"
	"time"
)

// StageLatencySummary is the dashboard's rendering of latency percentiles
// for a single pipeline stage.
type StageLatencySummary struct {
	Stage string
	Count int
	P50   float64
	P90   float64
	P95   float64
	P99   float64
}

// DashboardData is the full point-in-time snapshot rendered by the
// dashboard: latency percentiles per stage, error rates per (stage,
// vendor), and running cost per vendor. It is exported so callers (and
// tests) can inspect the aggregated view directly, independent of how it's
// rendered (HTML table or JSON).
type DashboardData struct {
	GeneratedAt time.Time
	Latency     []StageLatencySummary
	Errors      []ErrorStats
	Costs       []CostStats
}

// BuildDashboardData takes a point-in-time snapshot of r and shapes it into
// the aggregated view served by the dashboard.
func BuildDashboardData(r *LatencyRecorder) DashboardData {
	data := DashboardData{
		GeneratedAt: time.Now().UTC(),
		Errors:      r.ErrorSnapshot(),
		Costs:       r.CostSnapshot(),
	}

	for _, stage := range r.Stages() {
		data.Latency = append(data.Latency, StageLatencySummary{
			Stage: stage,
			Count: r.Count(stage),
			P50:   r.Percentile(stage, 50),
			P90:   r.Percentile(stage, 90),
			P95:   r.Percentile(stage, 95),
			P99:   r.Percentile(stage, 99),
		})
	}

	return data
}

// dashboardTemplate renders DashboardData as a minimal, human-readable HTML
// page: one table per section (latency percentiles, error rates, per-vendor
// cost). No external assets/JS are used so it renders standalone.
var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"pct": formatPercent,
}).Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>LangStream Observability Dashboard</title>
<style>
body { font-family: -apple-system, Helvetica, Arial, sans-serif; margin: 2rem; color: #1a1a1a; }
h1 { font-size: 1.4rem; }
h2 { font-size: 1.1rem; margin-top: 2rem; }
table { border-collapse: collapse; width: 100%; max-width: 900px; margin-top: 0.5rem; }
th, td { border: 1px solid #ddd; padding: 0.4rem 0.7rem; text-align: right; font-size: 0.9rem; }
th, td:first-child { text-align: left; }
th { background: #f4f4f4; }
caption { text-align: left; color: #666; font-size: 0.8rem; margin-bottom: 0.3rem; }
.empty { color: #888; font-style: italic; }
</style>
</head>
<body>
<h1>LangStream Observability Dashboard</h1>
<p>Generated at {{ .GeneratedAt.Format "2006-01-02T15:04:05Z07:00" }}</p>

<h2>Latency percentiles (ms)</h2>
{{ if .Latency }}
<table>
<tr><th>Stage</th><th>Count</th><th>p50</th><th>p90</th><th>p95</th><th>p99</th></tr>
{{ range .Latency }}
<tr><td>{{ .Stage }}</td><td>{{ .Count }}</td><td>{{ printf "%.2f" .P50 }}</td><td>{{ printf "%.2f" .P90 }}</td><td>{{ printf "%.2f" .P95 }}</td><td>{{ printf "%.2f" .P99 }}</td></tr>
{{ end }}
</table>
{{ else }}
<p class="empty">No latency samples recorded yet.</p>
{{ end }}

<h2>Error rates by stage/vendor</h2>
{{ if .Errors }}
<table>
<tr><th>Stage</th><th>Vendor</th><th>Errors</th><th>Total</th><th>Rate</th></tr>
{{ range .Errors }}
<tr><td>{{ .Stage }}</td><td>{{ .Vendor }}</td><td>{{ .Errors }}</td><td>{{ .Total }}</td><td>{{ pct .Rate }}</td></tr>
{{ end }}
</table>
{{ else }}
<p class="empty">No error/event data recorded yet.</p>
{{ end }}

<h2>Per-vendor cost</h2>
{{ if .Costs }}
<table>
<tr><th>Vendor</th><th>Total (USD)</th><th>Cost events</th></tr>
{{ range .Costs }}
<tr><td>{{ .Vendor }}</td><td>{{ printf "%.4f" .TotalUSD }}</td><td>{{ .Events }}</td></tr>
{{ end }}
</table>
{{ else }}
<p class="empty">No cost data recorded yet.</p>
{{ end }}

</body>
</html>
`))

// formatPercent renders a [0,1] ratio as a percentage string, e.g. 0.125 ->
// "12.50%".
func formatPercent(f float64) string {
	return formatFloat(f*100) + "%"
}

// NewDashboardHandler returns an http.Handler serving a human-readable
// observability dashboard backed by r:
//
//   - GET /            HTML page with latency/error/cost tables.
//   - GET /dashboard.json  the same data as JSON, for programmatic access.
//   - GET /metrics     Prometheus-text-exposition-format dump (delegates to
//     r.WriteText), for scraping.
//
// The handler holds no state of its own beyond r, so it is safe to mount
// under any prefix via http.StripPrefix, and safe to exercise in tests via
// httptest.NewRecorder without binding a real network port.
func NewDashboardHandler(r *LatencyRecorder) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/" {
			http.NotFound(w, req)
			return
		}
		data := BuildDashboardData(r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := dashboardTemplate.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	mux.HandleFunc("/dashboard.json", func(w http.ResponseWriter, req *http.Request) {
		data := BuildDashboardData(r)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := r.WriteText(w); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	return mux
}

// NewDashboardServer wires an *http.Server around NewDashboardHandler(r),
// listening on addr when Serve/ListenAndServe is later called by the
// caller (e.g. cmd/langstream at startup). Constructing the server does not
// bind a socket -- that only happens when ListenAndServe (or similar) is
// invoked -- so this function itself is trivially unit-testable.
func NewDashboardServer(addr string, r *LatencyRecorder) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           NewDashboardHandler(r),
		ReadHeaderTimeout: 5 * time.Second,
	}
}
