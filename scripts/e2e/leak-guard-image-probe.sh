#!/usr/bin/env bash
# The probe of scripts/e2e/leak-guard-image-job.sh, run inside the worker image by a Kubernetes
# Job. One argument: the arm — `head` for the image under test, whose guard runs the private-name
# rule file LEGION_PRIVATE_NAME_RULES_FILE names and no rule of its own, `control` for the base
# branch's published image, built before that change, which carries the same wrappers and runs the
# rule file embedded in its `legion` whatever that variable names — so one probe serves both and
# the expectations are stated once. Every check prints `CHECK <name>: got [x] want [y]`, and the
# last line is `JOB-RESULT pass` or `JOB-RESULT fail`, which is the verdict the harness reads; a
# Job's own phase says only that the container exited 0 (skills/legion-worker/references/tools.md).
#
# It is no proof of its own and nothing runs it from a checkout: a Job holds no checkout, so the
# harness beside this file reads this text in and writes it into the Job's command, which is the
# only way the probe reaches the container. The two files are one recipe; read them together, and
# change neither alone.
#
# No `set -e`: a refused push is a non-zero status this reads rather than an end of the run. Every
# step that must succeed goes through `must`, which ends the run with `JOB-RESULT fail` when it
# does not, so a probe that could not build its fixtures never reads as a pass.
set -u

arm=${1:-}
case "$arm" in
  head | control) ;;
  *) echo "probe: the arm must be head or control, not '${arm}'"; echo "JOB-RESULT fail"; exit 2 ;;
esac

scratch=/scratch

# The host every refusable case carries. It is of the shape a Tailscale MagicDNS name takes and is
# built here from two pieces, so no line of this file carries such a name whole: the words are
# invented and name no machine and no tailnet, and they are not the reserved `example-tailnet`
# name, which the rules embedded in the control's `legion` pass and which would prove nothing.
tailnet_label=mossy-lantern
magicdns_suffix=ts.net
refusable_host="quill-badger.${tailnet_label}.${magicdns_suffix}"
# The class a refusal has to name, or the push was refused for some other reason and the case
# proved nothing. Each arm's rules call the host by a class of their own: the head's are the rule
# file this Job writes and names (below), whose one rule is rule_class, and the control's are the
# rules embedded in its `legion`, which call it embedded_class.
rule_class='names the invented tailnet host of this probe'
embedded_class='names a Tailscale MagicDNS host'
if [ "$arm" = head ]; then
  refusal_class=$rule_class
else
  refusal_class=$embedded_class
fi

clean_domain=example.invalid
clean_line='a line of the probe that names nothing'
# What each refusable case writes, in one place, because the head's refusal must name the line of
# the record the host stands on (match_line_of): the commit message, the author identity, and the
# line added to the tracked file.
refusable_message="probe: the box at $refusable_host answered"
refusable_name='Probe Host'
refusable_email="probe@$refusable_host"
refusable_line="the probe box answers at $refusable_host"

# The image's real binaries, behind the wrappers. The build's last step refuses an image whose git
# is anywhere but /usr/bin/git and whose jj is anywhere but /usr/local/bin/jj, so these are the two
# the wrappers run. They are used here for the reads this probe makes of the remote and for seeding
# a remote before a measured push: a seed is not a write under test, and a read through the wrapper
# would put the wrapper in the path of its own observation.
real_git=/usr/bin/git

checks=0
mismatches=0

check() { # check <name> <got> <want>
  checks=$((checks + 1))
  if [ "$2" = "$3" ]; then
    echo "CHECK $1: got [$2] want [$3]"
  else
    mismatches=$((mismatches + 1))
    echo "CHECK $1: got [$2] want [$3] MISMATCH"
  fi
}

die() {
  echo "SETUP FAILED: $*"
  echo "JOB-RESULT fail"
  exit 2
}

must() {
  if ! "$@"; then
    die "the step \`$*\` failed"
  fi
}

# says <text> <fragment> -> yes|no. The fragment is quoted, so it is matched literally.
says() {
  case "$1" in
    *"$2"*) echo yes ;;
    *) echo no ;;
  esac
}

