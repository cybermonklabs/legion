import { expect, test } from "bun:test"
import { COMMAND_TIMEOUT_MS } from "@legion/envoy-client/envoy-config"
import hooks from "../hooks/hooks.json" with { type: "json" }
import { JJ_BASE_CHAIN_TIMEOUT_MS, OPEN_ASKS_TIMEOUT_MS } from "../hooks/session-hook-budgets"

// The open-asks and dispatch-first hooks resolve Dispatch before they write anything, and a `!`
// dispatch.token runs for up to 10 s (envoy-client's COMMAND_TIMEOUT_MS) in that hook's own
// process, after Bun has started. Claude Code's `timeout` is in seconds; a hook it kills first loses
// the open-asks summary or the dispatch-first context silently, so each of those outlasts the
// command with room for start-up. The answers hook prints and exits within its own bound, under
// 1 s from its start (`ANSWERS_BY_MS`, then `ANSWERS_SETTLE_MS`, in hooks/session-hook.ts: a token
// command runs in a detached refresher process it stops waiting for), so it gets the short bound,
// room for a slow start-up only. The session-title hook resolves nothing and makes no network call
// while Claude Code blocks the prompt on it, so it gets the short bound too, and a bundle of its
// own: the arguments are pinned whole here, since which file each hook runs is half of what this
// row guards.
test("each hook runs its own bundle, with the timeout its work needs: 20 s for the hooks that run a token command, 5 s for answers and session-title", () => {
  const commands = Object.values(hooks.hooks).flatMap((matchers) =>
    matchers.flatMap((matcher) => matcher.hooks.map((hook) => [hook.args, hook.timeout])),
  )
  const bundle = (name: string) => `\${CLAUDE_PLUGIN_ROOT}/dist/${name}.js`
  expect(commands).toEqual([
    [[bundle("session-hook"), "open-asks"], 20],
    [[bundle("session-hook"), "dispatch-first"], 20],
    [[bundle("session-hook"), "answers"], 5],
    [[bundle("session-hook"), "dispatch-first"], 20],
    [[bundle("session-hook"), "answers"], 5],
    [[bundle("session-title-hook")], 5],
  ])
})

// The open-asks hook's three waits run in series on its slow path with a `!` token credential:
// the jj chain for the overlay, then the session's token command, which `sessionDispatchSettings`
// runs, then the Dispatch read for the summary (`openAsks` in hooks/session-hook.ts). Claude Code
// kills the hook at the timeout the row above pins, and a killed hook loses the overlay's export
// and the summary, so a raise of any one of the three without the others has to fail here and not
// in a session. The sum must also leave room for Bun's own start-up inside the timeout, and this
// row does not check that room: `toBeLessThan` passes a sum 1 ms under the timeout. Today the three
// sum to 16 s against 20 s, which leaves 4 s. A broker credential's route is not this sum
// (hooks/session-hook-budgets.ts says why).
test("the open-asks hook's jj chain, token command and Dispatch read fit inside the timeout hooks.json gives it", () => {
  const openAsks = hooks.hooks.SessionStart.flatMap((matcher) => matcher.hooks).find((hook) =>
    hook.args.includes("open-asks"),
  )
  expect(openAsks?.timeout).toBeDefined()
  expect(JJ_BASE_CHAIN_TIMEOUT_MS + COMMAND_TIMEOUT_MS + OPEN_ASKS_TIMEOUT_MS).toBeLessThan(
    (openAsks?.timeout ?? 0) * 1_000,
  )
})
