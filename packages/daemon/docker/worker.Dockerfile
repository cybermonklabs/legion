# Legion worker image: every Legion agent process under `runtime: kubernetes` runs from this image.
# Build context: the repo root. Built by .github/workflows/worker-image.yaml on the GitHub-hosted runner
# (called from release.yaml beside its publish job, build only, then attested by a second call after
# publish; from ci.yaml's worker-image job on every head of a pull request that touches a file it
# builds from; or dispatched post-merge). Never build it on a workstation — no `docker build`,
# `docker buildx`, or `docker compose build` (Sami, 2026-09-12); the CI runner is not a workstation.
#
# Contents: pinned Bun; the Go `legion` (packages/daemon) compiled from this checkout at go.work's
# Go version, static, at /opt/legion/bin/legion (one binary: worker-shim, launcher, workspace-init,
# gh, git-credential, probe-image and the daemon's own commands), the first PATH entry and the
# ENTRYPOINT name, with the `agent-secrets` client and the `gh` that runs `legion gh` beside it; the
# pinned OMP fork build, the linux-x64 archive of the release .omp-pin names, at /opt/omp, laid out
# as mise's github backend lays out a tmux host's install of that pin (bin/omp; the tools stage);
# @sjawhar/pi-envoy and @sjawhar/pi-legion packed from this checkout's packages/pi-envoy and
# packages/pi-legion at /opt/legion/pi-envoy and /opt/legion/pi-legion, the two roots a pod names as
# Oh My Pi's explicit extensions with discovery on, linked into no profile (a plugin both linked and
# explicit loads twice; the pod lane's probe refuses an image whose pi-envoy does), and
# @bopstack/pi-codegraph (installed at /opt/omp-plugins from the committed lockfile at
# packages/daemon/docker/omp-plugins), the one plugin linked into the isolated OMP profile
# `legion`, where discovery loads it — backed by the CodeGraph CLI (@colbymchenry/codegraph,
# installed from the committed lockfile at packages/daemon/docker/codegraph) at
# /opt/codegraph/bin; @sjawhar/claude-envoy built from this checkout's packages/claude-envoy at
# /opt/legion/claude-envoy, the plugin root a phase worker on the `claude` backend loads (its
# dist/envoy-channel.js is that worker's one MCP server, its legion/ the generated Legion plugin
# Claude Code loads beside it), which the plugin stage builds from source; the pin of Claude Code
# and nothing else of it — packages/daemon/docker/claude-code's
# package.json and package-lock.json, which pin @anthropic-ai/claude-code exactly with the npm
# integrity of every tarball, at /opt/legion/claude-code. The image carries no Claude Code: its
# license reserves every right and grants no redistribution, and this image is public, so a pod
# whose role runs on the `claude` backend installs that pinned release from npm at every launch,
# verified against the lockfile's integrity and the pinned version, and fails the launch if it
# cannot (claudeCodeBlock, packages/daemon/internal/runtime/sandbox/plugins.go) — the pin is in
# every image, whatever a deployment's worker_backends names; jj; mise at /usr/local/bin/mise, which
# resolves the Oh My Pi pin for the scripts a role runs that name it (the runtime stage says which);
# git at /usr/bin/git (>= 2.42, from the debian:trixie-slim runtime base — jj's
# git backend requires it); gh; trixie's socat, which carries a connection over `kubectl exec` to a
# loopback port inside the pod, trixie's unzip, which model-gateway/apply_test.sh of
# example-org/deployment calls, trixie's busybox, which that repository's broker tests run their
# scripts under, trixie's tmux, which the skill-scenario rig's panes and the tmux runtime's tests
# need, and WebKit's host libraries, the Debian 13 list Playwright names for its WebKit, so a role
# runs the Dispatch browser suite's WebKit projects (the runtime-base stage says why for each);
# what the capability check
# (packages/daemon/internal/capabilities, which `legion probe-image` runs) requires of the image: the Go
# toolchain at go.work's version (go and gofmt, /opt/go) with trixie's gcc and libc6-dev, the C
# compiler and headers Go's cgo and race detector need (cgo stays off by default), gopls,
# typescript-language-server with TypeScript's tsc and tsserver, pyright with its pyright-langserver,
# and trixie's python3 with PyYAML (python3-yaml), curl, wget, jq, openssh-client (ssh-keygen),
# util-linux (flock), postgresql-client-18 (psql), redis-tools (redis-cli), sqlite3,
# netcat-openbsd (nc), bind9-dnsutils (dig) and chromium (the browser Oh My Pi finds on PATH);
# the PostgreSQL 18 server with pgvector and /usr/local/bin/legion-testpg, the pod's own test
# database on a pod-lifetime emptyDir (the runtime-base stage says why);
# and a generic toolchain for the
# repositories the workers work, specific to none of them: uv and uvx, Node LTS with npm and
# corepack's pnpm and yarn, the AWS CLI v2, the Google Cloud SDK at /opt/google-cloud-sdk with its
# gcloud and bq, azure-cli at /opt/azure-cli with its az, kubectl and mikefarah's yq —
# each of these on PATH at /usr/local/bin or /usr/bin; the warm Go module cache of the two modules
# go.work binds, at /home/legion/go/pkg/mod/cache/download, which /opt/go/go.env names first in
# GOPROXY, and which carries no version list, so a version query falls through to the public
# proxy (the go-modules stage and the toolchain stage say why);
# USER=legion, the runtime user's name, in the image's environment (the runtime stage's ENV says
# who reads it);
# and the license of every third-party piece of
# all that at /usr/share/doc/legion/THIRD_PARTY_NOTICES (the notices stage; its Debian list comes from
# the image's own dpkg database).
# Four RUNs of the runtime stage gate the publish, each as the runtime user. The first checks every
# binary runs on the base, proves jj accepts the image's git with a network-free `jj git clone` of
# a scratch repository, and links the CodeGraph plugin into the profile, which extracts Oh My Pi's
# natives from its binary; the second runs every toolchain command, the language servers', Python's
# and the browser's included; the third prints and checks the marker of the webkit-check stage,
# which, as the runtime user, downloads the WebKit build of the Playwright packages/dispatch locks
# into a scratch directory and launches it once, headless, through Playwright's own host
# validation, so a Playwright whose WebKit needs a library the image lacks fails the build. That
# download is checked against no hash, so it runs in a stage of its own and only its one-line
# marker reaches the runtime stage, whose bind mount of it is what makes BuildKit build that
# stage. The last
# runs `legion version` and `legion probe-image`, which runs the three launch probes (the daemon's two
# plus the session-storage probe), holds the Legion plugin to the daemon API contract and proves it
# loads with the Envoy plugin, holds the claude-envoy root the same way — its built channel server,
# its generated Legion plugin and the daemon API contract its manifest declares — and holds Claude
# Code's pin to one a pod can install and verify, checks every image-site capability against this
# image, and prints the results before the OK line the daemon's probe Sandbox reads.
# A broken image never publishes.
#
# The `legion` profile carries no model route, and neither does Legion: an operator's pod supplies it
# (runtime.kubernetes.pod, docs/kubernetes.md). In a pod, the Go `legion` starts Oh My Pi on Legion's
# pod baseline (packages/daemon/internal/podsafety: the worker shim, and `legion probe-image`, each
# with --pod-safety), which names no model, provider or route.

# Pins not derived from daemon code. The OMP fork pin is deliberately NOT an ARG: it is the one line of
# the repository's .omp-pin, its only home, which the tools stage copies; ARG OMP_SHA256 below is the
# digest of that release's archive, and it moves with the pin.
ARG BUN_VERSION=1.4.4
ARG MISE_VERSION=v2026.8.12
# SHA-256 of the tools stage's linux-x64 archive (the image builds linux/amd64 only; `install-jj`'s
# mise step reads this pair from here too). Take the hash from the output of
# `gpg --decrypt SHASUMS256.asc` — never by grepping the `.asc` file itself: a forged line added
# outside its signed block does not stop `gpg --verify` from reporting a good signature, but
# `gpg --decrypt`'s stdout is exactly the signed text, nothing else. Take the hash only once that
# command reports a good signature from release key 24853EC9F655CE80B48E6C3A8B81C9D17413A06D
# (published at https://mise.jdx.dev/gpg-key.pub, and at keys.openpgp.org by the same fingerprint).
ARG MISE_SHA256=0c782233b97745fd3ed317ba3acbfd7d256e6268470373757f0cc48d57bb87e6
# The jj fork sjawhar/jj, at a release built from sjawhar/jj#4 on top of sjawhar/jj#3. sjawhar/jj#3
# gives jj 0.46 a gix-pack that maps each pack, index and multi-pack-index in 8 MiB chunks and keeps
# the chunks reads mark within 512 MiB (JJ_PACK_MAPPED_LIMIT sets another budget): under gVisor a
# whole mapping counts as the pod's memory once one page of it is read. sjawhar/jj#4 starts from
# jj-vcs/jj#10100 and keeps, in a merge of two commits with several merge bases, what both sides
# hold (jj-vcs/jj#6369). A later pin must carry both patches, or one jj command on a large pack
# fills an issue pod again, or a role's merge silently reverts work.
ARG JJ_TOOL=github:sjawhar/jj@0.46.0-sami.20261125-112159
ARG GH_TOOL=gh@2.98.0
# github/gh-stack, the `gh stack` extension (GitHub's stacked pull requests), at the release this
# pin names, as a `github:<owner>/<repo>@<version>` pin the tools stage reads as it reads .omp-pin.
# The image installs it at /opt/legion/gh-extensions/gh-stack, and the shim links it into gh's
# extension directory under each role's data home (podsafety.EnsureGhExtensions), where gh reads
# extensions and nowhere else; ARG GH_STACK_SHA256 below holds its binary's digest.
ARG GH_STACK_TOOL=github:github/gh-stack@0.3.0
# The SHA-256 of the release archive the tools stage fetches for each of its three tools, which it
# checks before it unpacks anything, as the toolchain stage does its downloads: the linux-x64
# archive of the Oh My Pi release .omp-pin names (omp-v<version>-linux-x64.tar.gz), the x86_64
# musl archive of the jj release ARG JJ_TOOL names (jj-v<version>-x86_64-unknown-linux-musl.tar.gz,
# whose jj binary is the one the `jj` row of .github/ci-tools.yaml pins), and the linux amd64
# archive of the gh release ARG GH_TOOL names (gh_<version>_linux_amd64.tar.gz of cli/cli). A pin
# and its digest move in one commit: a bump of .omp-pin, ARG JJ_TOOL or ARG GH_TOOL that leaves its
# digest fails the tools stage's sha256sum check in that pull request's own worker-image build.
# Oh My Pi's was read on 2026-11-25 at its 18.18.6-sami.20261125-164517 pin, jj's on 2026-11-25 at
# its 0.46.0-sami.20261125-112159 pin and gh's on 2026-11-22 at 2.98.0, each as the `digest`
# GitHub's API gives the release asset (`gh api repos/<owner>/<repo>/releases/tags/<tag> --jq
# '.assets[]'`), and each equals the sha256 of the archive downloaded from the address the tools
# stage reads. gh's is also its line in the release's gh_<version>_checksums.txt, and `gh
# attestation verify <archive> --repo cli/cli` verified that archive against cli/cli's
# deployment.yml workflow. Oh My Pi's archive verifies the same way against the fork's
# sami-build.yml workflow at refs/heads/release-candidate (`gh attestation verify <archive> --repo
# cybermonklabs/oh-my-pi`); the jj release carries no attestation (the command answers 404). A
# bump repeats those reads.
ARG OMP_SHA256=359f95dec797f1a507accbd2f3b56b3ef9b5dfc5fb5cd4c8652a326175ad6c85
ARG JJ_SHA256=2e47fd34fc59a50c0db0a231db018dcf672e9640b494bdc57fe01d6b9560167e
ARG GH_SHA256=3b8ac6b30336802fc1a858d7c084e11cdf24ac1a761ca90b68022d7d729208de
# The SHA-256 of the linux-amd64 binary of the gh-stack release ARG GH_STACK_TOOL names (the release
# ships bare binaries, `linux-amd64`, no archive). Read on 2026-11-22 as the `digest` GitHub's API
# gives that release asset, equal to the sha256 of the binary downloaded from the address the tools
# stage reads, and `gh attestation verify linux-amd64 --repo github/gh-stack` verified that binary
# against github/gh-stack's release.yml workflow at refs/tags/v0.3.0. A bump repeats those reads
# and moves this digest in the same commit, or the tools stage's check fails the build.
ARG GH_STACK_SHA256=957e822cfa7e435cd84a297cd96c419b58e51e3ac58be249762c24bd9e45099b
# The pin of the shared public leak check: the tag of the repository a deployment names in its
# leak check setting (legion.yaml's leak_check, or LEGION_LEAK_CHECK_REPOSITORY), whose leak-check
# directory holds it. The image carries the pin alone. The pod's launcher names this tag to the
# daemon, which, under that setting, reads the check at it and pushes it to the launcher, and the
# launcher installs it under the issue's scratch before any role's agent starts
# (packages/daemon/internal/leakguard's
# Source and InstallServed). R5 of dispatch://INFRA-1527 requires the newest
# `leak-check-v<n>` tag at the time the pin is written, never a number a spec fixed: this is the
# newest, read with
# `gh api --paginate repos/example-org/deployment/git/matching-refs/tags/leak-check-v` at
# 2026-11-22T16:38:25Z, and a bump reads that list again. The runtime stage exports it as
# LEGION_LEAK_CHECK_TAG, which
# is where the launcher and each role's shim read it. Go holds the same tag once more:
# ghReadsProvenCheck in packages/daemon/cmd/legion/gh_reads.go, the check `legion gh`'s list of
# reads was proven against, and TestGhReadsAreProvenAgainstThePinnedCheck holds the two equal, so
# a bump moves both, by the steps ghReadsProvenCheck's comment gives.
ARG LEAK_CHECK_TAG=leak-check-v33
# go.work's `go` line: the Go stage builds in workspace mode, and the golang image's GOTOOLCHAIN=local
# fails the build if go.work moves past this. The toolchain stage ships the same release to the workers
# (/opt/go), checked against GO_SHA256: the SHA-256 https://go.dev/dl/ publishes for
# go${GO_VERSION}.linux-amd64.tar.gz (its ?mode=json&include=all listing), which a version bump reads
# from there again.
ARG GO_VERSION=1.27.3
ARG GO_SHA256=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
# gopls, which the gopls stage builds static from golang.org/x/tools/gopls at this tag (gopls/v<version>).
ARG GOPLS_VERSION=0.23.0
# The toolchain stage's pins: each is a release version and the SHA-256 of the linux/amd64 download the
# stage fetches for it (an archive, or a single binary as kubectl's and yq's are), which the build
# checks before unpacking or installing anything.
ARG UV_VERSION=0.13.5
ARG UV_SHA256=f68d291c75783122dad58ab77def0e13f3233f358731db5d6d7e8715603b928f
# Node 24 LTS.
ARG NODE_VERSION=24.22.1
ARG NODE_SHA256=6c172a9ab6718f7b801fb5a7bd486af2ee5e32758086a04b84fcce2aa77095e6
# Chromium for Testing is pinned separately from Debian's chromium package because it carries the
# Chrome Root Store revision that distrusts the compromised emSign root. Its SHA-256 is for
# chrome-linux64.zip from Chrome for Testing's public release archive.
ARG CHROMIUM_VERSION=158.0.8117.2
ARG CHROMIUM_SHA256=80d11972d4c483716685fed17ca95cf69eb9c849b1146c0da55c07a58617f627
ARG AWS_CLI_VERSION=2.38.4
# AWS publishes no SHA-256 for the CLI, only a PGP signature. This pin is the SHA-256 of the zip whose
# `.sig` gpg verified as a good signature from the AWS CLI Team key
# FB5DB77FD5C118B80511ADA8A6310ACC4672475C. A version bump repeats that check before taking its hash.
ARG AWS_CLI_SHA256=10ab32a95b27138d7cc1c1ac4ebb4d3acab60cd0a0df2fc37c4e2c440ed40bce
# kubectl, pinned to the minor version of the API server of the production cluster the Legion workers
# run on (v1.35.7 when this pin was set): Kubernetes' version skew policy supports kubectl one minor
# version either side of its server, so a bump follows that server's minor. A deployment whose cluster
# runs another minor layers its own kubectl on this image (docs/kubernetes.md, "Per-deployment
# toolchains layer on top"). The pin is the SHA-256 Kubernetes publishes beside the binary
# (https://dl.k8s.io/release/v<version>/bin/linux/amd64/kubectl.sha256), taken only once the binary's
# keyless Sigstore signature (kubectl.sig and kubectl.cert, beside it) verifies for the release identity
# krel-staging@k8s-releng-prod.iam.gserviceaccount.com issued by https://accounts.google.com. A version
# bump repeats both checks.
ARG KUBECTL_VERSION=1.35.9
ARG KUBECTL_SHA256=3cfeaf80be482b435b0aa214aff6e0b2c312ee23c0ff20810c75517b6004c6eb
# mikefarah's yq, the YAML processor: model-gateway/apply_test.sh of example-org/deployment, and
# the `apply` program it tests, call the bare name `yq` on PATH. A package runner does reach yq —
# `go run github.com/mikefarah/yq/v4@v4.54.1` works with this image's Go — but it could supply that
# bare name only through a file called `yq` on the role's own PATH, which
# skills/legion-worker/references/tools.md forbids, and `go run` costs seconds per call: one passing
# run of that test calls yq 228 times (counted 2026-11-06 at deployment main ec59a17a; the count
# moves with that repository). Debian's `yq` is a different program — kislyuk's Python
# wrapper over jq, 3.4.3 in trixie, whose command language is another one — so this is a pinned
# fetch, and an apt package must never take its place. The pin is the SHA-256 of yq_linux_amd64 as
# the release's own signed `checksums` file gives it: field 19 of that file's yq_linux_amd64 row,
# because line 18 of the `checksums_hashes_order` published beside it reads SHA-256. That file was
# taken only once its signature verified. The command, run on 2026-11-05, was
# `go run github.com/sigstore/cosign/v2/cmd/cosign@v2.6.0 verify-blob --bundle checksums.bundle
# --certificate-identity 'https://github.com/mikefarah/yq/.github/workflows/release.yml@refs/tags/v4.54.1'
# --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums`, and it printed
# `Verified OK`; that identity is a release workflow of that repository at the v4.54.1 tag. A
# version bump repeats that check.
ARG YQ_VERSION=4.54.1
ARG YQ_SHA256=8e34fc298390875de416e6a4afcb8cabeceb25d9aa8506c1a2f9353cf702ea5f
# The Google Cloud SDK, whose gcloud and bq the toolchain stage puts on PATH for every deployment
# (gsutil stays in the archive and is not linked): the versioned release archive
# google-cloud-cli-${GCLOUD_VERSION}-linux-x86_64.tar.gz of dl.google.com's rapid channel. 594.0.0
# is the newest release on 2026-11-18, read from that channel's components-2.json. Google publishes
# no SHA-256 for a versioned archive and no signature for any archive: its install page
# (https://cloud.google.com/sdk/docs/install) and its versioned-archives page
# (https://cloud.google.com/sdk/docs/downloads-versioned-archives) give a SHA-256 only for each
# platform's unversioned latest archive. So this pin is the SHA-256 of the versioned archive as it
# was downloaded on 2026-11-18. That day the unversioned latest archive was 594.0.0 too, matched the
# SHA-256 the page published for it (8cf5b2d7987e7aac008264fecda7c083423227ecfb26878707bd20c6219d7dab),
# and unpacked to the same files as the versioned one. A version bump repeats that comparison before
# it takes a hash.
ARG GCLOUD_VERSION=594.0.0
ARG GCLOUD_SHA256=1f7233537c26bba94a3ea3f9085a60396745eaf44b21b8454fc6fdd15bb6fcde
# The CPython uv installs for azure-cli in the toolchain stage: the newest 3.13 release that uv
# 0.13.5 (ARG UV_VERSION) offers, read with `uv python list --all-versions --only-downloads` on
# 2026-11-18. uv checks the CPython it downloads against the SHA-256 it carries for that release.
# azure-cli's own version is no ARG: it lives in packages/daemon/docker/azure-cli/requirements.in,
# its one home, and in the hashed lock beside it, requirements.txt.
ARG AZURE_CLI_PYTHON_VERSION=3.13.17
# The language servers Oh My Pi's LSP tools start for TypeScript and Python install from the
# committed lockfile at packages/daemon/docker/language-servers (COPY'd into the toolchain stage),
# with scripts off (`npm ci --ignore-scripts`): a reproducible install off that lockfile, no ARG
# pin to keep in step with it. The pin rule itself — TypeScript is the repository's own major
# because typescript-language-server wraps tsserver, which TypeScript 7 no longer ships (its npm
# package's bin is tsc alone) — lives in that package.json's description.
# apt packages carry no version pin (hadolint DL3008, ignored at each `apt-get install`): Debian's
# archive serves only a suite's current version of a package, so a pinned version stops resolving at
# the suite's next update. The base image's suite is the pin.
# The PostgreSQL server and pgvector come from apt.postgresql.org, not trixie: trixie's own archive
# stops at PostgreSQL 17, and the pod's cluster has to be the major the deployment's databases and CI
# run, 18 (packages/daemon/internal/testpg/testdb/testdb.go). That suite is no pin either, and a version pin
# is no answer there: read on 2026-10-30, the trixie-pgdg index carries the last three point releases
# of a major and no more (postgresql-18 at 18.6, 18.4 and 18.3; postgresql-17 at 17.11, 17.10 and
# 17.9, with 17.0 through 17.8 gone), so `postgresql-18=18.6-1.pgdg13+2` would stop resolving at the
# next point release. What is pinned is the repository's signing key: the runtime-base RUN fetches its
# armored export from https://www.postgresql.org/media/keys/ACCC4CF8.asc — RSA-4096 key
# B97B 0AFC AA1A 47F0 44F2 44A0 7FCC 7D46 ACCC 4CF8, uid "PostgreSQL Debian Repository" — and checks
# it against this SHA-256 before apt reads one index signed by it, so a rotated or re-exported key
# fails the build and gets read by a person instead of being trusted.
ARG PGDG_KEY_SHA256=0144068502a1eddd2a0280ede10ef607d1ec592ce819940991203941564e8e76
# The CodeGraph CLI and the Oh My Pi tool that wraps it are pinned to the versions the devbox runs
# (`codegraph --version`, `omp/plugins/package.json`). Each one installs from a committed lockfile
# in the plugin-deps stage: the tool, @bopstack/pi-codegraph, from packages/daemon/docker/omp-plugins,
# and the CLI, @colbymchenry/codegraph, from packages/daemon/docker/codegraph. Each package.json
# pins its one package, and each bun.lock pins everything Bun installs beside it, so, as for the
# language servers, there is no ARG pin to keep in step with them. An ARG cannot be that one place
# for the CLI: a lockfile is committed, so it names the version too, and the ARG would be a second
# name to keep in step. The notices stage reads the CLI's version out of that package.json, the way
# it reads the language servers' versions out of theirs.

