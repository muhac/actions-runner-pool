# Credential rotation

gharp's GitHub App credentials (`webhook_secret`, `pem`, `client_secret`)
are populated once during the manifest-flow setup at `/setup` and
persisted in the `app_config` table. Until v1.3 (or whatever release
introduces this endpoint), the only path to rotate any of them was to
delete the DB row, delete the GitHub App, and re-run `/setup` — painful
during a compliance window or a suspected leak.

This page documents the rotation endpoint that replaces that workflow.

## Endpoint

```
PATCH /admin/app-config
Authorization: Bearer <ADMIN_TOKEN>
Content-Type: application/json

{
  "webhook_secret": "<new-secret>",   // optional
  "pem":            "<new-pem>",      // optional
  "client_secret":  "<new-secret>"    // optional
}
```

Each field is independent. Send any subset; omitted fields are left
untouched.

## Prerequisites

- **`ALLOW_ADMIN_EDIT=true`** must be set in gharp's environment. With
  the default `false`, the endpoint returns `403 Forbidden`. This is a
  deliberate kill-switch: an accidentally-leaked `ADMIN_TOKEN` should
  not, on its own, be enough to rotate credentials.
- **`ADMIN_TOKEN`** should be set in gharp's environment for any
  non-localhost deployment. The flag (`ALLOW_ADMIN_EDIT`) alone does
  *not* authenticate callers — when `ADMIN_TOKEN` is empty and the
  flag is on, the endpoint is open to anyone who can reach gharp.
  See [configuration.md](./configuration.md#auth-ordering).
- gharp must already be set up (`app_config` row present). The endpoint
  returns `409 Conflict` if you haven't run `/setup` yet.

## Validation

| Field | Rules |
| --- | --- |
| `webhook_secret` | trimmed, length ≥ 16. Sending the current value is a no-op (returns 200 with `rotated: []`, no DB write). |
| `pem` | must parse as an RSA private key (PKCS#1 PEM). Whitespace around the value is preserved; an all-whitespace value is rejected. |
| `client_secret` | non-empty after trim. |

## Response

```json
{
  "rotated": ["webhook_secret", "pem"],
  "webhook_secret_fingerprint": "sha256:abcdef012345",
  "pem_fingerprint":            "sha256:0123abcdef67",
  "client_secret_fingerprint":  "sha256:fedcba987654"
}
```

`rotated` lists the fields that actually changed (an absent field or a
no-op value won't appear). Fingerprints are the first 12 hex chars of
`sha256(value)` for each currently-stored field — enough to confirm
which version is live without re-exposing the secret.

## Hot-swap behavior

Every consumer reads the relevant `app_config` field fresh from the
store on each request, so the new value is in effect immediately
without a restart:

| Field | Read site |
| --- | --- |
| `webhook_secret` | every incoming webhook signature check (`internal/httpapi/handlers/webhook.go`) |
| `pem` | every JWT mint (scheduler dispatch, reconciler sweeps) |
| `client_secret` | not read at runtime (used only during the initial OAuth manifest exchange); kept rotatable for completeness and compliance |

### Caveat: installation tokens after PEM rotation

The PEM only signs the App-level JWT; the **installation tokens**
that JWT mints are issued by GitHub and remain valid until their
GitHub-side expiry (~1 hour) regardless of which PEM signed the
mint request. gharp caches installation tokens at
`internal/github/client.go` to stay under the App's rate limit. After
a PEM rotation:

- gharp's next JWT mint uses the new PEM automatically (read fresh
  from the store) — no restart needed.
- Already-cached installation tokens stay usable until their natural
  expiry. This is harmless: those tokens were issued legitimately
  and would have been usable anyway.
- **If you suspect the old PEM is leaked**, rotating in gharp is
  *not* sufficient. An attacker holding the leaked PEM can mint
  fresh JWTs and request installation tokens directly from GitHub
  without going through gharp at all. The only way to truly stop
  the leaked PEM from being usable is to **delete it in the GitHub
  App's settings** (App settings → Private keys → trash icon next
  to the old key). gharp's rotation endpoint cannot do that for
  you — GitHub doesn't expose a public API to delete App private
  keys.

## Ordering (important for `webhook_secret`)

GitHub signs webhooks with a single secret at a time — when you change
the secret on GitHub's side, in-flight events still in their delivery
queue may have been signed with the old one. Recommended sequence:

1. **GitHub first.** In the App settings, generate or paste the new
   webhook secret. From this moment GitHub will sign new deliveries
   with the new secret.
2. **gharp within a few minutes.** PATCH the same value to
   `/admin/app-config`. Until gharp catches up, signature checks fail
   and return `401 Unauthorized`; GitHub redelivers failed events
   automatically (up to ~24 hours), so a small window is safe.

For `pem`, GitHub keeps both old and new keys valid simultaneously
during a transition — the order is forgiving:

1. Generate a new key in the App settings (GitHub gives you a fresh
   `.pem` to download).
2. PATCH it to gharp.
3. Verify by triggering any workflow; if the dispatch path works (look
   for new installation tokens minting cleanly), delete the old key in
   App settings.

For `client_secret`, the rotation is offline — nothing in the running
gharp uses it after setup. Just keep gharp's stored copy in sync with
GitHub's so a future re-run of `/setup` would succeed.

## curl recipes

Rotate one field:

```sh
curl -X PATCH "$BASE_URL/admin/app-config" \
     -H "Authorization: Bearer $ADMIN_TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"webhook_secret":"the-new-value-of-at-least-16-chars"}'
```

Rotate PEM from a file (multi-line value):

```sh
curl -X PATCH "$BASE_URL/admin/app-config" \
     -H "Authorization: Bearer $ADMIN_TOKEN" \
     -H "Content-Type: application/json" \
     --data @<(jq -Rs '{pem: .}' < ./new-private-key.pem)
```

Rotate everything in one call (useful right after creating a fresh App
in GitHub):

```sh
curl -X PATCH "$BASE_URL/admin/app-config" \
     -H "Authorization: Bearer $ADMIN_TOKEN" \
     -H "Content-Type: application/json" \
     --data @<(jq -nR --arg ws "$NEW_WEBHOOK_SECRET" \
                       --arg cs "$NEW_CLIENT_SECRET" \
                       --rawfile pem ./new-private-key.pem \
                       '{webhook_secret:$ws, pem:$pem, client_secret:$cs}')
```

## Response codes

| Code | Meaning |
| --- | --- |
| 200 | Rotation succeeded (or was a no-op). `rotated` lists what actually changed. |
| 400 | Validation failed (bad PEM, too-short secret, unknown JSON field, empty body, etc.). |
| 401 | `ADMIN_TOKEN` is set and the bearer is missing or wrong. Checked **first**, regardless of `ALLOW_ADMIN_EDIT` — see the "Prerequisites" section above. |
| 403 | Bearer is valid (or `ADMIN_TOKEN` is unset) **and** `ALLOW_ADMIN_EDIT` is false. Set the flag in gharp's env, then retry. |
| 409 | No `app_config` row exists. Run `/setup` to bootstrap before rotating. |
| 413 | Body exceeded 64 KiB. |
| 415 | `Content-Type` is not `application/json`. |
| 500 | Store error (logged server-side). Safe to retry. |

## Dashboard UI

The dashboard at `/` includes a **Rotate credentials** section with one
form per rotatable field. The form posts to the same PATCH endpoint and
shows the returned fingerprint on success. Submit buttons render
disabled (server-side) when `ALLOW_ADMIN_EDIT` is off, with a banner
explaining how to unlock writes.