echo "== the image this Job is running"
echo "arm: $arm"
echo "uname: $(uname -a)"
echo "id: $(id)"
echo "processors: $(nproc)"
legion version || echo "legion version: exit $?"
"$real_git" --version
/usr/local/bin/jj --version

echo
echo "== the wrappers the image puts first on PATH"
# On both arms' images the two wrappers lead PATH with the real binaries behind them, and the
# pre-push hook is root's in a directory no role can write: the base branch's image carries them
# too, so the arms part only where the rules their `legion` runs part, which is the change.
check "the git on PATH" "$(command -v git || echo 'no git at all')" /opt/legion/bin/git
check "the jj on PATH" "$(command -v jj || echo 'no jj at all')" /opt/legion/bin/jj
check "the pre-push hook" "$(stat -c '%u:%g %a' /opt/legion/githooks/pre-push 2> /dev/null || echo 'no hook')" '0:0 755'
if legion leak-check --help > /dev/null 2>&1; then
  check "legion leak-check" present present
else
  check "legion leak-check" absent present
fi

echo
echo "== the run's own environment, all of it on the Job's emptyDir"
export HOME=$scratch/home
export XDG_CONFIG_HOME=$scratch/config
export TMPDIR=$scratch/tmp
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=$scratch/gitconfig
export JJ_USER='Probe Clean'
export JJ_EMAIL="probe@$clean_domain"
export GIT_AUTHOR_NAME='Probe Clean'
export GIT_AUTHOR_EMAIL="probe@$clean_domain"
export GIT_COMMITTER_NAME='Probe Clean'
export GIT_COMMITTER_EMAIL="probe@$clean_domain"
must mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$TMPDIR" "$scratch/runs" "$scratch/empty"
: > "$GIT_CONFIG_GLOBAL"
echo "HOME=$HOME XDG_CONFIG_HOME=$XDG_CONFIG_HOME TMPDIR=$TMPDIR"

echo
echo "== the stand-in shared public leak check this Job writes for itself"
# A Job runs no launcher, so the daemon pushes it no copy of the private shared check. These
# stand-ins are what PUBLIC_LEAK_CHECK names instead. The clearing one passes every call, so
# wherever a case keeps it, the private-name rules alone decide the verdict; the other two are
# k7's two shared-check failures.
check_dir=$scratch/leak-check
must mkdir -p "$check_dir"
clears=$check_dir/public-leak-check.sh
internal_error=$check_dir/public-leak-check-internal-error.sh
cat > "$clears" <<'SH'
#!/usr/bin/env bash
# Clears every call: the private-name rules are what decide each verdict that keeps this check.
exit 0
SH
cat > "$internal_error" <<'SH'
#!/usr/bin/env bash
# k7's second shared-check failure: a check that could not finish.
echo "LEAK CHECK ERROR: this stand-in could not finish" >&2
exit 2
SH
must chmod +x "$clears" "$internal_error"
# The directory is not on PATH, so the "no check resolves" variant below really resolves none:
# the guard looks at PUBLIC_LEAK_CHECK, then public-leak-check.sh on PATH, then a pointer file
# under the configuration home.
check "no check on PATH" "$(command -v public-leak-check.sh > /dev/null 2>&1 && echo found || echo none)" none
echo "the clearing stand-in: $clears"
echo "the internal-error stand-in: $internal_error"

