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
#                                      stdout. The run runs it once, on the driver, in the
#                                      operator's own environment (write_github_apps --key-dir,
#                                      or mint_app_tokens where the caller passes --token-dir),
#                                      so it may read the operator's HOME, their own gcloud or
#                                      kubectl configuration and any variable of their shell. The
#                                      daemon never runs it: it reads the key from the run's own
#                                      0600 file, whose `cat` is the private_key_command the
#                                      block below carries. The input has no default and no
#                                      fallback: this repository is public (AGENTS.md), so a
#                                      value a live script needs is a required operator input the
#                                      script refuses to start without, never a literal, and no
#                                      name of the infrastructure a deployment keeps its key in
#                                      belongs here. What that command prints enters no variable,
#                                      no argv, no note and no message here: it passes from its
#                                      stdout to the run's own key file. The input itself may
#                                      hold a key even so — a one-line `printf` of a PEM is a
#                                      command, and runs — so it is in app_key_unsets, and no
#                                      process a proof starts with that list (the listener,
#                                      Dispatch, the daemon, the bridge) inherits it.
# LEGION_E2E_REVIEW_APP_SECRET is retired, and a run that still exports it is refused when the
# review role is read: that App's tokens no longer come from the broker.
# write_github_apps writes the `github_apps` block of legion.yaml from them, one credential field
# a role, which is what the daemon admits: the implement App a token_command that runs
# `agent-secrets <secret> … -- …` (or reads the release file the refresher below wrote, when its
# caller names that directory), the review App a private_key_command that reads the run's own key
# file with `cat`. The operator's command is never that field. It runs here instead, once, before
# the run writes anything, in the caller's own environment, and what it prints goes to a 0600 file
# of a directory the caller names and its cleanup removes unconditionally on every exit: --key-dir
# DIR, which write_github_apps resolves itself, or --token-dir DIR, which mint_app_tokens wrote
# the key into beside the releases. So the check below and the daemon can no longer disagree about
# that command, since both run the same `cat`, and a command that reads the operator's HOME or
# their own configuration is fine. Then check_github_apps proves each App from its token, whether
# its inputs came from a default or an operator: lib/app-logins.ts reads the token's bot login from
# GraphQL's viewer and refuses the run unless GitHub's GET /apps/<slug> answers the configured id
# and the write permission the role's work needs. No token and no key that a command prints
# enters this shell or an argv: each passes from its command's stdout to an owner-only file of a
# directory its caller names and removes at teardown, and the commands the block carries read
# those files. A token reaches a file
# only where the daemon can run neither agent-secrets nor a key command (the sandbox proof's
# container under bridge), through the refresher (mint_app_tokens, start_app_token_refresher,
# writing to the devbox user's tmpfs); the review App's key reaches one in every mode.
# Each caller that writes a legion.yaml includes $work/github-apps.yaml as it is
# (`$(<"$work/github-apps.yaml")` in its heredoc), never a copy or an edit, so the block the check
# proved is the block the daemon loads; Stage 4a, which
# writes no legion.yaml and acts as the implement App alone, hands its Go harness the same
# implement_app_id, implement_app_secret and app_token_command the check read. app_key_unsets is
# the `env` argument list that keeps every App credential variable out of a process the run starts.
# It is built when require_app_inputs runs, so a variable a caller exports after that call is not
# in it.

github_apps_lib=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)

# When a credential command's stderr reaches the operator is decided by the stages its relay
# pipeline is made of (app_token_mint_command: `head -c`, app_token_stderr_bound, then
# app_token_redact or app_token_redact_key), because each of them can hold a line it has already
# read until its own buffer fills or its writer closes. Two of the three do, with the tools a
# Debian devbox and this pod have, so a command that prints a line and then waits — a progress
# line before a slow read of a deployment's own store — reaches nobody until it exits:
#   awk       mawk, which is awk on a Debian devbox and in this pod (mawk 1.3.4), reads a pipe a
#             block at a time unless it is given `-W interactive`, which has it read a line at a
#             time and write each line out as it goes.
#   head -c   GNU head writes its stdout through stdio, which is fully buffered on a pipe, so it
#             holds the first line until 4 KiB have gone by or its input ends, unless it is run
#             under `stdbuf -o0`.
# Measured here through the real path, app_token_mint_key over a key command that prints a line
# and then sleeps two seconds: the line reaches the run's stderr after 2104 ms unchanged, after
# 2099 ms with the awk flag alone, after 2134 ms with the stdbuf alone — the two buffers are in
# series, so either one left in holds the line by itself — and after 99 ms with both, of a
# command that exits at 2128 ms. Stage by stage, over a line printed before a two-second sleep:
# both relays plain 2026 ms against 45 ms with the flag, one relay alone 2031 ms against 49 ms,
# `head -c 1048577` 2024 ms against 24 ms under `stdbuf -o0`. app_token_redact's `sed -u` is
# already prompt, at 28 ms, which is what its `-u` is for. Measure this in a script run by
# `bash`, never at a prompt of a shell whose `head` is a builtin (`type head` says which): a
# builtin writes straight out, so the pipeline looks prompt there while the one a stage proof
# runs, with /usr/bin/head in it, is not. That reading is how the 4 KiB above was missed once.
#
# Both are probes of the invocation rather than checks of a tool's name or version, since either
# flag belongs to one implementation. `-W` is gawk's compatibility switch, so another awk may
# take `-W interactive`, refuse it, or take it with a warning, and a warning would be a line of
# noise in the run's transcript for every line a relay passes — the channel these relays exist to
# keep readable. So the awk probe runs the flag exactly as the relays run it and takes it only
# when that run exits 0 and writes nothing at all to stderr. Both probes run once, here where the
# lib is sourced, rather than per relay call: the awk probe costs about 10 ms and the stdbuf one,
# which is two runs, about 30 ms, and neither answer can change within a run. A tool that
# refuses its flag leaves its array empty and keeps today's behaviour exactly, buffering and all,
# with nothing said about it. The probes write nothing anywhere and read nothing of the caller's:
# their input is /dev/null or a line of their own, their output goes to /dev/null or to a pipe of
# their own, and what they would have written to stderr is what they judge themselves by.
app_token_awk_interactive=()
if app_token_awk_probe=$(LC_ALL=C awk -W interactive '{ print }' </dev/null 2>&1 >/dev/null) && [ -z "$app_token_awk_probe" ]; then
  app_token_awk_interactive=(-W interactive)
