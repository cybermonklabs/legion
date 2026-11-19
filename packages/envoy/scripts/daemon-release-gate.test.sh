#!/usr/bin/env bash
# packages/envoy/scripts/daemon-release-gate.test.sh
#
# Proves daemon-release-gate.sh's verdicts with no network: a fake `gh` answers the deployment's
# files and directories, the release tags, the marketplace and the compare calls from files this
# test writes; a fake `docker` answers `buildx imagetools inspect` the way the real one answers a
# single image and an OCI image index (an index's `.Image.Config.Labels` is `null`, and its
# platforms are read one at a time) and `run` the way a worker image answers `cat` and `legion
# version`; and a fake `npm` answers `npm view`. All three record every call, so a case asserts
# what the gate printed and what it asked. Each case's HOME is a directory of its own, holding the
# developer-box files --clients reads.
#
# Negative control, run when this was written: make the gate read an index with
# `{{json .Image.Config.Labels}}`, as a single image is read; every --listeners row below that
# expects a comparison prints `UNKNOWN` instead, which is the blindness an index-shaped pin causes.
# Two controls run with every run of this file: a copy of the gate with one function replaced
# (mutant, below) must open on the fixture that exists to catch that replacement.
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly script="${project_root}/packages/envoy/scripts/daemon-release-gate.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
trap 'rm -rf "$temporary_dir"' EXIT
mkdir -p "${temporary_dir}/bin"

readonly commit="52b3cf5c1b6c7f42f6b8d2a1e3c4d5e6f7a8b9c0"
readonly legion="https://github.com/cybermonklabs/legion"
readonly other_source="https://github.com/example-org/deployment"
readonly old_name="https://github.com/sjawhar/legion"
readonly ahead_rev="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
readonly identical_rev="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
readonly behind_rev="cccccccccccccccccccccccccccccccccccccccc"
readonly other_rev="dddddddddddddddddddddddddddddddddddddddd"
# The commit of the deployment repository every case reads it at.
readonly deployment_head="eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

# The deployment's two settings, in neutral forms. Every case runs the gate with both, unless it
# leaves one out (run_gate --unset) or sets it empty.
readonly deployment="example-org/deployment"
readonly project_prefix="deployment"
export DAEMON_RELEASE_GATE_REPOSITORY="$deployment"
export DAEMON_RELEASE_GATE_PULUMI_PROJECT_PREFIX="$project_prefix"

