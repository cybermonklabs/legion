#!/usr/bin/env bash
# The capture of the approval message's check e4b07a29 (plan task t-12): while a person's
# approval of a root's spec stands, the root architect's model requests carry the approval message,
# on the pod's own model route. Real Oh My Pi sessions of the build .omp-pin names, each the root
# architect of an issue, load this branch's @sjawhar/pi-envoy and @sjawhar/pi-legion, packed as the
# release packs them, in the sandbox launch's order (pi-envoy first, `agentArgv` in
# packages/daemon/internal/runtime/sandbox/manifest.go), and then a recorder test extension
# (packages/pi-legion/test/approval-context-recorder.ts) whose `before_provider_request` handler
# writes each request body Oh My Pi sends and the model it goes to. Each session runs on the pod's
# own models.yml and operator overlay, so its turns are real model calls through the pod's model
# gateway. The daemon is a stand-in whose approval route answers that no approval stands until a
# case's entry is sent, and the approved version from then on, in the daemon's own words.
#
# The first case reads the pod's models.yml, its operator overlay and its architect role's overlay
# and prints only whether each sets a remoteCompaction, a compaction.remoteEndpoint or a Bedrock
# route, which none may, and whether the role overlay's fallback chain is the one the chain cases
# run. Then the cases on the model the overlay puts on `default`, some tens of short requests
# through a recording proxy that records each request body, which is how a compaction's own request
# is read: the entry of each of the six paths as the daemon renders it, to an idle session and to a
# session in a turn after a tool result, the carried-gate-opened notice among them; two cards in the
# same window; a people's-answers message in the same request; the two plugins in the reverse
# order; a compaction that summarizes the message's answer and one that keeps it; a fresh session;
# relaunches into the same session, also after a changes request and after a later approval; the
# date-reminder case; a task subagent, a side turn and the handoff prompt; R1's words sent as a card
# that wakes an idle session. Then the chain cases: a session on each model of the architect's
# fallback chain (claude-fable-5-2, gemini-4-neon, claude-sonnet-5-5, gpt-6.2-sol, claude-opus-5 and
# kimi-k3.1-pro) on the pod's own models.yml as it is, with the design-approved entry and two
# requests after it, which hold the message last as `system` on a model that takes a system message
# inside the conversation, and on any other at the end of the top-level system prompt and nowhere
# else, with pi-legion's mark in the session; and a session on claude-opus-5-5 whose case overlay
# chains it to gemini-4-neon alone, on a copy of the pod's models.yml (mode 0600 in the run's work
# directory, removed with the session) whose anthropic baseUrl is the stand-in. The stand-in
# forwards each request to the gateway until the placement is answered, then answers 429
# rate_limit_error with x-should-retry: false and a retry-after of ten minutes, so the session
# falls back to gemini-4-neon at once and stays there for two requests; then it forwards again,
# and the driver sets the model back to claude-opus-5-5 (the RPC set_model) for one more request.
# The driver, packages/pi-legion/test/approval-message-capture.ts, holds the cases and their
# checks, and packages/pi-legion/test/approval-message-omp.test.ts runs the cases on the default
# model in CI on a stand-in model, and again on claude-sonnet-5, which takes no system message
# inside the conversation. Each case prints `== <case>`, what it observed and `ok <case>` or
# `FAIL <case>: <why>`; a failed case does not stop the run, and the run ends with
# `approval message capture: PASS` or names the first case that failed, or the failure that stopped
# the driver before its cases.
#
# Run it in a Legion pod, from a checkout after `bun install`, as
#   bash scripts/e2e/approval-message.sh
# It needs bash, bun, go (for the NATS server it builds and the daemon's words), curl, tar, and jq
# and flock (for lib/pack-plugin.sh); git too with APPROVAL_MESSAGE_PLUGINS_REF.
# APPROVAL_MESSAGE_PLUGINS_REF=<git revision> packs both plugins from that revision instead
# (`git archive` and `bun install` in the run's work directory), and also checks that no request of
# the run holds an approval message: APPROVAL_MESSAGE_PLUGINS_REF=origin/main is the negative
# control, whose placing cases fail, and which exits 0 exactly when the run sent requests and none
# of them holds an approval message. The other inputs and their defaults:
#   APPROVAL_MESSAGE_MODELS     the pod's models.yml ($PI_CODING_AGENT_DIR/models.yml), which the
#                               driver reads in its own process, its base URLs and keys printed
#                               nowhere, and checks that it offers the overlay's default model on
#                               its anthropic provider and each model of the chain on its API
#   APPROVAL_MESSAGE_OVERLAY    the pod's operator overlay (the overlay.yml PI_CONFIG_FILES names),
#                               which each session reads through PI_CONFIG_FILES as the pod's do,
#                               and whose modelRoles.default every case but the chain cases runs
#   APPROVAL_MESSAGE_ROLE_OVERLAY  the overlay of the pod's architect role (role-architect.yml
#                               beside the operator overlay), whose fallback chain the chain cases
#                               run
#   APPROVAL_MESSAGE_ENTRIES    each-path: the first entry file of each path; every: all of them;
#                               chain-flag-off: the flag-off negative control alone (below);
#                               top-level-capture: none of the cases, but the capture of the wire
#                               bodies Oh My Pi itself sends to the models whose
#                               supportsMidConversationSystem flag is off (below)
#   APPROVAL_MESSAGE_ONLY       a prefix of case names: only the cases whose names start with it
#                               run, after the first (`chain:` runs the chain cases and the
#                               fallback case); unset, every case runs
#   APPROVAL_MESSAGE_OMP_ROOT   where the pinned build is installed ($TMPDIR/omp-pin); a build
#                               already there at .omp-pin's version is used again
#   APPROVAL_MESSAGE_EVIDENCE_DIR  a fresh directory under TMPDIR by default, kept and printed: the
#                               transcript, the NATS server's and each pack's log, and in records/
#                               every session's recorded requests, one file per case
#
# APPROVAL_MESSAGE_PLUGINS_REF=e516478c APPROVAL_MESSAGE_ENTRIES=chain-flag-off is the flag-off
# negative control: after the first case, a session on each model of the chain that takes no
# system message inside the conversation (gemini-4-neon, gpt-6.2-sol and kimi-k3.1-pro), with the
# plugins of a revision from before pi-legion put the approval message at the end of the top-level
# system prompt. Each must show the message as a user message of the conversation and no top-level
# system prompt holding it, and the run ends with
# `approval message capture: flag-off negative control PASS` or names the first case that failed.
# With this checkout's plugins those cases fail by design.
#
# APPROVAL_MESSAGE_ENTRIES=top-level-capture runs one short session of the pinned build per body
# on the pod's own models.yml and overlay, each with the recorder alone loaded and neither plugin,
# so no approval handler touches a request; it starts no NATS server and packs nothing. The driver
# writes each session's `before_provider_request` body of its second prompt, every local path
# written over, under packages/pi-legion/test/fixtures/approval-context/top-level/, and prints
# where: claude-sonnet-5 (anthropic-messages), gemini-4-neon and kimi-k3.1-pro
# (openai-completions), and gpt-6.2-sol (openai-responses) twice, on the pod's own models.yml and
# on a copy of it in the run's work directory, mode 0600, removed once that session ends, whose
# gpt-6.2-sol entry flips compat.supportsDeveloperRole, so the two bodies hold the system prompt
# as `instructions` and as leading `developer` items. It needs bash, bun, curl and tar, and ends
# with `approval message capture: top-level capture PASS` or names the first session that failed.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
started=$SECONDS
check=setup
check_started=$SECONDS
# Set once the driver runs: its own last line, its verdict, then ends the run.
driver_ran=

