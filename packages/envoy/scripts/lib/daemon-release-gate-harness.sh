# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-harness.sh
#
# The harness daemon-release-gate.test.sh sources before its scenarios: the temporary directory and
# its fake `gh`, `docker` and `npm`, the constants every case shares, the fixture builders that
# write a case's deployment files, images and developer box, the runners that call the gate, and
# the assertion helpers. It needs `script` (the gate under test) set before it is sourced, and is
# not run on its own.
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
# The deployment repository's default-branch commit, the one the --listeners leg reads its program at.
readonly deployment_head="eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
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

# The fake gh. It takes the endpoint (the argument that starts `repos/`) with any `?ref=` query
# dropped, and answers:
#   repos/<FAKE_DEPLOYMENT>/contents/<path>: for a file, run through the --jq query (`.content`, or
#     `.encoding, .content`), GitHub's answer for it: encoding base64 and the base64 of
#     FAKE_ROOT/files/<path>, or encoding none and no content for a path FAKE_ROOT/encoding-none lists,
#     as GitHub serves a file over 1 MB; for a directory, GitHub's listing of it ({name, type,
#     download_url} for each entry: file, dir or symlink, a file with a download URL, the entries of
#     FAKE_ROOT/listing-extra.json (a JSON list) added, at most 1,000 of them, as GitHub lists) run
#     through the --jq query, failing as a server error when FAKE_LIST_DOWN=1;
#   repos/<FAKE_DEPLOYMENT>/commits/HEAD: FAKE_HEAD;
#   repos/<FAKE_DEPLOYMENT>/git/trees/<sha>: GitHub's recursive tree of FAKE_ROOT/files, each file and
#     link a blob named by git's own hash of it, with the entries of FAKE_ROOT/tree-extra.json (a JSON
#     list) added, `truncated` when FAKE_ROOT/tree-truncated exists;
#   repos/<FAKE_DEPLOYMENT>/tarball/<sha>: a gzipped tar of FAKE_ROOT/files under one top directory,
#     as GitHub's archive holds a repository, its links' targets as written (FAKE_ROOT/archive.tgz
#     instead when it exists, and bytes that are no archive when FAKE_ROOT/corrupt-archive exists);
#     the paths FAKE_ROOT/archive-omit lists, one a line, left out of it, and the files under
#     FAKE_ROOT/archive-subst/ put in place of their namesakes, as export-ignore and export-subst do;
#   repos/cybermonklabs/legion/compare/<base>...<head>: FAKE_ROOT/compare/<head>, failing as an
#     unreachable API when FAKE_API_DOWN=1;
#   repos/cybermonklabs/legion/commits/refs/tags/<tag>: FAKE_ROOT/tags/<tag>;
#   repos/<owner>/<repo>/contents/.claude-plugin/marketplace.json, read raw:
#     FAKE_ROOT/marketplaces/<owner>/<repo>.json; any other path of such a repository, read raw:
#     FAKE_ROOT/marketplaces/<owner>/<repo>/<path>.
# An endpoint with a file at FAKE_ROOT/refuse/<endpoint with / as _> fails with that file's text.
# Any other call is refused as unexpected.
cat >"${temporary_dir}/bin/gh" <<'EOF'
#!/usr/bin/env bash
printf 'gh %s\n' "$*" >>"${FAKE_CALLS:?}"
[ "$1" = api ] || { echo "fake gh: unexpected command: $*" >&2; exit 2; }
shift
endpoint=""
query=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -H) shift 2 ;;
    --jq) query=$2 && shift 2 ;;
    repos/*) endpoint=$1 && shift ;;
    *) echo "fake gh: unexpected argument: $1" >&2; exit 2 ;;
  esac
done
[ -n "$endpoint" ] || { echo "fake gh: no endpoint" >&2; exit 2; }
path=${endpoint%%\?*}
refusal="${FAKE_ROOT:?}/refuse/$(printf '%s' "$path" | tr '/' '_')"
[ ! -f "$refusal" ] || { cat "$refusal" >&2; exit 1; }
case "$path" in
  repos/"${FAKE_DEPLOYMENT:?}"/contents/*)
    file="${FAKE_ROOT}/files/${path#repos/"${FAKE_DEPLOYMENT}"/contents/}"
    if [ -d "$file" ]; then
      [ "${FAKE_LIST_DOWN:-0}" != 1 ] || { echo "gh: Server Error (HTTP 500)" >&2; exit 1; }
      extra="${FAKE_ROOT}/listing-extra.json"
      [ -f "$extra" ] || extra=/dev/null
      {
        find "$file" -mindepth 1 -maxdepth 1 -printf '%f\t%y\n' | sort |
          jq -R -s 'split("\n") | map(select(length > 0) | split("\t") | {name: .[0], type: ({"f": "file", "d": "dir", "l": "symlink"}[.[1]] // "file")} | .download_url = (if .type == "file" then "https://raw.example/" + .name else null end))'
        cat "$extra"
      } | jq -s 'add // [] | .[0:1000]' | jq -r "$query"
      exit 0
    fi
    [ -f "$file" ] || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
    encoding=base64
    if [ -f "${FAKE_ROOT}/encoding-none" ] && grep -qxF -- "${path#repos/"${FAKE_DEPLOYMENT}"/contents/}" "${FAKE_ROOT}/encoding-none"; then
      encoding=none
    fi
    case "$query" in
      .content) ;;
      ".encoding, .content") echo "$encoding" ;;
      *) echo "fake gh: unexpected query for a file: $query" >&2; exit 2 ;;
    esac
    if [ "$encoding" = base64 ]; then base64 -w 76 <"$file"; else echo; fi
    ;;
  repos/"${FAKE_DEPLOYMENT}"/commits/HEAD) echo "${FAKE_HEAD:?}" ;;
  repos/"${FAKE_DEPLOYMENT}"/git/trees/*)
    python3 - "${FAKE_ROOT}/files" "${FAKE_ROOT}/tree-extra.json" "${FAKE_ROOT}/tree-truncated" "${path##*/}" <<'PY'
