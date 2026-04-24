package github

import (
	"context"
	"errors"
)

// AppJWT mints a short-lived JWT signed with the App private key (PEM).
// TODO: implement RS256 signing per https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-json-web-token-jwt-for-a-github-app
func (c *Client) AppJWT(pem []byte, appID int64) (string, error) {
	return "", errors.New("AppJWT: not implemented")
}

// InstallationToken exchanges the App JWT for a per-installation token (~1h TTL).
// TODO: cache per installationID until ~55 min before expiry.
func (c *Client) InstallationToken(ctx context.Context, jwt string, installationID int64) (string, error) {
	return "", errors.New("InstallationToken: not implemented")
}
