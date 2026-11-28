#!/usr/bin/env bash
# The issue-pod start proof under gVisor, at the base and at the head. An issue pod of a large
# repository spends its start in workspace-init, and most of that in jj's checkout of the tree:
# this proof starts two issue pods of rust-lang/crates.io-index (389,087 files, 6.93 GB in its tree)
# in each of two arms, `base` and then `head`, on one runner, and holds the head's workspace-init
# run of its second pod to at most half of the base's. Each arm is a throwaway kind cluster of its
# own, made and deleted in turn. `head` is the checkout's commit; `base` is its merge base with
# main, which GitHub's anonymous compare answer names
# (https://api.github.com/repos/cybermonklabs/legion/compare/main...<head>, .merge_base_commit.sha),
# the one call the run makes to api.github.com: where that call answers 403 or 429, the anonymous
# rate limit, the run ends void, exit 2, naming the answer. Each arm's pods run the worker image of
# its own commit, `ghcr.io/cybermonklabs/legion-worker:sha-<the first 12 hex of it>` by digest, and
# the run fails naming the tag when the registry has none. Both arms run this checkout's script and
# its driver, which build the same pod for both images: the pod template, the init container's flags
# and its environment are the same at both commits.
#
# The pods are built by Legion's own code, as shared-clone-kind.sh's are: the daemon's loader reads
# the driver's legion.yaml, and its sandboxOptions builds the Agent Sandbox runtime that launches
# each issue's root (internal/daemon's issue_pod_start_live_test.go). The issue pod's CPU limit is
# 4 and its memory 12Gi in both arms (issue_pod_start_kind_test.go's issuePodStartLiveResources).
# The driver launches the first issue, whose launch makes the repository cache, waits for the
# cache's Ready, reads the volume the first pod's claim is bound to, ends the first pod through the
# runtime's release, and launches the second once this script has read GitHub's tip of the default
# branch (T0). The script reads the tip again once the driver returns (T1), then reads the second
# pod: its init containers, the run of its workspace-init and the time from its creation to its
# roles container's start (from the typed Pod the driver read), its workspace-init log (at the head,
# a duration on every step line and the steps within 10 s of the run; at the base, its summary
# line's two durations), its workspace as the launched architect sees it (`jj sparse list`, `jj
# diff`, `git status`, and the counts of `git ls-files`, `git ls-tree` and `find`), its start commit
# against T0 and T1, its clone's refs and the two pods' volumes. A container the kernel killed for
# memory (OOMKilled) fails the run, naming it. Each check prints `== <name>`, what it observed and
# `ok <name> (<s>s)`; the first that fails ends the run non-zero, naming it. The last check, the
# verdict, prints `ARM <base|head> commit=<sha> image=<image by digest> run=<s>s
# create-to-roles=<s>s` and the second pod's workspace-init log for each arm, then the ratio of the
# two runs.
#
# `--small` is issue-pod-start-small-kind.sh's run: the same proof on tmux/tmux, whose verdict is
# that the head's run is at most the base's plus 10 s, the before-merge reading of "a pod of a small
# repository starts no slower".
#
# Run it from a checkout on an amd64 (x86_64) Linux host whose HEAD is pushed to
# cybermonklabs/legion, as
#   bash scripts/e2e/issue-pod-start-kind.sh
# with docker, go, git, kubectl, curl, jq, zstd and a tar that supports --zstd installed. It
# installs kind v0.33.0 by sha256 and makes each cluster on that release's node image as
# .github/ci-images.yaml copies it by digest (lib/kind-sandbox.sh's kind_sandbox_install_kind),
# unless LEGION_E2E_KIND names a kind binary, which then makes them on that kind's own default. Its
# busybox is the copy that file makes too. The node's local-path volumes, three copies of the
# repository's history and two of its tree, go to a host directory with at least 30 GB free:
# /mnt/legion-e2e, made with sudo, on a GitHub-hosted runner (RUNNER_ENVIRONMENT=github-hosted),
# and a directory of the run's work directory elsewhere. The run reads that directory's free space
# before each arm and ends void, exit 2, naming the bytes it needs and has, below 30 GB; after each
# arm's cluster is deleted it removes that arm's own subdirectory, which kind delete cluster leaves.
# .github/e2e-scripts.json marks this proof dispatch_only: e2e.yaml runs it on
# `gh workflow run e2e.yaml --ref <branch> -f script=issue-pod-start-kind`, whose checkout is the
# branch's head, for which worker-image.yaml publishes the head's image. Every kubectl call names
# its arm's kubeconfig and context, so the caller's kubeconfig is never read, and each cluster is
# deleted on any exit. ISSUE_POD_START_EVIDENCE_DIR (ISSUE_POD_START_SMALL_EVIDENCE_DIR for
# --small; default a fresh /tmp directory, kept and printed) keeps the transcript and, for each arm,
# the legion.yaml, the driver's result, the second pod's manifest and its workspace-init log.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
repository=rust-lang/crates.io-index default_branch=master verdict=half proof="issue pod start kind e2e"
evidence=${ISSUE_POD_START_EVIDENCE_DIR:-}
if [ "$#" -gt 0 ]; then
  if [ "$#" -ne 1 ] || [ "$1" != --small ]; then
    echo "usage: bash scripts/e2e/issue-pod-start-kind.sh [--small]" >&2
    exit 2
  fi
  repository=tmux/tmux default_branch=master verdict=no-slower proof="issue pod start small kind e2e"
  evidence=${ISSUE_POD_START_SMALL_EVIDENCE_DIR:-}
