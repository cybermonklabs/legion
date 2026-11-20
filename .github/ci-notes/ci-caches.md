# Notes for the CI caches workflow

The derivations of `.github/workflows/ci-caches.yaml`'s head comment, which that workflow's `# why:`
lines at its head point into, one section per paragraph of it, in the order the paragraphs stood.
The paragraph on the elan home and the entries that went with the browser suite's image stays in
the workflow's head, and the comments on its triggers, its concurrency, its permissions, its env
and its jobs stay in the workflow file beside what they are about.
In the text moved here, "this workflow" refers to `.github/workflows/ci-caches.yaml`.

## What this workflow fills, and why it runs on a push

Fills the Actions caches ci.yaml's jobs restore, so a pull request or a merge group finds
them warm. ci.yaml runs on a pull request, a merge group and a dispatch, and no longer on a push
to main: the queue tests every change on the tree it lands as, so a run on main would only test
that tree again (.github/ci-notes/ci.md#why-the-router-runs-on-these-events-and-no-others says
it). No step of that workflow saves a cache entry any
more, and the actions it calls save only in a dispatched run, so the families it restores need
a filler of their own, and this workflow is it.

Why the filler runs on a push: a run reads the cache scope of its own branch and that of the
default branch, and only an event that writes the default branch's scope - a push to main among
them - leaves an entry every later run can read. A save made in a merge group or on a pull
request lands in that run's own scope, which the next pull request's run cannot read, so it
would reach nobody.

## The Go families, their keys and how each is filled

