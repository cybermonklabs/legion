#!/usr/bin/env bash
# packages/envoy/scripts/daemon-release-gate.test.sh
#
# Proves daemon-release-gate.sh's verdicts with no network: a fake `gh` answers the stack files and
# the compare calls from files this test writes, and a fake `docker` answers `buildx imagetools
# inspect` the way the real one answers a single image and an OCI image index (an index's
# `.Image.Config.Labels` is `null`, and its platforms are read one at a time). Both record every
# call, so a case asserts what the gate printed and what it asked. The deployment repository is
# the one `--deployment` names, and each stack's config keys carry the project name its own
# Pulumi.yaml declares; the fake gh answers that repository alone, so a gate that read any other
# would fail every row.
#
# Negative control, run when this was written: make the gate read an index with
# `{{json .Image.Config.Labels}}`, as a single image is read; every --listeners row below that
# expects a comparison prints `UNKNOWN` instead, which is the blindness an index-shaped pin causes.
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly script="${project_root}/packages/envoy/scripts/daemon-release-gate.sh"
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

readonly worker_a="ghcr.io/cybermonklabs/legion-worker@sha256:aaaa"
readonly worker_b="ghcr.io/cybermonklabs/legion-worker@sha256:bbbb"
readonly worker_smoke="ghcr.io/cybermonklabs/legion-worker@sha256:5555"
readonly infra_image="us-west1-docker.pkg.dev/example-project/legion/worker-infra@sha256:1111"
readonly fleet="    - key: SMOKE
      replicas: 0
      image: ${worker_smoke}
    - key: LINUX
      replicas: 1
      image: us-west1-docker.pkg.dev/example-project/legion/worker-lean@sha256:2222
      daemonImage: ${worker_a}
    - key: INFRA
      replicas: 1
      image: ${infra_image}
      daemonImage: ${worker_b}
    - key: QUIET
      daemon: false
      image: ${worker_smoke}"

echo "case: every running daemon is at or after the commit"
new_case open
legion_stack "$fleet"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- "$commit"
check "exits 0" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"
check "LINUX passes on its daemonImage" "$(has_line "PASS: LINUX runs ${ahead_rev} (ahead of ${commit})")"
check "INFRA passes on its daemonImage" "$(has_line "PASS: INFRA runs ${identical_rev} (identical to ${commit})")"
check "SMOKE at replicas 0 is skipped by name" "$(has_line "SKIPPED: SMOKE runs no daemon (replicas: 0)")"
check "an entry with daemon: false is skipped by name" "$(has_line "SKIPPED: QUIET runs no daemon (daemon: false)")"
check "the image of an entry that sets daemonImage is never read" "$(lacks "$infra_image" "$calls")"
check "the image of an entry that runs no daemon is never read" "$(lacks "$worker_smoke" "$calls")"

echo "case: a running daemon behind the commit"
new_case behind
legion_stack "$fleet"
single "$worker_a" "$legion" "$behind_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$behind_rev" behind 89
compare "$identical_rev" identical 0
run_gate -- "$commit"
check "exits 1" "$(is "$status" 1)"
check "refuses naming the pin and its revision" "$(has_line "GATE SHUT: LINUX runs ${behind_rev} (behind ${commit} by 89)")"
check "the verdict is GATE SHUT" "$(last_line_is "GATE SHUT")"

echo "case: a daemon image that names no revision"
new_case unlabelled
legion_stack "$fleet"
single "$worker_a" "$legion" ""
single "$worker_b" "$legion" "$identical_rev"
compare "$identical_rev" identical 0
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the pin and the missing label" "$(has_line "UNKNOWN: LINUX: ${worker_a} names no org.opencontainers.image.revision label")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

