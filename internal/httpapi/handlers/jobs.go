package handlers

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/store"
)

const (
	defaultJobsLimit = 100
	maxJobsLimit     = 500
)

var allowedJobStatuses = map[string]struct{}{
	"pending":     {},
	"dispatched":  {},
	"in_progress": {},
	"completed":   {},
}

type jobsEnqueuer interface {
	Enqueue(jobID int64)
}

type JobsHandler struct {
	Cfg       *config.Config
	Store     store.Store
	Scheduler jobsEnqueuer
	Log       *slog.Logger
}

// GET /jobs?status=...&repo=...&limit=...
func (h *JobsHandler) Get(w http.ResponseWriter, r *http.Request) {
	if !authorizedBearer(h.Cfg, r.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	statuses, err := parseStatuses(r.URL.Query()["status"])
	if err != nil {
		http.Error(w, "invalid status query parameter", http.StatusBadRequest)
		return
	}

	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		http.Error(w, "invalid limit query parameter", http.StatusBadRequest)
		return
	}

	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	jobs, err := h.Store.ListJobs(r.Context(), store.JobListFilter{
		Statuses: statuses,
		Repo:     repo,
		Limit:    limit,
	})
	if err != nil {
		h.logError("list jobs", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	out := jobsResponse{
		Jobs:    make([]jobListResponse, 0, len(jobs)),
		Count:   len(jobs),
		Filters: jobsFilters{Statuses: statuses, Repo: repo, Limit: limit},
	}
	for _, j := range jobs {
		out.Jobs = append(out.Jobs, toJobListResponse(j))
	}

	writeJSON(w, out)
}

// GET /jobs/{job_id}
func (h *JobsHandler) GetByID(w http.ResponseWriter, r *http.Request) {
	if !authorizedBearer(h.Cfg, r.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	jobID, ok := parseJobIDPath(w, r)
	if !ok {
		return
	}
	job, err := h.Store.GetJob(r.Context(), jobID)
	if err != nil {
		h.logError("get job", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if job == nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	writeJSON(w, toJobDetailResponse(job))
}

// POST /jobs/{job_id}/retry
func (h *JobsHandler) Retry(w http.ResponseWriter, r *http.Request) {
	if !authorizedBearer(h.Cfg, r.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	jobID, ok := parseJobIDPath(w, r)
	if !ok {
		return
	}
	job, err := h.Store.GetJob(r.Context(), jobID)
	if err != nil {
		h.logError("get job before retry", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if job == nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	if job.Status != "completed" {
		http.Error(w, "job is not retryable in current status", http.StatusConflict)
		return
	}
	updated, err := h.Store.RetryJobIfCompleted(r.Context(), jobID)
	if err != nil {
		h.logError("retry job", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !updated {
		http.Error(w, "job retry was not applied", http.StatusConflict)
		return
	}
	if h.Scheduler != nil {
		h.Scheduler.Enqueue(jobID)
	}
	job, err = h.Store.GetJob(r.Context(), jobID)
	if err != nil {
		h.logError("get job after retry", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, actionResponse{Action: "retry", Job: toJobDetailResponse(job)})
}

// POST /jobs/{job_id}/cancel
func (h *JobsHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	if !authorizedBearer(h.Cfg, r.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	jobID, ok := parseJobIDPath(w, r)
	if !ok {
		return
	}
	job, err := h.Store.GetJob(r.Context(), jobID)
	if err != nil {
		h.logError("get job before cancel", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if job == nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	if job.Status != "pending" && job.Status != "dispatched" {
		http.Error(w, "job is not cancellable in current status", http.StatusConflict)
		return
	}
	updated, err := h.Store.CancelJobIfPending(r.Context(), jobID)
	if err != nil {
		h.logError("cancel job", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !updated {
		http.Error(w, "job cancel was not applied", http.StatusConflict)
		return
	}
	job, err = h.Store.GetJob(r.Context(), jobID)
	if err != nil {
		h.logError("get job after cancel", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, actionResponse{Action: "cancel", Job: toJobDetailResponse(job)})
}

func parseJobIDPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.PathValue("job_id"))
	if raw == "" {
		http.Error(w, "missing job_id path parameter", http.StatusBadRequest)
		return 0, false
	}
	jobID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || jobID <= 0 {
		http.Error(w, "invalid job_id path parameter", http.StatusBadRequest)
		return 0, false
	}
	return jobID, true
}

func authorizedBearer(cfg *config.Config, authHeader string) bool {
	if cfg == nil || cfg.AdminToken == "" {
		return true
	}
	parts := strings.Fields(authHeader)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return false
	}
	provided := parts[1]
	if len(provided) != len(cfg.AdminToken) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.AdminToken)) == 1
}

func parseStatuses(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, v := range raw {
		for _, chunk := range strings.Split(v, ",") {
			status := strings.ToLower(strings.TrimSpace(chunk))
			if status == "" {
				continue
			}
			if _, ok := allowedJobStatuses[status]; !ok {
				return nil, fmt.Errorf("invalid status %q", status)
			}
			if _, ok := seen[status]; ok {
				continue
			}
			seen[status] = struct{}{}
			out = append(out, status)
		}
	}
	return out, nil
}

func parseLimit(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultJobsLimit, nil
	}
	limit, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || limit <= 0 {
		return 0, strconv.ErrSyntax
	}
	if limit > maxJobsLimit {
		return maxJobsLimit, nil
	}
	return limit, nil
}

type jobsResponse struct {
	Jobs    []jobListResponse `json:"jobs"`
	Count   int               `json:"count"`
	Filters jobsFilters       `json:"filters"`
}

type jobsFilters struct {
	Statuses []string `json:"statuses,omitempty"`
	Repo     string   `json:"repo,omitempty"`
	Limit    int      `json:"limit"`
}

type jobListResponse struct {
	ID           int64     `json:"id"`
	Repo         string    `json:"repo"`
	JobName      string    `json:"job_name"`
	RunID        int64     `json:"run_id"`
	RunAttempt   int64     `json:"run_attempt"`
	WorkflowName string    `json:"workflow_name"`
	Action       string    `json:"action"`
	Labels       string    `json:"labels"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	RunnerID     int64     `json:"runner_id"`
	RunnerName   string    `json:"runner_name"`
	ReceivedAt   time.Time `json:"received_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type jobDetailResponse struct {
	jobListResponse
	PayloadJSON string `json:"payload_json"`
}

type actionResponse struct {
	Action string            `json:"action"`
	Job    jobDetailResponse `json:"job"`
}

func toJobListResponse(j *store.Job) jobListResponse {
	return jobListResponse{
		ID:           j.ID,
		Repo:         j.Repo,
		JobName:      j.JobName,
		RunID:        j.RunID,
		RunAttempt:   j.RunAttempt,
		WorkflowName: j.WorkflowName,
		Action:       j.Action,
		Labels:       j.Labels,
		Status:       j.Status,
		Conclusion:   j.Conclusion,
		RunnerID:     j.RunnerID,
		RunnerName:   j.RunnerName,
		ReceivedAt:   j.ReceivedAt,
		UpdatedAt:    j.UpdatedAt,
	}
}

func toJobDetailResponse(j *store.Job) jobDetailResponse {
	return jobDetailResponse{jobListResponse: toJobListResponse(j), PayloadJSON: j.PayloadJSON}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *JobsHandler) logError(msg string, err error) {
	if h.Log != nil {
		h.Log.Error(msg, "error", err)
	}
}