echo
echo "== the private-name rule files this Job writes for itself"
# A Job runs no launcher, so no daemon hands it the deployment's rule file, as one hands it to each
# role's launch. This Job writes one of its own instead, which k1's cases name in
# LEGION_PRIVATE_NAME_RULES_FILE: one rule, for the invented host above and no other. Its pattern
# is the host with each full stop a one-character class, so it matches the host's own text and no
# line of this file carries the host whole. Its description, rule_class, holds no colon, because a
# report line parts the record, the class and what matched with colons. The two other paths are
# the rule file's two setup failures: a path at which no file is, and a file that is no TOML.
rules_var=LEGION_PRIVATE_NAME_RULES_FILE
rules_dir=$scratch/rules
must mkdir -p "$rules_dir"
rule_file=$rules_dir/private-name-rules.toml
missing_rule_file=$rules_dir/no-such-rule-file.toml
unparseable_rule_file=$rules_dir/unparseable-rules.toml
refusable_pattern=${refusable_host//./[.]}
cat > "$rule_file" <<TOML
title = "the leak-guard image probe's own rules"

[[rules]]
id = "probe_host_rule"
description = "$rule_class"
regex = '''$refusable_pattern'''
TOML
printf '%s\n' 'this is = = no TOML [' > "$unparseable_rule_file"
must test -s "$rule_file"
must test -s "$unparseable_rule_file"
check "no file at the missing rule file's path" "$([ -e "$missing_rule_file" ] && echo present || echo absent)" absent
sed 's/^/      the rule file | /' "$rule_file"
echo "the missing rule file: $missing_rule_file"
echo "the unparseable rule file: $unparseable_rule_file"

echo
echo "== the shapes jj's own help enumerates"
# The shapes are read out of the image's own jj rather than from a list typed here, and a shape
# this probe drives that jj no longer has ends the run: a harness that silently stopped driving a
# shape would report a pass for a push nobody made.
help=$scratch/jj-git-push-help.txt
must jj git push --help > "$help" 2>&1
listed=$(grep -oE '^[[:space:]]+(-[a-zA-Z], )?--[a-z][a-z-]*' "$help" | grep -oE -- '--[a-z][a-z-]*' | sort -u)
echo "jj git push lists: $(echo "$listed" | tr '\n' ' ')"
for shape in --all --change --named --deleted --dry-run; do
  case $'\n'"$listed"$'\n' in
    *$'\n'"$shape"$'\n'*) ;;
    *) die "jj git push no longer lists $shape, which this harness drives" ;;
  esac
done
# The branch-name case under --change needs jj's own setting for the generated bookmark name,
# because --change names the bookmark itself.
if ! grep -q 'templates.git_push_bookmark' "$help"; then
  die "jj git push no longer names templates.git_push_bookmark, which this harness sets to drive the branch-name case under --change"
fi
echo "driven here: the bare shape, --all, --change, --named, --deleted"
# The other options that select what a push sends are named rather than silently left out, and
# they are read out of the same help: each selects updates the five driven shapes already carry
# to the same wrapper, so the wrapper sees nothing new in them.
undriven=""
for shape in --bookmark --revision --tag --tracked; do
  case $'\n'"$listed"$'\n' in
    *$'\n'"$shape"$'\n'*) undriven="$undriven $shape" ;;
  esac
done
echo "listed and not driven here:${undriven:- none}"

# branch_of <case> is the ref the case pushes under: the refusable host for the branch-name case,
# a name that holds nothing for every other.
branch_of() {
  case "$1" in
    branch) printf 'probe-%s' "$refusable_host" ;;
    *) printf 'probe-%s' "$1" ;;
  esac
}

# fragment_of <case> is the part of the report line a refusal must carry on either arm: the record,
# then the class this arm's rules call the host by. The records are the guard's own
# (packages/daemon/internal/leakguard/push.go), so the added-line case and the branch-name case
# are told apart here rather than being read as one refusal.
fragment_of() {
  case "$1" in
    message) printf 'message: %s' "$refusal_class" ;;
    author) printf 'author: %s' "$refusal_class" ;;
    line) printf 'probe.txt: %s' "$refusal_class" ;;
    branch) printf 'the ref name: %s' "$refusal_class" ;;
    *) printf '' ;;
  esac
}

# match_line_of <case> is the whole report line the head's refusal must carry: the record, the
# class of the Job's rule, the text it matched and the line of the record that text stands on, as
# the guard writes one (`<record>: <class>: matched "<match>" on line <n>: "<line>"`, RuleSet.scan
# in packages/daemon/internal/leakguard/rules.go). Each of these lines is printable ASCII with no
# double quote and no backslash, so Go's quoting of it is the line between double quotes. The
# message and the identity are one line each, the added line is the tracked file's second, after
# the base commit's one, and the ref is named in full.
match_line_of() {
  local record n line
  case "$1" in
    message) record=message n=1 line=$refusable_message ;;
    author) record=author n=1 line="$refusable_name <$refusable_email>" ;;
    line) record=probe.txt n=2 line=$refusable_line ;;
    branch) record='the ref name' n=1 line="refs/heads/$(branch_of branch)" ;;
    *) return 0 ;;
  esac
  printf '%s: %s: matched "%s" on line %s: "%s"' "$record" "$rule_class" "$refusable_host" "$n" "$line"
}