begin() {
  check=$1
  check_started=$SECONDS
  echo "== $check"
}
note() { echo "   $*"; }
pass() { echo "ok $check ($((SECONDS - check_started))s)"; }
fail() {
  echo "FAIL $check: $*" >&2
  exit 1
}

evidence=${APPROVAL_MESSAGE_EVIDENCE_DIR:-}
if [ -z "$evidence" ]; then
  evidence=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-approval-message-evidence.XXXXXXXX")
fi
# logs/ is the rig's own: start_process logs each service it starts to $evidence/logs/<name>.log.
mkdir -p "$evidence/logs"

# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"
work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-approval-message.XXXXXXXX")
timeout_hook=
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"

# cleanup stops the NATS server this run started and removes the work directory (its store, the
# packed plugins, a revision's tree, the copies of models.yml the top-level capture and the
# fallback case make); the sessions are the driver's own children, which it stops itself. The
# evidence directory, the installed build and the built natsd stay.
cleanup() {
  local rc=$1
  trap - EXIT INT TERM
  stop_pid "${nats_pid:-}"
  [ -z "${work:-}" ] || rm -rf "${work:?}"
  if [ "$rc" -ne 0 ] && [ -z "$driver_ran" ]; then
    echo "approval message capture: FAIL at $check after $((SECONDS - started))s (evidence: $evidence)" >&2
  fi
  exit "$rc"
}
trap 'cleanup "$?"' EXIT
trap 'cleanup 130' INT
trap 'cleanup 143' TERM