# ------------------------------------------------------------------------------------------------
# bun: the pinned Bun image alone. The two plugin stages build on it, and the runtime stage copies
# its bun from here rather than from a plugin stage, so that layer reads the Bun pin alone: no
# change to a lockfile or to the plugins' source reruns it, and BuildKit runs no plugin stage to
# find that out.
# ghcr.io/cybermonklabs/legion/ci, the copy of oven/bun:1.4.4-slim (.github/ci-images.yaml)
FROM ghcr.io/cybermonklabs/legion/ci@sha256:1846adf22496bd711036aae8be147e57bdebbaea2e5264e3e0b5af1db187d9a4 AS bun

# ------------------------------------------------------------------------------------------------
# plugin-deps: the workspace install, the playwright-core packages/dispatch locks, the Oh My Pi
# plugins' frozen install and the CodeGraph CLI's frozen install: everything of the plugins' build
# that reads manifests and lockfiles and no application source. The runtime stage copies the
# CodeGraph CLI and the Oh My Pi plugins from here, and the webkit-check stage binds playwright-core
# from here, so a change to the plugins' source or to skills/ reruns none of those layers. BuildKit
# keys a layer copied from another stage on that stage's own steps first, and reads the copied files
# only after it has run that whole stage; from the plugin stage, which copies the source, every
# such layer waited for the workspace install and the packs before BuildKit could reuse it.
FROM bun AS plugin-deps
WORKDIR /repo
# jq: the same version, contract number and omp.extensions rewrites release.yaml's pi_envoy_build and pi_legion_build jobs run.
# Both fetches of this RUN retry: five attempts, the fifth starting 150 s after the first, each
# attempt under a bound of its own. 120 s bounds an apt index read everywhere in this file, a few
# megabytes from the Debian mirror, and a package set gets 300 s — except runtime-base's set,
# whose own RUN says why it gets 600 s. `rm -rf` fetches nothing and stays bare.
# Each `apt-get update` in this file also carries `-o APT::Update::Error-Mode=any`, which makes a
# failed index read fail the command instead of warning and exiting 0: without it the retry has
# nothing to retry. .github/scripts/check-download-retry.sh's rule 5 is the whole reason.
# hadolint ignore=DL3008
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
    && retry 120 "apt-get update" apt-get update -o APT::Update::Error-Mode=any \
    && retry 300 "apt-get install" apt-get install -y --no-install-recommends jq \
    && rm -rf /var/lib/apt/lists/*
COPY package.json bun.lock ./
COPY patches patches
COPY packages/contracts/package.json packages/contracts/package.json
COPY packages/cipher-client/package.json packages/cipher-client/package.json
COPY packages/envoy-client/package.json packages/envoy-client/package.json
COPY packages/pi-envoy/package.json packages/pi-envoy/package.json
COPY packages/pi-legion/package.json packages/pi-legion/package.json
COPY packages/pi-shared/package.json packages/pi-shared/package.json
COPY packages/claude-envoy/package.json packages/claude-envoy/package.json
COPY packages/proof-editor/package.json packages/proof-editor/package.json
# proof-sdk-upstream is file:./proof-sdk-<commit>.tgz, a dependency of that manifest rather than a
# registry fetch: the upstream editor sources, which the install below reads from the context.
# The glob, not the file name the pinned commit is in, so raising that pin edits nothing here.
COPY packages/proof-editor/proof-sdk-*.tgz packages/proof-editor/
# What the glob costs, and the line that pays it. BuildKit builds every COPY source with
# AllowWildcard and AllowEmptyWildcard (buildkit v0.26.1
# frontend/dockerfile/dockerfile2llb/convert.go's dispatchCopy), and a wildcard matching nothing
# copies nothing and returns no error (solver/llbsolver/file/backend.go's docopy), where a COPY of
# an exact name the context does not hold fails at the cache key with `not found`. So the context
# is held to exactly one tarball here: without this the build would reach `bun install` and spend
# five attempts and 150 s of sleeps on a `file:` dependency no attempt can resolve. The tree's own
# count is tests/one-upstream-tarball.test.ts; this is the context's, which a .dockerignore can
# break where the tree is right. An unmatched pattern is left in `$1` by the shell, so it is
# dropped before the count is taken and a context with no tarball is reported as the none it is.
RUN set -- packages/proof-editor/proof-sdk-*.tgz; [ -e "$1" ] || set --; \
    { [ "$#" -eq 1 ] && [ -f "$1" ]; } \
    || { echo "the build context holds $# entries matching packages/proof-editor/proof-sdk-*.tgz, want one regular file" >&2; exit 1; }
COPY packages/dispatch/package.json packages/dispatch/package.json
COPY packages/test-fixtures/package.json packages/test-fixtures/package.json
COPY packages/envoy/internal/dispatch/pmdoc/gen/package.json packages/envoy/internal/dispatch/pmdoc/gen/package.json
COPY docs/site/package.json docs/site/package.json
# --ignore-scripts skips the lifecycle script of every package installed here, so the install
# fetches what bun.lock pins and nothing else. better-sqlite3 is the reason for it:
# packages/proof-editor reaches that package through proof-sdk, and under Bun its install script
# ends in `bunx node-gyp@latest` compiling it from source (oven-sh/bun#4290). That fetches
# node-gyp's newest release from npm and then the Node headers from nodejs.org, two downloads no pin
# or digest holds, so the image build passed or failed on them: worker-image run 44089746339 failed
# there once, and passed on its next attempt. Nothing of that build reaches the image, since no
# later stage copies /repo/node_modules, and nothing in this stage runs esbuild, the one other
# package here with an install script (Bun's bundler builds the plugins). Bun still applies the
# patches the root package.json names. packages/envoy/docker/Dockerfile installs the same
# workspace with the same flag.
# The workspace install is the largest fetch in this file: every dependency the root lockfile pins,
# tens of megabytes of tarballs from the npm registry, so an attempt gets 600 s. The two frozen
# installs below are a handful of packages each and get 300 s.
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
    && retry 600 "bun install" bun install --frozen-lockfile --ignore-scripts
# The playwright-core of the Playwright packages/dispatch locks, copied to /out/playwright-core:
# the webkit-check stage launches this Playwright's WebKit. The install above uses Bun's isolated
# linker, so playwright-core is not hoisted at /repo/node_modules, and
# packages/daemon/docker/resolve-playwright-core.mjs finds it from packages/dispatch with Node's
# module resolution (its header says how), so the check follows the version that package locks.
# The RUN prints that version and the directory it found, and fetches nothing.
RUN --mount=type=bind,source=packages/daemon/docker/resolve-playwright-core.mjs,target=/tmp/resolve-playwright-core.mjs \
    set -eu; \
    core="$(bun /tmp/resolve-playwright-core.mjs /repo/packages/dispatch/package.json)"; \
    mkdir -p /out; cp -a "$core" /out/playwright-core; \
    echo "playwright-core: $(jq -r .version /out/playwright-core/package.json) from $core"
# The Oh My Pi plugins the runtime stage links into the `legion` profile (its step 3), installed with
# `bun install --frozen-lockfile` from the committed lockfile at packages/daemon/docker/omp-plugins, a
# project of its own (no root workspace), into its own prefix, /opt/omp-plugins, which the runtime
# stage copies whole. The tree is the lockfile's whatever the registry has published since, and a
# package.json the lockfile does not match fails the install instead of resolving afresh; lifecycle
# scripts stay off, as for the language servers. It follows the workspace install because every
# frozen Bun install in this file comes after the root workspace manifests
# (.github/scripts/check-bun-workspace-manifests.sh).
WORKDIR /opt/omp-plugins
COPY packages/daemon/docker/omp-plugins/package.json packages/daemon/docker/omp-plugins/bun.lock ./
# The install retries on the 300 s bound of a small frozen install (the workspace install above).
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
    && retry 300 "bun install (omp plugins)" bun install --frozen-lockfile --ignore-scripts
# CodeGraph CLI (@colbymchenry/codegraph), installed the same way, from the committed lockfile at
# packages/daemon/docker/codegraph, a project of its own. `bin.codegraph` in the main npm package
# is a thin `#!/usr/bin/env node` launcher shim that locates and execs the per-platform
# optionalDependency (`@colbymchenry/codegraph-linux-x64`) — but this stage's base image has no
# system `node` (only bun's own fallback shim, which the final runtime stage does not copy over),
# so the shim itself cannot start. The platform package's own `bin/codegraph` is a `#!/bin/sh`
# wrapper that execs a *bundled* Node 24 runtime sitting beside it — fully self-contained, no
# system node required anywhere — so this copies that platform package alone, as `/opt/codegraph`
# in the runtime stage. The lockfile pins that bundle too, which the unlocked global install this
# replaced resolved afresh at every build. The install is in plugin-deps, which copies no
# application source, so a source change to the plugins the plugin stage packs never invalidates
# its layer. The bundled Node's
# version goes to /out/codegraph-node-version, for the notices stage to fetch that release's
# LICENSE.
WORKDIR /opt/codegraph-cli
COPY packages/daemon/docker/codegraph/package.json packages/daemon/docker/codegraph/bun.lock ./
# The install retries on the same 300 s bound as the plugins' above; the copy out of node_modules
# and the two version reads touch no network and stay bare.
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
    && mkdir -p /out \
    && retry 300 "bun install (codegraph cli)" bun install --frozen-lockfile --ignore-scripts \
    && cp -a node_modules/@colbymchenry/codegraph-linux-x64 /out/codegraph \
    && /out/codegraph/bin/codegraph --version \
    && /out/codegraph/node --version > /out/codegraph-node-version

# ------------------------------------------------------------------------------------------------
# plugin: the two packed plugins and claude-envoy, built on plugin-deps from this checkout's source.
FROM plugin-deps AS plugin
WORKDIR /repo
COPY packages/contracts packages/contracts
COPY packages/cipher-client packages/cipher-client
COPY packages/envoy-client packages/envoy-client
COPY packages/pi-shared packages/pi-shared
COPY packages/pi-envoy packages/pi-envoy
COPY packages/pi-legion packages/pi-legion
COPY skills skills
# Each plugin's prepack (scripts/pi-plugin-prepack.sh) builds its one bundle, stages its skills from
# skills/ into dist/skills, and writes its dist/THIRD_PARTY_NOTICES with scripts/third-party-notices.ts.
COPY scripts/pi-plugin-prepack.sh scripts/pi-plugin-prepack.sh
COPY scripts/third-party-notices.ts scripts/third-party-notices.ts
# The contract number pi-legion's manifest declares, which its committed manifest does not:
# scripts/daemon-contract.sh counts it from packages/contracts' fixtures, as release.yaml's
# pi_legion_build does, and reads no history, which this context has not got (.dockerignore
# leaves out .git).
COPY scripts/daemon-contract.sh scripts/daemon-contract.sh

# Each plugin ships from this checkout with the steps release.yaml's pi_envoy_build and
# pi_legion_build jobs run before `bun pm pack`: the release version set (PI_ENVOY_VERSION and
# PI_LEGION_VERSION, the pair a release resolved for this commit and links into `legion`; empty
# outside a release, which keeps the manifest's own version), pi-legion's contract number written,
# and the manifest pointed at the one packed bundle (the prepack refuses to pack with the source
# manifest). So an image built from a commit carries that commit's plugin code, labelled with the
# version its release links into `legion`, never a registry's build. Each tarball is unpacked into
# a directory: `omp plugin install` links a directory and rejects a tarball path (ENOTDIR).
ARG PI_ENVOY_VERSION
WORKDIR /repo/packages/pi-envoy
RUN jq --arg v "$PI_ENVOY_VERSION" '(if $v != "" then .version = $v else . end) | .omp.extensions = ["dist/envoy.js"]' package.json > tmp.json \
    && mv tmp.json package.json \
    && rm -f ./*.tgz && bun pm pack \
    && mkdir -p /out/pi-envoy \
    && tar xzf ./*.tgz -C /out/pi-envoy --strip-components=1
WORKDIR /repo/packages/pi-legion
ARG PI_LEGION_VERSION
RUN contract="$(bash /repo/scripts/daemon-contract.sh)" \
    && jq --arg v "$PI_LEGION_VERSION" --argjson c "$contract" '(if $v != "" then .version = $v else . end) | .legion.daemonApiVersion = $c | .omp.extensions = ["dist/legion.js"]' package.json > tmp.json \
    && mv tmp.json package.json \
    && rm -f ./*.tgz && bun pm pack \
    && mkdir -p /out/pi-legion \
    && tar xzf ./*.tgz -C /out/pi-legion --strip-components=1

# claude-envoy is built from this checkout's source rather than installed from npm: the image
# runs the tree it was built from, as it does for the rest of the repository. Nothing here packs
# it, unlike the two plugins above, so the prepack that resolves its skills/ links for a pack
# never runs.
# That tree's dist/ and legion/ are no input here: nothing commits them, and the release that
# publishes the plugin builds them in its own build job.
# `scripts/build.ts --out` writes /out/claude-envoy from the source instead: the plugin's own files
# with its skills symlinks resolved, since the image carries no repository root for them to point
# into, and a fresh dist/ and legion/. It refuses any Bun but the one .bun-version pins, which the
# bun stage's image carries (ARG BUN_VERSION; .github/scripts/check-bun-version.sh holds the ARG,
# the pin and that base's row in .github/ci-images.yaml equal).
# The copy drops group and other write, which a checkout's umask may have left, since the runtime
# stage's final step refuses a plugin file anyone but root can write.
COPY .bun-version /repo/.bun-version
COPY packages/claude-envoy /repo/packages/claude-envoy
WORKDIR /repo/packages/claude-envoy
RUN set -eu; \
    bun scripts/build.ts --out /out/claude-envoy; \
    chmod -R go-w /out/claude-envoy; \
    test -f /out/claude-envoy/dist/envoy-channel.js; \
    test -f /out/claude-envoy/legion/.claude-plugin/plugin.json

# ------------------------------------------------------------------------------------------------
# tools: mise, and the pinned OMP fork build, jj and gh. mise is checked against MISE_SHA256 before
# anything extracts or runs it, the same shape the toolchain stage below uses for uv, Node and the AWS
# CLI; .github/actions/install-jj reads the same pair for the CI runner's own mise. The image carries
# this mise for the scripts a role runs (the runtime stage says which), and the build runs nothing of
# it past `mise --version`.
# ghcr.io/cybermonklabs/legion/ci, the copy of debian:bookworm-slim (.github/ci-images.yaml)
FROM ghcr.io/cybermonklabs/legion/ci@sha256:99c0b25c46d0214d5a2091092e7908907dcaecd6a962cde0ac700acee222b6f3 AS tools
ARG MISE_VERSION
ARG MISE_SHA256
ARG JJ_TOOL
ARG GH_TOOL
ARG GH_STACK_TOOL
# Both fetches retry on this file's apt bounds, 120 s for the index read and 300 s for the set,
# and the index read carries this file's error-mode option.
# hadolint ignore=DL3008
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
    && retry 120 "apt-get update" apt-get update -o APT::Update::Error-Mode=any \
    && retry 300 "apt-get install" apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
# The one fetch here is curl's, so it carries curl's own retry rather than the wrapper: 8 attempts
# whose waits double from a second, the last starting 127 s after the first, each transfer bounded
# at 120 s for a release archive of a few tens of megabytes, and -f so a persistent 504 fails the
# build instead of writing the error body into the file the checksum then reads. The checksum, the
# unpack and the install are local and stay bare. `mise --version` asks https://mise.jdx.dev/VERSION
# for a newer release unless its environment names CI: mise's show_latest (src/cli/version.rs at
# v2026.8.12) returns before that read when ci_info's is_ci() is true, and ci_info 0.14.15 reads any
# CI, CONTINUOUS_INTEGRATION, BUILD_NUMBER or RUN_ID variable as CI. So CI=true keeps the version
# print local and the RUN fetches nothing past the curl (dispatch://INFRA-1545, round 68).
RUN set -eu; \
    t=/tmp/mise; mkdir -p "$t"; \
    curl -fsSL --retry 7 --retry-all-errors --max-time 120 -o "$t/mise.tar.gz" \
      "https://github.com/jdx/mise/releases/download/${MISE_VERSION}/mise-${MISE_VERSION}-linux-x64.tar.gz"; \
    printf '%s  %s\n' "$MISE_SHA256" "$t/mise.tar.gz" > "$t/SHA256SUMS"; \
    sha256sum --check --strict "$t/SHA256SUMS"; \
    tar -xzf "$t/mise.tar.gz" -C "$t" --no-same-owner; \
    install -m 0755 "$t/mise/bin/mise" /usr/local/bin/mise; \
    CI=true mise --version; \
    rm -rf "$t"
COPY .omp-pin /omp-pin
# The Oh My Pi build, jj and gh are each one archive of the release its pin names, and gh-stack one
# binary of its release, fetched from the address GitHub serves a release asset at,
# https://github.com/<owner>/<repo>/releases/download/<tag>/<name>, and checked against its ARG
# *_SHA256 before it is unpacked or installed. That address makes no GitHub API call.
# Until 2026-11-21 mise resolved the three through the API (the release by its tag, cli/cli's release
# list, the artifact attestations) with the run's GITHUB_TOKEN, whose hourly budget of 15,000 calls
# every job of this repository's CI shares. That day the budget ran out before its hour reset at
# 17:24:15Z (mise: `github rate limit: 0/15000 (core)`), mise skipped the install it could not
# resolve, and `mise x <pin> -- omp --version` failed every pull request's build in that hour with
# `"omp" couldn't exec process: No such file or directory` (runs 44928923080, 44929049036 and
# 44929265240). The pins are `github:<owner>/<repo>@<version>` (.omp-pin, ARG JJ_TOOL, ARG
# GH_STACK_TOOL) and `gh@<version>` (ARG GH_TOOL), each release's tag is `v` and that version, as the notices stage
# reads them too. Each part of a pin — owner, repository, version — is one or more of
# [A-Za-z0-9._-] and holds no `..`, so no part can be a path of its own or climb out of the one
# it stands in; a pin of any other shape fails here by its name rather than fetch an address it
# does not name. /opt/omp is the Oh My Pi archive with its one top-level directory stripped, as mise strips
# it: bin/omp. Each fetch refuses by the tool's name and the address it read: a digest ARG that is
# no 64-hex sha256, a download curl did not complete (a 404, a truncated body), or an archive
# whose sha256 is not the pinned one, so the log says which of the four failed and why, though
# BuildKit marks every line of this RUN. The four digests are declared here, after the layers
# above, so a bump of one rebuilds this layer alone. gh-stack is installed as gh lays out an
# extension it installs, a directory gh-stack holding the executable gh-stack, and must print the
# version its pin names.
# Each download retries, as every fetch of this file does (go-base's `apk add` among them, and
# .github/scripts/check-download-retry.sh refuses a fetch by any of its verbs here that does not):
# five attempts of 300 s each, the bound the one curl carried before (dispatch://INFRA-1545).
# gh-stack's binary goes through the same `fetch` and the same bound.
ARG OMP_SHA256
ARG JJ_SHA256
ARG GH_SHA256
ARG GH_STACK_SHA256
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; }; \
    set -eu; \
    omp_pin="$(cat /omp-pin)"; \
    part() { case "$1" in "" | *[!A-Za-z0-9._-]* | *..*) return 1 ;; esac; }; \
    pinned() { \
      rest=${2#github:}; owner=${rest%%/*}; after=${rest#*/}; repo=${after%@*}; version=${after##*@}; \
      [ "$rest" != "$2" ] && [ "$after" != "$rest" ] && [ "$version" != "$after" ] && \
        part "$owner" && part "$repo" && part "$version" || \
        { echo "tools: $1 is '$2', which is no github:<owner>/<repo>@<version> pin of [A-Za-z0-9._-] parts without '..'" >&2; exit 1; }; \
    }; \
    pinned .omp-pin "$omp_pin"; \
    pinned "ARG JJ_TOOL" "$JJ_TOOL"; \
    pinned "ARG GH_STACK_TOOL" "$GH_STACK_TOOL"; \
    part "${GH_TOOL#gh@}" && [ "${GH_TOOL#gh@}" != "$GH_TOOL" ] || { echo "tools: ARG GH_TOOL is '$GH_TOOL', which is no gh@<version> pin of [A-Za-z0-9._-] without '..'" >&2; exit 1; }; \
    omp_repo="${omp_pin#github:}"; omp_repo="${omp_repo%@*}"; omp_tag="v${omp_pin##*@}"; \
    jj_repo="${JJ_TOOL#github:}"; jj_repo="${jj_repo%@*}"; jj_tag="v${JJ_TOOL##*@}"; \
    gh_version="${GH_TOOL#gh@}"; \
    stack_repo="${GH_STACK_TOOL#github:}"; stack_repo="${stack_repo%@*}"; stack_version="${GH_STACK_TOOL##*@}"; \
    fetch() { \
      tool=$1; arg=$2; want=$3; url=$4; file=$5; \
      printf '%s\n' "$want" | grep -Eqx '[0-9a-f]{64}' || { echo "tools: $tool: ARG $arg is '$want', which is no 64-hex sha256, so the archive at $url is not fetched" >&2; exit 1; }; \
      retry 300 "$tool download from $url" curl -fsSL -o "$file" "$url" || { echo "tools: $tool: the archive did not download from $url (exit $?)" >&2; exit 1; }; \
      got=$(sha256sum "$file"); got=${got%% *}; \
      [ "$got" = "$want" ] || { echo "tools: $tool: the archive from $url has sha256 $got, and ARG $arg pins $want" >&2; exit 1; }; \
      echo "tools: $tool: $url, sha256 $got"; \
    }; \
    asset() { printf 'https://github.com/%s/releases/download/%s/%s' "$1" "$2" "$3"; }; \
    t=/tmp/tools; mkdir -p "$t" /opt/omp /opt/tools; \
    fetch omp OMP_SHA256 "$OMP_SHA256" "$(asset "$omp_repo" "$omp_tag" "omp-${omp_tag}-linux-x64.tar.gz")" "$t/omp.tar.gz"; \
    fetch jj JJ_SHA256 "$JJ_SHA256" "$(asset "$jj_repo" "$jj_tag" "jj-${jj_tag}-x86_64-unknown-linux-musl.tar.gz")" "$t/jj.tar.gz"; \
    fetch gh GH_SHA256 "$GH_SHA256" "$(asset cli/cli "v${gh_version}" "gh_${gh_version}_linux_amd64.tar.gz")" "$t/gh.tar.gz"; \
    fetch gh-stack GH_STACK_SHA256 "$GH_STACK_SHA256" "$(asset "$stack_repo" "v${stack_version}" linux-amd64)" "$t/gh-stack"; \
    tar -xzf "$t/omp.tar.gz" -C /opt/omp --strip-components=1 --no-same-owner; \
    tar -xzf "$t/jj.tar.gz" -C "$t" --no-same-owner ./jj; \
    tar -xzf "$t/gh.tar.gz" -C "$t" --no-same-owner "gh_${gh_version}_linux_amd64/bin/gh"; \
    install -m 0755 "$t/jj" /opt/tools/jj; \
    install -m 0755 "$t/gh_${gh_version}_linux_amd64/bin/gh" /opt/tools/gh; \
    install -D -m 0755 "$t/gh-stack" /opt/tools/gh-extensions/gh-stack/gh-stack; \
    rm -rf "$t"; \
    /opt/omp/bin/omp --version; /opt/tools/jj --version; /opt/tools/gh --version; \
    stack="$(/opt/tools/gh-extensions/gh-stack/gh-stack --version)"; echo "tools: gh-stack: $stack"; \
    [ "$stack" = "gh stack version ${stack_version}" ] || { echo "tools: gh-stack: the binary says '$stack', and ARG GH_STACK_TOOL pins ${stack_version}" >&2; exit 1; }