# failure_of <variant> is what a setup failure must say it met: the shared check's two failures in
# the guard's words for them (packages/daemon/internal/leakguard/check.go), and the rule file's two
# naming the file, the variable that named it and why it cannot be used (Guard.rules,
# packages/daemon/internal/leakguard/leakguard.go).
failure_of() {
  case "$1" in
    no-check) printf 'the shared public leak check could not be found' ;;
    internal-error) printf 'the shared public leak check could not finish (exit 2)' ;;
    rules-missing) printf 'the private-name rule file %s, which %s names, could not be opened' "$missing_rule_file" "$rules_var" ;;
    rules-unparseable) printf 'the private-name rule file %s, which %s names, is no rule file the guard can run' "$unparseable_rule_file" "$rules_var" ;;
    *) printf '' ;;
  esac
}

remote_state() { "$real_git" ls-remote "$1" 2> /dev/null | sort; }

ref_word() { # ref_word <ls-remote output> <branch>
  printf '%s\n' "$1" | awk -v want="refs/heads/$2" '$2 == want { seen = 1 } END { print seen ? "present" : "absent" }'
}

# A git clone with one clean commit holding the tracked file every case appends to.
git_base() { # git_base <dir>
  must "$real_git" init -q -b main "$1"
  printf '%s\n' "$clean_line" > "$1/probe.txt"
  must "$real_git" -C "$1" add probe.txt
  must "$real_git" -C "$1" commit -q -m 'probe: the base commit, which names nothing'
}

# The case's own commit, on top of that base. Exactly one thing about it names the host, which is
# what makes each case a case: the message, the identity, the added line, or nothing at all (the
# branch-name case, whose only hit is the ref, and the clean case, which has none).
git_case_commit() { # git_case_commit <dir> <case>
  local message='probe: a commit that names nothing'
  local name='Probe Clean' email="probe@$clean_domain" line='another line that names nothing'
  case "$2" in
    message) message=$refusable_message ;;
    author) name=$refusable_name; email=$refusable_email ;;
    line) line=$refusable_line ;;
  esac
  printf '%s\n' "$line" >> "$1/probe.txt"
  must "$real_git" -C "$1" add probe.txt
  must env GIT_AUTHOR_NAME="$name" GIT_AUTHOR_EMAIL="$email" \
    GIT_COMMITTER_NAME="$name" GIT_COMMITTER_EMAIL="$email" \
    "$real_git" -C "$1" commit -q -m "$message"
}

# The same, in a jj repository. The case's commit is made under the case's own identity, because
# jj sets a commit's author when it creates it: a `jj describe` afterwards moves the committer and
# leaves the author as it was, so an identity set only at describe time would never reach the
# author record the guard reads. The third argument is the revision to build on, where the shape
# has one of its own. @ is left empty above the case's commit, so every shape pushes @- and none
# pushes the working copy itself; nothing empty and undescribed is left between the commits a
# push would send, which jj refuses to push at all.
jj_case_commit() { # jj_case_commit <workspace> <case> [parent revision]
  local message='probe: a commit that names nothing'
  local name='Probe Clean' email="probe@$clean_domain" line='another line that names nothing'
  case "$2" in
    message) message=$refusable_message ;;
    author) name=$refusable_name; email=$refusable_email ;;
    line) line=$refusable_line ;;
  esac
  if [ "$#" -ge 3 ]; then
    must env JJ_USER="$name" JJ_EMAIL="$email" jj -R "$1" new "$3"
  else
    must env JJ_USER="$name" JJ_EMAIL="$email" jj -R "$1" new
  fi
  printf '%s\n' "$line" >> "$1/probe.txt"
  must env JJ_USER="$name" JJ_EMAIL="$email" jj -R "$1" describe -m "$message"
  must jj -R "$1" new
}

# A jj repository with the remote added and nothing else.
jj_init() { # jj_init <workspace> <bare>
  must jj git init "$1"
  must jj -R "$1" git remote add origin "$2"
}