fi
evidence=${evidence:-$(mktemp -d /tmp/legion-e2e-issue-pod-start-evidence.XXXXXXXX)}
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

run_id=legion-issue-pod-start-$$
work=$(mktemp -d "/tmp/legion-e2e-issue-pod-start.$$.XXXXXXXX")
# The free space each arm needs on the volumes' host directory: the repository cache's clone and
# each issue pod's clone and checkout (about 8 GB each for rust-lang/crates.io-index), with room.
volume_bytes=30000000000
check=setup
started=$SECONDS
check_started=$SECONDS
void_reason=
# The arm in progress: its cluster, its evidence directory and its volumes' directory.
cluster=
arm_evidence=$evidence
arm_volumes=

begin() { check=$1; check_started=$SECONDS; echo "== $check"; }
note() { echo "   $*"; }
pass() { echo "ok $check ($((SECONDS - check_started))s)"; }
fail() { echo "FAIL $check: $*" >&2; exit 1; }
# void ends the run with exit 2: what it would measure cannot be measured on this runner now.
void() { void_reason=$*; echo "VOID $check: $*" >&2; exit 2; }

# remove_arm_volumes removes the arm's own subdirectory of the volumes' host directory, by its exact
# path, once its cluster is gone. kind's node wrote it as root and the pods as their own users, so a
# busybox container removes it.
remove_arm_volumes() {
  [ -n "$arm_volumes" ] || return 0
  docker run --rm --cpus 1 --memory 256m -v "$volumes:$volumes" "$kind_busybox" rm -rf -- "$arm_volumes" ||
    echo "could not remove $arm_volumes, which the arm's volumes are in; remove it by hand" >&2
  arm_volumes=
}

# cleanup tears the arm in progress down and prints the run's last line through lib/run-verdict.sh:
# PASS only for a run that reached run_end, the script's last statement.
cleanup() {
  local status=$? ended failed
  ended=$(run_status "$status")
  [ -z "$cluster" ] || docker rm -f "$cluster-driver" >/dev/null 2>&1 || true
  kind_sandbox_teardown "$ended" "$arm_evidence"
  remove_arm_volumes
  rm -rf "$work" || echo "could not remove the work directory $work" >&2
  failed="$proof: FAIL at $check after $((SECONDS - started))s (evidence: $evidence)"
  [ -z "$void_reason" ] || failed="$proof: VOID at $check after $((SECONDS - started))s: $void_reason (evidence: $evidence)"
  run_verdict "$status" "$proof: PASS in $((SECONDS - started))s (evidence: $evidence)" "$failed"
}
trap cleanup EXIT

# on_kind NAME OPTION… -- COMMAND… runs COMMAND, a static binary of the work directory, in a
# busybox container (kind_busybox) on the kind network beside the node, with each docker run
# OPTION, as shared-clone-kind.sh's does: the pods reach the kind network's containers, while a
# host firewall can drop what they send the host at the network's gateway. Run in the background,
# it leaves the shell free to answer the driver's handshake.
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