# The fake gh. It finds the endpoint among its arguments and drops a `?ref=` query, then answers:
# `repos/example-org/deployment/commits/HEAD` with FAKE_HEAD; `.../contents/<path>` with the bytes
# of FAKE_ROOT/files/<path>, or for a directory one `<name>\t<file|dir>` line per entry, as the
# gate's --jq prints GitHub's listing; `.../tarball/<sha>` with a gzipped tar of FAKE_ROOT/files
# under one top directory, as GitHub's archive holds a repository (bytes that are no archive when
# FAKE_ROOT/corrupt-archive exists); `repos/cybermonklabs/legion/compare/<base>...<head>` with
# FAKE_ROOT/compare/<head>, failing as an unreachable API when FAKE_API_DOWN=1;
# `repos/cybermonklabs/legion/commits/refs/tags/<tag>` with FAKE_ROOT/tags/<tag>; and
# `repos/<owner>/<repo>/contents/.claude-plugin/marketplace.json` with
# FAKE_ROOT/marketplaces/<owner>/<repo>.json. An endpoint with a file at FAKE_ROOT/refuse/<endpoint
# with / as _> fails with that file's text. Any other call is refused.
cat >"${temporary_dir}/bin/gh" <<'EOF'
#!/usr/bin/env bash
printf 'gh %s\n' "$*" >>"${FAKE_CALLS:?}"
[ "$1" = api ] || { echo "fake gh: unexpected command: $*" >&2; exit 2; }
endpoint=""
for arg in "$@"; do
  case "$arg" in repos/*) endpoint=$arg && break ;; esac
done
[ -n "$endpoint" ] || { echo "fake gh: no endpoint in: $*" >&2; exit 2; }
path=${endpoint%%\?*}
refusal="${FAKE_ROOT:?}/refuse/$(printf '%s' "$path" | tr '/' '_')"
[ ! -f "$refusal" ] || { cat "$refusal" >&2; exit 1; }
case "$path" in
  repos/example-org/deployment/commits/HEAD) echo "${FAKE_HEAD:?}" ;;
  repos/example-org/deployment/tarball/*)
    if [ -e "${FAKE_ROOT}/corrupt-archive" ]; then
      echo "this is no archive"
    else
      tar -czf - -C "${FAKE_ROOT}/files" --transform "s,^\\.,example-org-deployment-${path##*/}," .
    fi
    ;;
  repos/example-org/deployment/contents/*)
    file="${FAKE_ROOT}/files/${path#repos/example-org/deployment/contents/}"
    if [ -d "$file" ]; then
      for entry in "$file"/*; do
        [ -e "$entry" ] || continue
        if [ -d "$entry" ]; then type=dir; else type=file; fi
        printf '%s\t%s\n' "${entry##*/}" "$type"
      done
    elif [ -f "$file" ]; then
      cat "$file"
    else
      echo "gh: Not Found (HTTP 404)" >&2
      exit 1
    fi
    ;;
  repos/cybermonklabs/legion/compare/*)
    [ "${FAKE_API_DOWN:-0}" != 1 ] || { echo "error connecting to api.github.com" >&2; exit 1; }
    head="${path##*...}"
    [ -f "${FAKE_ROOT}/compare/${head}" ] || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
    cat "${FAKE_ROOT}/compare/${head}"
    ;;
  repos/cybermonklabs/legion/commits/refs/tags/*)
    tag=${path##*/}
    [ -f "${FAKE_ROOT}/tags/${tag}" ] || { echo "gh: No commit found for SHA: refs/tags/${tag} (HTTP 422)" >&2; exit 1; }
    cat "${FAKE_ROOT}/tags/${tag}"
    ;;
  repos/*/contents/.claude-plugin/marketplace.json)
    repo=${path#repos/}
    repo=${repo%/contents/*}
    [ -f "${FAKE_ROOT}/marketplaces/${repo}.json" ] || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
    cat "${FAKE_ROOT}/marketplaces/${repo}.json"
    ;;
  *) echo "fake gh: unexpected api call: $*" >&2; exit 2 ;;
esac
EOF
# The fake docker. A ref's answers are files under FAKE_ROOT/images/<ref with / : @ as _>:
# mediatype, labels (a single image's) and platform-<os>_<arch> (an index's, one per platform),
# and platforms, the index's platform list as the range format prints it; and, for `run`, fs/<path>
# for `--entrypoint cat <path>` and legion-version for `--entrypoint /opt/legion/bin/legion version`.
# A ref with no fs/ is one the registry does not hold, as `docker run` reports it.
cat >"${temporary_dir}/bin/docker" <<'EOF'
#!/usr/bin/env bash
printf 'docker %s\n' "$*" >>"${FAKE_CALLS:?}"
if [ "$1" = run ]; then
  shift
  entrypoint=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --rm) shift ;;
      --network) shift 2 ;;
      --entrypoint) entrypoint=$2 && shift 2 ;;
      -*) echo "fake docker: unexpected run option: $1" >&2; exit 2 ;;
      *) break ;;
    esac
  done
  ref=$1
  shift
  dir="${FAKE_ROOT:?}/images/$(printf '%s' "$ref" | tr '/:@' '___')"
  if [ ! -d "$dir/fs" ]; then
    echo "Unable to find image '$ref' locally" >&2
    echo "docker: Error response from daemon: manifest for $ref not found: manifest unknown" >&2
    printf '\nRun '"'"'docker run --help'"'"' for more information\n' >&2
    exit 125
  fi
  case "$entrypoint $*" in
    "cat "*)
      [ -f "$dir/fs$1" ] || { echo "cat: $1: No such file or directory" >&2; exit 1; }
      cat "$dir/fs$1"
      ;;
    "/opt/legion/bin/legion version") cat "$dir/legion-version" ;;
    *) echo "fake docker: unexpected run: $entrypoint $*" >&2; exit 2 ;;
  esac
  exit 0
fi
[ "$1 $2 $3" = "buildx imagetools inspect" ] && [ "$5" = --format ] || { echo "fake docker: unexpected command: $*" >&2; exit 2; }
dir="${FAKE_ROOT:?}/images/$(printf '%s' "$4" | tr '/:@' '___')"
[ -f "$dir/mediatype" ] || { echo "ERROR: $4: not found" >&2; exit 1; }
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
# The fake npm. `npm view <spec> version --json` answers FAKE_ROOT/npm/<spec with / @ < > = as _>,
# and fails as an unreachable registry when FAKE_NPM_DOWN=1.
cat >"${temporary_dir}/bin/npm" <<'EOF'
#!/usr/bin/env bash
printf 'npm %s\n' "$*" >>"${FAKE_CALLS:?}"
[ "$1 $3 $4" = "view version --json" ] || { echo "fake npm: unexpected command: $*" >&2; exit 2; }
[ "${FAKE_NPM_DOWN:-0}" != 1 ] || { echo "npm error code ENOTFOUND" >&2; exit 1; }
file="${FAKE_ROOT:?}/npm/$(printf '%s' "$2" | tr '/@<>=' '_____')"
[ -f "$file" ] || { echo "npm error code E404" >&2; exit 1; }
cat "$file"
EOF
chmod +x "${temporary_dir}/bin/gh" "${temporary_dir}/bin/docker" "${temporary_dir}/bin/npm"

failures=0
case_root=""
out="${temporary_dir}/out"
err="${temporary_dir}/err"
calls="${temporary_dir}/calls"
status=0
gate_under_test="$script"

# new_case starts a scenario of its own: no deployment file, no image, no comparison and an empty
# HOME.
new_case() {
  case_root="${temporary_dir}/case-$1"
  mkdir -p "${case_root}/files/legion" "${case_root}/files/envoy-cluster" "${case_root}/files/dispatch" \
    "${case_root}/images" "${case_root}/compare" "${case_root}/tags" "${case_root}/refuse" \
    "${case_root}/marketplaces" "${case_root}/npm" "${case_root}/home"
}
# compare <revision> <status> <behind_by>: what GitHub answers for <commit>...<revision>.
compare() {
  printf '{"status":"%s","behind_by":%s}\n' "$2" "$3" >"${case_root}/compare/$1"
}
# refuse <endpoint> <answer>: GitHub answers <endpoint> (no query) with the error <answer>.
refuse() {
  printf '%s\n' "$2" >"${case_root}/refuse/$(printf '%s' "$1" | tr '/' '_')"
}
# image_dir <ref>: the directory of the fake docker's answers for <ref>.
image_dir() {
  printf '%s/images/%s' "$case_root" "$(printf '%s' "$1" | tr '/:@' '___')"
}
# single <ref> <source> <revision>: a single image labelled with source and revision; an empty
# source or revision leaves that label out.
single() {
  local dir
  dir=$(image_dir "$1")
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
  dir=$(image_dir "$1")
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
# worker <ref> <pi-envoy version> <legion commit> [no-claude-envoy]: a worker image's files, as
# `docker run` reads them: /opt/legion/pi-envoy/package.json at the version, the claude-envoy root's
# manifest at the source version the image never changes (left out with no-claude-envoy), and a
# legion whose `version` names the commit.
worker() {
  local dir
  dir=$(image_dir "$1")
  rm -rf "$dir/fs"
  mkdir -p "$dir/fs/opt/legion/pi-envoy"
  printf '{"name": "@sjawhar/pi-envoy", "version": "%s"}\n' "$2" >"$dir/fs/opt/legion/pi-envoy/package.json"
  if [ "${4:-}" != no-claude-envoy ]; then
    mkdir -p "$dir/fs/opt/legion/claude-envoy"
    printf '{"name": "@sjawhar/claude-envoy", "version": "1.0.0"}\n' >"$dir/fs/opt/legion/claude-envoy/package.json"
  fi
  printf 'legion (devel) commit %s\nplugins @sjawhar/pi-legion 19.28.1 @sjawhar/pi-envoy %s\n' "$3" "$2" >"$dir/legion-version"
}
# tag <tag> <commit>: the commit cybermonklabs/legion's release tag <tag> names.
tag() {
  printf '%s\n' "$2" >"${case_root}/tags/$1"
}
# dotfiles <pi-envoy pin> [<claude-envoy enabled>]: the developer box's ~/.dotfiles, with
# ~/.omp/plugins linked to its plugin tree, the pin installed there, and a settings.json whose
# marketplace legion-plugins is example-org/plugins, serving @sjawhar/claude-envoy at >=1.0.0,
# which npm matches to 3.3.0 and 4.0.0.
dotfiles() {
  local home="${case_root}/home" plugins="${case_root}/home/.dotfiles/omp/plugins"
  mkdir -p "$plugins/node_modules/@sjawhar/pi-envoy" "$home/.dotfiles/.claude" "$home/.omp" "${case_root}/marketplaces/example-org"
  printf '{"name": "omp-plugins", "private": true, "dependencies": {"@sjawhar/pi-envoy": "%s"}}\n' "$1" >"$plugins/package.json"
  printf '{"name": "@sjawhar/pi-envoy", "version": "%s"}\n' "$1" >"$plugins/node_modules/@sjawhar/pi-envoy/package.json"
  ln -sfn "$plugins" "$home/.omp/plugins"
  printf '{"extraKnownMarketplaces": {"legion-plugins": {"source": {"source": "github", "repo": "example-org/plugins"}}}, "enabledPlugins": {"claude-envoy@legion-plugins": %s}}\n' \
    "${2:-true}" >"$home/.dotfiles/.claude/settings.json"
  printf '{"name": "legion-plugins", "plugins": [{"name": "claude-envoy", "source": {"source": "npm", "package": "@sjawhar/claude-envoy", "version": ">=1.0.0"}}]}\n' \
    >"${case_root}/marketplaces/example-org/plugins.json"
  printf '["3.3.0", "4.0.0"]\n' >"${case_root}/npm/_sjawhar_claude-envoy___1.0.0"
}
# legion_stack <yaml list body>: the deployment's legion/projects/, one <nn>-<KEY>.yaml file per
# entry of the list, numbered 10, 20, ... in the list's order.
legion_stack() {
  mkdir -p "${case_root}/files/legion/projects"
  python3 - "${case_root}/files/legion/projects" "$1" <<'PY'
import os
import sys

import yaml

entries = yaml.safe_load("projects:\n" + sys.argv[2])["projects"]
for number, entry in enumerate(entries, start=1):
    with open(os.path.join(sys.argv[1], f"{number * 10:02d}-{entry['key']}.yaml"), "w") as handle:
        yaml.safe_dump(entry, handle)
PY
}
# envoy_program <declares|none>: the deployment's envoy/ program and the local module its go.mod
# replaces, ../shared. Either way the name NewNatsHub stands in every place that is not a use: a
# func's own name and a method's, a comment, a string and a raw string, a _test.go, and a comment
# in testdata/ and in another program of the repository; with declares, main.go calls it.
envoy_program() {
  local dir="${case_root}/files/envoy"
  mkdir -p "$dir/sdks/gcp" "$dir/testdata" "${case_root}/files/shared" "${case_root}/files/envoy-cluster"
  printf 'module example.org/deployment/envoy\n\nreplace example.org/sdk => ./sdks/gcp // the generated SDK\n\nreplace (\n\texample.org/shared => ../shared\n\texample.org/nats v1.0.0 => example.org/nats v1.0.1\n)\n' >"$dir/go.mod"
  printf 'package gcp\n\n// NewNatsHub registers a hub.\nfunc NewNatsHub(ctx int) error { return register("pkg:gcp:NatsHub") }\n\nfunc (m *module) NewNatsHub(name string) {}\n' >"$dir/sdks/gcp/natsHub.go"
  printf 'package main\n\nfunc TestHub() { _ = envoygcp.NewNatsHub(ctx, "hub") }\n' >"$dir/main_test.go"
  printf 'package fixture\n\n// envoygcp.NewNatsHub(ctx, "hub") once ran here\n' >"$dir/testdata/fixture.go"
  printf 'package main\n\n// envoy/main.go gives envoygcp.NewNatsHub as Zone.\n' >"${case_root}/files/envoy-cluster/alerts.go"
  cat >"$dir/hub_transform.go" <<'GO'
package main

/* envoygcp.NewNatsHub(ctx, "hub") made the VM hub */
const note = "NewNatsHub(\"" + `NewNatsHub(`
const quote = '"'
GO
  printf 'package shared\n\nfunc Names() []string { return nil }\n' >"${case_root}/files/shared/names.go"
  if [ "$1" = declares ]; then
    printf 'package main\n\n\thub, err := envoygcp.NewNatsHub(ctx, "hub", &envoygcp.NatsHubArgs{})\n' >"$dir/main.go"
  else
    printf 'package main\n\n\t// envoygcp.NewNatsHub(ctx, "hub", ...) went with the VM hub.\n' >"$dir/main.go"
  fi
}
# listener_stacks <envoy> <envoy-cluster> <dispatch envoyImage> [<dispatch listenerImage>]: the
# three stack files, an empty <envoy> leaving envoy's listenerImage out, and an envoy/ program that
# declares the VM hub.
listener_stacks() {
  envoy_program declares
  {
    printf 'config:\n'
    [ -z "$1" ] || printf '  %s-envoy:listenerImage: %s\n' "$project_prefix" "$1"
    printf '  %s-envoy:hubZone: us-west1-a\n' "$project_prefix"
  } >"${case_root}/files/envoy/Pulumi.prod.yaml"
  printf 'config:\n  %s-envoy-cluster:envoyImage: %s\n' "$project_prefix" "$2" >"${case_root}/files/envoy-cluster/Pulumi.prod.yaml"
  {
    printf 'config:\n  %s-dispatch:envoyImage: %s\n' "$project_prefix" "$3"
    [ -z "${4:-}" ] || printf '  %s-dispatch:listenerImage: %s\n' "$project_prefix" "$4"
  } >"${case_root}/files/dispatch/Pulumi.prod.yaml"
}

