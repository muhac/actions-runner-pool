package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/store"
)

// fakeStore is a minimal Store implementation; only the methods the
// handlers reach are non-stub.
type fakeStore struct {
	store.Store // embed nil interface so missing methods panic loudly if hit
	appConfig   *store.AppConfig
	getErr      error
	saveErr     error
	saved       *store.AppConfig

	// Webhook-side state.
	upsertedInstallations    []*store.Installation
	upsertedRepoInstallation map[string]int64
	removedRepoInstallation  []string
	insertedJobs             []*store.Job
	markJobInProgressNoOp    bool // when true, MarkJobInProgress reports advanced=false
	insertJobErr             error
	markedInProgress         []markedInProgress
	markedCompleted          []markedCompleted
	cancelledForRepo         []string
	updatedRunnerByName      []runnerStatusUpdate
}

type markedInProgress struct {
	jobID      int64
	runnerID   int64
	runnerName string
}

type markedCompleted struct {
	jobID      int64
	conclusion string
}

type runnerStatusUpdate struct {
	runnerName string
	status     string
}

func (f *fakeStore) GetAppConfig(_ context.Context) (*store.AppConfig, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.appConfig, nil
}

func (f *fakeStore) SaveAppConfig(_ context.Context, c *store.AppConfig) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = c
	f.appConfig = c
	return nil
}

func (f *fakeStore) UpsertInstallation(_ context.Context, inst *store.Installation) error {
	f.upsertedInstallations = append(f.upsertedInstallations, inst)
	return nil
}

func (f *fakeStore) UpsertRepoInstallation(_ context.Context, repo string, instID int64) error {
	if f.upsertedRepoInstallation == nil {
		f.upsertedRepoInstallation = map[string]int64{}
	}
	f.upsertedRepoInstallation[repo] = instID
	return nil
}

func (f *fakeStore) RemoveRepoInstallation(_ context.Context, repo string) error {
	f.removedRepoInstallation = append(f.removedRepoInstallation, repo)
	return nil
}

func (f *fakeStore) InsertJobIfNew(_ context.Context, j *store.Job) (bool, error) {
	if f.insertJobErr != nil {
		return false, f.insertJobErr
	}
	for _, existing := range f.insertedJobs {
		if existing.DedupeKey == j.DedupeKey {
			return false, nil
		}
	}
	f.insertedJobs = append(f.insertedJobs, j)
	return true, nil
}

func (f *fakeStore) MarkJobDispatched(_ context.Context, jobID int64) error {
	f.markedInProgress = append(f.markedInProgress, markedInProgress{jobID, 0, ""})
	return nil
}

func (f *fakeStore) MarkJobInProgress(_ context.Context, jobID, runnerID int64, runnerName string) (bool, error) {
	f.markedInProgress = append(f.markedInProgress, markedInProgress{jobID, runnerID, runnerName})
	if f.markJobInProgressNoOp {
		return false, nil
	}
	return true, nil
}

func (f *fakeStore) MarkJobCompleted(_ context.Context, jobID int64, conclusion string) error {
	f.markedCompleted = append(f.markedCompleted, markedCompleted{jobID, conclusion})
	return nil
}

func (f *fakeStore) CancelPendingJobsForRepo(_ context.Context, repo string) (int64, error) {
	f.cancelledForRepo = append(f.cancelledForRepo, repo)
	return 0, nil
}

func (f *fakeStore) CancelJobIfPending(_ context.Context, jobID int64) (bool, error) {
	// Webhook handlers don't call this; only dispatch does. Embedded
	// store.Store would panic via nil deref if a future test does, so
	// surface a clear panic instead.
	panic("CancelJobIfPending called on handler test fake")
}

func (f *fakeStore) UpdateRunnerStatusByName(_ context.Context, runnerName, status string) error {
	f.updatedRunnerByName = append(f.updatedRunnerByName, runnerStatusUpdate{runnerName, status})
	return nil
}

