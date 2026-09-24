# QueenRoyal — Launch Plan (shared by `True` and `queenroyal`)

> This file is kept **identical** in both repositories:
> `Gavrielsh/True` → `docs/PLAN.md` and `Gavrielsh/queenroyal` → `docs/PLAN.md`.
> When you change it in one repo, make the same change in the other in the same session.
>
> It replaces `ROADMAP.md` as the source of truth for *what is left*. `ROADMAP.md` is out of
> date: it still lists redemption, AMOE, engine CI, the suspended-player check and the
> Daily Wheel as open, and they are built.

Last updated: 2026-09-23

## Who owns what

| Layer | Repo | Stack | Talks to |
|---|---|---|---|
| **Engine** (the ledger) | `True` | Go, Gin, Postgres, Redis | Called only by the gateway, HMAC-signed |
| **Gateway** | `queenroyal/apps/financial-gateway` | Fastify, Prisma, Postgres, Redis | Engine + browser |
| **Web** | `queenroyal/src` | Next.js 15, React 19, TanStack Query | Gateway only |

Engine routes today (`True/internal/api/router.go`): `/api/v1/spin`, `/bet`, `/win`,
`/rollback`, `/session`, `/player/create`, `/store/purchase`, `/store/redeem`,
`/store/promo-grant`, `/kyc/decision`, plus `/healthz` and `/metrics`.

### Rules that never bend
- **queenroyal:** `.claude-instructions` (G1–G4). The gateway holds no balances; money is a
  validated decimal string end to end, never a JS number; every mutating call carries an
  idempotency key that is reused across retries; on engine 5xx/timeout, fail closed and honestly.
- **True:** money is `shopspring/decimal` in Go and `NUMERIC(18,4)` in Postgres; every ledger
  write is double-entry and idempotent; migrations are numbered, have a `down`, and are never
  edited after merge. The RTP tests are a build gate.
- **Cross-repo contract:** if the gateway and the engine disagree, the engine wins. A change to
  an engine endpoint lands in `True` first; the gateway change follows and cites the `True` PR.

### Status legend
`[ ]` open · `[~]` in progress · `[x]` done · ⚖️ legal blocker for launch ·
**E** = True engine · **G** = gateway · **W** = web

---

## Step 0 — Put what already exists live (ops, no code)
- [ ] Deploy the engine from `True` master (runs migrations `000010_promo_grants`,
  `000011_responsible_gaming`).
- [ ] Deploy the gateway (Prisma migration adds `daily_bonus_claims` and the 4 new `users`
      columns from the sign-up work).
- [ ] Serve the gateway on the same site as the web app (e.g. `api.` next to `www.`) and list
      the web origin in `CORS_ALLOWED_ORIGINS`, or the browser will not send the login cookie.
- [ ] Set `NEXT_PUBLIC_META_LIVE=entrance,dailyWheel,streak` and deploy the web app.
- [ ] Staging smoke test: sign up, log in, spin the wheel, balance goes up, a second spin that
      day is refused.
- [ ] A lawyer confirms the minimum age and the list of blocked states.

## Step 1 — Legal must-haves before launch ⚖️