import hashlib
import json
import os
import sys

root, extra, truncated, sha = sys.argv[1:5]


def blob(data):
    return hashlib.sha1(b"blob %d\0" % len(data) + data).hexdigest()


entries = []
for directory, dirs, files in os.walk(root):
    for name in sorted(dirs + files):
        path = os.path.join(directory, name)
        relative = os.path.relpath(path, root)
        if os.path.islink(path):
            entries.append({"path": relative, "mode": "120000", "type": "blob", "sha": blob(os.fsencode(os.readlink(path)))})
        elif os.path.isfile(path):
            with open(path, "rb") as handle:
                entries.append({"path": relative, "mode": "100644", "type": "blob", "sha": blob(handle.read())})
        elif os.path.isdir(path):
            entries.append({"path": relative, "mode": "040000", "type": "tree", "sha": "0" * 40})
if os.path.exists(extra):
    with open(extra) as handle:
        entries += json.load(handle)
print(json.dumps({"sha": sha, "tree": entries, "truncated": os.path.exists(truncated)}))
PY
    ;;
  repos/"${FAKE_DEPLOYMENT}"/tarball/*)
    top="$(printf '%s' "$FAKE_DEPLOYMENT" | tr '/' '-')-${path##*/}"
    if [ -e "${FAKE_ROOT}/corrupt-archive" ]; then
      echo "this is no archive"
    elif [ -f "${FAKE_ROOT}/archive.tgz" ]; then
      cat "${FAKE_ROOT}/archive.tgz"
    elif [ -f "${FAKE_ROOT}/archive-omit" ] || [ -d "${FAKE_ROOT}/archive-subst" ]; then
      served="${FAKE_ROOT}/archive-served"
      rm -rf "$served" && cp -a "${FAKE_ROOT}/files" "$served"
      if [ -f "${FAKE_ROOT}/archive-omit" ]; then
        while IFS= read -r omitted; do rm -rf "${served:?}/${omitted}"; done <"${FAKE_ROOT}/archive-omit"
      fi
      [ ! -d "${FAKE_ROOT}/archive-subst" ] || cp -a "${FAKE_ROOT}/archive-subst/." "$served/"
      tar -czf - --sort=name -C "$served" --transform "s,^\\.,${top},S" .
    else
      tar -czf - --sort=name -C "${FAKE_ROOT}/files" --transform "s,^\\.,${top},S" .
    fi
    ;;
  repos/cybermonklabs/legion/compare/*)
    [ "${FAKE_API_DOWN:-0}" != 1 ] || { echo "error connecting to api.github.com" >&2; exit 1; }
    head="${path##*...}"
    [ -f "${FAKE_ROOT}/compare/${head}" ] || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
    cat "${FAKE_ROOT}/compare/${head}"
    ;;
  repos/cybermonklabs/legion/commits/refs/tags/*)
    tag=${path#repos/cybermonklabs/legion/commits/refs/tags/}
    [ -f "${FAKE_ROOT}/tags/${tag}" ] || { echo "gh: No commit found for SHA: refs/tags/${tag} (HTTP 422)" >&2; exit 1; }
    cat "${FAKE_ROOT}/tags/${tag}"
    ;;
  repos/*/contents/.claude-plugin/marketplace.json)
    repo=${path#repos/}
    repo=${repo%/contents/*}
    [ -f "${FAKE_ROOT}/marketplaces/${repo}.json" ] || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
    cat "${FAKE_ROOT}/marketplaces/${repo}.json"
    ;;
  repos/*/contents/*)
    repo=${path#repos/}
    file="${FAKE_ROOT}/marketplaces/${repo%%/contents/*}/${repo#*/contents/}"
    [ -f "$file" ] || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
    cat "$file"
    ;;
  *) echo "fake gh: unexpected api call: $endpoint" >&2; exit 2 ;;
