#!/usr/bin/env bash
# The issue-clone live proof on a throwaway kind cluster. An issue pod that the daemon's own code
# launches under gVisor holds one repository at `/legion/repos/github.com/<owner>/<repo>`, with no
# `/legion/feed`. The pod's one `workspace-init` container receives the provisioning token, unpacks
# the repository cache's tar into the clone's `.git`, verifies that history, fetches the default
# branch and its own issue branches from GitHub, then creates the colocated jj workspace. The clone
# has no tags and exposes only its default and issue branches through its refspecs.
#
# The pods are built by Legion's own code: the daemon's loader reads the legion.yaml, and its
# sandboxOptions builds the Agent Sandbox runtime that launches each issue's root through Agent
# Sandbox v1.0.3's controller (internal/daemon's shared_clone_live_test.go), as the share-volumes
# proof's driver launches its pod, on a public repository of some tens of megabytes. Two pods run:
# the first creates the repository cache, and the second starts once that cache is Ready. The
# checks read each pod as its roles container sees it. They prove one init container, the absence
# of the legacy repository path and mount, the narrow refs and clone configuration, plain and named
# fetches as a launched role, a cache-backed second provision with a small GitHub delta, and a cache
# whose tar has only the default branch's objects. They also retain the gVisor, fsck and git/jj log
# checks. Each check prints `== <name>`, what it observed and `ok <name> (<s>s)`; the first that
# fails ends the run non-zero, naming it.

#
# Run it from a checkout on an amd64 (x86_64) Linux host — the gVisor release, kind's binary and
# the static binaries it builds are amd64 only — as
#   bash scripts/e2e/shared-clone-kind.sh
# with docker, go, kubectl, curl, jq, zstd and a tar that supports --zstd installed. It installs
# kind v0.33.0 by sha256 and makes the cluster on that release's node image as
# .github/ci-images.yaml copies it by digest (lib/kind-sandbox.sh's kind_sandbox_install_kind),
# unless LEGION_E2E_KIND names a kind binary, which then makes it on that kind's own default. Its
# busybox is the copy that file makes too. The pods run the worker image of the checkout's own
# commit: LEGION_E2E_IMAGE names it by digest, and where it is unset the run reads the digest of
# `ghcr.io/cybermonklabs/legion-worker:sha-<the first 12 hex of HEAD>`, which
# `.github/workflows/worker-image.yaml` publishes for a pull request's head, and fails naming the
# tag when the registry has none. The image's own `legion` runs the pods' workspace-init, so the
# image must be built from the commit under proof. That is why
# .github/e2e-scripts.json marks this proof dispatch_only: e2e.yaml runs it on
# `gh workflow run e2e.yaml --ref <branch> -f script=shared-clone-kind`, whose checkout is the
# branch's head, and never on a label's merge commit, for which no image is built, or the
# nightly's main, whose image release.yaml builds only for a push whose paths call it, so the
# nightly cannot rely on its tag existing yet. The cluster is lib/kind-sandbox.sh's, the run's own, and every kubectl call
# names its kubeconfig and context, so the caller's kubeconfig is never read. The cluster is
# deleted on any exit; SHARED_CLONE_EVIDENCE_DIR (default a fresh /tmp directory, kept and
# printed) keeps the transcript, the legion.yaml and both issue pods' manifests as the API server
# holds them.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
evidence=${SHARED_CLONE_EVIDENCE_DIR:-$(mktemp -d /tmp/legion-e2e-shared-clone-evidence.XXXXXXXX)}
mkdir -p "$evidence"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"
# shellcheck source-path=SCRIPTDIR source=lib/kind-sandbox.sh
. "$root/scripts/e2e/lib/kind-sandbox.sh"
# shellcheck source-path=SCRIPTDIR source=lib/role-namespaces.sh
. "$root/scripts/e2e/lib/role-namespaces.sh"
# shellcheck source-path=SCRIPTDIR source=lib/run-verdict.sh
. "$root/scripts/e2e/lib/run-verdict.sh"

cluster=legion-shared-clone-$$
work=$(mktemp -d "/tmp/legion-e2e-shared-clone.$$.XXXXXXXX")
check=setup
started=$SECONDS
check_started=$SECONDS

begin() { check=$1; check_started=$SECONDS; echo "== $check"; }
note() { echo "   $*"; }
pass() { echo "ok $check ($((SECONDS - check_started))s)"; }
fail() { echo "FAIL $check: $*" >&2; exit 1; }

