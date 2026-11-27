# shellcheck shell=bash disable=SC2034,SC2154
# Sourced by autodeploy_test.sh, after its fixture and before its rollback scenarios: how the
# deployer reads and writes compose/.env - through Compose alone, refusing by name on its standard
# output, its static check and behavioural row, the staging writer, a stop during the poll, and
# Compose 2.12.2. It shares autodeploy_test.sh's variables, so the unused (SC2034) and unassigned
# (SC2154) warnings of shellcheck are off here.
# A .env with neither of Dispatch's identity settings is refused before anything of the deploy
# runs: Dispatch's container refuses to start without one and restart-loops, and so would the
# rollback's, under the same compose file. No backup is taken, compose is not run, and the previous
# image still serves.
fresh_env
sed -i '/^ENVOY_ACCOUNT_GOOGLE_AUDIENCE=/d' "$compose/.env"
if output=$(deploy_once); then
  fail 'deployer deployed with no identity for Dispatch in .env'
fi
if [[ "$output" != *"neither ENVOY_ACCOUNT_GOOGLE_AUDIENCE nor ENVOY_ACCOUNT_TOKEN_FILE"*"not deploying new-image"* ]]; then
  fail 'deployer did not name both identity settings when it refused'
fi
if [[ -s "$log" ]] || grep -q '^pg_dump ' "$argv" ||
  [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer backed up or ran compose with no identity for Dispatch in .env'
fi

# The four settings the deployer checks before its backup are read from Compose's own resolution
# (`docker compose config`, the real binary on the real compose file). For each, as the setting the
# check depends on alone (the other identity setting taken out first, `drop`): the spellings Compose
# resolves empty, `NAME=""` and `NAME="${AUTODEPLOY_TEST_UNSET:-}"`, are refused before anything of
# the deploy runs, naming the setting; the spellings Compose resolves to the value, `export NAME=v`,
# `NAME=v # a note` and `NAME=${AUTODEPLOY_TEST_VALUE}` with that variable in the deployer's
# environment, go on to the backup.
with_line() {
  grep -v "^$1=" "$compose/.env" >"$compose/.env.new"
  printf '%s\n' "$2" >>"$compose/.env.new"
  mv "$compose/.env.new" "$compose/.env"
}
identity_refusal='neither ENVOY_ACCOUNT_GOOGLE_AUDIENCE nor ENVOY_ACCOUNT_TOKEN_FILE'
# The database URL fresh_env writes, read back rather than spelled again.
fresh_env
database_url=$(sed -n 's/^DATABASE_URL=//p' "$compose/.env")
[[ -n "$database_url" ]] || fail 'fresh_env wrote no DATABASE_URL'
for setting in \
  "ENVOY_ACCOUNT_GOOGLE_AUDIENCE|envoy-listener|$identity_refusal|-" \
  "ENVOY_ACCOUNT_TOKEN_FILE|/srv/dispatch/token|$identity_refusal|ENVOY_ACCOUNT_GOOGLE_AUDIENCE" \
  "DISPATCH_SERVER_URL|http://browser.test:8766|DISPATCH_SERVER_URL|-" \
  "DATABASE_URL|$database_url|DATABASE_URL|-"; do
  IFS='|' read -r name value refusal drop <<<"$setting"
  for spelling in "$name=\"\"" "$name=\"\${AUTODEPLOY_TEST_UNSET:-}\"" "export $name=$value" \
    "$name=$value # a note" "$name=\${AUTODEPLOY_TEST_VALUE}"; do
    fresh_env
    [[ "$drop" == - ]] || sed -i "/^$drop=/d" "$compose/.env"
    with_line "$name" "$spelling"
    case "$spelling" in
      *=\"\" | *UNSET*)
        if output=$(deploy_once); then
          fail "deployer deployed with $spelling, which Compose resolves empty"
        fi
        no_secret "the refusal of $name resolved empty"
        if [[ "$output" != *"$refusal"*"not deploying new-image"* ]]; then
          fail "deployer did not refuse $spelling, which Compose resolves empty, naming $name"
        fi
        if [[ -s "$log" ]] || grep -q '^pg_dump ' "$argv"; then
          fail "deployer backed up or ran compose with $spelling, which Compose resolves empty"
        fi
        ;;
      *)
        output=$(AUTODEPLOY_TEST_VALUE="$value" deploy_once) || true
        if [[ "$output" == *"$refusal"*"not deploying"* ]] || ! grep -q '^pg_dump ' "$argv"; then
          fail "deployer did not go on to its backup with $spelling, which Compose resolves to $value"
        fi
        ;;
    esac
  done
