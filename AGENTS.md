# Legion

Legion runs Oh My Pi coding agents on issues tracked in Dispatch and brings each one back as a pull request.
The Go daemon (`packages/daemon`) admits issues and runs an architect for each issue tree and the phase workers (planner, implementer, tester, reviewer, merger), in tmux panes or in one Agent Sandbox pod per issue.
Issues live in Dispatch, never in GitHub issues; Dispatch's server and Envoy, the event bus over NATS JetStream, are in `packages/envoy`.
`README.md` describes each part and has the commands that build and test the repository.

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

## Layout

Each directory below that is marked "guide" has an `AGENTS.md` of its own.

- `packages/daemon/`: the daemon, the `legion` CLI, the role prompts it embeds (`internal/prompts/roles/` and `internal/prompts/go/`) and the worker image (`docker/`);
  `docs/deployment.md` and `docs/kubernetes.md` say how it runs.
  A new line of a role prompt comes with the case or the record that shows a role fails without it, and never as a second copy of a tool's description or refusal.

- `packages/envoy/` (guide): the Envoy listener, Dispatch's server and the secrets broker, in Go;
  `cmd/dispatch/` has a guide, and so does `infra/`, the Pulumi components.

- `packages/dispatch/` (guide): Dispatch's web app.

- `packages/proof-editor/` (guide): Dispatch's document editor.

- `packages/contracts/` (guide): the shared event contracts and Dispatch specs, and the Go generated from them.

- `packages/pi-envoy/` (guide): the Oh My Pi plugin every session loads, with the Envoy tools and the `dispatch` command.

- `packages/pi-legion/` (guide): the Oh My Pi plugin a Legion pane loads beside it, with the Legion tool and the agents Legion's workers run.

- `packages/pi-shared/` (guide): the private package both plugins bundle.

- `packages/claude-envoy/` (guide): the Claude Code plugin, and the Codex CLI plugin `@sjawhar/codex-envoy` built from the same source.

- `skills/` (guide): the skills Legion's agents load.

- `scripts/e2e/`: the end-to-end tests; `scripts/e2e/README.md` says how each one runs.

- `.github/workflows/ci.yaml`: the CI router; its header says how a job joins it.

## Pull requests and merging

- `CONTRIBUTING.md` holds the pull request process: the title, the CI labels, changelog fragments, the daemon API's version, Bun and the CI images and tools, `bun.lock` after a merge, and where deploys come from.

## Conventions

- This repository is public: no name of the private infrastructure it deploys into (a hostname, tailnet machine, account id, IAM role, token audience, secret-store id, bucket or alert channel) goes in a file, a path, a commit or a pull request.
  Use placeholders (`<name>.internal.example`, `<name>.<tailnet>.ts.net`, `example/<service>/<name>`), and make a value a live script needs a required input.

- Name neither the repository Legion is deployed from nor the company that runs it:
  write "the deployment repository".
  Legion's own names are fine.

- A skill, a role prompt and an `AGENTS.md` name no person: write "a person", "the operator" or "the human".
  A login or a path that holds a name is fine.

- Point to another guide by plain path (`packages/x/AGENTS.md`), never as `@packages/x/AGENTS.md`:
  Oh My Pi and Claude Code paste an `@path` into every session started here.

- Guides keep semantic line breaks: one sentence per line, and a blank line between list items; `scripts/semantic-line-breaks.ts` checks a guide and fixes it.
  Add an item next to the one it belongs with, so two pull requests that edit one guide do not conflict.

- Version control is jj (Jujutsu).

## Documentation

- `docs/testing.md`: the rules a test must follow that the code does not show.

- `docs/site/src/content/docs/legion/concepts.md`: admission, the design gate, the merge and the production check.

- `STRATEGY.md`: what Legion and Dispatch are for.

- `docs/plans/` and `docs/solutions/` are history:
  no Legion role writes there, and a page marked `[HISTORICAL]` describes the code before the TypeScript rewrite.
