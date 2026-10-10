# push-relay

Generic multi-app push relay (APNs + FCM), Go, MIT. Companion of Tipsarr (`~/Documents/Dev/arrStack/tipsarr`). Plan and decisions live in the Obsidian vault: `Serveur/Dev/Tipsarr/plans/Tipsarr - Push Relay Plan.md`.

- Checks: `gofmt -l .`, `go vet ./...`, `go test -race ./...` (CI runs the same plus a Docker build).
- Never log or store device tokens or raw keys. Alert text only comes from templates, never from the request.
- New event: add it to `internal/templates/<app>.yaml` (en required) and to the README list.
- Never deploy or add a reverse proxy route without the user's explicit go.