begin prerequisites
ref=${APPROVAL_MESSAGE_PLUGINS_REF:-}
entry_files=${APPROVAL_MESSAGE_ENTRIES:-each-path}
only=${APPROVAL_MESSAGE_ONLY:-}
case "$entry_files" in
each-path | every | chain-flag-off) ;;
top-level-capture)
  [ -z "$ref" ] || fail "APPROVAL_MESSAGE_PLUGINS_REF is set, but the top-level capture loads no plugin"
  [ -z "$only" ] || fail "APPROVAL_MESSAGE_ONLY is set, but the top-level capture runs no case"
  ;;
*) fail "APPROVAL_MESSAGE_ENTRIES is '$entry_files'; it is each-path, every, chain-flag-off or top-level-capture" ;;
esac
# bun runs the driver and packs, go builds natsd and prints the daemon's words, curl and tar fetch
# and unpack the pinned build, and lib/pack-plugin.sh rewrites the manifest with jq under a flock;
# git takes a revision's tree. The top-level capture builds, prints and packs nothing.
tools=(bun curl tar)
[ "$entry_files" = top-level-capture ] || tools+=(go jq flock)
[ -z "$ref" ] || tools+=(git)
for tool in "${tools[@]}"; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
driver=$root/packages/pi-legion/test/approval-message-capture.ts
[ -f "$driver" ] || fail "$driver is missing"
[ -d "$root/packages/pi-legion/node_modules/@legion/pi-shared" ] ||
  fail "packages/pi-legion/node_modules holds no @legion/pi-shared: run bun install in this checkout"
# The driver reads the file's providers and checks the models the run needs before its first case
# or session; nothing here prints a line of it.
models=${APPROVAL_MESSAGE_MODELS:-${PI_CODING_AGENT_DIR:-}/models.yml}
[ -r "$models" ] || fail "no readable models.yml at '$models': set APPROVAL_MESSAGE_MODELS to the pod's own"
overlay=${APPROVAL_MESSAGE_OVERLAY:-}
if [ -z "$overlay" ]; then
  IFS=: read -r -a configs <<<"${PI_CONFIG_FILES:-}"
  for file in "${configs[@]}"; do
    [ "${file##*/}" != overlay.yml ] || overlay=$file
  done
fi
if [ -z "$overlay" ] || [ ! -r "$overlay" ]; then
  fail "no readable operator overlay ('$overlay'): set APPROVAL_MESSAGE_OVERLAY to the pod's own"
fi
note "model route: $models and the overlay $overlay"
if [ "$entry_files" = top-level-capture ]; then
  note "the top-level capture: the recorder alone, no plugin"
else
  # The fallback chain of the architect role, which the chain cases run and the first case reads.
  role_overlay=${APPROVAL_MESSAGE_ROLE_OVERLAY:-${overlay%/*}/role-architect.yml}
  [ -r "$role_overlay" ] ||
    fail "no readable architect role overlay ('$role_overlay'): set APPROVAL_MESSAGE_ROLE_OVERLAY to the pod's own"
  note "the architect role's overlay $role_overlay"
  note "entries: $entry_files; plugins: ${ref:-this checkout}${only:+; only the cases whose names start with '$only'}"
fi
note "evidence directory $evidence"
pass

begin omp
# .omp-pin is the mise tool spec github:<owner>/<repo>@<build>, which .github/actions/install-omp
# installs with `mise install`: mise's GitHub backend fetches the build's release asset for this
# platform and unpacks it. A pod has no mise, so this fetches the same asset and unpacks it the same
# way, under APPROVAL_MESSAGE_OMP_ROOT. omp --version prints the release alone (omp/<release>), so
# a build is known by the directory its asset unpacks to, which names the build.
pin=$(<"$root/.omp-pin")
tool=${pin%@*}
build=${pin##*@}
release=${build%%-*}
case "$tool" in
github:*/*) repo=${tool#github:} ;;
*) fail ".omp-pin names $pin, which is no github:<owner>/<repo>@<build> tool spec" ;;
esac
case "$(uname -s)-$(uname -m)" in
Linux-x86_64) platform=linux-x64 ;;
*) fail "the pinned build ships no asset for $(uname -s)-$(uname -m) that this script installs" ;;
esac
asset=omp-v$build-$platform
omp_root=${APPROVAL_MESSAGE_OMP_ROOT:-${TMPDIR:-/tmp}/omp-pin}
omp=$omp_root/$asset/bin/omp
version=$("$omp" --version 2>/dev/null) || version=
if [ "${version%%$'\n'*}" = "omp/$release" ]; then
  note "$pin is installed at $omp already"
