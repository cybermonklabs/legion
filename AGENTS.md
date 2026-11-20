# Legion

Legion runs Oh My Pi coding agents on issues tracked in Dispatch and brings each one back as a pull request.
The Go daemon (`packages/daemon`) admits issues and runs an architect for each issue tree and the phase workers (planner, implementer, tester, reviewer, merger) in tmux panes or in one Agent Sandbox pod per issue, on Oh My Pi or, where `worker_backends` says so, on Claude Code.
Issues live in Dispatch, never in GitHub issues; Dispatch's server and Envoy, the event bus over NATS JetStream, are in `packages/envoy`.
`README.md` describes each part.

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

- `packages/daemon/`: the daemon, the `legion` CLI (cobra commands that `newRoot` in `cmd/legion/cli.go` registers), the role prompts it embeds (`internal/prompts/roles/`) and the worker image (`docker/`); `docs/deployment.md` and `docs/kubernetes.md` say how it runs.

- `packages/envoy/` (guide): the Envoy listener, Dispatch's server and the secrets broker, in Go; `cmd/dispatch/` has a guide, and so does `infra/`, the Pulumi components, a module outside `go.work`.

- `packages/dispatch/` (guide): Dispatch's web app.

- `packages/proof-editor/` (guide): Dispatch's document editor.

- `packages/contracts/` (guide): the shared event contracts and Dispatch specs, and the Go generated from them.

- `packages/pi-envoy/` (guide): the Oh My Pi plugin every session loads, with the Envoy tools and the `dispatch` command.

- `packages/pi-legion/` (guide): the Oh My Pi plugin a Legion pane loads beside it, with the Legion tool and the agents Legion's workers run.

- `packages/pi-shared/` (guide): the private package both plugins bundle.

- `packages/claude-envoy/` (guide): the Claude Code plugin, and the Codex CLI plugin `@sjawhar/codex-envoy` built from the same source.

- `skills/` (guide): the skills Legion's agents load.

- `scripts/e2e/`: the end-to-end tests; `scripts/e2e/README.md` says how each one runs.

- `.github/workflows/ci.yaml`: the CI router; its header lists the eight places a new job joins it.

## Commands

```sh
bun install                                    # every workspace
bunx biome check .                             # the lint CI requires; or <package>/
(cd packages/<package> && bun run typecheck)   # the root has no tsconfig.json
(cd packages/<package> && bun run test)        # the root has no test script
(cd packages/<package> && bun test <file> -t '<test name>')
bun test scripts/<file or directory>           # scripts/ is no workspace
bunx tsc --noEmit -p scripts/dispatch          # likewise scripts/github-apps, scripts/sessions
go -C packages/daemon test ./...               # likewise packages/envoy
go -C packages/daemon test ./internal/api -run '^TestName$'
go -C packages/daemon vet -tags e2e ./...
gofmt -l packages/daemon packages/envoy
(cd packages/daemon && go build ./cmd/legion)  # then ./legion <command> --help
bun run dispatch:local                         # Dispatch, seeded and signed in
bun run docs:dev                               # the documentation site
```

A suite that needs a program or a service says so in the line it prints when it skips; a Postgres suite reads `LEGION_TEST_PG_DSN`.
The real Oh My Pi and Claude Code suites skip without a word unless `LEGION_TEST_OMP` with `LEGION_TEST_NATS_URL`, or `LEGION_TEST_CLAUDE`, is set; on GitHub Actions they fail instead.
`docs/testing.md` holds the rules a test must follow that the code does not show.

## Pull requests and merging

- `main` takes squash merges through GitHub's merge queue, and the pull request's title becomes the commit's subject, so write it as a conventional commit subject.

- `ci.yaml` holds back on a pull request each job whose `if:` tests a `changes` output that carries the `full-ci` term, and runs it in the merge queue.
  The label `full-ci` runs them on the pull request, and puts the browser suite on its full lane.

- Add `full-ci` to check a fix for a slow-suite failure that took the pull request out of the queue, when a check of the issue names a held-back job as its `ci_check`, when a person asks, or, before the approval, when the change touches `packages/envoy/internal/dispatch/store/migrations/**` or `packages/envoy/internal/pgmigrate/**`.
  The label takes effect at the next push.
  Take it off once that run passes, unless a `ci_check` names a held-back job, which reads `skipped` without it.