# The compare call takes any ref GitHub resolves, so a revision label that names a branch or a tag
# (`main`) would compare as wherever that ref points today, not as the commit the image was built
# from. The gate compares a revision only when it is 7 to 40 lowercase hex digits.
#
# Negative control, run when this was written: drop the commit-id check from judge; the rows below
# that expect `which is no commit id` print `PASS` instead, with a compare call for the label.
echo "case: a daemon image whose revision label is no commit id"
new_case revision-not-commit
legion_stack "$fleet"
single "$worker_a" "$legion" "main"
single "$worker_b" "$legion" "abcdef1"
compare "main" ahead 0
compare "abcdef1" identical 0
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the pin and its revision" "$(has_line "UNKNOWN: LINUX: ${worker_a} names revision main, which is no commit id")"
check "makes no compare call for that revision" "$(lacks "...main" "$calls")"
check "a revision of 7 hex digits is a commit id, and is compared" "$(has_line "PASS: INFRA runs abcdef1 (identical to ${commit})")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: the compare API cannot be reached"
new_case api-down
legion_stack "$fleet"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
run_gate FAKE_API_DOWN=1 -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the pin and GitHub's answer" "$(has_line "UNKNOWN: LINUX: cannot compare ${commit}...${ahead_rev} in cybermonklabs/legion: error connecting to api.github.com")"
check "the verdict is UNKNOWN" "$(last_line_is "UNKNOWN")"

echo "case: a daemon image built from another repository"
new_case deployment
legion_stack "$fleet"
single "$worker_a" "$other_source" "$other_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$identical_rev" identical 0
compare "$other_rev" ahead 0
run_gate -- "$commit"
check "exits 1" "$(is "$status" 1)"
check "refuses naming the source" "$(has_line "GATE SHUT: LINUX runs an image built from ${other_source}")"
check "makes no compare call for that revision" "$(lacks "...${other_rev}" "$calls")"

echo "case: an image the registry does not answer for"
new_case unreadable
legion_stack "$fleet"
single "$worker_b" "$legion" "$identical_rev"
compare "$identical_rev" identical 0
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the pin and docker's answer" "$(has_line "UNKNOWN: LINUX: cannot read ${worker_a}: ERROR: ${worker_a}: not found")"

echo "case: a running entry that names no image"
new_case no-image
legion_stack "    - key: LINUX
      replicas: 1"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the entry" "$(has_line "UNKNOWN: LINUX runs a daemon and names neither daemonImage nor image")"

# The deployment's legion/host.go: an entry at `runtime: tmux` runs its daemon on the project's host VM
# from host.legionWorker's legion (checkHost refuses daemonImage, replicas and daemon on it), so its
# `image` says nothing about the daemon. The gate reads no image for it: it is UNKNOWN by name. So is
# a runtime the gate does not know; an entry that names `runtime: kubernetes` is read as before.
#
# Negative control, run when this was written: drop the runtime branch from daemon_pins; the rows
# below that expect the runtime's UNKNOWN line print `PASS` for the entry's image instead.
readonly worker_tmux="ghcr.io/cybermonklabs/legion-worker@sha256:6666"
readonly worker_host="ghcr.io/cybermonklabs/legion-worker@sha256:7777"
echo "case: an entry on runtime tmux runs its daemon on a host, not from its image"
new_case tmux
legion_stack "    - key: LINUX
      replicas: 1
      runtime: kubernetes
      daemonImage: ${worker_a}
    - key: HOSTED
      runtime: tmux
      image: ${worker_tmux}
      host:
        legionWorker: ${worker_host}"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_tmux" "$legion" "$ahead_rev"
single "$worker_host" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the entry and its tmux host" \
  "$(has_line "UNKNOWN: HOSTED runs its daemon on a tmux host (runtime: tmux), from host.legionWorker, an image this gate does not read")"
check "never reads the entry's image" "$(lacks "$worker_tmux" "$calls")"
check "never reads the host's image either" "$(lacks "$worker_host" "$calls")"
check "an entry that names runtime kubernetes is still read" "$(has_line "PASS: LINUX runs ${ahead_rev} (ahead of ${commit})")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: an entry on a runtime the gate does not know"
new_case runtime-unknown
legion_stack "    - key: ODD
      replicas: 1
      runtime: nomad
      image: ${worker_tmux}"
single "$worker_tmux" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the entry and its runtime" \
  "$(has_line "UNKNOWN: ODD names runtime nomad, which this gate does not know, so it reads no image for its daemon")"
check "never reads the entry's image" "$(lacks "$worker_tmux" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: a stack that lists no running daemon"
new_case empty
legion_stack "    - key: SMOKE
      replicas: 0
      image: ${worker_smoke}"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "says it read no pin, rather than opening" "$(has_line "UNKNOWN: the deployment's Legion projects run no daemon, so no pin was read")"
check "the verdict is never GATE OPEN" "$(lacks "GATE OPEN" "$out")"

