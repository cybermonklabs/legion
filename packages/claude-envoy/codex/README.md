# Envoy for the Codex CLI

`@sjawhar/codex-envoy` is Legion's Envoy and Dispatch plugin for the Codex CLI.
A Codex session that loads it registers with Envoy under its own session id and shows on Dispatch's
Agents page with a live view of its conversation.
Messages sent to it from the Agents page (Aside, BTW and Steer) and messages on Dispatch issues
routed to it arrive in its conversation, and its `dispatch` tool runs the `dispatch` command as
the session.
It ships the Dispatch skills.

That holds for a session in a `codex` process of its own, such as the terminal on Codex 0.154.0
or `codex exec`, and for each session of Codex's shared app server, which runs several sessions in
one process, one per thread: plain `codex` on Codex 0.170.0 and 0.171.0, a second plain `codex`
that joins it, and `codex agents`.
There each thread registers under its own thread id once the person has trusted the plugin's hooks,
leaves the Agents page when Codex unloads the thread, and comes back when Codex loads it again.

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
