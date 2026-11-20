# Notes for the CI router and its filters

The derivations and the history of two files that point into this one:
`.github/ci-filters.yaml`, the path-filter data `.github/workflows/ci.yaml`'s `changes` job
selects jobs by, whose sections come first, and `.github/workflows/ci.yaml` itself, the router,
whose sections follow them. Each `# why:` line of either file names the section that holds the
derivation or the record its filter, its pattern, its step, its job or its key rests on, and
within each of the two the sections stand in the order their comment blocks stood. In ci.yaml a
comment block of at most two lines stays where it is, beside the pattern or the step it is
about; so does a value's own arithmetic, and a comment inside a `run:` body, which is shell and
stays with its command. `.github/ci-filters.yaml` holds patterns and names: beside its `# why:`
lines, its only comments are the lines of the blocks .github/scripts/check-ci-filters.sh and
.github/scripts/check-ts-filters.sh write. A comment of it that said which files a job reads
was deleted, since the filter beneath it is that record.
In the text moved here, "this file" and "this workflow" refer to `.github/workflows/ci.yaml`,
where every one of these comments stood before this change, the filters' comments among them.

## How a filter of the filters file is written

Each filter names every file its job reads: the package and what its tests, typecheck
and build read outside it; the composite actions the job installs its tools through;
for Go, the closure of the packages the job compiles (`go list -deps -test`), the
targets of the go commands its tests start, and the module files of the workspace the
build runs in, whose go.mod files move the versions it compiles; and this file, since a
job reads its own definition. The ci-filters job (.github/scripts/check-ci-filters.sh)
recomputes those sets and fails naming each file a filter misses.

dorny/paths-filter v4.0.3's `some-with-excludes` quantifier matches a change to a filter
when one of its patterns without `!` matches and none of its `!` patterns does.
worker_image's drop what the .dockerignore keeps out of the
worker image's build context. envoy_e2e_local and ci_images each end in '!**/*.md': no
step of either job reads a Markdown file, so a guide written under a directory pattern
of theirs (patches/**, packages/broker-client/**, .github/actions/install-crane/**)
starts neither. No Markdown file of the repository matches either filter today, so the
pattern holds no run back now; it keeps the next guide from riding in, which is what a
week of Markdown-only pull requests cost 1,504 runner-minutes (dispatch://INFRA-1413).
Every other filter of this file was tried with that pattern
and refused by rule 1 of .github/scripts/check-ci-filters.sh, which fails a filter that
stops matching a file its gate reads: those jobs read an embedded prompt, a README
packed into a plugin or an image, or a guide a test compares with code - go_notices
among them, whose own Markdown pattern in `.github/ci-filters.yaml` (`'!packages/envoy/internal/**/*.md'`) therefore names packages/envoy/internal
alone, the tree whose Markdown no release target's package directory holds.

