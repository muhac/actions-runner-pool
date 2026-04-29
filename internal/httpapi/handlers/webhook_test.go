package handlers

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
	// Mirror what config.Load does: precompute the lower-cased label
	// set so the handler reads RunnerLabelSet, not RunnerLabels.
	set := make(map[string]struct{}, len(runnerLabels))
	for _, l := range runnerLabels {
		set[strings.ToLower(strings.TrimSpace(l))] = struct{}{}
	}
	return &WebhookHandler{
		Cfg:       &config.Config{BaseURL: "https://example.test", RunnerLabels: runnerLabels, RunnerLabelSet: set},
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
	// New behavior: removed repos must also have their pending jobs
	// cancelled, otherwise dispatch keeps trying to mint a token for
	// an installation that no longer covers them.
	if len(st.cancelledForRepo) != 1 || st.cancelledForRepo[0] != "alice/gone" {
		t.Errorf("expected CancelPendingJobsForRepo(alice/gone), got %+v", st.cancelledForRepo)
	}
}

// installation:deleted means the App was uninstalled. We must (a)
// drop the repo->installation rows for every covered repo so dispatch
// stops trying to mint tokens, and (b) cancel any still-dispatchable
// jobs so they don't sit pending forever.
func TestWebhook_InstallationDeleted_CancelsAndUnmaps(t *testing.T) {
	body := []byte(`{
		"action": "deleted",
		"installation": {"id": 99, "account": {"id": 7, "login": "alice", "type": "User"}},
		"repositories": [{"full_name": "alice/repo1"}, {"full_name": "alice/repo2"}]
	}`)
	st := storeWithSecret(testWebhookSecret)
	h := newWebhookHandler(t, st, &spyEnqueuer{}, nil)
	rr := postWebhook(t, h, "installation", body, sign(testWebhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(st.cancelledForRepo) != 2 {
		t.Fatalf("expected 2 CancelPendingJobsForRepo calls, got %+v", st.cancelledForRepo)
	}
	if len(st.removedRepoInstallation) != 2 {
		t.Fatalf("expected 2 RemoveRepoInstallation calls, got %+v", st.removedRepoInstallation)
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

// Pool advertises [linux] but a job requires [self-hosted, gpu] →
// must be rejected because gpu is not satisfiable. Pre-superset, this
// also failed (no overlap). Post-superset, the rejection mechanism
// changed but the outcome is the same.
func TestWebhook_LabelFilter_DropsNonMatching(t *testing.T) {
	body := []byte(`{
		"action": "queued",
		"workflow_job": {"id": 12346, "labels": ["self-hosted", "gpu"]},
		"repository": {"full_name": "alice/repo"},
		"installation": {"id": 99}
	}`)
	st := storeWithSecret(testWebhookSecret)
	sch := &spyEnqueuer{}
	h := newWebhookHandler(t, st, sch, []string{"linux"}) // gpu unsatisfiable
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

func TestWebhook_InProgress_EmptyRunnerName_Skipped(t *testing.T) {
	// Observed in production: GitHub fires in_progress with runner_id=0
	// and runner_name="" when our gharp-launched runner lost the race
	// to a different runner (the runner↔job drift documented in
	// architecture.md). Without this skip, the row's status would
	// advance to in_progress and PendingJobs replay couldn't rescue it.
	body := []byte(`{
		"action": "in_progress",
		"workflow_job": {"id": 12345, "runner_id": 0, "runner_name": "", "labels": ["self-hosted"]},
		"repository": {"full_name": "alice/repo"},
		"installation": {"id": 99}
	}`)
	st := storeWithSecret(testWebhookSecret)
	h := newWebhookHandler(t, st, &spyEnqueuer{}, nil)
	rr := postWebhook(t, h, "workflow_job", body, sign(testWebhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(st.markedInProgress) != 0 {
		t.Errorf("MarkJobInProgress should not be called: %+v", st.markedInProgress)
	}
	if len(st.updatedRunnerByName) != 0 {
		t.Errorf("UpdateRunnerStatusByName should not be called: %+v", st.updatedRunnerByName)
	}
}

func TestWebhook_InProgress_NoOpWhenAlreadyAdvanced_DoesNotTouchRunner(t *testing.T) {
	// A late in_progress arriving after the row is already completed
	// must not flip the (now finished) runner back to busy. The fake's
	// markJobInProgressNoOp simulates the store's WHERE-status guard
	// returning advanced=false.
	body := []byte(`{
		"action": "in_progress",
		"workflow_job": {"id": 12345, "runner_id": 77, "runner_name": "runner-A", "labels": ["self-hosted"]},
		"repository": {"full_name": "alice/repo"},
		"installation": {"id": 99}
	}`)
	st := storeWithSecret(testWebhookSecret)
	st.markJobInProgressNoOp = true
	h := newWebhookHandler(t, st, &spyEnqueuer{}, nil)
	rr := postWebhook(t, h, "workflow_job", body, sign(testWebhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(st.markedInProgress) != 1 {
		t.Errorf("MarkJobInProgress should be called once: %+v", st.markedInProgress)
	}
	if len(st.updatedRunnerByName) != 0 {
		t.Errorf("runner status must NOT be touched on no-op advance: %+v", st.updatedRunnerByName)
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

func TestWebhook_OversizeBody_413(t *testing.T) {
	// Body just over the 1MiB cap; signature can be anything because the
	// limit fires before HMAC verification.
	body := bytes.Repeat([]byte("x"), maxWebhookBodyBytes+1)
	h := newWebhookHandler(t, storeWithSecret(testWebhookSecret), &spyEnqueuer{}, nil)
	rr := postWebhook(t, h, "workflow_job", body, "sha256=whatever")
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rr.Code)
	}
}

func TestWebhook_EmptySecret_RejectsSignature(t *testing.T) {
	// Misconfiguration: app_config.WebhookSecret == "". An attacker computing
	// HMAC with an empty key would otherwise produce a "valid" signature.
	st := &fakeStore{appConfig: &store.AppConfig{WebhookSecret: ""}}
	h := newWebhookHandler(t, st, &spyEnqueuer{}, nil)
	body := []byte(`{}`)
	rr := postWebhook(t, h, "ping", body, sign("", body))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (empty-secret rejection)", rr.Code)
	}
}

// labelsMatch enforces GitHub's cumulative runs-on semantics: a job is
// only accepted if every required label can be satisfied by this pool.
// 'self-hosted' is implicit (GitHub auto-assigns it) so it's always
// satisfiable. Empty configured = serve everything (legacy behavior
// for operators who haven't set RUNNER_LABELS).
func TestLabelsMatch_SupersetSemantics(t *testing.T) {
	makeSet := func(labels []string) map[string]struct{} {
		out := make(map[string]struct{}, len(labels))
		for _, l := range labels {
			out[strings.ToLower(strings.TrimSpace(l))] = struct{}{}
		}
		return out
	}
	cases := []struct {
		name       string
		runsOn     []string
		configured []string
		want       bool
	}{
		// The original failure mode: pool advertises self-hosted, job
		// also wants gpu — must REJECT (current code accepted it,
		// leaving a ghost runner GitHub never bound).
		{"requires-extra-label", []string{"self-hosted", "gpu"}, []string{"self-hosted"}, false},
		{"all-required-present", []string{"self-hosted", "gpu"}, []string{"gpu"}, true},
		{"only-self-hosted", []string{"self-hosted"}, []string{}, true},
		{"empty-configured-serves-everything", []string{"gpu", "linux"}, nil, true},
		{"case-insensitive-match", []string{"Self-Hosted", "GPU"}, []string{"gpu"}, true},
		{"explicit-self-hosted-not-required-in-cfg", []string{"self-hosted"}, []string{"linux"}, true},
		{"job-with-no-labels-trivially-satisfied", nil, []string{"gpu"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := labelsMatch(tc.runsOn, makeSet(tc.configured)); got != tc.want {
				t.Fatalf("labelsMatch(%v, %v) = %v, want %v", tc.runsOn, tc.configured, got, tc.want)
			}
		})
	}
}
