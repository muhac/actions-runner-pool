package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/github"
	"github.com/muhac/actions-runner-pool/internal/httpapi/handlers"
	"github.com/muhac/actions-runner-pool/internal/scheduler"
	"github.com/muhac/actions-runner-pool/internal/store"
)

func NewRouter(cfg *config.Config, st store.Store, gh *github.Client, sch *scheduler.Scheduler, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	dashboard := &handlers.DashboardHandler{Log: log}
	mux.HandleFunc("GET /{$}", dashboard.Get)

	mux.HandleFunc("GET /healthz", handlers.Health)

	setup := &handlers.SetupHandler{Cfg: cfg, Store: st, Log: log}
	mux.HandleFunc("GET /setup", setup.Get)

	jobs := &handlers.JobsHandler{Cfg: cfg, Store: st, Scheduler: sch, Log: log}
	mux.HandleFunc("GET /jobs", jobs.Get)
	mux.HandleFunc("GET /jobs/{job_id}", jobs.GetByID)
	mux.HandleFunc("POST /jobs/{job_id}/retry", jobs.Retry)
	mux.HandleFunc("POST /jobs/{job_id}/cancel", jobs.Cancel)

	stats := &handlers.StatsHandler{Cfg: cfg, Store: st, Log: log}
	mux.HandleFunc("GET /stats", stats.Get)

	metrics := handlers.NewMetricsHandler(cfg, st, log)
	mux.HandleFunc("GET /metrics", metrics.Get)

	cb := &handlers.CallbackHandler{Cfg: cfg, Store: st, GitHub: gh, Log: log}
	mux.HandleFunc("GET /github/app/callback", cb.Get)

	wh := &handlers.WebhookHandler{Cfg: cfg, Store: st, Scheduler: sch, Log: log}
	mux.HandleFunc("POST /github/webhook", wh.Post)

	return mux
}
