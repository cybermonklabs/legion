# Envoy

The Go module `github.com/sjawhar/envoy`: Legion's event transport (webhooks in, NATS JetStream routing, delivery to agent sessions), the Dispatch server and the secrets broker.
It holds no Legion workflow policy: the daemon decides what to do, and Envoy moves events to the right session.
Start with `docs/architecture.md`; `.dispatch/architecture/` holds a note per component.
Paths are from `packages/envoy`, except those under `.github/`, `.dispatch/` and `docs/site/`, which are from the repository root.

## Components

- `cmd/listener` with `internal/{webhook,bus,store,session,kvwatch,routing}`: the per-machine listener, its webhooks, its `/v1` API (`docs/architecture.md`) and each session's delivery; its settings are in `deploy/README.md`.

- `cmd/dispatch` with `internal/dispatch`: the Dispatch server, on Postgres with pgvector; read `cmd/dispatch/README.md` and `cmd/dispatch/AGENTS.md` first.

- `internal/contracts`: envelopes and NATS subject names; subscribers' topics and payloads are in `docs/site/src/content/docs/dispatch/envoy.md`.
  `listenerwire` holds the bodies of the listener's `/v1` API, which every Go client of the listener uses.

- `internal/cistore`: each pull-request head's CI record, whose settlement publishes `notifications.github.<owner>.<repo>.pr.<n>.checks`.
  Commit statuses reach it only through Dispatch's CI reconcile (`cmd/dispatch/README.md`, "CI reconcile"); the listener publishes no `status` webhook event.

- `cmd/natstail` prints the envelopes on a NATS subject; `cmd/mcp` is the MCP bridge; `cmd/spec-check` and `cmd/architecture-check` judge a checkout's `.dispatch/` as Dispatch's importer does.

- `deploy/`, `loadtest/` and `scripts/` each have a README; `docker/Dockerfile` builds the one image; `infra/` is a Go module of its own, with its own guide.

## Commands (from `packages/envoy`)

- Build `go build ./cmd/dispatch ./cmd/listener`; test `go test ./...`, one test `go test -count=1 -run '^TestName$' ./internal/bus/`; format `gofmt -l .`; vet `go vet ./...`.

- Set `DISPATCH_TEST_DATABASE_URL` and `BROKER_TEST_DATABASE_URL` (`scripts/dev-postgres.sh` starts a pgvector Postgres and prints its URL): without them most Postgres-backed tests skip, and `go test` still prints `ok`.

- Build tags hold the slow suites (`natslive` needs Docker; `multiprocess`, `twobuild`, `memory`, `smoke`); `.github/workflows/ci-job-envoy-*.yaml` show how CI runs each.

- `scripts/e2e-local.sh` runs the listener end to end with no container runtime.

- `internal/dispatch/pmdoc` tests compare with the browser editor through bun scripts in `pmdoc/gen`: they need `bun` and a `bun install` at the repository root, and without `bun` they skip here and fail in CI.

- `infra/` imports packages of this module (`internal/bus` and `internal/contracts` among them), and `./...` here does not build `infra/`: after a change to one of them, run `go vet ./...` in `infra/` too.

## Generated files

- `internal/contracts/*generated.go`, `listenerwire/envelope_generated.go` and `internal/dispatch/pmdoc/schema_generated.go` come from `packages/contracts`: edit the source there and run `bun run gen:go`.

- `packages/contracts/schemas/dispatch-openapi.json` comes from Dispatch's route table: `go run ./cmd/dispatch openapi --out ../contracts/schemas/dispatch-openapi.json`, then `bun run gen:api` in `packages/contracts`.

## Traps

- Issue, agent and role subjects have an old and a project form (`notifications.agent.<session>`, `notifications.agent.<PROJECT>.<session>`): compare a topic through `contracts.TopicForms`, never as one string.

- In Dispatch, call the listener before a transaction opens, never inside one (`internal/dispatch/api/envoy_resolve.go`).

- A transaction that appends events for more than one owner calls `events.Broker.LockOwners` on all of them before its first `Append`, or two writers can deadlock.

- A Dispatch route whose answer holds a credential goes in `answerHoldsSecret` (`internal/dispatch/api/request_keys.go`), or the keyed-write layer stores that answer for 24 hours.

- Dispatch and the broker migrate at boot while the previous release still serves the same database: a migration that drops, renames or narrows waits a release (`cmd/dispatch/README.md`, "Database migrations").

## Session consumers

- A session receives only from its own durable consumer on `ENVOY_NOTIFICATIONS`, which the listener holding its registration relays in stream order and acknowledges once the session wrote the message; there is no plain NATS subscription and no fallback path.

## Secrets broker

- `cmd/broker` and `internal/broker` are the broker, and `cmd/agent-secrets` and `cmd/agent-secrets-helper` its clients; read `cmd/broker/README.md`, `cmd/agent-secrets/README.md` and `docs/site/src/content/docs/broker/`.

- `cmd/broker-refgen` writes the broker's reference pages from doc comments and refuses a `BROKER_*` setting, route or refusal code without one; it runs in the docs build (`bun run generate` in `docs/site`), not in `go test`.

- `go test` skips `FuzzAirlock` unless `AIRLOCK_MODEL` names the model that `lake build` writes in `internal/broker/airlock/lean`.

## Operational notes

- The listener refuses to bind its durable `listener-<machine id>` when the durable's idle heartbeat or ack policy differs from the listener's.
  Deleting the durable replays all 72 hours the stream retains; to keep its cursor, recreate it at one past its ack floor while no listener runs for that machine:
  ```bash
  d=listener-<machine id>
  nats consumer info ENVOY_NOTIFICATIONS "$d" --json > "$d.json"
  jq '(.ack_floor.stream_seq + 1) as $next | .config | del(.idle_heartbeat, .flow_control) | .ack_policy = "explicit" | .deliver_policy = "by_start_sequence" | .opt_start_seq = $next' "$d.json" > "$d.recreate.json"
  nats consumer rm ENVOY_NOTIFICATIONS "$d" -f
  nats consumer add ENVOY_NOTIFICATIONS --config "$d.recreate.json"
  ```

- Remove the listener's shared token and its open `/v1` only after `credential="shared_token"` and `credential="none"` of `envoy_listener_requests_total` read zero on every deployed listener for 72 hours.
