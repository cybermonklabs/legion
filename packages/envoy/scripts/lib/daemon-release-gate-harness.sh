# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-harness.sh
#
# The harness daemon-release-gate.test.sh sources before its scenarios: the temporary directory and
# its fake `gh` and `docker`, the constants every case shares, the fixture builders that write a
# case's deployment files and images, the runners that call the gate, and the assertion helpers.
# It needs `script` (the gate under test) set before it is sourced, and is not run on its own.
# shellcheck disable=SC2034 # each constant and builder here is read by the scenarios that source it
: "${script:?set script, the gate under test, before sourcing this harness}"
readonly worker_a="ghcr.io/cybermonklabs/legion-worker@sha256:aaaa"
readonly worker_b="ghcr.io/cybermonklabs/legion-worker@sha256:bbbb"
readonly worker_smoke="ghcr.io/cybermonklabs/legion-worker@sha256:5555"
readonly infra_image="us-west1-docker.pkg.dev/example-project/legion/worker-infra@sha256:1111"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
trap 'rm -rf "$temporary_dir"' EXIT
mkdir -p "${temporary_dir}/bin"

readonly commit="52b3cf5c1b6c7f42f6b8d2a1e3c4d5e6f7a8b9c0"
readonly deployment="example-org/deployment"
readonly legion="https://github.com/cybermonklabs/legion"
readonly other_source="https://github.com/example-org/deployment"
readonly old_name="https://github.com/sjawhar/legion"
readonly ahead_rev="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
readonly identical_rev="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
readonly behind_rev="cccccccccccccccccccccccccccccccccccccccc"
readonly other_rev="dddddddddddddddddddddddddddddddddddddddd"
# The project name each stack's Pulumi.yaml declares by default, which prefixes its config keys.
readonly legion_project="deployment-legion"
readonly envoy_project="deployment-envoy"
readonly cluster_project="deployment-envoy-cluster"
readonly dispatch_project="deployment-dispatch"