fi
unset -v app_token_awk_probe

# The stdbuf probe is in two steps, because `stdbuf` works by preloading a libc shim into the
# program it runs: on a host whose libc it cannot preload into (musl) it is there, runs the
# program, exits 0 and says nothing, and buffers exactly as before. An exit status proves it ran,
# never that it unbuffered, so the second step watches the effect. First the run itself, as the
# pipeline makes it, which must exit 0 and write nothing: that is `stdbuf` present, running
# `head`, and silent, so the pipeline would gain no warning of its own. Then one line through a
# `head` of its own, written into it and read back while its input stays open: unbuffered, the
# whole step takes about 15 ms, and it allows 0.5 s, more than thirty times that, which is also
# the whole cost of the probe on a host where stdbuf changes nothing. head's stderr is read with
# its stdout, so a `stdbuf` that works but warns is refused here too. The timeout is a bound, not
# a measurement: a host loaded enough to miss it keeps today's buffering and says nothing.
app_token_head_unbuffered=()
if app_token_head_probe=$(stdbuf -o0 head -c 1 </dev/null 2>&1 >/dev/null) && [ -z "$app_token_head_probe" ]; then
  coproc app_token_head_coproc { stdbuf -o0 head -c 4096 2>&1; }
  app_token_head_in=${app_token_head_coproc[1]}
  app_token_head_out=${app_token_head_coproc[0]}
  app_token_head_line=
  printf 'x\n' >&"$app_token_head_in" 2>/dev/null || true
  IFS= read -r -t 0.5 -u "$app_token_head_out" app_token_head_line 2>/dev/null || true
  exec {app_token_head_in}>&- {app_token_head_out}<&-
  wait "${app_token_head_coproc_PID:-$$}" 2>/dev/null || true
  [ "$app_token_head_line" != x ] || app_token_head_unbuffered=(stdbuf -o0)
  unset -v app_token_head_coproc app_token_head_in app_token_head_out app_token_head_line
fi
unset -v app_token_head_probe

# The defaults, which a run that sets no input takes: the App ids of the legion-implementer and
# legion-reviewer Apps, and the name of the secrets broker github-app secret the implement App's
# tokens come from, which is agent tier. The review App has no default credential, since the only
# credential it takes is a key (see the header). A deployment with Apps of its own passes them
# through the inputs; no other deployment's ids or names belong here.
default_implement_app_id=5341240
default_implement_app_secret=LEGION_IMPLEMENT_APP
default_review_app_id=5341243

