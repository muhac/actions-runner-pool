package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	metricJobStatuses    = []string{"pending", "dispatched", "in_progress", "completed"}
	metricRunnerStatuses = []string{"starting", "idle", "busy", "finished"}
)

type MetricsHandler struct {
	Cfg   *config.Config
	Store store.Store
	Log   *slog.Logger

	handler http.Handler
}

func NewMetricsHandler(cfg *config.Config, st store.Store, log *slog.Logger) *MetricsHandler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(&summaryCollector{cfg: cfg, store: st})
	return &MetricsHandler{
		Cfg:     cfg,
		Store:   st,
		Log:     log,
		handler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{}),
	}
}

func (h *MetricsHandler) Get(w http.ResponseWriter, r *http.Request) {
	if !authorizedBearer(h.Cfg, r.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h.handler.ServeHTTP(w, r)
}

type summaryCollector struct {
	cfg   *config.Config
	store store.Store
}

var (
	jobsTotalDesc = prometheus.NewDesc(
		"gharp_jobs_total",
		"Current number of jobs by status.",
		[]string{"status"},
		nil,
	)
	runnersTotalDesc = prometheus.NewDesc(
		"gharp_runners_total",
		"Current number of runners by status.",
		[]string{"status"},
		nil,
	)
	activeRunnersDesc = prometheus.NewDesc(
		"gharp_active_runners",
		"Current number of active runners.",
		nil,
		nil,
	)
	maxConcurrentRunnersDesc = prometheus.NewDesc(
		"gharp_max_concurrent_runners",
		"Configured maximum number of concurrent runners.",
		nil,
		nil,
	)
)

func (c *summaryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- jobsTotalDesc
	ch <- runnersTotalDesc
	ch <- activeRunnersDesc
	ch <- maxConcurrentRunnersDesc
}

func (c *summaryCollector) Collect(ch chan<- prometheus.Metric) {
	// prometheus.Collector does not thread a request context; use a timeout so
	// a slow DB query does not block the scrape indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	summary, err := c.store.Summary(ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(jobsTotalDesc, err)
		return
	}

	for _, status := range metricJobStatuses {
		ch <- prometheus.MustNewConstMetric(jobsTotalDesc, prometheus.GaugeValue, float64(summary.JobsByStatus[status]), status)
	}
	for _, status := range metricRunnerStatuses {
		ch <- prometheus.MustNewConstMetric(runnersTotalDesc, prometheus.GaugeValue, float64(summary.RunnersByStatus[status]), status)
	}

	maxConcurrent := 0
	if c.cfg != nil {
		maxConcurrent = c.cfg.MaxConcurrentRunners
	}
	ch <- prometheus.MustNewConstMetric(activeRunnersDesc, prometheus.GaugeValue, float64(summary.ActiveRunners))
	ch <- prometheus.MustNewConstMetric(maxConcurrentRunnersDesc, prometheus.GaugeValue, float64(maxConcurrent))
}