echo "case: the stack file cannot be read"
new_case no-stack
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file" "$(has_line "UNKNOWN: cannot read ${deployment} legion/Pulumi.prod.yaml: gh: Not Found (HTTP 404)")"

echo "case: a commit that is no commit id"
new_case usage
run_gate -- "main"
check "exits 2" "$(is "$status" 2)"
check "says what it takes" "$(grep -qF "usage: daemon-release-gate.sh --deployment <owner/repo> [--listeners] <commit>" "$err" && echo true || echo false)"

# The deployment repository is the operator's to name, never a name the gate compiles in.
echo "case: no --deployment"
new_case no-deployment
legion_stack "$fleet"
run_bare -- "$commit"
check "exits 2" "$(is "$status" 2)"
check "names the input it needs" "$(grep -qF -- "--deployment <owner/repo> is required" "$err" && echo true || echo false)"
check "reads nothing" "$(is "$(wc -l <"$calls")" 0)"
run_bare -- --listeners "$commit"
check "with --listeners too: exits 2" "$(is "$status" 2)"
check "with --listeners too: reads nothing" "$(is "$(wc -l <"$calls")" 0)"

echo "case: a --deployment that is not owner/repo"
new_case bad-deployment
run_bare -- --deployment example-org "$commit"
check "exits 2" "$(is "$status" 2)"
check "names what it got" "$(grep -qF -- "--deployment example-org is not <owner>/<repo>" "$err" && echo true || echo false)"
check "reads nothing" "$(is "$(wc -l <"$calls")" 0)"

# Each stack's config keys are under the name its own Pulumi.yaml declares: the gate reads that name
# and never guesses one. A stack whose Pulumi.yaml is missing or declares no name is UNKNOWN, by name.
echo "case: the projects list is read under the name legion/Pulumi.yaml declares"
new_case project-name
project legion example-other-legion
legion_stack "$fleet" example-other-legion
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- "$commit"
check "exits 0" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"
check "reads its stack files from the deployment, and from nothing else" \
  "$(grep -qF "gh api repos/${deployment}/contents/legion/Pulumi.yaml" "$calls" && grep -qF "gh api repos/${deployment}/contents/legion/Pulumi.prod.yaml" "$calls" && ! grep -F 'gh api repos/' "$calls" | grep -vF '/compare/' | grep -qvF "gh api repos/${deployment}/contents/" && echo true || echo false)"

echo "case: the deployment read is the one --deployment names, not a fixed one"
new_case another-deployment
legion_stack "$fleet"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_bare FAKE_DEPLOYMENT=example-other-org/example-other-repo -- --deployment example-other-org/example-other-repo "$commit"
check "exits 0" "$(is "$status" 0)"
check "reads its stack files from that repository" \
  "$(grep -qF "gh api repos/example-other-org/example-other-repo/contents/legion/Pulumi.prod.yaml" "$calls" && echo true || echo false)"
check "never reads the default test repository" "$(lacks "repos/${deployment}/" "$calls")"

echo "case: keys under another project's name are not the stack's"
new_case project-mismatch
legion_stack "$fleet" example-other-legion
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming both places it looked" \
  "$(has_line "UNKNOWN: ${deployment} declares no project: legion/projects/ does not exist, and legion/Pulumi.prod.yaml holds no ${legion_project}:projects list")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

# Production keeps one file per project in legion/projects/ (<nn>-<KEY>.yaml, one mapping each,
# read in file-name order) and no projects list in legion/Pulumi.prod.yaml. A gate that read only
# the list would say UNKNOWN about every deployment of that shape.
echo "case: legion/projects/ holds the projects and legion/Pulumi.prod.yaml no list (production)"
new_case production-shape
legion_settings
fleet_files
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- "$commit"
check "exits 0" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"
check "reads LINUX's file" "$(has_line "PASS: LINUX runs ${ahead_rev} (ahead of ${commit})")"
check "reads INFRA's file" "$(has_line "PASS: INFRA runs ${identical_rev} (identical to ${commit})")"
check "skips SMOKE by name" "$(has_line "SKIPPED: SMOKE runs no daemon (replicas: 0)")"
check "skips QUIET by name" "$(has_line "SKIPPED: QUIET runs no daemon (daemon: false)")"
check "lists the directory in the named deployment" \
  "$(grep -qF "gh api repos/${deployment}/contents/legion/projects --jq" "$calls" && echo true || echo false)"