done

# A required variable missing a value (here NATS_URLS, `${NATS_URLS:?...}` in the compose file) is
# Compose's own refusal: the deployer refuses before its backup with Compose's first line. And a
# DATABASE_URL Compose quotes back in an error (Compose's message about a value with the password in
# it) keeps the password out of the log: an unclosed quote in the .env makes Compose name the line.
fresh_env
sed -i '/^NATS_URLS=/d' "$compose/.env"
if output=$(deploy_once); then
  fail 'deployer deployed with NATS_URLS missing, which Compose refuses'
fi
if [[ "$output" != *"Compose refused"*"NATS_URLS"*"not deploying new-image"* ]] ||
  [[ -s "$log" ]] || grep -q '^pg_dump ' "$argv"; then
  fail 'deployer did not refuse a configuration Compose refuses, by Compose'"'"'s own line, before its backup'
fi
fresh_env
with_line DATABASE_URL "DATABASE_URL=\"postgres://dispatch:$password@db.test:5432/dispatch"
if output=$(deploy_once); then
  fail 'deployer deployed with a DATABASE_URL Compose cannot read'
fi
no_secret 'Compose'"'"'s refusal of a DATABASE_URL it cannot read'
[[ "$output" == *"not deploying new-image"* ]] || fail 'deployer did not refuse a DATABASE_URL Compose cannot read'

# A configuration Compose refuses is logged by no text of Compose's: its output can begin with a
# warning, and can quote a .env line whole. With a `${...}` warning first and NATS_URLS missing, the
# refusal names NATS_URLS and holds no warning; an unterminated quote around a password with a space
# in it, or around the agent token, logs neither value, and names the command that shows Compose's
# message. The python3 step and Compose's timeout each name their cause.
fresh_env
sed -i '/^NATS_URLS=/d' "$compose/.env"
with_line DISPATCH_PORT "DISPATCH_PORT=\${AUTODEPLOY_TEST_UNSET}"
if output=$(deploy_once); then
  fail 'deployer deployed with NATS_URLS missing behind a warning'
fi
[[ "$output" == *"a required variable is missing a value: NATS_URLS; not deploying new-image"* && "$output" != *level=warning* && "$output" != *AUTODEPLOY_TEST_UNSET* ]] ||
  fail 'deployer did not name NATS_URLS alone when Compose warned first'
for line in "DATABASE_URL=\"postgres://dispatch@db.test:5432/dispatch?password='se cret'" 'DISPATCH_AGENT_TOKEN="agent-token-CANARY-1234'; do
  fresh_env
  with_line "${line%%=*}" "$line"
  if output=$(deploy_once); then
    fail "deployer deployed with an unterminated quote: ${line%%=*}"
  fi
  if [[ "$output" == *cret* || "$output" == *CANARY* ]] ||
    [[ "$output" != *"docker compose config exited "[0-9]*"; its message is not shown"*"docker compose -p dispatch-test -f $compose/dispatch.compose.yml config"* ]]; then
    fail "deployer logged a value from Compose's refusal of ${line%%=*}, or no command to read it"
  fi
done
fresh_env
if output=$(AUTODEPLOY_TEST_PYTHON_FAIL=1 deploy_once); then
  fail 'deployer deployed when python3 could not read the configuration'
fi
[[ "$output" == *"from docker compose config: python3 exited 3; could not read the deployed ENVOY_IMAGE_TAG; not deploying new-image"* && ! -s "$log" ]] ||
  fail 'deployer did not name the python3 failure'
fresh_env
if output=$(AUTODEPLOY_TEST_CONFIG_TIMEOUT=1 deploy_once); then
  fail 'deployer deployed when docker compose config timed out'
fi
[[ "$output" == *"docker compose config did not answer within 60 s; run \`docker compose -p dispatch-test -f $compose/dispatch.compose.yml config\` to see why; could not read the deployed ENVOY_IMAGE_TAG; not deploying new-image"* && ! -s "$log" ]] ||
  fail 'deployer did not name the config timeout'

