package handlers

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/store"
)

const testWebhookSecret = "shh-it-is-a-secret"

type spyEnqueuer struct {
	enqueued []int64
	calls    atomic.Int64
}

func (s *spyEnqueuer) Enqueue(jobID int64) {
	s.calls.Add(1)
	s.enqueued = append(s.enqueued, jobID)
}

func newWebhookHandler(t *testing.T, st store.Store, sch *spyEnqueuer, runnerLabels []string) *WebhookHandler {
	t.Helper()
	return &WebhookHandler{
		Cfg:       &config.Config{BaseURL: "https://example.test", RunnerLabels: runnerLabels},
		Store:     st,
		Scheduler: sch,
	}
}

func storeWithSecret(secret string) *fakeStore {
	return &fakeStore{appConfig: &store.AppConfig{WebhookSecret: secret}}
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(t *testing.T, h *WebhookHandler, event string, body []byte, signature string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	rr := httptest.NewRecorder()
	h.Post(rr, req)
	return rr
}

func TestWebhook_BadSignature_401(t *testing.T) {
	h := newWebhookHandler(t, storeWithSecret(testWebhookSecret), &spyEnqueuer{}, nil)
	body := []byte(`{"action":"queued"}`)
	rr := postWebhook(t, h, "workflow_job", body, "sha256=deadbeef")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestWebhook_NoAppConfig_503(t *testing.T) {
	st := &fakeStore{} // appConfig nil
	h := newWebhookHandler(t, st, &spyEnqueuer{}, nil)
	body := []byte(`{}`)
	rr := postWebhook(t, h, "ping", body, sign(testWebhookSecret, body))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestWebhook_PingPasses_200(t *testing.T) {
	body := []byte(`{"zen":"hi"}`)
	h := newWebhookHandler(t, storeWithSecret(testWebhookSecret), &spyEnqueuer{}, nil)
	rr := postWebhook(t, h, "ping", body, sign(testWebhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
}

func TestWebhook_UnknownEvent_200NoOp(t *testing.T) {
	body := []byte(`{}`)
	h := newWebhookHandler(t, storeWithSecret(testWebhookSecret), &spyEnqueuer{}, nil)
	rr := postWebhook(t, h, "push", body, sign(testWebhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestWebhook_InstallationCreated_Upserts(t *testing.T) {
	body := []byte(`{
		"action": "created",
		"installation": {
			"id": 99,
			"account": {"id": 7, "login": "alice", "type": "User"}
		},
		"repositories": [{"full_name": "alice/repo1"}, {"full_name": "alice/repo2"}]
	}`)
	st := storeWithSecret(testWebhookSecret)
	h := newWebhookHandler(t, st, &spyEnqueuer{}, nil)
	rr := postWebhook(t, h, "installation", body, sign(testWebhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(st.upsertedInstallations) != 1 || st.upsertedInstallations[0].ID != 99 {
		t.Errorf("UpsertInstallation not called: %+v", st.upsertedInstallations)
	}
	if got := st.upsertedRepoInstallation; got["alice/repo1"] != 99 || got["alice/repo2"] != 99 {
		t.Errorf("UpsertRepoInstallation: %+v", got)
	}
}

func TestWebhook_InstallationRepositoriesAddedRemoved(t *testing.T) {
	body := []byte(`{
		"action": "added",
		"installation": {"id": 99},
		"repositories_added": [{"full_name": "alice/new"}],
		"repositories_removed": [{"full_name": "alice/gone"}]
	}`)
	st := storeWithSecret(testWebhookSecret)
	h := newWebhookHandler(t, st, &spyEnqueuer{}, nil)
	rr := postWebhook(t, h, "installation_repositories", body, sign(testWebhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if st.upsertedRepoInstallation["alice/new"] != 99 {
		t.Errorf("expected upsert alice/new -> 99, got %+v", st.upsertedRepoInstallation)
	}
	if len(st.removedRepoInstallation) != 1 || st.removedRepoInstallation[0] != "alice/gone" {
		t.Errorf("expected remove alice/gone, got %+v", st.removedRepoInstallation)
	}
}

const queuedJobBody = `{
	"action": "queued",
	"workflow_job": {"id": 12345, "labels": ["self-hosted"]},
	"repository": {"full_name": "alice/repo"},
	"installation": {"id": 99}
}`

func TestWebhook_QueuedHappyPath_InsertsAndEnqueues(t *testing.T) {
	body := []byte(queuedJobBody)
	st := storeWithSecret(testWebhookSecret)
	sch := &spyEnqueuer{}
	h := newWebhookHandler(t, st, sch, nil) // nil = serve everything
	rr := postWebhook(t, h, "workflow_job", body, sign(testWebhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(st.insertedJobs) != 1 || st.insertedJobs[0].ID != 12345 {
		t.Errorf("InsertJobIfNew not called as expected: %+v", st.insertedJobs)
	}
	if sch.calls.Load() != 1 || sch.enqueued[0] != 12345 {
		t.Errorf("Enqueue: calls=%d enqueued=%v", sch.calls.Load(), sch.enqueued)
	}
	// Lazy-write: repo->installation should also be set.
	if st.upsertedRepoInstallation["alice/repo"] != 99 {
		t.Errorf("expected lazy upsert repo->installation, got %+v", st.upsertedRepoInstallation)
	}
}

func TestWebhook_QueuedDuplicate_DedupedAtStore(t *testing.T) {
	body := []byte(queuedJobBody)
	st := storeWithSecret(testWebhookSecret)
	sch := &spyEnqueuer{}
	h := newWebhookHandler(t, st, sch, nil)

	for range 2 {
		rr := postWebhook(t, h, "workflow_job", body, sign(testWebhookSecret, body))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d", rr.Code)
		}
	}
	if sch.calls.Load() != 1 {
		t.Errorf("Enqueue called %d times, want 1 (dedup at store)", sch.calls.Load())
	}
}

func TestWebhook_QueuedStoreError_503AndNoEnqueue(t *testing.T) {
	body := []byte(queuedJobBody)
	st := storeWithSecret(testWebhookSecret)
	st.insertJobErr = errors.New("disk on fire")
	sch := &spyEnqueuer{}
	h := newWebhookHandler(t, st, sch, nil)
	rr := postWebhook(t, h, "workflow_job", body, sign(testWebhookSecret, body))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if sch.calls.Load() != 0 {
		t.Errorf("Enqueue should not be called on store error")
	}
}

func TestWebhook_LabelFilter_DropsNonMatching(t *testing.T) {
	body := []byte(queuedJobBody) // labels: ["self-hosted"]
	st := storeWithSecret(testWebhookSecret)
	sch := &spyEnqueuer{}
	h := newWebhookHandler(t, st, sch, []string{"gpu"}) // configured doesn't match
	rr := postWebhook(t, h, "workflow_job", body, sign(testWebhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(st.insertedJobs) != 0 {
		t.Errorf("expected no insert, got %+v", st.insertedJobs)
	}
	if sch.calls.Load() != 0 {
		t.Errorf("expected no Enqueue, got %d", sch.calls.Load())
	}
}

func TestWebhook_LabelFilter_MatchProceeds(t *testing.T) {
	body := []byte(queuedJobBody)
	st := storeWithSecret(testWebhookSecret)
	sch := &spyEnqueuer{}
	h := newWebhookHandler(t, st, sch, []string{"self-hosted", "gpu"})
	rr := postWebhook(t, h, "workflow_job", body, sign(testWebhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if sch.calls.Load() != 1 {
		t.Errorf("expected Enqueue, got %d", sch.calls.Load())
	}
}

func TestWebhook_InProgress_BindsRunnerAndMarksBusy(t *testing.T) {
	body := []byte(`{
		"action": "in_progress",
		"workflow_job": {"id": 12345, "runner_id": 77, "runner_name": "runner-A", "labels": ["self-hosted"]},
		"repository": {"full_name": "alice/repo"},
		"installation": {"id": 99}
	}`)
	st := storeWithSecret(testWebhookSecret)
	sch := &spyEnqueuer{}
	h := newWebhookHandler(t, st, sch, nil)
	rr := postWebhook(t, h, "workflow_job", body, sign(testWebhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(st.markedInProgress) != 1 ||
		st.markedInProgress[0].jobID != 12345 ||
		st.markedInProgress[0].runnerID != 77 ||
		st.markedInProgress[0].runnerName != "runner-A" {
		t.Errorf("MarkJobInProgress: %+v", st.markedInProgress)
	}
	if len(st.updatedRunnerByName) != 1 ||
		st.updatedRunnerByName[0].runnerName != "runner-A" ||
		st.updatedRunnerByName[0].status != "busy" {
		t.Errorf("UpdateRunnerStatusByName: %+v", st.updatedRunnerByName)
	}
}

func TestWebhook_Completed_RecordsConclusion(t *testing.T) {
	body := []byte(`{
		"action": "completed",
		"workflow_job": {"id": 12345, "conclusion": "success", "runner_name": "runner-A", "labels": ["self-hosted"]},
		"repository": {"full_name": "alice/repo"},
		"installation": {"id": 99}
	}`)
	st := storeWithSecret(testWebhookSecret)
	h := newWebhookHandler(t, st, &spyEnqueuer{}, nil)
	rr := postWebhook(t, h, "workflow_job", body, sign(testWebhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(st.markedCompleted) != 1 || st.markedCompleted[0].conclusion != "success" {
		t.Errorf("MarkJobCompleted: %+v", st.markedCompleted)
	}
	if len(st.updatedRunnerByName) != 1 || st.updatedRunnerByName[0].status != "finished" {
		t.Errorf("UpdateRunnerStatusByName: %+v", st.updatedRunnerByName)
	}
}

func TestWebhook_BadJSON_400(t *testing.T) {
	body := []byte(`{not json`)
	h := newWebhookHandler(t, storeWithSecret(testWebhookSecret), &spyEnqueuer{}, nil)
	rr := postWebhook(t, h, "workflow_job", body, sign(testWebhookSecret, body))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}
