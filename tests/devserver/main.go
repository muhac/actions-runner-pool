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

	"github.com/muhac/actions-runner-pool/internal/httpapi/handlers"
)

func main() {
	port := "18080"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}

	mux := http.NewServeMux()

	dashboard := &handlers.DashboardHandler{}
	mux.HandleFunc("GET /{$}", dashboard.Get)
	mux.Handle("GET /css/", handlers.CSSHandler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	addr := "127.0.0.1:" + port
	log.Printf("devserver listening on http://%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