# The deployment's legion program reads legion/projects/ alone once the directory exists: a list
# left in legion/Pulumi.prod.yaml then names pins no daemon runs, so the gate reads it no more.
echo "case: a deployment with legion/projects/ is read from its files alone, a leftover list unread"
new_case both-shapes
legion_stack "    - key: LINUX
      replicas: 1
      daemonImage: ${worker_b}
    - key: OLDONLY
      replicas: 1
      daemonImage: ${worker_b}"
project_file 20-LINUX "key: LINUX
replicas: 1
daemonImage: ${worker_a}"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- "$commit"
check "exits 0" "$(is "$status" 0)"
check "LINUX is judged by its file's image" "$(has_line "PASS: LINUX runs ${ahead_rev} (ahead of ${commit})")"
check "LINUX is judged once" "$(is "$(grep -c '^PASS: LINUX ' "$out")" 1)"
check "the leftover list's project is not read" "$(lacks "OLDONLY" "$out")"
check "the leftover list's image is never read" "$(lacks "$worker_b" "$calls")"

echo "case: legion/projects/ exists and holds no file"
new_case project-directory-empty
legion_stack "$fleet"
mkdir -p "${case_root}/files/legion/projects"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the empty directory" \
  "$(has_line "UNKNOWN: ${deployment} declares no project: legion/projects/ holds no file")"
check "never falls back to the list" "$(lacks "docker" "$calls")"

echo "case: a project file holding a date, which YAML reads and JSON has no type for"
new_case project-file-date
legion_settings
project_file 20-LINUX "key: LINUX
replicas: 1
since: 2026-11-20
daemonImage: ${worker_a}"
single "$worker_a" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- "$commit"
check "exits 0" "$(is "$status" 0)"
check "LINUX is read and compared" "$(has_line "PASS: LINUX runs ${ahead_rev} (ahead of ${commit})")"

echo "case: a file of legion/projects/ that is not one mapping"
new_case project-file-not-mapping
legion_settings
fleet_files
project_file 30-ODD "- key: ODD"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file" \
  "$(has_line "UNKNOWN: ${deployment} legion/projects/30-ODD.yaml is not one mapping of a project's settings")"
check "reads no image" "$(lacks "docker" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: legion/projects/ cannot be listed"
new_case project-listing-down
legion_settings
fleet_files
run_gate FAKE_LIST_DOWN=1 -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the listing and GitHub's answer" \
  "$(has_line "UNKNOWN: cannot list ${deployment} legion/projects: gh: Server Error (HTTP 500)")"
check "reads no image" "$(lacks "docker" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: legion/Pulumi.yaml is missing"
new_case no-project-file
legion_stack "$fleet"
rm "${case_root}/files/legion/Pulumi.yaml"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file" "$(has_line "UNKNOWN: cannot read ${deployment} legion/Pulumi.yaml: gh: Not Found (HTTP 404)")"
check "reads no image" "$(lacks "docker" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: legion/Pulumi.yaml declares no name"
new_case no-project-name
legion_stack "$fleet"
printf 'runtime: go\n' >"${case_root}/files/legion/Pulumi.yaml"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file" \
  "$(has_line "UNKNOWN: ${deployment} legion/Pulumi.yaml declares no project name, so no key of legion/Pulumi.prod.yaml is known to be its own")"
check "reads no image" "$(lacks "docker" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

readonly envoy_pin="ghcr.io/cybermonklabs/legion/envoy@sha256:e001"
readonly cluster_pin="ghcr.io/cybermonklabs/legion/envoy@sha256:e002"
readonly server_pin="ghcr.io/cybermonklabs/legion/envoy@sha256:e003"
readonly dispatch_listener_pin="ghcr.io/cybermonklabs/legion/envoy@sha256:e004"

echo "case: --listeners, every listener index at or after the commit"
new_case listeners-open
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$legion" "$ahead_rev"
index "$cluster_pin" "$legion" "$identical_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- --listeners "$commit"
check "exits 0" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"
check "envoy's two platforms agree and are compared" "$(has_line "PASS: envoy runs ${ahead_rev} (ahead of ${commit})")"
check "envoy-cluster's are" "$(has_line "PASS: envoy-cluster runs ${identical_rev} (identical to ${commit})")"
check "dispatch's listener is read from listenerImage" "$(has_line "PASS: dispatch runs ${ahead_rev} (ahead of ${commit})")"
check "dispatch's server image is never read" "$(lacks "$server_pin" "$calls")"
check "each real platform is read, and no attestation entry" \
  "$(grep -qF '(index .Image "linux/arm64")' "$calls" && grep -qF '(index .Image "linux/amd64")' "$calls" && lacks '(index .Image "unknown/unknown")' "$calls")"