# cleanup tears the run down and prints its last line through lib/run-verdict.sh: PASS only for a
# run that reached run_end, the script's last statement.
cleanup() {
  local status=$? ended
  ended=$(run_status "$status")
  docker rm -f "$cluster-driver" >/dev/null 2>&1 || true
  kind_sandbox_teardown "$ended" "$evidence"
  rm -rf "$work"
  run_verdict "$status" \
    "shared clone kind e2e: PASS in $((SECONDS - started))s (evidence: $evidence)" \
    "shared clone kind e2e: FAIL at $check after $((SECONDS - started))s (evidence: $evidence)"
}
trap cleanup EXIT

# on_kind NAME OPTION… -- COMMAND… runs COMMAND, a static binary of the work directory, in a
# busybox container (kind_busybox) on the kind network beside the node, with each docker run
# OPTION, as share-volumes-kind.sh's does: the pods reach the kind network's containers, while a
# host firewall can drop what they send the host at the network's gateway.
on_kind() {
  local name=$1 options=()
  shift
  while [ "$1" != -- ]; do
    options+=("$1")
    shift
  done
  shift
  docker run --rm --name "$cluster-$name" --cpus 2 --memory 2g --network kind --user "$(id -u):$(id -g)" \
    -v "$work:$work" -e HOME=/tmp "${options[@]}" "$kind_busybox" "$@"
}

# worker_image is the image the run's pod runs: LEGION_E2E_IMAGE, or the digest the registry gives
# the checkout's own commit's sha- tag.
worker_image() {
  if [ -n "${LEGION_E2E_IMAGE:-}" ]; then
    echo "$LEGION_E2E_IMAGE"
    return
  fi
  local tag token digest
  tag=sha-$(git -C "$root" rev-parse HEAD | cut -c1-12)
  token=$(curl -fsS "https://ghcr.io/token?scope=repository:cybermonklabs/legion-worker:pull" | jq -er .token) ||
    fail "no anonymous pull token for ghcr.io/cybermonklabs/legion-worker"
  # The pull token goes to curl on standard input, never as an argument every process can read.
  digest=$(printf 'Authorization: Bearer %s\n' "$token" | curl -fsSI -H @- \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
    "https://ghcr.io/v2/cybermonklabs/legion-worker/manifests/$tag" | tr -d '\r' | awk 'tolower($1) == "docker-content-digest:" {print $2}') || true
  [[ $digest == sha256:* ]] ||
    fail "ghcr.io/cybermonklabs/legion-worker has no $tag; set LEGION_E2E_IMAGE to the worker image of $(git -C "$root" rev-parse HEAD), by digest, or run worker-image.yaml on it"
  echo "ghcr.io/cybermonklabs/legion-worker@$digest"
}

begin prerequisites
[ "$(uname -m)" = x86_64 ] || fail "the proof runs on amd64 (x86_64) only, since the gVisor release it installs and the binaries it builds are amd64; this host is $(uname -m)"
for tool in docker go kubectl curl jq zstd tar sha256sum sha512sum; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
if [ -n "${LEGION_E2E_KIND:-}" ]; then
  command -v "$kind_bin" >/dev/null || fail "LEGION_E2E_KIND names $kind_bin, which is not on PATH"
else
  kind_sandbox_install_kind "$work"
fi
note "kind $("$kind_bin" version)${kind_node_image:+, node image $kind_node_image}"
[[ $(tar --help 2>/dev/null) == *--zstd* ]] || fail "this tar has no --zstd; install GNU tar 1.31 or later"
image=$(worker_image)
[[ $image == *@sha256:* ]] || fail "LEGION_E2E_IMAGE must name the worker image by digest (<image>@sha256:<64 hex>)"
note "worker image $image, for $(git -C "$root" rev-parse HEAD)"
pass

begin binaries
CGO_ENABLED=0 go -C "$root/packages/daemon" test -c -tags e2e -o "$work/shared-clone.test" ./internal/daemon
pass

begin cluster
kind_sandbox_create "$cluster" "$work"
pass

begin agent-sandbox
kind_sandbox_install_agent_sandbox "$work"
pass

begin gvisor
kind_sandbox_install_gvisor "$work"
pass

begin images
kind_sandbox_load_image "$image"
kind_sandbox_internal_kubeconfig "$work/kubeconfig-internal"
pass