# ------------------------------------------------------------------------------------------------
# go-base: what the Go stages below share, the golang image, its GOPROXY and git.
# ghcr.io/cybermonklabs/legion/ci, the copy of golang:1.27.3-alpine (.github/ci-images.yaml)
FROM ghcr.io/cybermonklabs/legion/ci@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS go-base
WORKDIR /src

# A `|` between proxy entries makes a go command fall through to the next source after any error,
# where a `,` falls through only after a 404 or a 410 (Go's module reference, "GOPROXY protocol").
# go.sum holds each module's hash either way, so this is a second path, not a weaker check
# (sjawhar/legion#2512).
ENV GOPROXY=https://proxy.golang.org|direct
# `direct` is git: Go clones the module's own repository, so a stage without git has a fallback
# that cannot run. This base is golang's alpine image, which carries none, and the fallback had
# never been taken here until the gopls stage's install met a proxy error on 2026-11-11 and failed the
# whole image build with `unable to resolve git version: failed to execute git version: exec:
# "git": executable file not found in $PATH` (cybermonklabs/legion#3536, run 43047193527), where
# the same layer had built from the proxy an hour earlier. git is installed here rather than at
# the layer that met it, because every `go` command of this stage reads that same GOPROXY. It
# stays in this stage: the runtime base is debian:trixie-slim, whose own git is what the workers
# run (/usr/bin/git, above), and nothing of this stage but the two static binaries is copied out.
# The package carries no version pin, for the reason the apt packages above carry none (DL3008,
# the ARG block): Alpine's repository serves only its branch's current version of a package, so a
# pinned version stops resolving at the next update. The base image's digest is the pin.
# The fetch retries on the file's canonical loop, five attempts, the fifth starting 150 s after the
# first; this base's busybox carries the `timeout` the loop bounds an attempt with. 420 s an
# attempt is this file's two apt bounds together, 120 s for an index read and 300 s for a package
# set, because under `--no-cache` this one call is both: each attempt reads Alpine's index from
# its CDN again and then fetches git's packages, so a retry also retries the index read.
# hadolint ignore=DL3018
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
    && retry 420 "apk add" apk add --no-cache git

# ------------------------------------------------------------------------------------------------
# go-licenses (pinned in go-third-party-notices.sh), for the go-notices stage, and gopls, for the
# runtime stage, each in a stage of its own: neither reads this checkout's source or module files,
# so neither a source change nor a dependency bump rebuilds them, and the two build beside each
# other and beside go-modules, where one stage built them one after the other before the download.
FROM go-base AS go-licenses
COPY scripts/go-third-party-notices.sh /usr/local/bin/
# go-licenses comes down and is built in one act (`go install …@v2.0.1`, inside the script), with no
# download to separate out, so the retry wraps the script: 600 s an attempt, one module tree
# fetched from the proxy and compiled.
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
    && retry 600 "go-licenses install" go-third-party-notices.sh --install

# `go install pkg@version` builds the module at that tag with its own go.mod, whatever this
# checkout's go.work says, so the binary changes only with GOPLS_VERSION (or GO_VERSION), never
# with a commit.
FROM go-base AS gopls
ARG GOPLS_VERSION
# gopls downloads and builds in one act too, so the retry wraps it on the same 600 s bound; `env`
# carries the two variables, since the retry runs its argument list as a command. `mkdir` is local.
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; }; \
    set -eu; \
    mkdir -p /out; \
    retry 600 "gopls install" env CGO_ENABLED=0 GOBIN=/out go install "golang.org/x/tools/gopls@v${GOPLS_VERSION}"

# ------------------------------------------------------------------------------------------------
# go-modules: the module files and their download, in a stage that copies no source, so a commit
# rebuilds neither. GOMODCACHE is /gomodcache here, and the go-licenses and gopls stages keep Go's
# default, so the modules of those two tools stay out of this cache. The runtime stage ships
# /gomodcache/cache/download to the workers as their warm module cache, and the build stages below
# build on this stage, so one download serves the build and the workers: a second download for either
# would read the same 380 MiB from proxy.golang.org again on every build that reruns this layer,
# and this image's change (sjawhar/legion#2512) counts every such read.
FROM go-base AS go-modules
ENV GOMODCACHE=/gomodcache
COPY go.work go.work.sum ./
COPY packages/daemon/go.mod packages/daemon/go.sum packages/daemon/
COPY packages/envoy/go.mod packages/envoy/go.sum packages/envoy/
# The download writes an `@v/list` file for each module. That file names the versions this cache
# holds, and no other. A file proxy answers a list request from it with no error, and a `|` in
# GOPROXY falls through to the next source on an error alone, so proxy.golang.org would never be
# asked. In every repository a role opens, `go get X@latest`, `go get -u`, `go list -m -u all`,
# `go list -m -versions` and a `go mod tidy` that adds an import of a cached module would then
# stop at the versions this build pinned, and nothing would tell the role. The RUN therefore
# deletes every list file and every `@latest` file from the cache it ships. A version query gets
# a 404 from the file proxy and falls through to the public proxy, while the `.info`, `.mod` and
# `.zip` of a pinned version still come from the cache. The final stage's toolchain step refuses
# an image that ships one of these files again.
# The download also fetches from sum.golang.org the go.mod sums go.work.sum lacks, into the cache's
# sumdb directory (240 lines and 487 files on 2026-11-22), and those tiles and lookups differ with
# the hour they are read. So the RUN deletes that directory too: the cache it ships is then the same
# bytes for the same module files, and the runtime stage's layer of it, with every layer after it,
# is reused rather than built and pushed again on every build. A pod's go reads this cache only
# through GOPROXY's file proxy, and a go asks a proxy for the checksum database at
# <proxy>/sumdb/<name>/supported, which this cache has never held. A go whose own GOMODCACHE is the
# image's (`docker run`, with no pod's GOMODCACHE) reads it as its checksum cache, and without it
# fetches from sum.golang.org the checksums it lacks, as it does for any module not in the cache.
# The download is this image's whole module cache, 380 MiB of zips from the proxy, so an attempt
# gets 900 s — the longest bound in the file. The `find` and the `rm` that trim the cache are local
# and bare, and so is every build below, which reads this cache and fetches nothing.
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
    && retry 900 "go mod download" go mod download \
    && find /gomodcache/cache/download \( -path '*/@v/list' -o -name '@latest' \) -delete \
    && rm -rf /gomodcache/cache/download/sumdb

# ------------------------------------------------------------------------------------------------
# go-source: this checkout's two Go modules on go-modules, which the go and go-notices stages build
# from, beside each other.
FROM go-modules AS go-source
COPY packages/daemon packages/daemon
COPY packages/envoy packages/envoy
WORKDIR /src/packages/daemon

# ------------------------------------------------------------------------------------------------
# go-notices: the licenses of the Go modules `legion` and `agent-secrets` compile in, with the
# build's CGO_ENABLED (which decides them, with the GOARCH the go stage builds for); fails the
# build when one's license cannot be determined. go-licenses comes built from the go-licenses
# stage, at the path go-third-party-notices.sh installs it to, so the script finds it and builds it
# no second time. A stage of its own, so the compile waits for neither the tool nor this step.
FROM go-source AS go-notices
COPY scripts/go-third-party-notices.sh /usr/local/bin/
COPY --from=go-licenses /root/.cache/legion /root/.cache/legion
RUN mkdir -p /out && CGO_ENABLED=0 go-third-party-notices.sh /out/go-notices \
    ./cmd/legion github.com/sjawhar/envoy/cmd/cipher

# ------------------------------------------------------------------------------------------------
# go: the Go coordinator's `legion`, built as the repository builds it — `go build ./cmd/legion` in
# packages/daemon under go.work, whose other module (packages/envoy) contributes its go.mod and go.sum
# to dependency selection and its listenfd package to `legion start` — static, so it runs on any
# base. LEGION_REVISION is the commit the workflow builds; it is linked in so `legion version` names
# it, and the build refuses without it. LEGION_CONTRACT_ORDER is that commit's contract entries in
# main's merge order, the stamp worker-image.yaml reads with scripts/daemon-contract-order.sh from
# the history this context leaves out; it is linked in as api.contractOrder, from which the daemon
# numbers each entry by its rank, and the build refuses without it too: an unstamped `legion`
# numbers its entries by id instead, and the stamp of a commit of no entry is `v1:`, never an empty
# value.
FROM go-source AS go
# Declared here, after the dependency layers, so a new commit re-runs only the compile.
ARG LEGION_REVISION
ARG LEGION_CONTRACT_ORDER
ARG PI_LEGION_VERSION
ARG PI_ENVOY_VERSION
RUN test -n "$LEGION_REVISION" \
    && test -n "$LEGION_CONTRACT_ORDER" \
    && ldflags="-X main.revision=${LEGION_REVISION} -X github.com/sjawhar/legion/daemon/internal/api.contractOrder=${LEGION_CONTRACT_ORDER}" \
    && if [ -n "$PI_LEGION_VERSION" ] && [ -n "$PI_ENVOY_VERSION" ]; then ldflags="$ldflags -X main.piLegionVersion=${PI_LEGION_VERSION} -X main.piEnvoyVersion=${PI_ENVOY_VERSION}"; fi \
    && CGO_ENABLED=0 go build -ldflags "$ldflags" -o /out/legion ./cmd/legion \
    && CGO_ENABLED=0 go -C /src/packages/envoy build -o /out/agent-secrets ./cmd/cipher

