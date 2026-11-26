# Notes for the release workflow

The derivations and the history of `.github/workflows/release.yaml`, which points into this file.
Each `# why:` line of that workflow names the section below that holds the derivation or the
record its step, its job or its key rests on, and the sections stand in the order their comment
blocks stood in the workflow file. A value's own arithmetic stays beside the value, and a comment
inside a `run:` body stays beside its command, in the workflow file, or in the script its step
runs.
In the text moved here, "this workflow" refers to `.github/workflows/release.yaml`.

## Why the workflow level permissions block is empty, and the four stages a release waits for

Every job declares its own permissions (ref, legion_build, legion_names, pi_envoy_build,
pi_envoy_names, pi_legion_build, pi_legion_names, claude_envoy_build, claude_envoy_names,
publish, worker_image, worker_image_attest, worker_image_release); nothing runs under this
workflow-level default, so it stays empty rather than becoming an unreviewed grant to a job
added later with no block of its own.

Each package is built by a build job of its own and published by the one publish job. A build job
versions, installs, packs, builds and tests: it runs the dependencies' code and every third-party
module it links, so it holds no write scope and no secret, uploads what it made as an artifact,
and ends in a tripwire (`release-push.sh no-credential`) that fails when a credential is reachable
from it anyway. Each build job's names job (legion_names, pi_envoy_names, pi_legion_names,
claude_envoy_names), between it and publish, checks what that build hands publish for private
names with the deployment's term list, which no build job holds: the changelog entries publish
writes into the release's body, and claude_envoy_names the plugin's packed build too. A names
job runs no dependency code, and publish runs a package's release only when its names job
succeeded. The publish job holds the credentials and runs no dependency code: only
release-push.sh, git, gh, jq, sha256sum, npm (view, and publish of a handed tarball) and the
pinned actions its steps use (actions/checkout, actions/setup-node, actions/download-artifact, actions/attest-build-provenance), with no cache restored (`cache-mode: none`) and the deploy key in the env
of the steps that push. release-push.sh handed checks what each build job handed over before
anything acts on it.

So a release waits for a runner five times: for the ref job, which decides whether the run is on
main; for the four build jobs together; for their names
jobs; for publish beside worker_image's build; and for worker_image_attest's attestation beside
worker_image_release. Each stage holds what the one before it may not: publish cannot run in a
job that built what it publishes, a names job holds the term list no build job may, the image
build needs the plugin pair legion_build decides, and the attestation, which holds id-token and
attestations: write, cannot run in the job that ran the Dockerfile.

Every job condition downstream of a skippable job takes one shape: `always() &&
needs.<job>.result == 'success' && …` before it reads that job's outputs. GitHub gives an `if:`
that calls no status function an implicit `success()`, false whenever any job upstream through
`needs`, at any distance, was skipped (actions/runner#491), so a condition reading a build's
`skip` output without `always()` never decides anything once some plugin job upstream skipped:
that is what skipped legion's publish job in the first run on the build/publish split (run
38822866387) while legion_build had cut a version. `always()` turns that gating off, so each such
condition tests the result of every job whose outputs it reads, else a skipped or failed job is
read through its empty outputs. check-release-credentials.sh rule 5 pins it.

## Why a run a newer one replaces loses nothing

Only one run at a time by design, and a pending run GitHub replaces with a newer one loses
nothing: each build releases every commit in its package's paths since that package's last tag,
not this run's commit alone, so the next run releases every commit a replaced run would have.

## What the version argument lists are, and the Envoy plugin's own

Each package's version arguments, after its tag prefix: the one list its build job's version
step passes to `release-push.sh version`, and that legion_build passes for the two Oh My Pi
plugins too, to decide the plugin pair it links. A step expands one unquoted on purpose: it is
the argument list, split on its spaces. release-bump.sh reads a --patch-path only when it is
also one of the paths after --.