begin launch
result=$work/result
# shellcheck disable=SC2016 # expanded by the driver container's shell
on_kind driver \
  -e LEGION_E2E_KUBECONFIG="$work/kubeconfig-internal" -e LEGION_E2E_KUBE_CONTEXT="$kind_context" \
  -e LEGION_E2E_IMAGE="$image" -e LEGION_E2E_WORK="$work" -e LEGION_E2E_RESULT="$result" -- \
  sh -c 'LEGION_E2E_STREAM_HOST=$(ip -4 -o addr show eth0 | awk "{print \$4}" | cut -d/ -f1) exec "$0" -test.count=1 -test.timeout=45m -test.v -test.run "^TestAnIssuePodGetsOneRepositoryLive\$"' "$work/shared-clone.test" ||
  fail "the launch driver failed"
{ read -r pod repo && read -r second _ cache_sandbox; } < "$result" || true
[ -n "${pod:-}" ] && [ -n "${repo:-}" ] && [ -n "${second:-}" ] && [ -n "${cache_sandbox:-}" ] || fail "the launch driver recorded no two pods: $(cat "$result" 2>/dev/null)"
pod=${pod#*/} second=${second#*/}
cp "$work/shared-clone/legion.yaml" "$evidence/legion.yaml"
kind_k -n legion get pod "$pod" -o yaml >"$evidence/pod.yaml"
kind_k -n legion get pod "$second" -o yaml >"$evidence/second-pod.yaml"
note "pod $pod on $repo; pod $second on it once its repository cache $cache_sandbox was Ready"
pass

container=roles
in_pod() { kind_k -n legion exec "$pod" -c "$container" -- "$@"; }
clone=/legion/repos/github.com/$repo
# expected_refs <pod>: the refs the pod's clone holds outside refs/jj/: its issue's own bookmark,
# which the workspace's creation exports to git (CLONE-1 in the first pod, CLONE-2 in the second),
# the default branch, and origin's HEAD and default branch, in for-each-ref's order.
expected_refs() {
  local issue=CLONE-1
  [ "$1" = "$second" ] && issue=CLONE-2
  printf 'refs/heads/legion/%s\nrefs/heads/master\nrefs/remotes/origin/HEAD\nrefs/remotes/origin/master' "$issue"
}

refs_in_clone() {
  local proof_pod=${1:-$pod} refs
  refs=$(kind_k -n legion exec "$proof_pod" -c "$container" -- git -C "$clone" for-each-ref --format='%(refname)') ||
    fail "list refs in pod $proof_pod's $clone"
  printf '%s\n' "$refs" | grep -v '^refs/jj/' || true
}


begin provision
for proof_pod in "$pod" "$second"; do
  # shellcheck disable=SC2016 # a kubectl JSONPath, not the shell's
  ends=$(kind_k -n legion get pod "$proof_pod" -o jsonpath='{range .status.initContainerStatuses[*]}{.name}={.state.terminated.exitCode} {end}')
  [ "$ends" = "workspace-init=0 " ] || fail "pod $proof_pod's init containers ended $ends, want workspace-init=0 alone"
  note "pod $proof_pod init containers: $ends"
done
pass

begin gvisor-kernel
for proof_pod in "$pod" "$second"; do
  kernel=$(kind_k -n legion exec "$proof_pod" -c "$container" -- sh -c 'dmesg 2>/dev/null | head -1')
  [[ $kernel == *gVisor* ]] || fail "pod $proof_pod container $container's kernel is not gVisor's: $kernel"
  note "pod $proof_pod container $container: $kernel"
done
pass

begin no-feed
for proof_pod in "$pod" "$second"; do
  kind_k -n legion exec "$proof_pod" -c "$container" -- test ! -e /legion/feed ||
    fail "pod $proof_pod holds /legion/feed"
  mounts=$(kind_k -n legion get pod "$proof_pod" -o json | jq -r '[ (.spec.initContainers[]?, .spec.containers[]?) | .volumeMounts[]? | select(.mountPath == "/legion/feed" or (.mountPath | startswith("/legion/feed/"))) ] | length')
  [ "$mounts" = 0 ] || fail "pod $proof_pod has $mounts /legion/feed mount(s)"
  note "pod $proof_pod: /legion/feed absent; feed mounts: $mounts"
done
pass

begin refs
for proof_pod in "$pod" "$second"; do
  refs=$(refs_in_clone "$proof_pod")
  want=$(expected_refs "$proof_pod")
  [ "$refs" = "$want" ] || fail "pod $proof_pod's clone refs are $(printf %q "$refs"), want $(printf %q "$want")"
  note "pod $proof_pod git -C $clone for-each-ref outside refs/jj/: $(tr '\n' ' ' <<<"$refs")"
done
pass

begin clone-config
expected_fetch=$(printf '+refs/heads/master:refs/remotes/origin/master\n+refs/heads/legion/CLONE-1*:refs/remotes/origin/legion/CLONE-1*')
for digit in {0..9}; do
  expected_fetch+=$(printf '\n^refs/heads/legion/CLONE-1%s*' "$digit")
done
fetch_specs=$(in_pod git -C "$clone" config --get-all remote.origin.fetch) || true
tag_opt=$(in_pod git -C "$clone" config --get remote.origin.tagOpt) || true
gc_auto=$(in_pod git -C "$clone" config --get gc.auto) || true
maintenance_auto=$(in_pod git -C "$clone" config --get maintenance.auto) || true
fetch_tags=$(in_pod env XDG_CONFIG_HOME=/var/run/legion/home/.config jj -R "$clone" --ignore-working-copy config get remotes.origin.fetch-tags) || true
[ "$fetch_specs" = "$expected_fetch" ] || fail "remote.origin.fetch is $(printf %q "$fetch_specs"), want $(printf %q "$expected_fetch")"
[ "$tag_opt" = --no-tags ] || fail "remote.origin.tagOpt is $tag_opt, want --no-tags"
[ "$gc_auto" = 0 ] || fail "gc.auto is $gc_auto, want 0"
[ "$maintenance_auto" = false ] || fail "maintenance.auto is $maintenance_auto, want false"
[ "$fetch_tags" = '~*' ] || fail "jj remotes.origin.fetch-tags is $fetch_tags, want ~*"
note "remote.origin.fetch: $(tr '\n' ' ' <<<"$fetch_specs")"
note "remote.origin.tagOpt=$tag_opt gc.auto=$gc_auto maintenance.auto=$maintenance_auto remotes.origin.fetch-tags=$fetch_tags"
pass

begin fsck
for proof_pod in "$pod" "$second"; do
  fsck=$(kind_k -n legion exec "$proof_pod" -c "$container" -- git --git-dir="$clone/.git" fsck --connectivity-only --no-dangling --no-progress 2>&1) ||
    fail "git fsck --connectivity-only of pod $proof_pod's clone: $fsck"
  note "pod $proof_pod git fsck --connectivity-only of $clone: ok $(tr '\n' ' ' <<<"$fsck")"
done
pass

begin logs
# As the launched architect's agent runs them (role_enter_sh): inside its role-init's namespaces,
# as its shim's user. The launch driver's worker stream is gone once it exits, and the shim redials
# it without end, so the role's namespaces stay up for the checks.
role=architect
role_exec() { kind_k -n legion exec "$pod" -c "$container" -- sh -c "$role_enter_sh" role-enter "$role" "$@"; }
for _ in $(seq 1 90); do
  role_exec true >/dev/null 2>&1 && break
  sleep 2
done
git_log=$(role_exec git --git-dir="$clone/.git" log -1 --format='%H %s' refs/remotes/origin/master 2>&1) ||
  fail "git log of origin/master in the clone, as role $role: $git_log"
note "git log -1 origin/master → $git_log"
# shellcheck disable=SC2016 # a jj template, not the shell's
jj_log=$(role_exec sh -c "cd $clone && jj log --ignore-working-copy --no-graph --color=never -r 'master@origin' -T 'commit_id ++ \" \" ++ description.first_line() ++ \"\\n\"'" 2>&1) ||
  fail "jj log of master@origin in the clone, as role $role: $jj_log"
note "jj log -r master@origin → $jj_log"
[ "${git_log%% *}" = "${jj_log%% *}" ] || fail "git and jj read origin's master as ${git_log%% *} and ${jj_log%% *}"
pass

begin plain-fetch
git_fetch=$(role_exec git -C "$clone" fetch 2>&1) ||
  fail "plain git fetch as role $role: $git_fetch"
jj_fetch=$(role_exec sh -c "cd \"$clone\" && jj git fetch" 2>&1) ||
  fail "plain jj git fetch as role $role: $jj_fetch"
plain_refs=$(refs_in_clone)
want=$(expected_refs "$pod")
[ "$plain_refs" = "$want" ] || fail "the clone refs after plain fetch are $(printf %q "$plain_refs"), want $(printf %q "$want")"
note "plain git fetch: $(tr '\n' ' ' <<<"$git_fetch")"
note "plain jj git fetch: $(tr '\n' ' ' <<<"$jj_fetch")"
note "refs after plain fetch: $(tr '\n' ' ' <<<"$plain_refs")"
pass

begin named-fetch
other_branch=$(role_exec git -C "$clone" ls-remote --heads origin |
  awk '$2 != "refs/heads/master" { sub("^refs/heads/", "", $2); print $2; exit }')
[ -n "$other_branch" ] || fail "tmux/tmux names no branch other than master"
named_fetch=$(role_exec jj -R "$clone" git fetch --branch "$other_branch" 2>&1) ||
  fail "jj git fetch --branch $other_branch as role $role: $named_fetch"
# shellcheck disable=SC2016 # a jj template, not the shell's
other_row=$(role_exec jj -R "$clone" --ignore-working-copy log --no-graph --color=never -r "$other_branch@origin" -T 'commit_id ++ "\n"' 2>&1) ||
  fail "jj has no $other_branch@origin after its named fetch: $other_row"
note "jj git fetch --branch $other_branch: $(tr '\n' ' ' <<<"$named_fetch")"
note "jj log -r $other_branch@origin: $(tr '\n' ' ' <<<"$other_row")"
pass

begin cache-second-pod
fetched=$(kind_k -n legion logs "$second" -c workspace-init |
  grep -E "^workspace-init: [0-9,]+ bytes from the repository cache http://$cache_sandbox\.legion\.svc:[0-9]+ in .+; [0-9,]+ bytes from GitHub for the delta \([0-9]+ refs?\)$" |
  tail -1) ||
  fail "pod $second's workspace-init log names no stream from repository cache $cache_sandbox: $(kind_k -n legion logs "$second" -c workspace-init | tail -5)"
delta=$(sed -E 's/.*; ([0-9,]+) bytes from GitHub for the delta.*/\1/' <<<"$fetched" | tr -d ,)
[[ $delta =~ ^[0-9]+$ ]] && [ "$delta" -lt 1048576 ] || fail "pod $second fetched $delta bytes from GitHub beside the cache's tar, want under 1 MiB: $fetched"
note "workspace-init: $fetched"
pass

begin cache-default-only
cache_clone=/legion/cache/$repo.git
health=$(kind_k -n legion exec "$cache_sandbox" -c cache -- sh -c "curl -s -w '\n%{http_code}' localhost:8418/healthz") ||
  fail "read cache $cache_sandbox health: $health"
health_code=${health##*$'\n'}
health_json=${health%$'\n'*}
[ "$health_code" = 200 ] || fail "cache $cache_sandbox /healthz returned $health_code: $health_json"
default_branch_only=$(jq -r '.defaultBranchOnly // false' <<<"$health_json") ||
  fail "cache $cache_sandbox /healthz is not JSON: $health_json"
[ "$default_branch_only" = true ] || fail "cache $cache_sandbox /healthz defaultBranchOnly is $default_branch_only, want true"
cache_refs=$(kind_k -n legion exec "$cache_sandbox" -c cache -- git --git-dir="$cache_clone" for-each-ref --format='%(refname)') ||
  fail "list cache refs: $cache_refs"
[ "$cache_refs" = refs/heads/master ] || fail "cache refs are $(printf %q "$cache_refs"), want refs/heads/master alone"
# shellcheck disable=SC2016 # expanded by the cache pod's shell
index_objects=$(kind_k -n legion exec "$cache_sandbox" -c cache -- sh -c 'tmp=$(mktemp) || exit; trap "rm -f \"$tmp\"" EXIT; for index in "$1"/objects/pack/*.idx; do [ -e "$index" ] || continue; git show-index < "$index" > "$tmp" || exit; cut -d" " -f2 < "$tmp" || exit; done' sh "$cache_clone") ||
  fail "list cache pack indexes: $index_objects"
[ -n "$index_objects" ] || fail "cache $cache_sandbox has no pack index under $cache_clone/objects/pack"
# shellcheck disable=SC2016 # expanded by the cache pod's shell
master_objects=$(kind_k -n legion exec "$cache_sandbox" -c cache -- sh -c 'tmp=$(mktemp) || exit; trap "rm -f \"$tmp\"" EXIT; git --git-dir="$1" rev-list --objects master > "$tmp" || exit; cut -d" " -f1 < "$tmp"' sh "$cache_clone") ||
  fail "list cache master objects: $master_objects"
missing=$(comm -23 <(printf '%s\n' "$index_objects" | sort -u) <(printf '%s\n' "$master_objects" | sort -u))
[ -z "$missing" ] || fail "cache indexes name objects outside master: $(tr '\n' ' ' <<<"$missing")"
note "cache /healthz defaultBranchOnly=$default_branch_only; refs: $cache_refs"
note "cache indexes: $(printf '%s\n' "$index_objects" | sed '/^$/d' | wc -l) objects, all in master"
pass
run_end
