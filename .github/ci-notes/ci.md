# Notes for the CI router and its filters

The derivations and the history of the CI router, `.github/workflows/ci.yaml`, and of the
path filters its `changes` job selects jobs by. The filters were a file of the repository,
`.github/ci-filters.yaml`, until the changes job began to compute them when it runs (the next two
sections say how); the sections after them stood beside that file's filters, in the order its
comment blocks stood, and each says why a job reads what its gate in `.github/ci-gates` declares.
The router's sections follow them. Each `# why:` line of ci.yaml names the section that holds the
derivation or the record its step, its job or its key rests on. In ci.yaml a comment block of at
most two lines stays where it is, beside the pattern or the step it is about, until ci.yaml stands
at the byte limit `.github/scripts/check-workflow-files.sh#LIMIT`: then a derivation of two lines
moves here too, as that check's refusal says, and its place keeps a `# why:` line. A value's own
arithmetic stays, and so does a comment inside a `run:` body, which is shell and stays with its
command.
In the text moved here, "this file" and "this workflow" refer to `.github/workflows/ci.yaml`,
where every one of these comments first stood, the filters' comments among them, and a section
that names a pattern of `.github/ci-filters.yaml` names what the filter selected then.

## The changes job computes its filters when it runs

No file of the repository holds a filter. The changes job checks out the tree the run tests (a
pull request's merge with its base, a merge group's head), installs Go and the Bun workspace, and
runs `.github/scripts/ci-filters-generate.sh`, which runs `.github/scripts/check-ci-filters.sh
generate` and writes every filter into the runner's temporary directory; its one
dorny/paths-filter step reads that path (an expression with no colon, which the action reads as
the path of a file, `isPathInput` in its src/main.ts, and opens with `fs.readFileSync` from the
workspace). The job uploads the filters as computed (`ci-filters.computed.yaml`) as the run's
`ci-filters` artifact: the lint job holds them to the tsc programs and the image builds
(check-ts-filters.sh, check-image-trigger-paths.sh), and the test job's filter-check and
router-outputs harnesses read them. A script run by hand computes them with
`.github/scripts/check-ci-filters.sh generate <file>`. Each output of the job but ref_is_main
reads its one paths-filter step, and rule 19 of .github/scripts/check-ci-filters.sh refuses an
output naming a filter no gate declares. On pull_request
dorny/paths-filter reads the file list from the API, a rename read as the delete of the old name
and the add of the new one; on a merge group it diffs base_sha..head_sha with git. A dispatched run
computes the file too: a dispatch of a branch diffs its commit with its merge base with main
(.github/ci-notes/ci.md#what-the-filter-step-diffs-on-a-dispatched-run), and a run that selects
every job filters nothing.

A change to the computation itself - the engine or the shared read table under .github/scripts,
the two scripts that run them, or a file they read such as package.json (the generate mode lists
them) - can make many filters select less while no filter selects the change: a read dropped from
a helper every Bun gate shares narrows two dozen filters, and the committed file used to show that
drop because every filter named the file. So when the run's base commit is known (the pull
request's base, the merge group's base, a dispatched branch's merge base with main, which the
generation step hands the script as `CI_FILTERS_BASE`) and the change touches one of those files,
the script computes the base tree's filters too, with the base tree's own engine, and each filter whose
patterns differ is `'**'` in what the paths-filter step reads: every job whose selection the change
could have moved runs. Each such filter is printed in the step's log and summary with the patterns
only one side has. A base tree whose filters cannot be computed makes every filter `'**'`, as a
failed computation does (below). A
change that touches none of those files is not compared, and costs no second computation.

