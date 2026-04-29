package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/scheduler"
	"github.com/muhac/actions-runner-pool/internal/store"
)

// jobEnqueuer is the subset of *scheduler.Scheduler the webhook needs;
// extracted as an interface so tests can pass a spy.
type jobEnqueuer interface {
	Enqueue(jobID int64)
}

type WebhookHandler struct {
	Cfg       *config.Config
	Store     store.Store
	Scheduler jobEnqueuer
	Log       *slog.Logger
}

// maxWebhookBodyBytes caps how much we'll read from a webhook request before
// authentication. GitHub's largest documented payload is well under 1MB; this
// is generous-but-bounded so an unauthenticated caller can't OOM us.
const maxWebhookBodyBytes = 1 << 20 // 1 MiB

// POST /github/webhook
//
// 200 on success, 401 on bad signature, 400 on bad body, 413 on oversize body,
// 5xx ONLY on the queued path when the store fails (so GitHub retries; the
// INSERT-OR-IGNORE dedupe makes retry safe). Bookkeeping store errors on
// in_progress / completed are logged and swallowed to keep GitHub from
// retry-storming us.
func (h *WebhookHandler) Post(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	defer func() { _ = r.Body.Close() }()

	cfg, err := h.Store.GetAppConfig(r.Context())
	if err != nil {
		h.logError("load app config for webhook", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if cfg == nil {
		http.Error(w, "app not configured", http.StatusServiceUnavailable)
		return
	}
	if !verifySignature(cfg.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}

	switch r.Header.Get("X-GitHub-Event") {
	case "ping":
		w.WriteHeader(http.StatusOK)
	case "installation":
		h.handleInstallation(w, r, body)
	case "installation_repositories":
		h.handleInstallationRepositories(w, r, body)
	case "workflow_job":
		h.handleWorkflowJob(w, r, body)
	default:
		// Quietly accept events we did not subscribe to (defensive).
		w.WriteHeader(http.StatusOK)
	}
}

// ---------------- HMAC ----------------

func verifySignature(secret string, body []byte, header string) bool {
	// An empty secret would happily verify a forged HMAC. Treat it as a
	// fatal misconfiguration — no webhook is "anyone can post".
	if secret == "" {
		return false
	}
	if !strings.HasPrefix(header, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(header), []byte(expected))
}

// ---------------- installation ----------------

type installationEvent struct {
	Action       string `json:"action"`
	Installation struct {
		ID      int64 `json:"id"`
		Account struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
	} `json:"installation"`
	Repositories []struct {
		FullName string `json:"full_name"`
	} `json:"repositories"`
}

func (h *WebhookHandler) handleInstallation(w http.ResponseWriter, r *http.Request, body []byte) {
	var ev installationEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "decode installation event", http.StatusBadRequest)
		return
	}

	switch ev.Action {
	case "created":
		if err := h.Store.UpsertInstallation(r.Context(), &store.Installation{
			ID:           ev.Installation.ID,
			AccountID:    ev.Installation.Account.ID,
			AccountLogin: ev.Installation.Account.Login,
			AccountType:  ev.Installation.Account.Type,
		}); err != nil {
			h.logError("upsert installation", err)
		}
		for _, repo := range ev.Repositories {
			if err := h.Store.UpsertRepoInstallation(r.Context(), repo.FullName, ev.Installation.ID); err != nil {
				h.logError("upsert repo->installation", err)
			}
		}
	case "deleted":
		// The App was uninstalled. Remove the repo→installation rows and
		// cancel any still-dispatchable jobs for those repos: dispatch
		// would otherwise loop forever on jobs whose installation token
		// can no longer be minted (and the user already told GitHub
		// they're done with this).
		for _, repo := range ev.Repositories {
			n, err := h.Store.CancelPendingJobsForRepo(r.Context(), repo.FullName)
			if err != nil {
				h.logError("cancel pending jobs after installation deleted", err)
			} else if n > 0 && h.Log != nil {
				h.Log.Info("installation deleted: cancelled pending jobs", "repo", repo.FullName, "cancelled", n)
			}
			if err := h.Store.RemoveRepoInstallation(r.Context(), repo.FullName); err != nil {
				h.logError("remove repo->installation after installation deleted", err)
			}
		}
		if h.Log != nil {
			h.Log.Info("installation deleted", "installation_id", ev.Installation.ID, "repos", len(ev.Repositories))
		}
	}
	w.WriteHeader(http.StatusOK)
}

// ---------------- installation_repositories ----------------

type installationRepositoriesEvent struct {
	Action       string `json:"action"` // added | removed
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	RepositoriesAdded []struct {
		FullName string `json:"full_name"`
	} `json:"repositories_added"`
	RepositoriesRemoved []struct {
		FullName string `json:"full_name"`
	} `json:"repositories_removed"`
}