# One clean base commit for a shape whose remote holds none: it is @ itself, described, so that
# the case's commit sits straight on top of it.
jj_base() { # jj_base <workspace>
  printf '%s\n' "$clean_line" > "$1/probe.txt"
  must jj -R "$1" describe -m 'probe: the base commit, which names nothing'
}

# A remote that already holds the bookmark, with the real git, so the bare and --deleted shapes
# have a tracked bookmark to move and to delete. Seeding is not a write under test: it is how the
# state those two shapes need comes to exist at all.
seed_remote_bookmark() { # seed_remote_bookmark <bare> <branch> <seed dir>
  git_base "$3"
  must "$real_git" -C "$3" push -q "$1" "HEAD:refs/heads/$2"
}

# The measured push itself, one shape at a time. push_env is the environment the push is made in,
# which run_case sets from the case's variant.
#
# --change names the bookmark itself, from the change id, so the branch-name case reaches it only
# through jj's own setting for that name. The value is a TOML string holding a template, and the
# template for a fixed name is a string literal, which is why the name is quoted twice: the outer
# single quotes are TOML's and the inner double quotes are the template's.
push_bookmark_config() { # push_bookmark_config <branch>
  printf 'templates.git_push_bookmark=%s' "'\"$1\"'"
}

push_in_shape() { # push_in_shape <shape> <workspace> <bare> <branch>
  case "$1" in
    git) env "${push_env[@]}" git -C "$2" push "$3" "HEAD:refs/heads/$4" ;;
    jj-bare) env "${push_env[@]}" jj -R "$2" git push --remote origin ;;
    jj-all) env "${push_env[@]}" jj -R "$2" git push --remote origin --all ;;
    jj-change) env "${push_env[@]}" jj -R "$2" git push --remote origin --change @- --config "$(push_bookmark_config "$4")" ;;
    jj-named) env "${push_env[@]}" jj -R "$2" git push --remote origin --named "$4=@-" ;;
    jj-deleted) env "${push_env[@]}" jj -R "$2" git push --remote origin --deleted ;;
    *) die "no such shape: $1" ;;
  esac
}

# What jj itself says the shape would change, read before the measured push. This is the shapes
# being driven against jj's own `--dry-run` output rather than against a list typed here: a shape
# that selected nothing would print nothing, and the check below would catch it.
jj_dry_run() { # jj_dry_run <shape> <workspace> <branch>
  case "$1" in
    jj-bare) jj -R "$2" git push --remote origin --dry-run 2>&1 ;;
    jj-all) jj -R "$2" git push --remote origin --all --dry-run 2>&1 ;;
    jj-change) jj -R "$2" git push --remote origin --change @- --config "$(push_bookmark_config "$3")" --dry-run 2>&1 ;;
    jj-named) jj -R "$2" git push --remote origin --named "$3=@-" --dry-run 2>&1 ;;
    jj-deleted) jj -R "$2" git push --remote origin --deleted --dry-run 2>&1 ;;
  esac
}