When the computation fails for any reason - a step before it that failed (setup-go, Bun's setup
and the install each run with `continue-on-error`), a go command that cannot run, a gate file or a `//ci:` line that cannot be
read, a run past five minutes - the script writes instead a file in which every filter the changes
job hands on is `'**'`, its first line `# FAIL-OPEN:` with the reason, and exits 0: every job
runs, as on a dispatch of main, and the paths filter's verdicts and the outputs stay one shape. The file's
names are the filters the changes job's outputs hand on, so it holds no filter a step reads out of
the list of matched filters (#one-list-for-the-test-jobs-step-gates); the script also writes
`every_filter=true` to its step's outputs, which makes that list `all`, so every such step runs
too. The run is
red all the same: the lint job's two readers of the filters refuse a fail-open file, whatever the
cause. The ci-filters job, which every filter then selects, fails as well when the cause is the
engine's own (a go command, a gate file, a `//ci:` line), but not when it is the Bun install or the
TypeScript listing, which only the changes job runs. The `full-ci` term still holds the slow
suites back on a pull request that has no label, as it does when their filters match; the merge
group runs them.

The job's setup-go keeps no Go cache: its key is exact, so each pull request's first run and
every merge group missed it, and each miss saved 895 MB.

Why computed. The filters were one file every job's filter was in, and every pull request that
added a read, a test file or a package a job compiles edited it, most of them at the same lines:
of 433 open pull requests counted when this changed, 215 edited that file or the read table, and
most forward merges of main into one conflicted. Yet the lists it held were the read sets this
repository's check already computed: rules 1, 9 and 17 of the check held the file equal to them.
The hand-kept facts the sets are computed from are each in a file of their owner now: one file of
`.github/ci-gates` per gate, and a `//ci:` line beside each read a test makes outside its
package (.github/scripts/ci_filter_sources.py). Two pull requests that each add a read of a
different test, or a gate of a different job, edit different files.

## How a filter is computed

Each filter is every file its job reads: the package and what its tests, typecheck and build read
outside it; the composite actions the job installs its tools through; for Go, the closure of the
packages the job compiles (`go list -deps -test`), the targets of the go commands its tests start,
and the module files of the workspace the build runs in, whose go.mod files move the versions it
compiles; ci.yaml, since a job reads its own definition; and the job's own file of
`.github/ci-gates`, which its filter is computed from. A gate on a job's own `if:` reads what each
gate on one of that job's steps reads too, since the step runs only in a run the job's filter
selected: the test job's filter holds every harness its steps test.

The files become patterns in three forms (ci_filter_coverage.compressed): `<dir>/**` for a
directory whose every file but its Go test files is read, `<dir>/*` for such a directory's own
files, and the path itself otherwise. The directory forms select a file a change deletes there,
which a filter must: the change that deletes a source of a package a job compiles starts that
job. Then `!` patterns keep the filter off every Go test file those forms select and the job does
not compile - another module's, another package's, a tagged live one, a `//go:build race` one -
in two forms, `!<tree>/**/*_test.go` where no test file the job reads is under <tree> and
`!<path>` where one is. dorny/paths-filter v4.0.3's `some-with-excludes` quantifier matches a
change to a filter when one of its patterns without `!` matches and none of its `!` patterns does.
Each generated filter is checked against its own read set before it is written: what its patterns
select among the repository's files must be exactly those files.

Three kinds of file join a filter beside its job's reads. A PUSH_WORKFLOWS workflow joins the
filter rule 8 pairs it with, since a workflow that does not name itself never runs on a change to
itself (docs-pages.yaml in docs). Every file a TAG_RUNS tag step reads joins the filter of the job
that runs that package's untagged tests (rule 15). And a filter that ci_ts_programs.py's LISTED
names holds, in a block between a `# BEGIN listed` and a `# END listed` line, the files of its
tree that its sources' tsc programs compile or bundles load, which the changes job lists with Bun
(.github/scripts/ci_ts_listing.py).

worker_image and browser_image are the two filters kept by hand, each in its own file of
`.github/ci-gates` (`EXCLUDED = excluded(...)`): their judge is the image check,
.github/scripts/check-image-trigger-paths.sh, which proves each covers every file its image build
reads, and the worker image's context holds packages/daemon's test files while the .dockerignore
drops packages/envoy's. Their `!` patterns drop what the .dockerignore keeps out of the build
context.

A sentence below that says a filter "names" a path, or holds an anchor, describes the committed
file this computation replaced; what a job reads is its gate's, and the filter follows from it.

## Why the filters were one file, and the steps they stood in

History: what the filters file was before the changes job computed its filters. The path the
step hands the action is still a plain scalar for the reason below, and the environment-variable
cap it records is why no filter goes back into a workflow's own text.

Every path filter `.github/workflows/ci.yaml#changes` job selects jobs by, in one file.

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
main adds, and so are envoy_go and its fast half (envoy_go_checks then, envoy_go_build now), which the second step had
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

ci.yaml, which every gate filter but ci_filters names (ci_filters names it through `'.github/workflows/**'`): every job's `if:`
is in this file, so every job reads it - which is why ci_job_reads.py declares
CI_SELF on every gate. Each gate filter also names the `ci-job-<job id>.yaml` file
holding its own job's steps, and the gate's own file of `.github/ci-gates`, so a change to one of those files selects that job
alone. This entry stays beside them because a change to ci.yaml does reach every
job: dropping it would make each job's `if:` test a second output of changes, which
.github/scripts/ci_filter_coverage.py refuses (a job's `if:` tests exactly one), and
an output written as a disjunction instead would select exactly what this pattern
selects. The anchor is on the glob rather than on the list, since envoy_smoke and
worker_image name it too and the image check, .github/scripts/check-image-trigger-paths.sh, reads those two as flat lists.

## daemon_envoy_deps: The airlock's rules, its decision and its header swap

The airlock's rules, its decision and its header swap. Not the whole tree: lean/
holds the model, which airlock-lean alone reads, and testdata/ the fuzz seeds and
the mutant patches, which the proxyrules package's own tests read - the daemon
compiles that package without running them.

## daemon_envoy_deps: The daemon reads the Envoy session timings (heartbeat

The daemon reads the Envoy session timings (heartbeat, live window, claim
staleness and the listener call's attempts) through this one public package: it
is a module of its own, so Go's internal rule bars it from internal/contracts,
where the generated values are.

## legion_command: The broker's OAuth wire types: packages/envoy/internal/cipherclient

The broker's OAuth wire types: packages/envoy/internal/cipherclient (legion_command's `'packages/envoy/internal/cipherclient/*'`)
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
tests every package of packages/daemon but the five concurrent ones, and the race
step, which tests those five — so the filter covers the whole module's closure,
and what the Stage 1 proof, ci-job-daemon-go.yaml's "Stage 1 live proof" step, builds and sources. The
packages they compile, the Dispatch server `packages/daemon/internal/testbuild/testbuild.go#buildEnvoy` builds
for `packages/daemon/internal/daemon/outbox_scratch_dispatch_test.go#startScratchDispatchOn`, and what their
tests read outside the daemon: the contracts fixtures `packages/daemon/internal/api/state_golden_test.go#goldenPath`,
`packages/daemon/internal/api/state_golden_test.go#golden`,
`packages/daemon/internal/intake/intake_test.go#TestEnvoyGoldenSubjectsReachTheirRepositorysConsumer` and `packages/daemon/internal/intake/intake_test.go#TestAWorkflowRunEnvelopeIsNoFact`
read, the agents `packages/daemon/internal/prompts/roles_test.go#TestEveryTaskAgentARolePromptDispatchesIsShipped`
stats, the tools skill `packages/daemon/internal/prompts/roles_test.go#TestThePodMemoryRuleAndTheToolsSkillSendTheSameRunsToCI`
holds roles/mechanics/headless.md to, the rule file
`packages/daemon/internal/prompts/roles_test.go#TestNoReusablePartNamesARepositoryOrLibrary`
runs over each reusable role part, the merge gate
`packages/daemon/internal/workflow/engine_test.go#TestTheReviewerIsToldTheReviewedCommitLabelItsTaskCarries`
holds the reviewer's prompt to, the GitHub App helper
`packages/daemon/internal/api/real_github_test.go#runGitHubAppsLib` sources, and the examples and
docs its tests hold to the code (`packages/daemon/cmd/legion/config_example_test.go#stageShippedExample`,
`packages/daemon/internal/config/controller_test.go#TestLoadControllerReadsTheShippedExample`,
`packages/daemon/internal/config/kubernetes_test.go#operatorRoutePod`).
It also names the forced-order step's script, packages/envoy/scripts/forced-order.sh,
and the hooks under packages/envoy/internal/testnats/testdata/hooked that the script
patches nats-server with.
No bun_install and no skills/** - the two pages of skills/ this filter names are all
this job reads of it: the tests of this suite that packed the two plugins - and so
resolved the root manifest, the lockfile and the patches - or staged each plugin's
partition of skills/ with the prepack script are the plugin-pack job's now, and
this job installs nothing (dispatch://INFRA-1413, A4).

The list is the one the test job's filter used to name through this anchor while
it gated nothing of its own; it is now the daemon-go job's own filter, so a change
a Go test cannot see no longer runs 1,083 s of them (dispatch://INFRA-1413).

## daemon_go_tests: pi-legion's agents: internal/prompts/roles_test.go stats `agents/<name>.md`.

pi-legion's agents: `packages/daemon/internal/prompts/roles_test.go#TestEveryTaskAgentARolePromptDispatchesIsShipped`
stats `agents/<name>.md`.
With its manifest, which the Stage 1 proof (ci-job-daemon-go.yaml's "Stage 1 live proof" step) reads, they are the files of
pi-legion this suite reads. The pack and prompt probes, which read the rest of
it, are the plugin-pack job's.

## daemon_go_tests: The rule file internal/prompts/roles_test.go holds the role parts to

The repository's own rule file, .github/private-names.toml, which
internal/prompts/roles_test.go's TestNoReusablePartNamesARepositoryOrLibrary reads and
runs over every reusable role part, since the `legion` binary embeds no rule. A change
to that file alone changes what the test refuses, so it starts this job.

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
and so do two steps of the tooling-checks job - its verify-build step, which runs the same
command (packages/claude-envoy/scripts/verify-build.sh), as release.yaml's
claude_envoy_build does, and its build-input-list step, which runs
packages/claude-envoy/tests/build-inputs.test.ts. What only the whole claude-envoy
job reads is in the claude_envoy filter rather than here: the tooling-checks job runs
that job's lint and typecheck in no step, and no other test of the package.

## claude_envoy_build: release.yaml's version arguments are held to the build inputs

packages/claude-envoy/tests/build-inputs.test.ts's test ".github/workflows/release.yaml's
$CLAUDE_ENVOY_VERSION_ARGS carries the build inputs" holds release.yaml's
$CLAUDE_ENVOY_VERSION_ARGS equal to scripts/build-inputs.txt, so a change to either
must run both jobs.

## claude_envoy_build: Its src/broker-credential.ts imports @legion/cipher-client

Its src/broker-credential.ts imports @legion/cipher-client (brokerTokenClient from its
/environment module, chooseSignIn from /signins and isFinalRefusal from /token-client), whose exports
map every entry to `./src/<module>.ts`, so a build compiles that package's seven
sources with envoy-client's. check-ci-filters.test.sh holds a hand-written list of
such pairs to this pattern, and ci_job_reads.py's envoy_client_dep is the one
place that declares the pair, which check-ci-filters.sh then holds to the filter.

## claude_envoy: tests/envoy-channel-server.test.ts holds the marketplace

tests/envoy-channel-server.test.ts holds the marketplace entry to this package's
own name, to no `version` of its own, and to a source range that leaves out the
0.0.0 placeholder and admits the first release. No step of the tooling-checks job runs that
test, so the entry is this job's alone.
The same test holds the Codex marketplace entry to the Codex package's name and
range.

## contracts: The Go copies its tests hold the TypeScript to

The Go copies its tests hold the TypeScript to: src/dispatch-href.test.ts's "the Go
reader's table is the same table" and src/dispatch-text-references.test.ts's "the Go
reader's text table is the same table" read the Go reader's JSON tables,
src/dispatch-block-ref.test.ts reads pmdoc's block pointer and typed block id
tables, scripts/gen-go.test.ts checks every file scripts/gen-go.ts writes - the
Go contract, the Go copy of the tool specs, pmdoc's generated block schema and contracts'
own generated TypeScript - is current, and src/dispatch-tools.test.ts reads that Go copy
of the tool specs for the rule and the bounds each tool states. Not pmdoc/schema, the
block schema those generated types are the shape of: this job generates that Go from
packages/contracts/schemas, and reads no file of pmdoc/schema itself.

## dispatch_api_types: The two files the Dispatch API types are rendered from

packages/contracts/src/dispatch-api.generated.ts, the Dispatch API's TypeScript types,
is in no commit: every `bun install` renders it from schemas/dispatch-openapi.json
with scripts/gen-api.ts (the root package.json's postinstall), and so does the
`typecheck` script of each package that compiles it, so two pull requests
that each add a route never edit one generated file; every CI typecheck runs after
the job's install. Only a type check reads it:
dispatch-api.ts re-exports it with `export type *` and every other import of it is
an `import type`, so Bun, `bun build`, Vite and Astro erase each of them. A change
to what a typecheck compiles there is a change to one of those two files, so the gate
of every job whose tsc program holds the module declares both
(`.github/scripts/ci_job_reads.py#dispatch_api_types`), and the listing of a program gives the two
in the module's place (`.github/scripts/ci_ts_listing.py#RENDERED`), so check-ts-filters.sh holds each
gate to them. The contracts gate takes the whole package. The Go the document is
emitted from is not named here: a change to it is judged by envoy-go-build's
`openapi --check`, whose gate declares both that Go and the document.

## guide_layout_files: The six guides scripts/semantic-line-breaks.guides.test.ts reads, the checker it

The six guides scripts/semantic-line-breaks.guides.test.ts reads, the checker it
spawns and the test itself: the guide-layout step's filter, guide_layout, is them, and the
tooling-checks job's filter names them too, since a job's `if:` tests one changes output and
that step runs in no job the tooling_checks filter did not select. No other step of the job
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

## release_push_test: The rule file release-push.sh reads beside itself

`.github/scripts/release-push.sh#leak_guard` finds .github/private-names.toml
beside itself and runs gitleaks with it, and
`.github/scripts/release-push.test.sh#leak_rules_into` copies it into each
fixture checkout it runs a copy of the script from.

## test: The test job: the script harnesses and the Bun suites of scripts/

The test job: the script harnesses of the `release_push_test` to `go_test_step_expansion_test`
filters and the Bun suites of scripts/. Its plugin build, changelog check, guide-layout step,
lints and typechecks are the tooling-checks job's, with the tooling_checks filter, so they come
back on every push while these tests run in the merge queue
(.github/ci-notes/ci.md#what-the-tooling-checks-job-runs). The daemon's Go tests are the
daemon-go job's now, and daemon_go_tests, which this filter used to name through its anchor, is
that job's own filter.

It names every harness group (`*release_push_test` to `*go_test_step_expansion_test`), because a
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
makes one of those gates true. The engine adds each step gate's reads to its job's filter
(ci_filter_coverage.gate_read_sets), so the test job's filter holds every harness its steps test.

## tooling_checks: The tooling-checks job: the Claude Code plugin build, the changelog

The tooling-checks job: the Claude Code plugin build, check and pack, the plugin's build-input
list check, the changelog fragments' render check, the guides' layout step, and the lints and
typechecks of scripts/dispatch, scripts/github-apps, scripts/sessions, scripts/approval-eval
and scripts/e2e/lib. They were steps of the test job, beside its harnesses; this filter names
what they read and none of the harness groups, which stay on the `test` filter.

## tooling_checks: The guides' layout step's own gate, named here so the job runs

The guide-layout step's own files: its gate is the guide_layout filter, and a guide-only
change has to select this job for it, since a job's `if:` tests one changes output and a
step gate decides nothing in a job that was not selected.

## guide_layout: The guides' layout step of the tooling-checks job (ci-job-test.yaml's

The guides' layout step of the tooling-checks job (ci-job-test.yaml's
"Check the guides' semantic line breaks"), gated on exactly the files that step
reads, which the guide_layout_files group holds and the job's own filter names too.
The job's other steps read none of the guides, so a run that changed none of them skips
that step.

## docs: docs/site's typecheck, screenshots and build, with the sources its reference

docs/site's typecheck, screenshots and build, with the sources its reference
generators and its screenshot harness read. docs-site.yaml holds those steps, and
both the docs job here and docs-pages.yaml's build job call it. docs-pages.yaml
builds and publishes main's site on the pushes this same list selects: its
on.push.paths, which check-ci-filters.sh holds equal to this filter, so the list
names all three workflow files. The docs job is selected like any other job: no run of ci.yaml
is a push to main.

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
typecheck of the same directory are the tooling-checks job's, which the tooling_checks
filter names scripts/sessions for.

## pi_legion: src/skills-guard.test.ts spawns packages/claude-envoy/scripts/build.ts

src/skills-guard.test.ts spawns packages/claude-envoy/scripts/build.ts (PLUGIN_BUILDER) and
asserts an exact file list over the tree it writes, so this job reads that whole
package and the one entry of `packages/claude-envoy/scripts/build.ts#BUNDLE_ENTRYPOINTS` that
lives outside it. A spawn carries no import, so no derivation of this repository
sees the reach: both patterns are hand-kept, with their declared reads in
ci_job_reads.py.

## pi_legion_build: the skills' guard and what it reads

pi-legion-build runs src/skills-guard.test.ts too, so its gate (.github/ci-gates/pi-legion-build.py)
declares what pi_legion's declares for it: the builder above, and the roots of the guard's sweep
over every file a Legion agent reads (packages/daemon/internal, AGENTS.md, README.md,
docs/kubernetes.md, docs/site's docs and the controller's example manifest). A change to any of
them selects the job, so a skill or prompt change that breaks the guard fails on the pull request,
not first in the merge queue.

## pi_legion: The Dispatch e2e client that scripts/skill-scenarios/seed.ts imports

The Dispatch e2e client that scripts/skill-scenarios/seed.ts imports from
../../../dispatch/e2e/api: the suite reaches it (rig-isolation.test.ts names
e2e/run-server.sh, and the seed.ts it names imports e2e/api.ts), and pi-legion-build's
typecheck compiles it, a program that holds six modules of this directory (api.ts,
harness-ports.ts, harness-broker.ts, harness-processes.ts, harness-real-broker.ts
and harness-signin.ts), so both filters name the directory rather than the two it
reached when this entry was written.

## dispatch_ui: The dashboard's and the proof editor's fast checks (dispatch-ui)

The dashboard's and the proof editor's fast checks (dispatch-ui): what their lint,
typecheck, unit tests and the dashboard's build and asset checks read. No Go and no
Dispatch server: the job runs neither.

## dispatch_ui: The script whose `references)` arm lists the references lane's spec files

The script whose `references)` arm lists the references lane's spec files,
.github/scripts/dispatch-browser-shard-suite.sh: packages/dispatch/e2e/__tests__/reading-walk.test.ts
reads that list out of it and fails on a reading spec the lane does not name, so a change to
that list runs the job that runs the test.

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
condition, so check-ts-filters.sh holds this filter to both. The two suites of
`e2e/__tests__` that import it, `packages/dispatch/e2e/__tests__/delivery-live-probe.test.ts#startFakeDispatch` and
`packages/dispatch/e2e/__tests__/local-instance-support.test.ts#startFakeDispatch`, run in this job's test step and in no other,
so that read is this gate's (ci_job_reads.py's dispatch_ui).

## dispatch_browser: The browser suite (dispatch-browser), which .github/workflows/dispatch-browser.yaml

The browser suite (dispatch-browser), which .github/workflows/dispatch-browser.yaml
holds: the shards, the three `-alone` projects, the two-process job, the
fake-embedder job, the visual review and the merge job. Every entry is something a
browser row reads: the dashboard and its contracts, the Dispatch server its harness
builds, and the two workflow files that hold the steps. The retired `dispatch` job's
filter also held three fixture sets whose only readers are the dashboard's and the
proof editor's unit tests, and they are not here: dispatch-ui's filter names all
three, and with the dispatch_browser_pr output ungated on a pull request they would
start four shard machines and the merge job for a fixture-only change. The
dispatch_browser gate (browser_gate in .github/scripts/ci_job_reads.py) says what this
suite reads, and the changes job computes this filter from it: a path is in this filter
because a browser row reads it and for no other reason. The dispatch_browser_pr gate is
the same helper, so both filters hold the same patterns.

## dispatch_browser: dispatch_ui's set, which the SPA and the e2e helpers load,

dispatch_ui's set, which the SPA and the e2e helpers load, with the bundle of the
dispatch command its rows spawn through `packages/dispatch/e2e/dispatch-cli.ts#dispatchEntry` and `packages/dispatch/e2e/dispatch-cli.ts#runDispatch`, dispatch_two_builds_go's
set (ci_ts_programs.py, LISTED, says why). Its jobs' contracts build
(`build:dispatch`) compiles nothing outside the first: not legion-api.ts.

## dispatch_browser: The workflow that holds every browser step, which this job

The workflow that holds every browser step, which these jobs read as their own
definition - the place every other job of this file gives to its own
`ci-job-<id>.yaml`, so a change to it selects these two jobs and no other gate's: the
reference rows' preview, dispatch-references, which gates nothing, names it in a filter of its
own, whose patterns .github/ci-gates/dispatch-references.py keeps by hand. ci.yaml's
dispatch-browser-pr job runs its pull-request lane on a pull request, the two
projects a change is held to; its dispatch-browser job runs the full lane, every
project and the two-process job, in the merge group, on a dispatched run and on a
pull request carrying `full-ci` or `lane:dispatch_browser`, which is the lane the
nightly (.github/workflows/dispatch-browser-nightly.yaml) calls. So a worker can put
the whole suite on a pull request with the label, and the group is where a change to
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
whose shapes the corpus holds keeps reads it through `packages/dispatch/web/src/__tests__/pmdoc-fixtures.ts#pmdocFixtures` and `packages/dispatch/web/src/__tests__/pmdoc-fixtures.ts#TABLE_PATH`
in `web/src/__tests__`: pmdocFixtures parses the file whose path is the new URL of TABLE_PATH. This
entry declares that read explicitly and selects nothing on its own: the
`*dispatch_server` anchor this filter takes already matches it through its
own 'packages/envoy/internal/dispatch/pmdoc/**', as it does the block schema
`packages/dispatch/e2e/engine-reading.ts#blockSchema` reads. Measured with ci_filter_coverage.changes_filters
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
pods: the packages `go list -deps ./cmd/cipherd ./cmd/dev-oidc-issuer
./internal/cipher/ciphertest/requestseed` lists in packages/envoy beyond the
Dispatch server's, which e2e/run-broker.sh builds in one `go build`.

## dispatch_browser_pr: The browser suite's pull-request lane reads what its full lane reads

The dispatch-browser-pr job calls the same workflow as dispatch-browser, with the
`pull-request` lane: its legs are the same jobs of that file over fewer projects and
rows, so they read what the full lane's read: both gates are browser_gate of
.github/scripts/ci_job_reads.py, so the two computed filters hold the same patterns, each
with its own generated `!` patterns and listed block. This output carries no hold term: the pull-request lane is the rule's one named
exception (.github/ci-notes/ci.md#which-lane-a-job-runs-in-the-pull-request-or-the-merge-queue).

## envoy_go: Its natslive step runs cmd/listener's natslive tests

Its natslive step runs cmd/listener's natslive tests, so its test build reads the
fixtures cmd/listener's untagged tests read (api_test.go), which this job must then
run as well (check-ci-filters.sh's rule 15).

## envoy_go: internal/dispatch/text/whitespace_fixture_test.go holds unicode.IsSpace to this

internal/dispatch/text/whitespace_fixture_test.go holds unicode.IsSpace to this
list, which packages/contracts holds goBlankPattern to: an edit of the list
alone must start both jobs, or the side it disagrees with stays green.

## envoy_go: The three jobs whose tag steps lean on this job's untagged

The three jobs whose tag steps lean on this job's untagged run of ./cmd/dispatch
and ./cmd/cipherd: each runs only what its tag adds, so a change that starts one
must start this job too (check-ci-filters.sh's rule 15).

## envoy_go_build: envoy's Go build, vet, format, generated-code and migration checks (envoy-go-build)

envoy's Go build, vet, format, generated-code and migration checks (envoy-go-build): the
format, generated-contract and pmdoc checks, the vet of the module as darwin compiles it,
the edited-migrations check, and the build and pack of the envoy commands,
with the tests of internal/dispatch/architecture/notes, internal/runnerlog,
internal/dispatch/specgraph, cmd/architecture-check and cmd/spec-check, whose one
run this is (envoy-go's unit step leaves them out), the architecture-note check over
this repository's own notes through the composite action
.github/actions/architecture-check, and the spec-file check over its .dispatch/spec
through the composite action .github/actions/spec-check. gofmt -l . walks every .go
file under packages/envoy, and
`go build ./cmd/dispatch ./cmd/listener` and the darwin vet compile packages of that
module alone, as the two actions' builds of their commands do.

## envoy_go_build: Those two steps judge the files at the actions' default paths,

Those two steps judge the files at the actions' default paths,
.dispatch/architecture and .dispatch/spec. The pattern names .dispatch whole:
main holds no spec file yet, and check-ci-filters.sh refuses a pattern that
matches no file (its rule 9).

## envoy_go_checks: envoy's script tests (envoy-go-checks) and the scripts they drive

envoy's script tests (envoy-go-checks): each step runs a bash harness under packages/envoy
(scripts/, loadtest/ and deploy/scripts/) over fakes, and the harness runs, sources and reads
the scripts, fixtures and compose files beside it, so the filter names that tree whole, with the
workspace install the job's Bun step reads and .github/actions/install-helm, the helm the
cluster suite's render of nats.yaml runs. No step's `run:` starts a go command, so the
workspace's module files are not on it.

## text_checks: The text checks of the three Go modules, the Dispatch

The text checks of the three Go modules, the Dispatch API's ten static source
checks, and the two pure pins of packages/daemon's supervise table: the tests whose
subject is a file's words. Their job runs packages/envoy/internal/textcheck,
packages/envoy/infra/internal/textcheck and packages/daemon/internal/textcheck,
which the three Go lanes leave out of their own runs, those checks' twenty-two tests
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
backs' (`packages/daemon/internal/textcheck/way_back_doc_test.go#assertTheExtractIsTheGuidesBlock`) and the troubleshooting page's refusal table's
(`packages/daemon/internal/textcheck/troubleshooting_refusals_doc_test.go#TestTheTroubleshootingRefusalsExtractIsTheGuidesRefusalTable`), each committed beside the package whose
own check reads it. They are .txt files, which the module's `'packages/daemon/**/*.go'` in the `text_checks` filter does
not match.

## dispatch_memory: `go test -tags memory ./cmd/dispatch`. memory_profile_test.go

`go test -tags memory ./cmd/dispatch`.
`packages/envoy/cmd/dispatch/memory_profile_test.go#TestProfileTheColdEditOfTheHeaviestDocuments` reads three guides only
when DISPATCH_EDIT_PROFILE_OUT is set, which CI never sets, so they are
not here. `packages/envoy/cmd/dispatch/nats_unreachable_test.go#testnats.Start`, which carries no tag, starts the
in-process NATS.

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
that holds each server's socket (`packages/dispatch/e2e/run-server.sh#held_port`, e2e/harness-start.ts). Of
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
proxyrules package and compare it against the model's program, so a change to either
side runs the job. Both sides live under internal/cipher/proxyrules, which one glob
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

## envoy_e2e_local: And @legion/cipher-client with it: the tsc program of

And @legion/cipher-client with it: the tsc program of envoy-client holds all seven
of its modules, and at runtime, which is what this job's bun scripts do,
transport.ts's own closure holds two of them (delivery.ts's holds none), so the
pattern is needed through transport.ts (ci_job_reads.py's envoy_client_dep).

## envoy_e2e_local: The job reads no Markdown file: the listener, the renderer

The job reads no Markdown file: the listener, the renderer and the contracts
between them are its whole input, so a guide written under one of the directory
patterns of the envoy_e2e_local filter (`'packages/envoy-client/src/**'`, `'packages/cipher-client/**'` and the rest) would start it for nothing.

## worker_image: Every file packages/daemon/docker/worker.Dockerfile builds

Every file packages/daemon/docker/worker.Dockerfile builds from, for the worker-image
job (.github/workflows/worker-image.yaml, which the build reads too): the root
manifest, lockfile, patches and every workspace manifest the plugin-deps stage's frozen
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
drives, the nine actions it installs through, and the two files a row's version is
pinned in that the job itself reads (.bun-version for the bun rows and the
worker image's Dockerfile for mise, jj and uv).
packages/envoy/infra/internal/pulumitest/pulumitest.go, whose
`const DockerPluginVersion` the gate holds the pulumi-docker row's version to,
and packages/envoy/loadtest/cluster/render-nats.sh, whose `HELM=` line the gate
holds the helm row's version to, are not here and no step of either job reads
them: a raise of either pin must move its row, and .github/ci-tools.yaml is in
this list.
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
for every host the proof editor's old `github:sjawhar/proof-sdk` route, pinned to a commit, used.
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

## envoy_client: The test that no text states the old rule reads

`packages/envoy-client/src/__tests__/dispatch-who-waits.test.ts#no text states the old rule` reads
every text file git lists under these roots (OLD_RULE_ROOTS) and every AGENTS.md,
the root's and the nested ones among them, so a text there that states the
replaced rule must start this job. The recordings under packages/dispatch/e2e
start it too, though the row leaves them out: this filter takes no `!` pattern.

## envoy_go_race: The shared race job: the closure of `go test -race`

The shared race job: the closure of `go test -race` over delivery, the Dispatch
store, the bus, the listener, the registry store, the session cache, kvwatch, natscred,
the NATS auth service and the sign-in code flow (`go list -deps -test -race` in
packages/envoy), and every .md file under packages/envoy, which internal/dispatch/store's
`packages/envoy/internal/dispatch/store/search_text_test.go#TestSearchTextKeepsEveryLexeme` walks (its //ci:reads line declares
packages/envoy/**/*.md). The
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

## private_names_port: The private-name port harness, in a job of its own: it

The private-name port harness, in a job of its own: it builds the `legion` binary
of this head (.github/scripts/check-private-names-port.test.sh, its `go build` of
./cmd/legion) and holds the rules of .github/private-names.toml, as that binary
runs them, to the frozen shell script's verdicts, case for case. That build is why
this list is as long as it is - legion_command is what the binary is made of - and
why the step has a job: while it was the test job's, the whole closure sat on the
`test` filter, so a change to one package of the daemon ran thirty-odd shell
harnesses and the Bun suites of scripts/ with it (dispatch://INFRA-1413).

## private_names_port: The rule file the harness names to the binary

The rule file the harness names to the binary in LEGION_PRIVATE_NAME_RULES_FILE
(.github/scripts/check-private-names-port.test.sh, its `rule_file` and the line
that exports it), since the binary embeds none, and reads each rule's description
out of (its `description=` line).

## private_names_port: test-lib.sh, which the harness sources for check/is/summary

test-lib.sh, which the harness sources for check/is/summary (its `source` line), and
the frozen script it runs every case through beside the binary, symlinked into each
fixture (its four `ln -s "$frozen_script"` lines). That script is the subject of
the check_private_names_test filter, which gates another job's step: this job reads
it as the engine it compares against.

## dispatch_references: The reference rows' preview (dispatch-references): the eleven spec files

The reference rows' preview (dispatch-references): the eleven spec files
the reference framework's Verification document names and the nine of the reading
list's walk, which the `references` lane of dispatch-browser.yaml runs on Chromium, and
what they test. Narrow on purpose: unlike every gate's filter, which the changes job computes
from its job's reads, it names less than its job reads (the Bun
install, the Dispatch server's whole closure, the rest of the dashboard, the e2e harness every
spec shares, ci.yaml). That job gates nothing, so a change this list misses loses
a preview and no check, and the full lane the merge queue runs through the
dispatch_browser filter, which holds every read, runs every
one of these rows. It names the reference framework, the shared field, and the one
file of each covered box of the writing list, whose change can take a box's picker
away, and no more: over the 200 newest pull-request runs of 2026-11-13, a list that
also named the six feature directories mounting the field and the shared harness
selected about half of them (about 300 runs and 6,000 runner-minutes a day), so the
four shard machines start for a pull request that changes the references or a box
that holds the picker, and not for every dashboard change.
For the same reason its file of .github/ci-gates, dispatch-references.py, declares it EXCLUDED
(`EXCLUDED = excluded(...)`, its patterns written by hand) rather than as a gate, so the changes
job computes neither `!` patterns nor a listed block for it, as it does for a gate's filter. It
names neither ci.yaml nor its own file either, which every gate's filter reads, for the same
reason.

## dispatch_references: The workflow that holds the lane's jobs, and the script that holds its spec files

The workflow that holds the lane's jobs, and the script that holds its spec files:
.github/scripts/dispatch-browser-shard-suite.sh, the shard job's browser step, whose
`references)` arm lists them, so a change to that arm changes what the lane runs. Beside them
are the two scripts the merge job hands its caller's id, dispatch-references for this lane:
.github/scripts/dispatch-browser-reading-rows.sh keeps the reading to that caller's jobs, and
.github/scripts/dispatch-browser-latest-blobs.sh reads that caller's uploads by its prefix, so a
change to either changes what the lane reports.

## dispatch_references: The eleven spec files, and the helpers only they share

The eleven spec files, and the helpers only they share: the reference surfaces'
own helpers, the search helpers and the writing list's walk. check-ci-filters.sh
(rule 24) fails on a spec file the `lane_files` of the `references)` arm of
.github/scripts/dispatch-browser-shard-suite.sh runs that this list misses.

## dispatch_references: The server side of references

The server side of references: the text and reference resolution, the document
model, and the API files that answer search, references, comments, replies and
which sessions Dispatch holds a record of.

## dispatch_references: No browser row compiles a Go test file

No browser row compiles a Go test file, so a change to one of the packages above
that touches only its tests starts no preview. The changes job's some-with-excludes
quantifier reads a `!` pattern per changed file.

The sections from here on are `.github/workflows/ci.yaml`'s own, the router's, in the order
their comment blocks stood in it.

## The router: every check a pull request or the merge queue runs

The router: every check a pull request or the merge queue runs, in one workflow.
Its first job, `changes`, reads the paths the change touched and selects, through one
dorny/paths-filter filter per job, the jobs that read those paths; each job runs only when its
filter matches (lint runs on every change, and typecheck, test, tooling-checks, daemon-go,
plugin-pack, private-names-port, sessions-script and text-checks also run when `changes` itself
failed). A pull request runs the fast jobs its paths select, and the merge queue runs all of
them (.github/ci-notes/ci.md#which-lane-a-job-runs-in-the-pull-request-or-the-merge-queue).
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
at once when that job's own result decides a merge-group run of the queue's front group, so
the queue drops a doomed group at the first failure rather than at the end of the run; a group
behind the front posts nothing early, since the queue cannot drop it before the groups in front
of it end (.github/scripts/ci-ok-report.sh's header says what that costs), and the ci-ok job
holds a failing verdict of such a group until the group is the front
(.github/scripts/ci-ok-hold.sh), so the queue builds nothing on a failed entry; on every other
run the ci-ok job's last step then
sets every ci-ok check run on the head that is not its own to its own conclusion, so one
verdict stands at the end of every attempt. pr-title, the other check that ruleset requires, is
the `pr-title` job at the end of this file, in the same shape and for the same reason: it runs
check-pr-text.sh on a pull request that is not a draft and on the merge queue's group, the two
events this workflow runs that carry a pull request's text, and its own check run is the report,
so it posts nothing. pr-title.yaml carries that same check when the pull request is edited, the
event this workflow does not run on, as its own job's check run too; its header says why it runs
on nothing else.
The docs site's build on a push to main, and its GitHub Pages
deploy, are docs-pages.yaml's, a workflow of its own whose header says why. Its paths are the
docs filter the changes job computes, which `check-ci-filters.sh write` writes and rule 8 holds.

What a workflow file of this repository holds, and where its filter data, its longer shell
bodies and its prose go instead, is the rule .github/ci-notes/README.md writes down. Each
`changes` output that hands on a filter's verdict reads `steps.filter.outputs.<name>`, and
rule 19 of `check-ci-filters.sh` holds `<name>` to a filter a file of `.github/ci-gates` declares.

A new job joins the router in eight places: a gate in `.github/ci-gates/<job id>.py` that names
the go commands the job runs and every other file it reads, from which its filter is computed, an output of
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
nothing about it, and `//ci:` lines at the top of each test file the job compiles that reads a
file outside its package. An output an event must force is written in `changes`' outputs
rather than left to its filter, as every output's every-job case is
(.github/ci-notes/ci.md#what-a-dispatched-run-of-a-branch-selects-and-when-to-ask-for-every-job),
and so is one an event must hold back, as a slow suite's pull-request case is
(.github/ci-notes/ci.md#how-a-slow-suite-is-held-back-on-a-pull-request).
The ci-filters job's .github/scripts/check-ci-filters.sh holds the filter to that gate,
CI_OK_SELECTED_BY to the `if:`s, the report step to every job, the ci-ok job's `needs:` to
every job of the file but the exempt ones, and the re-run term to every job but `ci-ok` and
`pr-title`: each of those two is named for a check main's ruleset requires, where a skip is a
passing required check, so each refuses such an attempt in a step of its own instead.
A job split off another keeps its steps in that job's file instead: its caller passes
`job: <its id>` and its steps' job carries `if: inputs.job == '<its id>'`
(.github/ci-notes/ci.md#two-jobs-that-call-one-file).
docs/solutions/github/join-a-job-to-the-ci-router.md walks through it.

Caches: no step of the jobs this workflow calls saves an Actions cache entry itself; three
actions its jobs call can save one, and only in a dispatched run (setup-bun, and
architecture-check and spec-check at this paragraph's end). dispatch-two-builds restores the two
earlier Dispatch builds it runs (ci-job-dispatch-two-builds.yaml) and builds one on a miss;
ci-caches.yaml's dispatch-builds job fills both on main (the two-build section below says why).
setup-go caches nothing here (`cache: false` on every step, the two check actions'
included). The jobs that cache Go restore, in steps of their own, one of seven families with
actions/cache/restore: go-daemon (typecheck, daemon-go, plugin-pack — which compiles the same
internal/daemon test binary daemon-go's race step does — and pi-legion; the test job restored
it while it ran the daemon's Go suite and does not now), go-docs (docs, and the build job
of docs-pages.yaml, which saves it), go-envoy (envoy-e2e-local, envoy-session-stall and
envoy-refill, which build the same listener and natsd, listener-server-change and
envoy-go-build, which build the listener, envoy-go-checks, whose script tests run under the
setup they had in envoy-go-build's job, and envoy-go-race-docs), go-envoy-test (envoy-go, which
builds the test binary of every package of the envoy module, envoy-go-claim-model, which builds
the API's of that set, the binaries, multiprocess
and visual jobs of dispatch-browser.yaml, which build the Dispatch server and the real
broker's commands out of that same set, and the two jobs of ci-job-dispatch-two-builds.yaml,
whose sizing step and twobuild test step build out of it too), go-envoy-darwin-vet
(envoy-go-build's darwin vet, whose step's env gives it a build cache directory of its own,
so the standard library and the envoy module's dependencies compiled for darwin ride in
neither the go-envoy nor the
go-envoy-test entry, which the other envoy jobs download), go-envoy-infra (envoy-infra-build
and envoy-infra-go) and go-notices (lint's notices step). Every family but go-docs is filled by
.github/workflows/ci-caches.yaml, which builds those sets on a push to main and saves them;
go-docs stays with docs-pages.yaml, which saves it on that same push. Each family is kept per
runner architecture, keyed on the runner's architecture and on the Go version its job's
setup-go installed: a build cache holds the objects of the architecture that compiled them, so
an entry an x86_64 runner filled would give a job on ubuntu-24.04-arm its modules and none of
its objects, so each family is filled on every runner one of its jobs runs on.
go-notices puts the architecture before the OS in its key and the other families put it after:
its entry holds go-licenses, a program lint's notices step runs, and a run of a workflow file
older than the architecture in the key restores by go-notices-<os>-, which an <os>-<arch> key
starts with (ci-job-lint.yaml's restore step says what that did). go-daemon, go-notices,
go-envoy, go-envoy-test and go-envoy-darwin-vet are filled on every push to main: the fill
restores the newest entry
of the family, runs on it the build the job runs, which compiles only what changed since, deletes
from the build cache every entry that build did not use (.github/scripts/go-cache-working-set.sh;
without it an entry grew with each fill, go-envoy-test from 690 MB to 2,039 MB in a day, and every
job that restores it downloaded the growth), and saves the result under the job's key followed by
the commit (`-${{ github.sha }}`). No entry
carries the job's key itself, so GitHub's matching gives each job the newest entry whose key
starts with it, the last fill's, and ci-caches.yaml's refill-superseded job then deletes every
other entry of the family, so each family keeps one entry per runner, as before. A job that is
downloading a deleted entry at that moment gets a cache miss: actions/cache/restore warns and the
job goes on, compiling what the entry would have given it, slower but not failed. The keys of
the first four named only the Go version before, and the fill saved only when that exact key was
missing, so an
entry was filled once per Go version: on 2026-11-22 every job restored go-daemon, go-envoy and
go-envoy-test entries saved on 2026-11-18, and at main's e7381b7d4c9 a job that restored them
compiled again 312 of the daemon module's 335 packages and 573 of the envoy module's 618, against
135 and 321 from an entry five merges old. The key still holds no go.sum hash: a go.sum change
downloads only the changed modules, and Go's build cache is content-addressed, so the next fill
compiles only what changed. go-envoy-infra keeps a key with the hash of its module files, filled
only when that key is missing, and ci-caches.yaml deletes the entry each such fill replaces (its
envoy-infra job says how); its save takes the key its lookup computed, since a hash of the module
files taken after the build drifted from the lookup's and made every push build it again (run
45088325195). What changed under it in eleven days had cost its linux vet 83 to 85%
of what the same vet for darwin took with nothing cached. Since no entry of the five refilled
families carries a job's key itself, every restore of them takes the newest entry by prefix, on
main too: the key is a prefix of the fill's keys, so the dispatch-browser.yaml jobs that hold
their restore-keys off main still find the last fill's entry. Those restore-keys were held off
main so that the entry a run saved after a Go bump held nothing the older Go built, and no step
of this workflow saves a family any more (the two check actions below fall back only to an entry
of their own Go version, and a ci-caches.yaml fill's restore-keys hold its Go version; its key's
own prefix match can reach go1.27.30 for go1.27.3, whose objects Go keys on that other toolchain
and so never uses). There is no browser cache beside the Go
families any more: dispatch-browser.yaml's shard and alone jobs run in the image this
repository builds, which carries Chromium, WebKit and Firefox at /ms-playwright, so a
~/.cache/ms-playwright restore would carry nothing they do not already have, and
ci-caches.yaml's browsers job, the only writer of that entry, is gone with the restores
(dispatch://INFRA-1443). setup-bun reads and writes its cache only on main
(.github/actions/setup-bun), which of this workflow's events is the dispatched run alone, and
not in the tool-fallback and tool-fallback-arm jobs at all: that action reads the id of the
job that called it and passes no-cache for those two on every ref, so a restored Bun cannot
stand in for the install each of their cases is a reading of. An eighth family,
`architecture-check-<os>-<arch>-go<version>-<hash>`, is the composite action
.github/actions/architecture-check's own, and a ninth,
`spec-check-<os>-<arch>-go<version>-<hash>`, is .github/actions/spec-check's; envoy-go-build runs
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
    one is installed from its `-aarch64` row there, as changie (test and tooling-checks), crane
    (ci-images), mise and jj (pi-shared) and elan (airlock-lean) are;
  * the pinned Oh My Pi build, which .omp-pin names and whose release ships linux-x64 alone:
    daemon-go, plugin-pack, pi-envoy and pi-legion (pi-legion-build installs none, and runs on
    arm64);
  * a browser: dispatch-browser, dispatch-browser-pr and dispatch-references (the container of
    their shard and alone legs is an image built for linux/amd64 alone), dispatch-two-builds and
    docs (Playwright's Chromium on the runner's own libraries);
  * an image of this repository built for linux/amd64: worker-image, browser-image, and
    envoy-smoke;
  * envoy-go-build, whose pack step builds the release's listener archives with no
    CGO_ENABLED, as release-envoy-listener.yaml does on ubuntu-24.04: Go's default turns cgo on
    for a native build and off for a cross build, so only an x86_64 runner builds the release's
    own pair (ci-job-envoy-go-build.yaml says what the arm64 runner built instead);
  * envoy-go-checks, whose script tests drive this image's own `docker compose` and
    `locale-gen` and have run on no other image;
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
selects every job there. A dispatch of any other branch (`gh workflow run ci.yaml --ref <branch>`)
selects what that branch's diff with its merge base with main touches, through the same filters a
merge group reads, unless it asks for every job
(.github/ci-notes/ci.md#what-a-dispatched-run-of-a-branch-selects-and-when-to-ask-for-every-job),
and it reports under names of its own
(.github/ci-notes/ci.md#why-a-dispatched-run-off-main-reports-under-names-of-its-own).

## What a dispatched run of a branch selects, and when to ask for every job

A dispatched run of any branch but exactly refs/heads/main selects the jobs its own commit's diff
with its merge base with main touches: the filter step runs paths-filter over that diff with
the filters and the predicate a merge group reads, and every output takes the filter's verdict
as a merge group's does, the slow suites' included, since their `full-ci` term holds a suite
back on a pull request alone. So a branch that touched only a shell script runs that script's
harness step, lint and ci-ok, and not the Go suites, as the merge queue would. A diff that
touches ci.yaml still selects every job, since every filter names it (`ci_router`), and so does a
branch so far from main that its diff reaches every filter; a diff that changes how the filters
are computed selects every job whose filter it moves, as a merge group's does (below). The bypass check's
dispatch on main still selects every job: its run answers for a push the queue never tested, and
the push's range, not one diff, is what it must cover.

The `every-job` input (`gh workflow run ci.yaml --ref <branch> -f every-job=true`, default off)
selects every job on a branch too, as a run of main does. Use it only when the result needed is
of a job the branch's diff does not reach: checking that a suite still passes on the branch's
tree after a change outside its filter (which is a missing read in that job's gate under
.github/ci-gates to fix as well), or a full run to compare against main's. A test of the branch's own change needs no
input: its diff selects every job that reads what it changed. A branch with an open pull request
gets its result from the pull request's run and the merge group, and needs no dispatch at all.

The changes job's step "Read whether this run selects every job" decides it, and writes
every_job=true only on a workflow_dispatch run whose ref step read exactly refs/heads/main, whose
`every-job` input is true, or whose ref is no branch: a tag's checkout can hold a tag named main,
which paths-filter would read as its `base` and diff the commit with itself, so a tag dispatch
keeps every job as before. On every other run it writes every_job empty, which an expression
reads as false; it never writes `false`, a non-empty string an expression reads as true. Every
output opens with `steps.scope.outputs.every_job && 'true' ||`, and the filter step runs on `${{
!steps.scope.outputs.every_job }}`. Rule 19 of .github/scripts/check-ci-filters.sh refuses a
changes job without the step and an output that does not open with that term, so main's older
`github.event_name == 'workflow_dispatch' && 'true' ||` term, which a merge can carry back, fails
the check rather than selecting every job on every dispatch of a branch.
.github/scripts/ci-router-outputs.test.sh evaluates ci.yaml's own steps and outputs
(.github/scripts/ci_router_outputs.py) and holds them to this: a dispatch of main, with every
filter saying no, selects every job; a dispatch of a branch selects for each filter exactly what
a merge group with the same verdict selects, and every job when every filter says yes; a tag
dispatch and `every-job` select every job.

The changes job's step "Find the merge base with main a dispatch of a branch is measured against"
runs on such a dispatch and on no run that selects every job. It runs
.github/scripts/edited-migrations-base.sh, which finds the same commit for envoy-go-build's
migration check: it fetches main into refs/remotes/origin/main and both histories deep enough that
no newer common commit hides past the fetch, and prints the merge base, or fails the step when
the two histories share none. The filter generation is handed that commit as `CI_FILTERS_BASE`, as
a pull request's and a merge group's base are, so a branch whose diff changes how the filters are
computed is held against the merge base's own filters, and each filter it moves selects every path
(.github/ci-notes/ci.md#the-changes-job-computes-its-filters-when-it-runs). Handed none, a dispatch
of a branch would be the one run selected by the branch's own engine alone: a read dropped from a
helper every Bun gate shares would skip those jobs in the very dispatch meant to test the drop.
The step stays off a dispatch of main, where the script exits 2 for want of a migration-base.
The step must come before the generation step: the runner evaluates a step's `env:` when that
step starts, so a generation step that ran first would read `steps.base.outputs.sha` unwritten and
be handed no base. ci-router-outputs.test.sh holds the base the generation is handed on each
event, reading the generation's `env:` where the step starts (.github/scripts/ci_router_outputs.py
evaluates each step there), and three copies of ci.yaml fail it: one whose generation step reads
no `steps.base.outputs.sha`, one whose base step runs on a dispatch of main, and one whose base
step comes after the generation step.

.github/workflows/browser-image.yaml's header still says a workflow_dispatch run of ci.yaml forces
the browser_image output; on a dispatch of a branch the filter decides it now. That file is an
input of the browser image's recipe hash (.github/scripts/ci_images.py's recipe_sha256), so an
edit of its text, a comment included, moves the hash off the pinned image's label and fails
ci-images until the image is rebuilt: the header is corrected with the next change that rebuilds
the image.

## The commit main pointed at before the push the bypass check read

The commit main pointed at before the push the bypass check read: envoy-go-build compares
every migration it holds with this run's tree, the range the push landed. A dispatch on
main without it fails that check, which cannot tell what the run lands. A dispatch on
another branch without it compares with the branch's merge
base with main (.github/scripts/edited-migrations-base.sh).

## What a newer push cancels, and what is never cancelled

A newer push to a pull request cancels its older run, waiting or running. On pull_request the key
is the pull request's number: tests-closed.yaml joins this same group when the pull request
closes, cancelling what still waits or runs, and a merged pull request's closed event carries
github.ref = the base branch, so the number is the only key that names the pull request on every
event. A reopen within about a minute of the close can race tests-closed.yaml's run:
tests-closed.yaml says what happens and what to do. A merge group is a group of its own per
commit (its head) and is never cancelled: the queue waits for its ci-ok. A dispatched run of a
ref other than main keys on that ref (`ci-refs/heads/<branch>`) and cancels the ref's older
dispatched run, waiting or running: the newer dispatch tests the ref's newer state, and the
older run's `ci-ok (dispatch)` is required by nothing
(.github/ci-notes/ci.md#why-a-dispatched-run-off-main-reports-under-names-of-its-own), so a
cancelled one blocks no merge. A re-run of an older dispatched run of the branch (`gh run rerun`)
joins that same group, so it cancels the branch's newer dispatched run, waiting or running, and
tests the older commit in its place: re-run only the newest dispatch of a branch, or dispatch
again. A dispatch of main keys on the commit it runs at and is never
cancelled: each bypass check's run answers for its own push. That one comparison is GitHub's
`github.ref != 'refs/heads/main'`, which ignores case, so a dispatch of a branch spelled
refs/heads/Main keys on its commit and is never cancelled either; it errs toward running, and
the selection reads the ref step's exact shell test, not this. GitHub compares concurrency group
names without regard to case too, so two branches whose names differ only in case share one
group, and a dispatch of one cancels the other's.

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
file but these four. `pr-title` is the other check main's ruleset requires, carried by that
job's own check run: its result is nothing the verdict waits for, it carries no report step,
and the queue reads it as a check of its own, so a verdict that needed it would only make one
required check wait for the other. `dispatch-references` runs a slice of the browser suite on
a pull request for feedback alone: the merge queue's full lane gates every row of it already,
and a required check is the repository owner's to change, so it carries no report step and
the verdict waits for nothing of it
(.github/ci-notes/ci.md#the-reference-rows-preview-job
says the rest).
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

## Why the docs output is selected like any other

Selected like any other job now that no run of this workflow is a push to main.
docs-pages.yaml still builds and deploys main's site, on a push trigger of its own.

## The fast half of a split job comes back on every push

tooling_checks, pi_legion_build and envoy_infra_build select the halves of three jobs that a
pull request runs: the builds, lints, typechecks, vets and fast tests that shared a job with a
slow suite until that job was split in two
(.github/ci-notes/ci.md#which-lane-a-job-runs-in-the-pull-request-or-the-merge-queue). No hold
term, only the every-job one, so they come back on every push, as envoy_go_build does
(.github/ci-notes/ci.md#why-envoy-gos-build-and-checks-come-back-on-every-push). The slow half
keeps the old job's id and output and takes the hold term: test, pi_legion and envoy_infra_go.
Each half runs all its steps wherever it runs, or its own output skips it, so no check of
either reads `success` for a step that did not run.

## Why the daemon's Go suite has an output of its own

The daemon's Go suite, a job of its own since dispatch://INFRA-1413: the test job used
to carry it behind the `test` output, so a change to a shell script ran it too. It is a slow
suite (.github/ci-notes/ci.md#how-a-slow-suite-is-held-back-on-a-pull-request): 43.1 minutes at
the 90th percentile of a pull request's run on 2026-11-22.

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
that selects any of that job's harness steps.

## One list for the test job's step gates

Each `*.test.sh` step of the test job, and tooling-checks' guides layout step, has a filter of its
own naming that harness and the script it tests (the guides' step, the guides), so the step runs
when either changes and skips when neither does. Each job's own filter names every one of these
lists, since a step gate decides nothing in a job that was not selected.

The steps read those filters out of one output of the changes job, `matched_filters`: the
paths filter's own `changes` output, the JSON array of every filter that matched
(`["release_push_test","test"]`, written by JSON.stringify), or `all` on a run that selects every
job (a dispatch of main, a tag's, or one given `every-job`), whose filter step is skipped, and on
a run whose filters fell open: the fallback file
(#the-changes-job-computes-its-filters-when-it-runs) is named from the changes job's outputs and
holds none of these filters, and the generate step's `every_filter` output, which the script
writes `true` with that file, turns the list to `all`. Without that term every step gated on the
list would be skipped on exactly the run whose filters could not be computed, which is the run
the filter engine's own harnesses must run in. A dispatch of a branch runs its filter step over
its diff with its merge base with main, so its list is that step's and it runs the harnesses its
diff reaches, as its merge group would
(.github/ci-notes/ci.md#what-a-dispatched-run-of-a-branch-selects-and-when-to-ask-for-every-job):
the output opens with the every-job term, `steps.scope.outputs.every_job && 'all' ||`, as every
other output does, and a `github.event_name == 'workflow_dispatch'` term there would run every
harness on every dispatch of a branch. Each of ci-job-test.yaml's two callers hands it on as one
input, `matched-filters`, and the whole of each gated step's condition is
`inputs.changes-result != 'success' || inputs.matched-filters == 'all' ||
contains(inputs.matched-filters, '"<filter>"')`, with one more `|| contains(...)` term for each
other filter whose match also runs the step (the step that fetches the computed filters runs for
either of the two harnesses that read them). The quotes make the `contains` match one whole
name of the array: `ci_tools_test` without them would match `mirror_ci_tools_test` too. Before,
each gate was an output of the changes job and a `with:` line of its calling job, about 197
bytes of ci.yaml per gate, in a file .github/scripts/check-workflow-files.sh holds under 64,000
bytes; replacing the 50 of them with this one output took 10,512 bytes out of the file, and a
gate added now costs ci.yaml nothing.

.github/scripts/check-ci-filters.sh reads a step's filter out of the `contains` as it reads one
out of an output (rules 2 and 3, both through one reader, `gated_filters`); rule 2 asks an
output of the changes job only of a filter read through one, since the list hands on every filter
that matched. It holds the shape:
rule 19 holds `matched_filters` to its one value, `all` on a run that selects every job and on a fall-open
run included, and its job to one step of id `generate` running the generation; it refuses a
gate declaring a filter named `changes`, under which the paths filter would write that filter's
verdict in place of the list, or `<another filter>_count` or `_files`, names the paths filter
writes those filters' counts and files under, and refuses a filter name JSON would escape or two names that
differ only in case, which the quoted `contains` (GitHub's ignores case) could not tell apart;
rule 23 refuses a condition naming the list that is not that whole shape (`!= ''`, a `!` before
a `contains` or a term after the last, a `contains` without the quotes, a missing `all` or
result term), a job-level
read of it, and, in a condition or in any other `${{ }}` of a job, a spelling of the changes job
or an input it does not read (`needs['changes']`, `outputs['x']`, `INPUTS.x`, `inputs['x']`,
`toJSON(inputs)`). It also refuses every other read of a value that carries every filter's
verdict - the list, `filter_verdicts`, the ci-ok payload (`toJSON(needs.changes)`, which holds
both), `toJSON(needs)` and every job's outputs through `needs.*` (but `needs.*.result`): in a
condition but the payload's `!= ''`, which asks only whether ci.yaml is the caller, and in a
`run:`, `env:` or `with:` but ci.yaml's own `with:` to a called workflow, the ci-ok report
action's `changes:` and the ci-ok job itself. A step that read one of them for a filter
(`case '${{ inputs.matched-filters }}' in *apx*)`) would be gated on a filter no rule reads. No
job is gated on the list: ci-ok's CI_OK_SELECTED_BY maps a job to the output that is `'true'`
when changes selects it, and the list is never `'true'`, so each job
keeps an output of its own and that map needs no change.
.github/scripts/ci-router-outputs.test.sh evaluates each gated step's `if:` through the `with:`
that ci.yaml's calling job passes, as GitHub would (`ci_router_outputs.py --steps`): with each
filter matched alone, the steps that run are exactly the steps naming that filter, a dispatch of a
branch hands on its filter step's list and runs the steps it names, and every one runs on a run
that selects every job, on a fall-open run and when changes did not succeed.

## Why the browser image's build carries no full-ci term

The browser image's build. No `full-ci` term, and none is wanted: the image is what the
shard and alone jobs of .github/workflows/dispatch-browser.yaml pin, so gating its build behind a label would leave a pull request that edits
the Dockerfile writing a row whose digest the package does not hold, which the
ci-images job then refuses.

## The browser suite's two outputs, one per lane

Two outputs over one set of reads (browser_gate in .github/scripts/ci_job_reads.py, which
both gates are), one for each of the two callers of
.github/workflows/dispatch-browser.yaml that ci-ok needs. dispatch_browser_pr selects the dispatch-browser-pr job,
which runs the `pull-request` lane, the two projects and the `@pr` rows a change to the dashboard
is held to: no hold term, so it comes back on every push of a pull request its paths select.
dispatch_browser selects the dispatch-browser job, which runs the full lane, a slow suite held
back as the others are, with its own label `lane:dispatch_browser`. A job runs whole or not at
all, so the two lanes are two jobs, and their checks carry the caller's id:
`dispatch-browser-pr / shard (1)` on a pull request, `dispatch-browser / shard (1)` in the merge
queue. .github/ci-notes/ci.md#what-the-lane-input-reads says what each lane runs. The third
caller, dispatch-references, runs the `references` lane behind an output and a narrower filter
of its own, and gates nothing
(.github/ci-notes/ci.md#the-reference-rows-preview-job).

## Which lane a job runs in: the pull request or the merge queue

A pull request's run is a fast filter: the builds, the vets, the format, typecheck and lint
checks, and the fast tests. The merge queue runs every job its paths select on the tree the
change lands as, so a slow test runs once, in the queue, and main stays green. The rule that
sorts a job, read off its pull-request runs (attempt 1, every leg of the job that ran succeeded,
a job's time from its first leg's start to its last leg's end; the numbers below are
2026-11-22's):

- A job whose pull-request run takes 10 minutes or less at the 90th percentile runs on a pull
  request wherever its paths select it.
- A build, vet, format, typecheck or lint check runs on a pull request whatever its job's time,
  so a change that does not build or is not formatted fails while its author still has it.
- Every other test is a slow suite: the merge queue runs it, and a pull request runs it only
  under the label `full-ci`, or `lane:<output>` for that one job. A slow suite is held back
  through its job's `changes` output
  (.github/ci-notes/ci.md#how-a-slow-suite-is-held-back-on-a-pull-request). No job runs in part:
  a job that held a slow suite beside steps a pull request runs is split in two, the slow half
  keeping the job's id and output and the other half a job of its own, so each runs all its
  steps wherever it runs
  (.github/ci-notes/ci.md#the-fast-half-of-a-split-job-comes-back-on-every-push).

What that gives, with each job's 90th percentile in minutes:

- held back: daemon-go 43.1, envoy-go 39.1, envoy-go-race-api 36.4, envoy-go-race-docs 33.7,
  dispatch-memory 32.5, envoy-go-race 29.8,
  envoy-go-claim-model 21.1, dispatch-multiprocess 18.7, envoy-go-checks (24.0 as the one job
  it was; its script tests 17.4), pi-legion (14.8 as one job), envoy-infra-go (14.0 as one
  job), test (12.3 as one job), dispatch-browser (the full lane), dispatch-two-builds 13.7 and
  envoy-smoke 11.4;
- on the pull request: envoy-go-build 6.8 and the halves split off the jobs above -
  tooling-checks 1.0, pi-legion-build 0.8 and envoy-infra-build 3.0 - dispatch-browser-pr 14.2,
  and every other job, each at 9.8 or less.

dispatch-browser-pr is the one exception the rule names: at 14.2 minutes it is above the line,
and it stays on the pull request, since its rows tagged `@pr` are the ones a change to the
dashboard is held to (.github/ci-notes/ci.md#the-browser-suites-two-outputs-one-per-lane).
A job whose numbers cross the line moves class by the same rule. dispatch-references is in
neither class: it is outside ci-ok, runs on a pull request alone and never in the queue, whose
full lane already runs every row of it
(.github/ci-notes/ci.md#the-reference-rows-preview-job).

What this accepts: a slow test that would have failed the pull request now fails first in the
queue, where the failure takes the entry out and the queue rebuilds the groups behind it. Every
build, vet, format, typecheck and lint failure, and every failure of a fast test, still comes
back on the pull request.

## How a slow suite is held back on a pull request

A slow suite (.github/ci-notes/ci.md#which-lane-a-job-runs-in-the-pull-request-or-the-merge-queue)
is held back through its job's `changes` output. The merge queue runs it on the
tree the change lands as, and that is where its result normally comes from; a pull request runs
it only when it carries the label `full-ci`, or that one when it carries its `lane:<output>`
label (.github/ci-notes/ci.md#a-lane-label-turns-on-one-slow-suite-on-a-pull-request). Each such output is false on a pull-request run without the label, so
the skip is one `changes` chose: ci-ok reads it as it reads any other output, and no job's `if:`
knows about the label. The label is read
from the pull request the run's own event carries, so it takes effect at the next push,
reopen or ready-for-review, not when it is added: this workflow takes no `labeled`
trigger, which would start a run that joins this workflow's concurrency group and cancels the
pull request's run in progress. Take the label off once the run it was added for passes,
since every later push would run the slow suites again, unless a `ci_check` of the issue
names one of them: without the label that job's own check run (`test`) concludes `skipped`,
no job of the file it calls starts, and the run the check names (`test / test`) is absent.

## A citation names a symbol, never a line

A line number names whatever line a later edit moves there, and nothing reads a citation of a
file the citing change does not touch, so a line-number citation in the CI's own files drifted
with the next merge that edited the cited file (dispatch://INFRA-4017), and so did the line
numbers that the //ci: lines of the test files under packages/ cited. A citation under .github,
or in a //ci: line outside it, is therefore one exact token on one line: a backtick, the path of
a tracked file from the repository root as git ls-files prints it, a hash, a name, a backtick, as
in `.github/workflows/ci.yaml#browser_image`. Several names are several tokens, each with its
path.

.github/scripts/ci_citations.py reads every tracked text file under .github, and each //ci: line
of every tracked text file outside it, as plain text, and claims this much and no more:
- a token whose path is no tracked file fails, and so does a token whose name its file's code
  does not hold. The code is the cited file as its language's own scanner reads it: Python's
  tokenize, the typescript package's parser for TypeScript and JavaScript (one bun run of
  .github/scripts/ci_citations_code.ts), go/scanner for Go (one go run of
  .github/scripts/ci_citations_code.go), and for shell, YAML and the other hash-comment kinds a
  pass that removes a hash comment outside quotes. So a rename fails the change that makes it
  even when a comment keeps the old name. In Python, TypeScript, JavaScript and Go an identifier
  must be one of the file's identifier tokens, so a string or a Python docstring that keeps the
  old name holds nothing either. Any other name, such as a quoted test title, must be text of
  the code whose ends run into no longer word: a letter, a digit, an underscore or a hyphen
  right beside an end that is one of those means a longer word, so setup-go is held by neither
  setup-golang nor setup-go-x;
- a code span that starts with a path word and goes on with a hash, a colon or two colons and a
  name, and is no token, fails: a citation in that span is either a token, which is held, or
  refused, never skipped. A code span is read as CommonMark pairs one, from its backticks to the
  next run of as many, over every line of its paragraph, so a span over three lines is read as
  one over two is. A token wrapped onto another line is refused rather than joined, for a token
  is one line so that a search for it finds every citation of its name, and where a wrap fell,
  at a space or inside a word, and what comment marker it added, is a guess. In a span's line
  break a comment marker is one only where a space or the line's end follows it, so a token
  wrapped just before its hash is refused too. A path word followed by a space alone is a
  command, such as `.github/scripts/release-push.sh release`, and is read as prose;
- a path word followed right away by a hash and a name, outside a token, fails: a token with its
  backticks left off, or one inside a code span that other text starts. A Markdown file's path,
  a hash and a heading's anchor, as this note's links are written, is a link and passes. Inside
  a code span that the path does not start, a colon after it is a command's or an annotation's,
  such as ::error file=ci.yaml::x, and is read as prose, as a space before a hash is;
- a line number written beside a path word fails, in each spelling its docstring lists, a line
  that wraps between the path and the number included. A spelling it does not list passes: a
  colon and a space before the number, an @ before it, "ln" or "around line" are none of them.
  The rule is the listed spellings, not every way to write a line;
- a backticked text that is no token fails only where it stands beside a path word: right after
  it (after nothing, a possessive, a comma, a colon or an opening parenthesis, on the path's line
  or wrapped onto the next), or followed by in, of or from and the path word, because either is
  the shape of a name written beside its path, which no check could hold. A citation is
  rewritten as a token, and a text that is no citation is reworded so that it does not stand
  beside the path. Any other backticked text is read as prose and is not checked, so a citation
  written further from its path, as in "ci.yaml holds, among others, the `x` output", is
  neither held nor refused. That is the stated limit of this rule, and the test shows it.

A path word is the path of a tracked file, the end of one that holds a slash or a dot, or a
capitalised file name such as CODEOWNERS. A backticked text with a hash in it that is no citation
reads as a token whose path names no file, so it is written without the backticks or reworded.
A name is chosen so that the rename of what the citation means fails it, and so is text the
cited code holds once: a word every such file holds, such as a YAML key like if: or a shell
case's command word, is no such name, and nor is text that another part of the same file holds
too, which the rename of the part the citation means leaves standing. The release command of
release-push.sh is cited by its case arm, `.github/scripts/release-push.sh#release)`: that file
holds the arm's text once, so the rename of the arm alone fails the citation, which a citation
of its usage line would not. A root of the list that skills-guard.test.ts sweeps is cited by
its entry, `packages/pi-legion/src/skills-guard.test.ts#path.join(REPO_ROOT, "AGENTS.md")`,
since another string of that file holds the file name alone.

Outside .github a //ci: line alone is read: a line whose text, after any whitespace, starts with
the five characters //ci:, the test that the parser of .github/scripts/ci_filter_sources.py puts
to each line of a Go file before it reads the rest as what a test reads
(`.github/scripts/ci_filter_sources.py#DIRECTIVE`). So every line that parser reads is read here,
and such a line in any other text file is read too, so that a guide's example is held as well.
Each such line is read alone, as that parser reads it: no rule reads across its end into the next
line, and a problem names the line it is on. Any other line outside .github, among them a //ci:
that does not start its line, as in a string or after code, is not read, held or refused. The
check runs in the lint job, which every pull request and merge group runs, since a rename can be
in any file of the repository, after the job's setup-go and its bun install, which its scanners
need, and its test, .github/scripts/ci-citations.test.sh, runs just before it. The TypeScript
scanner's bun run resolves typescript from this repository, where the scanner stands: in CI from
the node_modules that the lint job's bun install --frozen-lockfile fills from bun.lock. A local
run with no node_modules does not fail on that: Bun installs typescript itself into its own
cache, at the version the root package.json and bun.lock give.

## A lane label turns on one slow suite on a pull request

A pull request label `lane:<output>` turns on that one slow suite, as `full-ci` turns on all of
them. `<output>` is one of the fifteen outputs the `full-ci` term gates, and the label turns
that output on where its paths matched: `daemon_go_tests`, `dispatch_browser`,
`dispatch_memory`, `dispatch_multiprocess`, `dispatch_two_builds`, `envoy_go`,
`envoy_go_checks`, `envoy_go_claim_model`, `envoy_go_race`, `envoy_go_race_api`,
`envoy_go_race_docs`, `envoy_infra_go`, `envoy_smoke`, `pi_legion` and `test`. It is for a pull
request that needs one slow suite's result at its head - a test that only that suite runs, or a
`ci_check` naming that job - without running the others. Each term names its own label, so the
label that turns a job on is written beside it and nowhere else, and the paths term stays: a
lane label never runs a suite its filter did not select.

A label that names no such output - a misspelling, a job id such as `lane:envoy-go` where the
output is `lane:envoy_go`, or an output that is not held back - turns on nothing and fails
nothing. The label decides only what runs, never the verdict, and ci-ok's report names each job
a pull request held back with the label that would run it, so a misspelt label shows as its
job not run. Failing the run on such a label would fail every check of it for a typo and need a
step of its own that reads the labels. GitHub compares the label without regard to case.

A lane label is read as `full-ci` is, from the pull request the run's own event carries: it
takes effect at the next push, reopen or ready-for-review, not when it is added, since this
workflow takes no `labeled` trigger. On a head with no new commit to push, close the pull
request and reopen it. Take it off once the run it was added for passes, as `full-ci`.
.github/scripts/ci-router-outputs.test.sh evaluates ci.yaml's own outputs
(.github/scripts/ci_router_outputs.py) and holds them to this: each lane label turns on its
own output and leaves every other one as an unlabelled pull request has it, `full-ci` still
turns all fifteen on, the paths term still holds under both, and a label naming no such output
turns on nothing.

## Why envoy-go's build and checks come back on every push

envoy-go's build and checks (envoy-go-build: the format and generated-code checks, the vets,
the migration check and the build and pack of the envoy commands), split out so they come back
on every push: no `full-ci` term, only the every-job one,
`steps.scope.outputs.every_job && 'true'`.

## The two-build job builds the previous release and runs both halves against

The two-build job builds the previous release and runs both halves against it (the Go
suite and the cross-build browser spec), which is the longest run of them all, so it is
gated like the other slow suites.

## The two-build job's Go step alone, behind a gate of its own

The two-build job's Go step alone, behind a gate of its own (ci-job-dispatch-two-builds.yaml
says why). The dispatch_two_builds output decides whether the job runs at all, the `full-ci` term with
it, so this one carries only the every-job term.

## Why the tool fallback proof carries no full-ci term

The second source of every tool install, proved by driving it: no `full-ci` term, so the
proof comes back on every change that selects it. It selects the arm64 half of that
proof too, which drives the same action over the same manifest through the same helper
and so reads the same files.

## The paths filter's own verdict for every filter, before the `full-ci` term

The paths filter's own verdict for every filter, before the `full-ci` term (`(github.event_name != 'pull_request' || contains(github.event.pull_request.labels.*.name, 'full-ci'))`) rewrites
the outputs the slow suites run behind. On a pull request without that
label each of those is `false` whether its paths matched or not, so a reader of the
outputs alone cannot tell "the filter said no" from "the label is absent"; with this
beside them ci-ok's report tells the two apart and says which
(`.github/scripts/ci-ok.jq#selection`). It gates no job and needs no plumbing of its
own: `toJSON(needs.changes)` carries every output of this job, so the ci-ok job and
ci-ok-report.sh both already have it. It is the one filter step's outputs as one object.
On a run that selects every job (a dispatch of main, or one asking for every job) that step is
skipped and this is `{}`, which is right - the label holds nothing back there, and the changes
job's other outputs are all 'true', but for the list of matched filters, which is `all` there. On
a dispatch of a branch the step runs and this is its verdicts, which no label term rewrites.

A job a pull request held back did not run at this head, and ci-ok passes over its skip, so
ci-ok.sh names it once more after its verdict, pass or fail, on a line of its own:
`not run at this head: <job> (selected by paths, held back on a pull request without full-ci or lane:<output>; runs in the merge queue)`,
where `<output>` is the job's own `changes` output, so the line names the label that would run
it. The same lines go to the ci-ok job's summary, apart from the jobs that ran and passed, so a
green ci-ok on a pull request without the label is not read as those jobs passing. No job runs
in part, so a job that ran and passed ran all its steps. A run that held nothing back writes no
summary.

## What the filter step diffs on a dispatched run

On a dispatched run of a branch the step gets `base: main` and `ref: <the run's commit>`, and
paths-filter diffs `main...<commit>`: it reads main as refs/remotes/origin/main, which the merge
base step fetched with enough of both histories for git to find their merge base, so it fetches
nothing more (with no merge base in reach it fetches main and the commit 100 commits deep and
deepens both until git finds one), and lists what the commit changed since it
(dorny/paths-filter v4.0.3, src/git.ts getLocalRef and getChangesSinceMergeBase). The filter step
and the filter generation thus measure the branch from one commit
(.github/ci-notes/ci.md#what-a-dispatched-run-of-a-branch-selects-and-when-to-ask-for-every-job).
The commit, not the branch name, so a push to the branch after the dispatch does not change what
this run selects. On a pull request and a merge group both inputs are empty, which paths-filter
reads as unset: a pull
request's files come from the API and a merge group's from its own base_sha..head_sha, and a
non-empty `base` would override the merge group's. On a run that selects every job the step is
skipped, since the outputs take 'true' without it, and a run of main has no merge base to diff
with but its own tip.

## Why the lint job holds each of its scopes

The ci-ok-report step is the whole of this job's GitHub writing: it posts a failing ci-ok
with `checks: write` and names the run's jobs in it with `actions: read`. `contents: read`
is the checkout's. The daemon API contract check reads no pull request: it fetches its base
from origin with git, with no token. The pull request's text is the `pr-title` job's, which
declares the scopes its own routes read.

## Why every Biome step prints every warning and error

Every step that runs Biome over a tree links here, and each but lint's whole-tree check carries
`--diagnostic-level=warn --max-diagnostics=none`. Biome prints 20 diagnostics by default, and
infos count among the 20, so the error that fails a step can be one it does not print. #5195's
lint run
(https://github.com/cybermonklabs/legion/actions/runs/45179104499/job/137708378563) printed 20
infos, then `Diagnostics not shown: 17.` and `Found 1 error.`: the error, a formatting error in
packages/envoy/internal/dispatch/api/testdata/visual-shot-reads.json, was among the 17, and the
log did not name it.

`--max-diagnostics=none` removes the limit. `--diagnostic-level=warn` leaves out the infos, and
an info fails no step: of Biome's flags, only `--error-on-warnings` makes a diagnostic below an
error fail, and it makes warnings fail, not infos. The level must not go above `warn`. Biome
counts only the diagnostics the level keeps, so with `--diagnostic-level=error
--error-on-warnings` a tree whose one fault is a warning exits 0, where the step without the
level exits 1.

Lint's whole-tree check runs .github/scripts/check-biome-tree.sh, which passes
`--max-diagnostics=none` and keeps the infos. It must: Biome does not read a file of its
`files.maxSize` or more and says so only in an info, and the script fails naming each such file
from that info. `--files-max-size` would not stand in for the info: it does not change the limit
of a nested biome.json that sets none (packages/claude-envoy's), which stays at Biome's default of
1 MiB.

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
the host the first source is on, installs all nine tools through the source
.github/ci-tools.yaml names for each, and then serves eighteen corrupted payloads and reads
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
Dockerfile, the manifest and the helper beside it, and the nine actions, so
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
are in .github/workflows/ci-job-tool-fallback.yaml, and on one of the twenty-six installs
there whose control passes a step whose `outcome` is `failure`, a bound is silent: a hang
would satisfy that control and leave the job green, where the job's own ceiling would have
cancelled it and failed ci-ok. That is why each of those installs is timed as well as read -
the eight with nowhere to go by the control step "Check all eight installs with nowhere to go failed, and failed in their own time", the eighteen corrupted cases by
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

lint, typecheck, test, tooling-checks, daemon-go, plugin-pack, private-names-port,
sessions-script and text-checks are jobs ci-ok condenses; main's ruleset requires none of their
names. Fail-open: when `changes` did not succeed, all of them but lint, which runs on every
change anyway, run rather than skip. ci-ok fails then anyway (ci-ok.sh), so this buys signal:
their results while `changes` is broken. The set is main's fail-open typecheck and test, and
every job that took a suite from that test job: its plugin build, changelog check, lints and
typechecks are tooling-checks', its daemon Go tests are daemon-go's, the tests of
ci_job_reads.py's PLUGIN_PACK_TESTS are plugin-pack's, the private-name port harness is
private-names-port's, the daemon's text checks are text-checks' and `bun test scripts/sessions`
is sessions-script's, so each of those carries the term too and a suite that moves out of a
fail-open job takes it with it. A re-run of only the failed jobs can leave the jobs `changes`
then selects skipped, and ci-ok fails a selected job that skipped rather than passing with
nothing tested: re-run every job instead.

## What the test job runs, and how each harness step is gated

The script harnesses and the Bun suites of scripts/, a slow suite the merge queue runs
(.github/ci-notes/ci.md#which-lane-a-job-runs-in-the-pull-request-or-the-merge-queue). Each
`*.test.sh` step runs behind a filter of its own naming that harness and the script it tests,
which the step reads out of the changes job's one list of the filters that matched
(#one-list-for-the-test-jobs-step-gates), so a change to one script runs one harness. The job's
own filter names every one of those lists, since a job's `if:` tests one output and a step
gate decides nothing in a job that was not selected. When `changes` did not succeed every step
runs. The plugin build, the changelog check, the guides' layout step, the lints and the
typechecks that ran beside these tests are the tooling-checks job's, in the same file. All but
three of the test job's harness steps queue their harness through the harness pool
(#the-harness-pool) and the job's verdict on them is its step "Wait for the script harnesses and
judge them": gh-read.test.sh, whose last case reads the job's Runner.Worker among its parents,
dispatch-browser-run-reads.test.sh, whose waits of a few seconds are judged against the real
clock, and harness-pool.test.sh, which tests the pool itself, stay steps of their own, run
before the pool queues any. Every harness step has its own gate, so a change can reach none of
them; the wait passes `--allow-none` for that run.

## Two jobs that call one file

ci.yaml calls 45 unique reusable workflows, the most .github/scripts/check-workflow-files.sh
allows it (its rule 5 keeps five below the 50 GitHub counts over every tree of nested calls),
so a job split in two takes no file of its own for its new half, and a new job takes none
either. test and tooling-checks call ci-job-test.yaml, pi-legion and pi-legion-build call
ci-job-pi-legion.yaml, envoy-infra-go and envoy-infra-build call ci-job-envoy-infra-go.yaml,
and envoy-e2e-local and envoy-refill, which build the same listener and natsd, call
ci-job-envoy-e2e-local.yaml. Such a file holds one job per caller and
takes a `job` input: each of its jobs carries exactly `if: inputs.job == '<its id>'`, each
caller passes `job: <its own id>`, and each job ends with the ci-ok-report step under its own
id. A call runs the one job named for its caller, and the other reads `skipped` under it.

.github/scripts/ci_filter_coverage.py holds that shape (check_called_jobs): once a file two
ci.yaml jobs call holds a job named for any of its callers, each caller must have a job of it
named for itself, which must carry that `if:`, and must pass its own id, since a job that ran
under a second caller would read files that caller's filter is never held to, and a caller with
no job of its own there runs nothing its filter is held to.
`.github/scripts/ci_job_reads.py#SHARED_FILE` maps each new half, and envoy-refill, to the file
it shares, so its gates find their steps there. Each job keeps a filter of its own, and both name
the shared file, so a change to it selects both. dispatch-browser.yaml has three callers,
dispatch-browser, dispatch-browser-pr and dispatch-references, but every job of it runs under
any of them: it takes the caller's id as `job` for its artifact names, its read of its own jobs
and its report, never to pick a job
(.github/ci-notes/ci.md#the-browser-suite-jobs-their-lanes-their-inputs-and-why-the-matrix-is-in-the-called-file).

## What the tooling-checks job runs

The steps of the old test job that a pull request runs: the Claude Code plugin's build, check
and pack as the release runs them, the check that the plugin's build-input list covers what the
build reads, the render check of every changelog fragment with the changie it installs, the
guides' layout step behind its own guide_layout filter, read out of the same list of matched
filters (#one-list-for-the-test-jobs-step-gates), and the lints and typechecks of
scripts/dispatch, scripts/github-apps, scripts/sessions and scripts/approval-eval, with the
typecheck of scripts/e2e/lib's plan ratings read test. They are builds, lints and typechecks,
which a pull request runs whatever their time, and they come to 1.0 minutes at the 90th
percentile (.github/ci-notes/ci.md#which-lane-a-job-runs-in-the-pull-request-or-the-merge-queue).
The job shares ci-job-test.yaml with the test job
(.github/ci-notes/ci.md#two-jobs-that-call-one-file) and fails open as
that job does (.github/ci-notes/ci.md#the-fail-open-jobs-and-what-they-buy-while-changes-is-broken).

## The harness pool

.github/scripts/harness-pool.sh runs a job's script harnesses beside each other. A job's steps
run one after another, and a shell harness of fakes mostly waits - on a fake's answer, a poll, a
deadline - so as steps they kept the runner's other cores idle: on green pull-request jobs of
2026-11-22, before the test job and envoy-go-checks lost their builds, lints and typechecks to
tooling-checks and envoy-go-build, envoy-go-checks' harness steps were 944 s of its 1,292 s
median (n=724) and the test job's 366 s of its 638 s (n=416). Each harness makes its own
temporary directory (mktemp); the pool gives it none. Before the pool was written,
envoy-go-checks' 19 harnesses of the time were run at once on a dev box, each with its own
TMPDIR, and all passed but dev-stack.test.sh, which fails alone there too (its fakes use `$$` as
a port, and that box's process ids pass 65535).

Each harness step keeps its name, its `if:` and its working directory, and runs
`harness-pool.sh start <name> <command>`, which queues the command and returns: the step's
result says only that its harness was queued. The harnesses start in the order queued, no more
at once than the runner has processors (nproc), each in a session of its own with its standard
input from /dev/null, its output in a log of its own, SIGINT and SIGQUIT at their defaults (a
command started with `&` by a shell without job control starts with both ignored, which would
make a harness's own INT traps do nothing), and the runner's per-step files (GITHUB_STEP_SUMMARY,
GITHUB_OUTPUT, GITHUB_ENV, GITHUB_PATH, GITHUB_STATE) pointed at files of its own entry, since
the step that queued it has ended. The job's step "Wait for the script harnesses and judge
them" waits for the last of them and is their verdict: it prints each one's log in a group of
its own headed by its exit status, wall seconds, CPU seconds and seconds waiting for a slot,
fails with an `::error` naming each that failed, and writes the same table to the job summary.
It runs after a failed step too (`!cancelled()`), so a harness queued before the failure is
still judged. A cancel or the job's timeout during the wait signals it: the step `exec`s it, and
the runner signals a running step only when its condition no longer holds, which with
`always()` it would always hold. The wait then names each harness still running with its log so
far; a job cancelled before the wait began runs "Name the script harnesses a cancelled job left"
(`wait --now`) instead. A recorder killed before its harness ended is a failure, and the pool
ends that harness's session before its slot goes to another. `start
--minutes <n>` bounds one harness as a step's timeout-minutes did and hands it
HARNESS_POOL_DEADLINE_MS. The runner ends a step's leftover processes only at the job's end, so
a harness outlives the step that queued it and the job's timeout-minutes still bounds it.

A harness belongs in the pool when nothing else in the job writes what it reads and it reads
nothing of its parents. One that needs an earlier step is queued after it
(release-push.test.sh after install-changie, disk_watch.test.sh after the user-namespace
sysctl, check-ci-filters.test.sh and go-cache-working-set.test.sh after setup-go, and the first
of them and ci-router-outputs.test.sh after the
step that fetches the filters the run's changes job computed, whose path each takes in its step's
env). In envoy-go-checks they are queued right after the Go cache restore and the helm install
(the cluster test's nats.yaml render runs that helm, and a pooled harness takes the PATH of the
step that queued it), longest first.

## Why the daemon's Go suite is a job of its own

The Go daemon's own suite: the plain step, the race step and the Stage 1 proof, with the
Postgres, the tools and the cache they need. It left the test job in dispatch://INFRA-1413,
where one filter had to cover both: a change to a shell script, to packages/claude-envoy or
to bun.lock ran 1,083 s (median) of Go tests that could not see it. Fail-open with typecheck
and test, for the reason #the-fail-open-jobs-and-what-they-buy-while-changes-is-broken gives.

It runs in legs of .github/workflows/ci-job-daemon-go.yaml, all at the same time: the race
step's legs and the plain step's, each over the packages .github/go-legs.yaml lists for it, and
two jobs, daemon-go and daemon-go-race, that pass only when their kind's legs passed and keep the
check-run names the two steps' jobs had (ci-job-daemon-go.md#why-the-daemons-tests-run-in-legs).

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
service on a job whose other steps would start it for nothing.

## The pi-legion plugin's suite and its build

Two jobs over packages/pi-legion, both calling ci-job-pi-legion.yaml
(.github/ci-notes/ci.md#two-jobs-that-call-one-file). pi-legion runs
the plugin's test suite with what only it needs - jj, the pinned Oh My Pi, Go and the go-daemon
restore for daemon-pane's `go run`, and the NATS service controller-start-omp.test.ts's role
claim needs - on ubuntu-24.04, since the Oh My Pi release ships linux-x64 alone. It is a slow
suite (14.8 minutes at the 90th percentile as the one job it was), held back behind its
`pi_legion` output (.github/ci-notes/ci.md#how-a-slow-suite-is-held-back-on-a-pull-request).
pi-legion-build builds contracts, typechecks the plugin, runs the skills' guard
(src/skills-guard.test.ts: every skill within its budget, every link resolving, every pinned
sentence present) and packs the plugin as the release does, uploading the tarball as
`pi-legion-pr`: a build, a typecheck and a text check, which a pull request runs (0.8 minutes),
on ubuntu-24.04-arm, since it installs no Oh My Pi. The guard also runs in pi-legion's suite in
the merge queue, where it costs under a second.

## The dashboard's and the proof editor's lint, typecheck, unit tests, build and

The dashboard's and the proof editor's lint, typecheck, unit tests, build and asset checks.
They were split out of the browser suite (the dispatch-browser job), whose full lane - the one
the merge queue, the nightly and a pull request labelled `full-ci` or `lane:dispatch_browser`
run - is a slow suite, while these checks must come back on every push.
No Postgres service and no Go, but contracts is built
before the suites: one case of the dashboard's test run spawns Playwright, whose own
resolution reads contracts' built dist/ (the comment on the step "Build contracts for the
suites that spawn Playwright" of .github/workflows/ci-job-dispatch-ui.yaml says what failed
without it). tsc and
bun read @legion/contracts through the workspace's "bun" export condition and the dashboard's
Vite build aliases its sources, so that spawn is the whole of what the build is for.

## The browser suite jobs: their lanes, their inputs, and why the matrix is in the called file

Every browser row of the Dispatch suite, which .github/workflows/dispatch-browser.yaml
holds: the binaries job that builds the Go commands the lanes run, the shard legs, the three
`-alone` projects a machine each, the two-process job, the fake-embedder job that runs on the
pull-request and full lanes, the containerised visual review, and the merge job that joins the
shard and alone reports into one and posts its caller's ci-ok report. Three jobs of this file
call it.

Two lanes, and which run pays for which. dispatch-browser-pr runs `pull-request` on a pull
request: the two projects a change is held to (chromium and iphone) over the `@pr` rows, so such
a run proves those rows and no more - it is not the run that proves the suite before main.
dispatch-browser runs `full` - every project, the three `-alone` ones, the two-process job and
the visual review - in the merge group, on a workflow_dispatch and on a pull request carrying
`full-ci` or `lane:dispatch_browser`, where both jobs run. So the merge group is where a change
meets the whole suite before it lands, and the nightly
(.github/workflows/dispatch-browser-nightly.yaml, which calls the same file with the full
lane, from main, under the job id dispatch-browser) runs it again on main's tip. A third lane,
`references`, is dispatch-references', a preview of the reference rows on a pull request that
gates nothing
(.github/ci-notes/ci.md#the-reference-rows-preview-job).

Two inputs, each a literal of its caller's. `lane` decides the projects each job of the called
workflow runs, so nothing here chooses them a second time; it picks no browser to install any
more: the shard and alone jobs run in the image this repository builds, which carries all three
engines (dispatch://INFRA-1443), and the two-process job installs Chromium whatever the lane.
`job` is the caller's own id. GitHub names each job of a called workflow `<caller> / <job>`, so a
pull request's browser checks are `dispatch-browser-pr / shard (1)` and the like, and a
`ci_check` naming `dispatch-browser / merge` finds no run of that name on a pull request
without the label: the dispatch-browser job's own run concludes `skipped`, and no job of
dispatch-browser.yaml starts under it. The merge job reads only the jobs named under its own
caller, every artifact name starts with the id so callers that share a run share none, and
the merge job's ci-ok report names it.

The matrix is in the called workflow and cannot be here. Every leg of a matrix shares one
job id, and .github/actions/ci-ok-report posts the failing ci-ok of a merge-group run from
the job whose own result decides that run, as that job's last step: legs written here would
each be `dispatch-browser`, so a decided group would take one such post per leg that failed,
each posting under a job id several legs answer to. A called workflow's legs are jobs of
that workflow, and one job of it - the merge job, which needs every other - posts the single
report its caller's id gets.

No timeout-minutes: GitHub's keywords for a job that calls a reusable workflow leave it out.
Each job of dispatch-browser.yaml carries its own limit.

## Why the dispatch-browser job declares the scopes the called workflow needs

The same rule as the docs job's: a caller that declares no `permissions:` hands the
called workflow the default GITHUB_TOKEN permissions rather than this file's block, so
dispatch-browser and dispatch-browser-pr each declare the same three, and dispatch-references
two of them
(.github/ci-notes/ci.md#the-reference-rows-preview-job).
`contents: read` is every job of dispatch-browser.yaml's checkout; `actions: read` is the
merge job's `gh api .../actions/runs/<id>/jobs` read of this run's jobs, and `checks: write`
that job's last step, the ci-ok report.

## What the lane input reads

`.github/workflows/dispatch-browser.yaml#lane`, which each of its three callers here passes as a literal:
dispatch-browser-pr `pull-request`, which runs the two projects a pull request is held to over
the `@pr` rows, dispatch-browser `full`, which runs every project, the three `-alone` ones,
the two-process job and the visual review over every row, and dispatch-references
`references`, which runs Chromium over the reference rows' spec files
(.github/ci-notes/ci.md#the-reference-rows-preview-job).
Which lane a run gets is which caller runs: dispatch-browser-pr and dispatch-references on a
pull request alone, and dispatch-browser wherever its `dispatch_browser` output is true, which
a pull request gets only under `full-ci` or `lane:dispatch_browser`
(.github/ci-notes/ci.md#the-browser-suites-two-outputs-one-per-lane). On the pull-request lane
the alone, multiprocess and visual jobs of the called file are skipped by their own `if:`, as
jobs of a lane it does not run, and on the references lane those three and the embedder job.
No other job of this file passes a lane: a job runs all its steps wherever it runs, and a job
that held a slow suite beside steps a pull request runs is two jobs now
(.github/ci-notes/ci.md#the-fast-half-of-a-split-job-comes-back-on-every-push).

## The reference rows' preview job

What it runs, and why it gates nothing.

The reference rows' preview: every Chromium row of the eleven spec files
the reference framework's Verification document names and the nine of the reading list's
walk, through the `references` lane of dispatch-browser.yaml - its binaries job, its four shard
legs and its merge job, and none of its
alone, two-process, embedder or visual jobs. The pull-request lane, the dispatch-browser-pr job,
runs only the few of those rows that carry `@pr`, so without this job a change to the
reference framework got the rest from a run by hand.

Outside ci-ok on purpose: CI_OK_EXEMPT names it, ci-ok does not need it, and it passes no
`ci-ok-changes`, so its merge job posts no ci-ok report. What a required check runs is the
repository owner's decision, and the `@pr` budget the pull-request lane is held to
(packages/dispatch/e2e/__tests__/pull-request-lane.test.ts) stays as it is, so this job adds
rows to no check that gates. A red here does not block a merge: it is a reading for the
author and the reviewer of the pull request. What gates these rows is the merge queue: every
merge group runs the dispatch-browser job on the full lane, which holds every one of
them, and ci-ok needs that job.

So it joins the router in fewer places than the eight
.github/ci-notes/ci.md#the-router-every-check-a-pull-request-or-the-merge-queue-runs names for a
new job: an EXCLUDED file in .github/ci-gates, dispatch-references.py, where a gate would be,
which holds its filter's patterns by hand, an output in `changes` for that filter, its `needs:
changes` and an `if:` on that output with the re-run term, and its name in CI_OK_EXEMPT. It has
no CI_OK_SELECTED_BY or
CI_OK_PULL_REQUEST_ONLY entry, no report step and no place in the ci-ok job's `needs:`,
each of which check-ci-filters.sh refuses for a job CI_OK_EXEMPT names. Its steps are
dispatch-browser.yaml's, as the dispatch-browser and dispatch-browser-pr jobs' are, rather than
a ci-job-dispatch-references.yaml of its own: they are the jobs of the one suite under another
lane, and a second copy of them would drift from the first. It passes its own id as that file's
`job`, as they do, so its checks read `dispatch-references / shard (1)` and the like, every
artifact it uploads starts with `dispatch-references-`, and its merge job reads only its own
legs when dispatch-browser-pr shares its run
(.github/ci-notes/ci.md#the-browser-suite-jobs-their-lanes-their-inputs-and-why-the-matrix-is-in-the-called-file).

Pull requests alone: the merge queue's full lane already runs these rows, so a merge group
would pay for them twice. The merge-group attempt term is the one every job of ci.yaml but
ci-ok and pr-title carries; with the pull_request term it holds nothing back, and it is there
because check-ci-filters.sh's rule 12 holds every job to it. Its `changes` output,
dispatch_references, carries no `full-ci` or `lane:` term either: the job is no slow suite held
back on a pull request, and the merge queue's full lane runs the same rows whatever the labels.
The `dispatch_references` filter selects it;
.github/ci-notes/ci.md#dispatch_references-the-reference-rows-preview-dispatch-references-the-eleven-spec-files
says why that filter is narrower than what the job reads.

No timeout-minutes: GitHub's keywords for a job that calls a reusable workflow leave it out.
Each job of dispatch-browser.yaml carries its own limit.

Its `permissions:` block is its own, for the reason the dispatch-browser job's
(.github/ci-notes/ci.md#why-the-dispatch-browser-job-declares-the-scopes-the-called-workflow-needs)
gives: a caller that declares no `permissions:` hands the called workflow the default
GITHUB_TOKEN permissions rather than ci.yaml's block. `contents: read` is every job's checkout
and `actions: read` the merge job's read of this run's jobs. No `checks: write`, which only that
job's ci-ok report needs: this call passes no `ci-ok-changes`, so the report step skips, as it
does in dispatch-browser-nightly.yaml's call, which declares these same two.

## envoy-go-build: envoy's Go build, vet, format, generated-code and migration checks

envoy's Go build, vet, format, generated-code and migration checks: the Go format check, the
two generated-code checks, the vet of the module as darwin compiles it, the check that no
migration a database may have run is edited, and the build and pack of the envoy commands. It
also runs the tests of internal/dispatch/architecture/notes, internal/runnerlog,
internal/dispatch/specgraph, cmd/architecture-check and cmd/spec-check, whose one run this is
(envoy-go's unit step leaves them out), the architecture-note check over this repository's own
.dispatch/architecture through the composite action .github/actions/architecture-check, and the
spec-file check over its .dispatch/spec through the composite action .github/actions/spec-check,
both of which other repositories can call. These come back on every push; the slow half, the
envoy-go job (the module's unit tests, the container-backed NATS matrix and the integration
suite), and the script tests, envoy-go-checks, are slow suites
(.github/ci-notes/ci.md#which-lane-a-job-runs-in-the-pull-request-or-the-merge-queue). No
Postgres service: nothing here opens a database. It restores go-envoy rather than
go-envoy-test, the family of the slow half (the test binary of every package of the envoy
module) and of dispatch-browser.yaml's Go-building jobs. It also runs the guard that no CI file
of this repository compares the spec files with the approved spec.

## envoy-go-checks: envoy's script tests, in the merge queue only

envoy's script tests: each step runs one bash harness of packages/envoy (scripts/, loadtest/
and deploy/scripts/) over fakes. They shared a job with envoy-go-build's steps, and took 15.8
minutes of its 21.5 at the median over the 695 pull-request runs of 2026-11-22; split out, they
are a slow suite the merge queue runs, and the build and checks beside them stay on the pull request.
The job keeps the setup they ran under there (Bun, the workspace install, Go and the go-envoy
restore), so no harness loses a tool it reads. Each step queues its harness through the harness
pool (#the-harness-pool), so they run beside each other, and the job's last step but the ci-ok
report is their verdict.

## What the envoy-go job runs

The slow half: the unit tests of every package of the envoy module, the container-backed NATS
matrix and the integration suite, all against this job's Postgres. Its format, generated-code,
build and pack steps are envoy-go-build's, and its shell-script tests envoy-go-checks'.

## The envoy-infra module, its own workspace and its live tests

The envoy-infra Pulumi plugin's module (packages/envoy/infra). It is not in the root go.work:
its own go.work binds it and packages/envoy, so Pulumi's requirements never move the versions
the Envoy binaries and the daemon build with, and every go command of ci-job-envoy-infra-go.yaml runs in that
workspace. Two jobs call that file
(.github/ci-notes/ci.md#two-jobs-that-call-one-file).
envoy-infra-build checks the module's format, vets it, the files behind the live-test tags
(pulumilive, natslive) included, as linux and as darwin compile them, and runs its unit tests
under the race detector: builds, vets and fast tests, which a pull request runs (3.0 minutes).
The unit tests start no container. envoy-infra-go runs the natslive files against real
nats-server and Postgres, on testnats.Image, testnats.HubImage and testpg.Image, pulled first
for the reason .github/actions/pull-test-images gives: a slow suite (14.0 minutes at the 90th
percentile as the one job it was), held back behind its `envoy_infra_go` output
(.github/ci-notes/ci.md#how-a-slow-suite-is-held-back-on-a-pull-request). That step runs only
the tests the tag adds, one call per package: envoy-infra-build's "Unit test envoy-infra" step
runs every untagged test of core, seed and gcp, so each call takes its -run from
.github/scripts/go-tagged-tests.sh, which reads the names off the source at run time, and
ci_job_reads.py's TAG_RUNS holds envoy-infra-build's filter to every file the natslive step
reads, so a change that starts the tagged run starts the untagged one with it. The gcp
natslive tests pull no image: they install helm, nats-server, the config reloader and the
exporter with go install, at the versions the clustered hub runs, and render the vendored chart
in gcp/testdata. The per-target compile runs in the lint job's notices step.

## The checks whose subject is text: each module's internal/textcheck package, the Dispatch

The checks whose subject is text: each module's internal/textcheck package, the Dispatch API's
ten static source checks, which read the module's own source, and the two pure pins of
packages/daemon's supervise table. None of them opens a database or starts a container, so the
job carries no service and no `bun install`, and the three Go lanes (daemon-go, envoy-go and envoy-infra-build) leave those
packages, those twenty-two tests and those two pins out of their own runs
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
(internal/cipher/natscallout), which starts a goroutine per request from nats.go's delivery
callback, counts those waiting for a decision slot and waits them out at Close, the sign-in
code flow (internal/oidc), whose sign-ins and refreshes use one shared x/oauth2 config at the
same time (TestCodeFlowGrantsUseTheDiscoveredConfigWithoutARace checks that use for a data
race), the Dispatch API and the document service - all under the race detector. A read
that walks a live document without its lock races the room's peers, which no ordinary run
shows: envoy-go's unit-test step runs without -race (LEGION-499). The lane sits beside
envoy-go because -race runs these packages several times slower, and each job carries a
package deadline of its own, since go test's ten-minute default is a whole package's.
envoy-go runs none of these packages plain any more (INFRA-1408): its unit step leaves out
exactly what the lane runs, and the one plain run it keeps of them is the step named "Test
what the race build cannot run at all or at full size", which runs the handful of
assertions the detector makes impossible or shrinks and nothing else.

The lane is three jobs: envoy-go-race (delivery, the Dispatch store, the bus, the listener,
the registry store, the session cache, kvwatch, natscred, the NATS auth service and the sign-in
code flow), envoy-go-race-api and envoy-go-race-docs, whose notes,
.github/ci-notes/ci.md#why-the-document-service-runs-alone-under-the-race-detector,
say why the document service runs alone.

api sits apart, on a runner of its own, because isolation is what made it fast: sharing a
runner, the race binaries contended for the four vCPUs, and alone api fell from 1099.487 s to
746.718 s. The packages
left in envoy-go-race are cheap enough to share one: the two heaviest of those measured
there, the bus at 286.931 s and the listener at 365.787 s (run 40926978371), are a quarter
and a third of the lane's bar of 1080 s, which is 60 % of a 30-minute step's own timeout, so
a runner each would buy nothing and cost about 8,000 replayed runner minutes. Running a heavy
package apart also reads its seconds as its own job's, and leaves the others' numbers
standing when one is red.

envoy-go-race-api's tests drive the Dispatch store, so its job carries the lane's Postgres
service.

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
current is envoy-go-build's. delivery's, the bus's, natscred's and the auth service's NATS
runs in the test process (internal/testnats), so no job here needs a container runtime of its
own.

Delivery, the Dispatch store, the bus, the listener, the registry store, the session cache,
kvwatch, natscred, the NATS auth service and the sign-in code flow share this runner. The
Dispatch store's tests take their databases from
`packages/envoy/internal/dispatch/store/store_test.go#testDatabaseURL` and delivery's from storetest.Open
(`packages/envoy/internal/dispatch/store/storetest/storetest.go#Open`), each of which reads
DISPATCH_TEST_DATABASE_URL and skips without it, and
`packages/envoy/cmd/listener/dispatch_resolution_test.go#migratedDispatchDatabase` gets its database through internal/testpg, so the
job carries the lane's Postgres service; the registry store itself is the NATS KV view, and
natscred's, the auth service's and the code flow's tests open no store, so they need none.

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
(`packages/envoy/internal/dispatch/docs/live_read_race_test.go#TestReadsOfALiveDocumentRunBesideItsPeers`,
LEGION-499).

The same run is the package's run under Go's cleanup detector: GODEBUG=checkfinalizers=1 makes
the runtime report a cleanup or a finalizer whose closure or argument reaches the object it is
attached to, a cycle that keeps that object alive for the life of the process, and that is the
defect that held this package's memory (dispatch://INFRA-1085). This is the only package of the
repository that registers a cleanup, so no other job needs it. The detector is the runtime's own
bookkeeping and runs beside the race detector in one process, so no job runs the package again
for it: the job envoy-go-docs-finalizers that ran all of its tests a second time, plain, for this
detector alone (994.286 s, job 137289139624, against 1770.949 s for this run, job 137289139255)
is gone.
The package's TestMain count of the services its tests left alive, which that job ran with -v
to print, prints here too, since -json streams the binary's own output (`docs: 1286 services
built, 0 still alive` in job 137289139255), and fails the run as it did there. What the race
build cannot run or runs shrunk - the room-memory bounds, the full-size render-reuse storms and
the xid window tests, whose gap bound only a plain build judges - runs plain, under the cleanup
detector too, in envoy-go's step "Test what the race build cannot run at all or at full size".

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
which needs main's history and the release tags, and builds it from `git archive`. The job checks
out its own commit alone and then fetches main and the release tags with their history but none
of their files (`--filter=blob:none`), which `git archive` fetches for a commit when it builds it:
the whole repository, 3,242 branches and 3,710 tags on 2026-11-23, took the checkout to its
2-minute budget in three merge groups that day (the workflow's comment has the numbers).
The first-rollout row runs the first-rollout build instead, the newest release before the first
that registers its schema numbers: the job pins it by its commit (FIRST_ROLLOUT_COMMIT), the
resolve step fails when its tag, legion-envoy-v30.20.2, names another commit, and the job builds
it the same way.
Each build is cached by its commit and the hash of the script that built it. The script removes
the archive it built in (src/) once it has copied the parts out, since the test reads only those
parts, so the entry holds the build alone. The `!<dir>/src` line the cache steps' path lists
carried before left nothing out: actions/cache resolves its list with @actions/glob and
`implicitDescendants: false`, so `<dir>` yields that one directory and tar archives it whole (a
round trip of a `!` line under a listed directory kept every file it named: run 45722872215, job
139585272669). The job restores each and
saves neither: .github/workflows/ci-caches.yaml's dispatch-builds job builds both on a push to
main, and again when release-envoy-listener.yaml has tagged a release (it dispatches
ci-caches.yaml with `only: dispatch-builds`), and saves them in main's scope, which every pull
request and merge group reads; its dispatch-builds-superseded job then deletes main's older
entries of each build and every entry of it on any other ref. The job saved them itself before,
in the scope of its run's own ref, where a merge group's entry is read by no later run and a pull
request's by that pull request alone: on 2026-11-23 25 such entries held 12.2 GB of the
repository's 25 GB cache and main held none, and on 2026-11-22 308 of 351 successful jobs built
the previous release again (107 s at the median, against 11 s to restore it). A run that resolves
a release main holds no entry of (one tagged minutes ago, or an older one a pull request's merge
base still names) builds it. Each row's NATS runs in its own test process (internal/testnats), so the job
needs no container runtime of its own. Then the browser half
(packages/dispatch/e2e/cross-build.multiprocess.e2e.ts), which runs twice: the browser suite's
two-process mode with Dispatch B run from the previous build's envoy-dispatch and web dist, then
from the first-rollout build's, and each direction's page served by one build against the other
build's server. Playwright installs as it does for dispatch-browser-multiprocess, and the rows
get a database of their own, since they truncate it. In a merge-group run the called file's
dispatch-two-builds-same job also runs the Go suite with this tree's own build as the previous
build, beside this job and not after it (that job's comment says why).

## Why the two-builds jobs restore go-envoy-test

Both jobs of ci-job-dispatch-two-builds.yaml restore go-envoy-test and save nothing, as
envoy-go-claim-model and dispatch-browser.yaml's binaries job do. setup-go's own cache is off,
so without a restore their Go steps start on an empty module cache and an empty build cache.
When the two earlier builds come out of their own cache entries, the sizing step's
`go run ./internal/dispatch/store/cmd/connections` and the twobuild test step are the first
go commands of the job, and they download and compile the set from nothing. In the 10 green
merge groups after 2026-11-24T08:40Z, the sizing step took 27 s, against 2 s before, when
the previous build's compile had filled the caches first. In the dispatch-two-builds job of run
45360231949, the two steps printed 16 and 67 `go: downloading` lines. Each push to main fills
go-envoy-test with `go test -count=1 -run '^$' ./...` over the envoy module, which builds the
test binary of every package and runs no test, so it holds what both steps build. Measured with
`go list -deps -test -tags twobuild ./cmd/dispatch ./internal/dispatch/store/cmd/connections`
at main 1b636f3193e, the steps need 626 packages of 80 modules, and `go list -deps -test ./...`
holds every one. go-envoy's build of the listener and natsd lacks 485 of the packages and 53
of the modules, so a restore of go-envoy would still download and compile most of the set. The
restore is a read of an entry that already exists: it adds no bytes to the repository's cache.
dispatch-two-builds-same's "Build this tree as the previous build" reads the same caches: the
modules and the compiled dependencies match, while the tree's own packages, built from the
script's `git archive` directory, compile again, since without -trimpath Go keys a package
outside the module cache on its directory.

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

The two-process tests of the secrets broker (packages/envoy/cmd/cipherd/multiprocess_test.go,
INFRA-236): each row starts real cmd/cipherd binaries against one Postgres - which is what the
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
`-tags multiprocess ./cmd/cipherd` would run the package's untagged tests a second time,
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

## What the refill live proof shows

packages/envoy/scripts/e2e-refill.sh, the refill live proof (INFRA-261): on a real listener
and a NATS server of its own, the listener keeps its copy of a bucket while it reads the
bucket again, and refuses its two list routes, with no active-sessions sample, while it cannot
stand behind that copy. The job runs every scenario of the script: reread, interrupt (stop
and restart), recreate-live (both shapes) and refuse.

## What the ci-filters job holds every filter and every workflow file to

Holds the table every filter is computed from to ci.yaml - every gate a filter the changes job
hands on to a real job, every go command a job runs claimed, every pin and citation still found -
and computes every filter the way the changes job does, recomputing each Go job's closure with go
list (.github/scripts/check-ci-filters.sh). When the changes job's computation fell open for a
cause of the engine's own (a go command, a gate file, a //ci: line), every filter selects this job
and it fails for the same cause; a fall-open from the Bun install or the TypeScript listing, which
this job does not run, leaves it green, and the lint job's readers are what turn the run red. go
list runs with GOPROXY=off over an empty module cache, so the job needs Go but downloads no module
and restores no cache. The tag sets of the darwin vet steps come from
.github/scripts/go_build_tags.py, whose go list runs with GOPROXY=off too, over the job's own
module cache. A run fails naming each file of the tree it changes, a checksum a go command writes
into a committed go.work.sum among them, but those its mode writes: each workflow
ci_job_reads.py's PUSH_WORKFLOWS names for `write`, and the filters and the .inputs file beside
them for `generate`. It also holds every workflow file to 64,000 bytes and to what GitHub's own
parser reads (.github/scripts/check-workflow-files.sh), which .github/workflows/workflow-files.yaml
runs a second time, in a file small enough to start when this one is too large to.

## What the proof editor's offline proof shows

The proof that no install of this repository fetches the proof editor's upstream sources over
the network: a frozen install runs behind a proxy that answers HTTP 504 for the hosts of the
old `github:sjawhar/proof-sdk` route, pinned to a commit, and the job fails when the proxy's log holds a
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
(`.github/workflows/browser-image.yaml#attest` job says why).

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
neither a pull request nor a group and has no text to check, so there the checkout and the check
are skipped and the job does nothing: it runs on a dispatch only so that GitHub evaluates its
name, `pr-title (dispatch)`, which no ruleset reads. Its `if:` is the draft term alone, since
this workflow runs on no event but those three. A job GitHub skips still posts
a check run, and a skipped one is a pass; GitHub does not evaluate the name of a job it skips,
so on a draft's run, which skips this one, the check run carries its `name:` expression's own
text and not `pr-title`
(.github/ci-notes/ci.md#why-a-dispatched-run-off-main-reports-under-names-of-its-own).

## What the pr-title job's scopes are for

What the two scripts this job runs, find-pr-payload.sh and check-pr-text.sh, read through gh, and nothing else, through
GitHub's GraphQL API, whose budget is apart from the REST one the rest of CI spends: the
pull request's commits, from GitHub's comparison of its base branch with its head, and the
commits of its base branch that touch .github/issue-key-cutoff (`contents: read`; the REST
compare route reads the commits instead when that comparison is short, as it is past 1000
commits), the pull request itself, its reviews and its review comments (`pull-requests: read`,
which find-pr-payload.sh's read of the pull request a queue branch names needs too), and its
issue comments (`issues: read`). The issue-key rule's releases and diffs are REST reads, made
only once that rule is active (`contents: read`, and `pull-requests: read` for the pull request's
files). No
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

## What the pr-title job checks out

The job reads nothing of the tree but find-pr-payload.sh, check-pr-text.sh and the
check-private-names.sh that one runs, so the .github cone is the whole checkout it needs.

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
take an innocent pull request out of the queue. The run of the front group, whose base is the
tip of main, posts that group's failing ci-ok from the job that failed, through the merge-group
branch of .github/scripts/ci-ok-report.sh, so the queue reads the verdict and rebuilds the
groups above it without waiting for the rest of that run. A group behind the front posts
nothing early: the queue takes its pull request out only once the groups in front of it have
ended, and a failing ci-ok makes the queue count the group as no longer building and build
another on it, which built 41 groups in 28 minutes behind one broken entry on 2026-11-22
(.github/scripts/ci-ok-report.sh's header). Its run's ci-ok job reaches its verdict at the end of
the run and holds a failing one until the group is the front, for the same reason
(.github/scripts/ci-ok-hold.sh). The cost: when the group becomes the front before that run
ends, the entry leaves at the run's end, or when a later job of the run fails once the group is
the front, rather than the moment the groups in front end.

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

## What the cancel-started job checks out

The job reads nothing of the tree but .github/scripts/cancel-started-act.sh and the
merge-group-cancels.sh it runs, so the .github cone is the whole checkout it needs.

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
than at the last job's end. What this job does do, when its verdict fails on a merge-group run,
is hold that failure until the group is the front of the queue: a failing ci-ok on a group
behind the front makes the queue build on the failed entry, and decides nothing sooner (the
"Hold a failing verdict until this group is the front of the queue" step says why).

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

`checks: write` and `actions: read`: the verdict step reads this run's check runs and their
annotations, to tell a job a runner shutdown killed from one the change broke (ci-ok.sh's
call of runner_shutdown_kills, runner-shutdown.sh), which `checks: read` covers and the
`checks: write` here carries, and
the reconcile step reads this head's ci-ok check runs and PATCHes them with that scope.
`actions: read` is the hold step's one read of this run's creation time. The checkout needs no
`contents: read`, as `cancel-started`'s does not — this repository is public, and GitHub serves
a public repository's git data to any valid token — and the hold step's `git ls-remote` of the
queue's refs needs no token at all.

## The job reads nothing of the tree but ci-ok.sh, ci-ok.jq, ci-ok-reconcile.sh and

The job reads nothing of the tree but ci-ok.sh, ci-ok.jq, ci-ok-hold.sh, ci-ok-reconcile.sh and
the gh-read.sh and runner-shutdown.sh they source, so the .github cone is the whole checkout
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

## Why a failing verdict of a group behind the front waits for the front

On attempt 1 of a merge-group run whose verdict step failed, this step, right after the
verdict, keeps the job open until the group is the front of the queue: until the queue's
branch is at `github.event.merge_group.base_sha`, the group's parent commit. The job's own check
run is the ci-ok the queue reads, and it stays in progress while the step waits, so the queue
still counts the group as building. A failing ci-ok on a group behind the front decides nothing
sooner — the queue takes a failed entry out only once every group in front of it has ended —
and makes the queue build the next entry at once, on top of the failed one, a group the rebuild
then throws away. .github/scripts/ci-ok-hold.sh's header holds the measurement and says when the
step stops sooner: once the queue no longer holds the group (its gh-readonly-queue branch is
gone or moved), on a read GitHub refuses, which prefers the failing verdict as every refusal of
the ci-ok steps does, and when the group is CI_OK_HOLD_AGE_SECONDS, 170 minutes, old, counted
from its run's creation, so the job concludes inside the queue's own 180-minute check timeout.
It reads both refs with `git ls-remote` every 30 seconds, which spends none of the
installation's REST API budget; its one REST read, the run's creation time, waits for no
budget reset.

It keys on the verdict step's own outcome (`steps.verdict.outcome`), not on `failure()`, so a
passing verdict never waits whatever a later step does; `!cancelled()` lets it run after that
failed step. The refusal of a re-run attempt is the verdict of a tree the queue no longer waits
on, so the step skips any attempt above the first. The reconcile step after it is skipped in
exactly the case it runs, so it is the job's last step there and the job's check run concludes
the moment it ends; that reconcile would only set a posted ci-ok of this head to failure, which
every ci-ok a report step posts already is. Its own timeout-minutes, 175, lets a run created
moments before it hold for nearly the whole age, 170 minutes, and for its last pause and read, whose
ladder is 30 s, then three attempts and their pauses, two minutes; the verdict and reconcile steps
carry 10 each, the whole job's limit before this step, so their wait for a spent API budget is
as long as it was.

## Why every other ci-ok on this head is set to this job's conclusion

A ci-ok the ci-ok-report step posted stays on the head as it was: no re-run updates a
check run a job created with POST, and it can sit in another workflow's check suite, so a
run that failed and was re-run green would stay blocked by it. This step sets every ci-ok
check run on the head but this job's own to what this job concluded, so one verdict stands
at the end of every attempt (ci-ok-reconcile.sh says what it passes over and why a refusal
only warns). `job.status` is this job's conclusion, and `always()` is what runs the step
after the verdict, the "Judge every job of this run" step, failed. On a dispatch of any ref but
exactly refs/heads/main the step is skipped: that run's verdict is not the head's ci-ok, so it
sets no ci-ok of the head either (the next section says why). On a merge group's first attempt
whose verdict failed it is skipped too: the hold step ends that job, and every ci-ok posted on
that head is failure already.

## Why a dispatched run off main reports under names of its own

A workflow_dispatch run of a ref other than main tests that ref's own tree, while a pull
request's run tests the merge of the branch with its base. Both put their check runs on the
same commit, the branch's head, which is the pull request's head, so under the plain names that
commit carried two ci-ok check runs with two verdicts over two trees, and a pr-title that the
dispatched run skips. Pull request #3513 at a368687e is the case, read on 2026-11-21: its
pull_request run 44860489650, over the merge with main, failed in dispatch-browser / binaries,
dispatch-ui and docs / build, and its ci-ok check run 136722779787 concluded failure at
03:58:53Z; workflow_dispatch run 44860519340 of the branch passed, and its ci-ok check run
136723497213 concluded success at 04:02:35Z; the same run posted pr-title check run
136712596880, skipped. GitHub's own reading of the pull request did not take the dispatched
run's: the commit's statusCheckRollup held 74 contexts, 73 check runs of pull_request runs and
one of CodeQL, and no check run of the dispatched run, ci-ok failure among them. That is
GitHub's documented rule (docs.github.com, "Troubleshooting
required status checks", "Checks from some workflow jobs are not evaluated": the checks of a
workflow_dispatch run's jobs do not appear in a pull request's checks and satisfy no required
check). A reader that takes the commit's check runs by name did:
`commits/<sha>/check-runs?check_name=ci-ok` lists both, the dispatched run's first, and a
record that keeps one check run per name and lets the higher id replace the lower, as
packages/envoy/internal/cistore's does (cistore.go's check-run update), would hold the
dispatched run's success as the head's ci-ok, and its skipped pr-title as the head's pr-title.

So on such a run the `ci-ok` job is named `ci-ok (dispatch)`, and the reconcile step, "Set every
other ci-ok on this head to this job's conclusion", is skipped, since every ci-ok that step could
set is the pull request's or the queue's. GitHub evaluates a job's name only when the job runs.
ci-ok runs on every event, so it is `ci-ok` on a pull request, a merge group and the bypass
check's dispatch on main, and GitHub evaluates its name when the job is queued, after its needs:
on scratch run 44872165102 the queued check run 136750697832 was already named ci-ok. pr-title
runs on a pull request that is not a draft, on a merge group and on a dispatch, and is named
pr-title on the first two and `pr-title (dispatch)` on the third, main's included. A draft's run
skips it, leaves its name unevaluated, and its check run carries the text of the expression,
`github.event_name == 'workflow_dispatch' && … || 'pr-title'`, with the conclusion skipped:
check run 136755360755 of draft scratch run 44874260916, whose own ci-ok, 136755360774,
concluded failure (read 2026-11-21). A dispatch skipped it too until #5231 (check run
136745386648 of scratch dispatch run 44871030917, and the dispatch run of #5231's head 1bca361a),
and a check run named by an expression's literal text reads as a defect on the head, so the job's
`if:` admits workflow_dispatch and its steps check nothing there. Either way it is not named
pr-title, so a draft's head carries no pr-title from this file, and
marking the pull request ready starts the run that checks it. (pr-title.yaml, which runs on an
edit of a pull request, draft or not, still posts its own.) pr-title needs no job, so its name
can read nothing but the event: `${{ github.event_name == 'workflow_dispatch' && 'pr-title
(dispatch)' || 'pr-title' }}`, and what keeps a skipped pr-title from posting a pass under that
name is that its name is an expression at all. check-ci-filters.sh holds the job to that one
`if:` (ci_filter_coverage.py's PR_TITLE_IF).

Which dispatch is of main is read by a shell test, not by an expression. GitHub's expression
`==` and `!=` ignore case, so `github.ref != 'refs/heads/main'` is false for the branches
refs/heads/Main and refs/heads/MAIN, which git accepts (`git check-ref-format` passes both),
and `github.ref_name != 'main'` is false for those and for a tag named main too: a dispatch of
any of them would report ci-ok and run the reconcile step. The changes job's step "Read whether
this run's ref is exactly refs/heads/main" runs `[ "$GITHUB_REF" = refs/heads/main ]`, whose `=`
is case-sensitive, and writes is_main=true for exactly that ref and is_main=false for every
other; the job hands it on as the output `ref_is_main`. ci-ok's name is `${{ github.event_name ==
'workflow_dispatch' && needs.changes.outputs.ref_is_main != 'true' && 'ci-ok (dispatch)' ||
'ci-ok' }}` and the reconcile step's if: is `${{ always() && (github.event_name !=
'workflow_dispatch' || needs.changes.outputs.ref_is_main == 'true') }}`. The comparisons left
are of a fixed event name and of the string the step wrote, which case cannot change. A dispatch
whose changes job failed or was cancelled wrote no output, and reads as a dispatch of another
ref: it is named `ci-ok (dispatch)` and skips the reconcile step, main's included. Evaluated with
GitHub's expression library (@actions/expressions 0.3.61) and the step run by bash for each ref
(2026-11-21): the expression form read Main, MAIN and Main with changes failed as main, and the
shell form reads only exactly refs/heads/main as main.

The dispatched run still reaches its verdict and shows it on the head under its own name, so a
reader by name finds one ci-ok and one pr-title on a pull request's head, and they are the pull
request's own. Only a pull_request run, a merge-group run and the bypass check's dispatch on
main report ci-ok, and only the first two report pr-title: they are the runs the ruleset and the
queue read, and the bypass check's run posts its verdict on main's tip, which is no pull
request's head and where a person reading that commit looks for it. The name is chosen by the
event and the ref of the run alone, so no run has to read whether another run exists, and
nothing races. check-ci-filters.sh holds both jobs to their one `name:` (ci_filter_coverage.py's
DISPATCH_NAMES), the reconcile step to its one `if:` (RECONCILE_IF), the changes job to the
output and the exact step (REF_OUTPUT, REF_STEP_RUN), and every other job to a name that can
evaluate to neither check.

## A cache restore comes before every step that can write a path it names

In a job that restores a cache entry (actions/cache/restore, or the restore half of actions/cache),
the restore runs before every step that can write a path the restore names. For the Go module
cache, `~/go/pkg/mod`, that is every step that can run a go command: a `go` line of the step's own,
a script that runs one, or a test that starts one. A lookup (`lookup-only: true`) extracts nothing
and is not held to it. The step that broke this was a test: lint's "Check every NATS server a
script starts or installs is one the floor takes" runs `bun test scripts/e2e/lib/nats-image.test.ts`,
and that file runs `go build ./natsversion/cmd/natsfloor` when it loads.

Each restore step carries a `# why:` line naming this section: those of ci.yaml's job workflows,
ci-caches.yaml, dispatch-browser.yaml, docs-site.yaml, dispatch-browser-nightly.yaml and
release.yaml, and those of the architecture-note and spec-file actions. The restores of
release-envoy-infra.yaml and release-envoy-listener.yaml (one restore step and three setup-go
caches) follow the rule and carry no line, since each of those files names itself in its
`on.push.paths`, so an edit of either starts that release workflow on main.

Go leaves a module it extracts read-only: its files 0444, its directories 0555. The restore's tar
(`/usr/bin/tar -xf <entry> -P -C $GITHUB_WORKSPACE --use-compress-program unzstd`, GNU tar 1.35)
can neither replace nor add a file in such a directory. It logs `Cannot open: File exists` for each
file the earlier step left, extracts every other file of the entry, and exits 2, and
actions/cache/restore v6.1.0 then reports the whole restore as failed (`Failed to restore:
"/usr/bin/tar" failed with error: The process '/usr/bin/tar' failed with exit code 2`) and as a miss
(`Cache not found for input keys`). Lint's job 138186405808 (run 45317043698, a dispatch of
72001eaa368, with the restore after that test) logged 1,139 such lines, 744 of them as `##[error]`
lines, over the five modules the test's build had extracted: github.com/klauspost/compress@v1.20.1
471, golang.org/x/crypto@v0.58.0 388, github.com/nats-io/nats.go@v1.55.0 241, nkeys@v0.4.16 30 and
nuid@v1.0.1 9. The rest of the entry did land, so the notices step after it was not cold: 92 s
there, against 102 s on a clean restore with the restore first (job 138246432797, run
45335461923) and 105 s for the same script in ci-caches.yaml's notices job (job 138205905737).
What the order cost was a restore that read as a miss, a log of errors that were no fault, and the
earlier build, which compiled with no cache at all: the floor test took 12.02 s in the first job and
8.02 s in the second. `~/.cache/go-build` is writable, so tar replaces what an earlier step wrote
there without a line in the log: that order is a cost to the earlier step only, and nothing shows
it.

So the instrument is the restore's own log: a `Cannot open: File exists` line in a job's restore
means a step before it wrote the module cache. Read on 2026-11-24 over the 22 jobs with a restore of
run 45317043698 (ci.yaml dispatched, every job selected) and the jobs of ci-caches.yaml run
45323050174, lint's go-notices restore was the only one with such a line. A reading of every step
before every restore of every workflow file, and of the scripts, actions and test files those steps
run, found one other step that writes a path its restore names, in the envoy fill below. Two
earlier go commands write elsewhere: envoy-go-build runs the architecture-note and spec-file
actions before its restore of the module cache, and their builds keep module and build caches of
their own under `$RUNNER_TEMP` (.github/scripts/build-envoy-command.sh). No job restores
`~/.bun/install/cache` or `node_modules`, and no manifest of the workspace has an install script,
so a `bun install` writes no restored path.

No check of the workflow text holds this rule. The go command lint's restore met was inside a test
file a `bun test` step loads, which no reading of the step's `run:` sees. A rule that refuses a
`go` line before a restore of the module cache passes that case and refused the envoy fill below;
one that also refuses `bun install` and `bun test` steps refuses six steps that write no restored
path. A check at run time is exact: right before an extracting restore, fail when a path it names
already holds anything, and when it cannot read one. That is the next step if a job breaks this
rule again; until then the rule is this note and the line above each restore.

The envoy fill had the second instance: ci-caches.yaml's envoy job filled go-envoy and
go-envoy-test in one job, so the go-envoy-test restore unpacked over the go-envoy restore and
build, and a step before it made the module cache writable (`chmod -R u+w ~/go/pkg/mod`) so tar
could replace those files (no such line in job 138205905583, run 45323050174). go-envoy-test now
fills in a job of its own, envoy-test, which restores first and then builds the listener and
natsd as the envoy job does and the test binaries, so its entry still holds go-envoy's objects,
built on its own entry, and the chmod step is gone. A run-time check would pass both jobs.
