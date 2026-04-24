package handlers

import (
	"log/slog"
	"net/http"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/scheduler"
	"github.com/muhac/actions-runner-pool/internal/store"
)

type WebhookHandler struct {
	Cfg       *config.Config
	Store     store.Store
	Scheduler *scheduler.Scheduler
	Log       *slog.Logger
}

// POST /github/webhook
// 1. Read raw body, verify X-Hub-Signature-256 against app_config.WebhookSecret.
// 2. Switch on X-GitHub-Event: workflow_job, installation, installation_repositories.
// 3. For workflow_job:queued — InsertJobIfNew, then Scheduler.Enqueue.
// 4. Always return 200 fast.
func (h *WebhookHandler) Post(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "webhook not yet implemented", http.StatusNotImplemented)
}
