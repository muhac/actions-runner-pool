// Package handlers contains HTTP request handlers for the autoscaler's API.
package handlers

import (
	"io"
	"net/http"
)

// Health returns an OK status for liveness/readiness probes.
func Health(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, "ok\n")
}
