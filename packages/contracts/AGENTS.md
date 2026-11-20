# Contracts

`@legion/contracts` holds the shapes Legion's TypeScript and Go code share:
the Envoy envelope and subjects, the Dispatch specs the `dispatch` command is built from, Dispatch's HTTP shapes, the Legion daemon's wire shapes, and the limits both languages enforce.
It has no release of its own; its consumers bundle it.
`README.md` has the GitHub and Slack subject hierarchy.

## Commands

Run each from this directory.

- `bun test`; one file: `bun test src/dispatch-tools.test.ts`.

- `bun run typecheck` and `bun run lint`.

- `bun run gen:go` rewrites the Go outputs below; `bun run check:go` names a stale one and writes nothing.

- `bun run gen:api` rewrites the HTTP types; `bun run check:api` compares them.

## Generated files

Change the source and regenerate; never edit these by hand.

- `scripts/gen-go.ts` writes, in `packages/envoy`, `listenerwire/envelope_generated.go` (the envelope), `internal/contracts/generated.go` (its aliases and every shared constant), `internal/contracts/dispatch_tools_generated.go` and `internal/dispatch/pmdoc/schema_generated.go`, and here `src/block-schema.generated.ts`.

- `schemas/dispatch-openapi.json` is reflected from the Go route table.
  In `packages/envoy`, `go run ./cmd/dispatch openapi --check` compares it and prints the command that rewrites it.

- `src/dispatch-api.generated.ts` is rendered from that document by `bun run gen:api`.

## Where things go

- A limit that both the server and a client enforce is one exported constant here, which `scripts/gen-go.ts` emits into Go as `contracts.<Name>`.
  Never write the number a second time.

- A Dispatch shape goes in the `src/dispatch-api-*.ts` module of its area.
  Callers import `src/dispatch-api.ts`, which re-exports every module, and never name a module by path.

- `src/legion-api.ts` mirrors the daemon's Go wire shapes by hand.
  In `packages/daemon`, `go test ./internal/api/ -update` and `go test ./internal/projection/ -update` rewrite the fixtures in `fixtures/daemon-api/`, and `src/legion-api.test.ts` then names each Go field that has no zod line.
  Import it as `@legion/contracts/legion-api`, never from the barrel, so a bundle that does not speak to the daemon does not carry it.

- A delivery mode goes into `DELIVERY_CAPABILITIES` before any server, adapter or UI accepts it.

## Traps

- A zod object drops the keys it does not declare.
  A new wire field that a nested `z.object` does not declare vanishes from every frame read through it.

- Dispatch, its clients and the plugins ship separately: prefer backward-compatible additions when you extend the envelope.
  A client reads an absent `service`, `session` or `project` in `WhoamiResponse` as a Dispatch older than that field, not as `null` (`src/dispatch-api-user.ts`).

- Build each Dispatch spec's fields through `zodSchemaApi(hostZod)`, and validate a call with `dispatchToolSchema(spec, zodSchemaApi(hostZod))`:
  the host's Zod can be Oh My Pi's `pi.zod` facade, which differs from zod v4 (`src/tool-schema.ts` says where).

- Each Dispatch spec's `example` is what `dispatch <command> --help` and every refusal print.
  Change it with the argument shape.

## Search limits

`dispatch search` sends its query and filters in the URL.
The load balancer in front of Dispatch answers a path and query over 16,369 bytes with a bare `414` before the server runs.
`SEARCH_QUERY_MAX` (1,000 UTF-16 units) and `PROJECT_KEY_PATTERN` (at most 10 characters) keep the largest call the tool accepts at a 9,069-byte path and query, since percent-encoding costs at most 9 bytes a unit.
Raising either, or adding a parameter, means redoing that sum.