# app_key_squeezed VALUE prints VALUE with every whitespace character, one surrounding quote of
# either kind and a leading UTF-8 byte-order mark taken out: the text the key guard below judges,
# so that an indented paste, a quoted one and one a text editor marked are read as the value they
# carry. It prints its argument and reads nothing else.
app_key_squeezed() {
  local value=${1//[[:space:]]/}
  value=${value#$'\xEF\xBB\xBF'}
  value=${value#[\"\']}
  value=${value%[\"\']}
  printf '%s' "${value#$'\xEF\xBB\xBF'}"
}

# require_app_inputs ROLE... reads each ROLE's inputs (implement, review) and fails naming an input
# that is set but malformed, never echoing its value (an operator who pastes a credential where its
# name or its command goes must not see it in the transcript). It sets <role>_app_id from
# LEGION_E2E_<ROLE>_APP_ID for each role, defaulting to the id above: 1 to 12 digits. For implement
# it sets implement_app_secret from LEGION_E2E_IMPLEMENT_APP_SECRET, defaulting to the name above:
# letters, digits and underscores, so it is safe in the token command's shell text and as a
# variable name. For review it sets review_app_key_command from LEGION_E2E_REVIEW_APP_KEY_COMMAND,
# which has no default, and refuses a value that is unset, empty, whitespace alone, or one of the
# common pastes of the key itself where its command goes: with the value's whitespace, its
# surrounding quotes and a leading byte-order mark taken out, one that starts with -----BEGIN (a
# PEM) and one that is base64 characters alone (the shape of the key GH_REVIEW_APP_PRIVATE_KEY_B64
# held, below, which `base64` wraps at 76 columns), and a value with three or more lines that are
# 40 or more base64 characters alone once a quoting `>` is off the front of each (a PEM body line
# is 64 and a `base64` line 76), each refused by its own message, which names the input and prints
# no part of the value. Every other value is taken, `awk '/-----BEGIN/,/-----END/' <file>` and a
# command carrying one long base64 word among them, since arbitrary shell text has no shape every
# legitimate command has and no positive test can stand. A paste these refusals do not catch is
# run: `sh` echoes back the lines it cannot run, and app_token_mint_command's key mode relays that
# stderr through app_token_redact_key, which masks a PEM's armour and the body lines of a key at
# its usual width, but not every shape a key can arrive in (that relay says which). It also
# refuses a run that still exports the retired LEGION_E2E_REVIEW_APP_SECRET, whose broker secret
# that App no longer has.
# When this call reads both roles, one App id for both is refused, since the daemon refuses two
# roles on one App at boot. It records the roles it read for write_github_apps, and sets
# app_key_unsets: `-u NAME` for the implement App's broker secret name, when that role is read (the
# review App has no broker secret), for the implement default's, for LEGION_REVIEW_APP, the name of
# the broker secret that released the review App's tokens before its key replaced them, for
# GH_REVIEW_APP_PRIVATE_KEY_B64, the variable an older recipe's private_key_command read that key
# out of, for LEGION_E2E_REVIEW_APP_KEY_COMMAND, which this lib alone reads and whose value may be
# a key itself, and for every exported variable whose name holds PRIVATE_KEY, so a process the run
# starts inherits no App credential, the proof human's included.
github_apps_read=()
app_key_unsets=()
require_app_inputs() {
  # The checks below use bracket ranges, which bash's =~ reads in the caller's locale: under a
  # UTF-8 locale [0-9] and [A-Za-z] admit fullwidth digits and accented letters. C keeps them ASCII.
  local LC_ALL=C
  local role upper id_var id_default id secret key_command key_squeezed key_line key_b64_lines name roles_read=()
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
      # The input is arbitrary shell text — a `cat` of a file, an `awk` between two markers, a
      # deployment's own secret-store client — so no shape every legitimate command has exists and
      # no positive test can stand. The guard therefore refuses the common pastes of the key
      # itself where its command goes and takes every other value: one that is a PEM, one that is
      # base64 of a PEM, and one with three or more lines that are 40 or more base64 characters
      # alone. Each test reads its text with the whitespace, the surrounding quotes and a leading
      # byte-order mark taken out, and the line test one leading `>` as well, so an indented paste,
      # a quoted one, one a mail client quoted line by line, one behind a mark an editor left and
      # a key wrapped at the 76 columns base64 prints by default are the same value here. The `>`
      # earns its own strip: `sh` reads a line that starts with one as a redirection and creates
      # an empty file named after the line in the working directory, so a quoted paste is turned
      # back here rather than written to disk under names no relay can mask. One `>` is taken off,
      # not every one: a paste quoted twice (`>> ` before each line) still starts each line with a
      # `>` here, is taken, and writes its lines to disk the same way.
      # A command that names a PEM marker is still a command (`awk '/-----BEGIN/,/-----END/'
      # <file>`), and so is one carrying a long base64 word: a PEM body line is 64 base64
      # characters and a base64 line 76, and no realistic command has three such lines. A paste
      # none of these catches is run, and `sh` echoes back the lines of it that it cannot run:
      # that stderr reaches the run's transcript through app_token_redact_key, which masks a PEM's
      # armour and the body lines of a key at its usual width, but not a line of 16 characters or
      # fewer. No refusal prints any part of the value.
      key_squeezed=$(app_key_squeezed "$key_command")
      [[ "$key_squeezed" != -----BEGIN* ]] ||
        fail "LEGION_E2E_REVIEW_APP_KEY_COMMAND must name a command that prints the review App's private key, not the key itself; its value is not printed"
      [[ ! "$key_squeezed" =~ ^[A-Za-z0-9+/=]{200,}$ ]] ||
        fail "LEGION_E2E_REVIEW_APP_KEY_COMMAND is base64 characters and whitespace alone, which is a key pasted where its command goes (the GH_REVIEW_APP_PRIVATE_KEY_B64 form an older recipe used, in one word or wrapped), not a command that prints a key; its value is not printed"
      key_b64_lines=0
      while IFS= read -r key_line; do
        key_line=$(app_key_squeezed "$key_line")
        key_line=${key_line#>}
        [[ "$key_line" =~ ^[A-Za-z0-9+/=]{40,}$ ]] || continue
        key_b64_lines=$((key_b64_lines + 1))
        [ "$key_b64_lines" -lt 3 ] || break
      done <<<"$key_command"
      [ "$key_b64_lines" -lt 3 ] ||
        fail "LEGION_E2E_REVIEW_APP_KEY_COMMAND has three or more lines that are 40 or more base64 characters alone, which is a key pasted where its command goes (a PEM body line is 64 such characters, a wrapped base64 line 76), not a command that prints a key; its value is not printed"
      review_app_key_command=$key_command
    else
      # The implement App is the one role whose credential is a broker secret's name, and the role
      # says so: the review App's credential is its key, whatever this deployment's shell exports.
      secret=${LEGION_E2E_IMPLEMENT_APP_SECRET-$default_implement_app_secret}
      [[ "$secret" =~ ^[A-Za-z_][A-Za-z0-9_]*$ && ${#secret} -le 64 ]] || fail "LEGION_E2E_IMPLEMENT_APP_SECRET must be the name of a secrets broker github-app secret (letters, digits and underscores, at most 64), not a credential; its value is not printed"
      implement_app_secret=$secret
    fi
    roles_read+=("$role")
  done
  if [[ " ${roles_read[*]} " == *" implement "* && " ${roles_read[*]} " == *" review "* ]]; then
    [ "$implement_app_id" != "$review_app_id" ] ||
      fail "LEGION_E2E_IMPLEMENT_APP_ID and LEGION_E2E_REVIEW_APP_ID are both $implement_app_id: the workflow needs two Apps"
  fi
  github_apps_read=("${roles_read[@]}")
  app_key_unsets=()
  # The implement App's broker secret name is the only name in the list below that depends on the
  # role that was read, since its releases are the only credential of a run that comes from the
  # broker: the names after it are stripped whatever roles this call read, and no variable of the
  # caller's shell is read for any of them.
  if [[ " ${roles_read[*]} " == *" implement "* ]]; then
    app_key_unsets+=(-u "$implement_app_secret")
  fi
  # The implement default; LEGION_REVIEW_APP, the broker secret that released the review App's
  # tokens before its key replaced them; GH_REVIEW_APP_PRIVATE_KEY_B64, the variable an older
  # recipe's private_key_command read that key out of; and the review role's own key command. A
  # stale export of any of the first three must not reach a process the run starts, and neither
  # must the fourth: the refusals above turn back the common pastes of a key, but a value that
  # holds one and still runs — a one-line `printf` of a PEM — is taken, so that input may be a key
  # itself, and this lib is the only thing that reads it.
  for name in "$default_implement_app_secret" LEGION_REVIEW_APP GH_REVIEW_APP_PRIVATE_KEY_B64 \
    LEGION_E2E_REVIEW_APP_KEY_COMMAND; do
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
# credential commands carry it in their shell text. It guards every directory a credential file of
# the run goes into: mint_app_tokens' releases and key alike, and write_github_apps' --token-dir
# and --key-dir.
app_token_dir_ok() {
  local LC_ALL=C
  [[ "$2" =~ ^[A-Za-z0-9_./-]+$ ]] ||
    fail "$1: the credential directory '$2' holds a character other than letters, digits and _ . / -, which the credential commands could not carry in their shell text"
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

# The refresher, for a daemon that can run neither agent-secrets nor anything else of the devbox's
# (the Claude Code sandbox proof's container under bridge). It mints, here, each App's credential
# into DIR, one directory a role:
#   DIR/implement/_all.json           the release of every installation (no owner)
#   DIR/implement/<owner>.json        OWNER's installation alone
#   DIR/implement/<owner>.read.json   OWNER's installation, read-only
#   DIR/review/key.pem                the review App's private key, written once by mint_app_tokens
# owner lowercased, each file 0600 and each directory 0700, which is the layout the commands
# write_github_apps --token-dir DIR writes read. A release, and the key with it, passes from its
# command's `sh` child to a temporary file of the same directory, then mv renames it over the file,
# so a reader sees the whole old file or the whole new one, and a mint that fails or prints nothing
# leaves no temporary file and the last good file as it was. No token and no key passes through
# this shell.
#
# app_token_mint DIR mints every release file of the implement App once, for the owners
# app_token_owners lists, and returns 1 at the first failure with app_token_mint_failed naming that
# App by its role and id, its github-app secret, the file and agent-secrets' exit status, never a
# token. That App is the one whose credential the broker holds; the review App's key is
# app_token_mint_key's, minted once rather than refreshed. agent-secrets' own stderr (the request a
# grant waits on) reaches this shell's stderr as it is written and whole but for a GitHub token
# form: its relay is app_token_redact, so the approval link of a request waiting on a person is
# read where it is printed.
app_token_owners=()
# app_token_implement is set when the last mint_app_tokens minted the implement App's releases,
# which is what the refresher mints again.
app_token_implement=
app_token_mint_failed=
# app_token_mint_reason STATUS RAN WHOLE SECRET [NOTE] prints why app_token_mint_command exited
# STATUS, which is the reason both minters below name their failure with, so a new status, or a fix
# to one of these lines, reaches the release mints and the key mint alike. RAN is what was run
# (agent-secrets, the key command), WHOLE what a whole output of it is (a release, a private key),
# SECRET what it prints and this reason never does (a token, a key), and NOTE, when given, follows
# the exit status of a status not listed here. It is called for a non-zero status alone.
app_token_mint_reason() {
  local status=$1 ran=$2 whole=$3 secret=$4 note=${5-}
  case "$status" in
    125) printf '%s printed nothing' "$ran" ;;
    126) printf '%s printed more than %s bytes, which no %s is' "$ran" "$app_token_release_limit" "$whole" ;;
    123) printf '%s wrote more than %s bytes to stderr' "$ran" "$app_token_release_limit" ;;
    *) printf '%s exited %s%s; no %s is printed' "$ran" "$status" "${note:+ $note}" "$secret" ;;
  esac
}
app_token_mint() {
  local dir=$1 owner ro file status
  app_token_mint_failed=
  app_token_dir_made implement "$dir/implement" || return 1
  for owner in "" "${app_token_owners[@]}"; do
    owner=${owner,,}
    for ro in "" 1; do
      # The release of every installation is never asked read-only.
      [ -n "$owner" ] || [ -z "$ro" ] || continue
      file=$dir/implement/${owner:-_all}${ro:+.read}.json
      status=0
      app_token_mint_file "$file" "$owner" "$ro" "$implement_app_secret" || status=$?
      [ "$status" != 0 ] || continue
      app_token_mint_failed="the implement App (App $implement_app_id, github-app secret $implement_app_secret) gave no release for $file: $(app_token_mint_reason "$status" agent-secrets release token)"
      return 1
    done
  done
}
# app_token_redact copies its stdin to its stdout, line by line as it arrives, with every GitHub
# token form in it replaced by [redacted], as the daemon redacts a token command's stderr
# (internal/appauth, tokenPattern): gh[opsur]_ or github_pat_ and 16 or more characters after it,
# which is the 20 and the 27 of the key relay below. It is the relay of the implement App's
# release mints and of nothing else, and its channel is agent-secrets' own stderr: the broker's
# own words, among them what a request waits on and the link a person approves a human-tier grant
# by. Nothing of the operator's is echoed back there — what runs is app_token_command's text,
# which names a secret and never a key — so a token is all there is to mask, and the rest of the
# line, approval link and path and identifier alike, reaches the operator whole. That is what the
# channel is for: a command that failed tells the operator what went wrong in its own words alone,
# and this script names no secret store of theirs to tell them with. Key material is masked on the
# key command's relay below, which is the one channel a key can be pasted into; keeping the two
# apart is the point of the two functions.
app_token_redact() {
  LC_ALL=C sed -u -E 's/(gh[opsur]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,})/[redacted]/g'
}
# app_token_redact_key is the relay of that one channel: the stderr of the operator's key command,
# which app_token_mint_key runs through app_token_mint_command's key mode. A value that was a key
# itself rather than a command reaches `sh` there, and `sh` echoes back the lines it cannot run,
# so this relay masks key material as well as the token forms above, and what it masks is settled
# by the text of a line and not by the shape of the value that produced it. Besides those forms it
# masks:
#   a PEM armour marker, -----BEGIN… or -----END…, whole or cut to its first word by the shell
#   that echoed the line;
#   a run of base64 characters too long for an ordinary message to carry: 20 or more of them when
#   the run holds an upper-case letter or a digit, as the base64 of a key does (a PEM body line is
#   64 of those characters, its last line fewer, and a `base64` line 76), and 40 or more
#   otherwise, which leaves a lower-case path in a message as it is. A `/` is a base64 character,
#   so a long path of letters, digits and slashes alone is masked too, and a path or an identifier
#   in the command's own error message can come back partly masked.
# Everything else of the line is passed on, for the reason the release relay passes every word.
# What it does not mask is a run shorter than those bounds, and a key can end in one: a PEM's last
# body line is 4*ceil((DER length mod 48)/3) characters, so a key whose DER length leaves 12 bytes
# or fewer in its last group ends in a line of 16 characters or fewer, which this relay passes.
# Measured, the RSA-2048 keys GitHub issues end in 24 characters (PKCS#8) to 56 (PKCS#1), RSA-3072
# the same, RSA-4096 32 and 60, an EC P-256 key 36 (SEC1) or 56 (PKCS#8), and RSA-1024 PKCS#8 keys
# ended in 12 and in 16; a key of its own carries a DER length of its own, which moves each of
# these a few characters. A key re-wrapped below 20 columns, or broken up every 16 characters,
# passes line by line for the same reason. A fragment that short rebuilds no key, and the bound
# stays at 20 rather than dropping lower, where the mask would take the ordinary words of a message
# instead.
#
# The armour goes first, in one gsub: no other rule here can match a `-`, so no match of theirs
# overlaps one of its. What is left is walked by its runs of credential characters — one split for
# the runs, one for the text between them, which together are the line — so the work is linear in
# the line's length for every run holding no `_`, which is one base64 run and is judged whole. A
# run holding a `_` is walked match by match for the token forms in it, which is quadratic in that
# run's length; the 1 MiB bound on the stream caps it, at minutes for one megabyte-long run of
# `a_`. The lengths are counted rather than written as regex bounds: mawk, which is awk on
# a Debian devbox, matches a bracket range followed by {n,} as though the bound were a `+`.
app_token_redact_key() {
  LC_ALL=C awk "${app_token_awk_interactive[@]}" '
    function masked(run) {
      if (run ~ /^gh[opsur]_/) return length(run) >= 20 ? "[redacted]" : run
      if (run ~ /^github_pat_/) return length(run) >= 27 ? "[redacted]" : run
      if (length(run) >= 40 || (length(run) >= 20 && run ~ /[A-Z0-9]/)) return "[redacted key]"
      return run
    }
    function masked_run(piece,   out) {
      if (index(piece, "_") == 0) return masked(piece)
      out = ""
      while (match(piece, /gh[opsur]_[A-Za-z0-9]+|github_pat_[A-Za-z0-9_]+|[A-Za-z0-9+\/=]+/)) {
        out = out substr(piece, 1, RSTART - 1) masked(substr(piece, RSTART, RLENGTH))
        piece = substr(piece, RSTART + RLENGTH)
      }
      return out piece
    }
    { line = $0
      gsub(/-----(BEGIN|END)[ A-Za-z0-9]*-----|-----(BEGIN|END)[A-Za-z0-9-]*/, "[redacted key]", line)
      runs = split(line, run, /[^A-Za-z0-9+\/=_]+/)
      gaps = split(line, gap, /[A-Za-z0-9+\/=_]+/)
      n = runs > gaps ? runs : gaps
      if (line ~ /^[A-Za-z0-9+\/=_]/)
        for (i = 1; i <= n; i++) printf "%s%s", gap[i], masked_run(run[i])
      else
        for (i = 1; i <= n; i++) printf "%s%s", masked_run(run[i]), gap[i]
      printf "\n"; fflush() }'
}
# app_token_stderr_bound LIMIT MARKER passes on whole lines of its stdin while their bytes stay
# within LIMIT. Its input is cut at LIMIT+1 bytes by the caller, so the line that crosses LIMIT,
# which may end in a fragment of a token, is dropped, nothing after it is read, and MARKER is
# created to say the stream overflowed. Every line it passes is whole, so the relay after it sees
# each token, and each run of key material, whole.
app_token_stderr_bound() {
  LC_ALL=C awk "${app_token_awk_interactive[@]}" -v limit="$1" -v marker="$2" '
    { total += length($0) + 1
      if (total > limit) { printf "" > marker; exit }
      print; fflush() }'
}
# app_token_mint_command FILE COMMAND [MODE] writes COMMAND's stdout to FILE atomically, which is
# how a release and the review App's key alike reach a file. COMMAND runs under `timeout` (35
# minutes: agent-secrets' 30-minute approval wait and a margin; killed 10 s after) with no stdin.
# Its stdout is cut at app_token_release_limit+1 bytes into a temporary file of FILE's own
# directory, which mv renames over FILE. Its stderr reaches the run's as it is written, whole
# lines only, through app_token_stderr_bound and one of the two relays, at most
# app_token_release_limit bytes: app_token_redact, which masks GitHub token forms alone, or, when
# MODE is `key`, app_token_redact_key, which masks key material too. Only the key command is given
# that mode, since it is the only COMMAND an operator can paste a key into. It returns COMMAND's
# non-zero status (124 when it timed out), 126 when it printed more than the limit, 123 when it
# wrote more than the limit to stderr, or 125 when it succeeded and printed nothing; either way it
# leaves no temporary file and FILE as it was. What COMMAND prints passes from its stdout to the
# file and enters no shell variable and no argv.
app_token_release_limit=1048576
app_token_mint_command() {
  local file=$1 command=$2 mode=${3-} tmp overflow status=0 relay=app_token_redact
  [ "$mode" != key ] || relay=app_token_redact_key
  tmp=$(umask 077 && mktemp "$(dirname -- "$file")/.mint.XXXXXXXX") || return 1
  overflow=$tmp.stderr-overflow
  (
    umask 077
    set -o pipefail
    {
      timeout --kill-after=10s 35m sh -c "$command" \
        2>&1 1>&3 3>&- </dev/null |
        "${app_token_head_unbuffered[@]}" head -c "$((app_token_release_limit + 1))" |
        app_token_stderr_bound "$app_token_release_limit" "$overflow" |
        "$relay" >&2
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
  # read-only flag alone, so it must reach every repository of that installation.
  LEGION_GITHUB_OWNER=$owner LEGION_GITHUB_REPOS='' LEGION_GITHUB_READ_ONLY=$ro \
    app_token_mint_command "$file" "$(app_token_command "$secret")"
}
# app_token_mint_key DIR writes the review App's private key, once, to DIR/review/key.pem (mode
# 0600, in a directory of mode 0700) by running the operator's LEGION_E2E_REVIEW_APP_KEY_COMMAND
# through app_token_mint_command in its key mode: here, on the driver, in the caller's own
# environment, so that command may read the operator's HOME and their own gcloud or kubectl
# configuration. This is the one call that asks for the key masking, because this is the one
# command whose input an operator can paste a key into. The daemon never runs it and reads this
# file instead, and both ways to that file come through here — mint_app_tokens, and
# write_github_apps --key-dir. It returns 1 with app_token_mint_failed naming the App by its role
# and id, the file and what went wrong, and never a byte the command printed; the command's own
# stderr has already reached the run's stderr, through app_token_redact_key.
app_token_mint_key() {
  local dir=$1 file=$1/review/key.pem status=0
  app_token_mint_failed=
  app_token_dir_made review "$dir/review" || return 1
  app_token_mint_command "$file" "$review_app_key_command" key || status=$?
  [ "$status" != 0 ] || return 0
  app_token_mint_failed="the review App (App $review_app_id, LEGION_E2E_REVIEW_APP_KEY_COMMAND) gave no private key for $file: $(app_token_mint_reason "$status" "the key command" "private key" key "(124 is its 35-minute timeout)")"
  return 1
}

# mint_app_tokens DIR OWNER... records the OWNERs the run serves (app_token_owners) and mints,
# synchronously, every release file of the implement App when the last require_app_inputs read
# that role (app_token_implement, the releases the refresher mints again) and, when that call read
# the review role, that App's private key once into DIR/review/key.pem. It fails, naming the App by
# its role and id, at the first mint that fails or prints nothing. A secret whose grant waits on a
# person's approval waits here, as long as agent-secrets does. DIR must exist; the caller owns it
# and removes it, the files with it.
mint_app_tokens() {
  local dir=$1 owner
  shift
  app_token_dir_ok mint_app_tokens "$dir"
  [ -d "$dir" ] || fail "mint_app_tokens: the token directory $dir is not a directory"
  [ "${#github_apps_read[@]}" -gt 0 ] || fail "mint_app_tokens: require_app_inputs has not run"
  for owner in "$@"; do
    [[ "$owner" =~ ^[A-Za-z0-9][A-Za-z0-9-]*$ ]] ||
      fail "mint_app_tokens: '$owner' is not a GitHub account login"
  done
  app_token_owners=("$@")
  # The implement App's releases are what there is to mint, and to mint again as they expire; the
  # review App has a key command instead, run once below. The role read decides which is minted,
  # and the implement App's secret name is the one the broker is asked for.
  app_token_implement=
  if [[ " ${github_apps_read[*]} " == *" implement "* ]]; then
    app_token_implement=1
    app_token_mint "$dir" || fail "$app_token_mint_failed"
  fi
  if [[ " ${github_apps_read[*]} " == *" review "* ]]; then
    app_token_mint_key "$dir" || fail "$app_token_mint_failed"
  fi
}

# start_app_token_refresher DIR starts the background refresher, which mints every release file of
# the last mint_app_tokens (the implement App's, for its owners, when that call minted them:
# app_token_implement and app_token_owners) again every app_token_refresh_seconds (15 minutes: an
# installation token lasts an hour, and the daemon runs its command again once fewer than 20
# minutes remain on a lease), and sets app_token_refresher to its pid. It never touches the review
# App's key file: a private key does not expire, and that App has no broker secret to ask for. A
# failed mint prints one line naming the App and keeps the last good files; the next round tries
# again. Run mint_app_tokens first, so the first releases and their failures are the
# prerequisites'. stop_app_token_refresher kills it.
app_token_refresh_seconds=900
app_token_refresher=
start_app_token_refresher() {
  local dir=$1 driver=$$
  # Nothing expires when the implement role's releases were never minted, so there is nothing to
  # start.
  [ -n "$app_token_implement" ] || return 0
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
      app_token_mint "$dir" ||
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

# write_github_apps (--token-dir DIR | --key-dir DIR) ROLE... writes $work/github-apps.yaml, the
# `github_apps` block of legion.yaml for each ROLE the last require_app_inputs read, or fails
# naming a role it did not read, before the file is opened. Each role carries its app_id and
# exactly one credential field, which is what the daemon admits
# (packages/daemon/internal/config/stage3.go, readGitHubApp): the implement App a token_command,
# app_token_command's, and the review App a private_key_command that reads the run's own key file,
# `cat -- 'DIR/review/key.pem'`, in any POSIX sh (busybox's included). The operator's key command
# is never that field, and the daemon never runs it, so a call that writes the review role names
# the directory that file goes in, one of two ways, or is refused having written nothing:
#   --key-dir DIR    this call runs LEGION_E2E_REVIEW_APP_KEY_COMMAND once, here on the driver, in
#                    the caller's own environment, into DIR/review/key.pem (mode 0600, in a
#                    directory of mode 0700, through app_token_mint_key). The implement App's
#                    token_command still runs agent-secrets, which that mode's daemon can run.
#   --token-dir DIR  the directory mint_app_tokens wrote, which already holds that key file and the
#                    releases the implement App's token_command then reads instead of running
#                    agent-secrets: DIR/implement/${owner:-_all}${LEGION_GITHUB_READ_ONLY:+.read}.json
#                    with the owner lowercased, with `cat --`. That form serves what the refresher
#                    minted, which is keyed by owner and the read-only flag alone, so it ignores
#                    LEGION_GITHUB_REPOS and answers a token over every repository of the owner's
#                    installation: the refresher mints one release an owner rather than one per
#                    scope a role asks for.
# The two name one key file by two routes, so a call naming both is refused. Either DIR must be a
# path the caller's cleanup removes unconditionally, on every exit, since the App's private key is
# in it: not the evidence directory, and not a workspace a failing run keeps for debugging — the
# rule lib/install-key-file-model-route.sh's own --key-dir states for the same reason. The block
# names commands, never a token and never a key.
write_github_apps() {
  local LC_ALL=C
  local role id_var token_dir='' key_dir='' key_file_dir field command block="github_apps:"$'\n'
  while [ "${1-}" = --token-dir ] || [ "${1-}" = --key-dir ]; do
    [ "$#" -ge 2 ] || fail "write_github_apps: $1 names no directory"
    [ -z "$token_dir" ] && [ -z "$key_dir" ] ||
      fail "write_github_apps: --token-dir and --key-dir name the review App's key file two ways; pass one"
    case "$1" in
      --token-dir) token_dir=$2 ;;
      *) key_dir=$2 ;;
    esac
    app_token_dir_ok write_github_apps "$2"
    shift 2
  done
  for role in "$@"; do
    [[ " ${github_apps_read[*]} " == *" $role "* ]] ||
      fail "write_github_apps: the $role App's inputs are unread: require_app_inputs $role has not run"
  done
  # The directory the review App's key file is in: written here under --key-dir, by mint_app_tokens
  # under --token-dir, and read by the private_key_command either way.
  key_file_dir=${token_dir:-$key_dir}
  if [[ " $* " == *" review "* ]]; then
    [ -n "$key_file_dir" ] ||
      fail "write_github_apps: the review App's key must be resolved into a directory the caller's cleanup removes unconditionally, on every exit: pass --key-dir DIR, which runs LEGION_E2E_REVIEW_APP_KEY_COMMAND once, here, in the caller's own environment, or --token-dir DIR, the directory mint_app_tokens already wrote that key into. The daemon never runs the operator's command: the private_key_command written here is a cat of that file"
    # The key is resolved before a line of the block is written, so a key command that fails leaves
    # no block behind either.
    if [ -n "$key_dir" ]; then
      app_token_mint_key "$key_dir" || fail "$app_token_mint_failed"
    fi
  fi
  for role in "$@"; do
    id_var=${role}_app_id
    if [ "$role" = review ]; then
      field=private_key_command
      # The single quotes hold the path to one word: app_token_dir_ok admits no quote in DIR.
      command=$(printf "cat -- '%s/review/key.pem'" "$key_file_dir")
    else
      field=token_command
      if [ -n "$token_dir" ]; then
        # shellcheck disable=SC2016 # the expansions are the token command's own, run by its sh
        command=$(printf 'owner=$(printf %%s "$LEGION_GITHUB_OWNER" | tr "[:upper:]" "[:lower:]"); case $owner in *[!a-z0-9-]*) echo "no release file for owner $owner" >&2; exit 2;; esac; cat -- '\''%s/%s/'\''"${owner:-_all}${LEGION_GITHUB_READ_ONLY:+.read}.json"' "$token_dir" "$role")
      else
        # The implement App is the one role whose token_command asks the broker, so its secret is
        # named here rather than reached through the role.
        command=$(app_token_command "$implement_app_secret")
      fi
    fi
    # jq quotes the command as a JSON string, which YAML reads as a double-quoted scalar.
    block+=$(printf '  %s:\n    app_id: "%s"\n    %s: %s' "$role" "${!id_var}" "$field" \
      "$(printf '%s' "$command" | jq -Rs .)")$'\n'
  done
  printf '%s' "$block" >"$work/github-apps.yaml"
}

# app_key_left [--check-line] DIR returns 1 when DIR is still there, and 0 when it is gone. It
# removes nothing itself: DIR is the directory write_github_apps --key-dir resolved the review
# App's private key into, the only thing that writes it, and every proof calls this at the end of
# its teardown to say whether the removals that teardown makes took the directory. A private key
# never expires, so a run that leaves one behind leaves a usable credential on the driver,
# whatever else its teardown did, and fails itself for it. The failure is the proof's own line:
# `FAIL cleanup: …` on stderr, as the sandbox proof's remove_app_tokens prints for the credential
# files it could not remove, or, with --check-line, the `CHECK app-key: FAIL: …` line on stdout
# that stage4b-sandbox-tree.sh writes its teardown checks in. The caller decides what the failure
# costs the run: an exit, a status, or a named teardown check.
app_key_left() {
  local check=
  [ "${1-}" != --check-line ] || { check=1; shift; }
  [ -e "$1" ] || return 0
  if [ -n "$check" ]; then
    echo "CHECK app-key: FAIL: the review App's private key remains in $1"
  else
    printf "FAIL cleanup: the review App's private key remains in %s\n" "$1" >&2
  fi
  return 1
}

# check_github_apps ROLE... proves each ROLE's App of $work/github-apps.yaml against GitHub and sets
# github_app_logins to lib/app-logins.ts's answer, {"<role>":"<slug>[bot]",…}: each App's
# credential command is run as the daemon runs it, and for the review App that is the very `cat`
# of the run's own key file the daemon will run, so no environment of the operator's stands
# between what this check proves and what the daemon loads; GraphQL's viewer under an
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
