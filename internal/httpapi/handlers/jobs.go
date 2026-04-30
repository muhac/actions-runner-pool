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

type JobsHandler struct {
	Cfg   *config.Config
	Store store.Store
	Log   *slog.Logger
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
		Jobs:    make([]jobResponse, 0, len(jobs)),
		Count:   len(jobs),
		Filters: jobsFilters{Statuses: statuses, Repo: repo, Limit: limit},
	}
	for _, j := range jobs {
		out.Jobs = append(out.Jobs, jobResponse{
			ID:         j.ID,
			Repo:       j.Repo,
			Action:     j.Action,
			Labels:     j.Labels,
			Status:     j.Status,
			Conclusion: j.Conclusion,
			RunnerID:   j.RunnerID,
			RunnerName: j.RunnerName,
			ReceivedAt: j.ReceivedAt,
			UpdatedAt:  j.UpdatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
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
	Jobs    []jobResponse `json:"jobs"`
	Count   int           `json:"count"`
	Filters jobsFilters   `json:"filters"`
}

type jobsFilters struct {
	Statuses []string `json:"statuses,omitempty"`
	Repo     string   `json:"repo,omitempty"`
	Limit    int      `json:"limit"`
}

type jobResponse struct {
	ID         int64     `json:"id"`
	Repo       string    `json:"repo"`
	Action     string    `json:"action"`
	Labels     string    `json:"labels"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	RunnerID   int64     `json:"runner_id"`
	RunnerName string    `json:"runner_name"`
	ReceivedAt time.Time `json:"received_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (h *JobsHandler) logError(msg string, err error) {
	if h.Log != nil {
		h.Log.Error(msg, "error", err)
	}
}
