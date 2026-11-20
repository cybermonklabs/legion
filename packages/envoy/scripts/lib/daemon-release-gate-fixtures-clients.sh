# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-fixtures-clients.sh
#
# The --clients fixtures the scenario files share: the fleet whose images carry the two clients,
# the sessions/ stack's image, each running project's rendered legion.yaml, the surfaces at or
# after the commit, and a developer-box case built on them. It defines and runs nothing;
# daemon-release-gate.test.sh sources it after the harness and before every scenario file, so no
# scenario group depends on another having run.
# shellcheck disable=SC2154 # every variable read here is set by the harness or the test that sources this file
# shellcheck disable=SC2034 # each constant here is read by the scenario files that source it after this

readonly lean_image="us-west1-docker.pkg.dev/example-project/legion/worker-lean@sha256:2222"
readonly session_image="us-west1-docker.pkg.dev/example-project/legion/agent-session@sha256:3333"
readonly sessions_project="deployment-sessions"
# sessions_stack <ref> [<project>]: the deployment's sessions/ stack, its Pulumi.yaml declaring
# sessions_project and its Pulumi.prod.yaml pinning <ref> as the agent-session image under
# <project>, by default that name.
sessions_stack() {
  mkdir -p "${case_root}/files/sessions"
  project sessions "$sessions_project"
  printf 'config:\n  %s:image: %s\n  %s:defaultSize: default\n' "${2:-$sessions_project}" "$1" "${2:-$sessions_project}" \
    >"${case_root}/files/sessions/Pulumi.prod.yaml"
}
# golden <KEY> [<pi_envoy>]: legion/testdata/<key>-legion.yaml, the project's rendered legion.yaml,
# whose runtime.kubernetes sets plugins with <pi_envoy> when one is given and no plugins otherwise.
golden() {
  local file
  file="${case_root}/files/legion/testdata/$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')-legion.yaml"
  mkdir -p "${case_root}/files/legion/testdata"
  printf 'project: %s\nruntime:\n  kubernetes:\n    namespace: legion\n    image: %s\n' "$1" "$lean_image" >"$file"
  [ -z "${2:-}" ] || printf '    plugins:\n      pi_legion: 19.28.1\n      pi_envoy: %s\n' "$2" >>"$file"
}
readonly client_fleet="    - key: SMOKE
      replicas: 0
      image: ${worker_smoke}
    - key: LINUX
      replicas: 1
      image: ${lean_image}
      daemonImage: ${worker_a}
    - key: INFRA
      replicas: 1
      image: ${infra_image}
      daemonImage: ${worker_a}"
# clients_open: every surface --clients reads at or after the commit: each image of client_fleet by
# its build, each daemonImage by the pi-envoy its legion links (linked_envoy, which the projects'
# rendered legion.yaml does not override), the sessions/ stack's image by its build, and the
# developer box (dotfiles) by its release tags; the manifests' own versions name release tags
# behind the commit.
clients_open() {
  legion_stack "$client_fleet"
  golden LINUX
  golden INFRA
  worker_image "$worker_a" "$ahead_rev"
  worker_image "$lean_image" "$identical_rev"
  worker_image "$infra_image" "$ahead_rev"
  sessions_stack "$session_image"
  worker_image "$session_image" "$ahead_rev"
  dotfiles 17.40.0
  tag pi-envoy-v17.40.0 "$ahead_rev"
  tag "pi-envoy-v${linked_envoy}" "$ahead_rev"
  tag claude-envoy-v4.0.0 "$identical_rev"
  tag pi-envoy-v16.12.1 "$behind_rev"
  tag claude-envoy-v1.0.0 "$behind_rev"
  compare "$ahead_rev" ahead 0
  compare "$identical_rev" identical 0
  compare "$behind_rev" behind 12
}
# clients_background: clients_open for the scenarios of another surface (the developer box, its
# versions, a project's fields), with the release tags the images' manifests name ahead of the
# commit too, so the images pass whatever they are judged by and those scenarios' rows answer for
# their own surface alone.
clients_background() {
  clients_open
  tag pi-envoy-v16.12.1 "$ahead_rev"
  tag claude-envoy-v1.0.0 "$ahead_rev"
}

readonly envoy_behind_rev="ffffffffffffffffffffffffffffffffffffffff"
readonly pi_envoy_ahead="pi-envoy 17.40.0 from pi-envoy-v17.40.0 at ${ahead_rev} (ahead of ${commit})"
readonly claude_note="; the version legion-plugins serves for >=1.0.0"
readonly claude_served="claude-envoy 4.0.0 from claude-envoy-v4.0.0 at ${identical_rev} (identical to ${commit})${claude_note}"
# box_case <name>: a case whose fleet and developer box carry the commit (clients_background), with
# a release tag behind it for pi-envoy 17.39.0 and claude-envoy 3.3.0.
box_case() {
  new_case "$1"
  clients_background
  tag pi-envoy-v17.39.0 "$envoy_behind_rev"
  tag claude-envoy-v3.3.0 "$behind_rev"
  compare "$envoy_behind_rev" behind 300
}
# The words a daemon's pi-envoy line carries: daemon_label after the image's surface, then the note
# of where the version came from, linked_note <KEY> for the pair its legion links and golden_note
# <KEY> for the one its rendered legion.yaml sets.
readonly daemon_label="(the pi-envoy its daemon installs)"
linked_note() {
  printf '; the pair its legion links, as legion/testdata/%s-legion.yaml sets no runtime.kubernetes.plugins' \
    "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
}
golden_note() {
  printf '; legion/testdata/%s-legion.yaml sets it as runtime.kubernetes.plugins.pi_envoy' \
    "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
}
# claude_install <version in the record> <version of the manifest at its path> [<installPath>]: one
# claude-envoy@legion-plugins install in the dotfiles' Claude Code plugin record, its copy under
# the dotfiles' plugins/cache unless an installPath is given.
claude_install() {
  local config="${case_root}/home/.dotfiles/.claude" path="${3:-}"
  if [ -z "$path" ]; then
    path="${config}/plugins/cache/legion-plugins/claude-envoy/$2"
    mkdir -p "$path"
    printf '{"name": "@sjawhar/claude-envoy", "version": "%s"}\n' "$2" >"$path/package.json"
  fi
  mkdir -p "${config}/plugins"
  printf '{"version": 2, "plugins": {"claude-envoy@legion-plugins": [{"scope": "user", "installPath": "%s", "version": "%s"}]}}\n' \
    "$path" "$1" >"${config}/plugins/installed_plugins.json"
}