# The fake gh. `api repos/<FAKE_DEPLOYMENT>/contents/<path> --jq .content` answers the base64 of
# FAKE_ROOT/files/<path>; for a directory, `--jq '.[] | select(.type == "file") | .name'` answers
# its file names, one a line, as GitHub's listing filtered so would, and FAKE_LIST_DOWN=1 makes a
# listing fail as a server error. Any other call is refused as unexpected;
# `api repos/cybermonklabs/legion/compare/<base>...<head> --jq ...` answers
# FAKE_ROOT/compare/<head>, and fails as an unreachable API when FAKE_API_DOWN=1.
cat >"${temporary_dir}/bin/gh" <<'EOF'
#!/usr/bin/env bash
printf 'gh %s\n' "$*" >>"${FAKE_CALLS:?}"
[ "$1" = api ] || { echo "fake gh: unexpected command: $*" >&2; exit 2; }
case "$2" in
  repos/"${FAKE_DEPLOYMENT:?}"/contents/*)
    file="${FAKE_ROOT:?}/files/${2#repos/"${FAKE_DEPLOYMENT}"/contents/}"
    if [ -d "$file" ]; then
      [ "${FAKE_LIST_DOWN:-0}" != 1 ] || { echo "gh: Server Error (HTTP 500)" >&2; exit 1; }
      [ "$4" = '.[] | select(.type == "file") | .name' ] || { echo "fake gh: unexpected listing query: $*" >&2; exit 2; }
      find "$file" -mindepth 1 -maxdepth 1 -type f -printf '%f\n'
      exit 0
    fi
    [ -f "$file" ] || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
    base64 -w 76 <"$file"
    ;;
  repos/cybermonklabs/legion/compare/*)
    [ "${FAKE_API_DOWN:-0}" != 1 ] || { echo "error connecting to api.github.com" >&2; exit 1; }
    head="${2##*...}"
    [ -f "${FAKE_ROOT}/compare/${head}" ] || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
    cat "${FAKE_ROOT}/compare/${head}"
    ;;
  *) echo "fake gh: unexpected api call: $*" >&2; exit 2 ;;
esac
EOF
# The fake docker. A ref's answers are files under FAKE_ROOT/images/<ref with / : @ as _>:
# mediatype, labels (a single image's) and platform-<os>_<arch> (an index's, one per platform),
# and platforms, the index's platform list as the range format prints it.
cat >"${temporary_dir}/bin/docker" <<'EOF'
#!/usr/bin/env bash
printf 'docker %s\n' "$*" >>"${FAKE_CALLS:?}"
[ "$1 $2 $3" = "buildx imagetools inspect" ] && [ "$5" = --format ] || { echo "fake docker: unexpected command: $*" >&2; exit 2; }
dir="${FAKE_ROOT:?}/images/$(printf '%s' "$4" | tr '/:@' '___')"
[ -d "$dir" ] || { echo "ERROR: $4: not found" >&2; exit 1; }
mediatype=$(cat "$dir/mediatype")
case "$6" in
  '{{json .Manifest.MediaType}}') printf '%s' "$mediatype" ;;
  '{{json .Image.Config.Labels}}')
    case "$mediatype" in
      *index*) echo null ;;
      *) cat "$dir/labels" ;;
    esac
    ;;
  '{{range .Manifest.Manifests}}'*) printf '%s' "$(cat "$dir/platforms")" ;;
  '{{json (index .Image "'*)
    platform=${6#'{{json (index .Image "'}
    platform=${platform%%'"'*}
    file="$dir/platform-$(printf '%s' "$platform" | tr '/' '_')"
    [ -f "$file" ] || { echo "ERROR: template: :1:17: executing \"\" at <index .Image \"$platform\">: nil pointer evaluating *v1.Image.Config" >&2; exit 1; }
    cat "$file"
    ;;
  *) echo "fake docker: unexpected format: $6" >&2; exit 2 ;;
esac
EOF
chmod +x "${temporary_dir}/bin/gh" "${temporary_dir}/bin/docker"

failures=0
case_root=""
out="${temporary_dir}/out"
err="${temporary_dir}/err"
calls="${temporary_dir}/calls"
status=0

# new_case starts a scenario of its own: no stack file, no image and no comparison yet, and each
# stack's Pulumi.yaml declaring its default project name.
new_case() {
  case_root="${temporary_dir}/case-$1"
  mkdir -p "${case_root}/files/legion" "${case_root}/files/envoy" "${case_root}/files/envoy-cluster" \
    "${case_root}/files/dispatch" "${case_root}/images" "${case_root}/compare"
  project legion "$legion_project"
  project envoy "$envoy_project"
  project envoy-cluster "$cluster_project"
  project dispatch "$dispatch_project"
}
# project <stack> <name>: <stack>/Pulumi.yaml, declaring <name> as its project's name.
project() {
  printf 'name: %s\nruntime: go\n' "$2" >"${case_root}/files/$1/Pulumi.yaml"
}
# compare <revision> <status> <behind_by>: what GitHub answers for <commit>...<revision>.
compare() {
  printf '{"status":"%s","behind_by":%s}\n' "$2" "$3" >"${case_root}/compare/$1"
}
# single <ref> <source> <revision>: a single image labelled with source and revision; an empty
# source or revision leaves that label out.
single() {
  local dir
  dir="${case_root}/images/$(printf '%s' "$1" | tr '/:@' '___')"
  mkdir -p "$dir"
  printf '"application/vnd.oci.image.manifest.v1+json"' >"$dir/mediatype"
  labels "$2" "$3" >"$dir/labels"
}
# index <ref> <source> <revision> [<arm64 revision>]: an OCI image index of linux/amd64 and
# linux/arm64 with an unknown/unknown attestation entry after each, as the release builds push; the
# arm64 image names <arm64 revision> when one is given. With <source> "attestations-only", the index
# holds the two attestation entries alone.
index() {
  local dir
  dir="${case_root}/images/$(printf '%s' "$1" | tr '/:@' '___')"
  mkdir -p "$dir"
  printf '"application/vnd.oci.image.index.v1+json"' >"$dir/mediatype"
  if [ "$2" = attestations-only ]; then
    printf 'unknown/unknown unknown/unknown ' >"$dir/platforms"
    return
  fi
  printf 'linux/amd64 unknown/unknown linux/arm64 unknown/unknown ' >"$dir/platforms"
  labels "$2" "$3" >"$dir/platform-linux_amd64"
  labels "$2" "${4:-$3}" >"$dir/platform-linux_arm64"
}
labels() {
  local fields=()
  [ -z "$1" ] || fields+=("\"org.opencontainers.image.source\": \"$1\"")
  [ -z "$2" ] || fields+=("\"org.opencontainers.image.revision\": \"$2\"")
  fields+=('"org.opencontainers.image.title": "legion"')
  local IFS=,
  printf '{\n  %s\n}\n' "${fields[*]}"
}
# legion_stack <yaml body> [<project>]: legion/Pulumi.prod.yaml, its projects list being the body,
# its keys under <project>, by default the name legion/Pulumi.yaml declares.
legion_stack() {
  local name=${2:-$legion_project}
  printf 'config:\n  %s:roleModels: {}\n  %s:projects:\n%s\n' "$name" "$name" "$1" >"${case_root}/files/legion/Pulumi.prod.yaml"
}
# legion_settings: legion/Pulumi.prod.yaml with no projects list, the shape a deployment that keeps
# one file per project in legion/projects/ has.
legion_settings() {
  printf 'config:\n  %s:roleModels: {}\n' "$legion_project" >"${case_root}/files/legion/Pulumi.prod.yaml"
}
# project_file <nn-KEY> <yaml body>: legion/projects/<nn-KEY>.yaml, one project's settings.
project_file() {
  mkdir -p "${case_root}/files/legion/projects"
  printf '%s\n' "$2" >"${case_root}/files/legion/projects/$1.yaml"
}
# fleet_files: the fleet above as legion/projects/ holds it in production, one file a project.
fleet_files() {
  project_file 10-SMOKE "key: SMOKE
replicas: 0
image: ${worker_smoke}"
  project_file 20-LINUX "key: LINUX
replicas: 1
image: us-west1-docker.pkg.dev/example-project/legion/worker-lean@sha256:2222
daemonImage: ${worker_a}"
  project_file 40-INFRA "key: INFRA
replicas: 1
image: ${infra_image}
daemonImage: ${worker_b}"
  project_file 50-QUIET "key: QUIET
daemon: false
image: ${worker_smoke}"
}
# listener_stacks <envoy> <envoy-cluster> <dispatch envoyImage> [<dispatch listenerImage>]
listener_stacks() {
  printf 'config:\n  %s:listenerImage: %s\n' "$envoy_project" "$1" >"${case_root}/files/envoy/Pulumi.prod.yaml"
  printf 'config:\n  %s:envoyImage: %s\n' "$cluster_project" "$2" >"${case_root}/files/envoy-cluster/Pulumi.prod.yaml"
  {
    printf 'config:\n  %s:envoyImage: %s\n' "$dispatch_project" "$3"
    [ -z "${4:-}" ] || printf '  %s:listenerImage: %s\n' "$dispatch_project" "$4"
  } >"${case_root}/files/dispatch/Pulumi.prod.yaml"
}

# run_bare [env assignments...] -- [gate arguments...]: runs the gate in the current case with
# exactly the arguments given; sets $status.
run_bare() {
  local assignments=()
  while [ "$#" -gt 0 ] && [ "$1" != -- ]; do assignments+=("$1"); shift; done
  [ "$#" -eq 0 ] || shift
  : >"$out"
  : >"$err"
  : >"$calls"
  status=0
  env PATH="${temporary_dir}/bin:${PATH}" FAKE_CALLS="$calls" FAKE_ROOT="$case_root" \
    FAKE_DEPLOYMENT="$deployment" "${assignments[@]}" bash "$script" "$@" >"$out" 2>"$err" || status=$?
}
# run_gate [env assignments...] -- [gate arguments...]: run_bare with `--deployment <deployment>`
# before the gate arguments, as an operator runs it.
run_gate() {
  local assignments=()
  while [ "$#" -gt 0 ] && [ "$1" != -- ]; do assignments+=("$1"); shift; done
  [ "$#" -eq 0 ] || shift
  run_bare "${assignments[@]}" -- --deployment "$deployment" "$@"
}

check() {
  if [ "$2" = true ]; then
    printf 'PASS: %s\n' "$1"
  else
    printf 'FAIL: %s\n' "$1"
    printf '  exit: %s\n' "$status"
    sed 's/^/  stdout: /' "$out"
    sed 's/^/  stderr: /' "$err"
    sed 's/^/  call: /' "$calls"
    failures=$((failures + 1))
  fi
}
has_line() { grep -qxF -- "$1" "$out" && echo true || echo false; }
lacks() { grep -qF -- "$1" "$2" && echo false || echo true; }
last_line_is() { [ "$(tail -n 1 "$out")" = "$1" ] && echo true || echo false; }
is() { [ "$1" = "$2" ] && echo true || echo false; }

