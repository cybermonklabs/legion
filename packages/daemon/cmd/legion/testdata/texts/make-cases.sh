#!/usr/bin/env bash
# make-cases.sh <dir>: writes the configurations `legion config check` is run over for an arm's
# folder of texts, one directory each under <dir>: six the check refuses, each naming its cause,
# and two it accepts. Each case directory holds its legion.yaml, whose every path is absolute and
# under <dir>, every other file the check reads before its first write (the operator's bearer file,
# and under runtime: kubernetes the Envoy and Dispatch bearer files and the kubeconfig), and the
# case's folders of texts. A folder holds Legion's role-prompt paths with texts of its own, beside
# a manifest.json recording the digest of Legion's own text at each path, read from this checkout
# (packages/daemon/internal/prompts), so each folder is refused for its case's reason alone.
#
# Refused:
#   gone                      the arm names a folder that does not exist
#   no-manifest               the folder holds no manifest.json
#   outside-mounts            under runtime: kubernetes, no pod mount holds the folder
#   unreached-part            the folder holds roles/planner.md; the experiment's roles are [reviewer]
#   claude-role               the folder is given to the reviewer, whose backend is claude
#   two-folders-one-role      two experiments, each with a folder of its own, both reach the reviewer
# Accepted, both under runtime: kubernetes with every folder a mount of the pods holds:
#   inside-mounts             the good folder of the refused cases
#   two-folders-different-roles  two experiments, roles [reviewer] and roles [architect], a folder each
#
# The refused cases run under runtime: tmux but for outside-mounts, and each is refused for its
# own reason wherever <dir> is. The two accepted ones mount a folder under <dir>, and the check
# refuses an operator's mount under /legion, which every Legion pod mounts for itself: give them a
# <dir> outside it (an issue pod's TMPDIR is under it).
#
# It needs bash, coreutils and sha256sum alone, and runs from any working directory. cmd/legion's
# TestConfigCheckRefusesEachBadTextsFolder runs it, as the check by hand does from
# packages/daemon:
#   bash cmd/legion/testdata/texts/make-cases.sh "$W/cases"
#   for c in "$W"/cases/*/; do "$W/legion" config check --config "$c/legion.yaml"; echo "exit $?"; done
set -euo pipefail

if [ "$#" -ne 1 ] || [ -z "$1" ]; then
  echo "usage: make-cases.sh <dir>" >&2
  exit 2
fi
mkdir -p -- "$1"
root=$(cd -- "$1" && pwd)
prompts=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../../internal/prompts" && pwd)
umask 077

# digest <path>: the digest of Legion's own text at path, a path of a folder of texts, as a
# manifest records it.
digest() {
  local sum
  sum=$(sha256sum -- "$prompts/$1")
  printf 'sha256:%s' "${sum%% *}"
}

# folder <dir> <path>...: a folder of texts at dir with a text of its own at each of Legion's
# paths, beside the manifest recording the digest of Legion's text at each.
folder() {
  local dir=$1 path entries='' separator=''
  shift
  for path in "$@"; do
    mkdir -p -- "$dir/$(dirname -- "$path")"
    printf "This folder's text in place of Legion's %s.\n" "$path" >"$dir/$path"
    entries+="$separator"$'\n'"    \"$path\": \"$(digest "$path")\""
    separator=,
  done
  printf '{\n  "files": {%s\n  }\n}\n' "$entries" >"$dir/manifest.json"
}

# start <case>: the case's directory, with the bearer files and the kubeconfig its legion.yaml may
# name.
start() {
  local dir="$root/$1"
  mkdir -p -- "$dir"
  printf 'operator\n' >"$dir/operator-token"
  printf 'envoy\n' >"$dir/envoy-token"
  printf 'dispatch\n' >"$dir/dispatch-token"
  # A context the check names and a server it never dials.
  cat >"$dir/kubeconfig" <<'EOF'
apiVersion: v1
kind: Config
clusters: [{name: example, cluster: {server: "https://192.0.2.20:6443"}}]
users: [{name: legion-daemon, user: {token: placeholder}}]
contexts: [{name: legion-daemon, context: {cluster: example, user: legion-daemon, namespace: legion}}]
current-context: legion-daemon
EOF
}

# tmux_yaml <case> <extra> <experiments>: the case's legion.yaml under runtime: tmux, extra its
# top-level lines and experiments the lines of projects.DEMO.experiments.
tmux_yaml() {
  local dir="$root/$1"
  cat >"$dir/legion.yaml" <<EOF
project: DEMO
port: 13370
postgres_dsn: postgres://127.0.0.1:1/legion
operator_token_file: $dir/operator-token
github_apps:
  implement: { app_id: "1", private_key: implement-test-key }
  review: { app_id: "2", private_key: review-test-key }
state_dir: $dir/state
$2projects:
  DEMO:
    experiments:
$3
EOF
}