# The deployment requires dispatch's listenerImage (its dispatch/main.go loadSettings; its
# pinListenerImage reads that key and no other), so a stack that sets none runs no listener this
# gate can read. Reading the server's envoyImage in its place would open the gate on an image the
# listener does not run, and for a configuration the deployment itself refuses.
echo "case: --listeners, a dispatch stack with no listenerImage is UNKNOWN, never the server's image"
new_case listeners-no-listener-pin
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin"
index "$envoy_pin" "$legion" "$ahead_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$server_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the key it needs" \
  "$(has_line "UNKNOWN: dispatch: dispatch/Pulumi.prod.yaml sets no ${dispatch_project}:listenerImage")"
check "never reads dispatch's server image" "$(lacks "$server_pin" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --listeners, a stack whose Pulumi.yaml declares no name, or is missing, is UNKNOWN by name"
new_case listeners-no-project
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
printf 'runtime: go\n' >"${case_root}/files/envoy/Pulumi.yaml"
rm "${case_root}/files/dispatch/Pulumi.yaml"
index "$envoy_pin" "$legion" "$ahead_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses envoy, naming its file" \
  "$(has_line "UNKNOWN: envoy: ${deployment} envoy/Pulumi.yaml declares no project name, so no key of envoy/Pulumi.prod.yaml is known to be its own")"
check "refuses dispatch, naming its file" \
  "$(has_line "UNKNOWN: dispatch: cannot read ${deployment} dispatch/Pulumi.yaml: gh: Not Found (HTTP 404)")"
check "never reads envoy's pin" "$(lacks "$envoy_pin" "$calls")"
check "never reads dispatch's pin" "$(lacks "$dispatch_listener_pin" "$calls")"
check "prints one line for each refused pin" "$(is "$(grep -cE '^UNKNOWN: (envoy|dispatch): ' "$out")" 2)"
check "still reads and compares envoy-cluster" "$(has_line "PASS: envoy-cluster runs ${ahead_rev} (ahead of ${commit})")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --listeners, an index whose platforms name different revisions"
new_case listeners-disagree
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$legion" "$ahead_rev" "$identical_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the disagreement" \
  "$(has_line "UNKNOWN: envoy platforms disagree: linux/amd64 names ${legion} at ${ahead_rev}, linux/arm64 names ${legion} at ${identical_rev}")"
check "the verdict is never GATE OPEN" "$(lacks "GATE OPEN" "$out")"

echo "case: --listeners, an index of attestation entries alone"
new_case listeners-attestations
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" attestations-only
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the pin" "$(has_line "UNKNOWN: envoy: ${envoy_pin} is an index with no platform but unknown/unknown")"

# sjawhar/legion is this repository under its name before the transfer, and the deployment records
# the two names as one repository in the repositoryRenames of its Legion projects, the value the
# daemon's ghrepo.Renames reads (packages/daemon/internal/ghrepo/renames.go). A build made under
# the old name is a build of this repository: it is compared like any other, and refused only for
# what the comparison says. A source no recorded pair makes one with this repository is still
# refused by name, and an unreadable record is UNKNOWN rather than a guess either way.
#
# Negative control, run when this was written: drop the alias check from judge; the rows below
# that record the pair print `GATE SHUT: envoy runs an image built from …` and make no compare.
readonly renamed_fleet="    - key: INFRA
      replicas: 0
      repositoryRenames:
        - from: sjawhar/legion
          to: cybermonklabs/legion"
readonly renamed_note="; built as ${old_name}, which the repositoryRenames of the deployment's Legion projects make one repository with ${legion}"

echo "case: --listeners, a pin built under the repository's old name, the rename recorded, ahead"
new_case listeners-renamed-ahead
legion_stack "$renamed_fleet"
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$old_name" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 0" "$(is "$status" 0)"
check "compares it and names the recorded rename" "$(has_line "PASS: envoy runs ${other_rev} (ahead of ${commit})${renamed_note}")"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