The '*_test.go' patterns of the filters in `.github/ci-filters.yaml` are written by a command rather than by
hand: rule 17 of .github/scripts/check-ci-filters.sh computes, per gate filter, the
`!` patterns that keep it off every Go test file the job it gates does not compile,
and `.github/scripts/check-ci-filters.sh write` puts them in that filter's own block
between a BEGIN and an END comment. The ordinary run of that check refuses a block
that is not what it computes, naming the first pattern that differs, so an edit by
hand is lost. The check prints how many it wrote on every run, so no count is kept
here. That is dispatch://INFRA-1413's R18, the selection half of the rule whose
coverage half is rule 1: a job must not run for a change to a test file of a package
whose tests it never runs, and the pattern that selects such a file (packages/envoy/**
and its kin) matches the job's own sources too, so nothing but an exclusion can say it.
Which test files a job reads is `go list`'s answer for the packages its steps name in
test mode, under the tags and the -race each run carries, so a test it never compiles -
another module's, another package's, a tagged live one, a `//go:build race` one - is
excluded, and a filter whose job compiles every test file its patterns reach carries no
block at all (text_checks, and every filter naming no Go file). A block
belongs to the filter whose list holds it and never to a group an anchor shares, since
two filters that share an anchor need not run the same tests. worker_image is the one
filter keeping such a pattern by hand, because this check computes no read set for it:
its judge is the image check, .github/scripts/check-image-trigger-paths.sh, and the worker image's context holds
packages/daemon's test files while the .dockerignore drops packages/envoy's. A filter
without a `!` pattern means what it would under the default, `some` (any pattern
matches), where a `!` pattern would match every file outside it.
.github/scripts/check-image-trigger-paths.sh reads the filters
that gate an image build (worker_image, envoy_smoke) under this quantifier, and only as
flat lists of globs, so those two name no anchor standing for a list. The one anchor
they do name, ci_router, is on the glob itself, which leaves their lists flat.

The first keys are no job's gate: each is a group many filters share, written once and named by
every filter that needs it through its anchor, which dorny/paths-filter flattens into the list
that names it. The gate filters follow, one per `changes` output. ci_router carries two
anchors, one on each scalar, because the image check, .github/scripts/check-image-trigger-paths.sh, reads the two filters that gate an image
build as flat lists of globs: every filter that names ci.yaml names .github/ci-filters.yaml beside it.

A comment block of three lines or more that stood beside a filter is in
.github/ci-notes/ci.md, under the heading the `# why:` line in its place names.
A block that only listed which files a job reads was deleted, since the filter beneath it is
that record.

## Why the filters are one file, and the steps they stood in

Every path filter .github/workflows/ci.yaml's `changes` job selects jobs by, in one file.

That job's one dorny/paths-filter step hands .github/ci-filters.yaml's path over as its `filters` input, and
the action reads the file off the disk against GITHUB_WORKSPACE. It takes the input as a path
only when the text holds no newline and no colon (the action's `isPathInput`), so the value
there stays a plain scalar: a block scalar or a quoted path with a colon in it would be read as
inline YAML and the step would fail. On a pull request that job checks out .github/ci-filters.yaml and no
other, paths-filter reading the changed-file list from the API and the filters from the
tree; on a merge group it has the whole clone, which the git diff of base_sha..head_sha needs.

Why the data is a file and not the workflow's own text. Actions hands a step's input to the
action as one environment variable, INPUT_FILTERS, and Linux caps one such string at 131,072
bytes (MAX_ARG_STRLEN). The whole block in one step came to 137,498 bytes once rule 17's
generated `!` patterns were written into it, and the job then failed to start at all ("An
error occurred trying to start process ... Argument list too long", dispatch://INFRA-1413).
The filters were declared in three steps for that, each string its own YAML document, and a
rule of .github/scripts/check-ci-filters.sh refused a string that came near the cap again.
That rule is retired now that the steps are one: the path handed over is about thirty bytes,
and no such cap reaches a path. One document also means one set of anchors, so a group the steps' filters
shared is written once here, where each was written out again in every step that named it - a
YAML anchor reaches no further than the string it is in.

The text below is what the changes job's steps carried until the filters became one file, kept
as it stood, so its present tense describes that layout. The exceptions are the lines that
pointed at a comment this change removed, or at a line number that moved, which now name what
they meant. A line that names the second or the third step's comment keeps its words: that
comment is the paragraph of this block that begins "The second of the three filter strings" or
"The third of the filter strings".

The second of the three filter strings. Actions hands a step's input to the action as one
environment variable, and Linux caps one of those at MAX_ARG_STRLEN, 131,072 bytes: the
whole block in one step came to 137,498 bytes once the generated `!` patterns were
written into it, and the job could not start at all ("An error occurred trying to start
process ... Argument list too long", dispatch://INFRA-1413). So the filters are declared
in more than one step, each string its own YAML document; the third step's comment says
why there are three. Which filter is in which step is
grouping, not meaning: the split keeps every filter that shares a group with another in
the step that holds the group, so only the seven cheapest groups are written twice, and
rule 18 of .github/scripts/check-ci-filters.sh refuses a string that comes near the cap
again. dispatch_browser and envoy_infra_go moved here from the first step when main's
own test files grew that string past the margin: they share the Dispatch server with
three filters that stayed, which is the seventh group, and the first step's remaining
filters are the ones whose generated blocks grow with every test file packages/daemon
gains. contracts went the other way in the same pass, to leave the two strings the same
distance from the margin: it carries no generated block, so the bytes it moves are bytes
that stay put. Each output above names the step its filter is in, which rule 19 of that
check holds it to.

The third of the filter strings, for the reason the second's comment gives: the second
came within rule 18's margin again once envoy_go_race carried a generated block of its
own, when search_text_test.go's read narrowed from packages/envoy/** to the Markdown it
opens, and the first had no room for a filter either (dispatch://INFRA-1413). The three
race lane filters are here, and so is envoy_client, which the first step had no room for
once main's new test files grew its generated blocks, and so is private_names_port, whose
generated block names every migration test of the daemon's store and grows with each one
main adds, and so are envoy_go and envoy_go_checks, its fast half, which the second step had
no room for once envoy_go's filter took the two contract paths the API's window tests read,
which its step runs plain, beside main's own growth of that string. They share no group with
a filter of the other steps but ci_router, go_workspace, bun_install and legion_command,
which this string writes again. Seven more filters moved here when main's growth and the
spec-check filters took the second string to 98,525 bytes, past the margin:
broker_multiprocess, airlock_lean, envoy_smoke, worker_image, ci_filters, tool_fallback and
ci_images, which need no group but ci_router and go_workspace. With them and envoy_go's two
filters here, each of the three strings has 25,000 bytes or more of room to rule 18's margin
again.

filter_verdicts above is one object over the three steps, the shape .github/scripts/ci-ok.jq
reads. A GitHub expression can build no object from three, so jq merges them here. A
name more than one step holds is a group written out again, which
.github/scripts/check-ci-filters.sh holds to the same patterns in each, so its verdict
is the same in each; rule 19 of that check refuses a filter an output of this job names
in two steps.

Each output names the step its filter is declared in. There are three of them because
Actions hands a step's input to the action as one environment variable and Linux caps
one of those at 131,072 bytes (the comments above the second and third steps say what
that cost here); rule 19 of .github/scripts/check-ci-filters.sh refuses an output naming
a step that does not declare its filter, so an output and its step cannot drift.

The groups the filters below share. Each is the first step's list pattern for
pattern, written again because a YAML anchor reaches no further than the string it
is in and each step's is a string of its own; each one's own list in .github/ci-filters.yaml says
what the group is. Rule 18 of .github/scripts/check-ci-filters.sh refuses a pair
that drifts apart. The group dispatch-memory, dispatch-multiprocess and
dispatch-two-builds share moved here whole instead, since no filter of the first
step names it.

The Dispatch server, this step's copy of the first step's group: dispatch_browser
builds it for the browser suite and envoy_infra_go for the permissions live test,
and both filters are in this step, while daemon_go_tests, plugin_pack and docs name
the group in the first one.

The groups the filters below share: the first step's list pattern for pattern,
written again because a YAML anchor reaches no further than the string it is in.

## ci_router: ci.yaml, which every gate filter but ci_filters names through

ci.yaml, which every gate filter of `.github/ci-filters.yaml` but ci_filters names through this anchor (ci_filters names it through `'.github/workflows/**'`): every job's `if:`
is in this file, so every job reads it - which is why ci_job_reads.py declares
CI_SELF on every gate. Each gate filter also names the `ci-job-<job id>.yaml` file
holding its own job's steps, so a change to one of those files selects that job
alone. This entry stays beside them because a change to ci.yaml does reach every
job: dropping it would make each job's `if:` test a second output of changes, which
.github/scripts/ci_filter_coverage.py refuses (a job's `if:` tests exactly one), and
an output written as a disjunction instead would select exactly what this pattern
selects. The anchor is on the glob rather than on the list, since envoy_smoke and
worker_image name it too and the image check, .github/scripts/check-image-trigger-paths.sh, reads those two as flat lists.

## daemon_envoy_deps: The airlock's rules, its decision and its header swap

The airlock's rules, its decision and its header swap. Not the whole tree: lean/
holds the model, which airlock-lean alone reads, and testdata/ the fuzz seeds and
the mutant patches, which the airlock package's own tests read - the daemon
compiles that package without running them.

## daemon_envoy_deps: The daemon reads the Envoy session timings (heartbeat

The daemon reads the Envoy session timings (heartbeat, live window, claim
staleness and the listener call's attempts) through this one public package: it
is a module of its own, so Go's internal rule bars it from internal/contracts,
where the generated values are.

## legion_command: The broker's OAuth wire types: packages/envoy/internal/brokerclient

The broker's OAuth wire types: packages/envoy/internal/brokerclient (legion_command's `'packages/envoy/internal/brokerclient/*'`)
imports them, so `go list -deps ./cmd/legion` lists this package and the anchor
was one package short of its own definition until this line. Every other filter
naming this anchor already named the package through a second pattern of its
own (docs' own broker list, listener_command's, which go_notices takes), so the
line adds the package to no other filter's meaning.

## legion_command: packages/envoy/migrationid is the identifier rule the daemon's embedded

packages/envoy/migrationid is the identifier rule the daemon's embedded
migrations are held to, in the envoy module so both sets keep one rule.

## legion_command: The one public door onto the generated Envoy session

The one public door onto the generated Envoy session timings the daemon's
liveness and notice rules read: it sits in the envoy module because the
daemon, a module of its own, cannot reach internal/contracts, where they are.

The doors onto the readers of a message name's two forms and onto the
nats-server versions the daemon refuses, for the same reason.

## daemon_go_tests: The daemon-go job: what its two Go steps read together

The daemon-go job: what its two Go steps read together — the plain step, which
tests every package of packages/daemon but the three concurrent ones, and the race
step, which tests those three — so the filter covers the whole module's closure,
and what the Stage 1 proof, ci-job-daemon-go.yaml's "Stage 1 live proof" step, builds and sources. The
packages they compile, the Dispatch server internal/testbuild/testbuild.go:112 builds
for internal/daemon/outbox_scratch_dispatch_test.go:354, and what their tests read
outside the daemon: contracts' fixtures
(internal/api/state_golden_test.go:868, internal/intake/intake_test.go:802), the
agents internal/prompts/roles_test.go:261 stats, the tools skill its :689 holds
roles/mechanics/headless.md to, the merge gate internal/workflow/engine_test.go:432
holds the reviewer's prompt to, the GitHub App helper
internal/api/real_github_test.go:115 sources, and the examples and
docs its tests hold to the code (cmd/legion/config_example_test.go:164,
internal/config/controller_test.go:214, internal/config/kubernetes_test.go:1474).
No bun_install and no skills/** - the two pages of skills/ this filter names are all
this job reads of it: the tests of this suite that packed the two plugins - and so
resolved the root manifest, the lockfile and the patches - or staged each plugin's
partition of skills/ with the prepack script are the plugin-pack job's now, and
this job installs nothing (dispatch://INFRA-1413, A4).

The list is the one the test job's filter used to name through this anchor while
it gated nothing of its own; it is now the daemon-go job's own filter, so a change
a Go test cannot see no longer runs 1,083 s of them (dispatch://INFRA-1413).

## daemon_go_tests: pi-legion's agents: internal/prompts/roles_test.go stats `agents/<name>.md`.

pi-legion's agents: internal/prompts/roles_test.go's TestEveryTaskAgentARolePromptDispatchesIsShipped
stats `agents/<name>.md`.
With its manifest, which the Stage 1 proof (ci-job-daemon-go.yaml's "Stage 1 live proof" step) reads, they are the files of
pi-legion this suite reads. The pack and prompt probes, which read the rest of
it, are the plugin-pack job's.

## plugin_pack: The plugin-pack job: internal/daemon's three tests of

The plugin-pack job: internal/daemon's three tests of PLUGIN_PACK_TESTS
(.github/scripts/ci_job_reads.py) — the one that packs packages/pi-envoy and
packages/pi-legion with `bun pm pack` and loads the two tarballs, and the two prompt
probes of agentmodels_omp_test.go, one of which lays out the prompts this checkout
ships as the release lays them out. The step runs those tests by name and compiles
the package they are in to do it, so this list is that package's test closure -
packages/daemon, the Dispatch server internal/testbuild builds for
outbox_scratch_dispatch_test.go, and the envoy packages internal/daemon imports -
and what the three read: the two plugin packages, everything their prepack builds
each bundle from, the skills it stages, and the frozen install that prepack builds
against, the lockfile included. Those reads are why the job exists: while these
tests were daemon-go's, bun.lock and skills/ were on that filter and a change to
either ran the daemon's whole Go suite (dispatch://INFRA-1413).
No Stage 1 script, no uv and no NATS images: those are daemon-go's steps, not this
one's.

## claude_envoy_build: What packages/claude-envoy/scripts/build.ts and scripts/verify-build.sh

What packages/claude-envoy/scripts/build.ts and scripts/verify-build.sh read, which
is every file the published plugin is built from: the claude-envoy job runs them,
and so do two steps of the test job - its verify-build step, which runs the same
command (packages/claude-envoy/scripts/verify-build.sh), as release.yaml's
claude_envoy_build does, and its build-input-list step, which runs
packages/claude-envoy/tests/build-inputs.test.ts. What only the whole claude-envoy
job reads is in the claude_envoy filter rather than here: the test job runs
that job's lint and typecheck in no step, and no other test of the package.

## claude_envoy_build: release.yaml's version arguments are held to the build inputs

packages/claude-envoy/tests/build-inputs.test.ts's test ".github/workflows/release.yaml's
$CLAUDE_ENVOY_VERSION_ARGS carries the build inputs" holds release.yaml's
$CLAUDE_ENVOY_VERSION_ARGS equal to scripts/build-inputs.txt, so a change to either
must run both jobs.

## claude_envoy_build: Its src/broker-credential.ts imports @legion/broker-client

Its src/broker-credential.ts imports @legion/broker-client (brokerTokenClient from its
/environment module, chooseSignIn from /signins and isFinalRefusal from /token-client), whose exports
map every entry to `./src/<module>.ts`, so a build compiles that package's seven
sources with envoy-client's. check-ci-filters.test.sh holds a hand-written list of
such pairs to this pattern, and ci_job_reads.py's envoy_client_dep is the one
place that declares the pair, which check-ci-filters.sh then holds to the filter.

## claude_envoy: tests/envoy-channel-server.test.ts holds the marketplace

tests/envoy-channel-server.test.ts holds the marketplace entry to this package's
own name, to no `version` of its own, and to a source range that leaves out the
0.0.0 placeholder and admits the first release. No step of the test job runs that
test, so the entry is this job's alone.
The same test holds the Codex marketplace entry to the Codex package's name and
range.

## contracts: The Go copies its tests hold the TypeScript to

The Go copies its tests hold the TypeScript to: src/dispatch-href.test.ts's "the Go
reader's table is the same table" and src/dispatch-text-references.test.ts's "the Go
reader's text table is the same table" read the Go reader's JSON tables,
src/dispatch-block-ref.test.ts reads pmdoc's block pointer and typed block id
tables, and scripts/gen-go.test.ts checks every file scripts/gen-go.ts writes - the
Go contract, pmdoc's generated block schema and contracts' own generated TypeScript
- is current. Not pmdoc/schema, the block schema those generated types are the
shape of: this job generates that Go from packages/contracts/schemas, and reads no
file of pmdoc/schema itself.

## contracts: `bun run check:api` renders schemas/dispatch-openapi.json into

`bun run check:api` renders schemas/dispatch-openapi.json into
src/dispatch-api.generated.ts and compares, and reads no Go at all
(scripts/gen-api.ts:6), so the Go that document is emitted from is not named here:
a change to it is judged by envoy-go-checks' `openapi --check`, whose filter
(envoy_go_checks) selects both that Go and this document, so either one alone
starts the job.

## guide_layout_files: The five guides scripts/semantic-line-breaks.guides.test.ts reads, the checker it

The five guides scripts/semantic-line-breaks.guides.test.ts reads, the checker it
spawns and the test itself: the guide-layout step's filter, guide_layout, is them, and the
test job's filter names them too, since a job's `if:` tests one changes output and
that step runs in no job the test filter did not select. No other step of the job
reads one of them, so the step's own gate is what keeps it off every other change.

## release_push_test: One filter per `*.test.sh` step of the test job: that

One filter per `*.test.sh` step of the test job: that harness and the script it
tests, so the step runs when either changes and skips when neither does. Each is
anchored and named again by the `test` filter, which is what selects the job
the steps run in: a job's `if:` may test one changes output only, so the job's own
filter has to match whatever any of its steps' gates match, and the step's gate
then decides which of them run. Every one of them names this file and
ci-job-test.yaml, as every gate does, and the harnesses under .github/scripts name
test-lib.sh, which each of them sources for `check`, `is` and `summary`.
Before these the job's filter carried one pattern, '.github/scripts/*', for all of
them: a change to any one script ran every harness, and 28 of the 66 commits that
touched that directory since 2026-10-01 changed no Go file (dispatch://INFRA-1413).

## priorities_seed_check_test: The one harness of the job outside .github/scripts

The one harness of the job outside .github/scripts: it carries its own helpers
and sources no test-lib.sh.

## test: The test job: the Claude Code plugin build, the changelog

The test job: the Claude Code plugin build, the changelog fold, the script
harnesses of the `release_push_test` to `go_test_step_expansion_test` filters and the Bun suites of scripts/. The daemon's Go tests are the
daemon-go job's now, and daemon_go_tests, which this filter used to name through
its anchor, is that job's own filter.

It names every harness group (`*release_push_test` to `*go_test_step_expansion_test`) and the guide-layout step's files, because a
job's `if:` tests one changes output and nothing runs in a job that is not
selected: the groups decide which steps run, this filter decides whether the job
they are in runs at all.

## test: The workspace of the one go command left here - the listener

The workspace of the one go command left here - the listener build
scripts/e2e/lib/held-port.test.ts starts - and of the setup-go that installs the
Go for it (packages/daemon/go.mod). The `legion` build of the private-name port
harness, which put that binary's whole closure on this filter, is the
private-names-port job's now, with the private_names_port filter.

## test: Each harness step's own gate, named here so the job runs

Each harness step's own gate, named here so the job runs for the change that
makes one of those gates true. Each of those lists names ci.yaml, .github/ci-filters.yaml and
ci-job-test.yaml, as every gate filter does, so the flattened list repeats the
three patterns the `test` filter already names, `*ci_router`, `*ci_filters_data` and `'.github/workflows/ci-job-test.yaml'`; a repeated pattern selects what it selected.

The guide-layout step's own files, for that same reason: its gate is
the guide_layout filter, and a guide-only change has to select this job for it.

## guide_layout: The guides' layout step of the test job (ci-job-test.yaml's

The guides' layout step of the test job (ci-job-test.yaml's
"Check the guides' semantic line breaks"), gated on exactly the files that step
reads, which the guide_layout_files group holds and the job's own filter names too.

## docs: docs/site's typecheck, screenshots and build, with the sources its reference

docs/site's typecheck, screenshots and build, with the sources its reference
generators and its screenshot harness read. docs-site.yaml holds those steps, and
both the docs job here and docs-pages.yaml's build job call it. docs-pages.yaml
builds and publishes main's site on the pushes this same list selects: its
on.push.paths, which check-ci-filters.sh holds equal to this filter, so the list
names all three workflow files.

## go_notices: lint's notices step: the Go packages it loads for every

lint's notices step: the Go packages it loads for every release target (`legion`,
the listener, the agent-secrets binaries and client, every envoy-infra command), the
workspaces they build in, the target lists, the script holding the loops and the
notices script it runs per target. Every Go and module file of the repository is in
it because scripts/go-third-party-notices.sh reads each target's `go list -deps`: a
new import changes the notices with no module file changed.

## go_notices: The step loads each release target's packages and writes

The step loads each release target's packages and writes their notices; it runs no
test, and no package directory it loads under packages/envoy/internal holds a
Markdown file. The 230 that ride in today are the pmdoc parser's corpus
(internal/dispatch/pmdoc/testdata), which only the jobs that run pmdoc's tests
read, so a guide or a corpus case written under packages/envoy/internal starts no
notices build (dispatch://INFRA-1413's A2).

## sessions_script: The session message-wait suite's own job: the suite

The session message-wait suite's own job: the suite, the script it spawns, and the
Bun that runs both. No install: the two import node: builtins and bun alone, so no
lockfile and no workspace manifest is an input of this job. The lint and the
typecheck of the same directory stay in the test job, which the `test` filter
names scripts/sessions for.

## pi_legion: src/skills-guard.test.ts spawns packages/claude-envoy/scripts/build.ts

src/skills-guard.test.ts spawns packages/claude-envoy/scripts/build.ts (PLUGIN_BUILDER) and
asserts an exact file list over the tree it writes, so this job reads that whole
package and the one entry of its BUNDLE_ENTRYPOINTS (scripts/build.ts:39-44) that
lives outside it. A spawn carries no import, so no derivation of this repository
sees the reach: both patterns are hand-kept, with their declared reads in
ci_job_reads.py.

## pi_legion: The Dispatch e2e client that scripts/skill-scenarios/seed.ts imports

The Dispatch e2e client that scripts/skill-scenarios/seed.ts imports from
../../../dispatch/e2e/api, which the
typecheck compiles: that program holds six modules of this directory (api.ts,
harness-ports.ts, harness-broker.ts, harness-processes.ts, harness-real-broker.ts
and harness-signin.ts), so the directory is named rather than the two it reached
when this entry was written.

## dispatch_ui: The dashboard's and the proof editor's fast checks (dispatch-ui)

The dashboard's and the proof editor's fast checks (dispatch-ui): what their lint,
typecheck, unit tests and the dashboard's build and asset checks read. No Go and no
Dispatch server: the job runs neither.

## dispatch_ui: The one markdown fixture table, read through `web/src/__tests__/pmdoc-fixtures.ts`

The one markdown fixture table, read through `web/src/__tests__/pmdoc-fixtures.ts`'s
pmdocFixtures, whose path is that file's TABLE_PATH, by the three suites this job runs over it:
features/refs/fixture-table-readings.test.ts,
markdown-engine.test.ts and markdown-surfaces.test.tsx. The dispatch-browser job
declares it too, for the browser rows that import the same module, but that job
runs no unit test.

## dispatch_ui: The dashboard's tsc program compiles @legion/envoy-client's

The dashboard's tsc program compiles @legion/envoy-client's sources, and its e2e
helpers' test/fake-dispatch with them, through the workspace's "bun" export
condition, so check-ts-filters.sh holds this filter to both. The two suites that
import it, `e2e/__tests__/delivery-live-probe.test.ts:6-9` and
local-instance-support.test.ts:2-5, run in this job's test step and in no other,
so that read is this gate's (ci_job_reads.py's dispatch_ui).

## dispatch_browser: The browser suite (dispatch-browser), which .github/workflows/dispatch-browser.yaml

The browser suite (dispatch-browser), which .github/workflows/dispatch-browser.yaml
holds: the shards, the three `-alone` projects, the two-process job, the
fake-embedder job, the visual review and the merge job. Every entry is something a
browser row reads: the dashboard and its contracts, the Dispatch server its harness
builds, and the two workflow files that hold the steps. The retired `dispatch` job's
filter also held three fixture sets whose only readers are the dashboard's and the
proof editor's unit tests, and they are not here: dispatch-ui's filter names all
three, and with this output ungated on a pull request they would start four shard
machines and the merge job for a fixture-only change. The dispatch_browser gate in
.github/scripts/ci_job_reads.py says what this suite reads, and check-ci-filters.sh
holds this filter to that gate - it refuses a filter that misses a read, never one
that is too wide, so a path is in this list because a browser row reads it and for
no other reason.

## dispatch_browser: dispatch_ui's set, which the SPA and the e2e helpers load,

dispatch_ui's set, which the SPA and the e2e helpers load, with the bundle of the
dispatch command its rows spawn (e2e/dispatch-cli.ts:8,39), dispatch_two_builds_go's
set (ci_ts_programs.py, LISTED, says why). Its jobs' contracts build
(`build:dispatch`) compiles nothing outside the first: not legion-api.ts.

## dispatch_browser: The workflow that holds every browser step, which this job

The workflow that holds every browser step, which this job reads as its own
definition - the place every other job of this file gives to its own
`ci-job-<id>.yaml`, so a change to it selects this job alone. An unlabelled pull
request runs the pull-request lane, the two projects a change is held to; a pull
request carrying `full-ci`, the merge group and a dispatch run the full lane,
every project and the two-process job, which is the lane the nightly
(.github/workflows/dispatch-browser-nightly.yaml) calls. So a worker can put the
whole suite on a pull request with the label, and the group is where a change to
that file meets it before main anyway. The nightly itself is not in this list:
no step of this suite reads it, which rule 9's second direction refuses, since
four shard machines and the merge job would start over a change no row sees. It
is a read of the ci_filters gate instead, so a change to it re-runs this check,
which holds this filter and that gate together.

## dispatch_browser: The memory and CPU sampler the shard and alone jobs'

The memory and CPU sampler the shard and alone jobs' browser steps start and read
back, the reading of the lanes' PostgreSQL those two jobs run before the suite,
the script the merge job runs to take each leg's newest attempt, and the script
that makes the merge job's two reads of its own run, which reads this run's own
attempt's jobs and says a refused blob download was a refused read. Beside them,
the twelve scripts that hold the bodies of the shard, the alone, the visual and the
merge jobs' steps: the shard job's wait for the file store it starts, its
browser step and the record of what that run left behind, the alone job's
browser step and its own record step, whose text differs from the shard job's
because no file store runs on that machine, the visual job's run against the
baselines, and the merge job's merge of the legs' blob reports, its four
readings - the rows, the flake rate, the leg exits and the row durations - and
the verdict that fails the job on a red leg or a failed row. The Playwright
install both jobs ran stood here too, with the pinned apt list it installed, the
key their apt archive cache was read under and the check that the list had not
fallen behind Playwright: those two jobs run in the image this repository builds
now, which carries the browsers and their host packages, so all four files are
gone (dispatch://INFRA-1443).

## dispatch_browser: The file store's one-collection rule, which the shard job's wait script and its check step run

The file store's one-collection rule: .github/scripts/dispatch-browser-wait-for-store.sh,
which the shard job's step "Wait for the S3-compatible server" runs, runs its prepare and its
check, and the step "Check that no write grew the file store" after the browser step runs its
check again. internal/tests3 embeds the same file for the Go tests' server, which envoy_go's
'packages/envoy/**' already selects.

## dispatch_browser: The container image the shard and alone jobs run in and

The container image the shard and alone jobs run in and everything that decides
which bytes it is: the row that pins its digest, the Dockerfile it is built from
and the workflow that builds it. A change to any of them changes what those jobs
run inside, so they run (dispatch://INFRA-1443). The row is named as the whole
file because a paths filter matches files and not rows, so any row change
re-runs the browser jobs; that is correct and cheap. The two-process and merge
jobs are on this same filter and run in no container, so an image change runs
them too, which costs a five-minute Chromium job and keeps one list rather than
two.

## dispatch_browser: The one markdown fixture table: the browser row each

The one markdown fixture table: the browser row each of the seven parity families
whose shapes the corpus holds keeps reads it at
`web/src/__tests__/pmdoc-fixtures.ts:48`, whose path is the new URL at :44. This
entry declares that read explicitly and selects nothing on its own: the
`*dispatch_server` anchor this filter takes already matches it through its
own 'packages/envoy/internal/dispatch/pmdoc/**', as it does the block schema
e2e/engine-reading.ts:13 reads. Measured with ci_filter_coverage.changes_filters
and workflow_paths.PathsFilter under both quantifiers: this filter covers the
table with the entry and without it, where dispatch-ui's own entry for this same
path covers it only with, that filter naming no anchor that reaches the table.
Those three are named rather than cited by line: a citation into this file moves
with every comment added above it, and one entry's line number took four values in
two rounds that way.

## dispatch_browser: What stood here was the file store's own image declaration

What stood here was the file store's own image declaration and the
pull-test-images action: no job of this suite pulls an image or runs a docker
command now, since a job inside a container can do neither. The shard job starts
the `weed` binary its own image carries instead, so neither file reaches it.
The real secrets broker e2e/broker-approval.e2e.ts approves a request on, the dev
OIDC issuer it trusts, and the seeding step that logs its service in and enrols its
pods: the packages `go list -deps ./cmd/broker ./cmd/dev-oidc-issuer
./internal/broker/brokertest/requestseed` lists in packages/envoy beyond the
Dispatch server's, which e2e/run-broker.sh builds in one `go build`.

## envoy_go: internal/dispatch/text/whitespace_fixture_test.go holds unicode.IsSpace to this

internal/dispatch/text/whitespace_fixture_test.go holds unicode.IsSpace to this
list, which packages/contracts holds goBlankPattern to: an edit of the list
alone must start both jobs, or the side it disagrees with stays green.

## envoy_go: The shard job's file-store steps and the script its wait step runs, which internal/tests3 holds to their bounds

internal/tests3/storesteps_test.go holds the shard job's file-store steps in
.github/workflows/dispatch-browser.yaml, and the script its wait step runs, to their bounds
(its RULES entry in .github/scripts/ci_job_reads.py), so an edit of either file alone must
start this job.

## envoy_go: The three jobs whose tag steps lean on this job's untagged

The three jobs whose tag steps lean on this job's untagged run of ./cmd/dispatch
and ./cmd/broker: each runs only what its tag adds, so a change that starts one
must start this job too (check-ci-filters.sh's rule 15).

## envoy_go_checks: envoy-go's fast half (envoy-go-checks): the format, generated-contract and pmdoc

envoy-go's fast half (envoy-go-checks): the format, generated-contract and pmdoc
checks, the shell-script tests, the vet of the module as darwin compiles it,
and the build and pack of the envoy commands,
with the tests of internal/dispatch/architecture/notes, internal/runnerlog,
internal/dispatch/specgraph, cmd/architecture-check and cmd/spec-check, whose one
run this is (envoy-go's unit step leaves them out), the architecture-note check over
this repository's own notes through the composite action
.github/actions/architecture-check, and the spec-file check over its .dispatch/spec
through the composite action .github/actions/spec-check. gofmt -l . walks every .go
file under packages/envoy, those tests and the scripts they drive live there, and
`go build ./cmd/dispatch ./cmd/listener` and the darwin vet compile packages of that
module alone, as the two actions' builds of their commands do.

## envoy_go_checks: Those two steps judge the files at the actions' default paths,

Those two steps judge the files at the actions' default paths,
.dispatch/architecture and .dispatch/spec. The pattern names .dispatch whole:
main holds no spec file yet, and check-ci-filters.sh refuses a pattern that
matches no file (its rule 9).

## text_checks: The text checks of the three Go modules, the Dispatch

The text checks of the three Go modules, the Dispatch API's nine static source
checks, and the two pure pins of packages/daemon's supervise table: the tests whose
subject is a file's words. Their job runs packages/envoy/internal/textcheck,
packages/envoy/infra/internal/textcheck and packages/daemon/internal/textcheck,
which the three Go lanes leave out of their own runs, those checks' seventeen tests
by name, and those two pins by name, which the daemon-go plain step skips.
packages/envoy stays a wide
root: file_copy_test.go walks every .go file under it (infra included), the API's
guards and one_expression_guard_test.go walk every non-test .go file of the
module, and the guard step compiles that package's whole test closure, which
reaches testdata and goldens across the module.
Of packages/daemon the job reads the .go files its own file_copy_test.go walks,
the .go files the supervise pins type-check from source - internal/supervise and
every package of this module it imports - the prompt the `text_checks` filter names (`packages/daemon/internal/prompts/roles/architect-root.md`), the committed copies
its text checks hold to the texts they copy, and the store's migrations; and its
credential-argument check reads every text file of the repository, so the `text_checks` filter's `'**'`
selects the job for every change but one under docs/plans or docs/superpowers. The
patterns after it name what the other checks read, which `**` covers too, so that
each read keeps its reason here.

credential_argument_test.go's credentialTextFiles lists every file git tracks or would
track, but docs/plans and docs/superpowers, which are dated history, and its
TestNoTextGivesACredentialAsACommandArgument reads each of them.

## text_checks: The seven copies of shipped texts that readExtract reads

The seven copies of shipped texts that readExtract (waiting_line_texts_test.go)
reads to hold them to the texts themselves: the waiting line's four, the two way
backs' (way_back_doc_test.go:129) and the troubleshooting page's refusal table's
(troubleshooting_refusals_doc_test.go:41), each committed beside the package whose
own check reads it. They are .txt files, which the module's `'packages/daemon/**/*.go'` in the `text_checks` filter does
not match.

## dispatch_memory: `go test -tags memory ./cmd/dispatch`. memory_profile_test.go

`go test -tags memory ./cmd/dispatch`. memory_profile_test.go reads four guides only
when DISPATCH_EDIT_PROFILE_OUT is set (its line 126), which CI never sets, so they are
not here. cmd/dispatch/nats_unreachable_test.go, which carries no tag, starts the
in-process NATS (its line 47).

## dispatch_two_builds_go: The two-build job's Go step alone, behind a gate of

The two-build job's Go step alone, behind a gate of its own (dispatch_two_builds_go):
`go test -tags twobuild ./cmd/dispatch`, the same closure as dispatch_multiprocess,
the script its -run comes from, the script that builds the previous build it runs
against, the sizing of its Postgres, and the current build's CLI
twobuild_cli_test.go runs (`bun packages/envoy-client/bin/dispatch.ts`, with the
sources it loads, after a frozen Bun install). TAG_RUNS pairs the step with envoy_go,
whose filter rule 15 holds to all of this, the listed block's files among them, and
to nothing of the browser half, which the dispatch_two_builds filter names.

## dispatch_two_builds: The two-build job: its Go step's reads, named again

The two-build job: its Go step's reads, the dispatch_two_builds_go filter's, named again so the job runs for the
change that starts the step, and the browser half,
`bun run e2e:multiprocess -- cross-build.multiprocess` in packages/dispatch: the
harness and spec under e2e/, the two SPA builds (web/, the editor package and the
patches it takes, the notices script the build runs), the Dispatch server
e2e/run-server.sh builds (dispatch_command_tests holds its packages) and the script
that holds each server's socket (run-server.sh:37, e2e/harness-start.ts). Of
packages/contracts it reads the manifest whose build:dispatch its contracts build runs
(*bun_install holds it) and the modules of the dispatch_two_builds filter's `BEGIN listed` block.

## listener_server_change: The server-change soak (listener-server-change): `go

The server-change soak (listener-server-change): `go test -tags serverchange
./cmd/listener`. The listener's own packages, and what its test binary compiles
beyond them - the in-process NATS and Postgres helpers, the builder of the binary it
starts, and the Dispatch packages cmd/listener's tests drive. A dependency's own
testdata is no input of this job: the tests that read it are that package's, and
this job runs cmd/listener's alone.

## listener_server_change: What cmd/listener's test binary compiles that `go list

What cmd/listener's test binary compiles that `go list -deps ./cmd/listener` does
not, so listener_command, which is that build closure, leaves them to the jobs
that run the tests: the chat client and the router.

## airlock_lean: The Lean model of the airlock and the Go code it covers

The Lean model of the airlock and the Go code it covers (airlock-lean): `lake build`
and the axiom print read the model's own project, and the two fuzz runs compile the
airlock package and compare it against the model's program, so a change to either
side runs the job. Both sides live under internal/broker/airlock, which one glob
names, and the Go side also compiles internal/fold, the case fold it compares names
in; the axiom check is a script of its own, which check k15 runs too; the Lean
toolchain comes from the elan the job's action installs, and that action reads the
`elan` row of the tool manifest through the fetch helper every install goes through.

## envoy_smoke: Every file packages/envoy/docker/Dockerfile builds from

Every file packages/envoy/docker/Dockerfile builds from, so envoy-smoke runs whenever
the image it smokes changes, every file the held-port image built on top of it reads
(packages/envoy/infra/host/heldportwrapper/Dockerfile and its .dockerignore), and what
the job's tests compile: TestSmoke in the root workspace, and envoy-infra's host live
tests and plugin in the infra one.
.github/scripts/check-image-trigger-paths.sh fails the build when this list misses a
file the image build reads; it reads this filter only as a flat list, so the one
anchor it names is ci_router, which stands for a single glob and leaves the list
flat. check-ci-filters.sh holds it to the rest: the job's go commands and its other
steps' reads.

## envoy_smoke: The job's steps, which build the image and smoke it

The job's steps, which build the image and smoke it: check-image-trigger-paths.sh
counts the workflow file holding a build step among the image's inputs, so a
change to it must start the build.

## envoy_smoke: The Envoy module, which the image's Go stage copies whole

The Envoy module, which the image's Go stage copies whole, and the envoy-infra
module under it, which the held-port image's build copies whole with it
(packages/envoy/infra/host/heldportwrapper/Dockerfile and its .dockerignore) and
envoy-infra's host live tests and plugin build compile in packages/envoy/infra's own
workspace. The envoy_smoke filter's two hand-written `!` patterns, `'!packages/envoy/deploy/**'` and `'!packages/envoy/.changes/**'`, leave out deploy and the changelog fragments,
which neither .dockerignore lets in; the generated block leaves out the test files
(internal/integration holds nothing else).

## envoy_e2e_local: And @legion/broker-client with it: the tsc program of

And @legion/broker-client with it: the tsc program of envoy-client holds all seven
of its modules, and at runtime, which is what this job's bun scripts do,
transport.ts's own closure holds two of them (delivery.ts's holds none), so the
pattern is needed through transport.ts (ci_job_reads.py's envoy_client_dep).

## envoy_e2e_local: The job reads no Markdown file: the listener, the renderer

The job reads no Markdown file: the listener, the renderer and the contracts
between them are its whole input, so a guide written under one of the directory
patterns of the envoy_e2e_local filter (`'packages/envoy-client/src/**'`, `'packages/broker-client/**'` and the rest) would start it for nothing.

## worker_image: Every file packages/daemon/docker/worker.Dockerfile builds

Every file packages/daemon/docker/worker.Dockerfile builds from, for the worker-image
job (.github/workflows/worker-image.yaml, which the build reads too): the root
manifest, lockfile, patches and every workspace manifest the plugin stage's frozen
install copies; the packages it copies whole (the daemon with the Dockerfile, the two
plugins and the workspace packages they inline, the Claude Code plugin, whose source
the plugin stage builds into the image's dist/ and legion/ with the .bun-version that
build is pinned to, the skills and the prepack that stages them); the Go workspace; packages/envoy, copied whole for its agent-secrets
binary but for packages/envoy/infra, which the .dockerignore drops, so each of its
top-level directories but infra is named; the scripts that write the image's
third-party notices; the Oh My Pi pin; and the context's .dockerignore. Then a `!`
pattern for each .dockerignore line that drops part of those packages, of
packages/envoy or of skills/, which the step's some-with-excludes reads as an
exclusion, so a pull request touching only files the build does not read builds no
image.
.github/scripts/check-image-trigger-paths.sh fails the build when this list misses a
file the image build reads, including a new top-level directory of packages/envoy, or
matches a file it does not read other than the two workflow files; it reads this
filter only as a flat list, so the one anchor it names is ci_router, which stands
for a single glob and leaves the list flat.

## worker_image: Changelog fragments, which worker_image's package patterns match and

Changelog fragments, which worker_image's package patterns (`'packages/daemon/**'` and the rest) match and no build reads: .dockerignore
leaves them out of the context, so a pull request that only writes a changelog entry
builds no worker image, and a fragment stays where it was written, so no pattern
here matches nothing.

## browser_image: Every file the browser image's build reads, for the browser-image

Every file the browser image's build reads, for the browser-image job
(.github/workflows/browser-image.yaml, which the build reads too): that build's
context is .github/docker, so every file under it reaches the build, and the
Dockerfile it builds is the only file there today.
.github/ci-images.yaml is deliberately not named. The build does read that file:
its `row` step reads the registry and the digest of the row built from that
Dockerfile out of it at run time, and its publish decision is that digest and the
label the image at it carries. But .github/scripts/check-image-trigger-paths.sh
reads what a build reads from its context and its Dockerfile alone, and it
refuses a path matched by a filter that decides nothing but an image build and
that no build reads by that reading: "browser_image matches
.github/ci-images.yaml, which no image build reads, so a pull request touching
only that builds an image for nothing; narrow the filter". The worker_image filter
leaves .github/scripts/repo-paths.sh out for that same reason, though its build
job runs that script. That refusal does not reach this filter today: the steps of
browser-image.yaml's build job are in no BUILD_PLUMBING_STEPS entry of that check
and its `attest` job is in none either, so the check reads the browser-image job
as doing more than build an image and leaves this filter's breadth unjudged
("not checked for files no image build reads"). The file stays out all the same:
the argument below is why, and a filter narrowed by hand needs no refusal to
keep it narrow.
Nothing is lost by leaving it out. A change to the row alone builds nothing, and
the digest a row names is one the package already holds: the ci-images job, whose
filter does name that file, is what reads whether it does
(.github/scripts/check-ci-images.sh package), and the push that writes the row
needs no build of its own: the image that row names was built and pushed by the
earlier push, the one that changed the Dockerfile under .github/docker, which
this filter does name. The `container:` jobs that run the image are in
.github/workflows/dispatch-browser.yaml, and this file declares no `container:`.
That check also fails the build when this list misses a file the image build reads;
it reads this filter only as a flat list, so the one anchor it names is ci_router,
which stands for a single glob and leaves the list flat.
.github/scripts/ci_images.py is named, and is the one file here the build does
not read as a file: the build's `labels:` is
legion.browser-image.source-sha256=<the row step's source output>, and that step
computes the output with that file's recipe_sha256, so the file's own bytes
decide a label of the image this build pushes. Without it a change to the rule
that defines the hash selected the ci-images job, whose rule 10 then refused the
published image for carrying a hash this tree no longer computes, while no build
ran and no row could move. check-image-trigger-paths.sh reads it as an input of
this build for that reason, from the COMPUTED_BUILD_INPUTS entry that writes the
argument down, and that entry is what refuses this filter if it stops naming it.

## ci_filters: The directory rather than the workflows the check names one by one

The directory rather than the workflows the check names one by one in
ci_job_reads.py's reads for this gate: its rules read every
workflow of it - ci_workflow_rules.check_one_merge_group_workflow refuses any but
this one that runs on merge_group, check_cache_round_trip holds every workflow's
restore of a family ci-caches.yaml fills to the fill's key, and ci_only_documents
reads them all to find the files only this one calls - and a rule that reads a
workflow has to run on the change that adds or edits one, which a list of the
names already there cannot select.

## tool_fallback: ci.yaml's tool-fallback and tool-fallback-arm jobs, the

ci.yaml's tool-fallback and tool-fallback-arm jobs, the live proof of the second
source every tool install reads when the first source does not serve the bytes:
the manifest that names both sources of each row, the fetch helper every install
goes through, the filler and the gate beside it, the rig and the fault proxy it
drives, the eight actions it installs through, and the two files a row's version is
pinned in that the job itself reads (.bun-version for the bun rows and the
worker image's Dockerfile for mise, jj and uv).
packages/envoy/infra/internal/pulumitest/pulumitest.go, whose
`const DockerPluginVersion` the gate holds the pulumi-docker row's version to,
is not here and no step of either job reads it: a raise of that constant must
move the row, and .github/ci-tools.yaml is in this list.
One filter for the two jobs, as `envoy_go_race_docs` is for its two: the arm64
job drives the same action over the same manifest through the same helper, so a
change that selects one of them must select both, and one list cannot drift from
another that does not exist. Each job carries a gate of its own in
.github/scripts/ci_job_reads.py for what it reads.
Each job holds its steps in a called file of its own, which this list names beside
ci.yaml, where both jobs keep their gate.

## ci_images: ci.yaml's ci-images job: the image copy (.github/scripts/mirror-ci-images.sh

ci.yaml's ci-images job: the image copy (.github/scripts/mirror-ci-images.sh,
through the crane .github/actions/install-crane installs), the package mode of
.github/scripts/check-ci-images.sh, which reads ghcr.io, and the tool copy
(.github/scripts/mirror-ci-tools.sh), which fills the second source of every tool
install. These files are what decides what those three steps do, the built row's
whole build recipe among them: the package mode hashes the recipe - the browser
Dockerfile's own path, every file of .github/docker and
.github/workflows/browser-image.yaml, folded by the recipe_sha256 of
.github/scripts/ci_images.py - and holds the built row's published image to that
hash. The gate's other half reads the whole tree and is a step of lint, which
runs on every change.
It names what this job reads and nothing else, the tool_fallback filter
included: rule 9's second direction refuses a pattern of a gate's filter that
matches no file its job reads, so this list cannot name that filter whole to be a
superset of it by construction. ci.yaml's tool-fallback job therefore carries
`!cancelled()` rather than leaning on that: its `needs: ci-images` is for order,
since a run that selects both must fill the copy before the proof reads it, and
the one file that can bring a new row in, .github/ci-tools.yaml, is in both
filters, so every change that adds a row runs the fill first.

## ci_images: The job reads no Markdown file: its steps read the image

The job reads no Markdown file: its steps read the image rows, the two scripts
that copy and check them, the tool rows and the filler beside them, and the
crane the action installs, so a guide written under one of those directories
would start it for nothing.

## proof_sdk_offline: The proof-sdk-offline job: a frozen install behind a

The proof-sdk-offline job: a frozen install behind a proxy that answers 504
for every host the proof editor's old `github:sjawhar/proof-sdk#<sha>` route used.
What can send that install to one of those hosts again is every file the install
reads, which is the shared group bun_install, so the filter names it whole rather than
the four sites of it this job is really about: bun.lock, which a reintroduced
remote dependency necessarily moves, package.json, the root manifest the frozen
install reads, packages/proof-editor/package.json, the manifest that names the
tarball, and packages/proof-editor/proof-sdk-*.tgz, the file itself. Beside it:
.github/scripts/ci-fault-proxy.py, the program that drives the install, and the
two files that hold the job: ci.yaml, which carries its `if:`, and the called
file its steps live in.

## envoy_client: The Envoy client's job (envoy-client)

The Envoy client's job (envoy-client).

## envoy_client: `src/__tests__/dispatch-who-waits.test.ts`'s `no text states the old rule` reads

`src/__tests__/dispatch-who-waits.test.ts`'s `no text states the old rule` reads
every text file git lists under these roots (OLD_RULE_ROOTS) and every AGENTS.md,
the root's and the nested ones among them, so a text there that states the
replaced rule must start this job. The recordings under packages/dispatch/e2e
start it too, though the row leaves them out: this filter takes no `!` pattern.

## envoy_go_race: The shared race job: the closure of `go test -race`

The shared race job: the closure of `go test -race` over delivery, the Dispatch
store, the bus, the listener, the registry store, the session cache, kvwatch, natscred
and the NATS auth service (`go list -deps -test -race` in packages/envoy), and every
.md file under packages/envoy, which internal/dispatch/store's
search_text_test.go:104-120 walks (the RULES row for
that file in .github/scripts/ci_job_reads.py declares packages/envoy/**/*.md). The
filter names the wide root rather than that package list and that glob, and its
generated block keeps it off the Go test files the job does not compile, and
listener_server_change's tagged soak test, which rule 15 holds this filter to, is
not among them. No bun_install anchor: no test of these packages runs bun.

