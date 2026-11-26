# shellcheck shell=bash
# packages/envoy/scripts/lib/dev-stack-test-docker.sh
# The fake docker scripts/dev-stack.test.sh puts on PATH beside its fake go, psql, curl and
# python3. It is a file of its own because only --project-access runs a container: dev-stack.sh
# runs one, the airlock's stands (lib/airlock-stack.sh), and only under that option. It is
# sourced, never run, and dev-stack.test.sh is its one caller, which calls write_fake_docker once,
# as it writes its other fakes.

# write_fake_docker writes the fake docker into the directory $1, executable. `run -d` writes the
# stands' JSON and their one line where the real container's logs would be, and writes the
# authority the broker reads into the directory the run bind-mounts; `logs` prints them back on
# the two streams the real one keeps apart; `inspect` answers an address of the shape a bridge
# network gives; `rm -f` is the stop. Every call is recorded to DEV_STACK_TEST_DOCKER_CALL_FILE,
# so a run that starts no container shows it made none. No container runs and no port is bound.
write_fake_docker() {
  local bin_dir="$1"
  cat >"${bin_dir}/docker" <<'EOF'
#!/usr/bin/env bash
[[ -z "${DEV_STACK_TEST_DOCKER_CALL_FILE:-}" ]] || printf 'docker %s\n' "$*" >>"$DEV_STACK_TEST_DOCKER_CALL_FILE"
state="${DEV_STACK_TEST_DOCKER_STATE:-${TMPDIR:-/tmp}/fake-docker}"
mkdir -p "$state"
case "${1:-}" in
  run)
    name="" stands=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --name) name="$2"; shift 2 ;;
        -v) case "$2" in *:/stands) stands="${2%%:*}" ;; esac; shift 2 ;;
        *) shift ;;
      esac
    done
    [ -n "$name" ] || { echo "fake docker: run with no --name" >&2; exit 2; }
    mkdir -p "$stands"
    printf 'fake-ca-cert\n' >"$stands/ca.crt"
    printf 'fake-ca-key\n' >"$stands/ca.key"
    printf '{"ca_cert_file":"/stands/ca.crt","ca_key_file":"/stands/ca.key","records_url":"http://127.0.0.1:1/"}\n' >"$state/$name.out"
    printf 'dev-airlock-stands: stands listening records=http://127.0.0.1:1/\n' >"$state/$name.err"
    printf 'fake-container-%s\n' "$name"
    ;;
  logs)
    name="${2:-}"
    [ ! -s "$state/$name.out" ] || cat "$state/$name.out"
    [ ! -s "$state/$name.err" ] || cat "$state/$name.err" >&2
    ;;
  inspect) printf '172.20.0.9\n' ;;
  rm) ;;
  *) echo "fake docker: unexpected command ${1:-}" >&2; exit 2 ;;
esac
EOF
  chmod +x "${bin_dir}/docker"
}