echo "case: --listeners, the measured shape: envoy's pin built under the old name, behind"
new_case listeners-measured
legion_stack "$renamed_fleet"
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$old_name" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" behind 400
run_gate -- --listeners "$commit"
check "exits 1" "$(is "$status" 1)"
check "shuts naming its revision and how far behind, not its source" \
  "$(has_line "GATE SHUT: envoy runs ${other_rev} (behind ${commit} by 400)${renamed_note}")"
check "compares its revision in cybermonklabs/legion" "$(grep -qF "compare/${commit}...${other_rev}" "$calls" && echo true || echo false)"
check "still reads and compares the other two" \
  "$(grep -qxF "PASS: envoy-cluster runs ${ahead_rev} (ahead of ${commit})" "$out" && grep -qxF "PASS: dispatch runs ${ahead_rev} (ahead of ${commit})" "$out" && echo true || echo false)"

echo "case: --listeners, a pin built under the old name with no rename recorded"
new_case listeners-no-rename
legion_stack "    - key: INFRA
      replicas: 0"
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$old_name" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 1" "$(is "$status" 1)"
check "refuses naming the source" "$(has_line "GATE SHUT: envoy runs an image built from ${old_name}")"
check "makes no compare call for its revision" "$(lacks "...${other_rev}" "$calls")"

echo "case: --listeners, a pin built under the old name when the rename record cannot be read"
new_case listeners-renames-unreadable
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$old_name" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the record it could not read" \
  "$(has_line "UNKNOWN: envoy: built from ${old_name}, and the repository renames of ${deployment}'s Legion projects could not be read: gh: Not Found (HTTP 404)")"
check "makes no compare call for its revision" "$(lacks "...${other_rev}" "$calls")"

echo "case: --listeners, a pin built under the old name when legion/Pulumi.yaml is missing"
new_case listeners-renames-no-project
legion_stack "$renamed_fleet"
rm "${case_root}/files/legion/Pulumi.yaml"
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$old_name" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file it could not read, never a guessed record" \
  "$(has_line "UNKNOWN: envoy: built from ${old_name}, and the repository renames of ${deployment}'s Legion projects could not be read: cannot read ${deployment} legion/Pulumi.yaml: gh: Not Found (HTTP 404)")"
check "makes no compare call for its revision" "$(lacks "...${other_rev}" "$calls")"
check "still reads and compares envoy-cluster" "$(has_line "PASS: envoy-cluster runs ${ahead_rev} (ahead of ${commit})")"

# daemon mode and --listeners read the same projects: where the deployment declares none, the rename
# record is no record, and a pin built under the old name is UNKNOWN, never shut as built elsewhere.
echo "case: --listeners, a pin built under the old name when the deployment declares no project"
new_case listeners-renames-no-projects
legion_stack "$renamed_fleet" example-other-legion
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$old_name" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN saying no project is declared, never GATE SHUT" \
  "$(has_line "UNKNOWN: envoy: built from ${old_name}, and the repository renames of ${deployment}'s Legion projects could not be read: ${deployment} declares no project: legion/projects/ does not exist, and legion/Pulumi.prod.yaml holds no ${legion_project}:projects list")"
check "makes no compare call for its revision" "$(lacks "...${other_rev}" "$calls")"

# An empty list is no declaration either: a deployment whose legion/Pulumi.prod.yaml lists no
# project declares none, so its rename record is no record, in both modes.
echo "case: an empty projects list declares no project"
new_case empty-list
legion_stack "    []"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the empty list" \
  "$(has_line "UNKNOWN: ${deployment} declares no project: legion/projects/ does not exist, and legion/Pulumi.prod.yaml's ${legion_project}:projects list is empty")"
check "reads no image" "$(lacks "docker" "$calls")"

echo "case: --listeners, a pin built under the old name when the projects list is empty"
new_case listeners-renames-empty-list
legion_stack "    []"
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$old_name" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN saying the list is empty, never GATE SHUT" \
  "$(has_line "UNKNOWN: envoy: built from ${old_name}, and the repository renames of ${deployment}'s Legion projects could not be read: ${deployment} declares no project: legion/projects/ does not exist, and legion/Pulumi.prod.yaml's ${legion_project}:projects list is empty")"
check "never shuts the gate on it" "$(lacks "GATE SHUT: envoy" "$out")"
check "makes no compare call for its revision" "$(lacks "...${other_rev}" "$calls")"

