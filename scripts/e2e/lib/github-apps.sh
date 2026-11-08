# shellcheck shell=bash disable=SC2034,SC2154 # require_app_inputs sets <role>_app_* by name; the caller sets work
# Legion's two role Apps as a live proof takes them: their ids and the implement App's broker
# secret from the defaults below or a deployment's own inputs, the review App's key from the
# operator's command, and either App proved against GitHub before the run writes anything there.
#
# Sourced, never run. The caller defines fail (which exits) and note, and sets work, the run's
# scratch directory. The gated Go proofs (packages/daemon/internal/api/real_github_test.go) are
# callers too: liveProofImplementApp defines fail and reads require_app_inputs's results and
# app_token_command from stdout, so neither of those two writes anything else to stdout, and
# proveLiveProofApps defines fail and note, writes $work/github-apps.yaml itself from the block the
# proofs act with, and runs check_github_apps on it. require_app_inputs reads each role from its
# own inputs:
#   LEGION_E2E_<ROLE>_APP_ID           the App's numeric id (ROLE is IMPLEMENT or REVIEW), which
#                                      defaults to the id below
#   LEGION_E2E_IMPLEMENT_APP_SECRET    the name of the secrets broker's github-app secret for the
#                                      implement App, which `agent-secrets` turns into a release of
#                                      that App's installation tokens, never its key; it is an
#                                      agent-tier secret and defaults to the name below
#   LEGION_E2E_REVIEW_APP_KEY_COMMAND  shell text that prints the review App's PEM private key on
#                                      stdout, which the daemon runs as that App's
#                                      private_key_command. It has no default and no fallback:
#                                      this repository is public (AGENTS.md), so a value a live
#                                      script needs is a required operator input the script
#                                      refuses to start without, never a literal, and no name of
#                                      the infrastructure a deployment keeps its key in belongs
#                                      here. The lib carries the command text alone; the key
#                                      itself never enters a variable, an argv, a note or a
#                                      message here.
# LEGION_E2E_REVIEW_APP_SECRET is retired, and a run that still exports it is refused when the
# review role is read: that App's tokens no longer come from the broker.
# write_github_apps writes the `github_apps` block of legion.yaml from them, one credential field
# a role, which is what the daemon admits: the implement App a token_command that runs
# `agent-secrets <secret> … -- …` (or reads the release file the refresher below wrote, when its
# caller names that directory), the review App the private_key_command the operator gave. Then
# check_github_apps proves each App from its token, whether its inputs came from a default or an
# operator: lib/app-logins.ts reads the token's bot login from GraphQL's viewer and refuses the run
# unless GitHub's GET /apps/<slug> answers the configured id and the write permission the role's
# work needs. No token and no key enters this shell or an argv, and either enters a file only
# through the refresher (mint_app_tokens, start_app_token_refresher), for a daemon that can run
# neither agent-secrets nor the key command: it writes each release, and the review App's key, to
# an owner-only file of a directory its caller names (the sandbox proof's, on the devbox user's
# tmpfs under bridge) and removes at teardown, and the caller passes that directory to
# write_github_apps, whose commands then read those files. Each caller that writes a legion.yaml
# includes $work/github-apps.yaml as it is (`$(<"$work/github-apps.yaml")` in its heredoc), never a
# copy or an edit, so the block the check proved is the block the daemon loads; Stage 4a, which
# writes no legion.yaml and acts as the implement App alone, hands its Go harness the same
# implement_app_id, implement_app_secret and app_token_command the check read. app_key_unsets is
# the `env` argument list that keeps every App credential variable out of a process the run starts.
# It is built when require_app_inputs runs, so a variable a caller exports after that call is not
# in it.

github_apps_lib=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)

# The defaults, which a run that sets no input takes: the App ids of the legion-implementer and
# legion-reviewer Apps, and the name of the secrets broker github-app secret the implement App's
# tokens come from, which is agent tier. The review App has no default credential, since the only
# credential it takes is a key (see the header). A deployment with Apps of its own passes them
# through the inputs; no other deployment's ids or names belong here.
default_implement_app_id=5341240
default_implement_app_secret=LEGION_IMPLEMENT_APP
default_review_app_id=5341243

