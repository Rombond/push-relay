# push-relay

A small, generic push relay for self-hosted apps. The app publisher runs it once; every self-hosted server sends it "something happened" and the relay forwards it to APNs (iOS) or FCM (Android). Built for [Tipsarr](https://github.com/Rombond/tipsarr), usable by any app.

Why a relay: APNs and FCM only accept messages signed with the publisher's key, so a user's own server cannot push to a store app it did not publish.

- **Fixed message shape, no free text by default.** The relay builds the alert from `event` + `lang` using its own templates, so a stolen key cannot put arbitrary text on a lock screen. The app fetches details from its own server when opened. An app can opt in with `free_text: true` in the config: its servers may then send `title` (100 characters) and `body` (300), cleaned of control characters, replacing the template text. Only for servers you run yourself. Titles and bodies are never logged: a log line holds the key id, app, platform, event and a short hash of the device.
- **Per-server API keys**, scoped to one app, stored hashed, revocable at once.
- **Rate limits** per key and per device token, daily quota per key.
- **Open registration** (optional, per app): any self-hosted server of a public store app gets its own key with `POST /v1/register`, under a global hourly cap, with a daily quota and templates only.
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
| `level` | v0.3.0, `free_text` apps: `passive` (no screen wake), `active` (default), `time-sensitive` (breaks through allowed Focus modes). `critical` is not offered (needs an Apple entitlement) |
| `sound` | v0.3.0, `free_text` apps: `default` or `none` (iOS; Android plays the channel sound, the app gets `sound: none` in `data`) |
| `thread` | v0.3.0, `free_text` apps: groups notifications, `[A-Za-z0-9._:-]{1,64}` (default: one group per `id`) |
| `collapse` | v0.3.0, `free_text` apps: a push with the same value replaces the previous one (`apns-collapse-id`, FCM `collapse_key` and tag) |
| `ttl` | v0.3.0, `free_text` apps: seconds an offline device may still get it, 0 to 2419200 (default 24 h); 0 = now or never |
| `image` | v0.3.0: `https` URL (512 characters max) on a host of the app's `image_domains`, or a URL from `POST /v1/media`. iOS: `mutable-content: 1` and `image` in the payload, for the app's Notification Service Extension; Android: `notification.image` |

Body max 8 KB, unknown fields rejected. A request without the v0.3.0 fields produces exactly the v0.2.0 payload (golden tests in `internal/apns` and `internal/fcm`).

Reply `{"status": "sent" | "invalid_token" | "rate_limited" | "quota_exceeded" | "error", "retryAfter"?, "error"?}`:

| HTTP | Meaning |
|---|---|
| 200 | `sent`, or `invalid_token` (delete that device on your side) |
| 400 / 403 / 413 | invalid request, key not valid for this app, body too large |
| 401 | missing, unknown or revoked key |
| 429 | `rate_limited`, or `quota_exceeded` (daily quota, `retryAfter` = seconds to next UTC midnight), see `Retry-After` |
| 502 | APNs/FCM failed, retry with backoff |

`POST /v1/media` (v0.3.0, apps with `media: true`, relay with `media.base_url`): raw JPEG or PNG body (1 MiB max) with the server key. Answers `{"url": "https://<base>/m/<id>?sig=...", "expiresAt": ...}`. The image is kept in memory for `media.ttl` (1 h) and `media.max_uses` downloads (5), then gone; a restart forgets every image. `GET /m/<id>?sig=...` serves it (404 otherwise). Use it for private pictures (camera snapshot) that the phone must fetch from outside the home network: it makes `/m/` public on the relay's route.

`POST /v1/push/batch` takes `{"items": [ ... ]}` (1 to 100 entries) and always answers 200 with `{"items": [result, ...]}` in the same order. `GET /healthz`. Prometheus counters on the separate `admin_listen` port at `/metrics`.

### Open registration (v0.4.0)

For apps with `open_registration: true`. No authentication: the server identifies itself with an `installId`, a random id it generates once and keeps (16-128 chars `[A-Za-z0-9_-]`, 128 bits of randomness or more).

`POST /v1/register`:

```json
{ "app": "tipsarr", "installId": "Zx8...", "serverUrl": "https://tipsarr.example.org", "version": "1.4.0" }
```

`serverUrl` (http/https, 256 chars) and `version` (`[A-Za-z0-9._+-]{1,32}`) are optional, kept for abuse investigation. Body max 2 KB, unknown fields rejected.

Reply `{"key": "pr_...", "keyId": 12, "app": "tipsarr", "rotated": false, "dailyQuota": 200}` (`dailyQuota` 0 = unlimited). The key is shown once; store it and send it as `Bearer` like any key.

| HTTP | `error` | Meaning |
|---|---|---|
| 201 | | new key |
| 200 | | same `app` + `installId` registered again: the key is rotated, the previous one stops working at once. Retrying or reinstalling costs nothing |
| 400 | `invalid_request`, `unknown_app`, `invalid_install_id`, `invalid_server_url`, `invalid_version` | fix the request, do not retry |
| 403 | `registration_not_allowed` | the app does not accept registration |
| 403 | `registration_closed` | kill switch on; retry much later (hours) |
| 429 | `rate_limited` | too many calls from this address, see `Retry-After` |
| 429 | `registration_cap` | the relay's hourly cap of new keys is reached, see `Retry-After` |

A key revoked by the operator answers 401 on push; register again with the same `installId` gets a new key (counts as new). On 401 the server should re-register once, not in a loop.

Public keys differ from manual ones: daily quota `registration.daily_quota` (200) unless raised with `relay keys quota`, never `free_text` (`title`/`body` ignored, `level`/`sound`/`thread`/`collapse`/`ttl` refused with 400) whatever the app allows, deleted after `registration.unused_days` (90) without any request.

Events for Tipsarr: `request.approved`, `request.declined`, `request.failed`, `request.available`, `request.created`, `issue.created`, `issue.comment`, `issue.resolved`. Texts: `internal/templates/tipsarr.yaml`. Another app brings its own file with `templates:` in the config.

## Run

```sh
cp deploy/relay.example.yaml config/relay.yaml     # fill bundle id, team id, key id, project id
# put the APNs .p8 and the FCM service account JSON under secrets/
docker compose -f deploy/compose.example.yaml up -d
docker compose exec push-relay /relay check                                       # validate config, templates and credentials
docker compose exec push-relay /relay keys add -app tipsarr -name "home server"   # prints the key once
docker compose exec push-relay /relay keys list
docker compose exec push-relay /relay keys revoke 1
docker compose exec push-relay /relay keys quota 12 1000                          # 0 = unlimited, default = back to the default
docker compose exec push-relay /relay keys revoke -since 2026-11-07T10:00:00Z -until 2026-11-07T12:00:00Z   # public keys only
docker compose exec push-relay /relay keys revoke -ip 203.0.113.7                 # public keys registered from this address
docker compose exec push-relay /relay registration close                          # kill switch, no restart (also: open, status)
```

### Open registration settings

```yaml
registration:
  closed: false            # kill switch (also RELAY_REGISTRATION_CLOSED=1, or `relay registration close` at runtime)
  new_keys_per_hour: 50    # new public keys over a sliding hour, all apps and addresses; rotations do not count; -1 = no cap
  per_ip_per_minute: 30    # HTTP rate limit of /v1/register per client address
  daily_quota: 200         # pushes per public key per UTC day; -1 = unlimited
  unused_days: 90          # public keys without any request this long are deleted (daily job); -1 = never
  ip_retention_days: 30    # registration address hashes are dropped after this; -1 = forever
  client_ip_header: ""     # e.g. X-Forwarded-For behind Caddy (last entry used); empty = TCP peer
apps:
  tipsarr:
    open_registration: true
```

There is no per-address key limit: many households share an address (CGNAT), and the global cap bounds the damage of a flood. Manual keys (`relay keys add`) keep `limits.daily_quota` (0 = unlimited by default).

Metrics on `admin_listen`: `push_relay_keys{kind}` (active keys, public/manual), `push_relay_registrations_total{app}`, `push_relay_rotations_total{app}`, `push_relay_registration_refusals_total{reason}`, `push_relay_quota_refusals_total{app}`, and `push_relay_pushes_total{app,platform,status}` for sends.

Without Docker: `go build ./cmd/relay && ./relay serve -config relay.yaml`.

## Develop

`go vet ./... && go test -race ./...`. Tests use fake APNs (HTTP/2 TLS) and FCM servers; nothing talks to Apple or Google.

Layout: `cmd/relay` (CLI + server wiring), `internal/server` (API, limits, validation), `internal/apns`, `internal/fcm` (senders behind `push.Sender`), `internal/store` (keys, usage), `internal/limit` (token buckets), `internal/templates`, `internal/config`.

## Security notes

Registration address: the relay stores an HMAC-SHA256 of the client address, keyed with a random secret kept in the database, never the address. A mass revoke only needs equality (`-ip` hashes the given address the same way); the secret prevents a precomputed table, but since IPv4 is small, anyone holding the database could still test every address: it is pseudonymisation, not anonymisation, hence the `ip_retention_days` purge. `serverUrl`, `version`, creation, rotation and last use times are kept for abuse handling.

Keep `/secrets` read-only and out of the image. The relay holds the push credentials of every app it serves: anyone who reads them can push to that app's users. Expose only the API port through the reverse proxy; `admin_listen` is for metrics on a private network.

MIT licensed.