# run_gate [--unset <variable>]... [env assignments...] -- [gate arguments...]: runs the gate in the
# current case, with HOME the case's own and no CLAUDE_CONFIG_DIR of the machine running this test
# unless an assignment gives one, each --unset variable taken out of its environment last; sets
# $status.
run_gate() {
  local unsets=() assignments=()
  while [ "$#" -gt 0 ] && [ "$1" != -- ]; do
    if [ "$1" = --unset ]; then
      unsets+=(-u "$2")
      shift 2
    else
      assignments+=("$1")
      shift
    fi
  done
  [ "$#" -eq 0 ] || shift
  : >"$out"
  : >"$err"
  : >"$calls"
  status=0
  env -u CLAUDE_CONFIG_DIR PATH="${temporary_dir}/bin:${PATH}" HOME="${case_root}/home" FAKE_CALLS="$calls" \
    FAKE_ROOT="$case_root" FAKE_HEAD="$deployment_head" "${assignments[@]}" \
    env "${unsets[@]}" bash "$gate_under_test" "$@" >"$out" 2>"$err" || status=$?
}

# mutant <function> <name>: writes a copy of the gate whose <function> is the definition read from
# stdin, at ${temporary_dir}/mutant-<name>.sh, and prints its path. It stops this test when the
# gate defines no <function> as `<function>() {` ... `}`, so a control can never run against a
# copy that is the gate unchanged.
mutant() {
  local copy="${temporary_dir}/mutant-$2.sh" body="${temporary_dir}/mutant-$2.body"
  cat >"$body"
  if ! awk -v name="$1" -v body="$body" '
    $0 == name "() {" { while ((getline line < body) > 0) print line; skipping = 1; found = 1; next }
    skipping { if ($0 == "}") skipping = 0; next }
    { print }
    END { exit !found }' "$script" >"$copy"; then
    echo "FAIL: the gate defines no $1() for the $2 control to replace" >&2
    exit 1
  fi
  printf '%s' "$copy"
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
readonly lean_image="us-west1-docker.pkg.dev/example-project/legion/worker-lean@sha256:2222"
readonly infra_image="us-west1-docker.pkg.dev/example-project/legion/worker-infra@sha256:1111"
readonly fleet="    - key: SMOKE
      replicas: 0
      image: ${worker_smoke}
    - key: LINUX
      replicas: 1
      image: ${lean_image}
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
check "names the commit it read the deployment at" "$(has_line "READ: ${deployment} at ${deployment_head}")"
check "LINUX passes on its daemonImage" "$(has_line "PASS: LINUX runs ${ahead_rev} (ahead of ${commit})")"
check "INFRA passes on its daemonImage" "$(has_line "PASS: INFRA runs ${identical_rev} (identical to ${commit})")"
check "SMOKE at replicas 0 is skipped by name" "$(has_line "SKIPPED: SMOKE runs no daemon (replicas: 0)")"
check "an entry with daemon: false is skipped by name" "$(has_line "SKIPPED: QUIET runs no daemon (daemon: false)")"
check "the image of an entry that sets daemonImage is never read" "$(lacks "$infra_image" "$calls")"
check "the image of an entry that runs no daemon is never read" "$(lacks "$worker_smoke" "$calls")"
check "reads every project file of legion/projects/" \
  "$(grep -qF "contents/legion/projects/40-QUIET.yaml?ref=" "$calls" && echo true || echo false)"
check "reads every deployment file at the one commit" \
  "$(awk -v ref="?ref=${deployment_head}" '/\/contents\// && !index($0, ref) { bad = 1 } END { exit bad }' "$calls" && echo true || echo false)"

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
new_case other-repository
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

# The deployment's legion/host.go: an entry at `runtime: tmux` runs its daemon on the project's
# host VM from host.legionWorker's legion (checkHost refuses daemonImage, replicas and daemon on
# it), so its `image` says nothing about the daemon. The gate reads no image for it: it is UNKNOWN
# by name. So is a runtime the gate does not know; an entry that names `runtime: kubernetes` is
# read as before.
#
# Negative control, run when this was written: drop the runtime branch from project_pins; the rows
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

echo "case: a deployment that lists no running daemon"
new_case empty
legion_stack "    - key: SMOKE
      replicas: 0
      image: ${worker_smoke}"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "says it read no pin, rather than opening" "$(has_line "UNKNOWN: legion/projects/ lists no running daemon, so no image was read")"
check "the verdict is never GATE OPEN" "$(lacks "GATE OPEN" "$out")"

# The deployment keeps one file per project in legion/projects/, named <nn>-<KEY>.yaml and read in
# file-name order, and its program refuses any other file there and a file whose key is not its
# name's. The gate holds the directory to the same rule: what it cannot take as a project file is
# UNKNOWN, never a project left out.
echo "case: legion/projects/ cannot be listed"
new_case no-projects
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the directory and GitHub's answer" \
  "$(has_line "UNKNOWN: cannot list ${deployment} legion/projects/ at ${deployment_head}: gh: Not Found (HTTP 404)")"

echo "case: legion/projects/ holds a file that is no project file"
new_case projects-stray-file
legion_stack "$fleet"
printf '# notes\n' >"${case_root}/files/legion/projects/README.md"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file" \
  "$(has_line "UNKNOWN: ${deployment} legion/projects/ at ${deployment_head} holds README.md (file), which is no <nn>-<KEY>.yaml project file")"
check "reads no daemon image from a directory it could not take" "$(lacks "$worker_a" "$calls")"

echo "case: a project file whose key is not its name's"
new_case projects-wrong-key
legion_stack "$fleet"
printf 'key: BET\nreplicas: 1\ndaemonImage: %s\n' "$worker_a" >"${case_root}/files/legion/projects/20-LINUX.yaml"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file and its key" \
  "$(has_line "UNKNOWN: ${deployment} legion/projects/ at ${deployment_head}: legion/projects/20-LINUX.yaml declares key 'BET', not LINUX")"

echo "case: a project file GitHub does not answer for"
new_case projects-unreadable
legion_stack "$fleet"
refuse "repos/${deployment}/contents/legion/projects/20-LINUX.yaml" "gh: Server Error (HTTP 502)"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file and GitHub's answer" \
  "$(has_line "UNKNOWN: cannot read ${deployment} legion/projects/20-LINUX.yaml at ${deployment_head}: gh: Server Error (HTTP 502)")"

echo "case: the head of the deployment repository cannot be read"
new_case no-head
legion_stack "$fleet"
refuse "repos/${deployment}/commits/HEAD" "gh: Bad credentials (HTTP 401)"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming GitHub's answer" "$(has_line "UNKNOWN: cannot read the head of ${deployment}: gh: Bad credentials (HTTP 401)")"
check "reads no deployment file" "$(lacks "/contents/" "$calls")"
check "the verdict is UNKNOWN" "$(last_line_is "UNKNOWN")"

echo "case: a commit that is no commit id"
new_case usage
run_gate -- "main"
check "exits 2" "$(is "$status" 2)"
check "says what it takes" "$(grep -qF "usage: daemon-release-gate.sh [--listeners | --clients] <commit>" "$err" && echo true || echo false)"

# The deployment's settings have no default in this repository, so a run without one its mode
# reads, or with it empty, is a usage error naming it, before any deployment file is read or any
# image inspected: the repository in every mode, and the Pulumi project prefix with --listeners,
# the one mode that reads a stack's config keys. Each case holds a deployment the gate would open
# on, so a gate that went on without the setting would say something other than the refusal.
#
# Negative control, run when this was written: drop the two require_setting calls from the gate;
# an unset setting then ends the run on bash's own `unbound variable` (exit 1), failing each unset
# case's exit and refusal rows, and an empty repository makes a `repos//commits/HEAD` call the fake
# gh refuses and an empty prefix reads no `-envoy-cluster:envoyImage`, both `UNKNOWN` (exit 3),
# failing every row of each empty case. No other row of this file fails.
for setting in DAEMON_RELEASE_GATE_REPOSITORY DAEMON_RELEASE_GATE_PULUMI_PROJECT_PREFIX; do
  for shape in unset empty; do
    echo "case: ${setting} ${shape}"
    new_case "${shape}-${setting}"
    legion_stack "$fleet"
    listener_stacks "$worker_a" "$worker_a" "$worker_a" "$worker_a"
    single "$worker_a" "$legion" "$ahead_rev"
    single "$worker_b" "$legion" "$identical_rev"
    compare "$ahead_rev" ahead 0
    compare "$identical_rev" identical 0
    mode=()
    [ "$setting" = DAEMON_RELEASE_GATE_REPOSITORY ] || mode=(--listeners)
    if [ "$shape" = unset ]; then
      run_gate --unset "$setting" -- "${mode[@]}" "$commit"
    else
      run_gate "${setting}=" -- "${mode[@]}" "$commit"
    fi
    check "exits 2" "$(is "$status" 2)"
    check "refuses naming ${setting}" \
      "$(grep -qF "daemon-release-gate.sh: ${setting} is unset or empty" "$err" && echo true || echo false)"
    check "reads no deployment file and inspects no image" "$([ -s "$calls" ] && echo false || echo true)"
    check "prints no verdict" "$([ -s "$out" ] && echo false || echo true)"
  done
done

echo "case: a mode that reads no stack config key needs no Pulumi project prefix"
new_case no-prefix-needed
legion_stack "$fleet"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate --unset DAEMON_RELEASE_GATE_PULUMI_PROJECT_PREFIX -- "$commit"
check "exits 0" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

readonly envoy_pin="ghcr.io/cybermonklabs/legion/envoy@sha256:e001"
readonly cluster_pin="ghcr.io/cybermonklabs/legion/envoy@sha256:e002"
readonly server_pin="ghcr.io/cybermonklabs/legion/envoy@sha256:e003"
readonly dispatch_listener_pin="ghcr.io/cybermonklabs/legion/envoy@sha256:e004"

echo "case: --listeners, every listener index at or after the commit, the VM hub declared"
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
check "fetches the deployment repository once, at the one commit" \
  "$([ "$(grep -c "tarball/${deployment_head}" "$calls")" = 1 ] && echo true || echo false)"
check "names the use that declares the hub" "$(has_line "VM HUB: envoy/main.go:3 at ${deployment_head} uses NewNatsHub")"
check "prints no NO VM HUB line" "$(lacks "NO VM HUB" "$out")"

# The VM hub's removal (the deployment's envoy/ program losing its gcp:NatsHub) deletes the
# listenerImage key with it, so an absent key cannot tell a gone hub from a stack file that lost
# its pin. The gate decides the hub from the program, positively: a declared hub whose pin is gone
# is UNKNOWN, and only a program that declares none, every file of it read, drops the VM pin.
echo "case: --listeners, the VM hub declared and its listenerImage absent"
new_case listeners-hub-no-pin
listener_stacks "" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the key the declared hub needs" \
  "$(has_line "UNKNOWN: envoy: envoy/Pulumi.prod.yaml sets no ${project_prefix}-envoy:listenerImage")"
check "prints no NO VM HUB line" "$(lacks "NO VM HUB" "$out")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

# Control: a gate whose VM leg reads an absent listenerImage as no VM hub, the reading this leg
# exists to refuse, opens on the fixture above; so that fixture fails such a gate, by name.
hub_from_pin=$(mutant vm_hub hub-from-pin <<'EOF'
vm_hub() {
  stack_file envoy/Pulumi.prod.yaml || return 1
  if [ -n "$(config_value "${project_prefix}-envoy:listenerImage")" ]; then hub=yes; else hub=no; fi
}
EOF
)
gate_under_test="$hub_from_pin"
run_gate -- --listeners "$commit"
gate_under_test="$script"
check "control: a VM leg that reads the absent pin as no hub opens on the declared-and-absent fixture" \
  "$([ "$(last_line_is "GATE OPEN")" = true ] && grep -q '^NO VM HUB: ' "$out" && echo true || echo false)"

echo "case: --listeners, no Go file of envoy/ declares the VM hub"
new_case listeners-no-hub
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
envoy_program none
index "$envoy_pin" "$legion" "$behind_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$identical_rev"
compare "$behind_rev" behind 4
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- --listeners "$commit"
check "exits 0" "$(is "$status" 0)"
check "says the program declares no VM hub, at the commit it read" \
  "$(has_line "NO VM HUB: envoy/ at ${deployment_head} declares no gcp:NatsHub")"
check "a func's own name, a method's, a comment, a string, a _test.go, testdata/ and another program declare nothing" \
  "$(grep -q '^VM HUB:' "$out" && echo false || echo true)"
check "reads no VM pin: not envoy's stack file, not its image" \
  "$([ "$(lacks "contents/envoy/Pulumi.prod.yaml" "$calls")" = true ] && lacks "$envoy_pin" "$calls" || echo false)"
check "opens on the other two pins" \
  "$(grep -qxF "PASS: envoy-cluster runs ${ahead_rev} (ahead of ${commit})" "$out" && grep -qxF "PASS: dispatch runs ${identical_rev} (identical to ${commit})" "$out" && echo true || echo false)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

echo "case: --listeners, the deployment repository cannot be fetched"
new_case listeners-hub-unfetched
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
refuse "repos/${deployment}/tarball/${deployment_head}" "gh: Resource not accessible by integration (HTTP 403)"
index "$envoy_pin" "$legion" "$ahead_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming GitHub's answer" \
  "$(has_line "UNKNOWN: envoy: cannot fetch ${deployment} at ${deployment_head}: gh: Resource not accessible by integration (HTTP 403)")"
check "neither reads the VM pin nor says the hub is gone" \
  "$([ "$(lacks "$envoy_pin" "$calls")" = true ] && lacks "NO VM HUB" "$out" || echo false)"
check "still reads the other two" "$(has_line "PASS: envoy-cluster runs ${ahead_rev} (ahead of ${commit})")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --listeners, an archive of the repository that cannot be read"
new_case listeners-hub-corrupt
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
envoy_program none
: >"${case_root}/corrupt-archive"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the archive" \
  "$(grep -qF "UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: the archive cannot be read: " "$out" && echo true || echo false)"
check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"

echo "case: --listeners, envoy/ holds no Go file"
new_case listeners-hub-no-go
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
find "${case_root}/files/envoy" -name '*.go' -delete
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, having nothing to decide by" \
  "$(has_line "UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: envoy/ holds no Go file to read the VM hub from")"
check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"

# Every form of a use declares the hub, wherever the repository's code holds it: a space or a
# comment between the name and its call, a reference called later, a helper package of envoy/, the
# module envoy/go.mod replaces with a local directory, a package under testdata/ or an _ directory,
# which go build compiles once something imports it, and another program of the repository, which
# can only lean the gate toward reading the VM pin. Each fixture's VM pin is behind, so a gate that
# missed the use would print NO VM HUB and open.
for shape in space comment reference helper replaced testdata underscore other-program; do
  echo "case: --listeners, the VM hub declared by a use of shape ${shape}"
  new_case "listeners-use-${shape}"
  listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
  envoy_program none
  use='package hub\n\nfunc Run() { envoygcp.NewNatsHub(ctx, "hub") }\n'
  case "$shape" in
    space) printf 'package main\n\n\thub, err := envoygcp.NewNatsHub (ctx, "hub")\n' >"${case_root}/files/envoy/main.go" ;;
    comment) printf 'package main\n\n\thub, err := envoygcp.NewNatsHub /* the VM hub */ (ctx, "hub")\n' >"${case_root}/files/envoy/main.go" ;;
    reference) printf 'package main\n\nvar newHub = envoygcp.NewNatsHub\n' >"${case_root}/files/envoy/main.go" ;;
    helper) mkdir -p "${case_root}/files/envoy/infra" && printf '%b' "$use" >"${case_root}/files/envoy/infra/hub.go" ;;
    replaced) printf '%b' "$use" >"${case_root}/files/shared/hub.go" ;;
    testdata) mkdir -p "${case_root}/files/envoy/testdata/hub" && printf '%b' "$use" >"${case_root}/files/envoy/testdata/hub/hub.go" ;;
    underscore) mkdir -p "${case_root}/files/envoy/_infra" && printf '%b' "$use" >"${case_root}/files/envoy/_infra/hub.go" ;;
    other-program) printf '%b' "$use" >"${case_root}/files/envoy-cluster/hub.go" ;;
  esac
  index "$envoy_pin" "$legion" "$behind_rev"
  index "$cluster_pin" "$legion" "$ahead_rev"
  index "$dispatch_listener_pin" "$legion" "$ahead_rev"
  compare "$behind_rev" behind 4
  compare "$ahead_rev" ahead 0
  run_gate -- --listeners "$commit"
  check "exits 1" "$(is "$status" 1)"
  check "names a use" "$(grep -q "^VM HUB: .* at ${deployment_head} uses NewNatsHub$" "$out" && echo true || echo false)"
  check "reads the VM pin and shuts on it" "$(has_line "GATE SHUT: envoy runs ${behind_rev} (behind ${commit} by 4)")"
  check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"