# worker_image COMMIT is the image the arm of COMMIT runs: the digest the registry gives that
# commit's sha- tag, read as shared-clone-kind.sh's worker_image reads the checkout's own.
worker_image() {
  local tag token digest
  tag=sha-$(cut -c1-12 <<<"$1")
  token=$(curl -fsS "https://ghcr.io/token?scope=repository:cybermonklabs/legion-worker:pull" | jq -er .token) ||
    fail "no anonymous pull token for ghcr.io/cybermonklabs/legion-worker"
  # The pull token goes to curl on standard input, never as an argument every process can read.
  digest=$(printf 'Authorization: Bearer %s\n' "$token" | curl -fsSI -H @- \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
    "https://ghcr.io/v2/cybermonklabs/legion-worker/manifests/$tag" | tr -d '\r' | awk 'tolower($1) == "docker-content-digest:" {print $2}') || true
  [[ $digest == sha256:* ]] ||
    fail "ghcr.io/cybermonklabs/legion-worker has no $tag, the worker image of $1; run worker-image.yaml on a branch at that commit (gh workflow run worker-image.yaml --repo cybermonklabs/legion --ref <branch>)"
  echo "ghcr.io/cybermonklabs/legion-worker@$digest"
}

# github_tip prints GitHub's tip of the repository's default branch, as git ls-remote reads it. git's
# own stderr, a refusal's reason among it, goes to the transcript.
github_tip() {
  local answer
  answer=$(GIT_TERMINAL_PROMPT=0 git ls-remote "https://github.com/$repository" "refs/heads/$default_branch") ||
    fail "git ls-remote https://github.com/$repository refs/heads/$default_branch failed"
  answer=${answer%%[[:space:]]*}
  [[ $answer =~ ^[0-9a-f]{40}$ ]] || fail "git ls-remote names no $default_branch of https://github.com/$repository: $answer"
  echo "$answer"
}

# result_value KEY prints the value the driver's result gives KEY (`<key> <value>` lines).
result_value() {
  awk -v key="$1" '$1 == key { print $2; found = 1 } END { exit !found }' "$result" ||
    fail "the launch driver's result names no $1: $(tr '\n' ' ' <"$result")"
}

# in_workspace COMMAND… runs COMMAND in the second pod's workspace as the launched architect's agent
# runs it (role_enter_sh): inside its role-init's namespaces, as its shim's user. Its stdout is the
# function's, and its stderr goes to $arm_work/stderr, which read_in_workspace reports.
in_workspace() {
  # shellcheck disable=SC2016 # expanded by the role's shell
  kind_k -n legion exec "$pod" -c roles -- sh -c "$role_enter_sh" role-enter "$role" \
    sh -c 'cd "$1" && shift && exec "$@"' in-workspace "$workspace" "$@" 2>"$arm_work/stderr"
}

# read_in_workspace COMMAND… is in_workspace's stdout, and fails naming COMMAND and its stderr
# where COMMAND exits non-zero. Its stderr, where there is any, is noted.
read_in_workspace() {
  local out
  out=$(in_workspace "$@") || fail "$* in $workspace, as role $role: $(cat "$arm_work/stderr")"
  [ ! -s "$arm_work/stderr" ] || note "$* wrote on stderr: $(tr '\n' ' ' <"$arm_work/stderr")" >&2
  printf '%s' "$out"
}