# A read of the deployed tag that fails is refused by name, before anything of the deploy runs, and
# never taken for an env file that names no tag. On `d8760c42c9`, whose read swallowed the failure,
# the deployer reported "Compose resolves no ENVOY_IMAGE_TAG" instead.
fresh_env
if output=$(AUTODEPLOY_TEST_ENV_READ_FAIL=1 deploy_once); then
  fail 'deployer deployed when it could not read the deployed tag'
fi
if [[ "$output" != *"docker compose config exited 1; its message is not shown"*"could not read the deployed ENVOY_IMAGE_TAG; not deploying new-image"* ]] ||
  [[ "$output" == *"resolves no ENVOY_IMAGE_TAG"* || -s "$log" ]] || grep -q '^pg_dump ' "$argv"; then
  fail 'deployer did not refuse a failed read of the deployed tag by name, before its backup'
fi

# Every line the deployer logs is on its standard output, the line of a reader that runs inside a
# command substitution included, so a host that keeps the standard output alone keeps why the
# deployed tag could not be read.
fresh_env
output=$(AUTODEPLOY_TEST_ENV_READ_FAIL=1 test_stderr_fd=4 deploy_once 4>"$root/stderr") || true
if [[ "$output" != *"could not read the deployed ENVOY_IMAGE_TAG; not deploying new-image"* ||
  "$(<"$root/stderr")" == *"could not read the deployed ENVOY_IMAGE_TAG"* ]]; then
  fail "deployer did not log the failed read of the deployed tag on its standard output; its standard error held: $(<"$root/stderr")"
fi

# Without `docker compose`, the deployer refuses by name, before its backup.
fresh_env
test_real_docker=/bin/false
if output=$(deploy_once); then
  fail 'deployer deployed when docker compose cannot run'
fi
unset test_real_docker
if [[ "$output" != *"docker compose cannot run"*"not deploying new-image"* ]] ||
  [[ -s "$log" ]] || grep -q '^pg_dump ' "$argv"; then
  fail 'deployer did not refuse, by name and before its backup, when docker compose cannot run'
fi

# compose_resolves NAME [VAR=value...] prints NAME as Compose resolves it from the test's
# compose/.env, by the real binary (`docker compose config --environment`), in the environment the
# deployer had: the VARs a row exported, and no ENVOY_IMAGE_TAG, which the deployer unsets.
compose_resolves() {
  local name=$1
  shift
  (
    for assignment in "$@"; do
      export "${assignment?}"
    done
    unset ENVOY_IMAGE_TAG
    "$real_docker" compose -f "$compose/dispatch.compose.yml" config --environment 2>/dev/null |
      awk -v key="$name=" 'index($0, key) == 1 && !found { print substr($0, length(key) + 1); found = 1 }'
  )
}
# good_deploy NAME VAR=value... deploys with the login check's callback right, so only a check that
# dials the wrong port, or a write Compose reads otherwise, can stop it. NAME is the row's.
good_deploy() {
  local name=$1 tag written schema_read
  shift
  output=$(
    for assignment in "$@" AUTODEPLOY_TEST_REDIRECT=http://browser.test:8766/auth/callback; do
      export "${assignment?}"
    done
    deploy_once
  ) || true
  if [[ "$output" != *"deployed new-image ("* ]] || [[ "$output" == *"rolling back"* ]]; then
    fail "$name: a good deploy did not land, or was rolled back"
  fi
  tag=$(compose_resolves ENVOY_IMAGE_TAG "$@")
  [[ "$tag" == new-image ]] || fail "$name: Compose resolves ENVOY_IMAGE_TAG to '$tag' after the deploy, not new-image"
  [[ $(grep -cE '^[[:space:]]*(export[[:space:]]+)?ENVOY_IMAGE_TAG[[:space:]]*=' "$compose/.env") == 1 ]] ||
    fail "$name: more than one line sets ENVOY_IMAGE_TAG after the deploy"
  # The schema record the deployer wrote is the value Compose reads back.
  written=$(sed -n 's/^ENVOY_IMAGE_SCHEMA=//p' "$compose/.env")
  schema_read=$(compose_resolves ENVOY_IMAGE_SCHEMA "$@")
  [[ "$written" == old-image:* && "$schema_read" == "$written" ]] ||
    fail "$name: the deployer wrote ENVOY_IMAGE_SCHEMA=$written, and Compose resolves '$schema_read'"
}