# require_app_inputs ROLE... reads each ROLE's inputs (implement, review) and fails naming an input
# that is set but malformed, never echoing its value (an operator who pastes a credential where its
# name or its command goes must not see it in the transcript). It sets <role>_app_id from
# LEGION_E2E_<ROLE>_APP_ID for each role, defaulting to the id above: 1 to 12 digits. For implement
# it sets implement_app_secret from LEGION_E2E_IMPLEMENT_APP_SECRET, defaulting to the name above:
# letters, digits and underscores, so it is safe in the token command's shell text and as a
# variable name. For review it sets review_app_key_command from LEGION_E2E_REVIEW_APP_KEY_COMMAND,
# which has no default, and refuses a value that is unset, empty, whitespace alone, or the key
# itself pasted where its command goes (it holds -----BEGIN); it also refuses a run that still
# exports the retired LEGION_E2E_REVIEW_APP_SECRET, whose broker secret that App no longer has.
# When this call reads both roles, one App id for both is refused, since the daemon refuses two
# roles on one App at boot. It records the roles it read for write_github_apps, and sets
# app_key_unsets: `-u NAME` for each read role's broker secret name, for the implement default's,
# for the two names the retired review inputs used (LEGION_REVIEW_APP,
# GH_REVIEW_APP_PRIVATE_KEY_B64), and for every exported variable whose name holds PRIVATE_KEY, so
# a process the run starts inherits no App credential, the proof human's included.
github_apps_read=()
app_key_unsets=()
require_app_inputs() {
  # The checks below use bracket ranges, which bash's =~ reads in the caller's locale: under a
  # UTF-8 locale [0-9] and [A-Za-z] admit fullwidth digits and accented letters. C keeps them ASCII.
  local LC_ALL=C
  local role upper id_var secret_var id_default secret_default id secret key_command name roles_read=()
  for role in "$@"; do
    case "$role" in implement | review) ;; *) fail "require_app_inputs: no App role '$role'" ;; esac
    upper=${role^^}
    id_var=LEGION_E2E_${upper}_APP_ID
    id_default=default_${role}_app_id
    # An input set to an empty value is a command that printed nothing, not a choice of the
    # default: it is refused, where an unset one takes the default.
    id=${!id_var-${!id_default}}
    # The bounds keep a pasted credential out of the message: a token is letters and digits too, so
    # only its length tells it from a secret's name, and a refusal prints neither.
    [[ "$id" =~ ^[1-9][0-9]*$ && ${#id} -le 12 ]] || fail "$id_var must be the $role App's numeric id (1 to 12 digits, no leading zero); its value is not printed"
    printf -v "${role}_app_id" %s "$id"
    if [ "$role" = review ]; then
      # The review App's credential is its key, which the operator's command prints: a required
      # input with no default, since this repository is public (AGENTS.md). Only the command's text
      # passes through this shell, and a refusal prints neither it nor the retired variable.
      [ -z "${LEGION_E2E_REVIEW_APP_SECRET+set}" ] ||
        fail "LEGION_E2E_REVIEW_APP_SECRET is retired: the review App's credential is its key, not a secrets broker release. Unset it and set LEGION_E2E_REVIEW_APP_KEY_COMMAND to a command that prints the App's PEM private key; neither value is printed"
      key_command=${LEGION_E2E_REVIEW_APP_KEY_COMMAND-}
      [[ "$key_command" =~ [^[:space:]] ]] ||
        fail "LEGION_E2E_REVIEW_APP_KEY_COMMAND must be set to shell text that prints the review App's PEM private key on stdout; its value is not printed"
      [[ "$key_command" != *-----BEGIN* ]] ||
        fail "LEGION_E2E_REVIEW_APP_KEY_COMMAND must name a command that prints the review App's private key, not the key itself; its value is not printed"
      review_app_key_command=$key_command
    else
      secret_var=LEGION_E2E_${upper}_APP_SECRET
      secret_default=default_${role}_app_secret
      secret=${!secret_var-${!secret_default}}
      [[ "$secret" =~ ^[A-Za-z_][A-Za-z0-9_]*$ && ${#secret} -le 64 ]] || fail "$secret_var must be the name of a secrets broker github-app secret (letters, digits and underscores, at most 64), not a credential; its value is not printed"
      printf -v "${role}_app_secret" %s "$secret"
    fi
    roles_read+=("$role")
  done
  if [[ " ${roles_read[*]} " == *" implement "* && " ${roles_read[*]} " == *" review "* ]]; then
    [ "$implement_app_id" != "$review_app_id" ] ||
      fail "LEGION_E2E_IMPLEMENT_APP_ID and LEGION_E2E_REVIEW_APP_ID are both $implement_app_id: the workflow needs two Apps"
  fi
  github_apps_read=("${roles_read[@]}")
  app_key_unsets=()
  for role in "${roles_read[@]}"; do
    # Only a role whose tokens come from the broker has a secret name to strip, and the review
    # role has none. Its own input, LEGION_E2E_REVIEW_APP_KEY_COMMAND, is exported and every
    # child does inherit it, but it carries no credential: the -----BEGIN refusal above holds it
    # to a command that prints the key rather than to the key itself.
    secret_var=${role}_app_secret
    if [ -n "${!secret_var-}" ]; then
      app_key_unsets+=(-u "${!secret_var}")
    fi
  done
  # The implement default, and the two names the retired review inputs used: a stale export of
  # either must not reach a process the run starts.
  for name in "$default_implement_app_secret" LEGION_REVIEW_APP GH_REVIEW_APP_PRIVATE_KEY_B64; do
    app_key_unsets+=(-u "$name")
  done
  while IFS= read -r name; do
    [[ "$name" != *PRIVATE_KEY* ]] || app_key_unsets+=(-u "$name")
  done < <(compgen -e)
}

# app_token_command NAME prints the token command that prints the release the secrets broker's
# github-app secret NAME gives: `agent-secrets` puts the release in one `sh` child's environment
# as NAME, which prints it to stdout, the daemon's pipe. It names --ttl 12h, as the deployment
# example does (docs/deployment.md): a deployment may hold a role's github-app secret at human
# tier, a human-tier request naming no lifetime gets 15 minutes, and the daemon renews a lease
# about 40 minutes after it mints one, so without the flag every renewal of such a run would wait
# for a person again. With it the approver reads the twelve hours on the card and decides them
# once. The three `${…}` expansions are the command's own, expanded by the sh the daemon runs it
# with: LEGION_GITHUB_OWNER, when set, narrows the release to that owner's installation
# (--github-owner), LEGION_GITHUB_REPOS, when set, to those repositories of it, one --github-repo
# for each name the daemon's single spaces separate, and LEGION_GITHUB_READ_ONLY, when set, to
# contents, metadata, pull requests and Actions read (--github-read-only). It depends on nothing
# but NAME.
app_token_command() {
  # shellcheck disable=SC2016 # the expansions are the token command's own, run by its sh
  printf 'agent-secrets %s --ttl 12h --reason=legion-live-proof-github-app-token ${LEGION_GITHUB_OWNER:+--github-owner "$LEGION_GITHUB_OWNER"} ${LEGION_GITHUB_REPOS:+$(printf '\'' --github-repo %%s'\'' $LEGION_GITHUB_REPOS)} ${LEGION_GITHUB_READ_ONLY:+--github-read-only} -- sh -c '\''printf %%s "$%s"'\''\n' "$1" "$1"
}

# app_token_dir_ok DIR fails unless DIR is a path of letters, digits and `_ . / -` alone, since the
# credential commands carry it in their shell text.
app_token_dir_ok() {
  local LC_ALL=C
  [[ "$2" =~ ^[A-Za-z0-9_./-]+$ ]] ||
    fail "$1: the token directory '$2' holds a character other than letters, digits and _ . / -, which the credential commands could not carry in their shell text"
}

# app_token_dir_made ROLE DIR makes DIR, where ROLE's credential files go, owner-only: mode 0700,
# and umask 077 for whatever is written under it. It returns 1 with app_token_mint_failed naming
# the directory when it cannot. Both minters below make their role's directory through this, so
# the releases and the key are given the same directory and the same refusal.
app_token_dir_made() {
  if ! (umask 077 && mkdir -p -- "$2" && chmod 0700 -- "$2"); then
    app_token_mint_failed="the $1 App's credential directory $2 could not be made owner-only"
    return 1
  fi
}

# The refresher, for a daemon that can run neither agent-secrets nor the operator's key command
# (the Claude Code sandbox proof's container under bridge). It mints, here, each role's credential
# into DIR, one directory a role:
#   DIR/<role>/_all.json           the release of every installation (no owner)
#   DIR/<role>/<owner>.json        OWNER's installation alone
#   DIR/<role>/<owner>.read.json   OWNER's installation, read-only
#   DIR/review/key.pem             the review App's private key, written once by mint_app_tokens
# owner lowercased, each file 0600 and each directory 0700, which is the layout the commands
# write_github_apps --token-dir DIR writes read. A release, and the key with it, passes from its
# command's `sh` child to a temporary file of the same directory, then mv renames it over the file,
# so a reader sees the whole old file or the whole new one, and a mint that fails or prints nothing
# leaves no temporary file and the last good file as it was. No token and no key passes through
# this shell.
#
# app_token_mint DIR ROLE... mints every release file once, for the owners app_token_owners lists,
# and returns 1 at the first failure with app_token_mint_failed naming the App's role, id and
# secret, the file and agent-secrets' exit status, never a token. Every ROLE it takes is a role
# whose App has a broker secret; the review App's key is app_token_mint_key's, minted once rather
# than refreshed. agent-secrets' own stderr (the request a grant waits on) reaches this shell's
# stderr as it is written.
app_token_owners=()
# app_token_roles is what the last mint_app_tokens minted releases for and what the refresher mints
# again: the read roles whose tokens come from the broker.
app_token_roles=()
app_token_mint_failed=
app_token_mint() {
  local dir=$1 role id_var secret_var owner ro file status
  shift
  app_token_mint_failed=
  for role in "$@"; do
    id_var=${role}_app_id
    secret_var=${role}_app_secret
    app_token_dir_made "$role" "$dir/$role" || return 1
    for owner in "" "${app_token_owners[@]}"; do
      owner=${owner,,}
      for ro in "" 1; do
        # The release of every installation is never asked read-only.
        [ -n "$owner" ] || [ -z "$ro" ] || continue
        file=$dir/$role/${owner:-_all}${ro:+.read}.json
        status=0
        app_token_mint_file "$file" "$owner" "$ro" "${!secret_var}" || status=$?
        case "$status" in
          0) continue ;;
          125) app_token_mint_failed="agent-secrets printed nothing" ;;
          126) app_token_mint_failed="agent-secrets printed more than $app_token_release_limit bytes, which no release is" ;;
          123) app_token_mint_failed="agent-secrets wrote more than $app_token_release_limit bytes to stderr" ;;
          *) app_token_mint_failed="agent-secrets exited $status; no token is printed" ;;
        esac
        app_token_mint_failed="the $role App (App ${!id_var}, github-app secret ${!secret_var}) gave no release for $file: $app_token_mint_failed"
        return 1
      done
    done
  done
}
# app_token_redact copies its stdin to its stdout, line by line as it arrives, with every GitHub
# token form replaced, as the daemon redacts a token command's stderr (internal/appauth,
# tokenPattern).
app_token_redact() {
  LC_ALL=C sed -u -E 's/(gh[opsur]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,})/[redacted]/g'
}
# app_token_stderr_bound LIMIT MARKER passes on whole lines of its stdin while their bytes stay
# within LIMIT. Its input is cut at LIMIT+1 bytes by the caller, so the line that crosses LIMIT,
# which may end in a fragment of a token, is dropped, nothing after it is read, and MARKER is
# created to say the stream overflowed. Every line it passes is whole, so app_token_redact, after
# it, sees each token whole.
app_token_stderr_bound() {
  LC_ALL=C awk -v limit="$1" -v marker="$2" '
    { total += length($0) + 1
      if (total > limit) { printf "" > marker; exit }
      print; fflush() }'
}
# app_token_mint_command FILE COMMAND writes COMMAND's stdout to FILE atomically, which is how a
# release and the review App's key alike reach a file. COMMAND runs under `timeout` (35 minutes:
# agent-secrets' 30-minute approval wait and a margin; killed 10 s after) with no stdin. Its stdout
# is cut at app_token_release_limit+1 bytes into a temporary file of FILE's own directory, which mv
# renames over FILE. Its stderr reaches the run's as it is written, whole lines only, through
# app_token_stderr_bound and app_token_redact, at most app_token_release_limit bytes. It returns
# COMMAND's non-zero status (124 when it timed out), 126 when it printed more than the limit, 123
# when it wrote more than the limit to stderr, or 125 when it succeeded and printed nothing; either
# way it leaves no temporary file and FILE as it was. What COMMAND prints passes from its stdout to
# the file and enters no shell variable and no argv.
app_token_release_limit=1048576
app_token_mint_command() {
  local file=$1 command=$2 tmp overflow status=0
  tmp=$(umask 077 && mktemp "$(dirname -- "$file")/.mint.XXXXXXXX") || return 1
  overflow=$tmp.stderr-overflow
  (
    umask 077
    set -o pipefail
    {
      timeout --kill-after=10s 35m sh -c "$command" \
        2>&1 1>&3 3>&- </dev/null |
        head -c "$((app_token_release_limit + 1))" |
        app_token_stderr_bound "$app_token_release_limit" "$overflow" |
        app_token_redact >&2
    } 3>&1 | head -c "$((app_token_release_limit + 1))" >"$tmp"
  ) || status=$?
  if [ -e "$overflow" ]; then
    status=123
  elif { [ "$status" = 0 ] || [ "$status" = 141 ]; } && [ "$(wc -c <"$tmp")" -gt "$app_token_release_limit" ]; then
    status=126
  fi
  rm -f -- "$overflow"
  if [ "$status" != 0 ]; then
    rm -f -- "$tmp"
    return "$status"
  fi
  if [ ! -s "$tmp" ]; then
    rm -f -- "$tmp"
    return 125
  fi
  if ! mv -f -- "$tmp" "$file"; then
    rm -f -- "$tmp"
    return 1
  fi
}
# app_token_mint_file FILE OWNER READ_ONLY SECRET writes one release of the secrets broker's
# github-app secret SECRET, for the installation of OWNER (empty for every installation) and the
# read-only flag READ_ONLY, through app_token_mint_command, whose status it returns.
app_token_mint_file() {
  local file=$1 owner=$2 ro=$3 secret=$4
  # The three variables are the token command's own inputs, as the daemon sets them for it.
  # LEGION_GITHUB_REPOS is empty, never the run's own: a recorded release is keyed by owner and the
  # read-only flag alone, so it must reach every repository of that installation. A key command
  # gets none of them, because the daemon sets none when it runs one (internal/config/stage3.go,
  # executePrivateKeyCommand: the command's environment is the daemon's own, less the NATS seeds).
  LEGION_GITHUB_OWNER=$owner LEGION_GITHUB_REPOS='' LEGION_GITHUB_READ_ONLY=$ro \
    app_token_mint_command "$file" "$(app_token_command "$secret")"
}
# app_token_mint_key DIR writes the review App's private key, once, to DIR/review/key.pem (mode
# 0600, in a directory of mode 0700) by running the operator's LEGION_E2E_REVIEW_APP_KEY_COMMAND
# through app_token_mint_command, as the daemon would run it. It returns 1 with
# app_token_mint_failed naming the App by its role and id, the file and what went wrong, and never
# a byte the command printed; the command's own stderr has already reached the run's stderr.
app_token_mint_key() {
  local dir=$1 file=$1/review/key.pem status=0
  app_token_mint_failed=
  app_token_dir_made review "$dir/review" || return 1
  app_token_mint_command "$file" "$review_app_key_command" || status=$?
  case "$status" in
    0) return 0 ;;
    125) app_token_mint_failed="the key command printed nothing" ;;
    126) app_token_mint_failed="the key command printed more than $app_token_release_limit bytes, which no private key is" ;;
    123) app_token_mint_failed="the key command wrote more than $app_token_release_limit bytes to stderr" ;;
    *) app_token_mint_failed="the key command exited $status (124 is its 35-minute timeout); no key is printed" ;;
  esac
  app_token_mint_failed="the review App (App $review_app_id, LEGION_E2E_REVIEW_APP_KEY_COMMAND) gave no private key for $file: $app_token_mint_failed"
  return 1
}

