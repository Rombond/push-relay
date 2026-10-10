# push-relay

A small, generic push relay for self-hosted apps. The app publisher runs it once; every self-hosted server sends it "something happened" and the relay forwards it to APNs (iOS) or FCM (Android). Built for [Tipsarr](https://github.com/Rombond/tipsarr), usable by any app.

Why a relay: APNs and FCM only accept messages signed with the publisher's key, so a user's own server cannot push to a store app it did not publish.

- **Fixed message shape, no free text by default.** The relay builds the alert from `event` + `lang` using its own templates, so a stolen key cannot put arbitrary text on a lock screen. The app fetches details from its own server when opened. An app can opt in with `free_text: true` in the config: its servers may then send `title` (100 characters) and `body` (300), cleaned of control characters, replacing the template text. Only for servers you run yourself. Titles and bodies are never logged: a log line holds the key id, app, platform, event and a short hash of the device.
- **Per-server API keys**, scoped to one app, stored hashed, revocable at once.
- **Rate limits** per key and per device token, optional daily quota per key.
- **Stateless about devices**: tokens are never stored or logged (only a hash prefix).
- APNs token auth (HTTP/2, sandbox flag per request), FCM HTTP v1 (service account). Pure Go, one static binary, SQLite for keys.

## API

`POST /v1/push` with `Authorization: Bearer <server key>`:

```json
{ "platform": "ios", "token": "<device token>", "sandbox": false,
  "event": "request.approved", "id": "123", "lang": "fr" }
```

| Field | Rule |
|---|---|
| `app` | optional, must match the key's app |
| `platform` | `ios` or `android` |
| `token` | iOS: 64-200 hex chars; Android: 20-1000 chars `[A-Za-z0-9:_-]` |
| `event` | must exist in the app's templates |
| `title`, `body` | optional, used only when the app has `free_text: true`, ignored otherwise |
| `id` | `[A-Za-z0-9_-]{1,64}` |
| `lang` | `en`, `fr`, ... falls back to `en` |
| `server` | optional host (and port) of the Tipsarr server; sent to the app as `server` so it opens the right account |

Body max 4 KB, unknown fields rejected.

Reply `{"status": "sent" | "invalid_token" | "rate_limited" | "error", "retryAfter"?, "error"?}`:

| HTTP | Meaning |
|---|---|
| 200 | `sent`, or `invalid_token` (delete that device on your side) |
| 400 / 403 / 413 | invalid request, key not valid for this app, body too large |
| 401 | missing, unknown or revoked key |
| 429 | rate limited or daily quota, see `Retry-After` |
| 502 | APNs/FCM failed, retry with backoff |

`POST /v1/push/batch` takes `{"items": [ ... ]}` (1 to 100 entries) and always answers 200 with `{"items": [result, ...]}` in the same order. `GET /healthz`. Prometheus counters on the separate `admin_listen` port at `/metrics`.

Events for Tipsarr: `request.approved`, `request.declined`, `request.failed`, `request.available`, `request.created`, `issue.created`, `issue.comment`, `issue.resolved`. Texts: `internal/templates/tipsarr.yaml`. Another app brings its own file with `templates:` in the config.

## Run

```sh
cp deploy/relay.example.yaml config/relay.yaml     # fill bundle id, team id, key id, project id
# put the APNs .p8 and the FCM service account JSON under secrets/
docker compose -f deploy/compose.example.yaml up -d
docker compose exec push-relay /relay keys add -app tipsarr -name "home server"   # prints the key once
docker compose exec push-relay /relay keys list
docker compose exec push-relay /relay keys revoke 1
```

Without Docker: `go build ./cmd/relay && ./relay serve -config relay.yaml`.

## Develop

`go vet ./... && go test -race ./...`. Tests use fake APNs (HTTP/2 TLS) and FCM servers; nothing talks to Apple or Google.

Layout: `cmd/relay` (CLI + server wiring), `internal/server` (API, limits, validation), `internal/apns`, `internal/fcm` (senders behind `push.Sender`), `internal/store` (keys, usage), `internal/limit` (token buckets), `internal/templates`, `internal/config`.

## Security notes

Keep `/secrets` read-only and out of the image. The relay holds the push credentials of every app it serves: anyone who reads them can push to that app's users. Expose only the API port through the reverse proxy; `admin_listen` is for metrics on a private network.

MIT licensed.