esac
EOF
# The fake docker. A ref's answers are files under FAKE_ROOT/images/<ref with / : @ as _>:
# mediatype, labels (a single image's) and platform-<os>_<arch> (an index's, one per platform),
# and platforms, the index's platform list as the range format prints it; and, for `run`, fs/<path>
# for `--entrypoint cat <path>` and legion-version for `--entrypoint /opt/legion/bin/legion version`.
# A ref with no fs/ is one the registry does not hold, and docker answers `run` as it does then.
# `run` takes the options the gate runs an image under (--rm, --quiet, --network, --read-only,
# --cap-drop, --security-opt) and refuses any other.
cat >"${temporary_dir}/bin/docker" <<'EOF'
#!/usr/bin/env bash
printf 'docker %s\n' "$*" >>"${FAKE_CALLS:?}"
if [ "$1" = run ]; then
  shift
  entrypoint=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --rm | --quiet | --read-only) shift ;;
      --network | --cap-drop | --security-opt) shift 2 ;;
      --entrypoint) entrypoint=$2 && shift 2 ;;
      -*) echo "fake docker: unexpected run option: $1" >&2; exit 2 ;;
      *) break ;;
    esac
  done
  ref=$1
  shift
  dir="${FAKE_ROOT:?}/images/$(printf '%s' "$ref" | tr '/:@' '___')"
  if [ ! -d "$dir/fs" ]; then
    echo "docker: Error response from daemon: manifest for $ref not found: manifest unknown" >&2
    printf '\nRun '"'"'docker run --help'"'"' for more information\n' >&2
    exit 125
  fi
  case "$entrypoint $*" in
    "cat "*)
      [ -f "$dir/fs$1" ] || { echo "cat: $1: No such file or directory" >&2; exit 1; }
      cat "$dir/fs$1"
      ;;
    "/opt/legion/bin/legion version")
      if [ ! -f "$dir/legion-version" ]; then
        echo "docker: Error response from daemon: failed to create task for container: exec: \"/opt/legion/bin/legion\": stat /opt/legion/bin/legion: no such file or directory: unknown" >&2
        printf '\nRun '"'"'docker run --help'"'"' for more information\n' >&2
        exit 127
      fi
      cat "$dir/legion-version"
      ;;
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
# The fake npm. `npm pack <spec> --dry-run [--ignore-scripts] --json` answers as npm 10.9.7's does:
# a list of the one package it packed, with the fields npm prints (id, name, version, size,
# unpackedSize, shasum, integrity, filename, files, entryCount, bundled), for the version
# FAKE_ROOT/npm/<spec with / @ < > = as _> names, which is the version npm's own pick takes for the
# spec under the npm configuration the case sets; that file with `.raw` added is answered as it is,
# as an npm that answers garbage would. With `--registry=<url>`, each is read from
# FAKE_ROOT/npm-registries/<url with / and : as _>/ instead. It fails as npm does (E404) when no
# file names a version, and as an unreachable registry when FAKE_NPM_DOWN=1. `npm exec` (the gate's
# parse of the marketplace's spec with the npm-package-arg npm ships), and `npm pack` when
# FAKE_NPM_REAL_PACK=1, run the real npm the harness found when it started, so a case sees what
# the real npm does with a spec. It records each call, the directory it runs in as an `npm-cwd
# <dir>` line, and each npm_config_ variable it runs with as an `npm-config <name>=<value>` line,
# so a case sees where the gate runs npm and the configuration it leaves npm; it refuses any other
# command, and a pack without --dry-run, which would write the tarball.
cat >"${temporary_dir}/bin/npm" <<'EOF'
#!/usr/bin/env bash
printf 'npm %s\n' "$*" >>"${FAKE_CALLS:?}"
printf 'npm-cwd %s\n' "$PWD" >>"$FAKE_CALLS"
env | grep '^npm_config_' | sort | sed 's/^/npm-config /' >>"$FAKE_CALLS"
if [ "$1" = exec ] || { [ "$1" = pack ] && [ "${FAKE_NPM_REAL_PACK:-0}" = 1 ]; }; then
  mapfile -t real_npm <"$(dirname "$0")/../real-npm"
  exec "${real_npm[0]}" "${real_npm[1]}" "$@"
