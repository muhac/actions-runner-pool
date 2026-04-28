package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/store"
)

// FuzzWebhook_Handler feeds arbitrary bytes as the webhook body — with a
// VALID signature so the HMAC gate passes — to make sure the JSON parser
// and event-routing logic never panic. The signature is computed on the
// fuzzer-supplied body so we exercise the parsing path, not the auth
// rejection path (which is already unit-tested).
//
// The store/enqueuer are recreated per iteration: fakeStore appends to
// its internal slices/maps, and a long fuzz run would otherwise grow
// memory unboundedly and let earlier iterations bias later behavior.
func FuzzWebhook_Handler(f *testing.F) {
	f.Add([]byte(`{}`), "ping")
	f.Add([]byte(`{"action":"queued"}`), "workflow_job")
	f.Add([]byte(`{"installation":{"id":1}}`), "installation")
	f.Add([]byte(`not json at all`), "workflow_job")
	f.Add([]byte(``), "workflow_job")
	f.Add([]byte(`{"action":"queued","workflow_job":{"id":1,"labels":["self-hosted"]},"repository":{"full_name":"a/b"},"installation":{"id":1}}`), "workflow_job")

	cfg := &config.Config{BaseURL: "https://example.test", RunnerLabels: []string{"self-hosted"}}

	f.Fuzz(func(t *testing.T, body []byte, event string) {
		st := &fakeStore{appConfig: &store.AppConfig{WebhookSecret: testWebhookSecret}}
		h := &WebhookHandler{Cfg: cfg, Store: st, Scheduler: &spyEnqueuer{}}

		req := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", sign(testWebhookSecret, body))
		rr := httptest.NewRecorder()
		h.Post(rr, req)
	})
}