The Envoy Oh My Pi plugin. The seed before the first pi-envoy-v* tag is the version line this
package continues, @sjawhar/pi-legion-envoy, which it was split from: its own name on npm holds
only a placeholder 0.0.0 until its first publish, so reading it would restart the line at 0.x.
scripts/pi-plugin-prepack.sh copies this plugin's eleven skills into dist/skills, which every
Oh My Pi session loads as the plugin's skills, so a commit there releases whatever its type. Its
notices generator, the prepack itself and patches/ (the bundle inlines nats.js with
patches/nats@2.29.3.patch applied) are shipped build inputs too, so they get the same patch
release.

## The Legion plugin's version arguments

The Legion Oh My Pi plugin, seeded as the Envoy one is. scripts/pi-plugin-prepack.sh copies
this plugin's six skills into dist/skills, which a Legion pane's prompts load, so a commit
there releases whatever its type. Its notices generator, the prepack itself and patches/ (the
bundle inlines nats.js through envoy-client, with its patch applied) are shipped build inputs
too, so they get the same patch release.

## The Claude plugin's version arguments and the list they come from

The Claude Code plugin, @sjawhar/claude-envoy. Its build runs from
packages/claude-envoy/scripts/build-inputs.txt, the one statement of what can change this
plugin's bundles and its generated legion/ plugin, so the paths of `CLAUDE_ENVOY_VERSION_ARGS` are that list's, in this
order: this package, the workspace packages its bundles inline, pi-legion's agents and all of
skills/ (scripts/pi-plugin-prepack.sh stages pi-legion's share into legion/skills and this
package's skills/ links into skills/), the notices generator, the prepack and patches/ (the
bundles inline nats.js with patches/nats@2.29.3.patch applied). Everything after
packages/claude-envoy/ is content the plugin ships rather than documentation, so a docs:
commit there still releases: each is a --patch-path. Nothing has been published under this
name, so --first-npm seeds the line at npm's 0.0.0 and the first release bumps from the whole
history.

## The Go legion's version arguments and the check that holds them to the build

The Go `legion`. The binary embeds its prompts (internal/prompts/roles and go), so a docs:
commit there still releases. The notices scripts and the target list are runtime inputs of the
release archives and worker image, so any change to one needs a new tag rather than a CI-only
rebuild. Beside packages/daemon/, `LEGION_VERSION_ARGS` names each envoy package directory the
release build compiles, public and internal, through the daemon module's `replace` on ../envoy,
for any legion target of .github/go-release-targets.json (one binary each, which
.github/scripts/release-build-legion.sh builds): GOOS and GOARCH decide which files, and so
which imports, a build compiles. A change to any of those packages changes the binary and must
release one. The list also names the module files that decide what that build compiles and with
which toolchain: the envoy module's go.mod, and go.work, the workspace the build runs in, whose
go line and use list move the toolchain and the module versions as a go.mod does. It carries the
envoy module's go.sum too, which holds checksums and chooses nothing; the check below neither
requires nor refuses it.