done

# A file that names NewNatsHub and cannot be read as Go could hide a use, so it is UNKNOWN; a file
# that does not name it cannot use it, whatever else is wrong with it.
for shape in comment utf-8 unrelated; do
  echo "case: --listeners, a Go file the gate cannot read as Go (${shape})"
  new_case "listeners-hub-not-go-${shape}"
  listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
  envoy_program none
  case "$shape" in
    comment)
      printf 'package main\n\n/* envoygcp.NewNatsHub(ctx, "hub") and a comment with no end\n' >"${case_root}/files/envoy/main.go"
      want="UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: envoy/main.go names NewNatsHub and has a /* comment with no end"
      ;;
    utf-8)
      printf 'package main\n\n// \xff\nvar hub = envoygcp.NewNatsHub\n' >"${case_root}/files/envoy/main.go"
      want="UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: envoy/main.go names NewNatsHub and is no UTF-8 text"
      ;;
    unrelated)
      printf 'package main\n\nfunc main( { /* \xff and no end\n' >"${case_root}/files/envoy/broken.go"
      want="NO VM HUB: envoy/ at ${deployment_head} declares no gcp:NatsHub"
      ;;
  esac
  index "$cluster_pin" "$legion" "$ahead_rev"
  index "$dispatch_listener_pin" "$legion" "$ahead_rev"
  compare "$ahead_rev" ahead 0
  run_gate -- --listeners "$commit"
  check "exits $([ "$shape" = unrelated ] && echo 0 || echo 3)" "$(is "$status" "$([ "$shape" = unrelated ] && echo 0 || echo 3)")"
  check "says ${want%%:*}" "$(has_line "$want")"
