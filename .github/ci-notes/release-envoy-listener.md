# Notes for the envoy listener release workflow

The derivations and the history of `.github/workflows/release-envoy-listener.yaml`, which points
into this file. Each `# why:` line of that workflow names the section below that holds the
derivation or the record its step, its job or its key rests on, and the sections stand in the
order their comment blocks stood in the workflow file. A value's own arithmetic stays beside the
value, and a comment inside a `run:` body stays beside its command, in the workflow file.
In the text moved here, "this workflow" and "this file" refer to
`.github/workflows/release-envoy-listener.yaml`.

## What the push paths list, and why

Every file packages/envoy/docker/Dockerfile builds from: the Go module and the Dockerfile
(packages/envoy/**), the web stage's frozen install (the root manifest, lockfile, patches and every
root workspace's package.json), the Bun pin that stage holds its own Bun to, the sources it builds
the SPA and the Go contracts from, the context's .dockerignore, and this workflow. A commit touching
only one of them would otherwise publish no image, so no pin could name it.
.github/scripts/check-image-trigger-paths.sh fails the build when this list misses a file the image
build reads. The envoy-infra plugin's module (packages/envoy/infra) is no input of the image or of
this release's binaries, and release-envoy-infra.yaml releases it.

## Why only the newest waiting push builds, and why a dispatch is never replaced

One build at a time per ref, and of the pushes waiting in the group only the newest builds: the
default queue (no `queue:` key) keeps one run waiting and replaces it with the next arrival
(docs.github.com, "Control the concurrency of workflows and jobs"), and cancel-in-progress: false
never cancels the running one. A replaced run is cancelled having started no job at all, so it costs
no runner minute, and the push that replaced it holds its commit: what the replaced commit loses is
an image and a version number of its own, never its change (release-push.sh version counts every
commit since the newest tag, and packages/envoy/deploy/scripts/autodeploy.sh deploys the newest of
main's last 30 commits that has a published image, so the next release carries every older commit's
change). That sentence is this repository's one copy of the reasoning: the Confirm the version step
of release_publish, `.github/scripts/release-push.sh#restart)` and
.github/scripts/check-release-credentials.sh's rule 4 point to this section instead of restating it,
so the day autodeploy reads another number of commits there is one paragraph to correct.

Each `workflow_dispatch` is a group of its own, keyed on github.run_id, and is never replaced:
release-push.sh restart starts a dispatch when a run loses its version race, and a group keyed on
the ref alone would let a newer push replace that waiting restart and leave its commit in no release
— the state .github/scripts/check-release-credentials.sh rule 4 exists to prevent.

release_publish keeps its job-level `release-envoy-listener` group with `queue: max` as well: the
workflow-level group coalesces runs that have started no job, the job-level one serializes the jobs
of runs that have already built and pushed an image, and none of them is dropped.

## Why the workflow level permissions are empty, and the three kinds of job

Every job declares its own permissions (ref, targets, build, docker, image_index, attest,
release_build, release_names, release_publish); nothing runs under this workflow-level default, so
it stays empty rather than becoming an unreviewed grant to a job added later with no block of its
own.

The release is split into build jobs and credentialed jobs. build, docker and release_build run the
module's code and every third-party module it links, so they hold no secret and no write scope
beyond docker's package push, and each ends in a tripwire (`release-push.sh no-credential`) that
fails when a credential is reachable from it anyway. attest and release_publish hold the credentials
and run no dependency code: attest runs one pinned action; release_publish only release-push.sh,
git, gh, jq, sha256sum, `docker buildx imagetools` and the pinned actions of its steps. Neither
restores a cache (`cache-mode: none`), and the deploy key is in the env of the one step that pushes
the tag. image_index is neither: it holds docker's package push alone and runs no dependency code,
only `docker buildx imagetools` over the digests the docker legs hand over. Nor is release_names,
which holds the deployment's term list and no write scope and runs no dependency code: it checks the
changelog entries release_build rendered for private names, and release_publish releases only when
it succeeded.

## The arguments of the one version computation

The arguments of the one version computation, shared by release_build (which decides the version)
and release_publish (which confirms it, --expect, inside the concurrency group): every notices
script changes an artifact this release publishes, and the target lists decide the archives, so they
take a patch release; packages/envoy/infra is excluded, since release-envoy-infra.yaml releases it
and nothing this workflow publishes compiles it in.

## What the targets job reads and derives, and why it is a build job

The targets job reads the architectures the archives are built for: .github/go-release-targets.json,
the list the pull request's notices check reads too. A full checkout to read one tracked file is
more than the targets job needs, but it's what every other job of this workflow that reads the tree
(and the wider CI config) does regardless of how much of the tree it touches; a sparse or shallow
checkout in targets alone would be the inconsistency.

It also derives the image every later job pushes, tags, attests or reads back, from
$GITHUB_REPOSITORY (.github/scripts/repo-paths.sh), so moving the repository to another owner or
name moves the image with it instead of leaving a literal in five places. It is derived in targets
and read everywhere else through its output because each of those jobs holds packages: write,
contents: write, id-token or attestations: write, and a job holding a credential runs no script of
this repository but release-push.sh (.github/scripts/check-release-credentials.sh). The targets job
holds contents: read alone, so it may run one - which makes it a build job, hence the tripwire last,
as in build, docker and release_build.

## What the envoy archive holds, and why the binary is stamped

The build job's archive holds envoy-listener and THIRD_PARTY_NOTICES, the license of every Go module
the binary compiles in (scripts/go-third-party-notices.sh, run with the build's own GOOS and
GOARCH), which fails the build when one cannot be determined. The binary is stamped with this run's
commit (internal/buildversion's commit), as the image's binaries are
(packages/envoy/docker/Dockerfile's LEGION_COMMIT), so every NATS connection of a listener run from
the archive names its build (`legion@<commit>`), which legacy-subjects-gate.sh's census of
connections reads; -buildvcs=false records no other commit. Its Build envoy binary step reads the
stamp back from the binary and fails without it.

## Why the image is built one leg per platform

The docker job runs one leg per platform, each on a runner of that architecture, so no build runs
under emulation (Docker's "Distribute build across multiple runners"). Each leg builds and
smoke-tests its own platform, pushes it by digest with no tag, and hands the digest over as an
artifact; image_index joins the two into the `envoy:<sha>` index. A matrix job's outputs keep only
the last leg's value, so the digests travel as artifacts instead.

## Why the builder sets provenance-add-gha to false, and its image

provenance-add-gha (true by default on the docker-container driver) stages $GITHUB_EVENT_PATH's full
JSON into any provenance predicate a build of the docker job computes, attached to the pushed image
or not. false keeps every build this builder runs — smoke and publish alike — free of that,
regardless of each build's own provenance setting. image= is the driver's own default BuildKit
image: ghcr.io/cybermonklabs/legion/ci, the copy of moby/buildkit:buildx-stable-1
(.github/ci-images.yaml).

## Why the smoke image is built in a step of its own

The docker job's smoke image build is a step of its own: what TestSmoke asserts is the container's
behaviour, so a slow build must fail nothing. Inside the test it spent 256.60–289.47 s of
`go test -timeout 5m` across ten runs while the five assertions measured 0.00 s each
(docs/solutions/testing/a-container-build-inside-go-test-spends-the-tests-deadline.md). The test
refuses to run without ENVOY_SMOKE_IMAGE rather than building one, so dropping that step turns the
lane red instead of restoring that budget silently. `load: true` is Docker's documented
test-before-push shape, and the only build form .github/scripts/check-image-trigger-paths.sh can
read. Same Dockerfile, context, builder and LEGION_COMMIT as the docker job's publish step, so each
leg smoke-tests the platform it pushes, natively, before it pushes it, and the publish step reuses
this build's layers from the same builder: the job builds the image once.

## What the smoke test's 5-minute bound covers

5m bounds only what the test does: start both containers behind the test's own 60 s health wait, and
assert. The docker job pre-pulls nothing, so unlike envoy-smoke it also pulls NATS (testnats.Image),
unretried. envoy-smoke has measured 1.82 s, 3.16 s and 1.88 s, and a loaded devbox 2.2–20.7 s across
four runs; the bound is deliberate headroom over those plus that pull, not a build budget.

## What mode=min provenance attaches, and what the two env settings drop

The docker job's publish step (`id: build`) sets `provenance: mode=min`, which attaches a BuildKit
SLSA provenance attestation to each leg's per-platform push, and image_index's index carries both
legs' attestation manifests. The deployment's post-apply verification reads it
(buildDefinition.externalParameters.request.root.request.args["vcs:revision"]) to map the pinned
digest back to the legion commit it was built from; provenance: false attached none. The builder's
provenance-add-gha=false, not this "min" level by itself, is what keeps this predicate free of the
GitHub Actions event. BUILDX_METADATA_PROVENANCE=disabled still drops the action's own copy of the
predicate from the job log's `##[group]Metadata` output, and DOCKER_BUILD_SUMMARY=false still drops
the separate build-summary artifact and page; neither affects what mode=min attaches to the pushed
image.

## Why the image build keeps no layer cache

The docker job keeps no BuildKit layer cache across runs. The Dockerfile passes LEGION_COMMIT (this
run's github.sha) into the build stage's `go build -ldflags` step, so every new commit runs the
build stage, and the web stage it copies from, again: a `mode=min` cache, which exports the final
stage's layers alone, would serve only a re-run at the same commit, while each main run would add
about 137 MB of Actions-cache entries (`mode=max` added about 1.6 GB) to a cache already past
GitHub's 10 GB limit. Not a registry cache either: any run holding `packages: write` can write a tag
main's attested build would read (GHSA-f2v9-hprr-32q3). Uncached, each leg takes 4.2-5.3 minutes
(docs/solutions/github/measure-a-build-cache-by-what-a-new-commit-invalidates-and-a-hit-by-same-run-job-reruns-pr2297.md).

## Why the digest artifact is not named legion-envoy

The docker legs' digest artifact's name must not start with `legion-envoy-`: release_publish
downloads `pattern: legion-envoy-*` into the release's assets, and release-push.sh handed refuses
anything there but the archives. The repository's default retention applies, so a re-run of
image_index or of one failed leg still finds both legs' digests; the files are empty.

## What image_index does, and why it checks each digest

image_index joins the docker legs' per-platform images into the `envoy:<sha>` index with
`docker buildx imagetools create`, and hands the index's digest to attest and release_publish. This
is the third kind of job in this workflow: it holds packages: write and runs no dependency code,
like neither a build job nor a credentialed one, but its inputs come from jobs that ran dependency
code. So each digest the legs hand over is checked to be 64 lower-case hex characters before it
reaches a command, and so is the index digest before it leaves the job. The runner's own docker
buildx runs imagetools, so image_index checks out nothing and sets up no builder, and with no
dependency code it ends in no tripwire.

## What the image attestation proves, and why attest stays in this file

The attest job makes a GitHub artifact attestation of the index image_index wrote, stored with
GitHub. The `envoy:<sha>` tag alone cannot say which run published an image: a workflow_dispatch
from any branch pushes one too. The attestation's Sigstore certificate carries this workflow file's
identity, ref and commit from the run's OIDC token, which no workflow edit can change, so a consumer
accepts a digest only when a run of this file on main built it:

```sh
gh attestation verify oci://ghcr.io/cybermonklabs/legion/envoy@sha256:… --repo cybermonklabs/legion \
  --cert-identity https://github.com/cybermonklabs/legion/.github/workflows/release-envoy-listener.yaml@refs/heads/main \
  --source-ref refs/heads/main --deny-self-hosted-runners
```

and adds `--source-digest <the commit its tag names>` to pin the commit as well. The certificate
names the workflow whose job signed, so the attest job stays in this file: moved into a called
workflow, it would name that file and every consumer's check would refuse it. The subject is the
image index's digest, the one image_index writes with `imagetools create` and a deployment pins; a
per-platform manifest's attestation would not verify against it. This statement is GitHub's,
separate from the BuildKit provenance each docker leg attaches to its platform
(`provenance: mode=min`), which the index carries. A branch dispatch attests too, and its
certificate names `refs/heads/<branch>`, which the command above refuses.

## What release_build builds, and why it decides the version

release_build builds agent-secrets (packages/envoy/cmd/cipher) and the host helper
(packages/envoy/cmd/cipher-helper): the same legion-envoy-v* release also ships these for a host to
install with mise's github backend, in the layout its installer expects — bin/agent-secrets,
bin/agent-secrets-helper. A linux archive, `agent-secrets-<arch>.tar.gz` for each architecture the
envoy key of .github/go-release-targets.json names, holds both; a darwin archive,
`agent-secrets-darwin-<arch>.tar.gz` for each target .github/agent-secrets-client-targets.json
names, holds bin/agent-secrets alone, since the helper's build constraints admit linux only: a
laptop runs the CLI, never the helper.

release_build decides the release's version first and stamps the tag into each binary: `--version`
prints it and the helper's startup line carries it, which is how a restart onto a release is
checked. It runs outside release_publish's concurrency group, so two runs can decide the same
version; release_publish confirms the version against the tags inside that group before it tags, and
when another run took it first, publishes nothing and sees to it that a run releases this commit
("Confirm the version"). Like build and docker, release_build runs the module's code and holds no
write scope and no secret: whatever that code does to this VM never meets the deploy key, the write
token or the OIDC token, which release_publish holds on a VM of its own. Only main releases: a
workflow_dispatch from any other branch publishes its `envoy:<sha>` image and stops there.

## Why the version is read first, before setup-go

release_build reads the version first, before setup-go: no dependency code has run and no cache has
been restored when the tags are read. release-push.sh version releases again a tag an earlier
attempt pushed for this commit (`tagged`, which refuses a tag that is not legion-envoy-vX.Y.Z) and
otherwise bumps from the newest legion-envoy-v* tag. $LEGION_ENVOY_VERSION_ARGS is expanded unquoted
on purpose: it is the argument list, split on its spaces.

## Which changelog entries the release renders

release_build renders this release's own changelog entries, which release_publish writes into the
GitHub release's body. A fragment stays under packages/envoy/.changes/unreleased/ after the release
that ships it, so fold-notes renders the ones this release's own commits first added, which are the
ones its tag will hold. A release no fragment belongs to renders no file, and the `notes` line
fold-notes prints says which of the two it was, since release_publish cannot look into the artifact
before it downloads it.

## Why the agent-secrets archives are static and wrapped in one directory

CGO_ENABLED=0 matches the existing agent-secrets builds (packages/envoy/docker/Dockerfile,
packages/daemon/docker/worker.Dockerfile): without it, a same-arch build (amd64 on release_build's
ubuntu-24.04 runner) links dynamically against the runner's glibc instead of producing a static
binary. Each archive is wrapped in one top-level "agent-secrets" directory (agent-secrets/bin/...)
rather than bin/... directly: mise's github backend auto-strips exactly one leading directory from
an asset shaped like that, so the installed tree still ends up bin/agent-secrets (and
bin/agent-secrets-helper on linux); a bare bin/... top level would itself be the one directory mise
strips, flattening the binaries to the install root instead. agent-secrets/THIRD_PARTY_NOTICES,
beside bin/, is the license of every Go module the archive's binaries compile in
(scripts/go-third-party-notices.sh, run with the build's own environment), and fails the build when
one cannot be determined.

## What release_names checks

release_names is the private-name check of the changelog entries release_build rendered, over the
very file release_publish writes into the GitHub release's body, through
.github/workflows/release-notes-names.yaml, whose header says why it is a job of its own and what it
runs. It runs whenever release_build released, and checks when that release rendered entries.
release_publish needs release_names, and its release steps run only when it succeeded, so a finding
releases nothing; `latest` still moves, since the image carries no changelog.

## What release_publish does, and what it runs

release_publish moves `latest` and, when release_build decided a release, publishes it: confirms the
version against the tags inside the concurrency group, checks what build and release_build handed
over, writes SHA256SUMS, tags the commit, attests the host binaries and creates the GitHub release.
When another run took the version first, it publishes nothing and sees to it that a run releases
this commit ("Confirm the version"). It runs no Go, no bun and no built binary, only
release-push.sh, git, gh, jq, sha256sum, `docker buildx imagetools` and the pinned actions of its
steps, so the scopes and the deploy key it holds never meet the code the build jobs run.

## What release_publish needs, and when it runs

docker and image_index publish the immutable `envoy:<sha>` index in parallel with the release jobs,
outside release_publish's group; release_publish serializes only moving tags and releases. Only main
moves `latest` or cuts a `legion-envoy-v*` tag and release: a workflow_dispatch from any other
branch publishes its `envoy:<sha>` image and stops there. `latest` moves whatever release_build did;
the release steps run only on its success and release_names', and once "Confirm the version" finds
no other run took the version. release_publish needs targets too: it holds contents: write, the
deploy key and the rest, so it runs no script to derive the image it tags and reads back, and takes
it from targets' output, which `needs` carries only to a job naming targets directly.

## Why release_publish queues every run

release_publish runs one at a time, and every run's turn comes: `queue: max` keeps up to 100 jobs
pending in the group, first in, first out, where the default keeps one and cancels it for the next
(docs.github.com, "Control the concurrency of workflows and jobs"). A job joins the queue when the
jobs it needs end, not in commit order, so with the default an older commit's job could cancel a
newer one's, leaving that commit in no release and its run merely cancelled; "Confirm the version"
also leaves a commit to a later run, which must then come to its turn. Its "Determine latest" step
walks forward through already-published commits for `latest`, and release_build bumps the version
from the last tag to HEAD.

## What each scope of release_publish is for

In release_publish, packages: write moves the `latest` tag to the newest already-published image
(`docker buildx imagetools create`, in Advance latest image); contents: write creates and uploads
the GitHub release (`release-push.sh release`); id-token and attestations: write are for the
agent-secrets provenance attestation; actions: write lists this workflow's runs and starts a new one
when another run took the version (`release-push.sh restart`). The git tag push uses the deploy key,
held only by the "Tag release" step's env, not any of these scopes.

## How Confirm the version decides, and what a lost race does

release_build decided the version outside release_publish's concurrency group, from the tags as they
were then. Inside it, with the tags "Determine latest" fetched, the same computation must give the
same version (or the tag an earlier attempt pushed for this commit), and only then do the release
steps after it run (`release`). When another run took the tag first, release-push.sh version
--expect says so in a notice and exits 3 when nothing of this commit is left to release, or 4 when
this commit is in no release yet. Either way release_publish publishes nothing, and on 4
release-push.sh restart sees to it that a run releases the commit: a later run on main that holds it
and has not finished, or a new run it starts on main by workflow_dispatch, which decides the version
again from the tags as they are then. A workflow_dispatch made with release_publish's GITHUB_TOKEN
creates a run, unlike most events that token causes (docs.github.com, "Triggering a workflow from a
workflow"), and on main that run's release_build and release_publish run as a push run's do;
actions: write is what lets the token make it. Each loss means another run published a release
between this run's release_build and its release_publish, so a chain of started runs ends once
releases stop landing; the bound only ends a chain a fault keeps alive: a started run gets
lost_races + 1, and the one that loses with lost_races 3 fails instead of starting a fourth, naming
the command a person runs. A commit release_publish leaves to another run loses an image and a
version number of its own, never its change:
.github/ci-notes/release-envoy-listener.md#why-only-the-newest-waiting-push-builds-and-why-a-dispatch-is-never-replaced
says why, and is the one copy of that reasoning. The run release-push.sh restart starts is a
workflow_dispatch, a concurrency group of its own, which no newer push replaces while it waits.
$LEGION_ENVOY_VERSION_ARGS is expanded unquoted on purpose, as in release_build.

## What the hand-over check names and writes

build and release_build choose the archives' bytes, and hand over only them and the version.
release_publish names the archives itself, from that version and its own checkout of the target
lists; release-push.sh handed refuses a version that is not X.Y.Z and an artifact that is not
exactly those archives, each a regular file, and prints the tag. SHA256SUMS is then written over
them, and the tag and the explicit lists go to the later steps: the host binaries' archives are the
attestation's subjects, every archive and SHA256SUMS the release's files.

## Why release_publish dispatches the Dispatch builds of ci-caches.yaml

The release just tagged is the previous build every pull request and merge group from now on
resolves for ci.yaml's dispatch-two-builds job, and that job only restores it: the fill of this push
in .github/workflows/ci-caches.yaml ran before the tag existed and built the release before. So the
step asks ci-caches.yaml on main to fill the Dispatch builds alone (`only: dispatch-builds`). A
workflow_dispatch made with release_publish's GITHUB_TOKEN creates a run, and actions: write lets
the token make it. Plain gh, as release_publish runs no script but release-push.sh
(check-release-credentials.sh rule 1). A refused dispatch costs builds, not the release, so it warns
and the job goes on.

## What the host binaries' attestation proves, and how to verify it

Build provenance for the host binaries a mise pin installs, linux or darwin, which the release step
uploads exactly. `--repo` alone accepts an archive any workflow of this repository attested, so the
check pins the signer as docs/kubernetes.md ("Pin by digest, never by tag") does:

```sh
gh attestation verify agent-secrets-<asset>.tar.gz --repo cybermonklabs/legion \
  --cert-identity https://github.com/cybermonklabs/legion/.github/workflows/release-envoy-listener.yaml@refs/heads/main \
  --source-ref refs/heads/main --deny-self-hosted-runners
```

and adds `--source-digest <the legion-envoy-v<version> tag's commit>` to pin the commit as well. The
attest step of release_publish runs after "Tag release", so a run whose tag step refuses (the tag
already names another commit) leaves no attestation for archives no release carries.

## How the changelog is written into the release body

release_build's entries become the release body's one "## Changelog" section, below the fixed
sentence the release step writes. release-section replaces a section that heading already holds, and
says there why the write is a replace and not an append. The artifact holds the one file fold-notes
wrote, under the name the build job gave it.
