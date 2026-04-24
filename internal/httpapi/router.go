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

	mux.HandleFunc("GET /healthz", handlers.Health)

	setup := &handlers.SetupHandler{Cfg: cfg, Store: st, Log: log}
	mux.HandleFunc("GET /setup", setup.Get)

	cb := &handlers.CallbackHandler{Cfg: cfg, Store: st, GitHub: gh, Log: log}
	mux.HandleFunc("GET /github/app/callback", cb.Get)

	wh := &handlers.WebhookHandler{Cfg: cfg, Store: st, Scheduler: sch, Log: log}
	mux.HandleFunc("POST /github/webhook", wh.Post)

	return mux
}