done

# A local replace or use that leaves the repository names code the gate cannot read, in any of the
# forms go.mod and go.work take: quoted, indented, in a block, absolute, in a go.work.
for shape in relative quoted indented absolute block work missing; do
  echo "case: --listeners, the program names a directory outside the repository (${shape})"
  new_case "listeners-go-mod-${shape}"
  listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
  envoy_program none
  mod="${case_root}/files/envoy/go.mod"
  outside="../../far"
  case "$shape" in
    relative) printf 'module example.org/deployment/envoy\n\nreplace example.org/far => ../../far\n' >"$mod" ;;
    quoted) printf 'module example.org/deployment/envoy\n\nreplace example.org/far => "../../far" // a comment\n' >"$mod" ;;
    indented) printf 'module example.org/deployment/envoy\n\n\treplace example.org/far v1.0.0 => ../../far\n' >"$mod" ;;
    absolute) outside="/srv/envoy helpers" && printf 'module example.org/deployment/envoy\n\nreplace example.org/far => "/srv/envoy helpers"\n' >"$mod" ;;
    block) printf 'module example.org/deployment/envoy\n\nreplace(\n\texample.org/near => ./near\n\texample.org/far => %s\n)\n' "\`../../far\`" >"$mod" ;;
    work) printf 'go 1.26\n\nuse (\n\t./envoy\n\t../far\n)\n' >"${case_root}/files/go.work" && outside="../far" ;;
    missing) rm "$mod" ;;
  esac
  case "$shape" in
    work) want="UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: go.work names ../far, a directory outside this repository" ;;
    missing) want="UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: envoy/ holds no go.mod" ;;
    *) want="UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: envoy/go.mod names ${outside}, a directory outside this repository" ;;
  esac
  index "$cluster_pin" "$legion" "$ahead_rev"
  index "$dispatch_listener_pin" "$legion" "$ahead_rev"
  compare "$ahead_rev" ahead 0
  run_gate -- --listeners "$commit"
  check "exits 3" "$(is "$status" 3)"
  check "refuses UNKNOWN naming why" "$(has_line "$want")"
  check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"
done

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
  "$(has_line "UNKNOWN: dispatch: dispatch/Pulumi.prod.yaml sets no ${project_prefix}-dispatch:listenerImage")"
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
# the two names as one repository in its project files' repositoryRenames, the value the daemon's
# ghrepo.Renames reads (packages/daemon/internal/ghrepo/renames.go). A build made under the old
# name is a build of this repository: it is compared like any other, and refused only for what the
# comparison says. A source no recorded pair makes one with this repository is still refused by
# name, and an unreadable record is UNKNOWN rather than a guess either way.
#
# Negative control, run when this was written: drop the alias check from judge; the rows below
# that record the pair print `GATE SHUT: envoy runs an image built from …` and make no compare.
readonly renamed_fleet="    - key: INFRA
      replicas: 0
      repositoryRenames:
        - from: sjawhar/legion
          to: cybermonklabs/legion"
