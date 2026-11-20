# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-read-image-clients.sh
#
# The --clients reader of the images, which daemon-release-gate.sh sources; it is not run on its
# own. It reads the gate's deployment, work, compared, unknown, printable, first_line, last_line,
# fetch, stack_pin (which sets pin and refuses one that is no image reference), its
# compare_revision, and judge_release, which lib/daemon-release-gate-read-developer-box.sh defines.
# shellcheck disable=SC2154 # deployment, work, pin and fetch_error are the gate's

# in_image <ref> <entrypoint> [<arg>...]: the output of <entrypoint> run in a container of <ref> with
# no network, a read-only root, no capability and no gain of privileges, quiet so a pull of <ref>
# adds nothing to its answer, and docker's exit status: 125 when docker could not run the container
# at all. The error, into $work/err, is the last line of docker's or the command's that is not blank
# and not docker's `Run 'docker run --help'` hint. A <ref> not pinned by digest is never run (125):
# under a tag, docker runs whatever image it holds by that name, which need not be the one deployed,
# and the deployment pins every image by digest.
in_image() {
  local status=0
  if ! [[ $1 =~ @sha256:[0-9a-f]+$ ]]; then
    printf '%s is not pinned by digest, so the image docker holds under that name need not be the one deployed\n' "$1" >"$work/err"
    return 125
  fi
  docker run --rm --quiet --network none --read-only --cap-drop ALL --security-opt no-new-privileges \
    --entrypoint "$2" "$1" "${@:3}" </dev/null 2>"$work/run.err" || status=$?
  awk '!/^Run \047/ && NF { line = $0 } END { print line }' "$work/run.err" >"$work/err"
  return "$status"
}

# load_version <ref>: runs <ref>'s /opt/legion/bin/legion version once for the run, into
# version_reads[<ref>]: `ok` and, on the lines after it, what that printed; or `error ` and why.
declare -A version_reads=()
load_version() {
  local text
  [ -z "${version_reads[$1]+read}" ] || return 0
  if text=$(in_image "$1" /opt/legion/bin/legion version); then
    version_reads[$1]="ok"$'\n'"$text"
  else
    version_reads[$1]="error $(first_line "$work/err")"
  fi
}

# read_clients <ref>: prints `commit <revision>`, the commit the image's build ran at, once it has
# found both clients the worker build puts at /opt/legion (packages/daemon/docker/worker.Dockerfile:
# pi-envoy packed from the build's own checkout, claude-envoy built from it), or `error <why>` naming
# what it could not read. The revision is the one the image's own legion prints (`legion version`'s
# first line, `legion <version> commit <revision>`, packages/daemon/cmd/legion/main.go), never a
# release tag a client's manifest names: claude-envoy's manifest keeps its source's static version,
# and pi-envoy's names a release version only when a release build sets it. It reads `legion
# version` from version_reads, which load_version fills.
read_clients() {
  local ref=$1 client text name status answer
  for client in pi-envoy claude-envoy; do
    status=0
    text=$(in_image "$ref" cat "/opt/legion/${client}/package.json") || status=$?
    if [ "$status" = 125 ]; then
      printf 'error cannot run %s: %s\n' "$ref" "$(first_line "$work/err")"
      return
    elif [ "$status" != 0 ]; then
      printf 'error %s carries no @sjawhar/%s at /opt/legion/%s/package.json: %s\n' "$ref" "$client" "$client" "$(first_line "$work/err")"
      return
    fi
    name=$(jq -r '.name // ""' <<<"$text" 2>/dev/null) || name=""
    if [ "$name" != "@sjawhar/${client}" ]; then
      printf "error %s's /opt/legion/%s/package.json names %s, not @sjawhar/%s\n" "$ref" "$client" "$(printable "${name:-no package}")" "$client"
      return
    fi
  done
  answer=${version_reads[$ref]}
  if [ "${answer%%$'\n'*}" != ok ]; then
    printf "error cannot run %s's /opt/legion/bin/legion version: %s\n" "$ref" "${answer#error }"
    return
  fi
  text=$(head -n 1 <<<"${answer#ok$'\n'}")
  if ! [[ $text =~ ^legion\ [^\ ]+\ commit\ ([0-9a-f]{7,40})$ ]]; then
    printf "error %s's /opt/legion/bin/legion version names no build commit: %s\n" "$ref" "$(printable "$text")"
    return
  fi
  printf 'commit %s\n' "${BASH_REMATCH[1]}"
}

# client_image <surface> <ref>: judges the two clients an image carries by the commit its build ran
# at, compared as a pin's revision is. An image several surfaces name is read once.
declare -A client_reads=()
client_image() {
  local surface=$1 ref=$2 answer
  compared=$((compared + 1))
  load_version "$ref"
  [ -n "${client_reads[$ref]+read}" ] || client_reads[$ref]=$(read_clients "$ref")
  answer=${client_reads[$ref]}
  case "$answer" in
    "commit "*) compare_revision "$surface (pi-envoy and claude-envoy)" "${answer#commit }" "${answer#commit }" "" ;;
    *)
      printf 'UNKNOWN: %s: %s\n' "$surface" "${answer#error }"
      unknown=$((unknown + 1))
      ;;
  esac
}

