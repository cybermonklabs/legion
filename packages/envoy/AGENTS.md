# Envoy

The Go module `github.com/sjawhar/envoy`:
Legion's event transport (webhooks in, NATS JetStream routing, delivery to agent sessions), the Dispatch server and the secrets broker.
It holds no Legion workflow policy: the daemon decides what to do, and Envoy moves events to the right session.
Start with `docs/architecture.md`; `.dispatch/architecture/` at the repository root holds a note per component, and `README.md` has the commands.

## Components

- `cmd/listener` with `internal/{webhook,bus,store,session,kvwatch,routing}`:
  the per-machine listener, its webhooks, its `/v1` API and each session's delivery; `deploy/README.md` has its settings and operations.

- `cmd/dispatch` with `internal/dispatch`: the Dispatch server, on Postgres with pgvector; read `cmd/dispatch/README.md` and `cmd/dispatch/AGENTS.md` first.

- `cmd/cipherd` and `internal/cipher`: the secrets broker, with its clients `cmd/cipher` and `cmd/cipher-helper`;
  `cmd/cipherd/README.md`, `cmd/cipher/README.md` and `docs/site/src/content/docs/broker/` at the repository root describe them.

- `internal/contracts`: envelopes and NATS subject names, whose topics and payloads for subscribers are in `docs/site/src/content/docs/dispatch/envoy.md` at the repository root;
  `listenerwire` holds the bodies of the listener's `/v1` API, which every Go client of the listener uses.

- `internal/cistore`: each pull-request head's CI record, whose settlement publishes the pull request's `checks` event.

- `cmd/natstail` prints the envelopes on a NATS subject;
  `cmd/mcp` is the MCP bridge; `cmd/spec-check` and `cmd/architecture-check` judge a checkout's `.dispatch/` as Dispatch's importer does;
  `cmd/cipher-refgen` writes the broker's reference pages from doc comments.

- `deploy/`, `loadtest/` and `scripts/` each have a README; `docker/Dockerfile` builds the one image; `infra/` is a Go module of its own, with its own guide.

## Rules

- Never edit a generated Go file (`*generated.go`): change its source in `packages/contracts` and regenerate it there.

- `infra/` imports packages of this module (`internal/bus` and `internal/contracts` among them) but builds apart from it:
  check a change to one of them in `infra/` too.

- A caller identity provider is one package `internal/cipher/identity/<kind>` (its verifier, token types, decision, principals, subjects and the parsing of its own `BROKER_*` variables, and a `contract_test.go` calling `identitytest.Contract`) and one file behind `!cipher_no_<kind>` in `internal/cipher/identity/boot`.

- Compare a topic through `contracts.TopicForms`, never as one string: issue, agent and role subjects have an old and a project form.

- In Dispatch, call the listener before a transaction opens, never inside one (`internal/dispatch/api/envoy_resolve.go`).

- A transaction that appends events for more than one owner calls `events.Broker.LockOwners` on all of them before its first `Append`, or two writers can deadlock.

- A Dispatch route whose answer holds a credential goes in `answerHoldsSecret` (`internal/dispatch/api/request_keys.go`), or the keyed-write layer stores that answer for 24 hours.

- Dispatch and the broker migrate at boot while the previous release still serves the same database:
  a migration that drops, renames or narrows waits a release (`cmd/dispatch/README.md`, "Database migrations").

- Deliver to a session only through its own durable consumer on `ENVOY_NOTIFICATIONS`: add no plain NATS subscription and no fallback path.
