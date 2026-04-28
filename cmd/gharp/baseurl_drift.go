package main

import (
	"fmt"

	"github.com/muhac/actions-runner-pool/internal/store"
)

// checkBaseURLDrift detects when the configured BASE_URL differs from the
// value persisted in app_config. The persisted URL is what GitHub knows
// (it was baked into the App's webhook + callback URLs at manifest time),
// so a mismatch means webhooks and the OAuth callback will hit the old
// host. We warn but don't block startup — the operator may be in the
// middle of a planned migration.
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