# mint_app_tokens DIR OWNER... records the OWNERs the run serves (app_token_owners) and mints,
# synchronously, every release file of each role the last require_app_inputs read whose tokens come
# from the broker (app_token_roles, the roles the refresher mints again) and, when that call read
# the review role, that App's private key once into DIR/review/key.pem. It fails, naming the App by
# its role and id, at the first mint that fails or prints nothing. A secret whose grant waits on a
# person's approval waits here, as long as agent-secrets does. DIR must exist; the caller owns it
# and removes it, the files with it.
mint_app_tokens() {
  local dir=$1 owner role secret_var
  shift
  app_token_dir_ok mint_app_tokens "$dir"
  [ -d "$dir" ] || fail "mint_app_tokens: the token directory $dir is not a directory"
  [ "${#github_apps_read[@]}" -gt 0 ] || fail "mint_app_tokens: require_app_inputs has not run"
  for owner in "$@"; do
    [[ "$owner" =~ ^[A-Za-z0-9][A-Za-z0-9-]*$ ]] ||
      fail "mint_app_tokens: '$owner' is not a GitHub account login"
  done
  app_token_owners=("$@")
  # A role whose App has a broker secret has releases to mint, and to mint again as they expire;
  # the review App has a key command instead, run once below.
  app_token_roles=()
  for role in "${github_apps_read[@]}"; do
    secret_var=${role}_app_secret
    [ -z "${!secret_var-}" ] || app_token_roles+=("$role")
  done
  if [ "${#app_token_roles[@]}" -gt 0 ]; then
    app_token_mint "$dir" "${app_token_roles[@]}" || fail "$app_token_mint_failed"
  fi
  if [[ " ${github_apps_read[*]} " == *" review "* ]]; then
    app_token_mint_key "$dir" || fail "$app_token_mint_failed"
  fi
}