readonly renamed_note="; built as ${old_name}, which legion/projects/'s repositoryRenames makes one repository with ${legion}"

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
  "$(has_line "UNKNOWN: envoy: built from ${old_name}, and the deployment's repository renames could not be read: cannot list ${deployment} legion/projects/ at ${deployment_head}: gh: Not Found (HTTP 404)")"
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

echo "case: a daemon built under the old name, the rename recorded on another project's file"
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

# --clients reads every client that delivers to a session: in each worker image of the fleet (both
# daemonImage and image of each running entry), the pi-envoy root, by its release tag, and the
# claude-envoy root, by the commit the image's own legion names (the image builds it from that
# checkout and never stamps it with a release version); and the developer box's pi-envoy pin and
# the claude-envoy version its marketplace serves, each by its release tag. clients_case sets up a
# fleet and a box where every one carries the commit; each case then moves one.
readonly envoy_ahead="17.40.0"
readonly envoy_behind="16.12.1"
readonly envoy_behind_rev="ffffffffffffffffffffffffffffffffffffffff"
clients_case() {
  new_case "$1"
  legion_stack "$fleet"
  worker "$worker_a" "$envoy_ahead" "$ahead_rev"
  worker "$lean_image" "$envoy_ahead" "$ahead_rev"
  worker "$worker_b" "$envoy_ahead" "$identical_rev"
  worker "$infra_image" "$envoy_ahead" "$identical_rev"
  tag "pi-envoy-v${envoy_ahead}" "$ahead_rev"
  tag "pi-envoy-v${envoy_behind}" "$envoy_behind_rev"
  tag "claude-envoy-v4.0.0" "$identical_rev"
  compare "$ahead_rev" ahead 0
  compare "$identical_rev" identical 0
  compare "$behind_rev" behind 12
  compare "$envoy_behind_rev" behind 300
  dotfiles "$envoy_ahead"
}
readonly pi_envoy_ahead="pi-envoy ${envoy_ahead} from pi-envoy-v${envoy_ahead} at ${ahead_rev} (ahead of ${commit})"
readonly claude_note="; the version legion-plugins serves for >=1.0.0"

echo "case: --clients, every client at or after the commit"
clients_case clients-open
run_gate -- --clients "$commit"
check "exits 0" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"
check "reads pi-envoy in the daemon image by its release tag" "$(has_line "PASS: LINUX daemonImage runs ${pi_envoy_ahead}")"
check "and in the session image" "$(has_line "PASS: LINUX image runs ${pi_envoy_ahead}")"
check "reads claude-envoy by the commit of the image's legion" \
  "$(has_line "PASS: INFRA image runs claude-envoy built at ${identical_rev}, the commit of the image's legion (identical to ${commit})")"
check "reads the developer box's pi-envoy pin" "$(has_line "PASS: dotfiles omp/plugins/package.json runs ${pi_envoy_ahead}")"
check "reads the claude-envoy version the marketplace serves, the highest npm matches" \
  "$(has_line "PASS: dotfiles .claude/settings.json runs claude-envoy 4.0.0 from claude-envoy-v4.0.0 at ${identical_rev} (identical to ${commit})${claude_note}")"
check "skips the entries that run no daemon, by name" \
  "$(grep -qxF "SKIPPED: SMOKE runs no daemon (replicas: 0)" "$out" && grep -qxF "SKIPPED: QUIET runs no daemon (daemon: false)" "$out" && echo true || echo false)"
check "never runs the image of an entry that runs no daemon" "$(lacks "$worker_smoke" "$calls")"
check "runs each image with no network" \
  "$(awk '/^docker run/ && !index($0, "--network none") { bad = 1 } END { exit bad }' "$calls" && echo true || echo false)"
check "reads each image's files with cat, from the roots a pod loads" \
  "$(grep -qF -- "--entrypoint cat ${lean_image} /opt/legion/pi-envoy/package.json" "$calls" && grep -qF -- "--entrypoint cat ${lean_image} /opt/legion/claude-envoy/package.json" "$calls" && echo true || echo false)"

echo "case: --clients, an image whose pi-envoy is behind the commit"
clients_case clients-pi-envoy-behind
worker "$lean_image" "$envoy_behind" "$ahead_rev"
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "shuts naming the image, the version and its tag" \
  "$(has_line "GATE SHUT: LINUX image runs pi-envoy ${envoy_behind} from pi-envoy-v${envoy_behind} at ${envoy_behind_rev} (behind ${commit} by 300)")"
check "the verdict is GATE SHUT" "$(last_line_is "GATE SHUT")"

echo "case: --clients, an image whose claude-envoy is behind the commit"
clients_case clients-claude-envoy-behind
worker "$infra_image" "$envoy_ahead" "$behind_rev"
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "shuts naming the image and the commit its claude-envoy was built at" \
  "$(has_line "GATE SHUT: INFRA image runs claude-envoy built at ${behind_rev}, the commit of the image's legion (behind ${commit} by 12)")"
check "while that image's pi-envoy passes" "$(has_line "PASS: INFRA image runs ${pi_envoy_ahead}")"
check "the verdict is GATE SHUT" "$(last_line_is "GATE SHUT")"

# Control: a gate that reads pi-envoy alone opens on the fixture above, whose one stale client is
# claude-envoy; so that fixture fails such a gate, by name.
pi_envoy_alone=$(mutant image_claude_envoy pi-envoy-alone <<'EOF'
image_claude_envoy() {
  :
}
EOF
)
gate_under_test="$pi_envoy_alone"
run_gate -- --clients "$commit"
gate_under_test="$script"
check "control: a gate that reads pi-envoy alone opens on the claude-envoy fixture" "$(last_line_is "GATE OPEN")"

echo "case: --clients, the developer box's pi-envoy pin behind the commit"
clients_case clients-dotfiles-behind
dotfiles "$envoy_behind"
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "shuts naming the pin, its version and its tag" \
  "$(has_line "GATE SHUT: dotfiles omp/plugins/package.json runs pi-envoy ${envoy_behind} from pi-envoy-v${envoy_behind} at ${envoy_behind_rev} (behind ${commit} by 300)")"
check "the verdict is GATE SHUT" "$(last_line_is "GATE SHUT")"

echo "case: --clients, an image with no claude-envoy package"
clients_case clients-no-claude-envoy
worker "$worker_b" "$envoy_ahead" "$identical_rev" no-claude-envoy
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the image and what it could not read" \
  "$(has_line "UNKNOWN: INFRA daemonImage: cannot read /opt/legion/claude-envoy/package.json in ${worker_b}: cat: /opt/legion/claude-envoy/package.json: No such file or directory")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, an image the registry does not answer for"
clients_case clients-image-unreadable
rm -r "$(image_dir "$worker_a")"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN for each client of the image, naming docker's answer" \
  "$([ "$(grep -c "^UNKNOWN: LINUX daemonImage: cannot read /opt/legion/.*manifest unknown$" "$out")" = 2 ] && echo true || echo false)"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, a pi-envoy version no release tag names"
clients_case clients-no-tag
worker "$worker_a" "17.99.0" "$ahead_rev"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the tag and GitHub's answer" \
  "$(has_line "UNKNOWN: LINUX daemonImage: pi-envoy 17.99.0 has no release tag pi-envoy-v17.99.0 in cybermonklabs/legion: gh: No commit found for SHA: refs/tags/pi-envoy-v17.99.0 (HTTP 422)")"

echo "case: --clients, a developer-box pin that is no exact version"
clients_case clients-pin-range
dotfiles "^17.40.0"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the pin" \
  "$(has_line "UNKNOWN: dotfiles omp/plugins/package.json: pi-envoy ^17.40.0 is no exact release version")"