fi
registry=""
words=()
for word in "$@"; do
  case "$word" in
    --registry=*) registry=${word#--registry=} ;;
    *) words+=("$word") ;;
  esac
done
set -- "${words[@]}"
case "$1 $#:${*:3}" in
  "pack 4:--dry-run --json" | "pack 5:--dry-run --ignore-scripts --json") ;;
  *) echo "fake npm: unexpected command: $*" >&2; exit 2 ;;
esac
[ "${FAKE_NPM_DOWN:-0}" != 1 ] || { echo "npm error code ENOTFOUND" >&2; exit 1; }
dir="${FAKE_ROOT:?}/npm"
[ -z "$registry" ] || dir="${FAKE_ROOT}/npm-registries/$(printf '%s' "$registry" | tr '/:' '__')"
file="${dir}/$(printf '%s' "$2" | tr '/@<>=' '_____')"
[ ! -f "${file}.raw" ] || { cat "${file}.raw"; exit 0; }
[ -f "$file" ] || { echo "npm error code E404" >&2; exit 1; }
name=${2%@*}
jq -n --arg name "$name" --arg version "$(tr -d '\n' <"$file")" '[{
  id: "\($name)@\($version)", name: $name, version: $version, size: 1, unpackedSize: 1, shasum: "",
  integrity: "", filename: "\($name | ltrimstr("@") | sub("/"; "-"))-\($version).tgz", files: [],
  entryCount: 0, bundled: []
}]'
EOF
chmod +x "${temporary_dir}/bin/gh" "${temporary_dir}/bin/docker" "${temporary_dir}/bin/npm"
# The real npm the fake npm runs for `npm exec` and for a case's real `npm pack`: the node npm runs
# on and npm's own npm-cli.js, as npm exec gives a command them (npm_node_execpath, npm_execpath),
# found once with the caller's own HOME and written beside the fakes. The suite needs npm and node.
# shellcheck disable=SC2016 # npm exec's shell expands them, with the values npm gives it
if ! (cd "$temporary_dir" && npm exec --offline --yes=false -c 'printf "%s\n%s\n" "$npm_node_execpath" "$npm_execpath"') \
  >"${temporary_dir}/real-npm" 2>"${temporary_dir}/real-npm.err" || [ "$(grep -c . "${temporary_dir}/real-npm")" -ne 2 ]; then
  echo "daemon-release-gate.test.sh needs npm and node on PATH: $(head -n 1 "${temporary_dir}/real-npm.err")" >&2
  exit 1