# ------------------------------------------------------------------------------------------------
# toolchain: what a worker needs to work a repository that is not Legion's, specific to none: uv (which
# installs each project's own Python from its `.python-version` or `requires-python`; the image's own
# python3 is trixie's, installed by the runtime-base stage for Oh My Pi's Python eval), Node with npm and
# corepack, the AWS CLI v2, the Google Cloud SDK's gcloud and bq, azure-cli's az, kubectl, mikefarah's
# yq, the Go toolchain at go.work's version (the release
# the go stage builds `legion` with, so a worker builds the daemon as CI does), and the TypeScript and
# Python language servers Oh My Pi's LSP tools start. Every download is checked against its pinned
# SHA-256 before it is unpacked or installed (azure-cli's wheels against the hashes of their lock, and
# the CPython they run on against the SHA-256 uv carries for it). uv, uvx, kubectl and yq are single
# binaries, copied to /usr/local/bin as gh and jj are; Node, Go, the AWS CLI, the Google Cloud SDK
# and azure-cli keep their own trees under /opt, and /out/bin holds the
# symlinks into them that the runtime stage copies to /usr/local/bin (the AWS installer's `--bin-dir`
# writes its two). The pnpm, pnpx, yarn and yarnpkg links are the ones `corepack enable` would write
# beside node, which the runtime user cannot: each runs the version a project's `packageManager`
# names, or else the default this corepack ships (COREPACK_DEFAULT_TO_LATEST=0, the runtime stage's
# ENV), fetched on first use into the user's own corepack cache. The language servers install from
# packages/daemon/docker/language-servers's committed lockfile (COPY'd below) with
# `npm ci --ignore-scripts`, into their own prefix, /opt/language-servers, so they land in
# /opt/language-servers/node_modules with their launchers in node_modules/.bin, linked from
# /out/bin as node's are; each launcher is a `#!/usr/bin/env node` script, which finds the image's
# node on PATH as npm's own does. typescript-language-server ships no TypeScript: it resolves
# `typescript` by Node's module resolution from its own directory, which reaches the TypeScript
# installed beside it under /opt/language-servers/node_modules, so a worker's tsserver is the
# version that package.json pins unless the repository's own node_modules holds one, which the
# server prefers. go finds its GOROOT, /opt/go, by resolving the /usr/local/bin/go
# symlink (cmd/go's findGOROOT tries the executable's own path, then its symlink target), so the image
# sets no GOROOT; the toolchain step below checks it. Right after unpacking Go, the RUN appends
# `CGO_ENABLED=0` to /opt/go/go.env, the file Go reads for a distributor's defaults: the runtime-base
# stage installs gcc for the race detector, and stock Go turns cgo on whenever it finds a C compiler on PATH,
# so without the line gcc's presence alone would flip the default of every build in a pod. The final
# stage's `ENV CGO_ENABLED=0` sets the same value for every process that inherits the image's
# environment; this line holds where that environment is scrubbed — Oh My Pi's Python eval kernel
# starts with an allowlisted environment that drops CGO_ENABLED, and a test may build with go under an
# environment it constructs. It is only a default: a role's `CGO_ENABLED=1` overrides it, as the final
# stage's ENV comment says.
# The same RUN points /opt/go/go.env's GOPROXY at the warm module cache the runtime stage copies,
# before the public proxy:
# `file:///home/legion/go/pkg/mod/cache/download|https://proxy.golang.org|direct`.
# A `|` between entries makes a go command fall through to the next source after any error. A
# module the image's cache does not hold therefore still comes from proxy.golang.org, and an image
# whose cache was deleted still builds. A version query falls through for the same reason: the
# go-modules stage deletes every `@v/list` and `@latest` file from the cache, so the file proxy answers
# `go get X@latest` or `go list -m -versions` with a 404 and proxy.golang.org answers with the
# releases it has (that stage says more). The line replaces the GOPROXY line stock go.env carries,
# and the RUN fails if that line is absent. go.env is the place for it, not an `ENV` of the final
# stage. Two facts decide that. First, a pod tells every role `GOMODCACHE=/legion/go/mod`, the
# issue volume (roleEnvironment, packages/daemon/internal/runtime/sandbox/manifest.go). That
# volume is empty in a fresh pod and dies with it (docs/kubernetes.md, "The disk dies with its
# pod"), so the image's cache reaches a role as a proxy and not as its GOMODCACHE. Second, a
# variable the final stage's ENV sets is one `runtime.kubernetes.pod.env` may not set
# (sandbox/operatorpod.go ImageEnv). A go.env default leaves GOPROXY to an operator, a role or a
# command that names its own. The process environment outranks go.env, so each of those wins.
# ghcr.io/cybermonklabs/legion/ci, the copy of debian:trixie-slim (.github/ci-images.yaml)
FROM ghcr.io/cybermonklabs/legion/ci@sha256:f6b08b09a05725f9431502d3b5714f5c4576e82c1a84fa1debe0c24a7e0c3ec4 AS toolchain
ARG UV_VERSION
ARG UV_SHA256
ARG NODE_VERSION
ARG NODE_SHA256
ARG AWS_CLI_VERSION
ARG AWS_CLI_SHA256
ARG CHROMIUM_VERSION
ARG CHROMIUM_SHA256
ARG KUBECTL_VERSION
ARG KUBECTL_SHA256
ARG YQ_VERSION
ARG YQ_SHA256
ARG GO_VERSION
ARG GO_SHA256
# Both fetches retry on this file's apt bounds, 120 s for the index read and 300 s for the set,
# and the index read carries this file's error-mode option.
# hadolint ignore=DL3008
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
    && retry 120 "apt-get update" apt-get update -o APT::Update::Error-Mode=any \
    && retry 300 "apt-get install" apt-get install -y --no-install-recommends ca-certificates curl unzip xz-utils \
    && rm -rf /var/lib/apt/lists/*
COPY packages/daemon/docker/language-servers/package.json packages/daemon/docker/language-servers/package-lock.json /opt/language-servers/
# The eight fetches of this RUN all take the wrapper, so one RUN reads one way. Each bound is what
# that download is: 120 s for a single binary or a small archive (uv, kubectl, yq), 300 s for the
# large ones (Node, the AWS CLI, Go and Chromium, tens to hundreds of megabytes each) and 300 s for
# the language servers' `npm ci`, a lockfile's worth of npm tarballs. Everything else here — the
# checksum check, the unpacking, the links, the go.env edits and the AWS installer — is local and
# stays bare. Each download's URL is set once and both fetched and named from that one variable, so
# a fetch that exhausts its attempts names the URL it failed on, and the name cannot drift from
# what the line fetches (dispatch://INFRA-1865, folded into dispatch://INFRA-1545).
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; }; \
    set -eu; \
    t=/tmp/toolchain; mkdir -p "$t" /out/bin /opt/node; \
    u="https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-x86_64-unknown-linux-gnu.tar.gz"; \
    retry 120 "uv download from $u" curl -fsSLo "$t/uv.tar.gz" "$u"; \
    u="https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz"; \
    retry 300 "node download from $u" curl -fsSLo "$t/node.tar.xz" "$u"; \
    u="https://awscli.amazonaws.com/awscli-exe-linux-x86_64-${AWS_CLI_VERSION}.zip"; \
    retry 300 "aws cli download from $u" curl -fsSLo "$t/awscli.zip" "$u"; \
    u="https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"; \
    retry 300 "go download from $u" curl -fsSLo "$t/go.tar.gz" "$u"; \
    u="https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/amd64/kubectl"; \
    retry 120 "kubectl download from $u" curl -fsSLo "$t/kubectl" "$u"; \
    u="https://github.com/mikefarah/yq/releases/download/v${YQ_VERSION}/yq_linux_amd64"; \
    retry 120 "yq download from $u" curl -fsSLo "$t/yq" "$u"; \
    u="https://storage.googleapis.com/chrome-for-testing-public/${CHROMIUM_VERSION}/linux64/chrome-linux64.zip"; \
    retry 300 "chromium download from $u" curl -fsSLo "$t/chromium.zip" "$u"; \
    printf '%s  %s\n' "$UV_SHA256" "$t/uv.tar.gz" "$NODE_SHA256" "$t/node.tar.xz" \
      "$AWS_CLI_SHA256" "$t/awscli.zip" "$GO_SHA256" "$t/go.tar.gz" \
      "$KUBECTL_SHA256" "$t/kubectl" "$YQ_SHA256" "$t/yq" \
      "$CHROMIUM_SHA256" "$t/chromium.zip" > "$t/SHA256SUMS"; \
    sha256sum --check --strict "$t/SHA256SUMS"; \
    tar -xzf "$t/uv.tar.gz" -C /out/bin --strip-components=1 --no-same-owner \
      uv-x86_64-unknown-linux-gnu/uv uv-x86_64-unknown-linux-gnu/uvx; \
    install -m 0755 "$t/kubectl" /out/bin/kubectl; \
    install -m 0755 "$t/yq" /out/bin/yq; \
    tar -xJf "$t/node.tar.xz" -C /opt/node --strip-components=1 --no-same-owner; \
    for tool in node npm npx corepack; do ln -s "/opt/node/bin/$tool" "/out/bin/$tool"; done; \
    for shim in pnpm pnpx yarn yarnpkg; do \
      ln -s "/opt/node/lib/node_modules/corepack/dist/$shim.js" "/out/bin/$shim"; \
    done; \
    (cd /opt/language-servers && PATH="/opt/node/bin:$PATH" retry 300 "npm ci (language servers)" npm ci --ignore-scripts --omit=dev --no-audit --no-fund); \
    for tool in typescript-language-server tsc tsserver pyright pyright-langserver; do \
      ln -s "/opt/language-servers/node_modules/.bin/$tool" "/out/bin/$tool"; \
    done; \
    tar -xzf "$t/go.tar.gz" -C /opt --no-same-owner; \
    printf 'CGO_ENABLED=0\n' >> /opt/go/go.env; \
    goproxy='GOPROXY=file:///home/legion/go/pkg/mod/cache/download|https://proxy.golang.org|direct'; \
    sed -i "s#^GOPROXY=.*#$goproxy#" /opt/go/go.env; \
    grep -qxF "$goproxy" /opt/go/go.env; \
    for tool in go gofmt; do ln -s "/opt/go/bin/$tool" "/out/bin/$tool"; done; \
    unzip -q "$t/awscli.zip" -d "$t"; \
    "$t/aws/install" --install-dir /opt/aws-cli --bin-dir /out/bin; \
    mkdir -p /out/licenses; \
    cp "$t/aws/THIRD_PARTY_LICENSES" /out/licenses/aws-cli-THIRD_PARTY_LICENSES; \
    unzip -q "$t/chromium.zip" -d "$t"; \
    mv "$t/chrome-linux64" /out/chromium; \
    rm -rf "$t"
# The Google Cloud SDK, in a RUN of its own after the one above, which makes /out/bin and
# /out/licenses, so a bump of its pin rebuilds this layer and azure-cli's two layers below it and no
# other. The archive is checked against GCLOUD_SHA256 (the ARG block says where that hash comes
# from) before it is unpacked into /opt/google-cloud-sdk, and CLOUDSDK_CONFIG keeps the build's own
# gcloud configuration in the scratch directory the RUN removes. install.sh lists the channel's
# components, which reads the component list from dl.google.com, the archive's host.
# `--usage-reporting false` and the
# installation property component_manager/disable_update_check stop the SDK reporting usage and
# looking for a newer release. The installation property core/custom_ca_certs_file points gcloud
# and bq at Debian's root bundle. As the SDK ships, both trust the CA files it bundles, which hold
# the compromised emSign and AAA roots: measured on 2026-11-18 in an INFRA issue pod, gcloud 589.0.0
# and bq 2.1.40 accepted the emSign chain. With the property at Debian's bundle both refuse the two
# chains. The two properties
# are defaults: a role's own gcloud configuration or
# CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE still overrides them, as a role's AWS_CA_BUNDLE does the
# image's.
# /opt is read-only in a pod, so the SDK cannot write the bytecode Python would otherwise compile at
# every run, and the RUN compiles it here with the SDK's bundled Python. Measured in an INFRA issue
# pod (2 vCPU under gVisor) on 2026-11-18, with `gcloud compute regions list` against example.com,
# that cut the CPU of one gcloud call from 5.0-7.1 s with no bytecode to 3.5 s with every module
# but the generated API clients compiled, and to 2.7-2.9 s with them too; a bq call went from
# 11.2-16.8 s to 6.4-7.2 s. The generated API clients (lib/googlecloudsdk/generated_clients) hold
# 144 MB of the 260 MB of bytecode a whole compile writes, for 212 APIs in 427 versions, of which a
# gcloud command imports the one it calls, so the compile leaves them out, except compute v1
# (5.8 MB): with every other module compiled, it makes a role's `gcloud compute` call about 0.6 s
# to 0.8 s faster (2.7-2.9 s against 3.5 s, the measurement above). The RUN's own `gcloud config
# set` calls run as root before the compile and write the bytecode of what they import, which holds
# generated_clients/apis/apis_map.py too. What the RUN compiles, about 124 MB, sits beside the
# SDK's 428 MB (both read from a scratch install of 594.0.0). compileall's -x leaves out the trees
# that hold the Python 2 sources the archive still carries, which Python 3 cannot compile: the
# python2/ trees, lib's third_party/yaml tree (its constructor.py and resolver.py are Python 2) and
# bq's third_party/gflags. bq's own third_party/yaml is PyYAML for Python 3, which bq imports on
# every command, so it is compiled. Any other file that fails to compile fails the build, so a
# version bump that adds one fails here and is read by a person. The installer's
# .install/.backup goes too. gcloud and bq are linked from /out/bin and gsutil is not; the SDK's
# LICENSE and its bundled CPython's LICENSE.txt go to the notices stage.
# The two fetches of this RUN retry. The archive, 94,824,123 bytes from dl.google.com, takes 300 s
# an attempt, this file's bound for a large archive (Node, the AWS CLI, Go and Chromium above); it
# came down cold in 1.0 to 1.4 s in an issue pod (2 vCPU under gVisor, 2026-11-19), and this whole
# RUN took 38.0 s in the worker-image build of cybermonklabs/legion#4377 (job 135176358267), the
# compile most of it. install.sh's one read is the channel's component list,
# https://dl.google.com/dl/cloudsdk/channels/rapid/components-2.json, 227,309 bytes, the SDK's own
# index, so it takes this file's 120 s for an index read: it ran in 2.7 to 3.6 s in that pod, and
# through a proxy refusing every host it exited 1, so the retry has a failure to retry. The two
# `gcloud config set --installation` calls write the installation's properties and read nothing
# from the network: through that proxy, with a fresh configuration directory, each exited 0 having
# asked it for nothing. They stay bare, as do the checksum, the unpack and the compile.
ARG GCLOUD_VERSION
ARG GCLOUD_SHA256
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; }; \
    set -eu; \
    t=/tmp/gcloud; mkdir -p "$t"; export CLOUDSDK_CONFIG="$t/config"; \
    u="https://dl.google.com/dl/cloudsdk/channels/rapid/downloads/google-cloud-cli-${GCLOUD_VERSION}-linux-x86_64.tar.gz"; \
    retry 300 "google cloud sdk download from $u" curl -fsSLo "$t/sdk.tar.gz" "$u"; \
    printf '%s  %s\n' "$GCLOUD_SHA256" "$t/sdk.tar.gz" > "$t/SHA256SUMS"; \
    sha256sum --check --strict "$t/SHA256SUMS"; \
    tar -xzf "$t/sdk.tar.gz" -C /opt --no-same-owner; \
    retry 120 "google cloud sdk install.sh" /opt/google-cloud-sdk/install.sh --quiet --usage-reporting false --path-update false --command-completion false; \
    /opt/google-cloud-sdk/bin/gcloud config set --installation component_manager/disable_update_check true; \
    /opt/google-cloud-sdk/bin/gcloud config set --installation core/custom_ca_certs_file /etc/ssl/certs/ca-certificates.crt; \
    sdkpython=/opt/google-cloud-sdk/platform/bundledpythonunix/bin/python3; \
    "$sdkpython" -m compileall -q -j0 -x '/python2/|/lib/third_party/yaml/|/third_party/gflags/|/generated_clients/' \
      /opt/google-cloud-sdk/lib /opt/google-cloud-sdk/platform/bq; \
    "$sdkpython" -m compileall -q -j0 /opt/google-cloud-sdk/lib/googlecloudsdk/generated_clients/apis/compute/v1; \
    rm -rf /opt/google-cloud-sdk/.install/.backup "$t"; \
    ln -s /opt/google-cloud-sdk/bin/gcloud /opt/google-cloud-sdk/bin/bq /out/bin/; \
    cp /opt/google-cloud-sdk/LICENSE /out/licenses/google-cloud-sdk-LICENSE; \
    cp /opt/google-cloud-sdk/platform/bundledpythonunix/lib/python3.*/LICENSE.txt /out/licenses/google-cloud-sdk-python-LICENSE.txt
# azure-cli, in a RUN of its own after the SDK's. Its tree is self-contained, as /opt/node, /opt/go
# and /opt/aws-cli are: a virtual environment, /opt/azure-cli/env, on the CPython uv installs under
# /opt/azure-cli/python (ARG AZURE_CLI_PYTHON_VERSION), rather than on Debian's /usr/bin/python3,
# because LINUX's worker-lean and linux-host images are Ubuntu, and a venv on this stage's python3
# would not run there when they copy /opt/azure-cli whole. The install takes wheels alone from the
# hashed lock,
# packages/daemon/docker/azure-cli/requirements.txt, with --require-hashes and --no-deps, so every
# byte it installs is one the lock names and nothing the lock leaves out comes in (requirements.in
# beside it holds azure-cli's pin and the command that writes the lock). --compile-bytecode, because
# /opt is read-only in a pod and Python could not write the bytecode at run time: measured with uv
# 0.13.5 in an INFRA issue pod on 2026-11-18, it cut an `az rest` call from about 2.0 s of CPU to
# 1.0 s. The lock is 150 distributions; the environment is 657 MB, 305 MB of it bytecode, and the
# Python 98 MB. uv writes `.lock` (mode 0666), `.temp` and `.gitignore` into its install directory,
# which the RUN deletes, and a cpython-3.13-linux-x86_64-gnu link beside the versioned tree, which
# stays; the environment's python links to the versioned tree, and the wheel's `az` entry script
# gets the absolute shebang #!/opt/azure-cli/env/bin/python, so the tree runs at /opt/azure-cli and
# nowhere else. `chmod -R a+rX,go-w` leaves it readable by the runtime user and writable by root
# alone. /usr/local/bin/az, through /out/bin, is a link to /opt/azure-cli/bin/az,
# packages/daemon/docker/azure-cli/az: the wrapper that sets azure-cli's CA-bundle and telemetry
# defaults, and says why they are set there. The wrapper is copied after the install, by a COPY
# that makes it root's and mode 0755 itself, so an edit of it rebuilds that one layer and
# reinstalls none of the 150 wheels. The LICENSE.txt of the wheel and the one of the CPython go to
# the notices stage, each read from the python3.<minor> directory the RUN derives from ARG
# AZURE_CLI_PYTHON_VERSION, so a bump of its minor reads the new directory.
# The two fetches of this RUN retry. `uv venv` downloads the CPython, 35,356,674 bytes from
# releases.astral.sh, falling back to github.com, so it takes this file's 120 s for a small archive
# (uv, kubectl and yq above): it came down and made the environment in 3.9 s cold in an issue pod
# (2 vCPU under gVisor, 2026-11-19), and through a proxy refusing every host it exited 2.
# `--clear` is what lets it run again: a second `uv venv` over an environment an earlier attempt
# made exits 2 (`A virtual environment already exists`) where --clear replaces it, and the first
# attempt meets no directory, so the flag changes nothing there. `uv pip install` reads pypi.org's
# index and downloads the lock's 150 wheels from files.pythonhosted.org (through that proxy it
# exited 2), then installs and compiles them in the same act, with no download to separate out, so
# the retry wraps the whole command, as it does the go stage's `go install`s: 300 s an attempt,
# about 2.4 times the 126.8 s it took cold in that pod. This whole RUN took 45.8 s in the
# worker-image build of cybermonklabs/legion#4377 (job 135176358267). The `rm`, the `chmod`, the
# link and the two copies are local and stay bare.
ARG AZURE_CLI_PYTHON_VERSION
RUN --mount=type=bind,source=packages/daemon/docker/azure-cli/requirements.txt,target=/tmp/azure-cli/requirements.txt \
    retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; }; \
    set -eu; \
    py="python${AZURE_CLI_PYTHON_VERSION%.*}"; \
    scratch="$(mktemp -d)"; export HOME="$scratch" UV_CACHE_DIR="$scratch/uv-cache"; \
    retry 120 "cpython ${AZURE_CLI_PYTHON_VERSION} download" env UV_PYTHON_INSTALL_DIR=/opt/azure-cli/python \
      /out/bin/uv venv --clear --python "cpython-${AZURE_CLI_PYTHON_VERSION}-linux-x86_64-gnu" /opt/azure-cli/env; \
    retry 300 "azure-cli wheels" /out/bin/uv pip install --python /opt/azure-cli/env/bin/python --require-hashes \
      --only-binary :all: --no-deps --compile-bytecode --no-cache -r /tmp/azure-cli/requirements.txt; \
    rm -rf /opt/azure-cli/python/.lock /opt/azure-cli/python/.temp /opt/azure-cli/python/.gitignore "$scratch"; \
    chmod -R a+rX,go-w /opt/azure-cli; \
    ln -s /opt/azure-cli/bin/az /out/bin/az; \
    cp /opt/azure-cli/env/lib/"$py"/site-packages/azure_cli-*.dist-info/licenses/LICENSE.txt /out/licenses/azure-cli-LICENSE.txt; \
    cp /opt/azure-cli/python/cpython-"${AZURE_CLI_PYTHON_VERSION}"-linux-x86_64-gnu/lib/"$py"/LICENSE.txt /out/licenses/azure-cli-python-LICENSE.txt
COPY --chmod=0755 packages/daemon/docker/azure-cli/az /opt/azure-cli/bin/az

