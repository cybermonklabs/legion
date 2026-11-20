# Claude Envoy

`@sjawhar/claude-envoy` is the Claude Code plugin for Envoy and Dispatch;
`codex/` and `src/codex/` are the Codex CLI plugin, `@sjawhar/codex-envoy`, built from this source.
`README.md` has the architecture, the bundle and release, the commands and the smokes.

## Components

- `src/envoy-channel-server.ts`: the MCP stdio server, the session's Envoy ingress, which serves the `envoy_*` tools.

- `hooks/mod.ts`: the mod, which writes into a terminal session; `hooks/mod-writer.ts` decides whether a message becomes a prompt or a row.

- `bin/dispatch`: puts the `dispatch` command on the Bash tool's `PATH`.

- `src/legion-*.ts`: the Legion phase worker, which the server runs in a pane `legion claude-bridge` launched.

- `scripts/build.ts`: builds the plugin's bundles.

- `tests/packed-skills.test.ts`: guards the twelve packed skills, including `spec-statement-review`.

## Rules

- Nothing commits `dist/` or `legion/`; the release builds them.

- A change that makes `scripts/build.ts` or `scripts/verify-build.sh` read a new file adds it to `scripts/build-inputs.txt` and to `$CLAUDE_ENVOY_VERSION_ARGS` in `.github/workflows/release.yaml`.

- Render inbound messages only through `renderInbound` (`@legion/envoy-client/delivery`).

- Subscribe the session's relay subject before any registration under its id, and read the id from the shared `SessionIdentity` at each use:
  a `/clear` moves the session to a new id.

- Hand the relay the nats.js `Msg` itself, never a copy: its `data`, `headers` and `reply` are prototype getters that an object spread drops.

- Every registry write goes through the session's one `serialized` chain, and nothing in it waits without a bound, since shutdown's deregistration queues behind it.

- Register the capabilities `sessionCapabilities` computes, never a constant, and read role records through `roleHeldBy` and `mayActOnRoleRecord` (`@legion/envoy-client/transport`), which pi-envoy shares.

- No Dispatch call goes through the MCP server, and no permission relay is carried: no reply path here authenticates its sender.

- Every message into a terminal session goes through the mod's `session.message`.

- The Legion hook entries go in `legion/hooks/hooks.json` (`LEGION_HOOKS` in `scripts/build.ts`), never `hooks/hooks.json`:
  a plain install has no `legion` server, so each would fail on every turn.