else
  mkdir -p "$omp_root"
  url=https://github.com/$repo/releases/download/v$build/$asset.tar.gz
  curl -fsSL --retry 3 -o "$work/$asset.tar.gz" "$url" || fail "could not fetch $url"
  tar -xzf "$work/$asset.tar.gz" -C "$omp_root" || fail "could not unpack $asset.tar.gz into $omp_root"
  version=$("$omp" --version 2>&1) || fail "$omp --version failed: $version"
  [ "${version%%$'\n'*}" = "omp/$release" ] || fail "$omp prints $version, not omp/$release"
  note "installed $pin from $url at $omp"
fi
note "${version%%$'\n'*}"
pass

if [ "$entry_files" = top-level-capture ]; then
  # The capture's sessions load no plugin, so they need no NATS server and no pack. The driver
  # prints a section per session and the run's last line, its verdict.
  driver_ran=1
  bun "$driver" --entries top-level-capture --omp "$omp" --models "$models" --overlay "$overlay" \
    --work "$work" --evidence "$evidence/records"
  exit 0
fi

begin nats
# A session whose Envoy plugin reaches no NATS server is given no consumer, and its claim boot then
# ends it, so the run starts a server of its own on a port the kernel picks (`-p -1`),
# read back from the line it logs once it listens. natsd is installed under TMPDIR, where it stays
# for the next run: `go install` links it again only when what it is built from changed.
natsd_bin=${TMPDIR:-/tmp}/legion-e2e-natsd
GOBIN=$natsd_bin go -C "$root/packages/envoy" install -buildvcs=false ./internal/testnats/cmd/natsd ||
  fail "./internal/testnats/cmd/natsd did not build"
start_process nats "$natsd_bin/natsd" -p -1 -store_dir "$work/nats-store"
await_start nats "$nats_pid" 60 "natsd to be ready" grep -q 'Server is ready' "$evidence/logs/nats.log"
nats_port=$(report_port "$evidence/logs/nats.log" 'Listening for client connections on [^ ]*:') ||
  fail "natsd logged no client port; see $evidence/logs/nats.log"
nats_url=nats://127.0.0.1:$nats_port
note "NATS on $nats_url"
pass

begin plugins
# Each plugin packed by lib/pack-plugin.sh, the release's pack, from this checkout or from the tree of
# APPROVAL_MESSAGE_PLUGINS_REF, then unpacked as the worker image unpacks it; a session loads the
# unpacked package's directory (`--extension`), as the sandbox launch loads the image's.
source_root=$root
if [ -n "$ref" ]; then
  commit=$(git -C "$root" rev-parse --verify --quiet "$ref^{commit}") ||
    fail "APPROVAL_MESSAGE_PLUGINS_REF '$ref' names no commit of this checkout's git"
  mkdir -p "$work/ref"
  git -C "$root" archive "$commit" | tar -x -C "$work/ref" || fail "could not take $commit's tree"
  (cd "$work/ref" && bun install --frozen-lockfile) >"$evidence/logs/bun-install-ref.log" 2>&1 ||
    fail "bun install failed in $ref's tree; see $evidence/logs/bun-install-ref.log"
  source_root=$work/ref
  note "the plugins of $ref ($commit)"
fi
for package in pi-envoy pi-legion; do
  tarball=$(bash "$source_root/scripts/e2e/lib/pack-plugin.sh" "$package" "$work/pack/$package" \
    2>"$evidence/logs/pack-$package.log") ||
    fail "$package did not pack; see $evidence/logs/pack-$package.log"
  mkdir -p "$work/plugins/$package"
  tar -xzf "$tarball" -C "$work/plugins/$package" || fail "could not unpack $tarball"
  [ -f "$work/plugins/$package/package/package.json" ] || fail "$tarball holds no package/package.json"
  note "$package: ${tarball##*/}"
done
pass

# The driver prints a section per case of its own and the run's last line, its verdict, also for a
# failure before its first case.
driver_ran=1
args=(
  --omp "$omp" --nats "$nats_url"
  --envoy "$work/plugins/pi-envoy/package" --legion "$work/plugins/pi-legion/package"
  --models "$models" --overlay "$overlay" --role-overlay "$role_overlay" --work "$work"
  --entries "$entry_files" --evidence "$evidence/records"
)
[ -z "$only" ] || args+=(--only "$only")
# A revision's plugins are the negative control: that no request holds an approval message, or,
# with chain-flag-off, the flag-off cases' own check.
[ -z "$ref" ] || [ "$entry_files" = chain-flag-off ] || args+=(--negative-control)
bun "$driver" "${args[@]}"
