#!/usr/bin/env bash
# packages/envoy/scripts/daemon-release-gate.test.sh
#
# Proves daemon-release-gate.sh's verdicts with no network: a fake `gh` answers the stack files and
# the compare calls from files this test writes, and a fake `docker` answers `buildx imagetools
# inspect` the way the real one answers a single image and an OCI image index (an index's
# `.Image.Config.Labels` is `null`, and its platforms are read one at a time). Both record every
# call, so a case asserts what the gate printed and what it asked.
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
readonly legion="https://github.com/cybermonklabs/legion"
readonly agent_infra="https://github.com/theorem-labs/agent-infra"
readonly old_name="https://github.com/sjawhar/legion"
readonly ahead_rev="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
readonly identical_rev="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
readonly behind_rev="cccccccccccccccccccccccccccccccccccccccc"
readonly other_rev="dddddddddddddddddddddddddddddddddddddddd"

# The fake gh. `api repos/theorem-labs/agent-infra/contents/<path> --jq .content` answers the base64
# of FAKE_ROOT/files/<path>; `api repos/cybermonklabs/legion/compare/<base>...<head> --jq ...`
# answers FAKE_ROOT/compare/<head>, and fails as an unreachable API when FAKE_API_DOWN=1.
cat >"${temporary_dir}/bin/gh" <<'EOF'
#!/usr/bin/env bash
printf 'gh %s\n' "$*" >>"${FAKE_CALLS:?}"
[ "$1" = api ] || { echo "fake gh: unexpected command: $*" >&2; exit 2; }
case "$2" in
  repos/theorem-labs/agent-infra/contents/*)
    file="${FAKE_ROOT:?}/files/${2#repos/theorem-labs/agent-infra/contents/}"
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

# new_case starts a scenario of its own: no stack file, no image and no comparison yet.
new_case() {
  case_root="${temporary_dir}/case-$1"
  mkdir -p "${case_root}/files/legion" "${case_root}/files/envoy" "${case_root}/files/envoy-cluster" \
    "${case_root}/files/dispatch" "${case_root}/images" "${case_root}/compare"
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
# legion_stack <yaml body>: legion/Pulumi.prod.yaml, its projects list being the body.
legion_stack() {
  printf 'config:\n  agent-infra-legion:roleModels: {}\n  agent-infra-legion:projects:\n%s\n' "$1" >"${case_root}/files/legion/Pulumi.prod.yaml"
}
# listener_stacks <envoy> <envoy-cluster> <dispatch envoyImage> [<dispatch listenerImage>]
listener_stacks() {
  printf 'config:\n  agent-infra-envoy:listenerImage: %s\n' "$1" >"${case_root}/files/envoy/Pulumi.prod.yaml"
  printf 'config:\n  agent-infra-envoy-cluster:envoyImage: %s\n' "$2" >"${case_root}/files/envoy-cluster/Pulumi.prod.yaml"
  {
    printf 'config:\n  agent-infra-dispatch:envoyImage: %s\n' "$3"
    [ -z "${4:-}" ] || printf '  agent-infra-dispatch:listenerImage: %s\n' "$4"
  } >"${case_root}/files/dispatch/Pulumi.prod.yaml"
}

# run_gate [env assignments...] -- [gate arguments...]: runs the gate in the current case; sets
# $status.
run_gate() {
  local assignments=()
  while [ "$#" -gt 0 ] && [ "$1" != -- ]; do assignments+=("$1"); shift; done
  [ "$#" -eq 0 ] || shift
  : >"$out"
  : >"$err"
  : >"$calls"
  status=0
  env PATH="${temporary_dir}/bin:${PATH}" FAKE_CALLS="$calls" FAKE_ROOT="$case_root" "${assignments[@]}" \
    bash "$script" "$@" >"$out" 2>"$err" || status=$?
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
readonly infra_image="us-west1-docker.pkg.dev/theorem-agent-infra/legion/worker-infra@sha256:1111"
readonly fleet="    - key: SMOKE
      replicas: 0
      image: ${worker_smoke}
    - key: LINUX
      replicas: 1
      image: us-west1-docker.pkg.dev/theorem-agent-infra/legion/worker-lean@sha256:2222
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
new_case agent-infra
legion_stack "$fleet"
single "$worker_a" "$agent_infra" "$other_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$identical_rev" identical 0
compare "$other_rev" ahead 0
run_gate -- "$commit"
check "exits 1" "$(is "$status" 1)"
check "refuses naming the source" "$(has_line "GATE SHUT: LINUX runs an image built from ${agent_infra}")"
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

# agent-infra legion/host.go: an entry at `runtime: tmux` runs its daemon on the project's host VM
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
check "says it read no pin, rather than opening" "$(has_line "UNKNOWN: legion/Pulumi.prod.yaml lists no running daemon, so no pin was read")"
check "the verdict is never GATE OPEN" "$(lacks "GATE OPEN" "$out")"

echo "case: the stack file cannot be read"
new_case no-stack
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file" "$(has_line "UNKNOWN: cannot read theorem-labs/agent-infra legion/Pulumi.prod.yaml: gh: Not Found (HTTP 404)")"

echo "case: a commit that is no commit id"
new_case usage
run_gate -- "main"
check "exits 2" "$(is "$status" 2)"
check "says what it takes" "$(grep -qF "usage: daemon-release-gate.sh [--listeners] <commit>" "$err" && echo true || echo false)"

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

# The deployment requires dispatch's listenerImage (agent-infra dispatch/main.go loadSettings; its
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
  "$(has_line "UNKNOWN: dispatch: dispatch/Pulumi.prod.yaml sets no agent-infra-dispatch:listenerImage")"
check "never reads dispatch's server image" "$(lacks "$server_pin" "$calls")"
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
# the two names as one repository in legion/Pulumi.prod.yaml's repositoryRenames, the value the
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
readonly renamed_note="; built as ${old_name}, which legion/Pulumi.prod.yaml's repositoryRenames makes one repository with ${legion}"

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
  "$(has_line "UNKNOWN: envoy: built from ${old_name}, and the repository renames of theorem-labs/agent-infra legion/Pulumi.prod.yaml could not be read: gh: Not Found (HTTP 404)")"
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
index "$envoy_pin" "$agent_infra" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 1" "$(is "$status" 1)"
check "refuses naming the source" "$(has_line "GATE SHUT: envoy runs an image built from ${agent_infra}")"
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