# DISPATCH_PORT is read as Compose resolves it for the container: quoted, or from the shell through
# `${...}`, the health and login checks dial the port Dispatch listens on, and a good deploy lands.
fresh_env
with_line DISPATCH_PORT 'DISPATCH_PORT="8766"'
good_deploy 'DISPATCH_PORT="8766"'
fresh_env
with_line DISPATCH_PORT "DISPATCH_PORT=\${PORT_FROM_SHELL}"
# The row's name is the .env line as written, `${...}` unexpanded.
# shellcheck disable=SC2016
good_deploy 'DISPATCH_PORT=${PORT_FROM_SHELL}' PORT_FROM_SHELL=9123 AUTODEPLOY_TEST_PORT=9123
# The tag an operator wrote as `export ENVOY_IMAGE_TAG="old-image"` is read as old-image, and the
# deploy's write leaves Compose reading new-image from one line, not the operator's line.
fresh_env
with_line ENVOY_IMAGE_TAG 'export ENVOY_IMAGE_TAG="old-image"'
good_deploy 'export ENVOY_IMAGE_TAG="old-image"'
[[ "$output" == *"(deployed old-image)"* ]] || fail 'the deployer did not read export ENVOY_IMAGE_TAG="old-image" as old-image'
# A DISPATCH_PORT Compose resolves to no port is refused before the backup.
fresh_env
with_line DISPATCH_PORT 'DISPATCH_PORT=not-a-port'
if output=$(deploy_once); then
  fail 'deployer deployed with DISPATCH_PORT=not-a-port'
fi
if [[ "$output" != *"DISPATCH_PORT to 'not-a-port'"*"not deploying new-image"* || -s "$log" ]] ||
  grep -q '^pg_dump ' "$argv"; then
  fail 'deployer did not refuse DISPATCH_PORT=not-a-port before its backup'
fi

# The static check: in autodeploy.sh, up-dispatch.sh and compose-refusal.sh, which both source, no
# line outside env_file, the one writer, refers to the env file's path - `COMPOSE_ENV`,
# `compose/.env`, or any `.env` path - so the only reads of it are Compose's own, and an alias, a
# backtick, an eval or a direct path, each of which has to name the path somewhere, is refused
# wherever it sits. Comment lines are not code. It fails, as UNKNOWN, when it finds the writer
# naming no path, which would mean it read nothing. Its controls are copies of autodeploy.sh with
# each of those four reads planted, each refused.
env_reads() {
  python3 - "$@" <<'PY'
import re, sys
writer = "env_file"
reference = re.compile(r"COMPOSE_ENV|compose/\.env|(^|[^A-Za-z0-9_.])\.env\b")
problems, writer_refs = [], 0
for path in sys.argv[1:]:
    function = None
    for n, line in enumerate(open(path).read().splitlines(), 1):
        start = re.match(r"([A-Za-z_][A-Za-z0-9_]*)\(\)\s*\{", line)
        if start:
            function = start.group(1)
            continue
        if line == "}":
            function = None
            continue
        if line.strip().startswith("#") or not reference.search(line):
            continue
        if function == writer:
            writer_refs += 1
            continue
        problems.append(f"{path}:{n}: refers to the env file's path outside {writer}: {line.strip()}")
if writer_refs == 0:
    problems.append(f"UNKNOWN: {writer} names the env file's path nowhere in {sys.argv[1:]}, so nothing was judged")
print("\n".join(problems))
sys.exit(1 if problems else 0)
PY
}
here=$(dirname "$0")
env_reads "$here/autodeploy.sh" "$here/up-dispatch.sh" "$here/compose-refusal.sh" >"$root/env-reads.txt" ||
  fail "a script refers to the env file's path outside its one writer: $(<"$root/env-reads.txt")"
# The plants are written as the script would hold them, `$(...)` and `$COMPOSE_DIR` unexpanded.
# shellcheck disable=SC2016
for plant in \
  'f="$COMPOSE_DIR/.env"
  x=$(sed -n "s/^DISPATCH_PORT=//p" "$f")' \
  'x=`sed -n s/^DISPATCH_PORT=//p $COMPOSE_DIR/.env`' \
  'eval '"'"'x=$(sed -n "s/^DISPATCH_PORT=//p" "$COMPOSE_DIR/.env")'"'"'' \
  'x=$(sed -n "s/^DISPATCH_PORT=//p" compose/.env)'; do
  PLANT=$plant python3 -c '
