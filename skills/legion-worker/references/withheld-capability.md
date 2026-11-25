# What the withheld capability is, and what it is not

## The test: where it lands, and what it leaves resolving

An install of your own (an interpreter, a browser, a second toolchain, an extraction) is sanctioned only when both hold:

1. It lands on the issue's volume under `/legion`, outside `$LEGION_WORKSPACE`, `/legion/repos` and `/legion/feed`: under a variable the runtime sets there (`XDG_CACHE_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, `TMPDIR`, `GOCACHE`, `GOMODCACHE`, `BUN_INSTALL_CACHE_DIR`, `UV_CACHE_DIR`, `UV_PYTHON_INSTALL_DIR`), or a path your harness names there. `$HOME` and `/tmp` are on the pod's disk, whose limit evicts the pod. The workspace is excluded because jj snapshots new files there into the change every role shares.
2. Once the command exits, nothing else resolves differently. Export a toolchain's `PATH` or `LD_LIBRARY_PATH` only around your harness's own commands, never into the shell you go on working in or a subagent's environment.

- A repository's own dependency install (`uv sync`, `bun install`) into its ignored directories is sanctioned.
- A pinned runner line pins the named package alone; its dependencies are resolved when it runs. When the work needs the whole dependency closure pinned (a comparison that has to hold across days or across pods), use a project's own lockfile with `uv run` or `bun install`, not a one-off runner line.
- Playwright keeps its browsers under `XDG_CACHE_HOME`, on the issue's volume, where the issue's roles share one registry under an install lock (`docs/kubernetes.md`, "Anatomy of a Sandbox pod"). Pointing `PLAYWRIGHT_BROWSERS_PATH` elsewhere loses that shared registry and the lock that makes a second role's install wait rather than race. A tool whose cache the runtime does not place, such as Puppeteer, needs its own variable (`PUPPETEER_CACHE_DIR`) pointed under one of the paths above.

## The Dispatch browser suite

A spec you add or change in `packages/dispatch` runs in the pod one spec a command (`bun run e2e -- e2e/<spec file>`), against the pod's own Postgres (*The database your tests use* in `skill://legion-worker/references/tools.md`). The change's proof is CI at the head: a pull request runs the rows tagged `@pr` in the `dispatch-browser-pr` checks, and every other row runs in the `dispatch-browser` checks, which the merge queue runs and a pull request runs only under the label `full-ci` or `lane:dispatch_browser`.
