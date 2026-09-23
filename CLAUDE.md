# True — notes for Claude Code

- **What to build next:** `docs/PLAN.md` (shared with the `queenroyal` repo; keep both copies
  identical). Items marked **E** are engine work.
- **Rules:** money is `shopspring/decimal` / `NUMERIC(18,4)`, never float. Ledger writes are
  double-entry and idempotent. Migrations in `migrations/` are numbered, have `up` and `down`,
  and are never edited after merge. RTP tests in `internal/game` are a build gate.
- **Contract:** the gateway (queenroyal) calls `internal/api/router.go` routes, HMAC-signed.
  Endpoint changes land here first; the gateway follows.
- **Checks before commit:** `go build ./... && go vet ./... && go test -race -count=1 ./...`
- Work on a feature branch, one PR per plan item, and tick the item in `docs/PLAN.md`.
