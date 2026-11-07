# push-relay

Generic multi-app push relay (APNs + FCM), Go, MIT. Companion of Tipsarr (`~/Documents/Dev/arrStack/tipsarr`). Plan and decisions live in the Obsidian vault: `Serveur/Dev/Tipsarr/plans/Tipsarr - Push Relay Plan.md`.

- Checks: `gofmt -l .`, `go vet ./...`, `go test -race ./...` (CI runs the same plus a Docker build).
- Never log or store device tokens or raw keys. Alert text comes from templates; the request's `title`/`body` replace it only for apps with `free_text: true`. Never log them.
- v0.3.0 fields (`level`, `sound`, `thread`, `collapse`, `ttl`) are `free_text` only; `image` needs the app's `image_domains` (or its own `POST /v1/media` upload). Absent fields must keep the v0.2.0 payload byte for byte: `regression_test.go` in `internal/apns` and `internal/fcm`.
- v0.4.0 open registration: public keys (`POST /v1/register`) never get free_text (`freeText()` in server.go). Schema changes go in `store.migrations` (PRAGMA user_version), never edit `schema`.
- `relay check` validates a config before a restart.
- New event: add it to `internal/templates/<app>.yaml` (en required) and to the README list.
- Never deploy or add a reverse proxy route without the user's explicit go.