## envoy_go_race: listener-server-change takes its -run from this script

listener-server-change takes its -run from this script, and this is the job that
runs cmd/listener's untagged tests, so it must run whenever the script changes -
and whenever that job's own definition does, for the same reason
(check-ci-filters.sh's rule 15).

## envoy_go_race_api: The API race job. Not packages/envoy/**, as envoy_go_race has: that

The API race job. Not packages/envoy/**, as envoy_go_race has: that root was here
because internal/dispatch/api's lock_level_test.go, event_insert_guard_test.go,
issue_route_writer_guard_test.go and review_write_guard_test.go walk every non-test
.go file of the module, and those source guards carry `//go:build !race`, so the race
build compiles none of them and the text-check job runs their tests instead.
one_expression_guard_test.go walks the module the same way and is on that side of
the tag for the same reason,
so no module-wide walk is left in this job's build (dispatch://INFRA-1413). What is
left is the closure of `go test -race ./internal/dispatch/api/` (`go list -deps
-test -race` in packages/envoy), one pattern per directory, with what those
packages embed and the testdata of the package whose tests this job runs: a
dependency's testdata is read by that package's own tests, which run in a job of
their own.

## envoy_go_race_api: The API's CI-reconcile passes route reads cireconcile's

The API's CI-reconcile passes route reads cireconcile's own record, so the race
build of ./internal/dispatch/api compiles that package too. Its testdata is not
here, for the reason this filter's header gives: a dependency's testdata is read
by that package's own tests, which run in a job of their own.

## envoy_go_claim_model: The mention sender's claim model's own job

The mention sender's claim model's own job: `go test -run` of its two tests in
./internal/dispatch/api. A -run moves no closure, so this is what the package's plain
test build reads. packages/envoy stays a wide root, as text_checks' does: the !race
source guards that build compiles walk every non-test .go file of the module, infra
and testdata included, and the package's test closure reaches embedded files,
migrations and goldens across it.

## envoy_go_race_docs: The document service's own race job. Not packages/envoy/**, as envoy_go_race

The document service's own race job. Not packages/envoy/**, as envoy_go_race has:
a race job of twenty minutes or more must not run on every change under that tree
(INFRA-490). This is the closure of ./internal/dispatch/docs/ (`go list -deps -test
-race`), one pattern per directory, with what those packages embed and the testdata
its own tests read - this package's, and pmdoc's, which one of them names through
`..`. A dependency's testdata no test of this job reads is not an input.

## envoy_go_race_docs: Two jobs run this package under the detector and share

Two jobs run this package under the detector and share this filter, so it names
both of their files (ci_job_reads.py gates envoy-go-race-docs and
envoy-go-docs-finalizers on it).

## private_names_port: The private-name port harness, in a job of its own: it

The private-name port harness, in a job of its own: it builds the `legion` binary
of this head (.github/scripts/check-private-names-port.test.sh:70) and holds the
rules that binary carries to the frozen shell script's verdicts, case for case.
That build is why this list is as long as it is - legion_command is what the
binary is made of, the rule file packages/daemon/internal/leakguard embeds among
it - and why the step has a job: while it was the test job's, the whole closure
sat on the `test` filter, so a change to one package of the daemon ran thirty-odd
shell harnesses and the Bun suites of scripts/ with it (dispatch://INFRA-1413).

## private_names_port: test-lib.sh, which the harness sources for check/is/summary

test-lib.sh, which the harness sources for check/is/summary (:63), and the
frozen script it runs every case through beside the binary, symlinked into each
fixture (:213,421,467,556). That script is the subject of
the check_private_names_test filter, which gates another job's step: this job reads
it as the engine it compares against.

The sections from here on are `.github/workflows/ci.yaml`'s own, the router's, in the order
their comment blocks stood in it.

## The router: every check a pull request or the merge queue runs

The router: every check a pull request or the merge queue runs, in one workflow.
Its first job, `changes`, reads the paths the change touched and selects, through one
dorny/paths-filter filter per job, the jobs that read those paths; each job runs only when its
filter matches (lint runs on every change, and typecheck, test, daemon-go, plugin-pack,
private-names-port, sessions-script and text-checks also run when `changes` itself failed).
ci-ok condenses every job's result into one check: it fails when a job `changes` selected did
not succeed.
Main's ruleset requires ci-ok and pr-title. ci-ok is the `ci-ok` job
at the end of this file, which needs every job of it but itself and those CI_OK_EXEMPT names,
and fails when .github/scripts/ci-ok.sh's verdict over their results is failure, so the
verdict rides on that job's own check run — a job's check run is in its run's own check suite,
where a check run posted with POST /repos/.../check-runs joins the Actions App's first suite
at that commit, which is another workflow's as often as not (.github/scripts/post-check-run.sh's
header holds the measurement). Every job but `changes` and those CI_OK_EXEMPT names still
ends with the ci-ok-report step (.github/actions/ci-ok-report), which posts a failing ci-ok
at once when that job's own result decides a merge-group run, so the queue drops a doomed
group at the first failure rather than at the end of the run; the ci-ok job's last step then
sets every ci-ok check run on the head that is not its own to its own conclusion, so one
verdict stands at the end of every attempt. pr-title, the other check that ruleset requires, is
the `pr-title` job at the end of this file, in the same shape and for the same reason: it runs
check-pr-text.sh on a pull request that is not a draft and on the merge queue's group, the two
events this workflow runs that carry a pull request's text, and its own check run is the report,
so it posts nothing. pr-title.yaml carries that same check when the pull request is edited, the
event this workflow does not run on, as its own job's check run too; its header says why it runs
on nothing else.
The docs site's build on a push to main, and its GitHub Pages
deploy, are docs-pages.yaml's, a workflow of its own whose header says why. Its paths are held
to the docs filter of .github/ci-filters.yaml.

What a workflow file of this repository holds, and where its filter data, its longer shell
bodies and its prose go instead, is the rule .github/ci-notes/README.md writes down. Each
`changes` output that hands on a filter's verdict reads `steps.filter.outputs.<name>`, and
rule 19 of `check-ci-filters.sh` holds `<name>` to a filter of .github/ci-filters.yaml.

A new job joins the router in eight places: a filter in .github/ci-filters.yaml that names every
file the job reads, with the `*ci_filters_data` line beside its `*ci_router`, an output of
`changes` for that filter, the job's `needs: changes` and an `if:` on that
output carrying the term that skips it on a re-run attempt of a merge-group run
(.github/ci-notes/ci.md#what-the-changes-job-selects-and-the-two-terms-every-job-carries
says why every job needs its own), the job's steps in a reusable workflow of
its own, `.github/workflows/ci-job-<id>.yaml`, which the job here calls with `uses:` and hands
`ci-ok-changes: ${{ toJSON(needs.changes) }}` in its `with:`, under a `permissions:` block of
its own (a calling job that declares none hands the called workflow the default GITHUB_TOKEN
permissions rather than this file's block, and the report step's ci-ok needs `checks: write`),
an entry in CI_OK_SELECTED_BY (this file's env) mapping the job to that output (and in
CI_OK_PULL_REQUEST_ONLY when its `if:` admits only pull_request), the ci-ok-report step as the
job's last step (a job that calls a workflow passes `ci-ok-changes` to it, and that workflow's
job ends with the step, as docs-site.yaml's and worker-image.yaml's do), the job's id in the
ci-ok job's `needs:`, without which the verdict is reached before that job ends and says
nothing about it, and a gate in
.github/scripts/ci_job_reads.py. An output an event must force is written in `changes`' outputs
rather than left to its filter, as every output's workflow_dispatch case is, and so is one an
event must hold back, as the ten slow suites' pull-request case is
(.github/ci-notes/ci.md#the-ten-slow-suites-whose-runner-time-dwarfs-every-other-jobs-the).
The ci-filters job's .github/scripts/check-ci-filters.sh holds the filter to that gate,
CI_OK_SELECTED_BY to the `if:`s, the report step to every job, the ci-ok job's `needs:` to
every job of the file but the exempt ones, and the re-run term to every job but `ci-ok` and
`pr-title`: each of those two is named for a check main's ruleset requires, where a skip is a
passing required check, so each refuses such an attempt in a step of its own instead.
docs/solutions/github/join-a-job-to-the-ci-router.md walks through it.

Caches: one step of the jobs this workflow calls saves an Actions cache entry itself,
dispatch-two-builds' `Cache the previous build` (ci-job-dispatch-two-builds.yaml), which keeps
the build of the previous commit that job compares against; three actions its jobs call can
save one too, and only in a dispatched run (setup-bun, and architecture-check and spec-check at
this paragraph's end).
setup-go caches nothing here (`cache: false` on every step, the two check actions'
included). The jobs that cache Go restore, in steps of their own, one of six families with
actions/cache/restore: go-daemon (typecheck, daemon-go, plugin-pack — which compiles the same
internal/daemon test binary daemon-go's race step does — and pi-legion; the test job restored
it while it ran the daemon's Go suite and does not now), go-docs (docs, and the build job
of docs-pages.yaml, which saves it), go-envoy (envoy-e2e-local and envoy-session-stall, which
build the same listener and natsd, listener-server-change and envoy-go-checks, which build the
listener, and envoy-go-race-docs and envoy-go-docs-finalizers), go-envoy-test (envoy-go, which
builds the test binary of every package of the envoy module, envoy-go-claim-model, which builds
the API's of that set, and the binaries, multiprocess
and visual jobs of dispatch-browser.yaml, which build the Dispatch server and the real
broker's commands out of that same set), go-envoy-infra
(envoy-infra-go) and go-notices (lint's notices step). Every family but go-docs is filled by
.github/workflows/ci-caches.yaml, which builds those sets on a push to main and saves them;
go-docs stays with docs-pages.yaml, which saves it on that same push. Each family keeps one
entry per runner architecture, keyed on the runner's architecture and on the Go version its
job's setup-go installed: a build cache holds the objects of the architecture that compiled
them, so an entry an x86_64 runner filled would give a job on ubuntu-24.04-arm its modules and
none of its objects, so each family is filled on every runner one of its jobs runs on.
go-notices puts the architecture before the OS in its key and the other families put it after:
its entry holds go-licenses, a program lint's notices step runs, and a run of a workflow file
older than the architecture in the key restores by go-notices-<os>-, which an <os>-<arch> key
starts with (ci-job-lint.yaml's restore step says what that did). An entry is
refreshed only when Go is bumped. A go.sum change downloads only the changed modules, and Go's
build cache is content-addressed, so a stale entry costs a run only what changed; a key on the
go.sum files left a whole entry, up to a gigabyte, per change for seven days. go-envoy-infra is
the exception: what changed under it in eleven days cost its linux vet 83 to 85% of what the
same vet for darwin took with nothing cached, so its key carries the hash of its module files
too, and ci-caches.yaml deletes the entry each fill replaces (its envoy-infra job says how). The
fallback by
prefix, when the exact key misses, now applies on every run, a dispatched one on main
included: it was held off main so that the entry a run saved after a Go bump held nothing the
older Go built, and no step of this workflow saves a family any more (the two check actions
below fall back only to an entry of their own Go version, so an entry one saves holds nothing an
older Go built). There is no browser cache beside the Go
families any more: dispatch-browser.yaml's shard and alone jobs run in the image this
repository builds, which carries Chromium, WebKit and Firefox at /ms-playwright, so a
~/.cache/ms-playwright restore would carry nothing they do not already have, and
ci-caches.yaml's browsers job, the only writer of that entry, is gone with the restores
(dispatch://INFRA-1443). setup-bun reads and writes its cache only on main
(.github/actions/setup-bun), which of this workflow's events is the dispatched run alone, and
not in the tool-fallback and tool-fallback-arm jobs at all: that action reads the id of the
job that called it and passes no-cache for those two on every ref, so a restored Bun cannot
stand in for the install each of their cases is a reading of. A seventh family,
`architecture-check-<os>-<arch>-go<version>-<hash>`, is the composite action
.github/actions/architecture-check's own, and an eighth,
`spec-check-<os>-<arch>-go<version>-<hash>`, is .github/actions/spec-check's; envoy-go-checks runs
both. Each action restores its family and saves it itself, from $RUNNER_TEMP/architecture-check
or $RUNNER_TEMP/spec-check and not from the Go caches above, keyed on the Go version its own
setup-go installed and on the SHA-256 of packages/envoy/go.sum (a miss restores the newest entry
of that Go version), and the ci-caches.yaml job of the action's name fills it on main. Each
action saves only in a push, schedule or workflow_dispatch run, so within this workflow, which
runs on no push and no schedule, only a dispatched run can save it (the bypass check dispatches
one at main), only on a miss of the exact key, and into the scope of the ref it was dispatched
on. check-ci-filters.sh's cache-family rule reads a workflow's own actions/cache steps and not a
composite action's, so this paragraph is the one record of the two actions' restores and saves.

Runners: a job of this workflow, or of a workflow it calls, runs on ubuntu-24.04-arm unless it
needs an x86_64 machine, and runs on ubuntu-24.04 then. The two pools queue apart, and both
queue. On 2026-11-16 a job of this repository waited 456 s for an ubuntu-24.04 runner at the
median and 678 s at the 90th percentile, and the jobs every run waits on twice (`changes` at
the start, and `ci-ok` after the last job ends) paid that x86_64 wait both times. On the run
that first moved jobs here (run 44069656523, 2026-11-17), the 24 arm64 jobs waited 217 s at the
median and 542 s at most, the 19 x86_64 ones 890 s and 1428 s: an arm64 job that starts with
twenty others still waits for its turn, but the gating jobs started in 7 to 23 s (`lint` 7 s,
`ci-ok` 14 s, `changes` 17 s, `pr-title` 23 s). What keeps a job on
ubuntu-24.04, each a reason of its own machine and not of the job's code:
  * a tool with no arm64 row in .github/ci-tools.yaml: the Pulumi CLI, its docker plugin and
    uv, which only x86_64 jobs install (envoy-smoke, daemon-go, tool-fallback). A tool that has
    one is installed from its `-aarch64` row there, as changie (test), crane (ci-images), mise
    and jj (pi-shared) and elan (airlock-lean) are;
  * the pinned Oh My Pi build, which .omp-pin names and whose release ships linux-x64 alone:
    daemon-go, plugin-pack, pi-envoy and pi-legion;
  * a browser: dispatch-browser (its container is an image built for linux/amd64 alone),
    dispatch-two-builds and docs (Playwright's Chromium on the runner's own libraries);
  * an image of this repository built for linux/amd64: worker-image, browser-image, and
    envoy-smoke;
  * envoy-go-checks, whose pack step builds the release's listener archives with no
    CGO_ENABLED, as release-envoy-listener.yaml does on ubuntu-24.04: Go's default turns cgo on
    for a native build and off for a cross build, so only an x86_64 runner builds the release's
    own pair (ci-job-envoy-go-checks.yaml says what the arm64 runner built instead);
  * tool-fallback, which is the x86_64 half of the tool proof; tool-fallback-arm is its arm64
    half.
Every Go cache family carries the runner's architecture in its key, as the paragraph above
says, so a job moved from one runner to the other reads a cold entry until
.github/workflows/ci-caches.yaml fills its family on the runner it moved to.

## Why the router runs on these events and no others

No `paths:`: every change starts this workflow, and `changes` selects what runs. No `branches:`
on pull_request: a pull request on any base is tested. No `edited`: a title or body edit starts
no run (pr-title.yaml checks those). `ready_for_review` starts the run a draft skipped. No
`closed`: tests-closed.yaml cancels a closed pull request's run
(.github/ci-notes/ci.md#what-a-newer-push-cancels-and-what-is-never-cancelled).
merge_group runs the router on the merge queue's commit of each queued group: `changes` selects
by what the queued change touches against the queue base (the group's base_sha..head_sha), and
ci-ok reports there, so the required checks pass on the merged tree before anything lands. No
`push`: the queue tests every change on the tree it lands as, so a run on main would only test
that tree again, and .github/workflows/ci-caches.yaml fills the Go caches on that push instead.
workflow_dispatch is the bypass check's (.github/workflows/main-bypass-check.yaml): it starts a
run at main's tip when a commit reached main without a passing merge-group ci-ok, and `changes`
selects every job there.

## The commit main pointed at before the push the bypass check read

The commit main pointed at before the push the bypass check read: envoy-go-checks compares
every migration it holds with this run's tree, the range the push landed. A dispatch on
main without it fails that check, which cannot tell what the run lands. A dispatch on
another branch without it, a full run started by hand, compares with the branch's merge
base with main (.github/scripts/edited-migrations-base.sh).

## What a newer push cancels, and what is never cancelled

A newer push to a pull request cancels its older run, waiting or running. On pull_request the key
is the pull request's number: tests-closed.yaml joins this same group when the pull request
closes, cancelling what still waits or runs, and a merged pull request's closed event carries
github.ref = the base branch, so the number is the only key that names the pull request on every
event. A reopen within about a minute of the close can race tests-closed.yaml's run:
tests-closed.yaml says what happens and what to do. A merge group is a group of its own per
commit (its head) and is never cancelled: the queue waits for its ci-ok. A dispatched run keys on
the commit it runs at and is never cancelled either.

## Why the router's permissions block holds those two scopes

Read-only but for the two scopes the ci-ok steps use: `actions: read` reads the run's jobs,
which the ci-ok-report step names in the check it posts, and `checks: write` posts ci-ok (the
ci-ok job's reconcile step reads and PATCHes check runs with it too). The token also clones,
downloads pinned tools, and, in `changes`, reads the pull
request's file list, which this public repository serves without `pull-requests: read`. The
jobs whose scopes differ declare them in blocks of their own: pr-title (what check-pr-text.sh
and find-pr-payload.sh read, and neither of the two above, since it posts nothing), ci-images
(the ghcr.io push its copy makes) and worker-image (the image
push and its attestation's scopes). docs-pages.yaml publishes the site to GitHub Pages.

## What CI_OK_SELECTED_BY holds, and which programs read it

ci-ok's map of every job but those CI_OK_ALWAYS_RUN names to the `changes` output its `if:`
tests, and
the list of those jobs whose `if:` also admits only pull_request (ci-ok.sh and ci-ok.jq say how
they decide a job's selection). The ci-ok job's step takes both from this env; ci-ok-report.sh,
which runs from a composite action this file hands its job's own inputs alone, reads both out
of this file at the commit the run checked out. check-ci-filters.sh holds both to the jobs'
`if:`s.

## The jobs that run on every change

The jobs that run on every change: no `changes` output gates one, so CI_OK_SELECTED_BY maps
none of them, and ci-ok passes only when each of them succeeded — a skipped, cancelled or
absent one fails the verdict, since nothing else says the job ran. `changes` is here because
it selects every other job, `lint` because every change is linted. ci-ok.sh reads this list
rather than knowing those two names, and check-ci-filters.sh holds every job of ci.yaml to
exactly one of ci-ok's three classes: mapped by CI_OK_SELECTED_BY, named by ci.yaml's
CI_OK_ALWAYS_RUN, or named by ci.yaml's CI_OK_EXEMPT. The list is a JSON array written over
lines rather than one long one, as ci.yaml's CI_OK_SELECTED_BY, a JSON object, is.

## The jobs outside ci-ok's accounting

The jobs outside ci-ok's accounting, each for a reason of its own. `cancel-started` exists
only to stop a merge-group run the queue has already thrown away, so it runs only on
`merge_group`, holds `actions: write`, runs no dependency code, reports no result to ci-ok
and is no job any verdict waits for. `ci-ok` is the verdict: it reports nothing to itself,
and the jobs it judges are its `needs:`, which check-ci-filters.sh holds to every job of this
file but these three. `pr-title` is the other check main's ruleset requires, carried by that
job's own check run: its result is nothing the verdict waits for, it carries no report step,
and the queue reads it as a check of its own, so a verdict that needed it would only make one
required check wait for the other.
That check asks none of the ci-ok rules of a job named here, and refuses
one that carries the ci-ok-report step: ci-ok.sh fails a result for a job CI_OK_SELECTED_BY
cannot map. The list is a JSON array written over lines rather than one long one, as ci.yaml's
CI_OK_SELECTED_BY, a JSON object, is.

## Why changes, pr-title, cancel-started and ci-ok run on arm64

changes, pr-title, cancel-started and ci-ok run on arm64: each runs JavaScript actions and
shell scripts over the image's own gh, jq, git and coreutils, and builds or fetches no binary,
so the architecture reaches nothing they compute; at busy hours the x64 pool queues a job for
about 15 minutes where the arm64 pool starts one in seconds (INFRA-4222).

## What the changes job selects, and the two terms every job carries

Selects the jobs that read what this change touched. Skipped on a draft pull request, so no
test job runs; marking it ready for review starts a run that selects by the same diff.

Also skipped on a re-run attempt of a merge-group run, by a term every job of this file but
the two named for a required check — `ci-ok` and `pr-title`, each of which refuses such an
attempt in a step instead — carries:
`(github.event_name != 'merge_group' || github.run_attempt == '1')` in
a job that runs on more than one event, and `github.run_attempt == '1'` in
`cancel-started`, which runs on merge_group alone. No job may lean on this one's skip
instead: `gh run rerun --failed` leaves out a `changes` that succeeded and hands the attempt
that job's outputs again, so an `if:` testing only `needs.changes.outputs.<x> == 'true'`
would run the job again in full, and its own report step would post a failing ci-ok on the
dead group's head the moment it failed. .github/scripts/check-ci-filters.sh holds every such
job to its term. A group is tested once: the queue tests a pull request it removed again
when it is added again, and a merge-group run `cancel-started` or
.github/workflows/dequeued.yaml force-cancelled tests a group the queue has thrown away, so
a passing re-run of it would put a ci-ok on a head no queue entry waits for. A re-run must
therefore test nothing and post nothing; the jobs of it that do run are `ci-ok` and
`pr-title`, neither of which can be skipped without leaving a passing required check behind,
and each concludes the failure such an attempt must get in a step instead
(.github/ci-notes/ci.md#why-a-re-run-attempt-of-a-merge-group-run-is-refused-in-a-step and
.github/ci-notes/ci.md#why-pr-title-refuses-a-re-run-attempt-in-a-step-of-its-own say why).

## Why the daemon's Go suite has an output of its own

The daemon's Go suite, a job of its own since dispatch://INFRA-1413: the test job used
to carry it behind the `test` output, so a change to a shell script ran it too.

## Why the plugin pack has an output of its own

The one test of that suite that packs the two plugin releases, in a job of its own: its
pack reads bun.lock, so with it in daemon-go a lockfile change ran the daemon's whole Go
suite (dispatch://INFRA-1413, A4).

## Why the private-name port harness has an output of its own

The private-name port harness, in a job of its own: it builds the `legion` binary of
this head, so as a step of the test job it put that build's whole closure on the test
filter (dispatch://INFRA-1413).

## Why the session message-wait suite has an output of its own

The session message-wait suite, in a job of its own: it is the one test of the test job
that needs a database, and a Postgres service there would start one for every change
that selects any of that job's thirty-two harness steps.

## One output per test.sh step of the test job

One output per `*.test.sh` step of the test job: each names that harness and the script
it tests, so the step runs when either changes and skips when neither does. The job's
own filter names every one of these lists, since a step gate decides nothing in a job
that was not selected.

## Why the browser image's build carries no full-ci term

The browser image's build. No `full-ci` term, and none is wanted: the image is what the
shard and alone jobs of .github/workflows/dispatch-browser.yaml pin, so gating its build behind a label would leave a pull request that edits
the Dockerfile writing a row whose digest the package does not hold, which the
ci-images job then refuses.

## Why the browser suite is ungated, and what the label decides

The browser suite, ungated: a pull request that changes what it reads runs it, because
the dispatch-browser job then runs the cheap `pull-request` lane, the two projects
a pull request is held to. The `full-ci` label no longer decides whether this suite runs,
only which lane it runs: that job's `lane:` input reads the label, and
.github/ci-notes/ci.md#what-the-lane-input-reads says what each event gets.

## The ten slow suites, whose runner time dwarfs every other job's. The

The ten slow suites, whose runner time dwarfs every other job's. The merge queue runs
them on the tree the change lands as, and that is where their result normally comes from;
a pull request runs them only when it carries the label `full-ci`. Each is false on a
pull-request run without that label, so the skip is one `changes` chose: ci-ok reads it as
it reads any other output, and no job's `if:` knows about the label. The label is read
from the pull request the run's own event carries, so it takes effect at the next push,
reopen or ready-for-review, not when it is added: this workflow takes no `labeled`
trigger, which would start a run that joins this workflow's concurrency group and cancels the
pull request's run in progress. Take the label off once the run it was added for passes,
since every later push would run the slow suites again, unless a `ci_check` of the issue
names one of them, which reads `skipped` on a pull request without the label.

## Why envoy-go's fast half comes back on every push

envoy-go's fast half (the format and generated-code checks, the shell-script tests and
the build of the envoy commands), split out so it comes back on every push: no `full-ci`
term, only the dispatched one, `github.event_name == 'workflow_dispatch' && 'true'`.

## The two-build job builds the previous release and runs both halves against

The two-build job builds the previous release and runs both halves against it (the Go
suite and the cross-build browser spec), which is the longest run of them all, so it is
gated like the other slow suites.

## The two-build job's Go step alone, behind a gate of its own

The two-build job's Go step alone, behind a gate of its own (ci-job-dispatch-two-builds.yaml
says why). The dispatch_two_builds output decides whether the job runs at all, the `full-ci` term with
it, so this one carries only the dispatched term.

## Why the tool fallback proof carries no full-ci term

The second source of every tool install, proved by driving it: no `full-ci` term, so the
proof comes back on every change that selects it. It selects the arm64 half of that
proof too, which drives the same action over the same manifest through the same helper
and so reads the same files.

## The paths filter's own verdict for every filter, before the `full-ci` term

The paths filter's own verdict for every filter, before the `full-ci` term (`(github.event_name != 'pull_request' || contains(github.event.pull_request.labels.*.name, 'full-ci'))`) rewrites
the nine outputs the ten slow suites run behind. On a pull request without that
label each of those nine is `false` whether its paths matched or not, so a reader of the
outputs alone cannot tell "the filter said no" from "the label is absent"; with this
beside them ci-ok's report tells the two apart and says which
(.github/scripts/ci-ok.jq's `selection`). It gates no job and needs no plumbing of its
own: `toJSON(needs.changes)` carries every output of this job, so the ci-ok job and
ci-ok-report.sh both already have it. It is the one filter step's outputs as one object.
On a dispatched run that step is skipped and this is `{}`, which is right - the label
holds nothing back there, and the changes job's other outputs are all 'true'.

## On pull_request dorny/paths-filter reads the file list from the API and reads

On pull_request dorny/paths-filter reads the file list from the API and reads only the
filters from the tree; merge_group diffs with git and needs the clone (a merge group diffs its
base_sha..head_sha, fetching what the clone lacks). A dispatched run clones too, and
filters nothing.

## On a pull request this job reads the changed files from the

On a pull request this job reads the changed files from the API and the filters from
.github/ci-filters.yaml, which dorny/paths-filter reads off the disk: so a pull
request checks out that one file, and nothing else.

## Why the filter step is skipped on a dispatched run

A dispatched run selects every job through the changes job's outputs, so there is nothing to
filter, and paths-filter has no diff base on workflow_dispatch: a step that failed here
would fail `changes`, whose result every job's `if:` reads.

## Why the lint job holds each of its scopes

The ci-ok-report step is the whole of this job's GitHub writing: it posts a failing ci-ok
with `checks: write` and names the run's jobs in it with `actions: read`. `contents: read`
is the checkout's. `pull-requests: read` is the daemon API contract check's: when it is
about to refuse a change it reads every open pull request of the repository, with the paths
each changes, to name a number no other one has taken. The pull request's text is the
`pr-title` job's, which declares the scopes its own routes read.

## What the ci-images job copies, and why the order is the point

Copies every row .github/ci-images.yaml names into ghcr.io/cybermonklabs/legion/ci, then reads the
package to check that it holds every row. The order is the point: the read fails a row the
package does not hold, so the copy for this change runs first. The ci_images filter selects
this job, and names the files that decide what its steps do: the rows, the scripts, the
module every reader of an image row goes through, the actions and this file. The gate's
other half reads the whole tree, and is a step of lint, which runs on every change: a file
that names a third-party registry again can be any file, and this filter names few of them.
This job is the one copy path: it runs on a pull request and on a merge group, which is every
event that can bring a new row in, and a failed copy is re-run with `gh run rerun`.
The copy of every tool asset is filled here too, in the step after the image half
(.github/scripts/mirror-ci-tools.sh): it is the same package, under the same login, and the
tool-fallback and tool-fallback-arm jobs read what this step filled, so that copy must
land before them.

## Why the ci-images job holds packages write

packages: write is the ghcr.io push, which is `crane copy` for an image row and the
registry API call .github/scripts/mirror-ci-tools.sh makes for a tool row; actions: read
and checks: write are the ci-ok-report step's.

## The tool fallback proof: what it drives, what it only reasons about, and how to read it when it is red

The live proof of the second source every tool install of this repository now has: it faults
the host the first source is on, installs all eight tools through the source
.github/ci-tools.yaml names for each, and then serves sixteen corrupted payloads and reads
the refusal of every one of them.
It is permanent rather than a scaffold. A second source nothing exercises rots — a copy
nothing fills, a seam a refactor dropped, a digest nobody reads — and the hour of 504s this
work answers is the hour nobody wants to learn that in (dispatch://INFRA-1873). This
repository's own principle is that testing means booting and running it, so what says the
fallback works on the day the assets are down is a job that takes them down and installs
anyway.

Once the fault is up, no step of this job needs a GitHub release host to answer.
Every install that must succeed reads a source the fault does not hold — the ghcr copy, npm,
PyPI or get.pulumi.com — or, for the Pulumi CLI, whose primary is get.pulumi.com and whose
second source is the release asset, the origin serving that release's own staged bytes; the
bytes the corrupted cases serve were staged before the fault; and the one case about a
binary a third party installed seeds mise's install directory from that staged payload
rather than downloading one inside the fault.
That is what keeps ci-ok green during the very outage this job proves survivable: this job
is in ci-ok's `needs:`, and its filter selects ci.yaml, .bun-version, the worker image's
Dockerfile, the manifest and the helper beside it, and the eight actions, so
a step of it that waited on a release host would turn every pull request touching one of
those files red on exactly the day the fallback is doing its work.
.github/scripts/ci-tools-proof.sh is the rig, which this job and tool-fallback-arm drive;
the test job runs its ladder-report verb as well, over logs of its own.
It needs ci-images for order, since that job fills the copy this proof reads, and carries
`!cancelled()` so a run that selected this job and not that one still runs: the ci_images
filter names what the ci-images job reads and nothing more, so it is no superset of this
job's, and a skipped need would otherwise skip a job ci-ok has as selected. The file that
can bring a new row in, .github/ci-tools.yaml, is in both filters, so the fill runs before
this proof on every change that adds one.

WHAT THIS JOB DRIVES, AND WHAT IT ONLY REASONS ABOUT.
Everything a case is read on is driven here: the fault, the origin that stands in for all
four sources, the corrupted payloads, and the removals each case makes before it.
One thing is not. oven-sh/setup-bun, which .github/actions/setup-bun's second step pins,
restores a cached Bun into ~/.bun/bin/bun inside a step of its own, and the Actions cache it
restores from is on hosts this job's fault does not hold.
.bun-version holds an exact version, so that action builds its key and its URL without
asking any host which version to take, and nothing the fault refuses stands in its way.
Taking a bun away before that step is therefore no answer to the cache, since the step would
put a restored one back.
What keeps every case here a reading of an install is the wrapper's own `no-cache`, true for
every job whose id starts with `tool-fallback` — `github.job` in a composite action is the
id of the job that called it — so no call of that action in this job can be served by the
Actions cache, on any event and any ref.
That is read from the wrapper's expression rather than from a run of this job: a run that
told the action not to read the cache cannot measure what the cache would have served.
The removals the cases do make are about this job's own state: a case that must read the
second source takes ~/.bun/bin/bun and the bunx beside it away before its install, so what
it reads is not the bun an earlier case left.

READING THIS JOB WHEN IT IS RED AND HAS NO LOG AT ALL.
A job whose log GitHub answers `HTTP 404` with `<Code>BlobNotFound</Code>` for did not hang
and was not cancelled: it lost its runner. The log is written when the runner finalises it,
and a runner the service stops hearing from never finalises, while a job that was cancelled
keeps its log. What outlives the log is the check run's annotation, which the service
writes:
  gh api `repos/<owner>/<repo>/check-runs/<job id>/annotations`
On 2026-11-10 that answered, for this job: "The hosted runner lost communication with the
server. Anything in your workflow that terminates the runner process, starves it for
CPU/Memory, or blocks its network access can cause this error."
Read the job record beside it: a lost runner leaves one step `in_progress` and every step
after it `pending`, and the job ends before its own `timeout-minutes` would have fired.
Three wrong causes were offered for that one job before anyone ran that command
(dispatch://INFRA-2605). `docs/testing.md` says the same for any job of this repository.

BEFORE YOU PUT A BOUND ON A STEP OF THIS JOB: A BOUND BELONGS ONLY WHERE FIRING IT IS LOUD.
A step the runner ends at its `timeout-minutes` leaves the same `outcome` behind it as a
step that failed on its own, and GitHub gives a later step nothing that tells the two apart.
That is measured and not assumed: on this change's own run a step with `continue-on-error`
and `timeout-minutes: 1` whose action slept 180 s was ended after 72 s, and the step after
it read it as `outcome='failure' conclusion='success'`, so the failure is in the value an
assertion reads while the job's own result says nothing happened.
So on a step with no `continue-on-error` a bound is loud: it fails the job. This job's steps
are in .github/workflows/ci-job-tool-fallback.yaml, and on one of the twenty-four installs
there whose control passes a step whose `outcome` is `failure`, a bound is silent: a hang
would satisfy that control and leave the job green, where the job's own ceiling would have
cancelled it and failed ci-ok. That is why each of those installs is timed as well as read -
the eight with nowhere to go by the control step "Check all eight installs with nowhere to go failed, and failed in their own time", the sixteen corrupted cases by
`report` itself - and why a new bound on a step some later assertion accepts the failure of
needs the same treatment or none at all (dispatch://INFRA-2605).

## Why the tool-fallback job needs no packages scope

contents: read is the two checkouts'; actions: read and checks: write are the
ci-ok-report step's. Every read of the package here is the anonymous one an install makes,
so this job needs no packages scope at all.

## The arm64 half of the tool fallback proof

The same two halves of the Bun install, proved again on an arm64 runner.
.github/actions/setup-bun's third step selects a row from `uname -m` — `bun-linux-aarch64`
here — and reads .github/ci-tools.yaml through .github/scripts/ci-tools.sh, which needs
python3 with PyYAML. Every job of this file that runs on ubuntu-24.04-arm and installs Bun
makes that same read, so a runner image without PyYAML fails all of them; what this job adds is
the second half, the install from npm when the release asset serves nothing, which no other
arm64 job drives and which the arm64 leg of .github/workflows/release-envoy-listener.yaml would
otherwise meet first, on a push to main and so only after a merge.
It proves both halves: the healthy path, where the release asset serves Bun and the action's
fourth step holds it to the aarch64 row, and the npm path, under the fault the tool-fallback
job uses, where the release hosts answer nothing and the second source serves the same
binary. Each half ends in a reading of what this runner holds, printed for a person reading
the run: the machine, the version, the path, the row's own pins, and what ci_tools_verify
reads that binary as against the row.
The npm half says which source served it as well, because neither the version nor the
digest can: the release asset and the npm package are one binary by design, so a run where
the release host answered after all would read exactly as a run where npm served. What says
it is the fault proxy's own 504 for github.com after the probe, the refusal that sent the
install to its second source. The `ci-tools:` line the install wrote would say it too, and
no step can read it: a composite action writes that line to the job's log, which no later
step reads.
What .github/ci-notes/ci-job-tool-fallback.md#the-steps-of-ciyamls-tool-fallback-job-in-a-file-of-their-own
says of the Actions cache holds here too: the wrapper passes
no-cache for every job whose id starts with `tool-fallback`, so no call of it in this job
can be served by the cache either, on any event and any ref.
The npm half takes ~/.bun/bin away before its install all the same, so no Bun the healthy
half left behind satisfies it.
The `tool_fallback` filter selects this job, as it selects that one: both drive the same
action over the same manifest through the same helper, so one filter decides both and
neither can drift from the other. CI_OK_SELECTED_BY maps this job to that filter, and
.github/scripts/ci_job_reads.py holds a gate of its own on it for what this job reads.
After the Bun halves it proves the other arm64 rows the same way: crane-aarch64,
changie-aarch64, mise-aarch64, jj-aarch64 and elan-aarch64, each installed once from its
release asset and once under the fault, where each must read its ghcr copy and each install's
own 504 is read after a probe of its own (the called file's comments say how). Those faulted
installs read the copies the ci-images job fills, so this job needs that job for order and
carries `!cancelled()`, as tool-fallback does and for the reason #the-tool-fallback-proof-what-it-drives-what-it-only-reasons-about-and-how-to-read-it-when-it-is-red gives.

## Why the tool-fallback-arm job needs no other scope

contents: read is the checkout's; actions: read and checks: write are the ci-ok-report
step's. Both sources serve this job's bytes without a credential, so it needs no other
scope.

## The fail-open jobs, and what they buy while changes is broken

lint, typecheck, test, daemon-go, plugin-pack, private-names-port, sessions-script and
text-checks are jobs ci-ok condenses; main's ruleset requires none of their names. Fail-open:
when `changes` did not succeed, all of them but lint, which runs on every change anyway, run
rather than skip. ci-ok fails then anyway (ci-ok.sh), so this buys signal: their results while
`changes` is broken. The set is main's fail-open typecheck and test, and every job that took a
suite from that test job: its daemon Go tests are daemon-go's, the tests of ci_job_reads.py's
PLUGIN_PACK_TESTS are plugin-pack's, the private-name port harness is private-names-port's, the
daemon's text checks are text-checks' and `bun test scripts/sessions` is sessions-script's, so
each of those carries the term too and a suite that moves out of a fail-open job takes it with
it. A re-run of only the failed jobs can leave the jobs `changes` then selects skipped, and
ci-ok fails a selected job that skipped rather than passing with nothing tested: re-run every
job instead.

## What the test job runs, and how each harness step is gated

The script harnesses, the Claude Code plugin build and the Bun suites of scripts/. Each
`*.test.sh` step runs behind a `changes` output of its own naming that harness and the
script it tests, handed down as an input of the same name
(.github/workflows/ci-job-test.yaml), so a change to one script runs one harness. The job's
own filter names every one of those lists, since a job's `if:` tests one output and a step
gate decides nothing in a job that was not selected.

## Why the daemon's Go suite is a job of its own

The Go daemon's own suite: the plain step, the race step and the Stage 1 proof, with the
Postgres, the tools and the cache they need. It left the test job in dispatch://INFRA-1413,
where one filter had to cover both: a change to a shell script, to packages/claude-envoy or
to bun.lock ran 1,083 s (median) of Go tests that could not see it. Fail-open with typecheck
and test, for the reason #the-fail-open-jobs-and-what-they-buy-while-changes-is-broken gives.

## Why the three plugin pack tests are a job of their own

The three tests of the daemon's Go suite that .github/scripts/ci_job_reads.py's
PLUGIN_PACK_TESTS names: the one that packs the two plugin releases and loads them, and the
two prompt probes of agentmodels_omp_test.go. They left daemon-go in
dispatch://INFRA-1413: the pack reads bun.lock and the two plugin packages, and the
agent-model probe stages skills/, so that filter named the lockfile and skills/, and a change
to either ran the daemon's whole Go suite. Here they run on their own inputs, and the
daemon-go race step skips them by name (.github/scripts/ci_job_reads.py's PLUGIN_PACK_TESTS
builds both steps' text).

## The private-names-port job, and the closure it took off the test filter

The harness that holds this repository's four private-name rules, as the `legion` binary of
this head runs them, to the frozen shell script's verdicts case for case
(.github/scripts/check-private-names-port.test.sh). It left the test job in
dispatch://INFRA-1413: the harness builds ./cmd/legion, so that job's filter had to name the
whole closure of that build, and a change to any package of the daemon ran thirty-odd shell
harnesses and the Bun suites of scripts/ that could not see it. Fail-open with typecheck,
test, daemon-go and plugin-pack, for the reason #the-fail-open-jobs-and-what-they-buy-while-changes-is-broken gives, and as this step
ran while it was one of the test job's.

## What the docs job builds, and why it declares no timeout-minutes

Builds the documentation site (docs/site) on every pull request and merge group that could
change it. No run of this workflow is a push to main: docs-pages.yaml builds and publishes
main's site to GitHub Pages, at the address .github/scripts/repo-paths.sh derives from the
repository it runs in (https://cybermonklabs.github.io/legion/ today), one push at a time in push
order. Both callers run the steps of docs-site.yaml, which holds them once. The site's
reference pages are generated from packages/ and skills/ at build time,
scripts/third-party-notices.ts writes the THIRD_PARTY_NOTICES.txt it publishes, and the built
site is checked with the private-name guard; the docs filter names all of them, docs-site.yaml
among them.

No timeout-minutes: GitHub's keywords for a job that calls a reusable workflow leave it out.
docs-site.yaml's own job carries the limit.

## Why the docs job declares the scopes the called workflow needs

A calling job that declares no `permissions:` hands the called workflow the default
GITHUB_TOKEN permissions, not this file's block (GitHub's reusable-workflow reference,
"Supported keywords for jobs that call a reusable workflow"), so the scopes docs-site.yaml
needs are declared here. `contents: read` is its checkout's; `actions: read` and
`checks: write` are its last step's, the ci-ok report this call turns on.

## Why the session message-wait suite carries a database of its own

The session message-wait suite, which creates a session-store database of its own and
refuses without one. It was a step of the test job until dispatch://INFRA-1413: the one step
of that job that needs a database, which is why it has one here instead of a Postgres
service on a job whose other thirty-odd steps would start it for nothing.

## The dashboard's and the proof editor's lint, typecheck, unit tests, build and

The dashboard's and the proof editor's lint, typecheck, unit tests, build and asset checks.
They were split out of the browser suite (the dispatch-browser job), whose full lane - the one
the merge queue, the nightly and a pull request labelled `full-ci` run - is as slow as the
ten suites that label holds back, while these checks must come back on every push.
No Postgres service and no Go, but contracts is built
before the suites: one case of the dashboard's test run spawns Playwright, whose own
resolution reads contracts' built dist/ (the comment on the step "Build contracts for the
suites that spawn Playwright" of .github/workflows/ci-job-dispatch-ui.yaml says what failed
without it). tsc and
bun read @legion/contracts through the workspace's "bun" export condition and the dashboard's
Vite build aliases its sources, so that spawn is the whole of what the build is for.

## The browser suite job: its lanes, its one input, and why the matrix is in the called file

Every browser row of the Dispatch suite, which .github/workflows/dispatch-browser.yaml
holds: the binaries job that builds the Go commands the lanes run, the shard legs, the three
`-alone` projects a machine each, the two-process job, the fake-embedder job that runs on both
lanes, the containerised visual review, and the merge job that joins the shard and alone
reports into one and posts this job's ci-ok report.

Two lanes, and which run pays for which. A pull request without the label `full-ci` runs
`pull-request`, the two projects a change is held to (chromium and iphone), so such a run
proves those two rows and no more - it is not the run that proves the suite before main.
Every other run - the merge group, a workflow_dispatch and a pull request carrying `full-ci`
- runs `full`: every project, the three `-alone` ones and the two-process job. So the merge
group is where a change meets the whole suite before it lands, and the nightly
(.github/workflows/dispatch-browser-nightly.yaml, which calls the same file with the full
lane, from main) runs it again on main's tip.

One input decides that: the called workflow reads `lane` for the projects each job runs, so
nothing here chooses them a second time. It picks no browser to install any more: the shard
and alone jobs run in the image this repository builds, which carries all three engines
(dispatch://INFRA-1443), and the two-process job installs Chromium whatever the lane.

The matrix is in the called workflow and cannot be here. Every leg of a matrix shares one
job id, and .github/actions/ci-ok-report posts the failing ci-ok of a merge-group run from
the job whose own result decides that run, as that job's last step: legs written here would
each be `dispatch-browser`, so a decided group would take one such post per leg that failed,
each posting under a job id several legs answer to. A called workflow's legs are jobs of
that workflow, and one job of it - the merge job, which needs every other - posts the single
report this job id gets.

No timeout-minutes: GitHub's keywords for a job that calls a reusable workflow leave it out.
Each job of dispatch-browser.yaml carries its own limit.

## Why the dispatch-browser job declares the scopes the called workflow needs

The same rule as the docs job's: a caller that declares no `permissions:` hands the
called workflow the default GITHUB_TOKEN permissions rather than this file's block.
`contents: read` is every job of dispatch-browser.yaml's checkout; `actions: read` is the
merge job's `gh api .../actions/runs/<id>/jobs` read of this run's jobs, and `checks: write`
that job's last step, the ci-ok report.

## What the lane input reads

An unlabelled pull request gets the two-project `pull-request` lane; a pull request
carrying `full-ci`, and every other event, gets the full lane. That is what lets a worker
recheck a slow-suite failure on a pull request: the label picks the lane, while the
`dispatch_browser` output of `changes` runs the suite either way.

## envoy-go's fast half: the Go format check, the two generated-code checks, the

envoy-go's fast half: the Go format check, the two generated-code checks, the shell-script
tests, the vet of the module as darwin compiles it, and the build and pack of
the envoy commands. It also runs the tests of internal/dispatch/architecture/notes,
internal/runnerlog, internal/dispatch/specgraph, cmd/architecture-check and cmd/spec-check,
whose one run this is (envoy-go's unit step leaves
them out), the architecture-note check over this repository's own .dispatch/architecture
through the composite action .github/actions/architecture-check, and the spec-file check over
its .dispatch/spec through the composite action .github/actions/spec-check, both of which
other repositories can call. The slow half, the envoy-go job (the module's unit tests, the
container-backed NATS matrix and the integration suite), is one of the ten slow suites,
which run in the merge queue and on a pull request labelled `full-ci`, while these checks
must come back on every push. No Postgres service: nothing here opens a database. It
restores go-envoy rather than go-envoy-test, the family of the slow half (the test binary of
every package of the envoy module) and of dispatch-browser.yaml's Go-building jobs. It also
runs the guard that no CI file of this repository compares the spec files with the approved
spec.

## What the envoy-go job runs

The slow half: the unit tests of every package of the envoy module, the container-backed NATS
matrix and the integration suite, all against this job's Postgres. Its format, generated-code,
shell-script, build and pack steps are envoy-go-checks'.

## The envoy-infra module, its own workspace and its live tests

The envoy-infra Pulumi plugin's module (packages/envoy/infra). It is not in the root go.work:
its own go.work binds it and packages/envoy, so Pulumi's requirements never move the versions
the Envoy binaries and the daemon build with, and every go command of ci-job-envoy-infra-go.yaml runs in that
workspace. Vet compiles the files behind the live-test tags (pulumilive, natslive) too, as
linux and as darwin compile them. The unit tests start no container; the natslive files run
in a step of their own against real
nats-server and Postgres, on testnats.Image, testnats.HubImage and testpg.Image, pulled first
for the reason .github/actions/pull-test-images gives. That step runs only the tests the tag
adds, one call per package: the "Unit test envoy-infra" step already ran every untagged test of core, seed
and gcp, so each call takes its -run from .github/scripts/go-tagged-tests.sh, which reads the
names off the source at run time. The gcp natslive tests pull no image:
they install helm, nats-server, the config reloader and the exporter with go install, at the
versions the clustered hub runs, and render the vendored chart in gcp/testdata. The per-target
compile runs in the lint job's notices step.

## The checks whose subject is text: each module's internal/textcheck package, the Dispatch

The checks whose subject is text: each module's internal/textcheck package, the Dispatch API's
nine static source checks, which read the module's own source, and the two pure pins of
packages/daemon's supervise table. None of them opens a database or starts a container, so the
job carries no service and no `bun install`, and the three Go lanes (daemon-go, envoy-go and envoy-infra-go) leave those
packages, those seventeen tests and those two pins out of their own runs
(.github/scripts/ci_job_reads.py builds both sides). Its filter names the trees those tests
walk, so it is selected often, which is why the job is kept this cheap: five `go test` calls —
the four that preceded the supervise pins take 4 minutes 4 seconds with an empty build cache,
and the pins' call adds about a quarter more cold time
(.github/workflows/ci-job-text-checks.yaml's header).

## The race lane: what it runs, why it is three jobs, and what is not in it

The race lane: delivery (its intake handler reads the routing settings the poll loop
publishes, from NATS's delivery goroutine), the Dispatch store (the pool's one-connection
guard, and the hold mark of its own each concurrent read takes), the bus (two connects on one
SOCKS5 dialer record each dial's error in one record and read it back,
TestConnectsSharingADialerRecordItsLastError), the listener, where the role claim watcher, the
session and interest watchers and the request handlers meet, with the role claim view, its
lock and its epoch in the registry store and the watchers' completeness and rescans in kvwatch
(INFRA-141), the session cache, whose reads take its shared lock beside watcher writes and the
expiry timer's callback as kvwatch's resume point is read while a replaced watcher still
applies, natscred, whose service keeper probes with the connection's options while
internal/bus may be setting its handlers (natscred/probe_race_test.go, which only the race
build compiles, so this lane is its one run), the NATS auth service
(internal/broker/natscallout), which starts a goroutine per request from nats.go's delivery
callback, counts those waiting for a decision slot and waits them out at Close, and the
Dispatch API and the document service - all under the race detector. A read
that walks a live document without its lock races the room's peers, which no ordinary run
shows: envoy-go's unit-test step runs without -race (LEGION-499). The lane sits beside
envoy-go because -race runs these packages several times slower, and each job carries a
package deadline of its own, since go test's ten-minute default is a whole package's.
envoy-go runs none of these packages plain any more (INFRA-1408): its unit step leaves out
exactly what the lane runs, and the one plain run it keeps of them is the step named "Test
what the race build cannot run at all or at full size", which runs the handful of
assertions the detector makes impossible or shrinks and nothing else.

The lane is three jobs: envoy-go-race (delivery, the Dispatch store, the bus, the listener,
the registry store, the session cache, kvwatch, natscred and the NATS auth service),
envoy-go-race-api and envoy-go-race-docs, whose notes, .github/ci-notes/ci.md#why-the-document-service-runs-alone-under-the-race-detector,
say why the document service runs alone.

api sits apart because isolation is what made it fast: sharing a runner, the race binaries
contended for the four vCPUs, and alone api fell from 1099.487 s to 746.718 s. The packages
left in envoy-go-race are cheap enough to share one: the two heaviest of those measured
there, the bus at 286.931 s and the listener at 365.787 s (run 40926978371), are a quarter
and a third of the lane's bar of 1080 s, which is 60 % of a 30-minute step's own timeout, so
a runner each would buy nothing and cost about 8,000 replayed runner minutes. Running a heavy
package apart also reads its seconds as its own job's, and leaves the others' numbers
standing when one is red.

pmdoc is not in this lane: its tests run plain only, in envoy-go's unit-test step
(INFRA-1408). It sat here because LEGION-571's flakes - pmdoc's own under -race and load, not
a race in production code - cost three review rounds when nothing ran the package under -race
before a PR claimed it. Its product code is still race-instrumented wherever a race job
compiles it, since envoy-go-race-docs imports pmdoc; what went is the second, race-built run
of pmdoc's own tests.

No job of this lane installs Bun: pmdoc's tests, which shell out to the generated TypeScript
fixtures under internal/dispatch/pmdoc/gen and refuse to run without bun in CI
(render_readback_test.go), were the only ones here that needed it, and they run in envoy-go's
unit step now, while the "Check pmdoc fixtures are up to date" step that keeps those fixtures
current is envoy-go-checks'. delivery's, the bus's, natscred's and the auth service's NATS
runs in the test process (internal/testnats), so no job here needs a container runtime of its
own.

Delivery, the Dispatch store, the bus, the listener, the registry store, the session cache,
kvwatch, natscred and the NATS auth service share this runner. The Dispatch store's tests
(store_test.go:28) and delivery's take their databases from storetest.Open
(storetest.go:64-66), which reads DISPATCH_TEST_DATABASE_URL and skips without it, and
cmd/listener/dispatch_resolution_test.go:158 gets its database through internal/testpg, so the
job carries the lane's Postgres service; the registry store itself is the NATS KV view, and
natscred's and the auth service's tests open no store, so they need none.

## The mention sender's claim model alone, on a runner of its own

The mention sender's claim model alone, on a runner of its own: its search takes every core
the runner has for as long as it runs, which beside envoy-go's other steps would put its
minutes on theirs and theirs on its deadline. Its rules' test opens the store, so the job
carries a Postgres service.

## Why the document service runs alone under the race detector

The document service alone, under the race detector, with a deadline of its own. It is by far
the slowest package of the race lane - 1773 s of the 1842 s the first step of envoy-go-race
spent on main at 14d907b6 (job 123461966609) - and sharing a 30-minute step with the other
five groups left it no headroom: that step died on go test's own deadline at job 123503246603.
Its own filter is the closure of this one package, not packages/envoy/**, so a change
elsewhere in the module does not start this job. No Bun: no test file here runs it, and no
job of this lane installs it. It runs under -race because a read that walks a live document
without its lock races the room's peers, which no ordinary run shows
(TestReadsOfALiveDocumentRunBesideItsPeers, internal/dispatch/docs/live_read_race_test.go:39,
LEGION-499).

## The cleanup detector run of the document service

The same package under Go's cleanup detector, which names what the memory of envoy-go-race-docs' race run went to:
GODEBUG=checkfinalizers=1 makes the runtime report a cleanup or a finalizer whose closure or
argument reaches the object it is attached to, a cycle that keeps that object alive for the
life of the process, and that is the defect that held this package's memory
(dispatch://INFRA-1085). This is the only package of the repository that registers a cleanup,
so no other job needs it. It runs without -race: the detector is the runtime's own
bookkeeping, and a plain binary leaves the runner room the race binary does not. It reads the
same closure as envoy-go-race-docs, so it reuses that filter and that `changes` output rather
than taking one of its own.

## The real-process memory tests, and why they are a job of their own

The real-process memory tests (packages/envoy/cmd/dispatch/memory_test.go, LEGION-481): each
row starts a Dispatch server, drives it over HTTP and the document websocket and reads its
resident memory from /proc, and together they run past go test's default ten-minute deadline,
which took envoy-go red at 600 s (run 37116639565). They build only under the `memory` tag, so
envoy-go's unit run leaves them out and this job runs them with a deadline of its own. It runs
only the tests the tag adds: `-tags memory ./cmd/dispatch` would run the package's untagged
tests a second time, envoy-go's unit step having run them already, so the -run comes from
.github/scripts/go-tagged-tests.sh, which reads the names off the source at run time. The
assignment is a line of its own: under bash -e it carries the script's exit status, so an
empty or refused list fails the step instead of running -run "", which matches every test.

## The Dispatch server's two-process tests

The two-process tests (packages/envoy/cmd/dispatch/multiprocess_test.go, INFRA-66): each row
starts two real envoy-dispatch processes against one Postgres and one NATS - which is what
production runs - and drives them over HTTP, their own logs and NATS's own consumer and
stream reads. They build only under the `multiprocess` tag, so envoy-go's unit run leaves
them out, and they are a job of their own for the reason dispatch-memory is: envoy-go's unit
step runs under go test's default ten-minute package deadline, which the memory tests
already took red at 600 s (run 37116639565), and one row here waits out a durable hand-over
or a maintenance loop's tick, tens of seconds each. Each row's NATS runs in its own test
process (internal/testnats), so the job needs no container runtime of its own. It runs only
the tests the tag adds: `-tags multiprocess ./cmd/dispatch` would run the package's untagged
tests a second time, envoy-go's unit step having run them already, so the -run comes from
.github/scripts/go-tagged-tests.sh, which reads the names off the source at run time. The
assignment is a line of its own: under bash -e it carries the script's exit status, so an
empty or refused list fails the step instead of running -run "", which matches every test.

## The two-build tests (packages/envoy/cmd/dispatch/twobuild_*_test.go): each row starts

The two-build tests (packages/envoy/cmd/dispatch/twobuild_*_test.go): each row starts
an envoy-dispatch of an earlier build and one of this tree against one Postgres and one NATS,
which is what a rolling release runs, and drives each build's CLI, stored documents, document
socket and bus traffic against the other (cmd/dispatch/README.md, "Two builds at once"). They
build only under the `twobuild` tag and are a job of their own for the reason
dispatch-multiprocess is: a row waits out durable hand-overs and settings polls, tens of seconds
each, and the earlier builds are built here first. Most rows run the previous build, the newest
legion-envoy-v* release in the history of origin/main; build-dispatch-build.sh resolves it,
which needs the whole history and the tags (fetch-depth 0), and builds it from `git archive`.
The first-rollout row runs the first-rollout build instead, the newest release before the first
that registers its schema numbers: the job pins it by its commit (FIRST_ROLLOUT_COMMIT), the
resolve step fails when its tag, legion-envoy-v30.20.2, names another commit, and the job builds
it the same way.
Each build is cached by its commit and the script that built it, so a pull request builds each
release once; its src/ is left out of the cache, since the test reads only the parts the script
copies out of it. Each row's NATS runs in its own test process (internal/testnats), so the job
needs no container runtime of its own. Then the browser half
(packages/dispatch/e2e/cross-build.multiprocess.e2e.ts), which runs twice: the browser suite's
two-process mode with Dispatch B run from the previous build's envoy-dispatch and web dist, then
from the first-rollout build's, and each direction's page served by one build against the other
build's server. Playwright installs as it does for dispatch-browser-multiprocess, and the rows
get a database of their own, since they truncate it. In a merge-group run the called file's
dispatch-two-builds-same job also runs the Go suite with this tree's own build as the previous
build, beside this job and not after it (that job's comment says why).

## The server-change soak

The server-change soak (packages/envoy/cmd/listener/server_change_test.go, INFRA-547): it
drives two real envoy-listener binaries over a three-server NATS cluster, with a session
stand-in moved between the servers by lame-duck shutdowns and forced kills and three senders
publishing through the listener, and holds every message to reaching its session or failing
its sender. It builds only under the `serverchange` tag, so no untagged run of the package
builds it, and it is a job of its own because it is slow and serial: it sends for at least a
minute and waits out each server's restart and the JetStream catch-up after it, which, beside
the listener's other tests, would hold up the job that runs them - envoy-go-race, which runs
cmd/listener under the race detector, where it would be slower still. It runs only the tests
the serverchange tag adds, since envoy-go-race runs the package's untagged tests already
(envoy-go's unit step leaves /cmd/listener$ out, for the lane to run under the detector): the
-run comes from .github/scripts/go-tagged-tests.sh, which reads the names off the source at
run time, so a second serverchange test added later runs here instead of being left out of a
hand-written list. The assignment is a line of its own: under bash -e it carries the script's
exit status, so an empty or refused list fails the step instead of running -run "", which
matches every test. The cluster runs in the test process (internal/testnats), so the job needs
no container runtime and no database.

## The secrets broker's two-process tests

The two-process tests of the secrets broker (packages/envoy/cmd/broker/multiprocess_test.go,
INFRA-236): each row starts real cmd/broker binaries against one Postgres - which is what the
broker's Deployment runs once it carries more than one replica - and drives them over real
HTTP and their own logs. They race one refresh token and one device code against both
processes, migrate one empty database with both at once, prove a retag read by one process is
enforced by the other through the reread fan-out, and signal a process to watch it fail
readiness while it serves a request out past the old shutdown budget.

They build only under the `multiprocess` tag, so envoy-go's unit run leaves them out, and
they could not run there in any case: that step runs under go test's default ten-minute
package deadline, which the memory tests already took red at 600 s (run 37116639565), and the
rows here wait out a migration race, a ten-second fan-out window and a twelve-second drain,
tens of seconds each. A job of their own rather than a step in dispatch-multiprocess, whose
Postgres service and shape this copies, so that each runs only on what it reads: the broker's
closure (broker_multiprocess) shares little with the Dispatch server's
(dispatch_multiprocess). They need no NATS and no test-container pull - the broker starts
nothing in a container - so neither step is here. The step runs only the tests the tag adds:
`-tags multiprocess ./cmd/broker` would run the package's untagged tests a second time,
envoy-go's unit step having run them already, so the -run comes from
.github/scripts/go-tagged-tests.sh, which reads the names off the source at run time. The
assignment is a line of its own: under bash -e it carries the script's exit status, so an
empty or refused list fails the step instead of running -run "", which matches every test.

## The Lean model of the airlock, and why its fuzz run is two minutes

The Lean model of the airlock and the Go code it covers: the model is built, the axioms the
theorems Axioms.lean names rest on are printed and held to the three Lean's own kernel admits, and
FuzzAirlock drives Validate, Decide and Rewrite against the model's program, so a change made
on one side and not on the other fails here rather than in production.

The fuzz run is two minutes, because this is a required check and a required check stays
short. The ten-minute run the issue's verification asks for is the implementer's recorded
result on the head that merges, not this job.

## Why the container smoke test is a job of its own

The container smoke test, the envoy-infra plugin's live tests, and the image both run. Its own
job, gated on envoy_smoke: the image is built from the Dispatch SPA and the Bun workspace as
well as the Go sources, so a Dispatch-only change changes the image and must smoke it, without
running envoy-go's whole Go suite. When the smoke lived in envoy-go, PR #1562 (packages/dispatch
only) skipped envoy-go and never smoked the image it changed (run 36494702052).
.github/scripts/check-image-trigger-paths.sh fails the build when envoy_smoke misses a file the
build reads.

## What the local end-to-end job runs

packages/envoy/scripts/e2e-local.sh: signed GitHub fixtures and a direct message through a real
listener and NATS, every frame rendered by both inbound renderers (renderInbound and Go's
Deliverer.Text) and checked against the acceptance contract.

## The session stall suites, the session state rig and the live Dispatch stack

The session stall suites (INFRA-548): each freezes a session's NATS path behind
envoy-client's stall proxy (test/stall-proxy.ts) on a real natsd and listener, which
test/envoy-stack.ts builds with go build from packages/envoy, and holds the client to a
reconnect within 60 s: envoy-client's shared connection options on their own, then the real
pi-envoy extension and the real claude-envoy channel server on consumer delivery, each
subscribing its relay subject again on the new connection, registering a new epoch, and showing
the message sent while it was cut off once. They run only with LEGION_E2E_ENVOY_STACK=1,
which only this job sets, so the envoy-client, pi-envoy and claude-envoy jobs, which set up no
Go, skip them.

The session state rig (dispatch://INFRA-392) runs on the same stack and needs no agent: it
SIGSTOPs a child process carrying envoy-client's own registration and heartbeat and holds the
listener to `unresponsive` — the role claim kept, the send stored — until the gone horizon.
Its longest property waits out that horizon and one reaper pass: the role claim reaper runs
at ENVOY_ROLE_CLAIM_REAP_INTERVAL, which the rig sets to two seconds, so the suite costs the
horizon rather than the five minutes that reaper's own default would cost it.
The live Dispatch-stack suite rides this job too (dispatch://INFRA-1727). It builds
packages/envoy's ./cmd/dispatch, makes a database of its own on DISPATCH_TEST_DATABASE_URL and
runs the real `dispatch` CLI against that server (packages/envoy-client/test/dispatch-stack.ts),
so it needs a PostgreSQL and a Go, which this job is the only envoy-client lane to have: the
envoy-client job sets up neither, so a suite gated on LEGION_E2E_DISPATCH_STACK would skip
there for ever. The filter envoy_session_stall already selects packages/envoy/** and
packages/envoy-client/**, so the suite brings no new path into this job's closure.

## What the ci-filters job holds every filter and every workflow file to

Holds every filter in `changes` to every file its job reads, recomputing each Go job's closure
with go list (.github/scripts/check-ci-filters.sh). go list runs with GOPROXY=off over an empty
module cache, so the job needs Go but downloads no module and restores no cache. It also holds
every workflow file to 64,000 bytes and to what GitHub's own parser reads
(.github/scripts/check-workflow-files.sh), which .github/workflows/workflow-files.yaml runs a
second time, in a file small enough to start when this one is too large to.

## What the proof editor's offline proof shows

The proof that no install of this repository fetches the proof editor's upstream sources over
the network: a frozen install runs behind a proxy that answers HTTP 504 for the hosts of the
old `github:sjawhar/proof-sdk#<sha>` route, and the job fails when the proxy's log holds a
single refusal. The steps, and why those hosts, are in the called file.

## Why the worker-image job grants every scope the called workflow declares

GitHub refuses a run whose calling job grants less than the called workflow's jobs declare,
whatever their `if:`
(.github/ci-notes/release.md#why-the-worker-image-call-declares-every-scope-worker-imageyaml-names
says the same): its workflow-level block
(the build's packages: write, and the actions: read and checks: write of its ci-ok-report
step) and its `attest` job's id-token and attestations: write. A pull request run still skips
`attest` (its `if:` requires `github.event_name != 'pull_request'`), and, passing no
`attest_digest`, leaves it empty, so the build job runs.

## Why the browser image's build runs on a merge group too

The browser image's build: browser-image.yaml builds .github/docker/browser.Dockerfile and
pushes it when the package does not already hold an image built from it. Unlike the worker-image
job, this job has no `pull_request` term: it runs on a merge group too, so the image a
merged head's `container:` jobs pin — the shard and alone jobs of
.github/workflows/dispatch-browser.yaml — is built from that head.

## Why the browser-image job grants every scope the called workflow declares

GitHub refuses a run whose calling job grants less than the called workflow's jobs declare,
whatever their `if:`: its workflow-level block (the build's packages: write, and the
actions: read and checks: write of its ci-ok-report step) and its `attest` job's id-token
and attestations: write. That `attest` job runs whenever the build pushed, whatever the
event, a pull request's run included, because the two-push order .github/ci-images.yaml's
header describes has a pull request's run push the image the rows then pin
(browser-image.yaml's `attest` job says why).

## The pr-title job: what it checks, and why it posts nothing

pr-title, the other check main's ruleset requires of this router, and with `ci-ok` one of the
two jobs of this file named for a required check. A pull request's title, body and commit
messages reach main's squash commit, the generated release notes and GitHub's pages, and its
comments and reviews are public on its page, so this job checks them, and that the title is a
conventional commit subject, at every push to a pull request that is not a draft, when a
draft is marked ready, and on the merge queue's group.

It posts nothing: pr-title is this job's own check run, which GitHub puts in this run's own
check suite, where a check run created with POST /repos/.../check-runs under GITHUB_TOKEN
joins the Actions App's first suite at that commit instead, which has left a required check
green in another workflow's suite and the merge queue refusing to enqueue
(.github/scripts/post-check-run.sh's header holds those measurements). So no step here calls
that script, and the job holds no `checks: write`.

A merge group carries no pull request, only the queue branch it runs on, so
find-pr-payload.sh reads the pull request that branch names and writes it in the shape
check-pr-text.sh wants. pr-title.yaml runs on an edit alone, whose payload carries the pull
request, so a pull request that is opened, reopened or pushed to gets its
pr-title from this job, and a fork's head gets one too: a job's check run costs no token
scope, where a POST needs `checks: write`, which a fork's run's read-only GITHUB_TOKEN does
not carry. A dispatched run carries
neither a pull request nor a group and has no text to check, which is why the `if:` names the
two events this job runs on rather than excluding the draft alone.

## What the pr-title job's scopes are for

What the two scripts this job runs, find-pr-payload.sh and check-pr-text.sh, read through gh, and nothing else: the pull request's commits
from the compare route (`contents: read`), the pull request itself, its reviews and its
review comments (`pull-requests: read`, which find-pr-payload.sh's read of the pull request
a queue branch names needs too), and its issue comments (`issues: read`). No
`checks: write`, since this job posts no check run, and no `actions: read`, since it carries
no ci-ok-report step: CI_OK_EXEMPT holds it outside ci-ok's accounting (this file's env says
why).

## Why pr-title refuses a re-run attempt in a step of its own

A merge group is tested once: the queue builds a new group rather than re-running one, so
an attempt above the first checks a tree the queue is no longer waiting on. This job
therefore carries no `github.run_attempt == '1'` term, unlike every job of this file but
the verdict, and refuses such an attempt in a step instead. `ci-ok` carries that same step
for the same reason, and the two share the rule because they are the two jobs named for a
check main's ruleset requires: an `if:` can only take the job away, a skipped job's check
run is a passing required check, and a dead group must get a real failure rather than the
check concluded by nothing. It comes before the checkout because no step after it runs
once it has failed.

## What the pr-title check step maps, and how it runs its scripts

PRIVATE_NAME_TERMS is the deployment's own list, which check-private-names.sh's fifth rule
reads and requires on a merge group (that script's header). Both scripts run by the
checkout's own path, the form every job that holds the list runs the check by;
check-pr-text.sh runs check-private-names.sh from its own directory.

## The merge queue's cancel act. Main's queue builds each group on top

The merge queue's cancel act. Main's queue builds each group on top of the group below it,
so when a group fails, every group built on it can no longer merge: the queue takes that
pull request out and rebuilds the groups above it. That removal is a dequeue, so
.github/workflows/dequeued.yaml force-cancels the removed group's own run, and the groups
built on the failed one keep their runners until the queue rebuilds them.

`cancel-started` runs at the start of every merge-group run, with no `needs:` so that it
starts at once, and it has one act: it cancels the groups of the same pull request the
queue has replaced, which test a tree that will not land. Its own queue branch names that
pull request (`gh-readonly-queue/<base>/pr-<n>-<parent sha>`), and
.github/scripts/merge-group-cancels.sh decides from it and the run list which runs those
are: the script makes no GitHub call and prints one `cancel <run id>` line per run to
force-cancel, in the order to act on them; its header says what it reads and why.

Each cancel is `POST /repos/{owner}/{repo}/actions/runs/{id}/force-cancel`, acted on in that
order. A force-cancel bypasses `if: always()` — the step of a running job is left skipped,
not run, measured on a probe run of this repository — so a force-cancelled run posts no
ci-ok, and its ci-ok job, which waits for every job of the run, is left unstarted or
cancelled rather than concluding one. That is what this act wants: a failing ci-ok on a
group the queue still holds could
take an innocent pull request out of the queue. The run of a group whose own job failed
posts that group's failing ci-ok from that job itself, through the merge-group branch of
.github/scripts/ci-ok-report.sh, so the queue reads the verdict and rebuilds the groups
above it without waiting for the rest of that run.

The job follows these rules:
  * it runs only on `merge_group` and only on attempt 1
    (.github/ci-notes/ci.md#what-the-changes-job-selects-and-the-two-terms-every-job-carries
    says why a re-run attempt does nothing);
  * `permissions:` is `actions: write` and nothing else. `gh run list` and the force-cancel
    need it (a scope granted `write` grants its read); the checkout needs no
    `contents: read`, because this repository is public and GitHub serves a public
    repository's git data to any valid token — measured here with `git ls-remote` over https
    against a public repository the token holds no grant on at all, which succeeded;
  * it carries no ci-ok-report step and reports nothing to ci-ok, so its id is in
    CI_OK_EXEMPT in this file's env, outside ci-ok's accounting;
  * it runs this repository's own .github/scripts/cancel-started-act.sh, and the
    merge-group-cancels.sh that script runs, out of the run's own checkout and nothing else:
    no bun, no go, no node, no installed dependency, no npm or
    bunx tool. A merge-group run checks out the queued tree, so the script it runs is the
    queued change's copy, which that change's review has read;
  * its step's token is ${{ github.token }}, and its time limit is short: the act is a
    handful of API calls.

A force-cancel that fails leaves a warning rather than a red job: the run it names may have
completed between the list and the call, and an act that reclaims nothing is no failure of
the run it runs in.

## The verdict, and one of the two checks main's ruleset requires of

The verdict, and one of the two checks main's ruleset requires of this router. It needs every
job of this file but itself, `cancel-started` and `pr-title`
(.github/ci-notes/ci.md#the-jobs-outside-ci-oks-accounting says why those three are outside
ci-ok's accounting), runs under `always()` so that a failed,
skipped or cancelled job still reaches a verdict, and fails when .github/scripts/ci-ok.sh's
verdict over `toJSON(needs)` is failure. ci-ok is therefore this job's own check run, which
GitHub puts in this run's own check suite, where a check run created with POST
/repos/.../check-runs under GITHUB_TOKEN joins the Actions App's first suite at that commit
instead, which is another workflow's as often as not (.github/scripts/post-check-run.sh's header
holds that measurement).

A job of its own waits for a runner, and that wait is the cost of this shape: it is created
with the run but starts only once the last of its needs has ended, when the pool is busiest
with the next wave. docs/solutions/github/join-a-job-to-the-ci-router.md holds every reading
of it. This job runs on ubuntu-24.04-arm because it needs nothing of an x86_64 machine, and
that pool queues apart from the x86_64 one and is idle by the time the last need ends: on the
run the "Runners" paragraph of .github/ci-notes/ci.md#the-router-every-check-a-pull-request-or-the-merge-queue-runs measures, it started 14 s after it was created. The shape
is kept because a late verdict is right while one in another workflow's
suite blocks the head until a person re-runs it. What this job cannot do, and the
ci-ok-report step every other job ends with still does, is post the failing ci-ok of a
merge-group run one job's result has already decided: this job waits for every job of the
run, and a group that can no longer merge should leave the queue at the first failure rather
than at the last job's end.

This job's `if:` is `always()` and nothing else. Every other job of this file is skipped on a
draft pull request: `changes` carries `!github.event.pull_request.draft`, the jobs its
outputs gate skip with it, and lint, typecheck, test, daemon-go, plugin-pack,
private-names-port, sessions-script, text-checks and `pr-title`, whose `if:`s do not wait on
`changes`, carry that term themselves.
Every job but `pr-title` and this one is skipped on a re-run attempt of a merge-group run by
`github.run_attempt == '1'`
(.github/ci-notes/ci.md#what-the-changes-job-selects-and-the-two-terms-every-job-carries says
what both terms do for them); this
job carries neither. A job an `if:` holds back is skipped, and a skipped job's check run is a
passing required check, so either term would leave the head a ci-ok nothing judged: a merge
group would merge on it, and a draft would carry it as a pass. An `if:` can only take the job
away, while what both cases must get is a verdict, so the job runs on both and a step of it
concludes the failure: the two refusal steps, "Refuse a draft pull request" and "Refuse a re-run attempt of a merge-group run", one for a draft pull request and one for an
attempt above the first. `pr-title`, the other job named for a check that ruleset requires,
holds no attempt term for the same reason and refuses such an attempt in a step of its own too.

So on a draft this job is the whole run: every other job is skipped, there is nothing to
judge, and the failure of its draft refusal is what keeps a draft from carrying a passing
ci-ok. That failure names the remedy — marking the pull request ready for review starts a run
that selects by the same diff and concludes a real verdict over it.

`checks: write` and nothing else: the verdict step reads this run's check runs and their
annotations, to tell a job a runner shutdown killed from one the change broke (ci-ok.sh's
call of runner_shutdown_kills, runner-shutdown.sh), which `checks: read` covers and the
`checks: write` here carries, and
the step after it reads this head's ci-ok check runs and PATCHes them with that scope. The
checkout needs no `contents: read`, as `cancel-started`'s does not — this repository is
public, and GitHub serves a public repository's git data to any valid token.

## The job reads nothing of the tree but ci-ok.sh, ci-ok.jq, ci-ok-reconcile.sh and

The job reads nothing of the tree but ci-ok.sh, ci-ok.jq, ci-ok-reconcile.sh and the
gh-read.sh and runner-shutdown.sh they source, so the .github cone is the whole checkout
it needs.

## Why a draft pull request is refused in a step

A draft pull request has asked for no test: every other job of this file is skipped
there, by the draft term `changes` carries or one of its own, so what this job would
judge is a run of nothing but skips. The refusal is a step and not a term of this job's
`if:`, as .github/ci-notes/ci.md#the-verdict-and-one-of-the-two-checks-mains-ruleset-requires-of
says: the `if:` would skip the job, and a skipped
job's check run is a passing required check, so the draft would carry a green ci-ok
nothing judged.

It comes before the merge-group refusal, the order a change meets the two cases in, and
the order decides nothing anyway: no run meets both. A merge_group event carries no
`github.event.pull_request`, so this term is false on the queue's runs, and a draft
cannot be queued.

## Why a re-run attempt of a merge-group run is refused in a step

A merge group is tested once: the queue builds a new group rather than re-running one, so
an attempt above the first judges a tree the queue is no longer waiting on, and the
verdict over it is failure whatever the jobs of the run did. Every other job of the file
is held back on such an attempt by a term of its own `if:`, so what this job would read
there is a run of skips; this step refuses it before the verdict step reads them.

The refusal is a step and not a term of this job's `if:` because a job an `if:` holds
back is skipped, and a skipped job's check run is a passing required check: the group
would read a ci-ok nothing judged and merge on it. An `if:` can only take the job away,
and what a dead group must get is a verdict, so the job runs and a step of it fails.
It comes after the checkout because the reconciliation step, "Set every other ci-ok on this head to this job's conclusion", runs under `always()`
and reads ci-ok-reconcile.sh out of the tree.

## NEEDS is `toJSON(needs)`: `changes`' result and outputs with every other job's result

NEEDS is `toJSON(needs)`: `changes`' result and outputs with every other job's result,
which is the shape ci-ok.sh takes. A failure here fails the job, and the job's check run
is the verdict. GH_TOKEN and HEAD_SHA are what that script asks GitHub with: a job of
this run that ended `failure` is read against the check runs on this head, where the
runner's shutdown annotation tells a job the platform killed from one the change broke
(runner-shutdown.sh). That read is this job's `checks: write`, which carries the
`checks: read` it needs; the reconcile step, "Set every other ci-ok on this head to this job's conclusion", reads the same route with the same
token. GITHUB_REPOSITORY and GITHUB_RUN_ID, the rest of what it reads, are Actions' own.

## Why every other ci-ok on this head is set to this job's conclusion

A ci-ok the ci-ok-report step posted stays on the head as it was: no re-run updates a
check run a job created with POST, and it can sit in another workflow's check suite, so a
run that failed and was re-run green would stay blocked by it. This step sets every ci-ok
check run on the head but this job's own to what this job concluded, so one verdict stands
at the end of every attempt (ci-ok-reconcile.sh says what it passes over and why a refusal
only warns). `job.status` is this job's conclusion, and `always()` is what runs the step
after the verdict, the "Judge every job of this run" step, failed.