import os, sys
text = open(sys.argv[1]).read()
anchor = "deployed_tag() {\n"
assert text.count(anchor) == 1
sys.stdout.write(text.replace(anchor, anchor + "  " + os.environ["PLANT"] + "\n"))
' "$here/autodeploy.sh" >"$root/planted.sh"
  if env_reads "$root/planted.sh" >/dev/null; then
    fail "the static check passed a copy of autodeploy.sh with this read of the env file planted: $plant"
  fi
done

# The text scan above is fast, and it is a text scan: a path spelled in pieces or matched by a glob
# passes it. The behavioural check: every setting the deployer reads is written so that its raw
# text differs from Compose's resolution of it - single-quoted with a note after it, double-quoted,
# an `export` prefix, or a `${...}` reference to the deployer's environment - so a read of the
# file's text, however it is spelled, gets a value Compose does not give, and this good deploy does
# not land.
fresh_env
with_line ENVOY_IMAGE_TAG "ENVOY_IMAGE_TAG='old-image' # the tag that serves"
# shellcheck disable=SC2016 # the .env lines are written as the file holds them, `${...}` unexpanded
with_line DISPATCH_SERVER_URL 'DISPATCH_SERVER_URL="${AUTODEPLOY_TEST_ORIGIN}"'
with_line DATABASE_URL "DATABASE_URL=\"$database_url\""
# shellcheck disable=SC2016
with_line DISPATCH_PORT 'DISPATCH_PORT="${AUTODEPLOY_TEST_PORT_VALUE}" # the port'
with_line ENVOY_ACCOUNT_GOOGLE_AUDIENCE "export ENVOY_ACCOUNT_GOOGLE_AUDIENCE='envoy-listener'"
good_deploy 'a .env whose every raw value differs from Compose'"'"'s resolution' \
  AUTODEPLOY_TEST_ORIGIN=http://browser.test:8766 AUTODEPLOY_TEST_PORT_VALUE=9124 AUTODEPLOY_TEST_PORT=9124

# The env file owns the deployed tag: a deployer started with ENVOY_IMAGE_TAG in its environment
# deploys once, reads the tag it wrote back, and the next pass deploys nothing, where one that let
# the inherited value win over the file would deploy the same image again on every pass.
fresh_env
good_deploy 'a deployer started with ENVOY_IMAGE_TAG=old-image exported' ENVOY_IMAGE_TAG=old-image
: >"$log"
: >"$argv"
output=$(export ENVOY_IMAGE_TAG=old-image; deploy_once) || true
if [[ "$output" == *"main has a newer published image"* || -s "$log" ]] || grep -q '^pg_dump ' "$argv"; then
  fail 'a deployer started with ENVOY_IMAGE_TAG exported deployed the same image again on its next pass'
fi

# The writer stages beside the env file, mode 0600, and gives the replacement its target's mode.
# A symlink's own mode is normally 777; the target's 0600 is what must reach the replacement.
fresh_env
target="$root/.env.target"
mv "$compose/.env" "$target"
chmod 600 "$target"
ln -s "$target" "$compose/.env"
good_deploy 'a .env symlink whose 0600 target keeps the replacement private'
[[ ! -L "$compose/.env" && "$(stat -c %a "$compose/.env")" == 600 && "$(stat -c %a "$target")" == 600 ]] ||
  fail "the 0600 target of a symlinked .env did not keep its replacement private: .env=$(stat -c %a "$compose/.env"), target=$(stat -c %a "$target")"

# A sweep leaves a stage file a live writer may still append, then deletes the one a SIGKILL left
# over a minute ago. The old sweep deletes the fresh file, so this row fails there.
fresh_env
running="$compose/.env.stage.running"
printf 'keep\n' >"$running"
deploy_once >/dev/null 2>&1 || true
[[ -f "$running" ]] || fail 'the start swept a fresh staging file a concurrent writer may still use'
touch -d '2 minutes ago' "$running"
deploy_once >/dev/null 2>&1 || true
[[ ! -e "$running" ]] || fail 'the start did not sweep a staging file older than one minute'