func newSetupHandler(t *testing.T, baseURL string, st store.Store) *SetupHandler {
	t.Helper()
	return &SetupHandler{
		Cfg:   &config.Config{BaseURL: baseURL},
		Store: st,
	}
}

func doGET(t *testing.T, h http.HandlerFunc, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func TestSetup_FreshInstall_RendersForm(t *testing.T) {
	h := newSetupHandler(t, "https://example.test", &fakeStore{})
	rr := doGET(t, h.Get, "/setup")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `action="https://github.com/settings/apps/new?state=`) {
		t.Errorf("missing form action in body")
	}
	if !strings.Contains(body, `name="manifest"`) {
		t.Errorf("missing manifest hidden field in body")
	}
	// Manifest JSON is HTML-escaped inside value=; verify the App name
	// prefix is present (the suffix is a hash of BaseURL).
	if !strings.Contains(body, "gharp-") {
		t.Errorf("manifest payload not embedded; body=%s", body)
	}

	cookies := rr.Result().Cookies()
	var stateCk *http.Cookie
	for _, c := range cookies {
		if c.Name == stateCookie {
			stateCk = c
			break
		}
	}
	if stateCk == nil {
		t.Fatalf("missing %s cookie", stateCookie)
	}
	if !stateCk.HttpOnly {
		t.Errorf("state cookie should be HttpOnly")
	}
	if stateCk.MaxAge != int(stateCookieTTL.Seconds()) {
		t.Errorf("state cookie MaxAge = %d, want %d", stateCk.MaxAge, int(stateCookieTTL.Seconds()))
	}
	if stateCk.Path != "/github/app/callback" {
		t.Errorf("state cookie Path = %q, want /github/app/callback", stateCk.Path)
	}
	if stateCk.Value == "" || len(stateCk.Value) < 16 {
		t.Errorf("state cookie value too short: %q", stateCk.Value)
	}
}

func TestSetup_HTTPSCookieIsSecure(t *testing.T) {
	h := newSetupHandler(t, "https://example.test", &fakeStore{})
	rr := doGET(t, h.Get, "/setup")
	for _, c := range rr.Result().Cookies() {
		if c.Name == stateCookie && !c.Secure {
			t.Errorf("state cookie should be Secure when BaseURL is https")
		}
	}
}

func TestSetup_HTTPCookieNotSecure(t *testing.T) {
	h := newSetupHandler(t, "http://localhost:8080", &fakeStore{})
	rr := doGET(t, h.Get, "/setup")
	for _, c := range rr.Result().Cookies() {
		if c.Name == stateCookie && c.Secure {
			t.Errorf("state cookie should NOT be Secure for plain http BaseURL")
		}
	}
}

func TestSetup_ConfiguredInstall_RendersInstallLink(t *testing.T) {
	st := &fakeStore{appConfig: &store.AppConfig{
		Slug: "gharp-test", BaseURL: "https://example.test",
	}}
	h := newSetupHandler(t, "https://example.test", st)
	rr := doGET(t, h.Get, "/setup")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	want := "https://github.com/apps/gharp-test/installations/new"
	if !strings.Contains(body, want) {
		t.Errorf("install link missing; body=%s", body)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == stateCookie {
			t.Errorf("should not set state cookie when already configured")
		}
	}
}

func TestSetup_BaseURLMismatch_RendersForm(t *testing.T) {
	// app_config.BaseURL is from a previous deployment — treat as fresh.
	st := &fakeStore{appConfig: &store.AppConfig{
		Slug: "old-app", BaseURL: "https://old.example.test",
	}}
	h := newSetupHandler(t, "https://example.test", st)
	rr := doGET(t, h.Get, "/setup")

	if rr.Code != http.StatusOK {
		t.Fatal(rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `name="manifest"`) {
		t.Errorf("expected fresh form, got: %s", rr.Body.String())
	}
}

func TestSetup_StoreError_500(t *testing.T) {
	st := &fakeStore{getErr: errors.New("disk on fire")}
	h := newSetupHandler(t, "https://example.test", st)
	rr := doGET(t, h.Get, "/setup")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}