# ------------------------------------------------------------------------------------------------
# runtime-base: debian:trixie-slim for its git (2.47; jj 0.46's git backend needs >= 2.42 — bookworm
# and bookworm-backports stop at 2.39.5). The dynamically linked binaries copied in below were built
# or fetched on bookworm; trixie's newer glibc runs them, and the probe RUN below proves it.
# No later layer may install a package: the image's Debian packages are the base's own and those this
# stage's one apt RUN adds, and the debian-packages stage lists them from this stage's dpkg database,
# so a package installed in a later layer would be missing from the notices file. ENV, USER and
# WORKDIR belong in the final stage, not here:
# TestImageEnvIsWhatTheWorkerImageSets (packages/daemon/internal/runtime/sandbox/operatorpod_test.go)
# reads the last stage's ENV only.
# ghcr.io/cybermonklabs/legion/ci, the copy of debian:trixie-slim (.github/ci-images.yaml)
FROM ghcr.io/cybermonklabs/legion/ci@sha256:f6b08b09a05725f9431502d3b5714f5c4576e82c1a84fa1debe0c24a7e0c3ec4 AS runtime-base
# git: jj's git backend and the workers' own git use. ca-certificates: GitHub, Dispatch, model APIs.
# chromium, python3, curl, wget, jq, openssh-client (for ssh-keygen), util-linux (for flock),
# postgresql-client-18 (for psql), redis-tools (for redis-cli), sqlite3, netcat-openbsd (for nc) and
# bind9-dnsutils (for dig): what the capability check's browser, eval-python and toolchain rows look for
# (packages/daemon/internal/capabilities/image.go) — the last five join flock in the toolchain row —
# trixie's since the suite is the apt pin (the note above the ARGs); chromium brings its library
# closure as its Depends (a font with it: fontconfig-config requires one, fonts-dejavu-core first),
# and its Recommends stay out with everyone else's — the setuid sandbox (chromium-sandbox) and
# fonts-liberation among them; Oh My Pi never uses the sandbox, launching the browser with
# --no-sandbox --disable-setuid-sandbox. jq and ssh-keygen are real binaries, not the harness's: the
# harness bash's jq builtin reaches only the bash tool, while a worker's subprocesses (`bun test`, a
# script, a subagent's shell) need /usr/bin/jq. openssh-client is the one apt route to ssh-keygen and
# also brings ssh, scp, sftp, ssh-agent and ssh-add, which reach no host that curl, wget, git, python3
# and socat on this line could not already reach (git's ssh transport is what they make runnable; a
# pod's git speaks https with its role's App token through gh): what a pod may reach is set by the
# cited issue, the capability table's withheld `network` row
# (packages/daemon/internal/capabilities/capabilities.go, dispatch://LEGION-5: the pod is the
# boundary, except a workload a role starts where its deployment's cluster admits it and the
# Service fronting it, docs/kubernetes.md "Running a workload in the cluster from a role"), a
# declaration the report renders, not a network control this repository ships. The
# package's two set-id files lose their bits in the RUN below: /usr/lib/openssh/ssh-keysign, setuid
# root for host-based authentication, which nothing here uses, and /usr/bin/ssh-agent, setgid _ssh by
# its postinst. A pod would ignore both anyway (allowPrivilegeEscalation false and every capability
# dropped, packages/daemon/internal/runtime/sandbox/manifest.go), and the final step refuses an image
# where any file under /usr/lib/openssh or /usr/bin/ssh-agent carries one, so a rebuild of this layer
# (a new trixie-slim digest, a package update) cannot bring a bit back unnoticed.
# socat: a pipe that carries a connection over `kubectl exec` to a port the pod listens on at 127.0.0.1
# (a dev server an agent runs), which under gVisor `kubectl port-forward` cannot reach. python3 or
# bash's /dev/tcp over the same exec works too; a listener on 0.0.0.0 is also reachable on the pod IP
# inside the cluster, unless the deployment's NetworkPolicy denies that ingress.
# util-linux (flock): the install Legion renders for an installing launch serialises on the issue's
# shared bun install cache with /usr/bin/flock (installBlock,
# packages/daemon/internal/runtime/sandbox/plugins.go), so a pod whose image lacks it
# fails every installing launch. The entry buys nothing today — Debian's priority:required set
# already brings util-linux into the base — and buys a build failure instead of a fleet-wide launch
# failure the day a base drops it. Do not delete it as redundant.
# The WebKit list, the four gstreamer1.0- packages and every lib package on the line but libc6-dev:
# the 56 packages Playwright's own `install-deps` names for WebKit on Debian 13 (its `debian13-x64`
# table in playwright-core/lib/coreBundle.js, at the Playwright version packages/dispatch locks,
# 1.64.1), every one in trixie's main (read against its Packages index on 2026-11-18), so a role
# runs the Dispatch browser suite's WebKit projects as the runtime user, which cannot run
# `install-deps`. All 56 are named, not only the ones the image lacked, so the line states WebKit's
# whole list and leans on no other package's dependencies. The `tools` group `install-deps` adds
# for every browser (xvfb and fonts) is left out: it holds no library WebKit links or loads, and
# Playwright's launch validation does not read it. The webkit-check stage fails the build when a
# Playwright bump needs a library this list lacks.
# postgresql-client-18 (psql): `bun run e2e`, the Dispatch browser suite this deployment's
# instructions require before a reader-facing Dispatch change merges, reaches psql through
# packages/dispatch/e2e/psql.ts, which seed.ts calls for every spec's database reset; and the local
# stack `bun run dispatch:local` (packages/dispatch/e2e/local-instance.ts) refuses to start without
# psql on PATH — so a role whose image lacks psql cannot run the suite its own change owes. The
# Debian psql is a Perl wrapper around the versioned client; perl-base is already in the image, so
# the wrapper resolves.
# postgresql-18 and postgresql-18-pgvector (the server and the vector extension): the pod's own test
# database, which /usr/local/bin/legion-testpg starts under LEGION_TESTPG_DIR
# (/var/run/legion/roles/testpg, the emptyDir the daemon mounts there) and LEGION_TEST_PG_DSN names.
# Without a server in the image, a role that needs Postgres either unpacks debs onto the issue volume
# for twenty minutes or points its test at the deployment's sessions database, whose login drops
# databases on the instance the whole fleet's sessions live in. 18 is the major the deployment's
# databases and CI run (packages/daemon/internal/testpg/testdb/testdb.go), so both come from
# apt.postgresql.org: trixie serves 17 alone, and a cluster of another major is a different planner,
# a different catalog and a different dump format from the one a test meets everywhere else. The ARG
# note above says what is pinned there and why a version pin is not. The source list and the key go
# in the same RUN, since this stage's single apt RUN is the only one the image allows (the paragraph
# at the top of the stage says why): it installs ca-certificates and curl first, writes the key and
# the list, updates again and then installs the whole set. The client is postgresql-client-18, named
# by its major: the unversioned postgresql-client follows apt.postgresql.org's newest major, which
# became 19 on 2026-11-19, and pulled postgresql-client-19 beside the server's 18.
# The extension goes into template1 at start, so every database a test makes from it carries pgvector
# and the Dispatch suite needs no step of its own. The RUN below deletes the cluster
# `pg_createcluster` makes from the package's postinst (/var/lib/postgresql, /etc/postgresql): no pod
# runs it, since a pod's cluster is initdb's under LEGION_TESTPG_DIR, under the runtime user. The
# server programs stay off PATH — postgresql-common wraps the client programs alone — so
# legion-testpg names initdb, pg_ctl and postgres by path.
# redis-tools (redis-cli): no repository of the project speaks Redis today. It is carried anyway,
# at about a megabyte, because a second image roll costs more than a megabyte.
# sqlite3: a repository's own SQLite fixture or database has no other way to open from a shell, and
# Oh My Pi's own profile databases under ~/.omp/profiles/legion are SQLite too. A role's session is
# not one of them: it is results.jsonl, state.json and title under XDG_STATE_HOME, and the session
# store these pods otherwise use is Postgres.
# netcat-openbsd (nc): a reachability check otherwise has to be written as an inline Python loop.
# `nc` is not a file of the package: it ships /usr/bin/nc.openbsd, and the `nc` name comes from the
# alternatives system, whose script runs as root during this build — so the checks below ask for
# `nc`, never `nc.openbsd`.
# bind9-dnsutils (dig): a role whose connection to a service name fails cannot otherwise tell "the
# name does not resolve" from "the packets are dropped". The package is named rather than the
# virtual `dnsutils`, which trixie has no candidate for: apt resolves it to this single provider
# today, and a second provider the day one appears would fail the build on a name that was never
# the package.
# unzip: model-gateway/apply_test.sh of example-org/deployment calls `unzip`, and no package
# runner in the image reaches it. A role cannot install it in the pod either: its /usr/local/bin and
# /opt are read-only, and apt-get cannot take dpkg's lock.
# busybox: example-org/deployment's broker/ tests run scripts under `busybox sh`, and without it
# the suite fails with "busybox is not on PATH". trixie's package (busybox 1:1.37.0-6+b9, read
# 2026-11-18; the toolchain step prints its version) installs /usr/bin/busybox alone, with no applet
# links, so `wget` and `sh` stay GNU wget and dash. Its `wget` and `ssl_client` applets do no
# certificate validation (its own help says "TLS certificate validation not implemented"), so a
# busybox applet must never become the `wget` on PATH: a role's `wget` would then fetch over https
# with no certificate check, and the `wget` the capability check's `toolchain` row asks for
# (packages/daemon/internal/capabilities/image.go) is GNU wget. busybox stays out of that
# `toolchain` capability row: that
# row is the floor every deployment's image is held to, and LINUX's `worker-lean` image copies
# /usr/local/bin from this one, not /usr/bin.
# tmux: the skill-scenario rig's panes (packages/pi-legion/scripts/skill-scenarios/rig.sh) and the
# tmux runtime's tests (packages/daemon/internal/runtime/tmux, whose real-tmux tests skip where
# tmux is absent) need it, and without it in the image a role rebuilds it after every pod
# replacement. trixie's package is tmux 3.5a-3 (read 2026-11-18); the toolchain step prints its
# version.
# gcc and libc6-dev: the C compiler and the libc headers and startup files Go's cgo builds with. Go
# refuses -race without cgo, so these are what lets a role run the race detector — the
# `go test -race …` of CI's envoy-go-race lane — as `CGO_ENABLED=1 go test -race …`, with no CC or
# linker override; gcc joins go in the toolchain row. libc6-dev is named rather than left to gcc
# because it is only a Recommends of gcc-14, and Recommends stay out here as everywhere. cgo stays off
# by default all the same (the final stage's `ENV CGO_ENABLED=0` and the toolchain stage's go.env line
# say how).
# python3-yaml: Debian's PyYAML for the image's python3, so `python3 -c "import yaml"` answers in a
# pod. Six of this repository's own gates run python3 with `import yaml`:
# .github/scripts/check-action-pins.sh, check-bun-version.sh, check-ci-images.sh,
# check-image-trigger-paths.sh, check-release-credentials.sh, and check-ci-filters.sh through
# ci_filter_coverage.py. A Legion role runs those gates on its own change before it returns the
# work. apt is the one install route here. This stage's apt RUN is the only one the image allows
# (the paragraph at the top of the stage says why), and trixie marks its python3 externally
# managed (PEP 668), which refuses a pip install into it.
# /opt/legion and /opt/legion/bin are created here, root-owned, before any COPY into them: a COPY
# creates a missing parent with its own --chown, so a runtime-user copy into /opt/legion would
# otherwise leave the runtime user free to rename bin/ and plant its own `legion`. The final step
# refuses an image where either is not root's.
# /var/run/legion/{gh,private,state,agent-secrets} are the empty, root-owned mount points an issue
# pod's role-init binds each role's own directories back at (packages/daemon/internal/launcher,
# roleinit_linux.go): inside a role's namespaces the role is the runtime user, which can create
# nothing under the root-owned /var/run, so the image makes them. The controller's pod mounts its
# volumes over the same paths, as it always has. The final step refuses an image where any is not
# root's or is not empty.
# /etc/apt/preferences.d/pgdg holds apt.postgresql.org to the PostgreSQL packages. That suite
# rebuilds packages trixie also carries, and without a pin its versions would win on candidate
# order for anything the two archives share, so a server upgrade could quietly pull a different
# libssl or ca-certificates into the fleet's image. Everything from that origin is priority 100,
# below the 500 a Debian suite gets, and `postgresql*` and libpq5 are 600: the server, the client,
# postgresql-common and the pgvector extension come from pgdg, libpq5 with them because 18's client
# needs a newer one than trixie ships, and every other package stays Debian's.
# The RUN also deletes /etc/ssl/private/ssl-cert-snakeoil.key and its certificate. postgresql-common
# depends on ssl-cert, whose postinst generates that self-signed pair at install time; this image is
# public, so it must carry no private key, and nothing here serves TLS from one — the pod's cluster
# listens on loopback with ssl off. The `find … -xtype l -delete` clears the hash link the
# certificate left behind in the trust directory, which would otherwise dangle.
ARG PGDG_KEY_SHA256
# The four fetches of this RUN retry. The index reads take this file's 120 s; the first install is
# ca-certificates and curl, the 300 s package set; the key is 4 KB of armored text and takes 60 s;
# and the second install is this image's whole Debian set — Chromium, PostgreSQL 18 with pgvector,
# gcc, WebKit's host libraries and the rest — which is why it alone takes 600 s. With busybox, tmux
# and the WebKit list it is 442 MB of archives, 1662 MB installed (325 MB and 1249 MB before them),
# and this whole RUN took 168.5 s where it had taken 98.6 s (worker-image builds of
# cybermonklabs/legion#4377, job 135176358267, and of #2918, job 135217448205), the second install
# about 150 s of it, so the bound is four times the attempt measured. Everything else here, the
# checksum of the key included, is local and stays bare. Both index reads carry this file's
# error-mode option.
# hadolint ignore=DL3008
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
    && install -d -m 0755 /etc/apt/keyrings \
    && retry 120 "apt-get update" apt-get update -o APT::Update::Error-Mode=any \
    && retry 300 "apt-get install" apt-get install -y --no-install-recommends ca-certificates curl \
    && retry 60 "pgdg key" curl -fsSL -o /etc/apt/keyrings/pgdg.asc https://www.postgresql.org/media/keys/ACCC4CF8.asc \
    && printf '%s  %s\n' "$PGDG_KEY_SHA256" /etc/apt/keyrings/pgdg.asc > /tmp/pgdg-key.sha256 \
    && sha256sum --check --strict /tmp/pgdg-key.sha256 \
    && rm -f /tmp/pgdg-key.sha256 \
    && echo 'deb [signed-by=/etc/apt/keyrings/pgdg.asc] https://apt.postgresql.org/pub/repos/apt trixie-pgdg main' \
      > /etc/apt/sources.list.d/pgdg.list \
    && printf '%s\n' 'Package: *' 'Pin: origin apt.postgresql.org' 'Pin-Priority: 100' '' \
      'Package: postgresql* libpq5' 'Pin: origin apt.postgresql.org' 'Pin-Priority: 600' \
      > /etc/apt/preferences.d/pgdg \
    && retry 120 "apt-get update" apt-get update -o APT::Update::Error-Mode=any \
    && retry 600 "apt-get install" apt-get install -y --no-install-recommends bind9-dnsutils busybox ca-certificates chromium curl gcc git \
      gstreamer1.0-libav gstreamer1.0-plugins-bad gstreamer1.0-plugins-base gstreamer1.0-plugins-good jq \
      libatk-bridge2.0-0t64 libatk1.0-0t64 libatomic1 libavif16 libc6-dev libcairo2 libdbus-1-3 libdrm2 \
      libegl1 libenchant-2-2 libepoxy0 libevdev2 libevent-2.1-7t64 libfontconfig1 libfreetype6 libgbm1 \
      libgdk-pixbuf-2.0-0 libgles2 libglib2.0-0t64 libglx0 libgstreamer-gl1.0-0 \
      libgstreamer-plugins-base1.0-0 libgstreamer1.0-0 libgtk-4-1 libgudev-1.0-0 libharfbuzz-icu0 \
      libharfbuzz0b libhyphen0 libicu76 libjpeg62-turbo liblcms2-2 libmanette-0.2-0 libnotify4 libopengl0 \
      libopenjp2-7 libopus0 libpango-1.0-0 libpng16-16t64 libproxy1v5 libsecret-1-0 libsoup-3.0-0 \
      libwayland-client0 libwayland-egl1 libwayland-server0 libwebp7 libwebpdemux2 libwoff1 libx11-6 \
      libxcomposite1 libxdamage1 libxkbcommon0 libxml2 libxslt1.1 netcat-openbsd openssh-client \
      postgresql-18 postgresql-18-pgvector postgresql-client-18 python3 python3-yaml redis-tools socat sqlite3 \
      tmux unzip util-linux wget \
    && rm -rf /var/lib/apt/lists/* /var/lib/postgresql /etc/postgresql \
      /etc/ssl/private/ssl-cert-snakeoil.key /etc/ssl/certs/ssl-cert-snakeoil.pem \
    && find /etc/ssl/certs -xtype l -delete \
    && chmod u-s /usr/lib/openssh/ssh-keysign \
    && chmod g-s /usr/bin/ssh-agent \
    && groupadd --gid 1000 legion \
    && useradd --uid 1000 --gid 1000 --create-home --shell /bin/bash legion \
    && install -d -m 0755 -o root -g root /opt/legion /opt/legion/bin /opt/legion/githooks \
    && install -d -m 0755 -o root -g root /var/run/legion /var/run/legion/gh /var/run/legion/private \
      /var/run/legion/state /var/run/legion/agent-secrets

# ------------------------------------------------------------------------------------------------
# debian-packages (build only): trixie's own dpkg-query over runtime-base's own database, one
# installed package per line. ${Package}, not ${binary:Package}, which prints name:amd64 for a
# Multi-Arch: same package, a name with no /usr/share/doc directory. dpkg-query writes to a file
# first so its failure fails the RUN (dash has no pipefail); only `installed` rows are kept, and
# LC_ALL=C sort -u makes the order locale-independent. An empty list fails the build in the
# assemble script's own empty-file check.
FROM runtime-base AS debian-packages
RUN dpkg-query -W -f '${db:Status-Status} ${Package}\n' > /tmp/dpkg-packages \
    && sed -n 's/^installed //p' /tmp/dpkg-packages | LC_ALL=C sort -u > /debian-packages

# ------------------------------------------------------------------------------------------------
# notices: /out/THIRD_PARTY_NOTICES, the image's /usr/share/doc/legion/THIRD_PARTY_NOTICES — the
# license of every third-party piece the image ships. Its first section names the Debian packages,
# listed by the debian-packages stage from runtime-base's dpkg database, whose terms are in
# /usr/share/doc/<package>/copyright; the npm packages installed unmodified have their own license
# files beside them. What this checkout builds carries notices generated from what the build included
# (the go-notices stage's, the packed plugin's); a prebuilt tool carries the license files its distribution
# ships (Node's LICENSE, the AWS CLI's THIRD_PARTY_LICENSES, the Google Cloud SDK's LICENSE and the
# LICENSE.txt of the CPython it bundles, the LICENSE.txt of azure-cli's wheel and that of the CPython
# uv installs for it) and those its source repository holds at
# the pinned release, fetched here. jj, uv and mise link Rust crates whose license texts none of
# their releases ships, so their sections name the Cargo.lock that lists those crates; gopls, which
# the gopls stage
# compiles, links Go modules the same way, and its section names the go.mod that lists them rather
# than a go-licenses report like `legion`'s, because go-licenses (go-third-party-notices.sh's
# v2.0.1, under Go 1.27.1) finds no license for one of them, github.com/segmentio/asm, and would fail
# the build. kubectl's section names the go.mod of the Kubernetes release its binary is built from,
# the same way. The k8s.io modules that go.mod replaces with ./staging/src, kubectl itself among them,
# are that release's own source, each with its license file under staging/src/k8s.io at the release's
# tag; the repository keeps the license text of every other module it links under LICENSES/vendor
# there. yq's and gh-stack's sections name their own go.mod the same way. The language servers' own LICENSE files come
# from their installed trees in the toolchain stage; TypeScript's ThirdPartyNoticeText.txt and the
# typeshed LICENSE pyright bundles come with them. azure-cli's Python distributions carry their own
# license files in their *.dist-info directories under /opt/azure-cli/env, where a wheel ships one,
# and each one's METADATA there names its license; the section on them lists them, name and
# version, from the lock, packages/daemon/docker/azure-cli/requirements.txt. The libraries the
# Google Cloud SDK vendors under lib/third_party, platform/bq/third_party and
# platform/gsutil/third_party carry
# the license files its archive ships beside them, which for five of them in 594.0.0 is none (attr,
# cattr and pyparsing under lib/third_party, cachetools and pyglib under platform/bq/third_party,
# read 2026-11-18). A missing or empty file fails the build. The
# fetches depend only on the pins they read, so they come first, in a layer a source change
# reuses; the assembly reads the other stages' outputs after them.
FROM tools AS notices
ARG BUN_VERSION
ARG MISE_VERSION
ARG JJ_TOOL
ARG GH_TOOL
ARG GH_STACK_TOOL
ARG UV_VERSION
ARG AWS_CLI_VERSION
ARG KUBECTL_VERSION
ARG YQ_VERSION
ARG GO_VERSION
ARG GOPLS_VERSION
COPY --from=plugin-deps /out/codegraph-node-version /in/codegraph-node-version
COPY packages/daemon/docker/language-servers/package.json /in/language-servers/package.json
COPY packages/daemon/docker/codegraph/package.json /in/codegraph/package.json
# The twenty-four reads below (seventeen LICENSE/NOTICE fetches, seven HEAD checks) are independent
# of each other and left sequential rather than backgrounded: this whole RUN is the cached layer
# the stage comment above describes (a source change, not a pin bump, reuses it, so it rebuilds
# rarely), and running unauthenticated GitHub raw-content requests in parallel risks tripping its
# secondary rate limit for a build-time saving that doesn't matter on a layer this cold.
# Every one of the twenty-four is a curl, so they carry curl's own retry rather than the wrapper and
# the RUN reads one way: 8 attempts whose waits double from a second, the last starting 127 s after
# the first, each transfer bounded at 60 s — a LICENSE file is a few kilobytes and a HEAD check is
# a set of headers — and -f, so a persistent 504 fails the build instead of writing the error body
# into a notices file.
RUN set -eu; \
    omp_pin="$(cat /omp-pin)"; omp_repo="${omp_pin#github:}"; omp_repo="${omp_repo%@*}"; \
    omp_tag="v${omp_pin##*@}"; \
    jj_repo="${JJ_TOOL#github:}"; jj_repo="${jj_repo%@*}"; jj_tag="v${JJ_TOOL##*@}"; \
    gh_tag="v${GH_TOOL#gh@}"; \
    stack_repo="${GH_STACK_TOOL#github:}"; stack_repo="${stack_repo%@*}"; stack_tag="v${GH_STACK_TOOL##*@}"; \
    codegraph_node_tag="$(cat /in/codegraph-node-version)"; \
    pin() { sed -n "s|^ *\"$2\": \"\([^\"]*\)\".*|\1|p" "/in/$1/package.json"; }; \
    typescript_version=$(pin language-servers typescript); tsls_version=$(pin language-servers typescript-language-server); pyright_version=$(pin language-servers pyright); \
    codegraph_version=$(pin codegraph @colbymchenry/codegraph); \
    : "${typescript_version:?typescript pin not read from /in/language-servers/package.json}"; : "${tsls_version:?typescript-language-server pin not read from /in/language-servers/package.json}"; : "${pyright_version:?pyright pin not read from /in/language-servers/package.json}"; : "${codegraph_version:?@colbymchenry/codegraph pin not read from /in/codegraph/package.json}"; \
    printf 'omp_repo=%s\nomp_tag=%s\njj_repo=%s\njj_tag=%s\ngh_tag=%s\nstack_repo=%s\nstack_tag=%s\ncodegraph_node_tag=%s\ncodegraph_version=%s\ntypescript_version=%s\ntsls_version=%s\npyright_version=%s\n' \
      "$omp_repo" "$omp_tag" "$jj_repo" "$jj_tag" "$gh_tag" "$stack_repo" "$stack_tag" "$codegraph_node_tag" "$codegraph_version" \
      "$typescript_version" "$tsls_version" "$pyright_version" > /in/tags.env; \
    gh_raw_url() { printf 'https://raw.githubusercontent.com/%s/%s/%s' "$1" "$2" "$3"; }; \
    fetch() { curl -fsSL --retry 7 --retry-all-errors --max-time 60 "$(gh_raw_url "$1" "$2" "$3")" -o "/in/$4"; }; \
    head_check() { curl -fsSI --retry 7 --retry-all-errors --max-time 60 -o /dev/null "$(gh_raw_url "$1" "$2" "$3")"; }; \
    fetch oven-sh/bun "bun-v${BUN_VERSION}" LICENSE.md bun-LICENSE.md; \
    fetch "$omp_repo" "$omp_tag" LICENSE omp-LICENSE; \
    fetch "$omp_repo" "$omp_tag" packages/coding-agent/THIRD-PARTY-NOTICES.txt omp-coding-agent-THIRD-PARTY-NOTICES.txt; \
    fetch "$omp_repo" "$omp_tag" packages/natives/THIRD-PARTY-NOTICES.txt omp-natives-THIRD-PARTY-NOTICES.txt; \
    fetch "$jj_repo" "$jj_tag" LICENSE jj-LICENSE; \
    fetch cli/cli "$gh_tag" LICENSE gh-LICENSE; \
    fetch "$stack_repo" "$stack_tag" LICENSE gh-stack-LICENSE; \
    fetch jdx/mise "${MISE_VERSION}" LICENSE mise-LICENSE; \
    fetch astral-sh/uv "$UV_VERSION" LICENSE-APACHE uv-LICENSE-APACHE; \
    fetch astral-sh/uv "$UV_VERSION" LICENSE-MIT uv-LICENSE-MIT; \
    fetch aws/aws-cli "$AWS_CLI_VERSION" LICENSE.txt aws-cli-LICENSE.txt; \
    fetch kubernetes/kubernetes "v${KUBECTL_VERSION}" LICENSE kubectl-LICENSE; \
    fetch mikefarah/yq "v${YQ_VERSION}" LICENSE yq-LICENSE; \
    fetch colbymchenry/codegraph "v${codegraph_version}" LICENSE codegraph-LICENSE; \
    fetch nodejs/node "$codegraph_node_tag" LICENSE codegraph-node-LICENSE; \
    fetch golang/go "go${GO_VERSION}" LICENSE go-LICENSE; \
    fetch golang/tools "gopls/v${GOPLS_VERSION}" LICENSE gopls-LICENSE; \
    crates() { \
      head_check "$1" "$2" Cargo.lock; \
      printf '%s links Rust crates whose license texts its release does not ship. They are the packages listed in\nhttps://github.com/%s/blob/%s/Cargo.lock, the lockfile of the release this image installs; the\nlicense of each is published with it on crates.io.\n' "$3" "$1" "$2" > "/in/$4"; \
    }; \
    crates "$jj_repo" "$jj_tag" jj jj-crates; \
    crates astral-sh/uv "$UV_VERSION" uv uv-crates; \
    crates jdx/mise "${MISE_VERSION}" mise mise-crates; \
    head_check golang/tools "gopls/v${GOPLS_VERSION}" gopls/go.mod; \
    printf 'gopls links Go modules whose license texts its binary does not carry. They are the modules listed in\nhttps://github.com/golang/tools/blob/gopls/v%s/gopls/go.mod, the module file of the release this image builds; the\nlicense of each is published with its source and shown on pkg.go.dev.\n' "$GOPLS_VERSION" > /in/gopls-modules; \
    head_check kubernetes/kubernetes "v${KUBECTL_VERSION}" go.mod; \
    printf 'kubectl links Go modules whose license texts its binary does not carry. They are the modules listed in\nhttps://github.com/kubernetes/kubernetes/blob/v%s/go.mod, the module file of the release this image installs.\nThe k8s.io modules that file replaces with ./staging/src (kubectl itself, client-go, apimachinery, cli-runtime\nand component-base among them) are source of the Kubernetes release itself, each with its license file under\nhttps://github.com/kubernetes/kubernetes/tree/v%s/staging/src/k8s.io; the license of every other module is in\nhttps://github.com/kubernetes/kubernetes/tree/v%s/LICENSES/vendor.\n' "$KUBECTL_VERSION" "$KUBECTL_VERSION" "$KUBECTL_VERSION" > /in/kubectl-modules; \
    head_check mikefarah/yq "v${YQ_VERSION}" go.mod; \
    printf 'yq links Go modules whose license texts its binary does not carry. They are the modules listed in\nhttps://github.com/mikefarah/yq/blob/v%s/go.mod, the module file of the release this image installs; the\nlicense of each is published with its source and shown on pkg.go.dev.\n' "$YQ_VERSION" > /in/yq-modules; \
    head_check "$stack_repo" "$stack_tag" go.mod; \
    printf 'gh-stack links Go modules whose license texts its binary does not carry. They are the modules listed in\nhttps://github.com/%s/blob/%s/go.mod, the module file of the release this image installs; the\nlicense of each is published with its source and shown on pkg.go.dev.\n' "$stack_repo" "$stack_tag" > /in/gh-stack-modules
# GCLOUD_VERSION, AZURE_CLI_PYTHON_VERSION and azure-cli's lock come in after the fetches, which
# read none of them; the assembly RUN below is what reads them. BuildKit puts every ARG declared
# before a RUN into that RUN's environment, and so into its cache key, and a COPY before a RUN
# changes the layer it runs on, so above the fetches a gcloud or CPython bump or a new lock would
# rerun the twenty-four reads.
ARG GCLOUD_VERSION
ARG AZURE_CLI_PYTHON_VERSION
COPY packages/daemon/docker/azure-cli/requirements.in packages/daemon/docker/azure-cli/requirements.txt /in/azure-cli/
COPY scripts/assemble-third-party-notices.sh /usr/local/bin/
COPY --from=debian-packages /debian-packages /in/debian-packages
COPY --from=go-notices /out/go-notices /in/go-notices
COPY --from=plugin /out/pi-envoy/dist/THIRD_PARTY_NOTICES /in/pi-envoy-notices
COPY --from=plugin /out/pi-legion/dist/THIRD_PARTY_NOTICES /in/pi-legion-notices
COPY --from=plugin /out/claude-envoy/dist/THIRD_PARTY_NOTICES /in/claude-envoy-notices
COPY --from=toolchain /opt/node/LICENSE /in/node-LICENSE
COPY --from=toolchain /out/licenses/aws-cli-THIRD_PARTY_LICENSES /in/aws-cli-THIRD_PARTY_LICENSES
COPY --from=toolchain /out/licenses/google-cloud-sdk-LICENSE /in/google-cloud-sdk-LICENSE
COPY --from=toolchain /out/licenses/google-cloud-sdk-python-LICENSE.txt /in/google-cloud-sdk-python-LICENSE.txt
COPY --from=toolchain /out/licenses/azure-cli-LICENSE.txt /in/azure-cli-LICENSE.txt
COPY --from=toolchain /out/licenses/azure-cli-python-LICENSE.txt /in/azure-cli-python-LICENSE.txt
COPY --from=toolchain /opt/language-servers/node_modules/typescript/LICENSE.txt /in/typescript-LICENSE.txt
COPY --from=toolchain /opt/language-servers/node_modules/typescript/ThirdPartyNoticeText.txt /in/typescript-ThirdPartyNoticeText.txt
COPY --from=toolchain /opt/language-servers/node_modules/typescript-language-server/LICENSE /in/typescript-language-server-LICENSE
COPY --from=toolchain /opt/language-servers/node_modules/pyright/LICENSE.txt /in/pyright-LICENSE.txt
COPY --from=toolchain /opt/language-servers/node_modules/pyright/dist/typeshed-fallback/LICENSE /in/pyright-typeshed-LICENSE
RUN set -eu; mkdir -p /out; . /in/tags.env; \
    azure_cli_version=$(sed -n 's/^azure-cli==//p' /in/azure-cli/requirements.in); \
    : "${azure_cli_version:?azure-cli pin not read from /in/azure-cli/requirements.in}"; \
    distributions="$(sed -n 's/^\([A-Za-z0-9._-]*==[^ ]*\).*/\1/p' /in/azure-cli/requirements.txt)"; \
    : "${distributions:?no name==version pin read from /in/azure-cli/requirements.txt}"; \
    printf 'azure-cli installs these Python distributions under /opt/azure-cli/env, each from its wheel, with the\nlicense file that wheel ships, where it ships one, and the METADATA that names its license, both in its\n*.dist-info directory under /opt/azure-cli/env/lib/%s/site-packages:\n%s\n' "python${AZURE_CLI_PYTHON_VERSION%.*}" "$distributions" > /in/azure-cli-distributions; \
    assemble-third-party-notices.sh /out/THIRD_PARTY_NOTICES \
      "Third-party software in the Legion worker image, with the license of each piece. Legion's own code is under the Apache License 2.0. The Debian packages the image installs, listed in the first section below, carry their terms in /usr/share/doc/<package>/copyright. npm packages installed unmodified carry their own license files beside them: the CodeGraph plugin and its dependencies under /opt/omp-plugins/node_modules, the CodeGraph CLI's dependencies under /opt/codegraph/lib/node_modules, and the language servers under /opt/language-servers/node_modules (their sections below). azure-cli's Python distributions carry their own license files in their *.dist-info directories under /opt/azure-cli/env, where a wheel ships one (azure-cli's sections below)." \
      "Debian packages installed in the image, one per line, each with its terms in /usr/share/doc/<package>/copyright" /in/debian-packages \
      "Go modules compiled into /opt/legion/bin/legion and /opt/legion/bin/agent-secrets" /in/go-notices \
      "npm packages inlined into the pi-envoy plugin, /opt/legion/pi-envoy (also its dist/THIRD_PARTY_NOTICES)" /in/pi-envoy-notices \
      "npm packages inlined into the pi-legion plugin, /opt/legion/pi-legion (also its dist/THIRD_PARTY_NOTICES)" /in/pi-legion-notices \
      "npm packages inlined into the claude-envoy plugin, /opt/legion/claude-envoy (also its dist/THIRD_PARTY_NOTICES)" /in/claude-envoy-notices \
      "Bun ${BUN_VERSION}, /usr/local/bin/bun: LICENSE.md of github.com/oven-sh/bun at bun-v${BUN_VERSION}" /in/bun-LICENSE.md \
      "Oh My Pi, /opt/omp and the native module it extracted into /home/legion/.omp/natives: LICENSE of github.com/${omp_repo} at ${omp_tag}" /in/omp-LICENSE \
      "Oh My Pi, /opt/omp: packages/coding-agent/THIRD-PARTY-NOTICES.txt of github.com/${omp_repo} at ${omp_tag}" /in/omp-coding-agent-THIRD-PARTY-NOTICES.txt \
      "Oh My Pi's native module, embedded in /opt/omp/bin/omp and extracted into /home/legion/.omp/natives: packages/natives/THIRD-PARTY-NOTICES.txt of github.com/${omp_repo} at ${omp_tag}" /in/omp-natives-THIRD-PARTY-NOTICES.txt \
      "jj, /usr/local/bin/jj: LICENSE of github.com/${jj_repo} at ${jj_tag}" /in/jj-LICENSE \
      "jj: the Rust crates it links" /in/jj-crates \
      "GitHub CLI, /usr/local/bin/gh: LICENSE of github.com/cli/cli at ${gh_tag}" /in/gh-LICENSE \
      "gh-stack, the gh stack extension, /opt/legion/gh-extensions/gh-stack: LICENSE of github.com/${stack_repo} at ${stack_tag}" /in/gh-stack-LICENSE \
      "gh-stack: the Go modules it links" /in/gh-stack-modules \
      "mise ${MISE_VERSION}, /usr/local/bin/mise: LICENSE of github.com/jdx/mise at ${MISE_VERSION}" /in/mise-LICENSE \
      "mise: the Rust crates it links" /in/mise-crates \
      "uv and uvx, /usr/local/bin/uv and uvx: LICENSE-APACHE of github.com/astral-sh/uv at ${UV_VERSION} (uv is offered under Apache-2.0 or MIT)" /in/uv-LICENSE-APACHE \
      "uv: LICENSE-MIT of github.com/astral-sh/uv at ${UV_VERSION}" /in/uv-LICENSE-MIT \
      "uv: the Rust crates it links" /in/uv-crates \
      "Node.js, /opt/node (with npm and corepack): the LICENSE its release archive ships" /in/node-LICENSE \
      "Node.js ${codegraph_node_tag}, /opt/codegraph/node (the runtime the CodeGraph CLI bundles): LICENSE of github.com/nodejs/node at ${codegraph_node_tag}" /in/codegraph-node-LICENSE \
      "AWS CLI v2, /opt/aws-cli: LICENSE.txt of github.com/aws/aws-cli at ${AWS_CLI_VERSION}" /in/aws-cli-LICENSE.txt \
      "AWS CLI v2: the THIRD_PARTY_LICENSES its installer archive ships" /in/aws-cli-THIRD_PARTY_LICENSES \
      "Google Cloud SDK ${GCLOUD_VERSION}, /opt/google-cloud-sdk (gcloud and bq at /usr/local/bin): the LICENSE its release archive ships; the libraries it vendors under /opt/google-cloud-sdk/lib/third_party, platform/bq/third_party and platform/gsutil/third_party carry the license files the archive ships beside them" /in/google-cloud-sdk-LICENSE \
      "Google Cloud SDK: the LICENSE.txt of the CPython it bundles, /opt/google-cloud-sdk/platform/bundledpythonunix" /in/google-cloud-sdk-python-LICENSE.txt \
      "azure-cli ${azure_cli_version}, /opt/azure-cli (az at /usr/local/bin): the LICENSE.txt its wheel ships" /in/azure-cli-LICENSE.txt \
      "azure-cli: the LICENSE.txt of CPython ${AZURE_CLI_PYTHON_VERSION}, /opt/azure-cli/python, which uv installs" /in/azure-cli-python-LICENSE.txt \
      "azure-cli: the Python distributions it installs" /in/azure-cli-distributions \
      "kubectl v${KUBECTL_VERSION}, /usr/local/bin/kubectl: LICENSE of github.com/kubernetes/kubernetes at v${KUBECTL_VERSION}" /in/kubectl-LICENSE \
      "kubectl: the Go modules it links" /in/kubectl-modules \
      "yq v${YQ_VERSION}, /usr/local/bin/yq: LICENSE of github.com/mikefarah/yq at v${YQ_VERSION}" /in/yq-LICENSE \
      "yq: the Go modules it links" /in/yq-modules \
      "CodeGraph CLI, /opt/codegraph: LICENSE of github.com/colbymchenry/codegraph at v${codegraph_version}" /in/codegraph-LICENSE \
      "Go ${GO_VERSION}, /opt/go (go and gofmt at /usr/local/bin): LICENSE of github.com/golang/go at go${GO_VERSION}" /in/go-LICENSE \
      "gopls v${GOPLS_VERSION}, /usr/local/bin/gopls: LICENSE of github.com/golang/tools at gopls/v${GOPLS_VERSION}" /in/gopls-LICENSE \
      "gopls: the Go modules it links" /in/gopls-modules \
      "TypeScript ${typescript_version}, /opt/language-servers/node_modules/typescript (tsc and tsserver at /usr/local/bin): the LICENSE.txt its npm package ships" /in/typescript-LICENSE.txt \
      "TypeScript: the ThirdPartyNoticeText.txt its npm package ships" /in/typescript-ThirdPartyNoticeText.txt \
      "typescript-language-server ${tsls_version}, /opt/language-servers/node_modules/typescript-language-server (/usr/local/bin/typescript-language-server): the LICENSE its npm package ships" /in/typescript-language-server-LICENSE \
      "pyright ${pyright_version}, /opt/language-servers/node_modules/pyright (pyright and pyright-langserver at /usr/local/bin): the LICENSE.txt its npm package ships" /in/pyright-LICENSE.txt \
      "pyright: the LICENSE of the typeshed stubs it bundles, /opt/language-servers/node_modules/pyright/dist/typeshed-fallback" /in/pyright-typeshed-LICENSE

# ------------------------------------------------------------------------------------------------
# webkit-check (build only): the check R7 of dispatch://INFRA-3257 asks for. It downloads the
# WebKit build of the Playwright packages/dispatch locks (about 100 MB, from Playwright's CDN) and
# launches it once, headless, through Playwright's own host validation
# (packages/daemon/docker/webkit-launch.mjs), so a pull request that moves packages/dispatch to a
# Playwright whose WebKit needs a library the runtime-base apt line lacks fails this build:
# ci.yaml's worker-image job runs on bun.lock and packages/dispatch/package.json, and gates ci-ok.
# It runs as the runtime user, as a role does. It is a stage of its own because Playwright 1.64.1
# checks that download against no hash, so nothing it writes may reach the image: in the runtime
# stage that user owns /home/legion, and a file the download wrote outside the scratch directory
# would ship. Only the marker crosses: /out/webkit-check, the launch script's one line
# (`webkit: Playwright <version> launched WebKit <version>`), in a directory root makes for the
# runtime user before the switch; the RUN prints the line too. It reads Node from the toolchain
# stage's /opt/node and playwright-core from the plugin-deps stage's /out/playwright-core, both by bind
# mount, and HOME, TMPDIR and PLAYWRIGHT_BROWSERS_PATH point into a scratch directory the step
# removes, so the browser is downloaded there. BuildKit reads a bind mount's source by content, so
# the stage reruns only when runtime-base or its packages, the Node pin, the locked Playwright or
# webkit-launch.mjs changes — never for a change to the plugins, the skills, the toolchain step or
# `legion`. BuildKit builds only the stages the target stage reaches, and the runtime stage's bind
# mount of this /out is what reaches this one: that mount is what makes BuildKit build the stage at
# all, so the gate stays as long as the mount does.
FROM runtime-base AS webkit-check
RUN install -d -m 0755 -o 1000 -g 1000 /out
USER 1000:1000
# The download is this stage's one fetch, and it retries: WebKit 2370's build for Debian 13,
# webkit-debian-13.zip, 101,343,399 bytes from Playwright's two hosts, cdn.playwright.dev and
# playwright.download.prss.microsoft.com, takes 300 s an attempt, this file's bound for a large
# archive and the one the download-retry job gives `bunx playwright install chromium`. It came down
# and unpacked in 20.9 s cold in an issue pod (2 vCPU under gVisor, 2026-11-19), and this whole RUN
# took 9.8 s in the worker-image build of cybermonklabs/legion#4377 (job 135176358267). Through a
# proxy refusing every host it exited 1 after Playwright's own five attempts over those two hosts,
# in 9.8 s, so the retry has a failure to retry. The launch reads the download and nothing else,
# and stays bare.
RUN --mount=type=bind,from=plugin-deps,source=/out/playwright-core,target=/tmp/playwright-core \
    --mount=type=bind,from=toolchain,source=/opt/node,target=/opt/node \
    --mount=type=bind,source=packages/daemon/docker/webkit-launch.mjs,target=/tmp/webkit-launch.mjs \
    retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; }; \
    set -eu; \
    scratch="$(mktemp -d)"; \
    export HOME="$scratch" TMPDIR="$scratch" PLAYWRIGHT_BROWSERS_PATH="$scratch/browsers"; \
    retry 300 "webkit download" /opt/node/bin/node /tmp/playwright-core/cli.js install webkit; \
    /opt/node/bin/node /tmp/webkit-launch.mjs /tmp/playwright-core > /out/webkit-check; \
    cat /out/webkit-check; \
    rm -rf "$scratch"