# The env file is written through a staging file beside it, `.env.stage.XXXXXX`, mode 0600, renamed
# over it. A deployer killed there by SIGKILL, which no trap sees, leaves that file: git ignores
# exactly that name (packages/envoy/.gitignore), and the next start removes it after a minute, and
# nothing else in the directory. A TERM exits the deployer and its EXIT trap removes the file.
repo_root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
fresh_env
AUTODEPLOY_TEST_KILL_AT_ENV_MV=1 deploy_once >/dev/null 2>&1 || true
read -r _ staged staged_mode < <(grep '^mv-source ' "$argv" | head -n 1) ||
  fail 'UNKNOWN: the deployer reached no move onto the env file, so nothing of its write was judged'
[[ "$(dirname "$staged")" == "$compose" && "$(basename "$staged")" == .env.stage.?????? ]] ||
  fail "the deployer staged the env file's new contents at $staged, not $compose/.env.stage.XXXXXX"
[[ "$staged_mode" == 600 ]] || fail "the staging file's mode was $staged_mode, want 600"
[[ -f "$staged" ]] || fail 'UNKNOWN: a deployer killed by SIGKILL at its rename left no staging file, so the sweep was not judged'
git -C "$repo_root" check-ignore -q "packages/envoy/deploy/compose/$(basename "$staged")" ||
  fail "git does not ignore the staging file a killed deployer leaves ($(basename "$staged"))"
if git -C "$repo_root" check-ignore -q packages/envoy/deploy/compose/env.stage.keep; then
  fail 'git ignores more than the staging prefix (env.stage.keep)'
fi
printf 'keep\n' >"$compose/env.stage.keep"
printf 'keep\n' >"$compose/.env.staged"
mkdir -p "$compose/.env.stage.dir"
touch -d '2 minutes ago' "$staged"
deploy_once >/dev/null 2>&1 || true
[[ ! -e "$staged" ]] || fail "the next start did not remove the staging file a killed deployer left: $staged"
[[ -f "$compose/env.stage.keep" && -f "$compose/.env.staged" && -d "$compose/.env.stage.dir" ]] ||
  fail 'the start removed a file or directory of COMPOSE_DIR other than a regular .env.stage.* file'
rm -rf "$compose/env.stage.keep" "$compose/.env.staged" "$compose/.env.stage.dir"
fresh_env
AUTODEPLOY_TEST_TERM_AT_ENV_MV=1 deploy_once >/dev/null 2>&1 || true
grep -q '^mv-source ' "$argv" || fail 'UNKNOWN: the deployer reached no move onto the env file under TERM'
left=$(find "$compose" -maxdepth 1 -name '.env.stage.*' | head -n 1)
[[ -z "$left" ]] || fail "a deployer stopped by TERM at its rename left its staging file: $left"