# run_arm ARM COMMIT IMAGE makes ARM's cluster, starts its two issue pods on IMAGE, reads its second
# pod, and deletes the cluster and its volumes. It leaves the second pod's run in arm_run, its
# creation-to-roles time in arm_create_to_roles, and its workspace-init log in
# $evidence/ARM/workspace-init.log.
run_arm() {
  arm=$1
  local commit=$2 image=$3
  cluster=$run_id-$arm arm_evidence=$evidence/$arm arm_work=$work/$arm
  mkdir -p "$arm_evidence" "$arm_work"

  begin "$arm-disk"
  local have
  have=$(df -B1 --output=avail "$volumes" | tail -n 1 | tr -d ' ')
  [[ $have =~ ^[0-9]+$ ]] || fail "df gives no free space of $volumes: $have"
  [ "$have" -ge "$volume_bytes" ] ||
    void "the arm's volumes need $volume_bytes bytes free in $volumes, and it has $have"
  arm_volumes=$volumes/$cluster
  mkdir -p "$arm_volumes"
  note "$volumes has $have bytes free; the arm's volumes go to $arm_volumes"
  pass

  begin "$arm-cluster"
  kind_sandbox_create "$cluster" "$arm_work" "$arm_volumes"
  pass

  begin "$arm-agent-sandbox"
  kind_sandbox_install_agent_sandbox "$arm_work"
  pass

  begin "$arm-gvisor"
  kind_sandbox_install_gvisor "$arm_work"
  pass

  begin "$arm-images"
  kind_sandbox_load_image "$image"
  kind_sandbox_internal_kubeconfig "$arm_work/kubeconfig-internal"
  note "the arm of $commit runs $image"
  pass

  begin "$arm-launch"
  result=$arm_work/result
  local first_ended=$arm_work/first-ended tip_before=$arm_work/tip-before driver tip_t0='' tip_t1
  # shellcheck disable=SC2016 # expanded by the driver container's shell
  on_kind driver \
    -e LEGION_E2E_KUBECONFIG="$arm_work/kubeconfig-internal" -e LEGION_E2E_KUBE_CONTEXT="$kind_context" \
    -e LEGION_E2E_IMAGE="$image" -e LEGION_E2E_WORK="$arm_work" -e LEGION_E2E_RESULT="$result" \
    -e LEGION_E2E_REPOSITORY="$repository" -e LEGION_E2E_DEFAULT_BRANCH="$default_branch" \
    -e LEGION_E2E_FIRST_ENDED="$first_ended" -e LEGION_E2E_TIP_BEFORE="$tip_before" -- \
    sh -c 'LEGION_E2E_STREAM_HOST=$(ip -4 -o addr show eth0 | awk "{print \$4}" | cut -d/ -f1) exec "$0" -test.count=1 -test.timeout=110m -test.v -test.run "^TestTwoIssuePodsStartLive\$"' "$work/issue-pod-start.test" &
  driver=$!
  # The driver writes the first pod's name once that pod is gone, then waits for GitHub's tip,
  # read here just before it launches the second pod (T0).
  while [ ! -s "$first_ended" ] && kill -0 "$driver" 2>/dev/null; do
    sleep 2
  done
  if [ -s "$first_ended" ]; then
    tip_t0=$(github_tip)
    printf '%s\n' "$tip_t0" >"$tip_before.part"
    mv "$tip_before.part" "$tip_before"
  fi
  wait "$driver" || fail "the launch driver failed"
  # The driver returns once the second pod's workspace-init has ended and its roles started (T1).
  tip_t1=$(github_tip)
  [ -n "$tip_t0" ] || fail "the launch driver returned without ending the first pod"
  # One read per line: an assignment's status is its last substitution's, so a failed read before
  # it would pass unseen under errexit.
  first_pod=$(result_value first-pod)
  second_pod=$(result_value second-pod)
  second_issue=$(result_value second-issue)
  pod=${second_pod#*/}
  cp "$arm_work/issue-pod-start/legion.yaml" "$arm_evidence/legion.yaml"
  cp "$result" "$arm_evidence/result"
  kind_k -n legion get pod "$pod" -o yaml >"$arm_evidence/second-pod.yaml"
  note "first pod $first_pod, ended before the second; second pod $second_pod for $second_issue on $repository"
  note "GitHub's tip of $default_branch: $tip_t0 before the second launch (T0), $tip_t1 once it returned (T1)"
  pass

  begin "$arm-provision"
  local ends
  # shellcheck disable=SC2016 # a kubectl JSONPath, not the shell's
  ends=$(kind_k -n legion get pod "$pod" -o jsonpath='{range .status.initContainerStatuses[*]}{.name}={.state.terminated.exitCode} {end}')
  [ "$ends" = "workspace-init=0 " ] || fail "pod $pod's init containers ended $ends, want workspace-init=0 alone"
  note "pod $pod init containers: $ends"
  pass

  begin "$arm-times"
  local run create_to_roles
  run=$(result_value run)
  create_to_roles=$(result_value create-to-roles)
  [[ $run =~ ^[0-9]+$ ]] && [[ $create_to_roles =~ ^[0-9]+$ ]] ||
    fail "the driver's run $run and create-to-roles $create_to_roles are no whole seconds"
  arm_run=$run arm_create_to_roles=$create_to_roles
  note "pod $pod: workspace-init ran ${run}s (its finishedAt less its startedAt); ${create_to_roles}s from its creation to its roles container's start"
  pass

  begin "$arm-log"
  local log
  log=$(kind_k -n legion logs "$pod" -c workspace-init) || fail "kubectl logs $pod -c workspace-init: $log"
  printf '%s\n' "$log" >"$arm_evidence/workspace-init.log"
  workspace=$(sed -n 's|^workspace-init: \(/[^ ]*\) on [^ ]* from [^ ]*$|\1|p' <<<"$log" | tail -n 1)
  [ -n "$workspace" ] || fail "pod $pod's workspace-init log ends with no workspace line: $(tail -n 3 <<<"$log")"
  if [ "$arm" = head ]; then
    local steps bad sum total
    steps=$(grep '^workspace-init: step ' <<<"$log") || fail "pod $pod's workspace-init log has no step line"
    bad=$(grep -Ev '^workspace-init: step [a-z]+(-[a-z]+)* took [0-9]+\.[0-9]s$' <<<"$steps") || true
    [ -z "$bad" ] || fail "step lines with no duration: $(tr '\n' ' ' <<<"$bad")"
    total=$(sed -n 's/^workspace-init: steps took \([0-9]*\.[0-9]\)s$/\1/p' <<<"$log")
    [ -n "$total" ] || fail "pod $pod's workspace-init log has no steps took line"
    sum=$(awk '{ sub(/s$/, "", $5); sum += $5 } END { printf "%.1f", sum }' <<<"$steps")
    awk -v run="$run" -v sum="$sum" 'BEGIN { d = run - sum; if (d < 0) d = -d; exit !(d <= 10) }' ||
      fail "the step durations add up to ${sum}s, and pod $pod's workspace-init ran ${run}s: more than 10 s apart"
    note "$(wc -l <<<"$steps") step lines, each with a duration, adding up to ${sum}s (steps took ${total}s) against a run of ${run}s"
  else
    local summary
    summary=$(grep -E '^workspace-init: [0-9,]+ bytes from the repository cache .+ in [^ ]+, its history verified in [^;]+; [0-9,]+ bytes from GitHub for the delta \([0-9]+ refs?\)$' <<<"$log" | tail -n 1) ||
      fail "pod $pod's workspace-init log has no summary line of the repository cache: $(tail -n 3 <<<"$log")"
    note "the summary line's durations: the transfer $(sed -E 's/.* in ([^ ]+), its history verified in .*/\1/' <<<"$summary"), the history check $(sed -E 's/.*, its history verified in ([^;]+);.*/\1/' <<<"$summary")"
    note "$summary"
  fi
  note "the workspace: $workspace"
  pass

  begin "$arm-workspace"
  role=architect
  for _ in $(seq 1 90); do
    in_workspace true >/dev/null && break
    sleep 2
  done
  local sparse summary_diff stat status files tree found
  # jj diff --summary is the one jj command here that snapshots the working copy, reading every
  # file of it: jj sparse list needs no snapshot, and jj diff --stat reads the one --summary took
  # (--ignore-working-copy), so jj reads a large tree once per arm.
  sparse=$(read_in_workspace jj sparse list --ignore-working-copy)
  [ "$sparse" = . ] || fail "jj sparse list printed $(printf %q "$sparse"), want ."
  summary_diff=$(read_in_workspace jj diff --summary)
  [ -z "$summary_diff" ] || fail "jj diff --summary printed $(printf %q "$summary_diff"), want nothing"
  stat=$(read_in_workspace jj diff --stat --ignore-working-copy)
  [ "$stat" = '0 files changed, 0 insertions(+), 0 deletions(-)' ] ||
    fail "jj diff --stat printed $(printf %q "$stat"), want only 0 files changed, 0 insertions(+), 0 deletions(-)"
  status=$(read_in_workspace git status --porcelain --untracked-files=no)
  [ -z "$status" ] || fail "git status --porcelain --untracked-files=no printed $(printf %q "$status"), want nothing"
  files=$(read_in_workspace sh -c 'git ls-files | wc -l' | tr -d ' ')
  tree=$(read_in_workspace sh -c 'git ls-tree -r --name-only HEAD | wc -l' | tr -d ' ')
  found=$(read_in_workspace sh -c 'find . \( -path ./.git -o -path ./.jj \) -prune -o \( -type f -o -type l \) -print | wc -l' | tr -d ' ')
  [[ $files =~ ^[1-9][0-9]*$ ]] && [ "$files" = "$tree" ] && [ "$files" = "$found" ] ||
    fail "git ls-files counts $files, git ls-tree -r HEAD $tree and find $found files and links outside .git and .jj, want three equal counts"
  note "jj sparse list: $sparse; jj diff --summary: nothing; jj diff --stat: $stat; git status --porcelain --untracked-files=no: nothing"
  note "git ls-files, git ls-tree -r --name-only HEAD and find outside .git and .jj each count $files"
  pass

  begin "$arm-start-commit"
  local parent origin scratch
  clone=/legion/repos/github.com/$repository
  # shellcheck disable=SC2016 # a jj template, not the shell's
  parent=$(read_in_workspace jj log --ignore-working-copy --no-graph --color=never -r @- -T 'commit_id ++ "\n"')
  origin=$(kind_k -n legion exec "$pod" -c roles -- git -C "$clone" rev-parse "refs/remotes/origin/$default_branch") ||
    fail "git rev-parse refs/remotes/origin/$default_branch in pod $pod's $clone: $origin"
  [ "$parent" = "$origin" ] || fail "@- is $parent, and the clone's origin/$default_branch is $origin"
  if [ "$parent" = "$tip_t1" ]; then
    note "@- $parent is the clone's origin/$default_branch and GitHub's tip once the second pod started (T1)"
  else
    scratch=$arm_work/tips.git
    git init -q --bare "$scratch"
    git -C "$scratch" remote add origin "https://github.com/$repository"
    GIT_TERMINAL_PROMPT=0 git -C "$scratch" fetch -q --no-tags --filter=tree:0 origin "$tip_t1" ||
      fail "git fetch --filter=tree:0 of $tip_t1 from https://github.com/$repository"
    git -C "$scratch" merge-base --is-ancestor "$tip_t0" "$parent" ||
      fail "GitHub's tip before the second launch, $tip_t0 (T0), is no ancestor of @- $parent"
    git -C "$scratch" merge-base --is-ancestor "$parent" "$tip_t1" ||
      fail "@- $parent is no ancestor of GitHub's tip once the second pod started, $tip_t1 (T1)"
    note "@- $parent is the clone's origin/$default_branch, between T0 $tip_t0 and T1 $tip_t1 by git ancestry"
  fi
  pass

  begin "$arm-refs"
  local refs want
  refs=$(kind_k -n legion exec "$pod" -c roles -- git -C "$clone" for-each-ref --format='%(refname)') ||
    fail "list refs in pod $pod's $clone: $refs"
  refs=$(grep -v '^refs/jj/' <<<"$refs") || true
  want=$(printf 'refs/heads/legion/%s\nrefs/heads/%s\nrefs/remotes/origin/HEAD\nrefs/remotes/origin/%s\n' "$second_issue" "$default_branch" "$default_branch" | LC_ALL=C sort)
  [ "$refs" = "$want" ] || fail "pod $pod's clone refs outside refs/jj/ are $(printf %q "$refs"), want $(printf %q "$want")"
  note "git -C $clone for-each-ref outside refs/jj/: $(tr '\n' ' ' <<<"$refs")"
  pass

  begin "$arm-volumes"
  local first_claim first_volume second_claim second_volume
  first_claim=$(result_value first-claim)
  first_volume=$(result_value first-volume)
  second_claim=$(result_value second-claim)
  second_volume=$(result_value second-volume)
  [ "$first_claim" != "$second_claim" ] && [ "$first_volume" != "$second_volume" ] ||
    fail "pod $first_pod's claim $first_claim is bound to $first_volume, and pod $second_pod's claim $second_claim to $second_volume: want two claims on two volumes"
  note "claim $first_claim of $first_pod on volume $first_volume (read before that pod ended); claim $second_claim of $second_pod on volume $second_volume"
  pass

  begin "$arm-teardown"
  kind_sandbox_teardown 0 "$arm_evidence"
  remove_arm_volumes
  note "deleted kind cluster $cluster and the arm's volumes"
  pass
}

begin prerequisites
[ "$(uname -m)" = x86_64 ] || fail "the proof runs on amd64 (x86_64) only, since the gVisor release it installs and the binaries it builds are amd64; this host is $(uname -m)"
for tool in docker go git kubectl curl jq zstd tar sha256sum sha512sum df; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
if [ -n "${LEGION_E2E_KIND:-}" ]; then
  command -v "$kind_bin" >/dev/null || fail "LEGION_E2E_KIND names $kind_bin, which is not on PATH"
else
  kind_sandbox_install_kind "$work"
fi
note "kind $("$kind_bin" version)${kind_node_image:+, node image $kind_node_image}"
[[ $(tar --help 2>/dev/null) == *--zstd* ]] || fail "this tar has no --zstd; install GNU tar 1.31 or later"
if [ "${RUNNER_ENVIRONMENT:-}" = github-hosted ]; then
  volumes=/mnt/legion-e2e
  sudo mkdir -p "$volumes"
  sudo chown "$(id -u):$(id -g)" "$volumes"
else
  volumes=$work/volumes
  mkdir -p "$volumes"
fi
note "the arms' local-path volumes go under $volumes; the proof runs $repository ($default_branch)"
pass

begin arms
head=$(git -C "$root" rev-parse HEAD)
compare=$(curl -sS -w '\n%{http_code}' -H 'Accept: application/vnd.github+json' \
  "https://api.github.com/repos/cybermonklabs/legion/compare/main...$head") ||
  fail "GitHub's compare of main...$head could not be read"
code=${compare##*$'\n'} answer=${compare%$'\n'*}
case $code in
  200) ;;
  403 | 429)
    void "GitHub answered the anonymous compare of main...$head with $code (its rate limit): $(jq -r '.message // empty' <<<"$answer" 2>/dev/null || true)"
    ;;
  *) fail "GitHub answered the compare of main...$head with $code: $(head -c 500 <<<"$answer")" ;;