- A change worth a changelog line adds a fragment to each package it changes that `.github/changelog-packages` lists.
  Open the pull request first, since the fragment carries its number, then run `go run github.com/miniscruff/changie@v1.26.0 new --kind <Added|Changed|Removed|Fixed> --body '<text>' --custom Issue=pr<number>` in the package.
  Two such runs for one package and kind in the same second write one file, and the first entry is lost.
  Never edit a `CHANGELOG.md`, and never run `changie init`, `batch` or `merge`: a release renders its fragments into its GitHub release notes.

- A change to the daemon API (`packages/daemon/internal/api`, mirrored in `packages/contracts/src/legion-api.ts`) raises `DaemonAPIVersion` in `internal/api/version.go` above its base's.
  `go -C packages/daemon test ./internal/api -run '^(TestDaemonAPIVersionGolden|TestStateGolden)$' -update` rewrites the two fixtures, and `.github/scripts/check-daemon-api-contract.sh` names a free number and each file that must agree.

- After a merge of `main` that touched `bun.lock`, take one side whole (`git checkout origin/main -- bun.lock`), then run `bun install`.
  Commit what it writes only when the difference holds just the lines the change explains, `lockfileVersion` has not moved, and no entry names a registry other than npm's, which a configured mirror writes into every entry.

- CI pulls each image by digest from `ghcr.io/cybermonklabs/legion/ci` (`.github/ci-images.yaml`) and each tool through a row of `.github/ci-tools.yaml`; the deployment examples pull through `mirror.gcr.io`.

- Moving a copied image or Bun to a new version takes two commits: first the new row, then the pins (`.bun-version`, `ARG BUN_VERSION`, each `FROM`, the `bun-*` tool rows) with the old row deleted.
  In one commit, a job can pull a digest the copy has not landed yet.

- An image this repository builds (a `built:` row) takes two pushes: the push that changes its recipe builds it, and a later push moves the row's `digest:` onto it.

- Deploys come only from main: a fix a deployment needs merges to `main` and ships in the next release, and no long-lived integration or deployment branch is kept.

- If a daemon built before migration `0058_catch_up_cursor` ran after a later one, set every catch-up cursor to null (`update issues set caught_up_seq = null`) before a later daemon starts again, or its catch-up replays events the earlier daemon already applied.

## Conventions

- This repository is public: no name of the private infrastructure it deploys into (a hostname, tailnet machine, account id, IAM role, token audience, secret-store id, bucket or alert channel) goes in a file, a path, a commit or a pull request.
  Use placeholders (`<name>.internal.example`, `<name>.<tailnet>.ts.net`, `example/<service>/<name>`), and make a value a live script needs a required input.

- Name neither the repository Legion is deployed from, nor the company that runs it, nor its Dispatch keys: write "the deployment repository" and a neutral key such as `ACME`.
  Legion's own names and `LEGION-<n>` keys are fine.
  In a pod, the guard runs before the write: `git push`, `jj git push` and `legion gh` pass Legion's leak guard (`packages/daemon/internal/leakguard`) first.

- A skill, a role prompt and an `AGENTS.md` name no person: write "a person", "the operator" or "the human".
  A login or a path that holds a name is fine.

- Point to another guide by plain path (`packages/x/AGENTS.md`), never as `@packages/x/AGENTS.md`: Oh My Pi and Claude Code paste an `@path` into every session started here.

- Guides keep semantic line breaks: one sentence per line, and a blank line between list items.
  Add an item next to the one it belongs with, so two pull requests that edit one guide do not conflict.
  `bun scripts/semantic-line-breaks.ts [--write] <file>` checks a guide, or fixes it.

- Version control is jj (Jujutsu): `jj status`, `jj diff`, `jj git push`.

## Documentation

- `docs/site/src/content/docs/legion/concepts.md`: admission, the design gate, the merge and the production check.

- `STRATEGY.md`: what Legion and Dispatch are for.

- `docs/plans/` and `docs/solutions/` are history: no Legion role writes there, and a page marked `[HISTORICAL]` describes the code before the TypeScript rewrite.