# The deployer sets no INT or TERM trap, so one stopped by TERM while it waits out its poll stops at
# once, where a TERM trap made bash wait for the running `sleep` to end first. The poll is an hour,
# so a deployer that waits out its sleep stops after an hour and one that does not stops at once;
# the row's bound, 120 s of the clock (SECONDS), guards only against never, so the load of the box
# cannot move a run across it. Every process the row starts is stopped by its own process ID.
# The deploy before it lands new-image, so the poll's pass finds nothing to deploy and sleeps.
fresh_env
output=$(AUTODEPLOY_TEST_REDIRECT=http://browser.test:8766/auth/callback deploy_once) || true
[[ "$output" == *"deployed new-image ("* ]] ||
  fail 'UNKNOWN: the deploy before the poll did not land, so the stop was not judged'
POLL=3600 AUTODEPLOY_TEST_SLEEP_REAL=1 run_deployer >"$root/poll.out" 2>&1 &
job=$!
poll_sleep=
deadline=$((SECONDS + 30))
while ((SECONDS < deadline)); do
  # The deployer is the background job itself or its one child, and its poll is its `sleep` child.
  for pid in "$job" $(pgrep -P "$job" || true); do
    poll_sleep=$(pgrep -P "$pid" -x sleep) && break 2
  done
  sleep 0.1
done
if [[ -z "$poll_sleep" ]]; then
  # Stop every descendant, deepest first, before the wrapper: a stalled command substitution can
  # be deeper than the deployer itself.
  tree=()
  descendants() {
    local pid=$1 child
    for child in $(pgrep -P "$pid" || true); do
      descendants "$child"
      tree+=("$child")
    done
  }
  descendants "$job"
  for pid in "${tree[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  kill "$job" 2>/dev/null || true
  wait "$job" || true
  # A process sent TERM can still be running for a moment, and it is no child of this shell, so
  # each one gets 5 s of the clock to be gone or a zombie before it counts as one the give-up left.
  alive=()
  for pid in "${tree[@]}"; do
    gone=$((SECONDS + 5))
    while state=$(ps -o stat= -p "$pid") && [[ ${state// /} != Z* ]] && ((SECONDS < gone)); do
      sleep 0.1
    done
    if state=$(ps -o stat= -p "$pid") && [[ ${state// /} != Z* ]]; then
      alive+=("$pid")
    fi
  done
  left=
  ((${#alive[@]} == 0)) || left=" (and the give-up left process(es) ${alive[*]} running)"
  fail "UNKNOWN: the deployer reached no poll within 30 s, so its stop was not judged$left; its output: $(<"$root/poll.out")"
fi
deployer=$(ps -o ppid= -p "$poll_sleep" | tr -d ' ')
kill -TERM "$deployer"
stopped=false
termed=$SECONDS
deadline=$((SECONDS + 120))
while ((SECONDS <= deadline)); do
  state=$(ps -o stat= -p "$deployer") || { stopped=true; break; }
  [[ ${state// /} != Z* ]] || { stopped=true; break; }
  sleep 0.1
done
$stopped || kill -KILL "$deployer" 2>/dev/null || true
kill "$poll_sleep" 2>/dev/null || true
wait "$job" || true
$stopped || fail "a deployer stopped by TERM while it waited out its hour's poll was still running $((SECONDS - termed)) s later"

# With the schema already recorded for the serving tag, the deploy's first write of the env file is
# its tag's, after the new image serves; BACKUPS on another filesystem than the env file changes
# nothing for it, and the deploy lands with its tag persisted.
other_fs=$(mktemp -d /dev/shm/autodeploy-test.XXXXXX) || fail 'UNKNOWN: no /dev/shm to put BACKUPS on another filesystem'
[[ "$(stat -c %d "$other_fs")" != "$(stat -c %d "$compose")" ]] ||
  fail 'UNKNOWN: /dev/shm is on the compose directory'"'"'s filesystem here, so the case was not judged'
fresh_env
with_line ENVOY_IMAGE_SCHEMA 'ENVOY_IMAGE_SCHEMA=old-image:5'
good_deploy 'the schema recorded for the serving tag and BACKUPS on another filesystem' test_backups="$other_fs"
rm -rf "$other_fs"

# The deployer reads only through `docker compose config --format json`, so it runs on Compose
# releases that lack `config --environment` (before 2.28): a good deploy lands through Compose
# 2.12.2, the oldest release measured, the release binary taken by digest out of this repository's
# copy of the image that ships it (compose-2.12.2_test.sh). A wrapper is the `docker` that hands
# `compose ...` to it, and counts the calls, so the row cannot pass on another Compose.
# shellcheck source=SCRIPTDIR/compose-2.12.2_test.sh
. "$(dirname "$0")/compose-2.12.2_test.sh"
compose_2_12="$root/docker-compose-2.12.2"
take_compose_2_12 "$real_docker" "$compose_2_12" ||
  fail 'UNKNOWN: could not take Compose 2.12.2 out of its image, so the oldest release was not judged'
cat >"$root/docker-2.12" <<EOF
#!/usr/bin/env bash
[[ "\$1" == compose ]] || exec "$real_docker" "\$@"
shift
printf '%s\n' "\$*" >>"$root/compose-2.12.calls"
exec "$compose_2_12" "\$@"
EOF
chmod +x "$root/docker-2.12"
[[ "$("$root/docker-2.12" compose version --short)" == 2.12.2 ]] || fail 'UNKNOWN: the Compose 2.12.2 wrapper does not run 2.12.2'
: >"$root/compose-2.12.calls"
fresh_env
test_real_docker="$root/docker-2.12"
good_deploy 'a deploy through Compose 2.12.2'
unset test_real_docker
[[ $(grep -c ' config ' "$root/compose-2.12.calls") -ge 2 ]] ||
  fail "UNKNOWN: Compose 2.12.2 served $(grep -c ' config ' "$root/compose-2.12.calls") config reads, want the one of the tag and the schema record, and the settings'"

