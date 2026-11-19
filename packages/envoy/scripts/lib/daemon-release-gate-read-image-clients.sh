# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-read-image-clients.sh
#
# The --clients reader of the worker images, which daemon-release-gate.sh sources; it is not run on
# its own. It reads the gate's work, compared, unknown and first_line, and its compare_revision.
# shellcheck disable=SC2154 # work is the gate's

# in_image <ref> <entrypoint> [<arg>...]: the output of <entrypoint> run in a container of <ref> with
# no network, quiet so a pull of <ref> adds nothing to its answer, and docker's exit status: 125
# when docker could not run the container at all. The error, into $work/err, is the last line of
# docker's or the command's that is not blank and not docker's `Run 'docker run --help'` hint.
in_image() {
  local status=0
  docker run --rm --quiet --network none --entrypoint "$2" "$1" "${@:3}" </dev/null 2>"$work/run.err" || status=$?
  awk '!/^Run \047/ && NF { line = $0 } END { print line }' "$work/run.err" >"$work/err"
  return "$status"
}

# read_clients <ref>: prints `commit <revision>`, the commit the image's build ran at, once it has
# found both clients the worker build puts at /opt/legion (packages/daemon/docker/worker.Dockerfile:
# pi-envoy packed from the build's own checkout, claude-envoy built from it), or `error <why>` naming
# what it could not read. The revision is the one the image's own legion prints (`legion version`'s
# first line, `legion <version> commit <revision>`, packages/daemon/cmd/legion/main.go), never a
# release tag a client's manifest names: claude-envoy's manifest keeps its source's static version,
# and pi-envoy's names a release version only when a release build sets it.
read_clients() {
  local ref=$1 client text name status
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
      printf "error %s's /opt/legion/%s/package.json names %s, not @sjawhar/%s\n" "$ref" "$client" "${name:-no package}" "$client"
      return
    fi
  done
  if ! text=$(in_image "$ref" /opt/legion/bin/legion version); then
    printf "error cannot run %s's /opt/legion/bin/legion version: %s\n" "$ref" "$(first_line "$work/err")"
    return
  fi
  text=$(head -n 1 <<<"$text")
  if ! [[ $text =~ ^legion\ [^\ ]+\ commit\ ([0-9a-f]{7,40})$ ]]; then
    printf "error %s's /opt/legion/bin/legion version names no build commit: %s\n" "$ref" "$text"
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