| # | Item | Layers | Exists | Missing |
|---|---|---|---|---|
| 1 | Login & registration | G W | Register/login/refresh/logout, sign-up with 18+, state and terms checks (queenroyal#17) | `[x]` done. Only the Step 0 deploy items remain. |
| 2 | **Legal pages** | W | Nothing. The sign-up checkbox and footer link to pages that don't exist. | Terms of Service, Official Sweepstakes Rules, Privacy Policy, Responsible Gaming page. Static, versioned, linked from footer and sign-up. |
| 3 | **Redemption (cash out SC)** | E G W | Engine `/store/redeem`, gateway service + worker, admin approve/reject | Player redemption screen (eligibility, min amount, daily cap, status history). A real payout provider behind the `PaymentProvider` seam; only a fake one exists. |
| 4 | **Free entry (AMOE)** | W | Gateway `/api/amoe` | Public page with the free-entry rules and the request form. |
| 5 | **KYC** | G W | Data model, webhook, fake provider, engine `/kyc/decision` | Real provider adapter (Persona / Veriff / Jumio) and a player document-upload flow. |
| 6 | **Responsible gaming** | E G W | `[x]` **E**: migration `000011_responsible_gaming`, engine guards self-exclusion / cool-off / loss limit / deposit limit inside the wallet-locked transaction for `/spin`, `/bet`, `/win`, `/rollback`, `/store/purchase`; signed `POST /player/limits` (set/lift), `/player/limits/query`, and the pre-charge `/player/limits/check-purchase` (branch `feat/responsible-gaming`). | Session limits, reality-check reminders. Gateway: store settings, call `/player/limits/check-purchase` before charging a card, refund if `/store/purchase`'s backstop ever refuses. Web: `/account/limits` page. |
| 7 | **Fraud & AML** | E G | Nothing | Velocity rules, device & IP fingerprinting, multi-account detection, bonus-abuse checks, redemption reporting. |

## Step 2 — The product players see

| # | Item | Layers | Notes |
|---|---|---|---|
| 8 | Real lobby | G W | `Game` model + `GET /api/games`. The lobby UI exists and runs on demo data. |
| 9 | More games | E (then G W) | Shared `Game` interface in the engine, then 5-reel slots with free spins, then more titles. Each game keeps its RTP tests. |
| 10 | Live balance (SSE) | G | Web client exists. Needs `/api/wallet/stream` and Redis pub/sub. Events are invalidation signals only (G1). |
| 11 | Backends for designed features | G (E where money moves) W | VIP levels, missions & chests, tournaments & leaderboards, jackpot & recent wins. Same recipe as the Daily Wheel: gateway endpoint → client parser → tests → add to `LIVE_CAPABLE` in `src/lib/meta/features.ts`. |
| 12 | Richer store | G W | First-purchase offer, time-limited bundles, personalised offers. Stripe is already integrated. |

## Step 3 — Operations

| # | Item | Layers |
|---|---|---|
| 13 | Admin panel UI: player search, ledger view, KYC review queue, manual adjustments (through the engine, never local) | E G W |
| 14 | Analytics (DAU, retention, actual vs declared RTP), notifications (email, push, in-app), load tests for `/spin` (`True/loadtest`) | E G |
| 15 | Rewrite `ROADMAP.md` to match what is built, or delete it in favour of this file | — |

## Order
**0 → 2 (legal pages) → 3 (redemption) → 4 (AMOE) → 5 (KYC) → 6 (responsible gaming) → 7 →
8 + 10 → 11 → the rest.** If time runs short, cut game breadth (9), never compliance.

Parallel tracks that do not block each other:
- **True** can run 6 (engine side), 7 (engine side), 9 and 14 (load tests) while queenroyal
  does 2, 3, 4.
- Anything that adds or changes an engine endpoint: `True` PR first, then queenroyal.

## Definition of done (every item)
1. Code + tests on a feature branch, one PR per item.
2. **True:** `go build ./... && go vet ./... && go test -race -count=1 ./...` pass; new
   migration has `up` and `down`.
   **queenroyal:** `npm run typecheck && npm run lint && npm test` at the root and in
   `apps/financial-gateway`; `npx playwright test` for any user-facing change.
3. No G1–G4 violation (`queenroyal`), no float money anywhere.
4. This file updated in **both** repos: box ticked, "Exists/Missing" columns corrected.

---

## Claude Code prompts

Run `claude` from the repo root. Each prompt is self-contained; paste one per session.

### True (engine) — kickoff
```
Read docs/PLAN.md, ARCHITECTURE.md and internal/api/router.go. You are working on the True Go
ledger engine. Summarise in 10 lines: what the engine serves today, which PLAN.md items have
engine work (marked E), and in what order you would do them. Don't change code yet. Then ask me
which item to start.
```

### True — Responsible gaming, engine side (item 6)
```
Implement the engine side of PLAN.md item 6 (responsible gaming). Goal: the engine refuses
money-moving calls for a player who is self-excluded, in cool-off, or over a loss / deposit limit.
- Start with a short design in chat: tables (new numbered migration with up and down), which
  handlers check limits (/spin, /bet, /store/purchase), the error code returned, and a signed
  admin-style endpoint the gateway uses to set limits. Wait for my OK.
- Money is shopspring/decimal and NUMERIC(18,4). Limits are checked inside the same transaction
  that locks the wallet, so a concurrent spin cannot slip past.
- Tests: table tests for each limit type, a concurrency test, and a router test for the new
  endpoint with HMAC.
- Run go build ./..., go vet ./..., go test -race -count=1 ./... before every commit.
- Update docs/PLAN.md (and tell me the exact diff so I can copy it to queenroyal).
Work on a new branch, commit in small steps, open a PR when green.
```

### True — Game interface + 5-reel slot (item 9)
```
Implement PLAN.md item 9 in internal/game. Step 1: extract a Game interface from the existing
slot without changing its behaviour or RTP (all existing RTP tests must pass untouched). Step 2:
add a 5-reel slot with free spins, with its own declared RTP, TestDeclaredRTPMatchesModel,
TestRTPInRegulatoryBand and a Monte-Carlo convergence test. /spin must select the game by id;
unknown ids are a 400. Show me the interface before writing step 2. go test -race must pass.
Update docs/PLAN.md. New branch, PR when green.
```

### True — Fraud signals, engine side (item 7)
```
Implement the engine side of PLAN.md item 7. Add velocity limits per player on /spin,
/store/purchase and /store/redeem (configurable, Redis-backed, fail-closed if Redis is down),
and a signed read endpoint that returns a player's recent risk signals for the gateway's admin
queue. Propose the design first and wait for my OK. Tests + go test -race. Update docs/PLAN.md.
```

### queenroyal — kickoff
```
Read docs/PLAN.md, .claude-instructions (rules G1–G4, never break them), ARCHITECTURE.md and
apps/financial-gateway/src/routes. Summarise in 10 lines what is live, what the next PLAN.md
item is, and your plan for it. Don't change code yet. Then ask me to confirm.
```

### queenroyal — Legal pages (item 2)
```
Implement PLAN.md item 2: Terms of Service, Official Sweepstakes Rules, Privacy Policy and a
Responsible Gaming page as Next.js routes under src/app (/terms, /rules, /privacy,
/responsible-gaming). Content comes from versioned markdown files (e.g. content/legal/*.md with a
version and effective date at the top) so a lawyer can edit them without touching code. Write
placeholder text that is clearly marked DRAFT — NOT LEGAL ADVICE, covering the standard sections
of a US sweepstakes casino (no purchase necessary, AMOE, eligibility 18+ and excluded states from
the existing sign-up config, prize redemption, SC expiry). Wire the sign-up checkbox and footer
links to them, and store the accepted terms version on sign-up if the user model has a field for
it (propose a Prisma migration if not). Vitest + a Playwright test that every footer link returns
200. Run typecheck, lint, tests. Update docs/PLAN.md. New branch, PR when green.
```

### queenroyal — Redemption screen (item 3)
```
Implement the player redemption screen for PLAN.md item 3. The gateway service, worker and
engine /store/redeem already exist — read them first and use them, don't reinvent. The screen
shows redeemable SC, minimum amount, daily cap, KYC requirement (link to KYC if not VERIFIED),
a form that mints an idempotency key per attempt and reuses it on retry (G3), and a history of
the player's redemption requests with status. Money is a decimal string only (G2); on 5xx or
timeout show an honest error and never touch the balance cache (G1, G4). If a history endpoint
is missing, add GET /api/store/redemptions to the gateway. Vitest for every state (200, 400,
403 KYC, 409, 503), Playwright for the happy path. Update docs/PLAN.md. New branch, PR when green.
```

### queenroyal — AMOE page (item 4)
```
Implement the public AMOE page for PLAN.md item 4 on top of the existing /api/amoe route (read
it first). Public route /free-entry: the free-entry rules, and a request form for logged-in
players that calls /api/amoe with an idempotency key; logged-out visitors see the rules and a
login link. Link it from the footer and from /rules. Vitest + Playwright. Update docs/PLAN.md.
```

### queenroyal — Real KYC provider (item 5)
```
Implement PLAN.md item 5. Add a real KYC provider adapter next to the fake one (propose Persona,
Veriff or Jumio with a one-paragraph trade-off and wait for my choice). Verify webhook signatures,
keep the fake as the default when the provider env vars are unset, and add a player flow that
starts verification and shows the current KYC status. Never log document data. Tests for webhook
signature failure, replay and each status transition. Update docs/PLAN.md.
```

### queenroyal — Responsible gaming UI + gateway (item 6, after the True PR merges)
```
The engine side of PLAN.md item 6 is merged in True (read its PR/diff via docs/PLAN.md). Build
the gateway endpoints that set and read limits through the engine, and a /account/limits page:
self-exclusion (with confirm and a fixed period), cool-off, deposit / loss / session limits, and
a reality-check reminder every N minutes of play. Limit increases take effect after 24 h;
decreases are immediate. Handle the engine's refusal code on spin and purchase with clear copy.
Tests + Playwright. Update docs/PLAN.md.
```

### Any repo — keep the plan in sync
```
Compare docs/PLAN.md with the code in this repo and mark every item whose status is wrong
(done but unchecked, or checked but missing). Show me the diff; after I approve, commit it and
print the same diff so I can apply it to the other repo.
```