# start_app_token_refresher DIR starts the background refresher, which mints every release file of
# the last mint_app_tokens (its broker-secret roles and its owners, app_token_roles and
# app_token_owners) again every app_token_refresh_seconds (15 minutes: an installation token lasts
# an hour, and the daemon runs its command again once fewer than 20 minutes remain on a lease), and
# sets app_token_refresher to its pid. It never touches the review App's key file: a private key
# does not expire, and that App has no broker secret to ask for. A failed mint prints one line
# naming the App and keeps the last good files; the next round tries again. Run mint_app_tokens
# first, so the first releases and their failures are the prerequisites'. stop_app_token_refresher
# kills it.
app_token_refresh_seconds=900
app_token_refresher=
start_app_token_refresher() {
  local dir=$1 driver=$$
  # Nothing expires when no read role's tokens come from the broker, so there is nothing to start.
  [ "${#app_token_roles[@]}" -gt 0 ] || return 0
  (
    sleeper=
    trap '[ -z "$sleeper" ] || kill "$sleeper" 2>/dev/null; exit 0' TERM
    while :; do
      sleep "$app_token_refresh_seconds" &
      sleeper=$!
      wait "$sleeper" || true
      sleeper=
      # A driver killed without its TERM trap leaves this loop behind: stop once the driver or
      # its token directory is gone, so no orphan keeps minting tokens.
      kill -0 "$driver" 2>/dev/null && [ -d "$dir" ] || exit 0
      app_token_mint "$dir" "${app_token_roles[@]}" ||
        printf '   [refresher] could not refresh: %s; the last releases stay\n' "$app_token_mint_failed" >&2
    done
  ) &
  app_token_refresher=$!
}
# stop_app_token_refresher stops the refresher, when one runs, and waits for it.
stop_app_token_refresher() {
  [ -n "$app_token_refresher" ] || return 0
  kill -TERM "$app_token_refresher" 2>/dev/null || true
  wait "$app_token_refresher" 2>/dev/null || true
  app_token_refresher=
}