func (h *WebhookHandler) handleInstallationRepositories(w http.ResponseWriter, r *http.Request, body []byte) {
	var ev installationRepositoriesEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "decode installation_repositories event", http.StatusBadRequest)
		return
	}
	for _, repo := range ev.RepositoriesAdded {
		if err := h.Store.UpsertRepoInstallation(r.Context(), repo.FullName, ev.Installation.ID); err != nil {
			h.logError("upsert repo->installation", err)
		}
	}
	for _, repo := range ev.RepositoriesRemoved {
		// Cancel before removing the installation mapping so dispatch,
		// if it raced us, finds the same "no longer dispatchable" state
		// the install removal implies.
		n, err := h.Store.CancelPendingJobsForRepo(r.Context(), repo.FullName)
		if err != nil {
			h.logError("cancel pending jobs after repo removed", err)
		} else if n > 0 && h.Log != nil {
			h.Log.Info("repo removed from installation: cancelled pending jobs", "repo", repo.FullName, "cancelled", n)
		}
		if err := h.Store.RemoveRepoInstallation(r.Context(), repo.FullName); err != nil {
			h.logError("remove repo->installation", err)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// ---------------- workflow_job ----------------

func (h *WebhookHandler) handleWorkflowJob(w http.ResponseWriter, r *http.Request, body []byte) {
	var ev scheduler.WorkflowJobEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "decode workflow_job event", http.StatusBadRequest)
		return
	}

	if !labelsMatch(ev.WorkflowJob.Labels, h.Cfg.RunnerLabels) {
		w.WriteHeader(http.StatusOK)
		return
	}

	switch ev.Action {
	case "queued":
		// Lazy-write the repo->installation mapping in case we missed
		// the installation event (resilience).
		if ev.Installation.ID != 0 && ev.Repository.FullName != "" {
			if err := h.Store.UpsertRepoInstallation(r.Context(), ev.Repository.FullName, ev.Installation.ID); err != nil {
				h.logError("lazy upsert repo->installation", err)
			}
		}

		job := &store.Job{
			ID:        ev.WorkflowJob.ID,
			Repo:      ev.Repository.FullName,
			Action:    "queued",
			Labels:    strings.Join(ev.WorkflowJob.Labels, ","),
			DedupeKey: strconv.FormatInt(ev.WorkflowJob.ID, 10),
			Status:    "pending",
		}
		inserted, err := h.Store.InsertJobIfNew(r.Context(), job)
		if err != nil {
			h.logError("insert job", err)
			http.Error(w, "store unavailable", http.StatusServiceUnavailable)
			return
		}
		if inserted {
			h.Scheduler.Enqueue(job.ID)
		}
		w.WriteHeader(http.StatusOK)

	case "in_progress":
		// Empty runner_name on an in_progress event means GitHub fired
		// the transition before binding a real runner — observed in
		// dispatch-race situations (we launched a runner but a different
		// runner won the assignment, leaving this row with no real
		// owner). Skipping preserves the 'pending'/'dispatched' status
		// so the scheduler's replay can still rescue the job.
		if ev.WorkflowJob.RunnerName == "" {
			if h.Log != nil {
				h.Log.Warn("webhook: in_progress with empty runner_name, skipping", "job_id", ev.WorkflowJob.ID)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		advanced, err := h.Store.MarkJobInProgress(r.Context(), ev.WorkflowJob.ID, ev.WorkflowJob.RunnerID, ev.WorkflowJob.RunnerName)
		if err != nil {
			h.logError("mark job in_progress", err)
		}
		// Only flip the runner to busy if the job row actually advanced.
		// A late in_progress arriving after completed leaves the job
		// untouched; we must not resurrect a finished runner.
		if advanced {
			if err := h.Store.UpdateRunnerStatusByName(r.Context(), ev.WorkflowJob.RunnerName, "busy"); err != nil {
				h.logError("update runner status busy", err)
			}
		}
		w.WriteHeader(http.StatusOK)

	case "completed":
		if err := h.Store.MarkJobCompleted(r.Context(), ev.WorkflowJob.ID, ev.WorkflowJob.Conclusion); err != nil {
			h.logError("mark job completed", err)
		}
		if ev.WorkflowJob.RunnerName != "" {
			if err := h.Store.UpdateRunnerStatusByName(r.Context(), ev.WorkflowJob.RunnerName, "finished"); err != nil {
				h.logError("update runner status finished", err)
			}
		}
		w.WriteHeader(http.StatusOK)

	default:
		w.WriteHeader(http.StatusOK)
	}
}

// labelsMatch returns true if every job runs-on label can be satisfied
// by this pool — i.e. job.runs_on ⊆ cfg.RunnerLabels (with the
// implicit "self-hosted" label always considered satisfied because
// GitHub assigns it to every self-hosted runner). An empty configured
// set means "serve everything".
//
// GitHub's runs-on semantics are cumulative: a runner is only eligible
// for a job if it has ALL of the job's labels (per the
// "using-self-hosted-runners-in-a-workflow" docs). The previous
// any-overlap check accepted jobs we couldn't fulfill — e.g. a job
// requiring [self-hosted, gpu] on a pool that only advertises
// [self-hosted] would launch a runner GitHub would never bind, leaving
// a ghost.
func labelsMatch(runsOn, configured []string) bool {
	if len(configured) == 0 {
		return true
	}
	have := make(map[string]struct{}, len(configured)+1)
	for _, l := range configured {
		have[normalizeLabel(l)] = struct{}{}
	}
	// "self-hosted" is GitHub-assigned to every self-hosted runner, so
	// a job requiring it is always satisfiable on this pool even if
	// the operator didn't list it explicitly.
	have["self-hosted"] = struct{}{}
	for _, l := range runsOn {
		if _, ok := have[normalizeLabel(l)]; !ok {
			return false
		}
	}
	return true
}

// normalizeLabel lower-cases and trims a label. GitHub treats labels
// as case-insensitive (per the labels doc), so we do the same to avoid
// rejecting Self-Hosted vs self-hosted.
func normalizeLabel(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func (h *WebhookHandler) logError(msg string, err error) {
	if h.Log != nil {
		h.Log.Error(msg, "error", err)
	}
}