# kubernetes_yaml <case> <experiments> <pod>: the case's legion.yaml under runtime: kubernetes,
# experiments the lines of projects.DEMO.experiments and pod those of runtime.kubernetes.pod.
kubernetes_yaml() {
  local dir="$root/$1"
  cat >"$dir/legion.yaml" <<EOF
project: DEMO
postgres_dsn: postgres://127.0.0.1:1/legion
bind: 10.0.0.5
daemon_url: http://10.0.0.5:13370
envoy_url: http://envoy-listener.internal.example:9020
envoy_token_file: $dir/envoy-token
operator_token_file: $dir/operator-token
dispatch_url: https://dispatch.internal.example
dispatch_token_file: $dir/dispatch-token
nats_urls: [nats://nats.internal.example:4222]
github_apps:
  implement: { app_id: "1", private_key_command: "exit 1" }
  review: { app_id: "2", private_key_command: "exit 1" }
state_dir: $dir/state
projects:
  DEMO:
    experiments:
$2
runtime:
  kubernetes:
    namespace: legion
    image: ghcr.io/cybermonklabs/legion-worker@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    storage_class: gp2
    session_store: postgres
    session_dsn_secret: SESSION_DSN
    kubeconfig: $dir/kubeconfig
    context: legion-daemon
    pod:
$3
EOF
}

# experiment <name> <roles> <folder>: the lines of one experiment reaching roles, drawing between
# an arm that names folder and a control.
experiment() {
  cat <<EOF
      $1:
        mode: random
        roles: $2
        variants:
          - {name: folder, texts: $3}
          - {name: control}
EOF
}

# items <path>...: the items of a ConfigMap volume that holds a folder with those files beside its
# manifest, the kubelet writing each at its path under the mount: a ConfigMap's key holds no `/`,
# so a folder's tree of files mounts only so, each key its path with `/` as `-`.
items() {
  local path list='{key: manifest.json, path: manifest.json}'
  for path in "$@"; do
    list+=", {key: ${path//\//-}, path: $path}"
  done
  printf '%s' "$list"
}

# The good folder: the reviewer's two role parts.
good=(roles/reviewer.md go/reviewer.md)

start gone
tmux_yaml gone '' "$(experiment shorter '[reviewer]' "$root/gone/texts")"

start no-manifest
folder "$root/no-manifest/texts" "${good[@]}"
rm -- "$root/no-manifest/texts/manifest.json"
tmux_yaml no-manifest '' "$(experiment shorter '[reviewer]' "$root/no-manifest/texts")"

start outside-mounts
folder "$root/outside-mounts/texts" "${good[@]}"
kubernetes_yaml outside-mounts "$(experiment shorter '[reviewer]' "$root/outside-mounts/texts")" "      volumes:
        - name: operator-texts
          config_map: {name: legion-operator-texts, items: [$(items "${good[@]}")]}
      volume_mounts:
        - {volume: operator-texts, mount_path: /etc/legion-texts}"

start unreached-part
folder "$root/unreached-part/texts" "${good[@]}" roles/planner.md
tmux_yaml unreached-part '' "$(experiment shorter '[reviewer]' "$root/unreached-part/texts")"

start claude-role
folder "$root/claude-role/texts" "${good[@]}"
tmux_yaml claude-role $'worker_backends: {reviewer: claude}\n' "$(experiment shorter '[reviewer]' "$root/claude-role/texts")"

start two-folders-one-role
folder "$root/two-folders-one-role/first" "${good[@]}"
folder "$root/two-folders-one-role/second" "${good[@]}"
tmux_yaml two-folders-one-role '' "$(experiment first-texts '[reviewer]' "$root/two-folders-one-role/first")
$(experiment second-texts '[implementer, reviewer]' "$root/two-folders-one-role/second")"

start inside-mounts
folder "$root/inside-mounts/texts" "${good[@]}"
kubernetes_yaml inside-mounts "$(experiment shorter '[reviewer]' "$root/inside-mounts/texts")" "      volumes:
        - name: operator-texts
          config_map: {name: legion-operator-texts, items: [$(items "${good[@]}")]}
      volume_mounts:
        - {volume: operator-texts, mount_path: $root/inside-mounts/texts}"

start two-folders-different-roles
architect=(roles/architect.md go/architect-common.md)
folder "$root/two-folders-different-roles/reviewer" "${good[@]}"
folder "$root/two-folders-different-roles/architect" "${architect[@]}"
kubernetes_yaml two-folders-different-roles "$(experiment reviewer-texts '[reviewer]' "$root/two-folders-different-roles/reviewer")
$(experiment architect-texts '[architect]' "$root/two-folders-different-roles/architect")" "      volumes:
        - name: reviewer-texts
          config_map: {name: legion-reviewer-texts, items: [$(items "${good[@]}")]}
        - name: architect-texts
          config_map: {name: legion-architect-texts, items: [$(items "${architect[@]}")]}
      volume_mounts:
        - {volume: reviewer-texts, mount_path: $root/two-folders-different-roles/reviewer}
        - {volume: architect-texts, mount_path: $root/two-folders-different-roles/architect}"