# ------------------------------------------------------------------------------------------------
# runtime: the final stage, the image itself, on runtime-base.
FROM runtime-base AS runtime
LABEL org.opencontainers.image.source=https://github.com/cybermonklabs/legion
# The toolchain goes in first, on runtime-base. A layer is reused only while every layer before it is,
# so this stage copies in the order its inputs change, least often first (main commits in the 30
# days to 2026-11-22): the toolchain pins and gopls (at most two each), Bun, jj, gh and mise, the
# CodeGraph tool's lockfiles (two), Oh My Pi (.omp-pin: nine), legion-testpg (nine), the probe
# layer, the module cache (go.work's module files: 40), the toolchain step, the Claude Code pin, the
# WebKit marker, the notices, then the plugins (their source: 40 of 180 main commits on 2026-11-21
# and 22) and `legion`, which changes on every commit. A toolchain pin bump reruns the probe layer
# and its natives, which an Oh My Pi bump reruns anyway; an Oh My Pi, module or plugin change reruns
# none of the toolchain. It lands outside HOME, in /opt and /usr/local/bin, so no volume a pod
# mounts under HOME shadows it, and /usr/local/bin is on the image PATH and on every pod's
# (imagePath, packages/daemon/internal/runtime/sandbox/names.go). /opt/go is the Go toolchain,
# /opt/language-servers carries the language servers (node_modules), /opt/google-cloud-sdk is the
# Google Cloud SDK, /opt/azure-cli is azure-cli with the CPython it runs on, /opt/chromium is the
# pinned Chrome for Testing, and /out/bin holds every launcher's link; gopls comes from the gopls
# stage, which reads no source, so its binary — and this layer — changes only with a pin.
COPY --from=toolchain /opt/node /opt/node
COPY --from=toolchain /opt/language-servers /opt/language-servers
COPY --from=toolchain /opt/go /opt/go
COPY --from=toolchain /opt/aws-cli /opt/aws-cli
COPY --from=toolchain /opt/google-cloud-sdk /opt/google-cloud-sdk
COPY --from=toolchain /opt/azure-cli /opt/azure-cli
COPY --from=toolchain /out/bin/ /usr/local/bin/
COPY --from=toolchain /out/chromium /opt/chromium
COPY --from=gopls /out/gopls /usr/local/bin/gopls
# Pinned Bun: the bun stage's binary, which the plugin stages build and pack with (the copy of
# oven/bun:${BUN_VERSION}-slim).
COPY --from=bun /usr/local/bin/bun /usr/local/bin/bun
RUN ln -s bun /usr/local/bin/bunx
COPY --from=tools /opt/tools/jj /usr/local/bin/jj
COPY --from=tools /opt/tools/gh /usr/local/bin/gh
# gh's extensions, root's like the rest of /opt/legion, so no role changes the copy its siblings
# run: gh-stack, the `gh stack` extension ARG GH_STACK_TOOL pins. gh reads an extension only from
# its own data directory, which in a pod is under the issue's scratch, so the shim links each one
# there before Oh My Pi starts (podsafety.EnsureGhExtensions, from podsafety.GhExtensionsDir).
COPY --from=tools /opt/tools/gh-extensions /opt/legion/gh-extensions
# mise, the binary the tools stage installed and checked against MISE_SHA256, so there is no pin of
# its own here, for the two scripts a role runs that resolve the Oh My Pi pin with `mise where` and
# `mise x`: scripts/e2e/lib/install-plugin-profile.sh and the skill-scenario rig
# (packages/pi-legion/scripts/skill-scenarios/rig.sh). The image does not seed mise's store: the rig
# installs the pin into the role's own MISE_DATA_DIR at its first run, which downloads it from
# GitHub (mise reads MISE_GITHUB_TOKEN when it is set).
COPY --from=tools /usr/local/bin/mise /usr/local/bin/mise
# CodeGraph CLI (@colbymchenry/codegraph): the self-contained per-platform bundle alone (bundled
# Node runtime + app), never the npm package's own launcher shim — see the plugin-deps stage's comment.
COPY --from=plugin-deps /out/codegraph /opt/codegraph
# The Oh My Pi plugins' frozen install (the plugin-deps stage's), root's like the plugins below, so no
# role can change the CodeGraph plugin code the profile's link (step 3 below) resolves to.
COPY --from=plugin-deps /opt/omp-plugins /opt/omp-plugins
COPY --from=tools /opt/omp /opt/omp
# legion-testpg: root-owned and mode 0555, like every other program on the image's PATH, so a role
# cannot rewrite the script the roles container's launcher runs.
COPY --chmod=555 packages/daemon/docker/legion-testpg /usr/local/bin/legion-testpg
# /var/run/legion/testpg.log is the mount point an issue pod's role-init binds the pod's test
# server log at, read-only (packages/daemon/internal/launcher, roleinit_linux.go; legion-testpg
# says what the server logs there): inside a role's namespaces the role is the runtime user, which
# can create nothing under the root-owned /var/run, so the image makes the file, as it makes the
# role mount points beside it. It is empty, root's and mode 0444: a role of a pod that serves no
# test database reads an empty file there. The final step refuses an image where it is anything
# else.
RUN install -m 0444 -o root -g root /dev/null /var/run/legion/testpg.log
# /etc/gitconfig holds the four pack settings of every git in an issue pod or a repository cache
# (packWindowConfig, packages/daemon/internal/workspace/config.go): pack data mapped 32 MiB at a
# time up to 256 MiB, and 64 MiB each of pack-objects' window memory and of cached delta bases.
# git's defaults map 1 GiB windows, and gVisor counts every window a process touched whole as the
# pod's memory. This file is for provisioning's git in the init container: its runner removes the
# role environment's GIT_CONFIG pairs, which carry the same four to every role's git, and the
# shared clone's own configuration, which provisioning writes them into, does not hold them yet
# when that git first clones it. A Dockerfile cannot read the Go list, so the values are written
# out here, and the final step fails the build when the file holds any others.
RUN /usr/bin/git config --system core.packedGitWindowSize 32m \
    && /usr/bin/git config --system core.packedGitLimit 256m \
    && /usr/bin/git config --system pack.windowMemory 64m \
    && /usr/bin/git config --system core.deltaBaseCacheLimit 64m
# Keep Debian's chromium package for its shared-library dependency closure, but make its vulnerable
# executable unreachable and expose the pinned Chrome for Testing binary under the usual name.
RUN rm -f /opt/codegraph/node /usr/bin/chromium /usr/lib/chromium/chromium \
    && ln -s /opt/node/bin/node /opt/codegraph/node \
    && ln -s /opt/chromium/chrome /usr/local/bin/chromium
