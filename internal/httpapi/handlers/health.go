package handlers

import (
	"io"
	"net/http"
)

func Health(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "ok\n")
}