# golden_pi_envoy <file> <path>: the pi_envoy <file>, the project's rendered legion.yaml at <path> in
# the deployment, sets under runtime.kubernetes.plugins, or nothing when it sets no plugins; it
# fails, naming why, on a file that is not one YAML mapping and on plugins that name no pi_envoy
# this gate can read, as the daemon refuses a plugins block that is no mapping of both versions.
golden_pi_envoy() {
  python3 - "$1" "$2" <<'PY'
import json
import sys

import yaml

file, name = sys.argv[1:3]
try:
    with open(file, encoding="utf-8") as handle:
        document = yaml.safe_load(handle)
except UnicodeDecodeError:
    sys.exit(f"{name} is not UTF-8 text")
except yaml.YAMLError as error:
    sys.exit(f"{name} is not YAML: {str(error).splitlines()[0]}")
if not isinstance(document, dict):
    sys.exit(f"{name} is not a mapping")
# A kubernetes project's render always holds runtime.kubernetes, so a golden without it is a read
# that went wrong, not one that sets no plugins.
node = document
for key, field in (("runtime", "runtime"), ("kubernetes", "runtime.kubernetes")):
    node = node.get(key)
    if node is None:
        sys.exit(f"{name} sets no runtime.kubernetes, which every kubernetes project's render holds")
    if not isinstance(node, dict):
        sys.exit(f"{name}'s {field} is not a mapping")
plugins = node.get("plugins")
if plugins is None:
    sys.exit(0)
version = plugins.get("pi_envoy") if isinstance(plugins, dict) else None
if not isinstance(version, str) or not version or not version.isprintable():
    sys.exit(f"{name} sets runtime.kubernetes.plugins to {json.dumps(plugins, default=str)}, which names no pi_envoy this gate can read")
print(version)
PY
}

# client_daemon <surface> <ref> <golden>: judges the pi-envoy the daemon of <ref> installs into a
# kept session pod on a stale-plugin relaunch (packages/daemon/internal/daemon/kubernetes.go,
# ResolvedPlugins): the project's runtime.kubernetes.plugins.pi_envoy when <golden>, its rendered
# legion.yaml, sets one, else the pi-envoy of the pair the image's legion links, which `legion
# version` prints on its second line (`plugins @sjawhar/pi-legion <version> @sjawhar/pi-envoy
# <version>`, packages/daemon/cmd/legion/main.go). That version is judged by the commit of its
# release tag (judge_release), never by the image's build commit. A golden the gate cannot read, a
# legion that links no pair where the golden sets none, and a second line that names no pi-envoy
# version are UNKNOWN.
client_daemon() {
  local label="$1 (the pi-envoy its daemon installs)" ref=$2 golden=$3 version answer second
  compared=$((compared + 1))
  if ! fetch "$golden" "$work/golden.yaml"; then
    printf 'UNKNOWN: %s: %s\n' "$label" "$fetch_error"
    unknown=$((unknown + 1))
    return
  fi
  if ! version=$(golden_pi_envoy "$work/golden.yaml" "$golden" 2>"$work/err"); then
    printf 'UNKNOWN: %s: %s %s\n' "$label" "$deployment" "$(last_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  if [ -n "$version" ]; then
    judge_release "$label" pi-envoy "$version" "; $golden sets it as runtime.kubernetes.plugins.pi_envoy"
    return
  fi
  load_version "$ref"
  answer=${version_reads[$ref]}
  if [ "${answer%%$'\n'*}" != ok ]; then
    printf "UNKNOWN: %s: cannot run %s's /opt/legion/bin/legion version: %s\n" "$label" "$ref" "${answer#error }"
    unknown=$((unknown + 1))
    return
  fi
  second=$(sed -n 2p <<<"${answer#ok$'\n'}")
  if [ -z "$second" ]; then
    printf "UNKNOWN: %s: %s's /opt/legion/bin/legion version links no plugin pair, and %s sets no runtime.kubernetes.plugins\n" "$label" "$ref" "$golden"
    unknown=$((unknown + 1))
    return
  fi
  if ! [[ $second =~ ^plugins\ @sjawhar/pi-legion\ [^\ ]+\ @sjawhar/pi-envoy\ ([0-9A-Za-z.+-]+)$ ]]; then
    printf "UNKNOWN: %s: %s's /opt/legion/bin/legion version prints \"%s\" as its second line, which names no @sjawhar/pi-envoy version\n" "$label" "$ref" "$(printable "$second")"
    unknown=$((unknown + 1))
    return
  fi
  judge_release "$label" pi-envoy "${BASH_REMATCH[1]}" "; the pair its legion links, as $golden sets no runtime.kubernetes.plugins"
}

# sessions_image: the agent-session image the deployment's sessions/ stack pins, sessions/Pulumi.prod.yaml
# `<project>:image` under the name sessions/Pulumi.yaml declares. That image is built FROM the worker
# image, keeps its /opt/legion/pi-envoy, which `omp plugin link` puts in the session's Oh My Pi, and
# its pods deliver to sessions, so it is judged as a project's image is (client_image). A stack or a
# pin the gate cannot read, a pin that is no plain reference among them, is UNKNOWN by name and
# never run (stack_pin).
sessions_image() {
  local key="sessions image"
  stack_pin "$key" sessions image || return 0
  client_image "$key" "$pin"
}