# OMP_PROFILE=legion: the isolated profile the CodeGraph plugin is linked into (plugins resolve to
# /home/legion/.omp/profiles/legion/plugins/node_modules); the Envoy and Legion plugins stay unlinked,
# named by path (step 3 below). LEGION_OMP_PATH: how `legion probe-image`
# — and a daemon pointed at this image — names the OMP executable without mise. HOME is explicit
# because OMP's DirResolver derives the profile root from it. USER=legion: the runtime user is uid
# 1000, `legion` (created in runtime-base), and a process that reads USER finds it there (the
# skill-scenario rig's scripts run under `set -u`); as image ENV, a deployment's
# runtime.kubernetes.pod.env may not set it (sandbox/operatorpod.go ImageEnv). DO_NOT_TRACK=1:
# CodeGraph's telemetry
# and update-check opt-out (ranked above CODEGRAPH_TELEMETRY, above stored config, above
# default-on) — every worker's own `codegraph` call, and the warm-up the tmux daemon runs outside
# this image, must never phone home for an automatic, non-opt-in tool. No PUPPETEER_EXECUTABLE_PATH:
# Oh My Pi picks its browser by that variable first, else the first of google-chrome-stable,
# google-chrome, chromium, chromium-browser and chrome on PATH (then /usr/bin/chromium and its
# neighbours), and downloads Chrome for Testing only with none of those — so the image's chromium on
# PATH is the one every worker gets with no download, and the capability check runs that same
# resolution (`browser`, image.go). The variable stays the operator's: every variable this ENV sets
# is one runtime.kubernetes.pod.env is refused (sandbox/operatorpod.go ImageEnv), and an operator
# pod naming another browser is exactly what the probe should check, not refuse.
ENV OMP_PROFILE=legion \
    LEGION_OMP_PATH=/opt/omp/bin/omp \
    HOME=/home/legion \
    USER=legion \
    DO_NOT_TRACK=1 \
    PATH=/opt/legion/bin:/opt/omp/bin:/opt/codegraph/bin:/usr/local/bin:/usr/bin:/bin
# Numeric uid:gid (user `legion`, created above) so Kubernetes `runAsNonRoot` can verify it from the
# image alone.
USER 1000:1000
WORKDIR /home/legion
# 1. Every copied binary runs on this base (they were built or fetched on bookworm; trixie's glibc is
#    newer, so they do — proven here, not assumed), and git is new enough for jj.
#    mise answers --version with MISE_CACHE_DIR and MISE_DATA_DIR in the step's scratch directory:
#    the call writes its version check's cache and a migrations marker into those two, which under
#    HOME this layer would keep.
# 2. jj's git backend must accept the git on this image: `jj git clone` of a scratch bare repository
#    runs jj's `git fetch --porcelain` subprocess exactly as `legion workspace-init` does in the init
#    container, with no network. A git older than 2.42 fails it (`Git does not recognize required
#    option: porcelain`), which is how bookworm's 2.39.5 shipped in an image that passed every probe:
#    `legion probe-image` never runs jj, and the daemon host's own git is newer.
# 3. Link the CodeGraph plugin's frozen install, /opt/omp-plugins/node_modules/@bopstack/pi-codegraph,
#    into the legion profile: `omp plugin link` symlinks that directory into the profile's
#    plugins/node_modules and records it enabled in omp-plugins.lock.json, resolving nothing from a
#    registry (an npm spec given to `omp plugin install` runs `bun install` in the profile with no
#    lockfile, which is how a dependency published moments before a build once failed it). Link it
#    and nothing else: /opt/legion/pi-envoy and /opt/legion/pi-legion are the two roots a
#    pod names as Oh My Pi's explicit extensions (--extension, discovery on;
#    packages/daemon/internal/runtime/sandbox/manifest.go agentArgv), and must never be linked into
#    the profile too. Oh My Pi loads an installed plugin and an explicit one by their own paths, so a
#    plugin that is both loads twice — its factory runs twice in every role. The pod lane's probe in
#    the final step refuses an image whose pi-envoy loads twice (the Envoy interface counts its
#    publishers); a re-linked pi-legion alone it cannot see, since that plugin's load marker is one
#    object the second load overwrites — so the two links come and go together, here. Loading both
#    from the profile instead, as a pane does, would stop the image probe certifying the two roots by
#    path (bootgate.go's pod lane reads the explicit root's manifest for the contract and holds
#    pi-envoy to its root): explicit roots and an unlinked profile are one decision. This is OMP's
#    first run in the image, so the CodeGraph link also extracts OMP's native module from the
#    binary (no network) into /home/legion/.omp/natives/<version>-<digest prefix>/ (the version and
#    the first 12 hexadecimal digits of the embedded addons' digest; getNativesDir in
#    @oh-my-pi/pi-natives native/natives-dir.js names .omp/natives, since the build sets none of
#    PI_NATIVES_DIR, OMP_DOWNLOAD_ROOT, XDG_CACHE_HOME and OMP_CONFIG_ROOT) — the build's success is
#    the check. From 18.11.0 OMP writes only the variant the CPU selects, so this is the build runner's
#    (ubuntu-24.04, AVX2), 124 MiB; this layer ships it, so a pod whose node selects the same
#    variant extracts nothing, while a node that selects the other has each role write that one into
#    its own home overlay's upper layer, on the pod's role-homes volume, at its first OMP run: up to
#    six copies in an issue pod. The probes in the final step never wait on it.
# Any failure fails the build: a broken image never publishes. The pinned omp must list
# --session-id in its help, which the fork's 18.18.6 builds do from 18.18.6-sami.20261117-074351
# on: a pin older than that, such as 18.18.6-sami.20261117-013345, fails here. Nothing of Legion's
# passes the flag yet; the check is a floor on the pin and does not prove the session leases that
# OMP_SESSION_EXCLUSIVE relies on, which come with 18.18.6-sami.20261118-101239.
# Its `mise --version` runs on empty cache and data directories, and CI=true keeps it local, for
# the reason the tools stage's own `mise --version` gives: without it, mise asks mise.jdx.dev for a
# newer release (dispatch://INFRA-1545, round 68).
RUN set -eu; \
    bun --version; omp --version; omp --help | grep -q -e '--session-id'; jj --version; gh --version; git --version; codegraph --version; \
    scratch="$(mktemp -d)"; \
    MISE_CACHE_DIR="$scratch/mise-cache" MISE_DATA_DIR="$scratch/mise-data" CI=true mise --version; \
    git init --quiet --bare "$scratch/origin.git"; \
    jj git clone "$scratch/origin.git" "$scratch/clone"; \
    rm -rf "$scratch"; \
    omp plugin link /opt/omp-plugins/node_modules/@bopstack/pi-codegraph; \
    rm -rf /home/legion/.omp/profiles/legion/logs
# The warm Go module cache: the download cache the go-modules stage filled from go.work's module files
# (its zips, .mod and .info files and their hashes). It lands at the module cache a go with no
# GOMODCACHE in its environment reads, /home/legion/go/pkg/mod: HOME is /home/legion, and GOPATH is
# its default, $HOME/go. The extracted module trees stay out of the image. go extracts what a build
# needs from these zips, and a role's GOMODCACHE is the issue volume anyway (the toolchain stage's
# go.env paragraph), so the image carries the one form both uses read: 380 MiB of download cache,
# 419 module zips with their .mod, .info and hash files, rather than the 2.2 GiB of the whole
# module cache, which is that download cache and the extracted trees together.
# --chown makes the cache the runtime user's. Its files are mode 0644 in mode 0755 directories,
# which is what lets a role add a module the cache does not hold. The read-only 0444 files and 0555
# directories of a module cache are in the extracted trees, which this is not. The COPY comes after
# the probe layer, which reads no module, and before the toolchain step, which checks the cache, in
# the order the toolchain's comment gives.
COPY --from=go-modules --chown=1000:1000 /gomodcache/cache/download /home/legion/go/pkg/mod/cache/download
# Corepack resolves a project with no `packageManager` to the pnpm and yarn it ships as defaults,
# never to npm's newest release, so every place the image runs (a pod, `docker run`, the check below)
# gets one version until the Node pin moves. It is image ENV, not a pod variable, for that reason.
# AWS_CA_BUNDLE makes the AWS CLI use Debian's current system roots. Its vendored bundle has no
# trust path for example.com's current chain after the compromised AAA root is removed, while the
# system bundle trusts that chain and holds neither the emSign nor the AAA root.
# CGO_ENABLED=0 keeps cgo off by default now that gcc is on PATH (the apt line above says why gcc is
# there), for every process that inherits the image's environment: the bash tool, subagents, and go
# test's test binaries. The toolchain stage's go.env line alone would not reach two of them: a go/build
# reader such as go/types' source importer (packages/daemon/internal/supervise/table_test.go) reads only
# the process environment, never go.env, so this ENV is what makes its cgo setting match the go
# command's; and a toolchain Go switches to for GOTOOLCHAIN or a go.mod `toolchain` line has its own
# go.env, which lacks the line. A role opts in per command, `CGO_ENABLED=1 go test -race …`;
# `go env -w CGO_ENABLED=1` does not override it, since Go ranks the environment above its go env file;
# and as image ENV a deployment's runtime.kubernetes.pod.env may not set it (sandbox/operatorpod.go
# ImageEnv).
ENV AWS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt COREPACK_DEFAULT_TO_LATEST=0 CGO_ENABLED=0
# The toolchain step: every toolchain command runs as the runtime user from the image PATH, the
# corepack shims fetching their shipped default pnpm and yarn, with TMPDIR, COREPACK_HOME,
# XDG_CONFIG_HOME, CLOUDSDK_CONFIG and AZURE_CONFIG_DIR in a scratch directory the step removes, so
# the layer keeps nothing (go, gofmt and
# gopls write Go telemetry counters under the user config directory on every run, local mode being
# Go's default; XDG_CONFIG_HOME is where they look for it). The image's own Python is checked here,
# python3 --version, and so is the PyYAML the gates import, `python3 -c 'import yaml'`;
# uv still installs each project's own at run time. `go version` alone would pass
# with a GOROOT go cannot find, and every `go build` would then fail, so the step requires GOROOT to
# resolve to /opt/go through the /usr/local/bin/go symlink. gcc answers --version and is checked beside
# go, whose cgo (and so whose race detector) needs it. The step then reads cgo's default with
# CGO_ENABLED removed from the environment (XDG_CONFIG_HOME in scratch, so no `go env -w` file is
# read), where only /opt/go/go.env's line holds it at 0, so the build fails if that line, the default
# a scrubbed environment depends on, is ever lost. It reads GOPROXY the same way, with GOPROXY
# removed from the environment, and requires the line the toolchain stage wrote into the same
# go.env: the warm module cache reaches a role through that line alone. It then requires the cache
# to carry no `@v/list` and no `@latest` file. One of those files would answer every version query
# of every repository a role opens with the versions this image pinned, and the fall-through to
# proxy.golang.org happens on an error alone (the go-modules stage says more). It then holds the cache
# itself to the runtime user and to a writable download directory. A role that cannot write there
# cannot add a module the cache does not hold. The step prints how many modules the cache carries.
# gofmt takes no --version; formatting a
# line from stdin is its check. pyright-langserver takes none either (it opens its LSP connection
# first and refuses without --stdio), so `pyright --version`, the same package's CLI, answers for it.
# Being its own layer above `legion`, it reruns only when the toolchain or a layer before it changes,
# so a commit that only rebuilds `legion` fetches nothing from a registry. pnpx is `pnpm dlx`, which
# takes no --version; its --help, whose first line names the pnpm version, is the check. `kubectl
# version --client` reports the client alone: it reads no kubeconfig and contacts no API server. socat
# answers -V, not --version, with its compiled-in features after the version line; the step prints
# that line alone. ssh-keygen takes no --version (its -V is the certificate-validity option), so it
# generates a throwaway ed25519 key in the scratch directory, which -l fingerprints and the step
# removes. flock answers --version; it is on this line because an installing launch's rendered
# install serialises on the issue's shared bun install cache with it.
# psql, redis-cli and sqlite3 answer --version, psql's being the Debian Perl wrapper, which resolves
# because perl-base is in the image; nc takes no --version, its -h printing usage and exiting 0, and
# it is checked under `nc`, the name the alternatives system installs, never nc.openbsd; and dig -v
# prints its version on stdout, measured in a pod of this image (`dig -v 2>/dev/null` prints
# `DiG 9.20.30-1~deb13u1-Debian`, `dig -v 2>&1 >/dev/null` prints nothing), so the step takes it as
# it comes. `unzip -v` prints its banner and exits 0, while yq is checked with `--version`, not
# `-v`, which is mikefarah's verbose flag: it emits `level=DEBUG` lines and reads stdin rather than
# printing a version.
# tmux answers -V. busybox takes no --version: its --help, whose first line names the version
# (`BusyBox v1.37.0 (Debian 1:1.37.0-6+b9) multi-call binary.`, measured on trixie's package),
# exits 0, so the step prints that line alone, as it does pnpx's. `gcloud --version`, `bq version`
# and `az version` print their versions with their configuration in the scratch directory
# (CLOUDSDK_CONFIG, AZURE_CONFIG_DIR), so nothing lands in the runtime user's home; `az version`
# fetches the newest versions to compare on every call (packages/daemon/docker/azure-cli/az says
# when azure-cli does). The step then reads the SDK's two installation properties back through
# gcloud, from a configuration directory that sets neither, so each value it requires — Debian's
# root bundle for core/custom_ca_certs_file, `true` for component_manager/disable_update_check —
# answers from the installation the toolchain stage configured. gcloud stores a property as the
# text `config set` was given and `config get-value` prints that text back, so the step compares
# the `true` the toolchain stage set (measured with SDK 594.0.0: `true` prints `true`, `True`
# prints `True`).
# legion-testpg runs here for real, not as a version print: it initdbs a cluster in the scratch
# directory as the runtime user, starts it, makes a database from template1 and reads pgvector back
# out of it, then stops it with `legion-testpg stop`, a fast shutdown it waits for — a postmaster
# killed instead would leave its shutdown checkpoint running and the `rm -rf` below would race it.
# It then makes the server's log mode 000, as a role that remounts its view of the bound log
# read-write can, and runs `ensure` again over the stopped cluster: the restart must put the log
# back to mode 600 and serve the DSN, where a start that wrote the log as it found it would fail at
# its first write, and that server is stopped the same way. An issue pod's launcher runs the same
# command, so a build whose server package, extension or script no longer works fails here rather
# than at every role's start in every pod. The DSN is the one this image's own launcher serves to
# every role of an issue pod: a loopback host, no password, and legion_test for both the login and
# the database — the non-superuser role `ensure` creates, not the cluster's own superuser, and the
# database `ensure` creates with that role as its owner.
# The corepack shims are the fetches of this step that retry: each of pnpm, yarn, yarnpkg and pnpx
# pulls the version this corepack ships from the npm registry the first time it runs, a package of
# a few megabytes, so each retries at 120 s an attempt. `gcloud --version` and `bq version` read
# the image: through a proxy refusing every host, each exited 0 having asked it for nothing (SDK
# 594.0.0 in an issue pod, 2026-11-19), and `gcloud config get-value` reads the installation's
# properties. `az version` does reach the network, azcliprod.blob.core.windows.net, for the newest
# versions, and stays bare all the same: azure-cli bounds that read at 10 s itself, catches every
# error of it and exits 0 without it (get_latest_version_from_ame_storage in azure-cli 2.93.0's
# azure/cli/core/util.py; through that proxy it exited 0 in 2.4 s), so a retry around it would wait
# for a failure that never comes, which is why .github/scripts/check-download-retry.sh's rule 7
# refuses one around `mise x`. Every other command here reads the image.
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; }; \
    set -eu; \
    scratch="$(mktemp -d)"; \
    export TMPDIR="$scratch" COREPACK_HOME="$scratch/corepack" XDG_CONFIG_HOME="$scratch/config" \
      CLOUDSDK_CONFIG="$scratch/gcloud" AZURE_CONFIG_DIR="$scratch/azure"; \
    uv --version; uvx --version; node --version; npm --version; npx --version; corepack --version; \
    aws --version; kubectl version --client; yq --version; \
    gcloud --version; bq version; az version; \
    test "$(gcloud config get-value core/custom_ca_certs_file 2>/dev/null)" = /etc/ssl/certs/ca-certificates.crt; \
    test "$(gcloud config get-value component_manager/disable_update_check 2>/dev/null)" = true; \
    for shim in pnpm yarn yarnpkg; do retry 120 "corepack $shim" "$shim" --version; done; \
    retry 120 "corepack pnpx" pnpx --help > "$scratch/pnpx-help"; sed -n 1p "$scratch/pnpx-help"; \
    go version; goroot="$(go env GOROOT)"; echo "GOROOT: $goroot"; test "$goroot" = /opt/go; gcc --version; \
    test "$(env -u CGO_ENABLED go env CGO_ENABLED)" = 0; \
    test "$(env -u GOPROXY go env GOPROXY)" = 'file:///home/legion/go/pkg/mod/cache/download|https://proxy.golang.org|direct'; \
    modcache=/home/legion/go/pkg/mod/cache/download; \
    test -z "$(find "$modcache" \( -path '*/@v/list' -o -name '@latest' \) -print -quit)"; \
    test "$(stat -c '%U' "$modcache")" = legion; test -w "$modcache"; \
    printf 'go module cache: %s modules\n' "$(find "$modcache" -name '*.zip' | wc -l)"; \
    printf 'package main\n' | gofmt; gopls version; \
    typescript-language-server --version; tsc --version; pyright --version; \
    python3 --version; python3 -c 'import yaml'; curl --version; wget --version; jq --version; chromium --version; \
    socat -V > "$scratch/socat-V"; sed -n '/^socat version/p' "$scratch/socat-V"; \
    tmux -V; busybox --help > "$scratch/busybox-help"; sed -n 1p "$scratch/busybox-help"; \
    ssh-keygen -q -t ed25519 -N '' -f "$scratch/probe-key"; ssh-keygen -l -f "$scratch/probe-key.pub"; \
    flock --version; \
    psql --version; redis-cli --version; sqlite3 --version; nc -h; dig -v; unzip -v; \
    legion-testpg check; \
    probe_dsn='postgres://legion_test@127.0.0.1:5433/legion_test?sslmode=disable'; \
    LEGION_TESTPG_DIR="$scratch/testpg" LEGION_TEST_PG_DSN="$probe_dsn" legion-testpg ensure; \
    psql -X -q "$probe_dsn" -c 'create database probe'; \
    psql -X -tAq 'postgres://legion_test@127.0.0.1:5433/probe?sslmode=disable' -c "select extname from pg_extension where extname = 'vector'" | grep -qx vector; \
    psql -X -q "$probe_dsn" -c 'drop database probe with (force)'; \
    LEGION_TESTPG_DIR="$scratch/testpg" legion-testpg stop; \
    chmod 000 "$scratch/testpg/server.log"; \
    LEGION_TESTPG_DIR="$scratch/testpg" LEGION_TEST_PG_DSN="$probe_dsn" legion-testpg ensure; \
    test "$(stat -c %a "$scratch/testpg/server.log")" = 600; \
    test "$(psql -X -tAq "$probe_dsn" -c 'select 1')" = 1; \
    LEGION_TESTPG_DIR="$scratch/testpg" legion-testpg stop; \
    rm -rf "$scratch"
# Claude Code's pin, packages/daemon/docker/claude-code's package.json and package-lock.json, at
# /opt/legion/claude-code (claudeCodePin, packages/daemon/internal/runtime/sandbox/names.go):
# root's, under the root-owned /opt/legion the final step checks, and all of Claude Code this
# image carries. A Claude role's start script copies both into its generation's directory on the
# issue's volume and runs `npm ci --ignore-scripts --omit=dev` from them there, then holds the
# installed binary's --version to this package.json's pin (claudeCodeBlock,
# packages/daemon/internal/runtime/sandbox/plugins.go). After the toolchain step, so a pin bump
# reruns no layer above it.
COPY packages/daemon/docker/claude-code/package.json packages/daemon/docker/claude-code/package-lock.json /opt/legion/claude-code/
# The webkit-check stage's marker, bound from that stage's /out: the RUN prints the launch line it
# holds and fails unless the line starts `webkit: Playwright `. This mount is what makes BuildKit
# build that stage at all, so it is the WebKit gate; the RUN writes nothing into the image. It
# follows the toolchain step and the Claude Code pin, whose inputs change less often than the
# Playwright packages/dispatch locks, and BuildKit reads the mount by content, so a rerun of that
# stage that prints the same line leaves this layer and every one after it cached.
RUN --mount=type=bind,from=webkit-check,source=/out,target=/tmp/webkit-check \
    set -eu; \
    cat /tmp/webkit-check/webkit-check; \
    case "$(cat /tmp/webkit-check/webkit-check)" in \
      "webkit: Playwright "*) ;; \
      *) echo "the webkit-check stage's marker does not start with 'webkit: Playwright '" >&2; exit 1 ;; \
    esac
