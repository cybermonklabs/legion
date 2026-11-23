# Envoy for the Codex CLI

`@sjawhar/codex-envoy` is Legion's Envoy and Dispatch plugin for the Codex CLI.
A Codex session that loads it registers with Envoy under its own session id and shows on Dispatch's
Agents page with a live view of its conversation.
Messages sent to it from the Agents page (Aside, BTW and Steer) and messages on Dispatch issues
routed to it arrive in its conversation, and its `dispatch` tool runs the `dispatch` command as
the session.
It ships the Dispatch skills.

That holds for a session in a `codex` process of its own, such as the terminal on Codex 0.154.0
or `codex exec`.
Codex's shared app server runs several sessions in one process (`codex agents`, a plain `codex`
that joins a running one, and on Codex 0.170.0 every terminal session), and there the plugin
registers no session with Envoy: such a session gets no Envoy messages and does not show on the
Agents page, though its `dispatch` tool and its Dispatch hooks still work.

```bash
codex plugin marketplace add cybermonklabs/legion
codex plugin add codex-envoy@legion-plugins
```

The package's `bin/dispatch` is the `dispatch` command for a terminal, and its npm `bin`. The
plugin's `dispatch` tool ends a command after 120 s, so a command that waits for a person runs
there, such as the device login of a machine with no identity of its own:

```bash
"${CODEX_HOME:-$HOME/.codex}"/plugins/cache/legion-plugins/codex-envoy/<version>/bin/dispatch login --device --person <address>
npx --package @sjawhar/codex-envoy dispatch login --device --person <address>
```

It is built from the same source as the Claude Code plugin, `@sjawhar/claude-envoy`, whose README
describes both hosts: https://github.com/cybermonklabs/legion/tree/main/packages/claude-envoy