# One case: build it, read the remote, push it, read the remote again, and say whether what
# happened is what this arm calls for. Each variant is k1's environment with one thing changed:
#
#   variant=clears            k1: the stand-in clears and the Job's rule file is named, so the
#                             private-name rules decide.
#   variant=no-rules          no rule file is named at all, as in a pod whose daemon hands none.
#   variant=no-check          k7: nothing resolves a shared check at all.
#   variant=internal-error    k7: the shared check exits 2.
#   variant=rules-missing     k7: the rule file named is not there.
#   variant=rules-unparseable k7: the rule file named is no TOML.
run_case() { # run_case <shape> <case> <variant>
  local shape=$1 kase=$2 variant=$3
  local id="$shape/$kase/$variant"
  local dir=$scratch/runs/$shape-$kase-$variant
  local bare=$dir/origin.git workspace=$dir/work seed=$dir/seed
  local branch
  branch=$(branch_of "$kase")

  must mkdir -p "$dir"
  must "$real_git" init -q --bare "$bare"

  case "$shape" in
    git)
      git_base "$workspace"
      git_case_commit "$workspace" "$kase"
      ;;
    jj-all | jj-change | jj-named)
      jj_init "$workspace" "$bare"
      jj_base "$workspace"
      jj_case_commit "$workspace" "$kase"
      if [ "$shape" = jj-all ]; then
        must jj -R "$workspace" bookmark set "$branch" -r @-
      fi
      ;;
    jj-bare)
      # The bare shape pushes tracking bookmarks alone, so the remote holds this one already,
      # the case's commit is made on top of it, and the push moves the bookmark on to it.
      seed_remote_bookmark "$bare" "$branch" "$seed"
      jj_init "$workspace" "$bare"
      must jj -R "$workspace" git fetch --remote origin
      must jj -R "$workspace" bookmark track "$branch@origin"
      jj_case_commit "$workspace" "$kase" "$branch"
      must jj -R "$workspace" bookmark set "$branch" -r @-
      ;;
    jj-deleted)
      # A ref change with no commit: the remote holds the bookmark and the push deletes it.
      seed_remote_bookmark "$bare" "$branch" "$seed"
      jj_init "$workspace" "$bare"
      must jj -R "$workspace" git fetch --remote origin
      must jj -R "$workspace" bookmark track "$branch@origin"
      must jj -R "$workspace" bookmark delete "$branch"
      ;;
    *) die "no such shape: $shape" ;;
  esac

  case "$variant" in
    clears) push_env=("PUBLIC_LEAK_CHECK=$clears" "$rules_var=$rule_file") ;;
    no-rules) push_env=(-u "$rules_var" "PUBLIC_LEAK_CHECK=$clears") ;;
    # No check anywhere, under a deployment that names one: the role is told the deployment's
    # manifest digest (LEGION_LEAK_CHECKER_MANIFEST_SHA256, packages/daemon/internal/leakguard's
    # CheckerVar), so the guard expects a check and refuses the push for its absence. Without it
    # the role is one of a deployment that runs no shared check, which no write is refused for.
    no-check) push_env=(-u PUBLIC_LEAK_CHECK "HOME=$scratch/empty" "XDG_CONFIG_HOME=$scratch/empty"
      "LEGION_LEAK_CHECKER_MANIFEST_SHA256=5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e" "$rules_var=$rule_file") ;;
    internal-error) push_env=("PUBLIC_LEAK_CHECK=$internal_error" "$rules_var=$rule_file") ;;
    rules-missing) push_env=("PUBLIC_LEAK_CHECK=$clears" "$rules_var=$missing_rule_file") ;;
    rules-unparseable) push_env=("PUBLIC_LEAK_CHECK=$clears" "$rules_var=$unparseable_rule_file") ;;
    *) die "no such variant: $variant" ;;
  esac

  echo
  echo "---- $id (branch $branch)"
  if [ "$shape" != git ]; then
    printf '%s\n' "$(jj_dry_run "$shape" "$workspace" "$branch")" | sed 's/^/      jj --dry-run | /'
  fi

  local before after out status
  before=$(remote_state "$bare")
  out=$(push_in_shape "$shape" "$workspace" "$bare" "$branch" 2>&1)
  status=$?
  printf '%s\n' "$out" | sed 's/^/      push | /'
  echo "      push exited $status"
  printf '%s\n' "${before:-(no ref)}" | sed 's/^/      ls-remote before | /'
  after=$(remote_state "$bare")
  printf '%s\n' "${after:-(no ref)}" | sed 's/^/      ls-remote after  | /'

  local status_word remote_word
  if [ "$status" -eq 0 ]; then status_word=zero; else status_word=non-zero; fi
  if [ "$before" = "$after" ]; then remote_word=unchanged; else remote_word=changed; fi

  # What this arm calls for. On either arm, where the private-name rules run, every case but the
  # clean one is refused; and under either of the shared check's failures the clean one is refused
  # too, since a check that did not run has cleared nothing. The arms part where the change does:
  #
  #   - no rule file named: the head runs no private-name rule, so the refusable push lands, and
  #     the rules embedded in the control's `legion` refuse it;
  #   - the Job's rule file named: both refuse, the head naming its rule's class and what that rule
  #     matched, the control naming its own class alone;
  #   - a rule file named that cannot be used: the head refuses the push as a setup failure, and
  #     the control, which reads no such variable, lands the clean push.
  local want=land
  case "$arm/$variant" in
    */clears | control/no-rules) [ "$kase" = clean ] || want=refuse-rules ;;
    */no-check | */internal-error | head/rules-missing | head/rules-unparseable) want=refuse-setup ;;
  esac

  case "$want" in
    refuse-rules)
      check "$id the push" "$status_word" non-zero
      check "$id the remote" "$remote_word" unchanged
      check "$id names the refusal" "$(says "$out" 'LEAK CHECK: refusing')" yes
      check "$id names the record and the class" "$(says "$out" "$(fragment_of "$kase")")" yes
      if [ "$arm" = head ]; then
        check "$id names what matched and its line" "$(says "$out" "$(match_line_of "$kase")")" yes
      else
        check "$id names the class alone" "$(says "$out" 'matched "')" no
      fi
      ;;
    refuse-setup)
      check "$id the push" "$status_word" non-zero
      check "$id the remote" "$remote_word" unchanged
      check "$id names the failure" "$(says "$out" 'LEAK CHECK SETUP ERROR')" yes
      check "$id names which failure" "$(says "$out" "$(failure_of "$variant")")" yes
      ;;
    land)
      check "$id the push" "$status_word" zero
      check "$id the remote" "$remote_word" changed
      if [ "$shape" = jj-deleted ]; then
        check "$id the ref" "$(ref_word "$after" "$branch")" absent
      else
        check "$id the ref" "$(ref_word "$after" "$branch")" present
      fi
      ;;
  esac

  # With no rule file named the head's guard runs the shared check alone and says nothing of it:
  # the daemon's boot says once, in its own log, that no rule file is named, and a line on each
  # command would break a script that reads what the command prints. So the head's push prints no
  # `LEAK CHECK` line at all. The control refuses this push, so its output holds the refusal.
  if [ "$variant" = no-rules ] && [ "$arm" = head ]; then
    check "$id prints no LEAK CHECK line" "$(says "$out" 'LEAK CHECK')" no
  fi
}