fi

failures=0
case_root=""
out="${temporary_dir}/out"
err="${temporary_dir}/err"
calls="${temporary_dir}/calls"
status=0
# The gate each run calls: the one under test, or a control's copy of it (mutant).
gate_under_test="$script"

# new_case starts a scenario of its own: no stack file, no image, no comparison, no release tag and
# an empty HOME yet, each stack's Pulumi.yaml declaring its default project name, and an envoy/
# program that declares the VM hub, as every deployment did before the hub's removal.
new_case() {
  case_root="${temporary_dir}/case-$1"
  mkdir -p "${case_root}/files/legion" "${case_root}/files/envoy" "${case_root}/files/envoy-cluster" \
    "${case_root}/files/dispatch" "${case_root}/images" "${case_root}/compare" "${case_root}/tags" \
    "${case_root}/refuse" "${case_root}/marketplaces" "${case_root}/npm" "${case_root}/home"
  project legion "$legion_project"
  project envoy "$envoy_project"
  project envoy-cluster "$cluster_project"
  project dispatch "$dispatch_project"
  envoy_program declares
}
# project <stack> <name>: <stack>/Pulumi.yaml, declaring <name> as its project's name.
project() {
  printf 'name: %s\nruntime: go\n' "$2" >"${case_root}/files/$1/Pulumi.yaml"
}
# compare <revision> <status> <behind_by>: what GitHub answers for <commit>...<revision>.
compare() {
  printf '{"status":"%s","behind_by":%s}\n' "$2" "$3" >"${case_root}/compare/$1"
}
# refuse <endpoint> <answer>: GitHub answers <endpoint> (no query) with the error <answer>.
refuse() {
  printf '%s\n' "$2" >"${case_root}/refuse/$(printf '%s' "$1" | tr '/' '_')"
}
# tag <tag> <commit>: the commit cybermonklabs/legion's release tag <tag> names.
tag() {
  printf '%s\n' "$2" >"${case_root}/tags/$1"
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
# worker_image <ref> <build commit> [no-pi-envoy|no-claude-envoy|no-commit|no-plugins]: a worker
# image's files as `docker run` reads them: the two clients the build puts at /opt/legion, each
# manifest at the version a build outside a release leaves it (pi-envoy at the source's 16.12.1,
# claude-envoy at the source's static 1.0.0), and a legion whose `version` names <build commit> on
# its first line and, on its second, the plugin pair a release build links into it (pi-envoy
# 17.42.0, linked_envoy). The option leaves that client out, makes `legion version` name no
# commit, or leaves its second line out, as a build that links no pair prints.
readonly linked_envoy="17.42.0"
worker_image() {
  local dir
  dir=$(image_dir "$1")
  rm -rf "$dir/fs"
  mkdir -p "$dir/fs/opt/legion/pi-envoy" "$dir/fs/opt/legion/claude-envoy"
  printf '{"name": "@sjawhar/pi-envoy", "version": "16.12.1"}\n' >"$dir/fs/opt/legion/pi-envoy/package.json"
  printf '{"name": "@sjawhar/claude-envoy", "version": "1.0.0"}\n' >"$dir/fs/opt/legion/claude-envoy/package.json"
  printf 'legion (devel) commit %s\nplugins @sjawhar/pi-legion 19.28.1 @sjawhar/pi-envoy %s\n' "$2" "$linked_envoy" >"$dir/legion-version"
  case "${3:-}" in
    no-pi-envoy) rm "$dir/fs/opt/legion/pi-envoy/package.json" ;;
    no-claude-envoy) rm "$dir/fs/opt/legion/claude-envoy/package.json" ;;
    no-commit) printf 'legion (devel)\n' >"$dir/legion-version" ;;
    no-plugins) printf 'legion (devel) commit %s\n' "$2" >"$dir/legion-version" ;;
  esac
}
# plugins_line <ref> <line>: the second line <ref>'s `legion version` prints, in place of its own.
plugins_line() {
  local file
  file="$(image_dir "$1")/legion-version"
  printf '%s\n%s\n' "$(head -n 1 "$file")" "$2" >"$file"
}
# dotfiles <pi-envoy pin> [<claude-envoy enabled>]: the developer box's ~/.dotfiles, with
# ~/.omp/plugins linked to its plugin tree, the pin installed there, and a settings.json whose
# marketplace legion-plugins is example-org/plugins, serving @sjawhar/claude-envoy at >=1.0.0, for
# which npm's pick takes 4.0.0.
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
  printf '4.0.0\n' >"${case_root}/npm/_sjawhar_claude-envoy___1.0.0"
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
# envoy_program <declares|none>: the deployment's envoy/ program and the local module its go.mod
# replaces, ../shared. Either way the name NewNatsHub stands in every place that is not a use: a
# func's own name and a method's, a comment, a string and a raw string, a _test.go, and a comment
# in testdata/ and in another program of the repository; and the hub's type token stands where the
# generated SDK registers it, inside the body of its own NewNatsHub, and in the SDK's switch of the
# types it rebuilds, neither of which declares a hub; with declares, main.go calls it.
envoy_program() {
  local dir="${case_root}/files/envoy"
  mkdir -p "$dir/sdks/gcp" "$dir/testdata" "${case_root}/files/shared" "${case_root}/files/envoy-cluster"
  printf 'module example.org/deployment/envoy\n\nreplace example.org/sdk => ./sdks/gcp // the generated SDK\n\nreplace (\n\texample.org/shared => ../shared\n\texample.org/nats v1.0.0 => example.org/nats v1.0.1\n)\n' >"$dir/go.mod"
  cat >"$dir/sdks/gcp/natsHub.go" <<'GO'
package gcp

// NewNatsHub registers a hub.
func NewNatsHub(ctx *pulumi.Context,
	name string, args *NatsHubArgs, opts ...pulumi.ResourceOption) (*NatsHub, error) {
	if args == nil {
		args = &NatsHubArgs{}
	}
	var resource NatsHub
	err := ctx.RegisterRemoteComponentResource("envoy-infra:gcp:NatsHub", name, args, &resource, opts...)
	return &resource, err
}

func (m *module) NewNatsHub(name string) {}
GO
  cat >"$dir/sdks/gcp/init.go" <<'GO'
package gcp

func (m *module) Construct(ctx *pulumi.Context, name, typ, urn string) (r pulumi.Resource, err error) {
	switch typ {
	case "envoy-infra:gcp:NatsHub":
		r = &NatsHub{}
	}
	return
}
GO
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
# listener_stacks <envoy> <envoy-cluster> <dispatch envoyImage> [<dispatch listenerImage>]: the three
# stack files; an empty <envoy> leaves envoy's listenerImage out, keeping its other keys.
listener_stacks() {
  {
    printf 'config:\n'
    [ -z "$1" ] || printf '  %s:listenerImage: %s\n' "$envoy_project" "$1"
    printf '  %s:hubZone: us-west1-a\n' "$envoy_project"
  } >"${case_root}/files/envoy/Pulumi.prod.yaml"
  printf 'config:\n  %s:envoyImage: %s\n' "$cluster_project" "$2" >"${case_root}/files/envoy-cluster/Pulumi.prod.yaml"
  {
    printf 'config:\n  %s:envoyImage: %s\n' "$dispatch_project" "$3"
    [ -z "${4:-}" ] || printf '  %s:listenerImage: %s\n' "$dispatch_project" "$4"
  } >"${case_root}/files/dispatch/Pulumi.prod.yaml"
}

# run_bare [--unset <variable>]... [env assignments...] -- [gate arguments...]: runs the gate in
# the current case with exactly the arguments given, HOME the case's own and no CLAUDE_CONFIG_DIR
# of the machine running this test unless an assignment gives one, each --unset variable taken out
# of its environment last; sets $status.
run_bare() {
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
    FAKE_ROOT="$case_root" FAKE_DEPLOYMENT="$deployment" FAKE_HEAD="$deployment_head" "${assignments[@]}" \
    env "${unsets[@]}" bash "$gate_under_test" "$@" >"$out" 2>"$err" || status=$?
}
# run_gate [--unset <variable>]... [env assignments...] -- [gate arguments...]: run_bare with
# `--deployment <deployment>` before the gate arguments, as an operator runs it.
run_gate() {
  local assignments=()
  while [ "$#" -gt 0 ] && [ "$1" != -- ]; do assignments+=("$1"); shift; done
  [ "$#" -eq 0 ] || shift
  run_bare "${assignments[@]}" -- --deployment "$deployment" "$@"
}

# mutant <function> <name>: writes a copy of the gate and the readers it sources from lib/
# (daemon-release-gate-read-*.sh and legion-deployment-projects.sh) under
# ${temporary_dir}/mutant-<name>/, in which the one file that
# defines <function> as `<function>() {` ... `}` holds the definition read from stdin instead, and
# sets mutant_gate to the copy's gate. When no file, or more than one, defines <function>, it fails
# a row naming that and sets mutant_gate to a path that does not exist, so a control never runs
# against a copy that is the gate unchanged, and its own row fails too.
mutant_gate=""
mutant() {
  local dir="${temporary_dir}/mutant-$2" body="${temporary_dir}/mutant-$2.body" file found=0
  cat >"$body"
  rm -rf "$dir"
  mkdir -p "$dir/lib"
  cp "$script" "$dir/daemon-release-gate.sh"
  cp "$(dirname "$script")"/lib/daemon-release-gate-read-*.sh "$(dirname "$script")"/lib/legion-deployment-projects.sh "$dir/lib/" 2>/dev/null || true
  for file in "$dir/daemon-release-gate.sh" "$dir"/lib/*.sh; do
    if [ ! -f "$file" ] || ! grep -qxF "$1() {" "$file"; then continue; fi
    found=$((found + 1))
    awk -v name="$1" -v body="$body" '
      $0 == name "() {" { while ((getline line < body) > 0) print line; skipping = 1; next }
      skipping { if ($0 == "}") skipping = 0; next }
      { print }' "$file" >"$file.new"
    mv "$file.new" "$file"
  done
  mutant_gate="$dir/daemon-release-gate.sh"
  if [ "$found" != 1 ]; then
    check "the $2 control replaces the one $1() the gate defines (it defines ${found})" false
    mutant_gate="$dir/no-such-gate.sh"
  fi
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
