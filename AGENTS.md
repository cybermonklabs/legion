# Legion

## Design principles

Each line is a default and its reason.
Where two collide, decide the case and say why.
A measurement outranks any line here.

- Everything in Dispatch is a node in the graph, and a feature is a query over it; a feature that is hard to build over the model shows a defect in the model.

- One source of truth for each fact: a copy never overrules the system that writes the fact.

- The daemon is a state machine, not a gateway.

- Agents are engineers: they get the privileges their work needs.

- A new or changed requirement, or a weaker check, waits for a person's approval; no other fix needs one.

- Start with the standard answer, then make it agent-native.

- Fix a known mistake now; the status quo is no reason to keep it.

- Prefer events to polling, and instrument and log everything.

- Keep each bound you state: at the bound, refuse with the reason, and drop nothing in silence.

- Fix waste before you buy capacity, and do not design around a cap you have not hit.

- Testing means you boot the thing and run it; unit tests are scaffolding.

- Make invalid input impossible to represent.

- Change a contract so that the clients already deployed can still read it: no rollout updates both sides at once.

## Work, changes and releases

- Work is tracked in Dispatch: an agent opens no GitHub issue in this repository.

- The changes that need the owner's decision before an agent builds them are the "Boundaries" list of `STRATEGY.md`.

- `CONTRIBUTING.md` holds the pull request process: the title, the CI labels, changelog fragments, the daemon API's version, Bun and the CI images and tools, `bun.lock` after a merge, and where deploys come from.

- A patch in the root `patches/` is keyed to one exact version: when you bump a patched package, redo its patch.
  Bun installs a package whose patch names another version without the patch, prints only a warning and exits 0, even under `--frozen-lockfile`.

- Change nothing under the operator's `~/.omp` (its configuration and installed plugins) from pi-envoy's or pi-legion's code or tests: a test runs Oh My Pi under a HOME of its own (pi-shared's `omp-harness`), and installing a plugin there is a person's step.
  pi-envoy's `smoke-btw.sh` and `smoke-delivery.sh` are the exception: they run the installed `omp` under the caller's HOME, so their sessions are recorded there.

## This repository is public

- No name of the private infrastructure Legion is deployed into (a host, an account id, an IAM role, a token audience, a secret-store id, a bucket or an alert channel) goes in a file, a path, a commit or a pull request: write a placeholder, and make a value a live script needs a required input.

- Name neither the repository Legion is deployed from nor the company that runs it, in a file, a commit, a branch, or pull-request or review text: write "the deployment repository".
  `.github/scripts/check-private-names.sh` refuses both, but it reads pull-request text only after GitHub shows it.

- A skill, a role prompt and an `AGENTS.md` name no person: write "a person", "the operator" or "the human".
  A login or a path that holds a name is fine.

## Guides

- Before you change a file, read every `AGENTS.md` between the repository root and that file's directory.

- A guide holds only what an agent cannot learn from the code, would get wrong without it, and no check enforces: commands, decisions and traps.
  It holds no layout, component list or code path, and a rule about one symbol goes in that symbol's doc comment.

- Name a file in a guide by its plain path, never as `@path`: Oh My Pi and Claude Code paste an `@path`'s file into every session started here.

- Run `bun scripts/semantic-line-breaks.ts --write <file>` on each guide you change: CI checks the layout of only some of them.
  Add an item next to the one it belongs with, so two pull requests that edit one guide do not conflict.

## Tools and documents

- A `codegraph` tool, where the session has one (every Legion worker pod's does), answers callers, callees, impact and affected tests from an index of the checkout: use it before grep to find what calls or depends on a symbol and what a change touches, and read an empty or failed answer as "use grep", not as "nothing depends on it".

- `docs/testing.md` holds the rules a test must follow that the code does not show.

- `docs/plans/` and `docs/solutions/` are history: each page describes the code as it was when the page was written, and no Legion role writes there.

Where this file and the code disagree, the code is right: follow the code and correct this file.