echo
echo "======== k1: the five cases, under git push and under each jj shape"
echo "Each push names the Job's own rule file in $rules_var, and the stand-in shared check clears."
for shape in git jj-bare jj-all jj-change jj-named; do
  for kase in message author line branch clean; do
    run_case "$shape" "$kase" clears
  done
done
echo
echo "---- jj-deleted carries two of the five"
echo "A deletion sends a ref change and no commit, so the message, the author identity and the"
echo "added line have no form under --deleted; the four shapes above drive them. What a ref change"
echo "can carry is the ref name, which is the branch-name case, and a name that holds nothing,"
echo "which is the clean case; both are driven here."
for kase in branch clean; do
  run_case jj-deleted "$kase" clears
done

echo
echo "======== no rule file named: a refusable push, under git push and under each jj shape"
echo "$rules_var is unset, as in a pod whose daemon hands it no rule file. The head's guard"
echo "then runs no private-name rule: the push lands, and no command prints a LEAK CHECK line,"
echo "since the daemon says once at boot that no rule file is named. The control's legion runs the"
echo "rules embedded in it, which refuse the push. The added-line case is driven under every shape"
echo "that sends a commit, and the branch-name case under --deleted, which sends none."
for shape in git jj-bare jj-all jj-change jj-named; do
  run_case "$shape" line no-rules
done
run_case jj-deleted branch no-rules

echo
echo "======== k7: the four setup failures, at k1's own call sites"
echo "Two are the shared check's: none resolves, and the one that resolves exits 2 for an internal"
echo "error. Two are the rule file's: $rules_var names a path at which no file"
echo "is, or a file that is no TOML. Each is driven under git push and under every jj shape, on the"
echo "clean case, so the only reason to refuse is the failure itself. The control's legion reads no"
echo "such variable and runs the rules embedded in it, so under the rule file's two it lands the"
echo "clean push, and refuses under the shared check's two as the head does."
for variant in no-check internal-error rules-missing rules-unparseable; do
  for shape in git jj-bare jj-all jj-change jj-named jj-deleted; do
    run_case "$shape" clean "$variant"
  done
done

echo
echo "======== summary"
echo "arm: $arm"
echo "checks: $checks"
echo "mismatches: $mismatches"
if [ "$mismatches" -eq 0 ]; then
  echo "JOB-RESULT pass"
  exit 0
fi
echo "JOB-RESULT fail"
exit 1