esac
base=$(jq -r '.merge_base_commit.sha // empty' <<<"$answer") || true
[[ $base =~ ^[0-9a-f]{40}$ ]] || fail "GitHub's compare of main...$head names no merge base commit: $(head -c 500 <<<"$answer")"
[ "$base" != "$head" ] || fail "$head is its own merge base with main, so the base arm and the head arm would run one commit"
base_image=$(worker_image "$base")
head_image=$(worker_image "$head")
note "head $head runs $head_image"
note "base $base, the merge base of main and the head, runs $base_image"
pass

begin binaries
CGO_ENABLED=0 go -C "$root/packages/daemon" test -c -tags e2e -o "$work/issue-pod-start.test" ./internal/daemon
pass

run_arm base "$base" "$base_image"
base_run=$arm_run base_create_to_roles=$arm_create_to_roles
run_arm head "$head" "$head_image"
head_run=$arm_run head_create_to_roles=$arm_create_to_roles

begin verdict
echo "ARM base commit=$base image=$base_image run=${base_run}s create-to-roles=${base_create_to_roles}s"
cat "$evidence/base/workspace-init.log"
echo "ARM head commit=$head image=$head_image run=${head_run}s create-to-roles=${head_create_to_roles}s"
cat "$evidence/head/workspace-init.log"
[ "$base_run" -gt 0 ] || fail "the base arm's workspace-init ran ${base_run}s, which no ratio can divide"
ratio=$(awk -v head="$head_run" -v base="$base_run" 'BEGIN { printf "%.2f", head / base }')
echo "ratio: the head's run is $ratio of the base's (${head_run}s / ${base_run}s)"
if [ "$verdict" = half ]; then
  [ $((2 * head_run)) -le "$base_run" ] ||
    fail "the head's workspace-init ran ${head_run}s, more than half of the base's ${base_run}s (ratio $ratio)"
else
  [ "$head_run" -le $((base_run + 10)) ] ||
    fail "the head's workspace-init ran ${head_run}s, more than the base's ${base_run}s plus 10 s"
fi
pass
run_end
