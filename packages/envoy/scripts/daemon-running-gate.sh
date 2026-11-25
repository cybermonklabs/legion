#!/usr/bin/env bash
# packages/envoy/scripts/daemon-running-gate.sh --deployment <owner/repo> [--context <kube-context>] <commit>
#
# Says whether every Legion daemon that runs now runs a build of cybermonklabs/legion at or after
# <commit>: plan C's running-daemons reading, taken once the deploy that drops the shared wake filter
# runs in production. daemon-release-gate.sh reads the deployment's image pins, which say what should
# run; this reads what does run. A daemon still running an earlier build on its static grant can
# update a durable's filters, so a pin alone cannot show the daemons are past <commit>.
#
# The deployment's Legion projects are read as daemon-release-gate.sh reads them, through the same
# file (lib/legion-deployment-projects.sh), so the two never disagree on which daemons run: a
# project at `replicas: 0` or `daemon: false` is SKIPPED, and one on a runtime other than kubernetes
# (a tmux host) is UNKNOWN, by name. For each project that runs a daemon, in the namespace
# legion.ProjectNamespace gives it (`legion`, or `legion-<key in lower case>` for a key the
# deployment's shared/legion/privateprojects/lists.go declares private) and the Deployment the
# deployment's legion program names (`legion-daemon` for SMOKE, else
# `legion-<key in lower case>-daemon`), read with kubectl:
#
#   - the rollout is complete: status.observedGeneration equals metadata.generation, and
#     spec.replicas, status.updatedReplicas, status.readyReplicas and status.replicas are all equal;
#     and no ReplicaSet the Deployment owns, other than the one of its current revision, holds a pod;
#   - every running pod of the Deployment (by its selector), each container: its imageID's digest
#     (not the pod template's image, which is only a pin) has an image config, read with
#     `crane config <repository>@<digest>`, whose org.opencontainers.image.revision has <commit> as
#     an ancestor (`git merge-base --is-ancestor`, in the clone this script is in). A container
#     whose image names another org.opencontainers.image.source than this repository (or a name the
#     deployment's repositoryRenames make one repository with it) is listed and not judged;
#   - the daemon's own report of its build: `kubectl exec <pod> -c legion -- legion version` prints
#     `commit <revision>`, equal to that container's image label.
#
# Per project it prints CURRENT, SKIPPED, GATE SHUT naming the pod, container, digest and revision
# or the rollout field that differs, or UNKNOWN naming any read it could not make (the projects,
# the private list, a Deployment, ReplicaSet or pod list kubectl could not read, an image config
# crane could not read, a revision git does not hold, an exec refused or printing no commit). The
# last line is GATE OPEN (exit 0) only when every running daemon is current and at least one was
# read, GATE SHUT (exit 1) when any is not, and UNKNOWN (exit 3) otherwise. A usage error exits 2
# and reads nothing.
#
# It needs bash, gh (read access to the deployment repository), python3 with PyYAML, jq, kubectl
# (signed in to the cluster that runs the daemons; --context, else the current context), crane and
# git, run from a clone of cybermonklabs/legion that holds <commit>.
set -euo pipefail

usage() {
  [ -z "${1:-}" ] || echo "daemon-running-gate.sh: $1" >&2
  echo "usage: daemon-running-gate.sh --deployment <owner/repo> [--context <kube-context>] <commit>" >&2
  exit 2
}

deployment=""
context=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --deployment) [ "$#" -ge 2 ] || usage "--deployment takes a value"; deployment=$2; shift 2 ;;
    --context) [ "$#" -ge 2 ] || usage "--context takes a value"; context=$2; shift 2 ;;
    -*) usage "unknown flag $1" ;;
    *) break ;;
  esac
