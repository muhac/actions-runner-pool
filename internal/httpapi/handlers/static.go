package handlers

import (
	"io/fs"
	"net/http"
)

// CSSHandler serves the embedded stylesheet bundle under /css/.
// Files live in templates/css/ alongside the HTML templates (so a
// single embed.FS covers both) and are exposed read-only.
func CSSHandler() http.Handler {
	sub, err := fs.Sub(templatesFS, "templates/css")
	if err != nil {
		// fs.Sub on a static, vetted embed path can only fail at build
		// time (missing directory). Panicking surfaces that immediately
		// instead of returning 404s at runtime.
		panic("css embed: " + err.Error())
	}
	return http.StripPrefix("/css/", http.FileServer(http.FS(sub)))
}
