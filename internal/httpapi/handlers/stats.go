package handlers

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/store"
)

type StatsHandler struct {
	Cfg   *config.Config
	Store store.Store
	Log   *slog.Logger
}

func (h *StatsHandler) Get(w http.ResponseWriter, r *http.Request) {
	if !authorizedBearer(h.Cfg, r.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	summary, err := h.Store.Summary(r.Context())
	if err != nil {
		h.logError("stats: store summary", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, toStatsResponse(summary, h.maxConcurrentRunners()))
}

func (h *StatsHandler) maxConcurrentRunners() int {
	if h.Cfg == nil {
		return 0
	}
	return h.Cfg.MaxConcurrentRunners
}

func (h *StatsHandler) logError(msg string, err error) {
	if h.Log != nil {
		h.Log.Error(msg, "error", err)
	}
}

type statsResponse struct {
	Jobs      map[string]int64 `json:"jobs"`
	Runners   map[string]int64 `json:"runners"`
	Capacity  capacityResponse `json:"capacity"`
	UpdatedAt time.Time        `json:"updated_at"`
}

type capacityResponse struct {
	MaxConcurrentRunners int   `json:"max_concurrent_runners"`
	ActiveRunners        int64 `json:"active_runners"`
	AvailableSlots       int64 `json:"available_slots"`
}

func toStatsResponse(summary *store.Summary, maxConcurrent int) statsResponse {
	jobs := statusCounts(summary.JobsByStatus, store.JobStatuses)
	runners := statusCounts(summary.RunnersByStatus, store.RunnerStatuses)

	active := runners["starting"] + runners["idle"] + runners["busy"]
	available := int64(maxConcurrent) - active
	if available < 0 {
		available = 0
	}

	return statsResponse{
		Jobs:    jobs,
		Runners: runners,
		Capacity: capacityResponse{
			MaxConcurrentRunners: maxConcurrent,
			ActiveRunners:        active,
			AvailableSlots:       available,
		},
		UpdatedAt: time.Now().UTC(),
	}
}

func statusCounts(in map[string]int64, known []string) map[string]int64 {
	out := make(map[string]int64, len(in)+len(known))
	for _, status := range known {
		out[status] = 0
	}
	for status, count := range in {
		out[status] = count
	}
	return out
}
