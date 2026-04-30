package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AppJWT mints a short-lived JWT signed with the App private key (RS256).
// Claims: iat=now-60s (clock skew), exp=now+10m, iss=appID.
func (c *Client) AppJWT(pem []byte, appID int64) (string, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM(pem)
	if err != nil {
		return "", fmt.Errorf("parse pem: %w", err)
	}
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
		Issuer:    fmt.Sprintf("%d", appID),
	})
	signed, err := tok.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return signed, nil
}

// cachedInstallationToken is a single entry in the token cache: the raw
// token string and its effective expiry (server-reported expires_at minus
// a 5-minute safety margin).
type cachedInstallationToken struct {
	token string
	exp   time.Time
}

// InstallationToken returns a cached or freshly minted installation token.
// Cache TTL is server-reported expires_at minus 5-minute safety margin.
func (c *Client) InstallationToken(ctx context.Context, jwt string, installationID int64) (string, error) {
	if v, ok := c.tokenCache.Load(installationID); ok {
		ct := v.(cachedInstallationToken)
		if c.nowFn().Before(ct.exp) {
			return ct.token, nil
		}
		// Stale entry — drop it so the cache doesn't grow forever for
		// installations that came and went.
		c.tokenCache.Delete(installationID)
	}
	tok, exp, err := c.fetchInstallationToken(ctx, jwt, installationID)
	if err != nil {
		return "", err
	}
	c.tokenCache.Store(installationID, cachedInstallationToken{
		token: tok,
		exp:   exp.Add(-5 * time.Minute),
	})
	return tok, nil
}

func (c *Client) fetchInstallationToken(ctx context.Context, jwt string, installationID int64) (string, time.Time, error) {
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", c.cfg.GitHubAPIBase, installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return "", time.Time{}, fmt.Errorf("installation token: status %d", resp.StatusCode)
	}
	var body struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", time.Time{}, fmt.Errorf("decode token: %w", err)
	}
	if body.Token == "" {
		return "", time.Time{}, errors.New("installation token: empty token in response")
	}
	if body.ExpiresAt.IsZero() {
		return "", time.Time{}, errors.New("installation token: missing expires_at in response")
	}
	if !body.ExpiresAt.After(c.nowFn()) {
		return "", time.Time{}, errors.New("installation token: expires_at already in the past")
	}
	return body.Token, body.ExpiresAt, nil
}