# write_github_apps [--token-dir DIR] ROLE... writes $work/github-apps.yaml, the `github_apps` block
# of legion.yaml for each ROLE the last require_app_inputs read, or fails naming a role it did not
# read, before the file is opened. Each role carries its app_id and exactly one credential field,
# which is what the daemon admits (packages/daemon/internal/config/stage3.go, readGitHubApp): the
# implement App a token_command, app_token_command's, and the review App a private_key_command, the
# operator's command verbatim. Given --token-dir DIR (the directory mint_app_tokens wrote), the
# implement App's token_command instead reads the release file of the owner and read-only flag the
# daemon asks for, `DIR/<role>/${owner:-_all}${LEGION_GITHUB_READ_ONLY:+.read}.json` with the owner
# lowercased, with `cat --`, in any POSIX sh (busybox's included), and the review App's
# private_key_command reads DIR/review/key.pem the same way. That form serves what the refresher
# minted, which is keyed by owner and the read-only flag alone, so it ignores LEGION_GITHUB_REPOS
# and answers a token over every repository of the owner's installation: the refresher runs
# agent-secrets, and the key command, where the daemon can run neither, and it mints one release
# per owner rather than one per scope a role asks for. The block names commands, never a token and
# never a key.
write_github_apps() {
  local LC_ALL=C
  local role id_var secret_var token_dir='' field command block="github_apps:"$'\n'
  if [ "${1-}" = --token-dir ]; then
    [ "$#" -ge 2 ] || fail "write_github_apps: --token-dir names no directory"
    token_dir=$2
    shift 2
    app_token_dir_ok write_github_apps "$token_dir"
  fi
  for role in "$@"; do
    [[ " ${github_apps_read[*]} " == *" $role "* ]] ||
      fail "write_github_apps: the $role App's inputs are unread: require_app_inputs $role has not run"
    id_var=${role}_app_id
    if [ "$role" = review ]; then
      field=private_key_command
      if [ -n "$token_dir" ]; then
        # The single quotes hold the path to one word: app_token_dir_ok admits no quote in DIR.
        command=$(printf "cat -- '%s/%s/key.pem'" "$token_dir" "$role")
      else
        command=$review_app_key_command
      fi
    else
      field=token_command
      if [ -n "$token_dir" ]; then
        # shellcheck disable=SC2016 # the expansions are the token command's own, run by its sh
        command=$(printf 'owner=$(printf %%s "$LEGION_GITHUB_OWNER" | tr "[:upper:]" "[:lower:]"); case $owner in *[!a-z0-9-]*) echo "no release file for owner $owner" >&2; exit 2;; esac; cat -- '\''%s/%s/'\''"${owner:-_all}${LEGION_GITHUB_READ_ONLY:+.read}.json"' "$token_dir" "$role")
      else
        secret_var=${role}_app_secret
        command=$(app_token_command "${!secret_var}")
      fi
    fi
    # jq quotes the command as a JSON string, which YAML reads as a double-quoted scalar.
    block+=$(printf '  %s:\n    app_id: "%s"\n    %s: %s' "$role" "${!id_var}" "$field" \
      "$(printf '%s' "$command" | jq -Rs .)")$'\n'
  done
  printf '%s' "$block" >"$work/github-apps.yaml"
}

# check_github_apps ROLE... proves each ROLE's App of $work/github-apps.yaml against GitHub and sets
# github_app_logins to lib/app-logins.ts's answer, {"<role>":"<slug>[bot]",…}: each App's
# credential is taken from the block as the daemon takes it, GraphQL's viewer under an
# installation's token names the bot, and GET /apps/<slug> must answer the App's id and the write
# permission the role's work needs (the implement App's contents, the review App's checks), which
# also refuses the two Apps swapped whole.
# A command that fails, a release GitHub refuses, an answer naming another id or lacking the
# permission, or a GitHub that never answers fails the check, naming the App by its role and id and
# never a token. One of those failures has its fix in an input rather than on GitHub: the secrets
# broker refusing the implement App's github-app secret outright, UNKNOWN_SECRET (`no agent secret
# has this name`), which says this deployment's broker holds no credential under the name that
# role's input gave. app-logins.ts reads that refusal out of the run it already makes, asking the
# broker nothing of its own (a probe of a human-tier secret raises a person's approval card), and
# writes the role and the LEGION_E2E_IMPLEMENT_APP_SECRET to set on its own stderr, above the
# refusal below.
github_app_logins=
check_github_apps() {
  local roles
  roles=$(IFS=, && printf %s "$*")
  github_app_logins=$(bun "$github_apps_lib/app-logins.ts" --roles "$roles" "$work/github-apps.yaml") ||
    fail "lib/app-logins.ts could not prove the Legion role Apps against GitHub from their tokens (its reason is above)"
  note "GET /apps/<slug> answers each App's configured id under its own token: $(jq -c . <<<"$github_app_logins")"
}