echo "case: --clients, pi-envoy installed in a plugin tree outside ~/.dotfiles"
clients_case clients-outside-tree
mkdir -p "${case_root}/home/.omp/profiles/work/plugins/node_modules/@sjawhar/pi-envoy"
printf '{"name": "@sjawhar/pi-envoy", "version": "16.0.0"}\n' \
  >"${case_root}/home/.omp/profiles/work/plugins/node_modules/@sjawhar/pi-envoy/package.json"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the tree" \
  "$(has_line "UNKNOWN: dotfiles omp/plugins/package.json: @sjawhar/pi-envoy is installed outside ~/.dotfiles, in ${case_root}/home/.omp/profiles/work/plugins")"

echo "case: --clients, claude-envoy installed outside ~/.dotfiles"
clients_case clients-claude-outside
mkdir -p "${case_root}/home/.claude/plugins"
printf '{"version": 2, "plugins": {"claude-envoy@legion-plugins": [{"version": "1.0.0"}]}}\n' \
  >"${case_root}/home/.claude/plugins/installed_plugins.json"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming where it is installed" \
  "$(has_line "UNKNOWN: dotfiles .claude/settings.json: claude-envoy is installed outside ~/.dotfiles, in ${case_root}/home/.claude/plugins")"

echo "case: --clients, a settings.json that enables no claude-envoy"
clients_case clients-claude-off
dotfiles "$envoy_ahead" false
run_gate -- --clients "$commit"
check "exits 0" "$(is "$status" 0)"
check "skips it by name" "$(has_line "SKIPPED: dotfiles .claude/settings.json enables no claude-envoy@legion-plugins")"
check "asks npm nothing" "$(lacks "npm view" "$calls")"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

echo "case: --clients, npm cannot be reached"
clients_case clients-npm-down
run_gate FAKE_NPM_DOWN=1 -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming npm's answer" \
  "$(has_line "UNKNOWN: dotfiles .claude/settings.json: npm cannot resolve @sjawhar/claude-envoy@>=1.0.0: npm error code ENOTFOUND")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

# The dotfiles' copy of pi-envoy is what a new session loads. An install that did not finish leaves
# it behind the pin, and `omp plugin link` puts a link to a checkout at its path, which a tree
# linked to the dotfiles carries inside them; neither may read as the pin.
echo "case: --clients, the dotfiles' installed pi-envoy behind its pin"
clients_case clients-installed-behind
printf '{"name": "@sjawhar/pi-envoy", "version": "%s"}\n' "$envoy_behind" \
  >"${case_root}/home/.dotfiles/omp/plugins/node_modules/@sjawhar/pi-envoy/package.json"
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "shuts naming the installed version and the pin" \
  "$(has_line "GATE SHUT: dotfiles omp/plugins installed @sjawhar/pi-envoy runs pi-envoy ${envoy_behind} from pi-envoy-v${envoy_behind} at ${envoy_behind_rev} (behind ${commit} by 300); the pin names ${envoy_ahead}")"

echo "case: --clients, the dotfiles' pi-envoy a link to a checkout outside them"
clients_case clients-linked-checkout
mkdir -p "${case_root}/outside/pi-envoy"
printf '{"name": "@sjawhar/pi-envoy", "version": "%s"}\n' "$envoy_ahead" >"${case_root}/outside/pi-envoy/package.json"
rm -r "${case_root}/home/.dotfiles/omp/plugins/node_modules/@sjawhar/pi-envoy"
ln -s "${case_root}/outside/pi-envoy" "${case_root}/home/.dotfiles/omp/plugins/node_modules/@sjawhar/pi-envoy"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming where the link goes" \
  "$(has_line "UNKNOWN: dotfiles omp/plugins/package.json: @sjawhar/pi-envoy is installed outside ~/.dotfiles, in ${case_root}/home/.dotfiles/omp/plugins, a link to ${case_root}/outside/pi-envoy")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, a profile's plugin tree whose pi-envoy links to the dotfiles' copy"
clients_case clients-profile-link
mkdir -p "${case_root}/home/.omp/profiles/work/plugins/node_modules/@sjawhar"
ln -s "${case_root}/home/.dotfiles/omp/plugins/node_modules/@sjawhar/pi-envoy" \
  "${case_root}/home/.omp/profiles/work/plugins/node_modules/@sjawhar/pi-envoy"
run_gate -- --clients "$commit"
check "exits 0, the copy being the dotfiles' own" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

for shape in malformed directory; do
  echo "case: --clients, a Claude Code plugin record that is ${shape}"
  clients_case "clients-claude-record-${shape}"
  mkdir -p "${case_root}/home/.claude/plugins"
  if [ "$shape" = malformed ]; then
    printf '{"version": 2, "plugins": {"claude-envoy@legion-plugins": [' >"${case_root}/home/.claude/plugins/installed_plugins.json"
  else
    mkdir "${case_root}/home/.claude/plugins/installed_plugins.json"
  fi
  run_gate -- --clients "$commit"
  check "exits 3" "$(is "$status" 3)"
  check "refuses UNKNOWN naming the record" \
    "$(grep -qF "UNKNOWN: dotfiles .claude/settings.json: ${case_root}/home/.claude/plugins/installed_plugins.json cannot be read as JSON: " "$out" && echo true || echo false)"
  check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"
done

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

echo "case: --clients, claude-envoy installed in the dotfiles' Claude Code configuration, behind"
clients_case clients-claude-installed-behind
tag "claude-envoy-v3.3.0" "$behind_rev"
claude_install 3.3.0 3.3.0
run_gate CLAUDE_CONFIG_DIR="${case_root}/home/.dotfiles/.claude" -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "shuts naming the installed copy" \
  "$(has_line "GATE SHUT: dotfiles .claude/plugins installed claude-envoy runs claude-envoy 3.3.0 from claude-envoy-v3.3.0 at ${behind_rev} (behind ${commit} by 12)")"
check "still reads the version the marketplace serves" \
  "$(grep -qF "PASS: dotfiles .claude/settings.json runs claude-envoy 4.0.0 " "$out" && echo true || echo false)"

echo "case: --clients, a Claude Code install whose copy is behind what its record says"
clients_case clients-claude-copy-behind
tag "claude-envoy-v3.3.0" "$behind_rev"
claude_install 4.0.0 3.3.0
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "judges the copy, naming the record's word" \
  "$(has_line "GATE SHUT: dotfiles .claude/plugins installed claude-envoy runs claude-envoy 3.3.0 from claude-envoy-v3.3.0 at ${behind_rev} (behind ${commit} by 12); the plugin record says 4.0.0")"

for shape in outside no-path no-manifest; do
  echo "case: --clients, a Claude Code install the gate cannot hold to the dotfiles (${shape})"
  clients_case "clients-claude-install-${shape}"
  record="${case_root}/home/.dotfiles/.claude/plugins/installed_plugins.json"
  case "$shape" in
    outside)
      mkdir -p "${case_root}/outside/claude-envoy"
      printf '{"name": "@sjawhar/claude-envoy", "version": "4.0.0"}\n' >"${case_root}/outside/claude-envoy/package.json"
      claude_install 4.0.0 4.0.0 "${case_root}/outside/claude-envoy"
      want="${record} records an install of claude-envoy@legion-plugins at ${case_root}/outside/claude-envoy, outside $(readlink -f "${case_root}/home/.dotfiles/.claude")"
      ;;
    no-path)
      mkdir -p "$(dirname "$record")"
      printf '{"version": 2, "plugins": {"claude-envoy@legion-plugins": [{"version": "4.0.0"}]}}\n' >"$record"
      want="${record} records an install of claude-envoy@legion-plugins with no installPath"
      ;;
    no-manifest)
      claude_install 4.0.0 4.0.0
      rm "${case_root}/home/.dotfiles/.claude/plugins/cache/legion-plugins/claude-envoy/4.0.0/package.json"
      want="${record} records an install of claude-envoy@legion-plugins at ${case_root}/home/.dotfiles/.claude/plugins/cache/legion-plugins/claude-envoy/4.0.0, whose package.json cannot be read: "
      ;;
  esac
  run_gate -- --clients "$commit"
  check "exits 3" "$(is "$status" 3)"
  check "refuses UNKNOWN naming the install" \
    "$(grep -qF "UNKNOWN: dotfiles .claude/settings.json: ${want}" "$out" && echo true || echo false)"
