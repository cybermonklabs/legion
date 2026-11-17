# shellcheck shell=bash
# The preamble the acceptance drivers share: the bearer each run reads, the directory it works in,
# the lines it prints for a check and the CLI it runs. Sourced, never run. Each function that
# refuses prints why on stderr and exits the calling script with 2; one that names its usage calls
# the caller's own `usage`.
#
# The functions below read what the run sets: `token` (read_bearer), `work` (work_dir), and
# `bun_bin`, `saved_path`, `saved_tmpdir` and the `home` and `state` directories under $work
# (cli_env). `cli` needs three more, which each driver sets itself and nothing here sets: the
# `base` URL it talks to, the driver's own `session` and its `cli_entry`.

# require_env NAME…: refuses the first named environment variable that is unset or empty.
require_env() {
  local variable
  for variable in "$@"; do
    if [[ -z "${!variable:-}" ]]; then
      printf 'required environment variable %s is unset\n' "$variable" >&2
      usage
      exit 2
    fi
  done
}

# require_commands COMMAND…: refuses the first named command that is not on PATH.
require_commands() {
  local command
  for command in "$@"; do
    command -v "$command" >/dev/null || {
      printf 'required command %s is unavailable\n' "$command" >&2
      exit 2
    }
  done
}

# read_bearer sets the caller's unexported `token` to the agent bearer, DISPATCH_TOKEN or else
# DISPATCH_TOKEN_FILE's contents trimmed of surrounding whitespace, and refuses an unreadable file
# or an empty bearer. It never prints the bearer, and it unsets both variables, so no child process
# the caller starts inherits the bearer from its environment.
read_bearer() {
  if [[ -n "${DISPATCH_TOKEN:-}" ]]; then
    token="$DISPATCH_TOKEN"
  elif [[ -n "${DISPATCH_TOKEN_FILE:-}" ]]; then
    [[ -r "$DISPATCH_TOKEN_FILE" ]] || {
      printf 'DISPATCH_TOKEN_FILE names %s, which cannot be read\n' "$DISPATCH_TOKEN_FILE" >&2
      exit 2
    }
    token="$(<"$DISPATCH_TOKEN_FILE")"
    token="${token#"${token%%[![:space:]]*}"}"
    token="${token%"${token##*[![:space:]]}"}"
  else
    printf 'set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE to an agent bearer\n' >&2
    usage
    exit 2
  fi
  [[ -n "$token" ]] || {
    printf 'the agent bearer is empty\n' >&2
    exit 2
  }
  unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE
}

# work_dir sets the caller's `work` to a directory of its own, which the caller removes. The umask
# is the run's own, so nothing it writes there is readable by another account.
work_dir() {
  umask 077
  work="$(mktemp -d)"
  readonly work
}

# cli_env saves what `cli` needs from the environment it scrubs — the caller's `bun_bin`,
# `saved_path` and `saved_tmpdir` — and makes the `home` and `state` directories under $work that
# `cli` points the CLI at. A driver that runs the CLI calls it after work_dir.
cli_env() {
  bun_bin="$(command -v bun)"
  readonly bun_bin
  readonly saved_path="$PATH"
  readonly saved_tmpdir="${TMPDIR:-}"
  mkdir "$work/home" "$work/state"
}

pass() { printf 'PASS: %s %s\n' "$1" "$2"; }
fail() {
  printf 'FAIL: %s %s: %s\n' "$1" "$2" "$3" >&2
  exit 1
}
skip() { printf 'SKIP: %s %s\n' "$1" "$2"; }

# The real `dispatch` CLI as an agent session runs it, in an environment holding only what this
# driver names: nothing a calling shell exported (its Dispatch URL, token file or Envoy config)
# reaches it. It reads the caller's $base, $token, $session, $work, $cli_entry and what cli_env
# saved.
cli() {
  (
    set +x
    local name
    local -a exported
    mapfile -t exported < <(compgen -e)
    for name in "${exported[@]}"; do
      unset "$name" 2>/dev/null || true
    done
    export PATH="$saved_path" HOME="$work/home" DISPATCH_STATE_DIR="$work/state"
    if [[ -n "$saved_tmpdir" ]]; then
      export TMPDIR="$saved_tmpdir"
    fi
    export DISPATCH_HOST=omp DISPATCH_SESSION_ID="$session" DISPATCH_URL="$base"
    export DISPATCH_TOKEN="$token"
    exec "$bun_bin" "$cli_entry" "$@"
  )
}

# run SECTION WHAT ARGS...: runs the CLI with ARGS, its output in $work/out; a non-zero exit fails.
run() {
  local section="$1" what="$2"
  shift 2
  cli "$@" >"$work/out" 2>&1 || fail "$section" "$what" "dispatch $1 exited $?: $(<"$work/out")"
}