echo "case: --listeners, a recorded rename holds whatever the case of the source's name"
new_case listeners-renamed-case
legion_stack "$renamed_fleet"
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "https://github.com/SJawhar/Legion" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 0" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

echo "case: --listeners, a recorded rename makes no other repository one with this one"
new_case listeners-renamed-other-source
legion_stack "$renamed_fleet"
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$other_source" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 1" "$(is "$status" 1)"
check "refuses naming the source" "$(has_line "GATE SHUT: envoy runs an image built from ${other_source}")"
check "makes no compare call for its revision" "$(lacks "...${other_rev}" "$calls")"

echo "case: a daemon built under the old name, the rename recorded on another project's entry"
new_case daemons-renamed
legion_stack "$renamed_fleet
    - key: LINUX
      replicas: 1
      daemonImage: ${worker_a}"
single "$worker_a" "$old_name" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- "$commit"
check "exits 0" "$(is "$status" 0)"
check "compares it and names the recorded rename" "$(has_line "PASS: LINUX runs ${ahead_rev} (ahead of ${commit})${renamed_note}")"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

# In production the pair sits in a project's own file of legion/projects/, not in a list in
# legion/Pulumi.prod.yaml: a rename record read from the list alone reads empty there, and shuts as
# "built from" a build of this very repository.
echo "case: a daemon built under the old name, the rename recorded in a project's file (production)"
new_case daemons-renamed-files
legion_settings
project_file 40-INFRA "key: INFRA
replicas: 0
image: ${infra_image}
repositoryRenames:
  - from: sjawhar/legion
    to: cybermonklabs/legion"
project_file 20-LINUX "key: LINUX
replicas: 1
daemonImage: ${worker_a}"
single "$worker_a" "$old_name" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- "$commit"
check "exits 0" "$(is "$status" 0)"
check "compares it and names the recorded rename" "$(has_line "PASS: LINUX runs ${ahead_rev} (ahead of ${commit})${renamed_note}")"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

echo "case: --listeners, a pin built under the old name, the rename recorded in a project's file (production)"
new_case listeners-renamed-files
legion_settings
project_file 40-INFRA "key: INFRA
replicas: 0
repositoryRenames:
  - from: sjawhar/legion
    to: cybermonklabs/legion"
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$old_name" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 0" "$(is "$status" 0)"
check "compares it, not shuts it, and names the recorded rename" "$(has_line "PASS: envoy runs ${other_rev} (ahead of ${commit})${renamed_note}")"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

echo "case: --listeners, a listener pin behind the commit"
new_case listeners-behind
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$legion" "$ahead_rev"
index "$cluster_pin" "$legion" "$behind_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$behind_rev" diverged 3
run_gate -- --listeners "$commit"
check "exits 1" "$(is "$status" 1)"
check "refuses naming the pin and its revision" "$(has_line "GATE SHUT: envoy-cluster runs ${behind_rev} (diverged from ${commit}, behind by 3)")"

echo "case: --listeners, a single-image listener pin"
new_case listeners-single
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
single "$envoy_pin" "$legion" "$ahead_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 0" "$(is "$status" 0)"
check "reads the single image's own labels" "$(has_line "PASS: envoy runs ${ahead_rev} (ahead of ${commit})")"

echo "case: --listeners, revision labels that are no commit id"
new_case listeners-revision-not-commit
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$legion" "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
index "$cluster_pin" "$legion" "abcdef"
index "$dispatch_listener_pin" "$legion" "${ahead_rev}a"
compare "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" ahead 0
compare "abcdef" ahead 0
compare "${ahead_rev}a" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses upper-case hex" \
  "$(has_line "UNKNOWN: envoy: ${envoy_pin} names revision AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA, which is no commit id")"
check "refuses 6 hex digits" "$(has_line "UNKNOWN: envoy-cluster: ${cluster_pin} names revision abcdef, which is no commit id")"
check "refuses 41 hex digits" "$(has_line "UNKNOWN: dispatch: ${dispatch_listener_pin} names revision ${ahead_rev}a, which is no commit id")"
check "makes no compare call" "$(lacks "/compare/" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

if [ "$failures" -ne 0 ]; then
  printf '%s check(s) failed\n' "$failures"
  exit 1
fi
echo "all checks passed"