Ten Go families, each one a restore step of ci.yaml's run names, keyed on the runner's
architecture, on the Go version the family's own module installs, and on the hash of the files
that decide what its readers fetch: the module files of each workspace their fetch files name,
those fetch files (.github/go-fetch/<job>.txt), each tool pin file they name, and
.github/scripts/go-fetch.sh (go-notices on the notices scripts and target lists too):
go-notices (lint's notices step), go-daemon (typecheck, daemon-go, plugin-pack and pi-legion),
go-envoy (envoy-go-checks, envoy-go-race, envoy-go-race-api, envoy-go-race-docs,
envoy-go-docs-finalizers, listener-server-change, envoy-e2e-local, envoy-session-stall,
airlock-lean, broker-multiprocess, dispatch-memory and dispatch-multiprocess, and the embedder
job of .github/workflows/dispatch-browser.yaml), go-envoy-test (envoy-go,
envoy-go-claim-model, which builds the API's test binary of that set, and the binaries,
multiprocess and visual jobs of .github/workflows/dispatch-browser.yaml, which ci.yaml's
dispatch-browser job calls: those three build the Dispatch server and the real broker's
commands, whose packages the go-envoy entry's build of the listener and natsd alone does not
hold), go-envoy-infra (envoy-infra-go, envoy-smoke, and the build job of
release-envoy-infra.yaml), and five that hold modules alone, each what one job's fetch step
fetches, for the jobs with a fetch step that restore no family above: go-fetch-text-checks,
go-fetch-lint, go-fetch-test, go-fetch-private-names-port and go-fetch-dispatch-two-builds (the
comment above their jobs says why).
Each fill runs, before its save, the fetch file of every job that restores its family, in the
order of their paths, as each reader's fetch step runs its own after its restore: so an entry
holds what each reader's fetch fetches, and a reader that finds its key downloads nothing.
check-ci-filters.sh holds the two sides to one list (rule 22) and each key to the files it must
hash (rule 23).
The key carries `runner.arch` because Go's build cache holds the objects of the architecture
that compiled them: an entry an x86_64 runner filled gives an arm64 job its modules and none of
its objects. So each family is filled on every architecture one of its readers runs on, one
matrix leg per runner: go-notices on ubuntu-24.04-arm alone, where lint runs, and go-daemon,
go-envoy, go-envoy-test and go-envoy-infra on ubuntu-24.04-arm and on ubuntu-24.04 both, since
each has a reader on each (typecheck and the envoy lanes on the first; daemon-go, plugin-pack,
pi-legion, envoy-go-checks, envoy-go-claim-model, envoy-smoke, the browser lane and the release
builds on the second), and each go-fetch family on the one runner its job runs on: ubuntu-24.04
for dispatch-two-builds, ubuntu-24.04-arm for the other four. A module cache holds no objects,
but `go list -deps` follows the imports of one GOOS and GOARCH, so a fill on the reader's own
architecture fetches what that reader's fetch does.
A leg builds what that runner's readers build and no more: go-daemon's arm64 reader is
typecheck alone, so its arm64 leg runs typecheck's vet and leaves the test and race builds to
the x86_64 leg.
go-notices carries the architecture before the OS (go-notices-<arch>-<os>-go<version>) and
every other family after it: the go-notices entry holds go-licenses, a program lint's notices
step runs, and a run of a workflow file older than the architecture in the key restores by
go-notices-<os>-, which an <os>-<arch> key starts with (ci-job-lint.yaml's restore step says what
that did). One
Go setup per leg, since the key carries the version that module's go.mod names: the daemon's
go.mod for go-daemon and for go-notices, which lint installs the daemon's Go for; the envoy
module's for go-envoy and go-envoy-test; the infra module's for go-envoy-infra; for each
go-fetch family, the go.mod its reader sets Go up from. Each family is
first looked up (`lookup-only`, and no `restore-keys`: the question is whether that exact key
exists, and the answer downloads nothing), and only a miss runs the fetch and the build the
ci.yaml jobs run and saves what it left in the module cache and the build cache. A family whose
key is already there builds nothing, so a run that finds every key costs a checkout and a
setup-go. Each job fills one family: an entry holds what every step before its save left, so a
second family's fill in the same job would hold the first one's fetch too. A leg of a matrix
that fails leaves the other running (`fail-fast: false`): a cancelled leg saves nothing, and one
architecture's fill has no reason to cost the other's.

## The two families the check actions keep themselves

Beside those ten, the architecture-check job fills
architecture-check-<os>-<arch>-go<version>-<hash>, the family of the composite action
.github/actions/architecture-check, and the spec-check job fills
spec-check-<os>-<arch>-go<version>-<hash>, the family of .github/actions/spec-check; ci.yaml's
envoy-go-checks runs both actions. Neither is a lookup step of this workflow: each action
restores and saves its entry itself, keyed on the runner's architecture, on the Go version its
own setup-go installs from packages/envoy/go.mod and on the SHA-256 of packages/envoy/go.sum,
so both jobs run on ubuntu-24.04, the runner envoy-go-checks runs on.
On a miss it restores the newest entry of that Go version (restore-keys, which this workflow's
own lookups do not use). It saves only on a miss of the exact key in a push, schedule or
workflow_dispatch run: a run of any other event (pull_request, pull_request_target,
merge_group, issue_comment, workflow_run and the rest) may hold another person's code, so it
restores the entry and never saves it. This workflow's push to main is where the entry lands in
the scope every later run reads. check-ci-filters.sh's cache-family rule reads this workflow's
own actions/cache steps and not a composite action's, so it holds nothing of these two families;
the "Caches" paragraph of
.github/ci-notes/ci.md#the-router-every-check-a-pull-request-or-the-merge-queue-runs is the
record of their restores and saves.

## The apt entry, which is no Go family

A second entry that is no Go family, beside the elan home the workflow's head names: the apt
job's, the .deb files and checksums of every pin of
.github/apt-packages.txt, which daemon-go and pi-envoy (tmux), claude-envoy and dispatch-ui (zsh),
dispatch-unit-repeat.yaml's harness lane (zsh) and e2e.yaml's proofs (their rows' packages)
restore and install with no network (.github/scripts/apt-cache.sh, whose header says how). Its
key is the runner's OS, architecture and release and the hash of that list and that script, so
a pin moves to another version, or the script changes, with a fresh entry on the next push.

## What this workflow does not fill

go-docs is not here: docs-pages.yaml saves it on this same push, from the build job it shares
with ci.yaml's docs job. The Bun cache is not here either: .github/actions/setup-bun reads and
writes it on main, and five workflows that run there call it.