The list is written out because the version step runs before setup-go
(.github/ci-notes/release.md#why-legion_build-reads-the-tags-before-setup-go), where no go
command can compute it. Rule 25 of .github/scripts/check-ci-filters.sh holds it to the build
instead, from the entry `.github/scripts/ci_job_reads.py#VERSION_ARGS` declares, and the ci-filters
job runs it on the pull request, on every change to a Go file, a module file or a workflow:

- What the build compiles: the package directories each target's build lists, from the go list
  runs lint's notices step already asks for, and the go.mod and go.work of its workspace.
- What the list reads: git decides, reading the same words from the repository's root as the git
  log of release-bump.sh does. A path ending in `/` takes in a directory alone, so `go.mod/` reads
  nothing.
- It fails naming each package directory or module file the list does not read, each listed path
  git reads no file for, and each listed path inside the build's modules under which the build
  compiles nothing. It does not refuse a listed path wider than the build, such as packages/.
- The entry pins release-build-legion.sh whole, as an Exact entry pins a step, and its go
  commands say what that text compiles: ./cmd/legion for each legion target, with CGO_ENABLED=0.
  Any change to the script fails the check, naming the first line that differs, until the entry's
  pinned text, and its go commands where the build moves, change with it.
- The step that runs it is pinned the same way: legion_build's "Build legion" runs the script's
  path alone, from the repository's root, and no other step of the workflow names it, so a
  prefix, an export or a source before it, or a mention in a comment fails. An env: of the step,
  the job or the workflow that can move what go compiles fails, and a name go never reads, such
  as another release's list in the workflow's env:, goes in ENV_ALLOWED. A step before it that
  writes $GITHUB_ENV or $GITHUB_PATH, or runs `go env -w`, fails too, and so does a shell or a
  container. A step that writes go's env file by its path is not read.

The envoy part of the list for the host's target, by hand, from packages/daemon:
  go list -deps ./cmd/legion | grep '^github.com/sjawhar/envoy' |
    sed 's#^github.com/sjawhar/envoy#packages/envoy#' | sort

## Why GOPROXY falls through to direct

A `|` between proxy entries makes a go command fall through to the next source after ANY error,
where a `,` falls through only after a 404 or a 410 (Go's module reference, "GOPROXY protocol").
proxy.golang.org reset a download mid-stream and turned a Go lane red on a module whose hash
go.sum already holds (sjawhar/legion#2512); with `|direct` the command fetches the same bytes
straight from the source and checks them against go.sum, so this is a second path, not a weaker
check.

## Why the ref job, and not github.ref, decides main

The jobs that release, publish or deploy run only on main, and the `ref` job decides what main
is: one bash test, `[ "$GITHUB_REF" = refs/heads/main ]`, whose verdict each such job reads as
`needs.ref.outputs.main == 'true'`. An `if:` cannot make that test itself. GitHub compares
strings in an expression ignoring case (docs.github.com, "Evaluate expressions in workflows and
actions", Operators), and `startsWith`, `endsWith` and `contains` do too, so
`github.ref == 'refs/heads/main'` is also true on a branch named `Main` or `MAIN`.

GitHub keeps such a branch beside `main` as a branch of its own, no ruleset of this repository
covers it, and a workflow_dispatch runs on it: measured on a scratch repository on 2026-11-21,
where a job behind that `if:` ran in a push run and in a dispatched run on `Main` while `main`
stood at another commit. GitHub itself compares the ref exactly elsewhere, measured the same day:
`on.push.branches: [main]` starts no run on a push to `Main`, so only a dispatch reaches these
jobs; two branches whose names differ only in case get two concurrency groups from a group keyed
on `github.ref`, and two Actions cache scopes; and `gh attestation verify` refused an archive
attested on main when asked for `refs/heads/Main`, so a deployment's check, which asks for
`refs/heads/main`, refuses an attestation made on `Main`. The npm publishes and the GitHub
releases and tags check nothing of the kind; the listener's `latest` step alone also checks that
its commit is on main. The `github.ref` comparisons that stay, in cache conditions
(setup-bun's `no-cache`, `.github/workflows/worker-image.yaml#cache-to`, the `restore-keys` lines) and in
worker_image_attest's `skip_attest`, still take `Main` for main. On `Main`, `no-cache` and
`cache-to` write only `Main`'s own cache scope, the `restore-keys` lines write nothing and only
restore less, and `skip_attest` skips an attestation.

`github.ref_protected` would answer only while the rulesets protect `main` and no branch named
`Main`, which a settings change outside this tree can alter. The `ref` job holds no permission
and checks nothing out, so it runs no code of this repository, and its `timeout-minutes` of 5
bounds a lost runner only. It is a job of its own in each workflow, never a step of a job that
runs code of the checkout: a job's outputs are read when the job ends, after every step of it.
release-envoy-listener.yaml, release-envoy-infra.yaml and docs-pages.yaml carry copies of it.

## What legion_build releases and why it holds no credential

The Go `legion`: linux and darwin archives of the binary, which embeds its role prompts, with
its THIRD_PARTY_NOTICES, on a legion-v* tag of the commit the run checked out. Nothing is
committed: a Go module carries no version file, so the tag alone records the version, and
`legion version` names it. Only main releases one: a workflow_dispatch from another branch skips
this job, and the worker image job, worker_image, then publishes that commit's sha- tag alone, as it does
for any branch.

legion_build versions, builds and smoke-tests the archives and hands them to publish as an
artifact. It runs the module's code and every third-party module it links, so it holds no write
scope and no secret: whatever that code does to this VM (a planted binary on the path, an
environment variable for later steps, the runner's memory) never meets the deploy key, the write
token or the OIDC token, which publish holds on a VM of its own.

## Why legion_build reads the tags before setup-go

First, before setup-go: no dependency code has run and no cache has been restored when the
tags are read. release-push.sh version releases again a tag an earlier attempt pushed for
this commit (`tagged`, which refuses a tag that is not legion-vX.Y.Z; publish's "legion: Tag
release" accepts its own tag at this commit and its "legion: Create GitHub release" reuses a
published release and publishes a draft one), and otherwise bumps from the newest legion-v*
tag over the paths $LEGION_VERSION_ARGS names (the workflow's `env:` says why each is there).

## Why legion_build resolves the plugin pair before setup-go

Also before setup-go, so no dependency code has run yet either: the plugin pair this legion
links, which the smoke test checks and the worker image installs. For each Oh My Pi plugin
it is the version that plugin's build job releases in this run, decided from the same tags
and the same arguments as that job's own version step, or, when the plugin has nothing to
release, the version of its newest tag, which npm carries: a release commits nothing, so
the committed manifest records no release and the tags alone say what was published. The
workflow's one concurrency group keeps one run of main at a time, so no other run moves a
tag between this step and the plugins' build jobs. publish's "legion: Check the linked
plugin pair" refuses to release this legion unless npm then carries exactly this pair.

## What the legion build writes and why it is static

CGO_ENABLED=0: a static binary, as the worker image builds it, so a linux archive runs on any
distribution. One archive per target .github/go-release-targets.json lists (the pull
request's notices check reads the same list), each holding one top-level directory,
`legion-<os>-<arch>`, with `legion` and THIRD_PARTY_NOTICES in it: mise's github backend
strips that one directory, leaving the binary. THIRD_PARTY_NOTICES is the license of every
Go module that target's binary compiles in (scripts/go-third-party-notices.sh, run with the
build's own GOOS, GOARCH and CGO_ENABLED), and fails the build when one cannot be determined.
publish writes SHA256SUMS over the archives it accepts.

## What legion_names checks

The private-name check of the changelog entries legion_build rendered, over the very file
publish writes into the GitHub release's body, through
.github/workflows/release-notes-names.yaml, whose header says why it is a job of its own and
what it runs. It runs whenever legion_build released, and checks when that release rendered
entries. publish needs this job, and its legion steps run only when it succeeded, so a finding
releases nothing of legion.

## What pi_envoy_build hands to publish

The Envoy Oh My Pi plugin, @sjawhar/pi-envoy. pi_envoy_build versions it, installs the
workspace and packs it (the dependencies' code, so no write scope and no secret), and hands the
tarball to publish, which publishes those bytes to npm, tags the release and creates the
GitHub release. Nothing is committed: the tag records the version.

## Why only main publishes a plugin

Only main publishes: a workflow_dispatch from another branch skips this job and publish, so
no branch build ever reaches npm or moves `latest` (CONTRIBUTING.md, "Deploys come only from
main"). A workflow_dispatch on main runs it as a push does.

## Why pi_envoy_build reads the tags before setup-bun, and what a re-run releases

First, before setup-bun: no dependency code has run and no cache has been restored when the
tags are read. A re-run after publish's "pi-envoy: Push tag" pushed the tag checks out this
same commit with that tag on it, where the range would be empty and skip the release it
never finished; release-push.sh version releases that tag again instead (`tagged`), and
every publish step is safe to repeat: npm reuses a published version, `release-push.sh
release` reuses a published release and publishes a draft one, and `tag` accepts its own
tag on this commit. Otherwise it bumps from the newest pi-envoy-v* tag over the paths
$PI_ENVOY_VERSION_ARGS names (the workflow's `env:` says why each is there).

## What the Envoy plugin's changelog step renders

This release's own changelog entries, which publish writes into the GitHub release's body.
A fragment stays under packages/pi-envoy/.changes/unreleased/ after the release that ships
it, so fold-notes renders the ones this release's own commits first added, which are the
ones its tag will hold. A release no fragment belongs to renders no file, and the `notes`
line fold-notes prints says which of the two it was, since publish cannot look into the
artifact before it downloads it.

## Why pi_legion_build reads the tags before setup-bun

First, before setup-bun, as pi_envoy_build's version step is, and releasing again a tag
an earlier attempt pushed for this commit as that step does, over the paths
$PI_LEGION_VERSION_ARGS names (the workflow's `env:` says why each is there).

## What the Legion plugin's changelog step renders

This release's own changelog entries, as in pi_envoy_build and for the reasons given
there: the fragments this release's own commits first added, and the `notes` line
fold-notes prints says whether there were any.

## What claude_envoy_build packs and why it builds the plugin first

The Claude Code plugin, @sjawhar/claude-envoy, built and published as the two Oh My Pi plugins
are. The difference is what it packs: the plugin's own tree carries no built file, so this job
builds dist/ and legion/ with packages/claude-envoy/scripts/verify-build.sh — the one
statement of what the published plugin must hold, which ci.yaml runs on every pull request —
puts that checked build into the checkout and packs it with the rest of the package.
It also packs the Codex CLI plugin, @sjawhar/codex-envoy, from that same checked
dist/ and at the same version: the two hosts' plugin rules cannot both hold in one package
(packages/claude-envoy/scripts/build.ts, CODEX_SOURCES), so the Codex one is a package of its
own, built from the same source and released with this one.
.claude-plugin/marketplace.json names this package as an `npm` source with the range
`>=1.0.0`, so Claude Code installs the newest release and never the 0.0.0 placeholder that
`latest` points at until the first one.

## Why claude_envoy_build reads the tags before setup-bun

First, before setup-bun, as pi_envoy_build's version step is, and releasing again a tag
an earlier attempt pushed for this commit as that step does, over the paths
$CLAUDE_ENVOY_VERSION_ARGS names (the workflow's `env:` says why each is there).

## What the Claude plugin's changelog step renders

This release's own changelog entries, as in pi_envoy_build and for the reasons given
there: the fragments this release's own commits first added, and the `notes` line
fold-notes prints says whether there were any.

## What the Claude plugin's build and pack step checks

What it builds and checks: packages/claude-envoy/scripts/verify-build.sh, which builds the
plugin twice into scratch directories outside the checkout, compares the two byte for
byte and packs it into the tar `release-push.sh check-built` then checks member by member.
The private-name check of the build is claude_envoy_names's, below.

## What the Claude plugin's pack step puts in the tarball

package.json's "files" decides what is packed, not git, so the build's two directories go
in even though .gitignore names them. The pack runs the package's prepack and postpack
(packages/claude-envoy/scripts/pack-skills.sh): `skills/<name>` is a link into the
repository's skills/, which bun packs as nothing at all, so the prepack replaces each
link with the skill's files and the postpack puts the links back.

## What the Codex plugin's build and pack step builds

The Codex plugin's package, built from the checked dist/ the "Pack plugin" step packed rather than
bundled again, with the manifests and skills of this tree (scripts/build.ts --codex-out).
It is written outside the checkout, as every build here is.

## What claude_envoy_names checks, and why it is a job of its own

The private-name checks of what claude_envoy_build hands publish: the plugin it packed, over
the tarball publish will hand npm (its dist/ and legion/, the very bytes verify-build.sh tarred
and extract-built put into the package before the pack), and the changelog entries it rendered,
as legion_names says. A job of its own, because the check reads the deployment's term list
(PRIVATE_NAME_TERMS) and no build job holds it: this one checks out the .github cone and runs
.github/actions/private-names-packed-plugin, which downloads the tarball, lays its two
directories out at their repository path and runs the check, and
.github/actions/private-names-release-notes when the release rendered entries, and no
dependency code (.github/scripts/check-release-credentials.sh reads the actions' steps). The
check fails a push's run without the list. publish needs this job, and its claude-envoy steps
run only when it succeeded.

## What the publish job runs, and why one job publishes all four packages

Publishes what the four build jobs made, in one job for all four packages, so their release
waits for a runner once: pi-envoy, pi-legion, claude-envoy, then legion, which links the two
Oh My Pi plugins and so goes only once both are on npm. It runs no bun, no node, no Go,
nothing a build job made and no scripts/
program, only release-push.sh, git, gh, jq, sha256sum, npm (view, and publish of a handed
tarball) and the pinned actions its steps use (actions/checkout, actions/setup-node, actions/download-artifact, actions/attest-build-provenance), so the scopes and the deploy key it holds never meet the
code the build jobs ran. release-push.sh itself also runs curl, tar and one binary of its own:
the gitleaks release it fetches and holds to a pinned sha256, which runs this repository's
private-name rules (.github/private-names.toml, from this checkout)
over every write this job's steps make to a public repository and refuses the write when they match. That
binary reads what is about to be published and nothing else, and it is a pinned upstream
release, not code from a build job.
It runs on main even when nothing releases: its pair_ok output decides
whether worker_image_attest attests the image, and each step decides for its own package.

A package's first step runs when its build released and its names job succeeded, and each later
step when the one before it in the same package succeeded (`steps.<id>.outcome`, under
`!cancelled()`). So a private name in a package's changelog entries, or a failed step, stops
the rest of its own package and no other: one package's failure never holds back another
whose build released (legion, which links the two Oh My Pi plugins, waits for both). Each
package starts from the run's commit (`git checkout --force --detach
"$GITHUB_SHA"` and `git clean -ffdx`, in its hand-over check), and nothing of a release is
committed: `release-push.sh tag` tags $GITHUB_SHA itself, which main already holds, and
`tagged` finds a tag only on HEAD. So no package's steps move main, and a re-run of this job
reads the same tags its first attempt pushed.

## Why the publish job holds each of its scopes

contents: write creates the GitHub releases (`release-push.sh release`, in each "Create GitHub release" step); id-token: write
is the OIDC token npm trusted publishing exchanges for the publish credential (the registry
records `trustedPublisher: github` for each plugin; no NPM_TOKEN is set), which also signs npm
provenance (`npm publish --provenance`) and is the Sigstore signing certificate's token for
the legion archives' provenance attestation, which attestations: write stores. The tag pushes
use the deploy key, held only by the env of each plugin's "Push tag" step and of legion's
"Tag release", which pushes its own tag, not any scope.

## What the Envoy plugin's hand-over check refuses

pi_envoy_build chooses the tarball's bytes, and hands over only it and the version. This
step puts the checkout back on the run's commit, then names the tarball itself, from that
version and the committed manifest's name (Bun names a scoped tarball
sjawhar-pi-legion-1.2.3.tgz); release-push.sh handed refuses a version that is not X.Y.Z and
an artifact that is not exactly that one regular file, and prints the tag. The "pi-envoy: Publish to npm" and "pi-envoy: Create GitHub release" steps
take the name's stem and spell the `.tgz` themselves, so each reads as the publish of a
tarball (check-release-credentials.sh allows `npm publish` of nothing else).

## Why the deploy key reaches the Envoy plugin's tag push alone

The deploy key reaches this step alone of this package's. release-push.sh push-tag writes
it to a directory of its own, pushes over SSH to GitHub's pinned host keys, and removes the
key again. The tag goes before the release, the "pi-envoy: Create GitHub release" step, which verifies it on GitHub; the tag is
all this package puts in the repository.

## How the Envoy plugin's changelog reaches the release body

pi_envoy_build's entries become the release body's one "## Changelog" section, below the
notes `release-push.sh release` generated for it. release-section replaces a section that
heading already holds, and says there why the write is a replace and not an append. The
artifact holds the one file fold-notes wrote, under the name the build job gave it.

## Why the Claude plugin is not part of the plugin pair

claude-envoy: what claude_envoy_build packed, published as the two Oh My Pi plugins, pi-envoy and pi-legion,
are. It is not part of the plugin pair the "Check the plugin pair npm carries" step decides: that pair is the two Oh My Pi plugins the
Go `legion` links and the worker image installs, and neither the binary nor the image
reads this one, which Claude Code installs for itself from the marketplace. So a run whose
steps for this plugin fail still releases legion, in the steps after them. This job does
need claude_envoy_build, as it needs every build job, so a legion release waits for that
build to finish, whatever it decided; and it needs claude_envoy_names, whose private-name
check of the packed plugin must pass before the plugin is published.

## What the publish job does with the Codex plugin

codex-envoy: the Codex CLI plugin claude_envoy_build packed beside claude-envoy, from the
same build and at the same version, published to npm on its own name and tagged with
claude-envoy's tag alone. npm's trusted publishing cannot create a package, so until an
owner of the @sjawhar scope has published the name once by hand and named this workflow
its trusted publisher, this step says so and publishes nothing; any other failure of the
registry read is an error. Like claude-envoy's steps, none of these gates the legion
release, the "legion:" steps. Its first step also needs claude_envoy_names: the Codex package's dist/ is
the checked dist/ that job's packed-plugin check reads, copied byte for byte
(scripts/build.ts --codex-out --dist), and the rest of it is written from this tree's
codex/, skills/ and scripts/build.ts, which the merge queue's private-names job read, so a
finding there stops both packages.

## What the plugin pair check decides

The plugin pair npm carries for this run, which legion links and the worker image installs:
for each Oh My Pi plugin, the version its build released when "Create GitHub release", the
step that publishes its package, succeeded, or, when its build had nothing to release, the
version of its newest tag, which npm carries: a release commits nothing, so the committed
manifest records no release and the tags alone say what was published. ok=true only when
both plugins are in one of those states; otherwise legion is not released and
worker_image_attest does not attest the image on main, so neither links a plugin version
npm lacks.

## Why legion is released only once both plugins are on npm

legion: what legion_build built and smoke-tested, once legion_names found no private name in
its changelog entries. It goes only once both plugins it links are on npm, and this first
step refuses archives that link any pair but the one npm now carries: the binary installs
that pair from npm on a stale-plugin relaunch.

## What legion's hand-over check refuses, and where SHA256SUMS is written

legion_build chooses the archives' bytes, and hands over only them and the version. This
step puts the checkout back on the run's commit, then names the archives itself, from that
version and that commit's target list; release-push.sh handed refuses a version that is not
X.Y.Z and an artifact that is not exactly those archives, each a regular file, and prints the
tag. SHA256SUMS is then written over them, and the tag and the explicit lists go to the
steps after it, from "legion: Tag release" on.

## Why the deploy key reaches legion's tag step alone

The only legion step that holds the deploy key. release-push.sh push-tag writes it to a
directory of its own, pushes the tag over SSH to GitHub's pinned host keys, and removes the
key again. The tag names $GITHUB_SHA, the commit legion_build built, which is what every
package of this job checks out and tags.

## What the archives' attestation is for and how a consumer verifies it

Build provenance for the archives a mise pin installs, which the "legion: Create GitHub release" step uploads
exactly, so a downloaded asset is checked against a run of this workflow on main rather than
against SHA256SUMS, which sits in the same mutable release. `--repo` alone accepts an
archive any workflow of this repository attested, so the check pins the signer as
docs/kubernetes.md ("Pin by digest, never by tag") does:
  gh attestation verify `legion-<os>-<arch>.tar.gz` --repo cybermonklabs/legion \
    --cert-identity https://github.com/cybermonklabs/legion/.github/workflows/release.yaml@refs/heads/main \
    --source-ref refs/heads/main --deny-self-hosted-runners
and adds `--source-digest <the legion-v<version> tag's commit>` to pin the commit as well.
It runs after "legion: Tag release", so a run whose tag step refuses (the tag already names
another commit) leaves no attestation for archives no release carries.

## What legion's GitHub release step does on a re-run

`.github/scripts/release-push.sh#release)`: creates the release, reuses a published one, or publishes a draft an
earlier attempt left, with the files the "legion: Check build's archives and write SHA256SUMS" step listed. Its
version and tag are the job's outputs, which worker_image_release names the worker image on.

## What the worker image build is given and what it pushes

The worker image, built from this run's commit with the plugin pair legion_build decided (empty
off main, where legion_build skips and the Dockerfile links none) and labelled with the legion
version legion_build decided (`legion_version`; empty when legion_build skips or has nothing to
release, which leaves the label `sha-<short>`). This first call of worker-image.yaml builds and
pushes the image's sha- tag only (`skip_attest`): worker_image_attest attests it and
worker_image_release tags it with that version and names it on the legion release once publish
released it.

## Why the worker image call declares every scope worker-image.yaml names

No `secrets:`: the called workflow reads only GITHUB_TOKEN, which it receives automatically;
`inherit` would hand DEPLOY_KEY and every other repository secret to a job that runs a
PR-authored Dockerfile. This job's permissions must cover every scope `.github/workflows/worker-image.yaml#permissions:` blocks declare, even for the `attest` job this call skips: its workflow-level
block (the build, with actions: read and checks: write for the ci-ok report its build runs
only on ci.yaml's pull_request call) and its `attest` job's (id-token and attestations:
write, for the image's GitHub attestation). The release note, which needs contents: write,
and the legion version tag on ghcr.io, which needs packages: write, are the worker_image_release job's.

## Why the image attestation is a second call of worker-image.yaml

The worker image's GitHub attestation: a second call of worker-image.yaml, whose attest job
attests the digest worker_image pushed (`attest_digest`; its build job then skips). It stays in
worker-image.yaml, whose file the consumers' `gh attestation verify --cert-identity` names
(docs/kubernetes.md, "Pin by digest, never by tag"), and it holds id-token and attestations:
write, so it cannot run in the job that ran the Dockerfile. It waits on publish so an image
attested on main never links a plugin version that did not publish: on main it attests only
when publish's pair_ok says both plugins it links are on npm. That condition is the
`skip_attest` input rather than this job's `if:`, because check-image-trigger-paths.sh reads
the `if:` of a job calling worker-image.yaml as an image gate and refuses one that tests another
job's outputs or the ref. Off main (a workflow_dispatch from a branch) publish skips and the
image is attested, as a branch dispatch's image always was; on a branch named `Main` or `MAIN`,
which that `github.ref` comparison takes for main, publish skips and the image is not attested.

## Why the attest call is handed the image repository

The image repository the attestation names. worker-image.yaml derives it from
$GITHUB_REPOSITORY in its build job, which this call skips (attest_digest), and its attest
job holds id-token and attestations: write, so it runs no script to derive it for itself.
The build-only call, the worker_image job, reported the repository it pushed to as its `image` output, so
this call hands that value back: both calls name one image, whatever the repository is
called.

## What names the worker image on the legion release

Names the worker image on the legion release this run made: tags the image's digest with the
legion version on ghcr.io, so a version tag still means "released", and writes the digest
reference into the release notes. It runs when legion released in this run, even when a later
package in publish failed. It runs beside worker_image_attest, not after it: if the attestation
fails, the release names a digest that the consumers' `gh attestation verify` refuses, which
fails closed. It runs no build and no dependency code, only the runner's own `docker buildx
imagetools`, release-push.sh, gh and the pinned login action, so the scopes it holds never meet
the Dockerfile worker_image ran.

## Why worker_image_release checks the run's commit out

For release-push.sh, which the last step runs: nothing else checks this job out, and that
step would exit 127. The run's commit by sha, the commit the release this names was built
from; no credential is left in the checkout.

## Why the version tag is a carbon copy of the manifest the build pushed

The runner's own docker buildx runs imagetools, so this job sets up no BuildKit builder.
--prefer-index=false makes the tag a carbon copy of the single manifest the build pushed,
where the default would wrap it in a new index of another digest; the read-back in this step's `run:` (`docker buildx imagetools inspect`) fails
the job unless the tag names exactly the digest the release note and the attestation name.

## What the release's worker image section holds

The release's body gets one "## Worker image" section, naming the digest the version tag
points at now. release-section replaces a section that heading already holds, which is what
a whole-run rerun of this image needs, and says there why the write is a replace.