done

# A link whose target is gone is something there that cannot be read, never nothing there.
for shape in package outside-record dotfiles-record; do
  echo "case: --clients, a link that does not resolve (${shape})"
  clients_case "clients-dangling-${shape}"
  case "$shape" in
    package)
      at="${case_root}/home/.dotfiles/omp/plugins/node_modules/@sjawhar/pi-envoy"
      rm -r "$at" && ln -s ../gone "$at"
      want="UNKNOWN: dotfiles omp/plugins/package.json: @sjawhar/pi-envoy is installed outside ~/.dotfiles, in ${case_root}/home/.dotfiles/omp/plugins, a link to ../gone, which does not exist"
      ;;
    outside-record)
      mkdir -p "${case_root}/home/.claude/plugins" && ln -s gone.json "${case_root}/home/.claude/plugins/installed_plugins.json"
      want="UNKNOWN: dotfiles .claude/settings.json: ${case_root}/home/.claude/plugins/installed_plugins.json cannot be read as JSON: "
      ;;
    dotfiles-record)
      mkdir -p "${case_root}/home/.dotfiles/.claude/plugins" && ln -s gone.json "${case_root}/home/.dotfiles/.claude/plugins/installed_plugins.json"
      want="UNKNOWN: dotfiles .claude/settings.json: ${case_root}/home/.dotfiles/.claude/plugins/installed_plugins.json cannot be read as JSON: "
      ;;
  esac
  run_gate -- --clients "$commit"
  check "exits 3" "$(is "$status" 3)"
  check "refuses UNKNOWN naming the link" "$(grep -qF "$want" "$out" && echo true || echo false)"
  check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"
done

echo "case: --clients, claude-envoy installed in the CLAUDE_CONFIG_DIR of another configuration"
clients_case clients-claude-config-dir
mkdir -p "${case_root}/other-claude/plugins"
printf '{"version": 2, "plugins": {"claude-envoy@legion-plugins": [{"version": "4.0.0"}]}}\n' \
  >"${case_root}/other-claude/plugins/installed_plugins.json"
run_gate CLAUDE_CONFIG_DIR="${case_root}/other-claude" -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming where it is installed" \
  "$(has_line "UNKNOWN: dotfiles .claude/settings.json: claude-envoy is installed outside ~/.dotfiles, in ${case_root}/other-claude/plugins")"

echo "case: --clients, a marketplace with two claude-envoy entries"
clients_case clients-marketplace-twice
printf '{"name": "legion-plugins", "plugins": [{"name": "claude-envoy", "source": {"source": "npm", "package": "@sjawhar/claude-envoy", "version": ">=1.0.0"}}, {"name": "claude-envoy", "source": {"source": "npm", "package": "@sjawhar/claude-envoy", "version": "1.0.0"}}]}\n' \
  >"${case_root}/marketplaces/example-org/plugins.json"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, choosing neither" \
  "$(grep -q '^UNKNOWN: dotfiles .claude/settings.json: example-org/plugins .claude-plugin/marketplace.json serves claude-envoy as .*2 entries name claude-envoy' "$out" && echo true || echo false)"
check "asks npm nothing" "$(lacks "npm view" "$calls")"

for shape in text mixed; do
  echo "case: --clients, npm answers ${shape} where a list of versions belongs"
  clients_case "clients-npm-${shape}"
  if [ "$shape" = text ]; then
    printf 'npm warn config something\n4.0.0\n' >"${case_root}/npm/_sjawhar_claude-envoy___1.0.0"
  else
    printf '["4.0.0", {"version": "1.0.0"}]\n' >"${case_root}/npm/_sjawhar_claude-envoy___1.0.0"
  fi
  run_gate -- --clients "$commit"
  check "exits 3" "$(is "$status" 3)"
  check "refuses UNKNOWN" "$(has_line "UNKNOWN: dotfiles .claude/settings.json: npm answers no version of @sjawhar/claude-envoy for >=1.0.0")"
  check "the verdict is the last line" "$(last_line_is "UNKNOWN")"
done

# npm serves the highest version by semantic-version precedence, where a release ranks above its
# own pre-release; `sort -V` ranks them the other way. Here the release is behind and its
# pre-release ahead, so a gate that took the pre-release would open.
echo "case: --clients, npm matches a release and its own pre-release"
clients_case clients-npm-precedence
printf '["4.0.0-rc.1", "4.0.0"]\n' >"${case_root}/npm/_sjawhar_claude-envoy___1.0.0"
tag "claude-envoy-v4.0.0" "$behind_rev"
tag "claude-envoy-v4.0.0-rc.1" "$ahead_rev"
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "judges the release npm serves" \
  "$(has_line "GATE SHUT: dotfiles .claude/settings.json runs claude-envoy 4.0.0 from claude-envoy-v4.0.0 at ${behind_rev} (behind ${commit} by 12); the version legion-plugins serves for >=1.0.0")"

echo "case: --clients, npm answers one string holding two versions"
clients_case clients-npm-newline
printf '["1.0.0\\n4.0.0"]\n' >"${case_root}/npm/_sjawhar_claude-envoy___1.0.0"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN" "$(has_line "UNKNOWN: dotfiles .claude/settings.json: npm answers no version of @sjawhar/claude-envoy for >=1.0.0")"

echo "case: --clients, a pre-release pin a release tag names"
clients_case clients-pre-release
dotfiles "17.41.0-rc.1"
tag "pi-envoy-v17.41.0-rc.1" "$ahead_rev"
run_gate -- --clients "$commit"
check "exits 0" "$(is "$status" 0)"
check "reads it as the exact version it is" \
  "$(has_line "PASS: dotfiles omp/plugins/package.json runs pi-envoy 17.41.0-rc.1 from pi-envoy-v17.41.0-rc.1 at ${ahead_rev} (ahead of ${commit})")"

echo "case: --clients, a run with no HOME"
clients_case clients-no-home
run_gate --unset HOME -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming HOME" "$(has_line "UNKNOWN: dotfiles: HOME is not set, so this box's ~/.dotfiles cannot be found")"
check "the verdict is the last line" "$(last_line_is "UNKNOWN")"

# A field of the wrong type is never a reason to skip a project, and an image reference with a
# tab in it is never read as the reference before the tab: YAML's false equals 0 in Python, and a
# tab cut the gate's own line apart.
echo "case: a project whose fields are of the wrong type, or whose image is no plain reference"
new_case invalid-fields
legion_stack "    - key: LINUX
      replicas: 1
      daemonImage: ${worker_a}
    - key: BET
      replicas: false
      daemonImage: ${worker_b}
    - key: OSSV
      daemon: \"false\"
      daemonImage: ${worker_b}
    - key: INFRA
      replicas: 1
      daemonImage: \"${worker_a}\tsuffix\""
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$behind_rev"
compare "$ahead_rev" ahead 0
compare "$behind_rev" behind 3
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "a replicas of false is no count, not a skip" "$(has_line "UNKNOWN: BET names replicas false, which is no count of replicas")"
check "a daemon that is a string is no boolean" "$(has_line "UNKNOWN: OSSV names daemon \"false\", which is neither true nor false")"
check "an image with a tab is no reference, and is not read cut short" \
  "$(has_line "UNKNOWN: INFRA names daemonImage \"${worker_a}\\tsuffix\", which is no image reference")"
check "no project is skipped" "$(lacks "SKIPPED" "$out")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

if [ "$failures" -ne 0 ]; then
  echo "${failures} check(s) failed"
  exit 1
fi
echo "all checks passed"
