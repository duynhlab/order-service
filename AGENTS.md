# AGENTS.md

Agent-focused guide for `order-service`. Keep changes minimal, verified against
the code, and consistent with existing patterns.

## Authority and scope

This repository implements the service. It does **not** define the contract.

- **Canonical contract:** [`homelab/docs/api/order.md`](https://github.com/duynhlab/homelab/blob/main/docs/api/order.md)
- **Shared API rules:** [`homelab/docs/api/api.md`](https://github.com/duynhlab/homelab/blob/main/docs/api/api.md)

Implement against those files. When this repository and the contract disagree,
**stop and classify the mismatch** using
[Resolving a mismatch](https://github.com/duynhlab/homelab/blob/main/docs/api/README.md#resolving-a-mismatch)
before changing either side. One class — an implementation that violates the
intended contract — **blocks the release tag**. This service holds money and
stock in flight, so that rule is the cheap half of the cost.

No route, RPC, payload or error inventory belongs in this file. Manifests,
gateway routing, NetworkPolicy, database topology and platform observability
belong to [duynhlab/homelab](https://github.com/duynhlab/homelab).

## Contribution workflow

- Never commit or push to `main`. Branch first, then open a PR.
- Branch names use conventional prefixes: `feat/`, `fix/`, `docs/`, `chore/`,
  `refactor/`, `test/`.
- Commit subjects: imperative mood, capitalised, ≤ 50 characters, no trailing
  period. Add a body wrapped at 72 characters when the change is non-trivial.
- Do not add attribution trailers (`Signed-off-by`, `Co-authored-by`,
  `Generated-by`, etc.), GitHub issue references, or `@`-mentions in commit
  messages. Put issue links in the PR description.
- One logical change per PR. PRs are squash-merged and CI must be green.

## Build, test, lint

These are the commands CI runs, so a green local run means a green pipeline.

```bash
go build ./...
go vet ./...
go test -race ./...
go test -tags=integration ./internal/core/repository/...   # needs Docker (testcontainers)
golangci-lint run
```

Sonar new-code coverage must be ≥80%; `**/cmd/**`, `**/db/migrations/**` and
`**/core/repository/**` are excluded, everything else counts.

Local development against an unreleased `pkg`: `pkg` is one module per package,
so its root has no `go.mod` and a single `replace github.com/duynhlab/pkg` can no
longer resolve. Use one commented `replace` line per module — the trailer in
`go.mod` shows the shape, and
[`docs/api/pkg.md`](https://github.com/duynhlab/homelab/blob/main/docs/api/pkg.md)
explains why.

## Architecture boundaries

**3-layer, dependencies flow one way only: transport → logic → core** — plus a
saga layer that is neither.

- **Transport** — `internal/web/v1/` (HTTP, plus the east-west client adapters)
  and `internal/grpc/v1/` (the creation RPC). Cross-service entry is **gRPC**,
  not HTTP.
- **Logic** — `internal/logic/v1/`.
- **Core** — `internal/core/` owns the domain, including the status FSM, and the
  Postgres repositories.
- **Saga** — `internal/saga/` holds workflow and activity code, with
  `internal/fulfillment/`, `internal/cancellation/`, `internal/reconcile/` and
  `internal/sweeploop/` around it. Workflow code has different rules from
  everything else; see the versioning invariant below.

One binary, four modes: serve (no argument), `worker`, `migrate`, `seed`. The
worker serves probes only — no HTTP API, no gRPC server.

## Invariants

Rules an implementer can violate at the keyboard. Several of them are the reason
an order cannot be charged twice.

- **The status FSM is a closed table.** A transition absent from it does not
  exist. Three edges look wrong until you read why they are there: completed →
  cancelling exists because otherwise the cancellation window would be a few
  seconds wide; manual_review → completed exists so an operator untangling a
  failed cancellation can put an order back exactly where it was; confirmed →
  manual_review is the ambiguous-pivot seam, where a commit succeeded but its
  acknowledgement was lost and parking it for a human is the only honest answer.
- **Actor class is a second FSM dimension, enforced at the write.** A user may
  only reach cancelling; only an operator leaves manual_review; the system only
  moves pending to failed. It is checked under the same lock as the transition,
  so a handler bug that routes a user request into an operator-only edge is
  caught by the database, not by convention.
- **There is exactly one status writer**, and it takes a row lock before a
  version-and-status-guarded update. The lock, not the version predicate, is what
  lets workflow activities, the user cancel path and operator resolves interleave
  without an application retry loop. Under that lock, an unexpected row count is
  a broken invariant rather than a lost race — do not "handle" it by retrying.
- **`updated_at` is written unconditionally**, because the reconciler's scan
  window keys on it. A terminal transition that skipped it would silently hide
  the order from settlement.
- **Idempotency is anchored in the schema, and the saga kickoff is status-gated.**
  A replayed key is only allowed to start the saga while the order is still
  pending: a key replayed after Temporal's retention has expired must never
  re-run the saga on a confirmed order, because that re-charges and re-ships.
- **Command ids carry a server-read epoch**, never a client-supplied one. The
  path through manual_review back to cancelling can legally happen more than
  once, so a version-free id would make the second episode replay the first one's
  outcome instead of transitioning. A stale epoch from an old browser tab is
  exactly the input this defends against.
- **An unservable participant token panics the workflow task rather than
  guessing.** Either guess releases stock one service never reserved and orphans
  the hold in the other. A panic fails the workflow *task*, not the workflow, so
  the saga stalls loudly until a build that understands the token serves the
  queue — nothing is lost and nothing is corrupted.
- **Worker deployment versioning: changing workflow code requires a new build
  id.** Both workflows are pinned explicitly rather than inheriting a default,
  because this saga holds money and stock and a workflow must never be moved onto
  a new build mid-flight. Setting the environment variable only makes a worker
  *poll* as versioned — the deployment's current version must be set server-side
  in the same operation, or new workflows target unversioned workers and stall
  silently. The recorded histories under `internal/saga/testdata/` are the
  regression guard; if the replay test fails, the change is not safe to ship as a
  patch.
- **The pivot is order confirmation.** Before it, failures compensate; after it,
  nothing rolls back and the tail is best-effort. `failed` is never used while
  side effects are unaccounted for — that is what manual_review exists for.
- **The inventory commit retries unlimited attempts but bounded elapsed time.**
  The elapsed bound is the load-bearing half: with unlimited attempts alone, a
  deterministic bug in commit parks the workflow forever and the breach report —
  metric and log — is never reached. Compensations get a harder policy, because
  one that gives up leaves money held or stock reserved with nothing to drive it.
- **Money is integer minor units end to end.** Dollars exist only in the HTTP
  response adapter.
- **Metric labels are bounded domain values** — no order or user ids, no payment
  tokens, no decline text, no amounts. Workflow-emitted counters are guarded
  against replay so an ordinary history replay after a worker restart does not
  re-count. The order-value histogram is deliberately label-free: the amount rides
  in the value, and it is recorded once per genuine creation, never on a replay.
- **The reconciler drains in two phases.** Skipping straight to cancel would abort
  an in-flight commit or release, which shows up as a repair failure on *every*
  worker restart and poisons the signal a deploy should leave untouched.
- **Some loops must run in exactly one process.** The reconciler has no locking
  claim, so only one build may have it enabled. Its disabled state is logged at
  warn rather than info on purpose — a silent off switch on a settlement loop is
  not an informational event.
- **Pooler-safe database settings live in `pkg/dbx`.** One DSN serves the app and
  migrations so both connect identically.
- **Graceful-shutdown ordering is load-bearing:** readiness 503 → drain delay →
  HTTP → gRPC `GracefulStop` → pool close → OTel last.
- **Probe suppression is one contract across logs and traces**, through the same
  skip list; a **failing** probe is still recorded.

## Repository map

- `cmd/main.go` — wiring, the four modes, background loops, graceful shutdown
- `config/config.go` — env config and validation
- `internal/web/v1/` — HTTP handlers, the details aggregation, and the cart, shipping, payment and inventory client adapters
- `internal/grpc/v1/` — the `OrderService` creation RPC
- `internal/logic/v1/` — business rules and metrics
- `internal/core/domain/` — models, the status FSM and its actor rules, outbox and projection types
- `internal/core/repository/` — Postgres repositories, including the single status writer
- `internal/saga/` — workflows, activities, projection, metrics, and recorded replay histories in `testdata/`
- `internal/fulfillment/`, `internal/cancellation/` — outbox dispatchers
- `internal/reconcile/` — the settlement reconciler
- `internal/sweeploop/` — the shared ticker both dispatchers use
- `db/migrations/`, `db/seed/` — embedded SQL
- `middleware/` — tracing and logging only

## Gotchas

- Kyverno admission rejects a workload image tagged `:latest` or unpinned. The
  published image is `ghcr.io/duynhlab/order-service/order-service:<tag>` — the
  repository path repeats, and the tag carries no `v` prefix. The worker is the
  same image under the `worker` argument, deployed once per build id.
- Metrics leave over OTLP. There is no `/metrics` endpoint and nothing scrapes
  this service.
- The one surviving REST call to another service is the **tokenless** internal
  cart clear, deliberately tokenless so no bearer token lands in workflow
  history.
- `user_id` is the OIDC token subject: an opaque string, never an integer
  (ADR-042). The gRPC guard bounds its length; the saga passes it verbatim into
  notify and authorize. `isInt32` survives for product ids only — do not
  re-point it at a subject, and do not parse one anywhere.
- Dial failure behaviour differs by mode: on the serve path some clients
  soft-fail and the enrichment degrades; on the worker path every dependency is
  fatal, because a saga cannot compensate with a client it does not have.

## API change synchronization

An API change is not done when the code compiles.

- The contract in homelab and this repository move **together** — same change,
  and either the same PR pair or an immediate follow-up.
- Behaviour that is designed but not deployed is marked **`Planned`** in the
  contract; it is never described as current.
- A material mismatch between the contract and this implementation **blocks the
  release tag** until it is reconciled or explicitly accepted.