# The notices stage's file, after the toolchain step so a notices change never reruns it, and before
# `legion`, which changes on every commit while the notices change only with a dependency, a pin, or
# runtime-base's package set.
COPY --from=notices /out/THIRD_PARTY_NOTICES /usr/share/doc/legion/THIRD_PARTY_NOTICES
# The three plugins are root's like the rest of /opt/legion: every role of an issue pod that does not
# install the plugins loads them from here, one container's filesystem, so a role that could write
# them could change the plugin code its siblings load. A launch that installs the plugins loads them
# from the role's state home on the issue volume instead, which every role of the issue can write;
# nothing here covers that copy. The final step refuses an image where anything under them is not
# root's or is writable by anyone else.
COPY --from=plugin /out/pi-envoy /opt/legion/pi-envoy
COPY --from=plugin /out/pi-legion /opt/legion/pi-legion
# claude-envoy, the plugin root a phase worker on the `claude` backend loads: its package root
# beside the two Oh My Pi roots (names.go's claudePluginDir), holding the build of this checkout
# the plugin stage made — `dist/envoy-channel.js`, the worker's one MCP server, and `legion/`, the
# generated Legion plugin Claude Code loads as a second --plugin-dir (claudelaunch).
# Every Claude role of an issue pod loads this one copy, so it is root's too.
COPY --from=plugin /out/claude-envoy /opt/legion/claude-envoy
# `legion` goes in after the probe layer and the toolchain: its binary differs on every commit (it
# links the commit), so a new commit rebuilds only the layers from here down, never the probe layer and
# its natives.
COPY --from=go /out/legion /opt/legion/bin/legion
# agent-secrets (packages/envoy/cmd/cipher): the pod's secrets client — the shim
# runs `keygen` before its hello and `renew` after its enrollment, and the agent's tools call it
# from PATH, which /opt/legion/bin leads in the image and in every worker container (the PATH
# mainEnvironment sets in packages/daemon/internal/runtime/sandbox/manifest.go). The daemon's
# Tools.AgentSecrets names this path.
COPY --from=go /out/agent-secrets /opt/legion/bin/agent-secrets
# gh in a pod is `legion gh`, first on the image PATH, which runs the real gh at /usr/local/bin/gh
# (LEGION_GH_PATH, which every agent container is told) with GH_CONFIG_DIR at the gh directory of
# the repository the command names, or of its owner where the bundle holds the repository none:
# packages/daemon/docker/gh, the same script a tmux pane's launcher directory holds
# (packages/daemon/internal/runtime/workerbin).
COPY --chmod=0755 packages/daemon/docker/gh /opt/legion/bin/gh
# git in a pod is Legion's wrapper, first on the image PATH. It changes a commit and a push and runs
# every other command unchanged with the real git at /usr/bin/git. A commit goes to `legion git`,
# which adds the commit's Omp-Session and Legion-Role trailers and then runs the real git. A push
# runs with `-c core.hooksPath=/opt/legion/githooks` in front and the push's `--no-verify` taken
# out, so git's own pre-push hook runs the leak guard over the update lines git computed and a
# role can neither edit that hook nor skip it. The `-c` outranks the runtime's
# core.hooksPath=/dev/null pin, which still holds for every other command (mainEnvironment,
# packages/daemon/internal/runtime/sandbox/manifest.go): packages/daemon/docker/git.
COPY --chmod=0755 packages/daemon/docker/git /opt/legion/bin/git
# jj in a pod is Legion's wrapper too, which runs the real jj at /usr/local/bin/jj with the wrapper
# above as jj's git.executable-path on every command line, and again as the line's last layer
# after a push. jj's push is a `git … push --porcelain --no-verify`, so that wrapper is what puts
# the leak guard in front of it. Before a `jj git push` or `jj gerrit upload`, aliases and the
# default command expanded as jj expands them, it also checks that the git on PATH is the wrapper
# and refuses a push whose configuration names another git, rather than pushing unchecked. It
# hands a `jj git push` to `legion jj --real /usr/local/bin/jj`, whose push step sets the author of
# a commit the push sends from that commit's Legion-Role trailer, where its rules let it, and which
# then runs the real jj:
# packages/daemon/docker/jj.
COPY --chmod=0755 packages/daemon/docker/jj /opt/legion/bin/jj
# The pre-push hook the git wrapper points git at, in a root-owned directory no role can write:
# it hands git's remote, URL and update lines to `legion leak-check pre-push` and holds no rule of
# its own (packages/daemon/docker/githooks/pre-push, packages/daemon/internal/leakguard).
COPY --chmod=0755 packages/daemon/docker/githooks/pre-push /opt/legion/githooks/pre-push
# The pin of the shared public leak check, re-declared here so the runtime stage can read the
# global ARG above, and exported as a variable of the image: every process the image runs finds
# it. The pod's launcher reads the pin from its own environment and names it to the daemon, and
# each role's `legion worker-shim` reads it from its own before it starts a role's agent
# (packages/daemon/internal/leakguard's TagVariable and Installed). The image is its
# only setter, so a deployment's `runtime.kubernetes.pod.env` may not move it
# (sandbox/operatorpod.go ImageEnv). It goes in after the toolchain and the probe layer, with the
# wrappers above, so a tag bump reruns only these last few layers. The check itself is not in the
# image: it publishes often, and the image build has no credential for the private repository
# that publishes it. The daemon reads it at this tag and pushes it to each pod's launcher.
ARG LEAK_CHECK_TAG
ENV LEGION_LEAK_CHECK_TAG=${LEAK_CHECK_TAG}
ARG GH_STACK_TOOL
# The final step: /opt/legion, /opt/legion/bin and /opt/legion/githooks are root's with mode 0755,
# so the runtime user can neither rename nor replace what they hold; everything under
# /opt/legion/pi-envoy,
# /opt/legion/pi-legion, /opt/legion/claude-envoy and /opt/legion/gh-extensions is root's and
# writable by no one else, so no role changes the image's copy
# of the plugin code (a launch that installs the plugins loads them instead from the role's state
# home on the issue volume, which every role of the issue can write); the role mount points under
# /var/run/legion are root's, mode 0755 and empty, and the mount point of the pod's test server
# log there, /var/run/legion/testpg.log, is root's, mode 0444 and empty;
# and no file under /usr/lib/openssh or /usr/bin/ssh-agent
# carries a setuid or setgid bit (the apt RUN drops the two openssh-client installs, ssh-keysign's and
# ssh-agent's), so a rebuild of that layer that brings one back fails here.
# /etc/gitconfig holds the four pack settings the runtime stage wrote, at their values: the step
# prints `gitconfig: <key>=<value>` for each, as `git config --system --get` reads it, and fails on
# any other value, a missing key included.
# Then the `legion` on the image PATH is this one, it runs on this base, and it names the
# commit the workflow built. Its `legion version --contracts` is held line by line to
# LEGION_CONTRACT_ORDER, the stamp the go stage linked: `contract` L plus the stamp's count of
# ids, L the greatest number of packages/contracts/fixtures/daemon-api/released-contracts.json
# (bind-mounted from the build context); `order stamped`, for the stamp `v1:` of no entry too;
# and each id of the stamp in its order, the k-th numbered L + k and followed by its slug. A
# `legion` linked with no stamp prints `order id`, so it fails here even where its ids fall in
# the stamp's order. git, jj and gh each resolve to Legion's own wrapper in
# /opt/legion/bin and the step refuses any other path, and behind each one the real binary runs
# and prints its version: /usr/bin/git, /usr/local/bin/jj and /usr/local/bin/gh. The real gh runs
# `gh stack` from a data directory laid out as the shim lays a role's out (a link to
# /opt/legion/gh-extensions/gh-stack under gh/extensions), and it must print the version ARG
# GH_STACK_TOOL pins, so an image whose extension gh cannot find or whose binary is another
# release fails here. So the three
# absolute paths stay as fixed as they were when git and jj were reached by their own names, and
# a pod environment can name all three; what has moved is that a push now passes through the git
# wrapper's hook and a commit through `legion git`. One commit through the PATH's git, in a scratch
# repository under /tmp with a probe identity, LEGION_ROLE=tester and the Oh My Pi session
# image-probe, must carry the lines `Omp-Session: image-probe` and `Legion-Role: tester`. That
# runs the image's own git wrapper with the image's own `legion`, which no pod runs before the
# image rolls out. The pre-push hook itself is root's, executable, and in the root-owned
# directory checked above, so no role can edit it or put another in its place. flock is pinned
# the same way, and for a reason the other gates do not
# cover: the installs Legion renders call it as /usr/bin/flock — the plugin pair's for an
# installing launch (installBlock) and Claude Code's for every Claude launch (claudeCodeBlock), both
# packages/daemon/internal/runtime/sandbox/plugins.go — while the apt line, the toolchain check
# above and the toolchain capability row all ask only that `flock` be on PATH, so a base that kept
# flock and moved it would pass every one of those and fail every such launch. The declaration and
# the use have to agree on the path, and this is where they are held to it. npm is pinned at
# /usr/local/bin/npm for the same reason: claudeCodeBlock calls it by that path, while the
# toolchain check asks only that `npm` be on PATH.
# nc is pinned for the neighbouring reason: it is the one name here the alternatives system
# supplies rather than its own package (netcat-openbsd ships /usr/bin/nc.openbsd), so the
# alternatives link is what the name resolves through and a base that changed it would pass every
# other gate. The Envoy plugin's `dispatch` shim runs its bundled CLI under
# the image's bun and prints its help, so a pi-envoy packed without bin/ or dist/dispatch.js, or an
# image without bun, fails here; that the extension puts bin/ first on an agent's PATH is pi-envoy's
# own test. Then `legion probe-image` runs the three launch probes through
# the daemon's own code, loading the plugins the way a Sandbox pod does (--envoy-plugin-root then
# --plugin-root: the two explicit extensions, discovery on, so the profile's CodeGraph plugin and a
# repository's own extensions load beside the two roots; a pi-envoy that loaded twice, a profile
# link beside the explicit root, is refused), holds the Legion plugin to the daemon
# API contract this binary speaks and to the Envoy plugin's interface, and resolves by name every
# task agent and skill Legion's prompts name (shipped in the Legion plugin's agents/ and the two
# plugins' dist/skills directories). It leaves those agents' models unresolved
# (--skip-agent-models): the build has none of the operator's model configuration, which the pod
# brings. --claude-root and --claude-code-pin name the Claude lane's two paths — the image's own,
# which they default to, named here so this line says what it checks: the claude-envoy root's
# built channel server (dist/envoy-channel.js) and generated Legion plugin
# (legion/.claude-plugin/plugin.json), each a regular file, and that root's manifest held to the
# same daemon API contract the Legion plugin is; and Claude Code's pin at /opt/legion/claude-code,
# which must pin one exact version and lock it and its linux-x64 package with the sha512 integrity
# `npm ci` holds each tarball to — so an image whose Claude worker could not start fails here
# rather than at a pod's first claim. The image carries no Claude Code, and this step fetches none:
# what it proves is that the image is self-consistent, and the fetch itself is proved where it
# happens, by every Claude launch's start script, which fails that launch when the fetch fails or
# does not verify (verifyClaude, packages/daemon/internal/daemon/bootgate.go, says why the probe
# reads the pin rather than installing from it). Then it checks every image-site capability as a
# worker's Oh My Pi would find each:
# `omp setup python --check`; the chromium on PATH (or the browser a PUPPETEER_EXECUTABLE_PATH names)
# with --version; gopls, typescript-language-server and pyright-langserver on PATH; the CodeGraph CLI
# on PATH with its plugin enabled in the profile's lock; and go, gcc, curl, wget, python3, node, bun,
# uv, kubectl, jq, ssh-keygen, flock, psql, redis-cli, sqlite3, nc and dig on PATH with `go version`
# running, and the claude-code row's claude-envoy root and Claude Code pin as the lane held them
# with npm and node on PATH (packages/daemon/internal/capabilities/image.go). It prints the table,
# one `probe-image: capability <name>: <status> (<detail>)` line per row, then
# `probe-image: OK (/opt/omp/bin/omp) session-storage=probed extensions=discovered
# agent-models=skipped capabilities=checked model-fallback=on omp-version=omp/<version>
# omp-session-id=<taken|absent|unread> daemon-api-version=<N>` — `on` here is
# Oh My Pi's own default for retry.modelFallback, since the build runs under no operator overlay; a
# probe pod reads the operator's value — and a build whose image lacks a capability fails here, the
# probe naming every failure. The daemon's probe Sandbox runs it
# again with its own contract, on the pod baseline and under the operator's pod, resolving every
# agent's model, and refuses a skipped result, before any claim runs on the image
# (packages/daemon/internal/runtime/sandbox/probe.go).
# It needs the natives step 3 extracted, which the cached probe layer above carries. The step leaves no
# residue: Oh My Pi's logs, and the Go telemetry counters the capability check's `go version` writes
# under HOME (the toolchain step's XDG_CONFIG_HOME does not reach a probe that runs as a pod would).
# Each plugin root's package.json must name the version of this checkout's manifest (bind-mounted
# from the build context, so the image keeps no copy), or the release version a release passes
# (PI_ENVOY_VERSION, PI_LEGION_VERSION): an image whose plugin roots name any other version, or no
# version, fails here.
ARG LEGION_REVISION
ARG LEGION_CONTRACT_ORDER
ARG PI_LEGION_VERSION
ARG PI_ENVOY_VERSION
# Each client plugin's build-commit stamp: the commit this image is built from, written beside the
# bundle of pi-envoy and of claude-envoy, which the plugin names after its NATS connection's name
# (@legion/envoy-client's clientBuild: a plugin this image packs from its checkout carries that
# checkout's manifest version, not a release's), and which legacy-subjects-gate.sh reads from the
# hub's census of connections. Root's and mode 0444, as every plugin file is root's alone (the check
# below). A revision that is no whole commit id writes none. pi-envoy then names its packed
# manifest's version, @sjawhar/pi-envoy@<version>, which the gate judges by that release's tag, not
# by this checkout's commit, so a tag behind the new-form commit shuts it; claude-envoy, which reads
# a version only from a plugin cache's path, names no build, which the gate reads as unknown.
USER root
RUN if printf '%s\n' "$LEGION_REVISION" | grep -Eqx '[0-9a-f]{40}'; then \
      for dir in /opt/legion/pi-envoy/dist /opt/legion/claude-envoy/dist; do \
        printf '%s\n' "$LEGION_REVISION" > "$dir/build-commit" && chmod 0444 "$dir/build-commit"; \
      done; \
    fi
USER 1000:1000
RUN --mount=type=bind,source=packages/pi-envoy/package.json,target=/tmp/source/pi-envoy.json \
    --mount=type=bind,source=packages/pi-legion/package.json,target=/tmp/source/pi-legion.json \
    --mount=type=bind,source=packages/contracts/fixtures/daemon-api/released-contracts.json,target=/tmp/source/released-contracts.json \
    set -eu; \
    for plugin in pi-envoy pi-legion; do \
      case "$plugin" in pi-envoy) want="${PI_ENVOY_VERSION:-}" ;; *) want="${PI_LEGION_VERSION:-}" ;; esac; \
      if [ -n "$want" ]; then source="the release's"; else \
        source="this commit's"; want="$(jq -er '.version | strings' "/tmp/source/$plugin.json")"; \
      fi; \
      have="$(jq -er '.version | strings' "/opt/legion/$plugin/package.json")"; \
      echo "$plugin: /opt/legion/$plugin $have, $source $want"; test "$have" = "$want"; \
    done; \
    for dir in /opt/legion /opt/legion/bin /opt/legion/githooks /var/run/legion /var/run/legion/gh /var/run/legion/private \
      /var/run/legion/state /var/run/legion/agent-secrets; do \
      owner="$(stat -c '%u:%g %a' "$dir")"; echo "$dir: $owner"; test "$owner" = "0:0 755"; \
    done; \
    for dir in /var/run/legion/gh /var/run/legion/private /var/run/legion/state /var/run/legion/agent-secrets; do \
      test -z "$(ls -A "$dir")"; \
    done; \
    testpg_log="$(stat -c '%u:%g %a %s' /var/run/legion/testpg.log)"; echo "/var/run/legion/testpg.log: $testpg_log"; \
    test "$testpg_log" = "0:0 444 0"; \
    plugins="$(find /opt/legion/pi-envoy /opt/legion/pi-legion /opt/legion/claude-envoy /opt/legion/gh-extensions \( ! -user 0 -o ! -group 0 -o -perm /022 \) -print)"; \
    echo "plugin and gh extension files not root's alone: ${plugins:-none}"; test -z "$plugins"; \
    stat -c '%a %n' /usr/lib/openssh/ssh-keysign /usr/bin/ssh-agent; \
    setid="$(find /usr/lib/openssh /usr/bin/ssh-agent -perm /6000)"; echo "set-id bits: ${setid:-none}"; test -z "$setid"; \
    git="$(command -v git)"; echo "git: $git"; test "$git" = /opt/legion/bin/git; /usr/bin/git --version; \
    for setting in core.packedGitWindowSize=32m core.packedGitLimit=256m pack.windowMemory=64m core.deltaBaseCacheLimit=64m; do \
      key="${setting%%=*}"; value="$(/usr/bin/git config --system --get "$key" || true)"; \
      echo "gitconfig: $key=$value"; test "$key=$value" = "$setting"; \
    done; \
    jj="$(command -v jj)"; echo "jj: $jj"; test "$jj" = /opt/legion/bin/jj; /usr/local/bin/jj --version; \
    hook="$(stat -c '%u:%g %a' /opt/legion/githooks/pre-push)"; echo "pre-push hook: $hook"; \
    test "$hook" = "0:0 755"; test -x /opt/legion/githooks/pre-push; \
    flock="$(command -v flock)"; echo "flock: $flock"; test "$flock" = /usr/bin/flock; \
    nc="$(command -v nc)"; echo "nc: $nc"; test "$nc" = /usr/bin/nc; \
    legion="$(command -v legion)"; echo "legion: $legion"; test "$legion" = /opt/legion/bin/legion; \
    probe="$(mktemp -d)"; git init -q "$probe"; \
    GIT_AUTHOR_NAME='Image probe' GIT_AUTHOR_EMAIL=image-probe@example.invalid \
      GIT_COMMITTER_NAME='Image probe' GIT_COMMITTER_EMAIL=image-probe@example.invalid \
      LEGION_ROLE=tester DISPATCH_HOST=omp DISPATCH_SESSION_ID=image-probe \
      git -C "$probe" commit --allow-empty -q -m probe; \
    message="$(git -C "$probe" log -1 --format=%B)"; printf 'probe commit:\n%s\n' "$message"; \
    printf '%s\n' "$message" | grep -qx 'Omp-Session: image-probe'; \
    printf '%s\n' "$message" | grep -qx 'Legion-Role: tester'; \
    rm -rf "$probe"; \
    gh="$(command -v gh)"; echo "gh: $gh"; test "$gh" = /opt/legion/bin/gh; /usr/local/bin/gh --version; \
    ghdata="$(mktemp -d)"; mkdir -p "$ghdata/gh/extensions"; ln -s /opt/legion/gh-extensions/gh-stack "$ghdata/gh/extensions/gh-stack"; \
    stack="$(XDG_DATA_HOME="$ghdata" XDG_CONFIG_HOME="$ghdata/config" XDG_STATE_HOME="$ghdata/state" XDG_CACHE_HOME="$ghdata/cache" \
      GH_CONFIG_DIR="$ghdata/gh-config" GH_NO_UPDATE_NOTIFIER=1 GH_STACK_NO_UPDATE_NOTIFIER=1 GH_TELEMETRY=0 /usr/local/bin/gh stack --version)"; \
    rm -rf "$ghdata"; echo "gh stack: $stack"; test "$stack" = "gh stack version ${GH_STACK_TOOL##*@}"; \
    npm="$(command -v npm)"; echo "npm: $npm"; test "$npm" = /usr/local/bin/npm; \
    DISPATCH_HOST=omp DISPATCH_SESSION_ID=image-check HOME=/tmp \
      /opt/legion/pi-envoy/bin/dispatch --help > /tmp/dispatch-help; \
    echo "dispatch: $(sed -n 1p /tmp/dispatch-help)"; grep -q '^Usage: dispatch ' /tmp/dispatch-help; \
    rm /tmp/dispatch-help; \
    agent_secrets="$(command -v agent-secrets)"; test "$agent_secrets" = /opt/legion/bin/agent-secrets; \
    version="$(legion version)"; echo "$version"; \
    expected="legion (devel) commit ${LEGION_REVISION}"; \
    if [ -n "${PI_LEGION_VERSION:-}" ] && [ -n "${PI_ENVOY_VERSION:-}" ]; then \
      expected="$(printf '%s\nplugins @sjawhar/pi-legion %s @sjawhar/pi-envoy %s' "$expected" "$PI_LEGION_VERSION" "$PI_ENVOY_VERSION")"; \
    fi; \
    test "$version" = "$expected"; \
    contracts="$(legion version --contracts)"; echo "$contracts"; \
    last="$(jq -e max /tmp/source/released-contracts.json)"; \
    expected="$(printf '%s\n' "${LEGION_CONTRACT_ORDER:-}" | awk -F, -v l="$last" 'sub(/^v1:/, "") { print "contract", l + NF; print "order stamped"; for (k = 1; k <= NF; k++) print $k, l + k }')"; \
    test "$(printf '%s\n' "$contracts" | awk 'NR > 2 && NF == 3 { print $1, $2; next } 1')" = "$expected"; \
    legion probe-image --plugin-root /opt/legion/pi-legion --envoy-plugin-root /opt/legion/pi-envoy --claude-root /opt/legion/claude-envoy --claude-code-pin /opt/legion/claude-code --skip-agent-models; \
    agent-secrets --help >/dev/null; \
    rm -rf /home/legion/.omp/profiles/legion/logs /home/legion/.config/go
# The Kubernetes runtime sets every container's command explicitly
# (packages/daemon/internal/runtime/sandbox/manifest.go): the init containers run `legion
# workspace-init fetch …` and `legion workspace-init provision …`, and the main container runs `legion
# worker-shim --connect tcp://<daemon>:<worker_stream_port> --boot-token-file … -- omp --mode rpc …`.
# This ENTRYPOINT therefore only makes `docker run <image> version` and `docker run <image>
# probe-image --plugin-root /opt/legion/pi-legion --envoy-plugin-root /opt/legion/pi-envoy
# --claude-root /opt/legion/claude-envoy --claude-code-pin /opt/legion/claude-code
# --skip-agent-models` work.
ENTRYPOINT ["legion"]
