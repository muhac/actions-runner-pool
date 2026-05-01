package main

import (
	"fmt"

	"github.com/muhac/actions-runner-pool/internal/store"
)

// checkBaseURLDrift detects BASE_URL mismatch between config and persisted app_config.
// Warns if they differ (webhooks would hit the old URL).
func checkBaseURLDrift(existing *store.AppConfig, configured string) (warn bool, msg string) {
	if existing == nil {
		return false, ""
	}
	if existing.BaseURL == configured {
		return false, ""
	}
	return true, fmt.Sprintf(
		"BASE_URL drift: configured=%q but app_config has %q; GitHub still points at the old URL — re-run /setup or revert BASE_URL",
		configured, existing.BaseURL,
	)
}