done
[ "$#" -eq 1 ] && [[ $1 =~ ^[0-9a-f]{7,40}$ ]] || usage "one <commit>, 7 to 40 hex digits, follows the flags"
[ -n "$deployment" ] || usage "--deployment <owner/repo> is required"
[[ $deployment =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || usage "--deployment ${deployment} is not <owner>/<repo>"
for tool in gh python3 jq kubectl crane git; do
  command -v "$tool" >/dev/null 2>&1 || usage "$tool is required and is not on PATH"
done

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
readonly commit=$1 deployment context here
readonly legion_source="https://github.com/cybermonklabs/legion"
work=$(mktemp -d)
readonly work
trap 'rm -rf "$work"' EXIT

# shellcheck source=lib/legion-deployment-projects.sh
. "$here/lib/legion-deployment-projects.sh"

kc() {
  if [ -n "$context" ]; then
    kubectl --context "$context" --request-timeout=60s "$@"
  else
    kubectl --request-timeout=60s "$@"
  fi
}

shut=0
unknown=0
current=0

say_shut() {
  printf 'GATE SHUT: %s\n' "$1"
  shut=$((shut + 1))
}
say_unknown() {
  printf 'UNKNOWN: %s\n' "$1"
  unknown=$((unknown + 1))
}

if ! git -C "$here" cat-file -e "${commit}^{commit}" 2>/dev/null; then
  say_unknown "this clone holds no commit ${commit}, so no revision can be compared with it"
  echo UNKNOWN
  exit 3
fi

# private_keys: the keys the deployment declares private (shared/legion/privateprojects/lists.go,
# the `projects` list), one per line, into $work/private; fails naming what it could not read.
private_keys() {
  if ! fetch shared/legion/privateprojects/lists.go "$work/lists.go"; then
    private_error=$fetch_error
    return 1
  fi
  if ! awk '
      /^var projects = \[\]Project\{\}/ { found = 1; exit }
      /^var projects = \[\]Project\{/ { found = 1; inside = 1; next }
      inside && /^\}/ { exit }
      inside { while (match($0, /Key: *"[A-Z][A-Z0-9]*"/)) { key = substr($0, RSTART, RLENGTH); sub(/Key: *"/, "", key); sub(/"$/, "", key); print key; $0 = substr($0, RSTART + RLENGTH) } }
      END { if (!found) exit 1 }' "$work/lists.go" >"$work/private"; then
    private_error="${deployment} shared/legion/privateprojects/lists.go declares no \`var projects = []Project{\` list"
    return 1
  fi
}

load_entries
if [ "$entries_state" = failed ]; then
  say_unknown "cannot read ${deployment} Legion projects: ${entries_error}"
  echo UNKNOWN
  exit 3
fi
load_aliases
if ! private_keys; then
  say_unknown "$private_error"
  echo UNKNOWN
  exit 3
fi

# projects: key, then RUN, SKIP and why, or UNKNOWN and why, one line each.
jq -r '
  if .found | not then "-\tUNKNOWN\tthe deployment declares no project: \(.missing)"
  else .entries[]
    | (.key // "") as $key
    | if ($key | test("^[A-Z][A-Z0-9]*$") | not) then "\($key)\tUNKNOWN\tits key is no project key"
      elif has("replicas") and ((.replicas | type) != "number" or .replicas < 0) then "\($key)\tUNKNOWN\treplicas \(.replicas | tojson), which is no count of replicas"
      elif .replicas == 0 then "\($key)\tSKIP\treplicas: 0"
      elif .daemon == false then "\($key)\tSKIP\tdaemon: false"
      elif (.runtime // "kubernetes") != "kubernetes" then "\($key)\tUNKNOWN\tits daemon runs on runtime \(.runtime | tojson), not in a pod this gate reads"
      else "\($key)\tRUN\t-"
      end
  end' "$work/entries.json" >"$work/daemons"

# ours <source>: whether an image's source is this repository or a recorded rename of it.
ours() {
  local name=${1#https://github.com/}
  name=${name%.git}
  name=$(printf '%s' "$name" | tr 'A-Z' 'a-z')
  [ "$name" = cybermonklabs/legion ] && return 0
  [ "$aliases_state" = read ] && grep -qxF "$name" "$work/aliases"
}

# daemon <key>: reads one running daemon and prints its lines; returns 0 when it is current.
daemon() {
  local key=$1 lower namespace name deployment_json uid revision selector rs_json pods_json
  lower=$(printf '%s' "$key" | tr 'A-Z' 'a-z')
  namespace=legion
  grep -qxF "$key" "$work/private" && namespace="legion-${lower}"
  name="legion-${lower}-daemon"
  [ "$key" != SMOKE ] || name=legion-daemon
  local where="${key} (${namespace}/${name})"
  local before_shut=$shut before_unknown=$unknown

  if ! kc -n "$namespace" get deployment "$name" -o json >"$work/deployment.json" 2>"$work/err"; then
    say_unknown "${where}: cannot read the Deployment: $(first_line "$work/err")"
    return
  fi
  local rollout
  rollout=$(jq -r '
    [ if (.status.observedGeneration // 0) != .metadata.generation then "status.observedGeneration \(.status.observedGeneration // 0), metadata.generation \(.metadata.generation)" else empty end,
      (.spec.replicas // 1) as $want
      | ([["status.updatedReplicas", (.status.updatedReplicas // 0)], ["status.readyReplicas", (.status.readyReplicas // 0)], ["status.replicas", (.status.replicas // 0)]]
         | map(select(.[1] != $want)) | map("\(.[0]) \(.[1]), spec.replicas \($want)") | .[]) ] | join("; ")' "$work/deployment.json")
  [ -z "$rollout" ] || say_shut "${where}: the rollout is not complete: ${rollout}"
  uid=$(jq -r '.metadata.uid' "$work/deployment.json")
  revision=$(jq -r '.metadata.annotations["deployment.kubernetes.io/revision"] // ""' "$work/deployment.json")
  selector=$(jq -r '.spec.selector.matchLabels // {} | to_entries | map("\(.key)=\(.value)") | join(",")' "$work/deployment.json")
  if [ -z "$selector" ]; then
    say_unknown "${where}: the Deployment names no matchLabels selector"
    return
  fi

  if ! kc -n "$namespace" get replicasets -o json >"$work/rs.json" 2>"$work/err"; then
    say_unknown "${where}: cannot read the ReplicaSets: $(first_line "$work/err")"
  else
    jq -r --arg uid "$uid" --arg revision "$revision" '
      .items[] | select(any(.metadata.ownerReferences[]?; .uid == $uid))
      | select((.metadata.annotations["deployment.kubernetes.io/revision"] // "") != $revision)
      | select((.status.replicas // 0) > 0)
      | "\(.metadata.name) (revision \(.metadata.annotations["deployment.kubernetes.io/revision"] // "none")) holds \(.status.replicas) pod(s)"' \
      "$work/rs.json" >"$work/old-rs"
    while IFS= read -r line; do
      say_shut "${where}: an old ReplicaSet still holds a pod: $(printable "$line")"
    done <"$work/old-rs"
  fi

  if ! kc -n "$namespace" get pods -l "$selector" -o json >"$work/pods.json" 2>"$work/err"; then
    say_unknown "${where}: cannot read the pods: $(first_line "$work/err")"
    return
  fi
  jq -r '.items[] | select(.status.phase == "Running") | .metadata.name as $pod
    | .status.containerStatuses[]? | [$pod, .name, (.imageID // "")] | @tsv' "$work/pods.json" >"$work/containers"
  if [ ! -s "$work/containers" ]; then
    say_unknown "${where}: no running pod of the Deployment was found"
    return
  fi
  local pod container image_id ref labels source label_revision answer
  while IFS=$'\t' read -r pod container image_id; do
    ref=${image_id#docker-pullable://}
    if ! [[ $ref =~ ^[A-Za-z0-9][A-Za-z0-9._/:-]*@sha256:[0-9a-f]{64}$ ]]; then
      say_unknown "${where}: pod ${pod} container ${container} reports imageID $(printable "$image_id"), which names no digest"
      continue
    fi
    if ! labels=$(crane config "$ref" 2>"$work/err" | jq -c '.config.Labels // {}' 2>>"$work/err"); then
      say_unknown "${where}: cannot read the image config of ${ref}: $(first_line "$work/err")"
      continue
    fi
    source=$(jq -r '.["org.opencontainers.image.source"] // ""' <<<"$labels")
    label_revision=$(jq -r '.["org.opencontainers.image.revision"] // ""' <<<"$labels")
    if ! ours "$source"; then
      printf 'LISTED: %s: pod %s container %s runs %s, built from %s, not judged\n' "$where" "$pod" "$container" "$ref" "$(printable "$source")"
      continue
    fi
    if ! [[ $label_revision =~ ^[0-9a-f]{7,40}$ ]]; then
      say_unknown "${where}: pod ${pod} container ${container} runs ${ref}, whose revision label $(printable "$label_revision") is no commit id"
      continue
    fi
    if ! git -C "$here" cat-file -e "${label_revision}^{commit}" 2>/dev/null; then
      say_unknown "${where}: pod ${pod} container ${container} runs ${ref} at revision ${label_revision}, which this clone does not hold (fetch it)"
      continue
    fi
    if git -C "$here" merge-base --is-ancestor "$commit" "$label_revision" 2>/dev/null; then
      :
    else
      case $? in
        1) say_shut "${where}: pod ${pod} container ${container} runs ${ref} at revision ${label_revision}, which is not at or after ${commit}"; continue ;;
        *) say_unknown "${where}: git could not compare ${commit} with ${label_revision}"; continue ;;
      esac
    fi
    if [ "$container" = legion ]; then
      if ! answer=$(kc -n "$namespace" exec "$pod" -c legion -- legion version 2>"$work/err"); then
        say_unknown "${where}: kubectl exec ${pod} -c legion -- legion version was refused or failed: $(first_line "$work/err")"
        continue
      fi
      local reported
      reported=$(sed -n 's/^legion .* commit \([0-9a-f]\{7,40\}\)$/\1/p' <<<"$answer" | head -n 1)
      if [ -z "$reported" ]; then
        say_unknown "${where}: legion version in pod ${pod} names no commit: $(printable "$(head -n 1 <<<"$answer")")"
      elif [ "$reported" != "$label_revision" ]; then
        say_shut "${where}: legion version in pod ${pod} names commit ${reported}, not its image's revision ${label_revision}"
      fi
    fi
  done <"$work/containers"
  if [ "$shut" = "$before_shut" ] && [ "$unknown" = "$before_unknown" ]; then
    printf 'CURRENT: %s: every running container at or after %s\n' "$where" "$commit"
    current=$((current + 1))
  fi
}

while IFS=$'\t' read -r key what detail; do
  case "$what" in
    SKIP) printf 'SKIPPED: %s runs no daemon (%s)\n' "$key" "$detail" ;;
    UNKNOWN) say_unknown "$(printable "$key"): $(printable "$detail")" ;;
    RUN) daemon "$key" ;;
  esac
done <"$work/daemons"

if [ "$shut" -gt 0 ]; then
  echo "GATE SHUT"
  exit 1
fi
if [ "$unknown" -gt 0 ] || [ "$current" -eq 0 ]; then
  [ "$current" -gt 0 ] || [ "$unknown" -gt 0 ] || echo "UNKNOWN: the deployment's Legion projects run no daemon, so none was read"
  echo "UNKNOWN"
  exit 3
fi
echo "GATE OPEN"
