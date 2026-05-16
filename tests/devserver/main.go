// devserver is a minimal HTTP server for Playwright tests.
// It serves the dashboard HTML and CSS using the real embedded templates,
// and exposes /healthz for the playwright webServer readinessCheck.
// All API endpoints (/stats, /jobs, etc.) are intentionally omitted —
// tests mock them with page.route().
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/httpapi/handlers"
)

func main() {
	port := "18080"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}

	mux := http.NewServeMux()

	// Force the admin-write kill-switch on in the devserver so the
	// Playwright tests can click retry/cancel buttons — production
	// gharp gates them server-side rendering disabled buttons when
	// ALLOW_ADMIN_EDIT is unset (the default). The API calls those
	// buttons fire are still mocked via page.route() in the test
	// suite, so this doesn't widen the test's auth surface.
	dashboard := &handlers.DashboardHandler{Cfg: &config.Config{AllowAdminEdit: true}}
	mux.HandleFunc("GET /{$}", dashboard.Get)
	mux.Handle("GET /css/", handlers.CSSHandler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprintln(w, "ok"); err != nil {
			log.Printf("write healthz response: %v", err)
		}
	})

	addr := "127.0.0.1:" + port
	log.Printf("devserver listening on http://%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
