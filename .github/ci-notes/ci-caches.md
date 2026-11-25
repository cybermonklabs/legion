# Notes for the cache fills

The derivations and the history behind `.github/workflows/ci-caches.yaml`, the workflow that fills
the Actions caches ci.yaml's jobs restore. Each `# why:` line links to the notes for its step or
job, here or in the notes for the workflow that restores the cache.

## The seven Go families, their keys and the runners each is filled on

This workflow fills seven Go families that ci.yaml or a workflow it calls restores directly.
go-daemon, go-envoy, go-envoy-test and go-envoy-infra fill on ubuntu-24.04 and ubuntu-24.04-arm.
go-notices fills only on ubuntu-24.04-arm, and go-dispatch-browser and go-envoy-darwin-vet only
on ubuntu-24.04. See the [go-dispatch-browser fill](dispatch-browser.md#the-fill-of-go-dispatch-browser-in-ci-cachesyaml)
for its reader and inputs. The key carries `runner.arch` because Go's build cache holds the
objects of the architecture that compiled them: an entry an x86_64 runner filled gives an arm64
job its modules and none of its objects. So each family is filled on every architecture one of
its readers runs on, one matrix leg per runner. A leg builds what that runner's readers build
and no more: go-daemon's arm64 reader is typecheck alone, so its arm64 leg runs typecheck's vet
and leaves the test and race builds to the x86_64 leg.

Each key names the runner architecture and the Go version from the module its readers use.
go-daemon and go-notices use the daemon module's go.mod. go-envoy, go-envoy-test,
go-dispatch-browser and go-envoy-darwin-vet use the envoy module's go.mod. go-envoy-infra uses
the infra module's go.mod and also keys on the workspace's module files. go-notices puts the
architecture before the OS so an old OS-only restore prefix cannot match its
architecture-specific go-licenses binary. The other six put it after the OS.

## Which runners the readers of go-envoy and go-envoy-test run on

On ubuntu-24.04, envoy-go-build and envoy-go-checks restore go-envoy. The cache workflow's
darwin vet fill also restores it for its module cache. envoy-go-claim-model and the nightly
browser workflow's re-record job restore go-envoy-test on that runner. The other CI readers
of these two families run on ubuntu-24.04-arm, so the fill builds on both runner types.

dispatch-browser.yaml's binaries job, on ubuntu-24.04, restores go-dispatch-browser and not
go-envoy-test ([its restore's notes](dispatch-browser.md#why-the-binaries-job-restores-go-dispatch-browser)
say why).

## Why each fill builds on the newest entry of its family

go-daemon, go-notices, go-envoy and go-envoy-test used to save only when their exact key was
missing, so they were filled once per Go version. The go-daemon, go-envoy and go-envoy-test
entries every job restored on 2026-11-22 had been saved on 2026-11-18 between 13:03 and 13:08Z
(go-notices' on 2026-11-19), and at main's e7381b7d4c9 a job that restored them had to compile
again 312 of the daemon module's 335 packages and 573 of the envoy module's 618 (`go list -test
-deps -f {{.Stale}}`); from an entry filled five merges earlier, 135 and 321.
go-dispatch-browser was filled that way longer, and
[its fill's notes](dispatch-browser.md#the-fill-of-go-dispatch-browser-in-ci-cachesyaml) hold
its own readings.

The refill-superseded job keeps one entry per family, runner, Go version and ref: the one the
fill just saved. It deletes the family's other entries of that runner and Go, those whose key is
the readers' key followed by `-` and any entry under the readers' bare key, which would otherwise
win every reader's restore as an exact match. An entry of another runner or another Go stays. It
deletes nothing until it finds the new key in that ref's list, so a save that did not land leaves
the old entries in place. Without this cleanup the entries would pile up: each fill saves a new
one, GitHub drops an entry only after seven days unread, and over the repository's cache limit
it evicts the least recently used entries, which can be another family's live one.
A job downloading a deleted entry gets a cache miss. actions/cache/restore warns and the job
goes on, compiling what the entry would have given it, so it is slower, not failed.

## Why each fill keeps only what its own build used

Each fill keeps in the build cache only what its own build used: a step before the build marks
every restored entry unused, the build marks what it reads or writes, and a step after it deletes
the rest (.github/scripts/go-cache-working-set.sh), each with the GOCACHE the build uses, which
for go-envoy-darwin-vet is the vet's own directory. The envoy job fills go-envoy-test on top of
go-envoy, so go-envoy-test's pair also keeps the entries go-envoy's build used, and that entry
holds both builds. Without that, an entry grew with each fill: a build adds the objects of every
package that changed and nothing takes the replaced ones out, since Go's own trim deletes only
what no build has used for five days and every fill was within that.
go-envoy-test grew from 690 MB on 2026-11-22 to 2,039 MB by 19:18Z on 2026-11-23, and its restore
in the binaries job of main's dispatch-browser.yaml, which restored go-envoy-test, from 15.5 s to
59 s at the median. The module cache keeps every module version it has downloaded (Go removes
none by itself), so it grows only with a change to a module file, until a Go bump starts the
family again: after a bump the fill finds no entry of the new Go (unless an older version's text
starts with the new one's, as the workflow's prefix note says) and builds from nothing.

## Why a lookup's save takes the lookup's key

The build can change a hashed file (a go command may add the checksums it fetched to go.sum or
go.work.sum), so a hash taken after the build need not be the lookup's. go-envoy-infra's save
hashed its files again, and its key drifted: in run 45088325195 the lookup asked for
...-85ddfce1... and the save wrote ...-f2bbc22a..., so every push missed the lookup and built for
seven to ten minutes a leg.

## The architecture-check and spec-check families

The architecture-check job fills architecture-check-<os>-<arch>-go<version>-<hash>, the family of
the composite action .github/actions/architecture-check, and the spec-check job fills
spec-check-<os>-<arch>-go<version>-<hash>, the family of .github/actions/spec-check; ci.yaml's
envoy-go-build runs both actions. Neither is a lookup step of the workflow: each action restores
and saves its entry itself, keyed on the runner's architecture, on the Go version its own setup-go
installs from packages/envoy/go.mod and on the SHA-256 of packages/envoy/go.sum, so both jobs run
on ubuntu-24.04, the runner envoy-go-build runs on. On a miss it restores the newest entry of that
Go version (restore-keys). It saves only on a miss of the exact key in a push, schedule or
workflow_dispatch run: a run of any other event (pull_request, pull_request_target, merge_group,
issue_comment, workflow_run and the rest) may hold another person's code, so it restores the entry
and never saves it. The workflow's push to main is where the entry lands in the scope every later
run reads. check-ci-filters.sh's cache-family rule reads the workflow's own actions/cache steps and
not a composite action's, so it holds nothing of these two families; the "Caches" paragraph of
.github/ci-notes/ci.md#the-router-every-check-a-pull-request-or-the-merge-queue-runs is the record
of their restores and saves.

## The two entries that went with the jobs that read them

Two entries that were no Go family went with the jobs that read them - the Playwright browsers and
the .deb files of the apt list the browser suite installed - because dispatch-browser.yaml's shard
and alone jobs run in the image this repository builds, which carries the browsers and their host
packages, so neither entry has a restore left to serve (dispatch://INFRA-1443). A save whose
restore is gone is what check-ci-filters.sh refuses, so the two jobs that wrote them are gone with
them.
