import { expect, test } from "bun:test"
import { commandTimeoutMs } from "@legion/envoy-client/envoy-config"
import hooks from "../hooks/hooks.json" with { type: "json" }
import { JJ_BASE_CHAIN_TIMEOUT_MS, OPEN_ASKS_TIMEOUT_MS } from "../hooks/open-asks-bounds"

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

/** The room the open-asks hook keeps beside its bounded steps: Bun's start-up of the bundle, the
 *  session handoff and title writes, and resolving the config file, none of which has a bound of
 *  its own. */
const START_UP_ALLOWANCE_MS = 2_000

// The open-asks hook's bounded steps run one after another (hooks/session-hook.ts `openAsks`): jj's
// own user config chain for the attribution overlay (`JJ_BASE_CHAIN_TIMEOUT_MS`), the `!` token
// command that resolving Dispatch may run (envoy-client's `commandTimeoutMs`), and the Dispatch
// read of the session's open asks (`OPEN_ASKS_TIMEOUT_MS`). Claude Code kills the hook at its
// hooks.json timeout and the summary is lost, so the three, with the start-up allowance, end
// inside it; a failure prints each step, so it names the one that grew. A row that runs the hook
// cannot show this by a clock: a loaded machine starting the hook slowly stretches the clock,
// and a hook given a raised bound still returns inside that row's own timeout.
test("the open-asks hook's jj, token command and Dispatch read bounds, one after another, end inside its hooks.json timeout", () => {
  const timeouts = Object.values(hooks.hooks).flatMap((matchers) =>
    matchers.flatMap((matcher) =>
      matcher.hooks.filter((hook) => hook.args.includes("open-asks")).map((hook) => hook.timeout),
    ),
  )
  expect(timeouts).toEqual([20])
  const [timeoutSeconds = 0] = timeouts
  const budget = {
    jj: JJ_BASE_CHAIN_TIMEOUT_MS,
    tokenCommand: commandTimeoutMs({}),
    dispatchRead: OPEN_ASKS_TIMEOUT_MS,
    startUp: START_UP_ALLOWANCE_MS,
  }
  const spent = budget.jj + budget.tokenCommand + budget.dispatchRead + budget.startUp
  const timeout = timeoutSeconds * 1_000
  expect({ ...budget, spent, timeout, fits: spent <= timeout }).toEqual({
    ...budget,
    spent,
    timeout,
    fits: true,
  })
})
