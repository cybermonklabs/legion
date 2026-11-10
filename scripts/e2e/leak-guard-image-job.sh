#!/usr/bin/env bash
# The live proof that the published worker image's own `git` and `jj` refuse a push that names
# something private, and that nothing else is changed: one Kubernetes Job on the image digest the
# caller names, driving the image's wrappers, its `legion leak-check`, the gitleaks engine linked
# into that binary and the rule file embedded in it
# (`packages/daemon/internal/leakguard/private-names.toml`). It is check k1 of
# dispatch://INFRA-1527 and, for the two shared-check failures at k1's own call sites, part of k7;
# the tester runs it before the merge and the production check runs it again after the rollout, so
# this file is the one recipe all three read.
#
# The Job holds no Kubernetes credential and no GitHub credential, so it cannot fetch the private
# shared public leak check a pod installs. It writes a stand-in of its own into its `emptyDir`
# instead and names it in `PUBLIC_LEAK_CHECK`; that stand-in clears every call, so the rule file
# alone decides each verdict. Every one of k1's five cases turns on the Tailscale MagicDNS class,
# which only the rule file holds, which is why a stand-in is enough here. The real shared check is
# driven after the rollout, in a real pod, where a credential exists.
#
# What the Job drives, inside the image, against a bare scratch repository on its `emptyDir`:
#
#   - k1's five cases — a commit whose message carries the refusable host, one whose author
#     identity carries it, one whose only hit is an added line of a tracked file, one whose only
#     hit is the branch name, and a clean one — under `git push` and under `jj git push` in each
#     shape jj's own help enumerates: bare, `--all`, `--change`, `--named` and `--deleted`, the
#     last a ref change with no commit. `git ls-remote` is read before and after every push, and
#     the added-line case and the branch-name case are reported as checks of their own, because a
#     wrapper that passed only commit messages and identities would refuse the other three and
#     still let through the one push this repository was measured leaking.
#
#   - k7's two shared-check failures at those same call sites: the stand-in removed, so no check
#     resolves at all, and a stand-in that exits 2 for an internal error. Each must refuse every
#     one of `git push` and `jj git push` and name the failure it met. k7's other two failures —
#     the rule file absent, and the rule file present but unparseable — cannot be met here at all,
#     because the rules are embedded in the `legion` binary and the engine is linked into it; the
#     Job says so in its own log, and the release lane, which reads the file from a checkout, is
#     where those two are driven.
#
# The negative control is the base branch's published image, driven by the same Job with the same
# probe: it carries no wrappers, so every one of the five cases lands. Pass it as `--control`.
# A run with no `--control` proves only the head: it shows that the head's image refuses, and
# nothing about whether the cases would have been let through without the change.
#
# Inputs, all of them required unless marked:
#
#   --image <ref>@sha256:<64 hex>    the image to prove, by digest, never by tag. The digest is in
#                                    the `Record digest` step of the `Worker Image` run of the
#                                    commit under test (docs/kubernetes.md, "Pin by digest, never
#                                    by tag"). Also the first positional argument.
#   --control <ref>@sha256:<64 hex>  optional: the base branch's image, driven as the negative
#                                    control. Also the second positional argument.
#   --node-selector <key=value>[,…]  the deployment's compute class, which docs/kubernetes.md
#                                    requires a workload to select by `nodeSelector`. Its value is
#                                    in the deployment's own declaration, which this public
#                                    repository does not carry, so it is an input and never a
#                                    literal here. `LEAK_GUARD_NODE_SELECTOR` sets it too. It must
#                                    be a class the namespace's admission allows
#                                    `runtimeClassName: gvisor` on, which this Job always sets: a
#                                    class that admits no sandbox is refused at the create, the
#                                    policy naming every class and the runtimeClassName it takes,
#                                    and nothing of the arm is left.
#   --namespace <namespace>          optional: the namespace to run in. The default is the one the
#                                    pod's own credential names, read from
#                                    /var/run/secrets/kubernetes.io/serviceaccount/namespace.
#   --prefix <generateName prefix>   optional: the Job's `metadata.generateName`, which must end in
#                                    a hyphen. The default names this issue.
#   --dry-run                        render the manifest, validate it with kubectl, create nothing,
#                                    and exit. This is what proves the manifest without an image.
#
# Exit statuses:
#
#   0  every check of every arm held: on the head's image the four refusable cases were refused
#      and named their class, the clean case landed, and both shared-check failures refused under
#      both commands; on the control's image, if one was given, all five landed.
#   1  the Job ran and a check did not hold. Its log, printed above the verdict, says which.
#   2  the harness could not run: a usage error, a missing input, no Kubernetes credential or a
#      namespace that refuses a Job, a Job that started no pod, a log that could not be read, or a
#      log with no `JOB-RESULT` line in it. Nothing is proved either way.
#   3  the Job did not reach a verdict inside its deadline. Nothing is proved either way.
#
# This script is synchronous and its clean-up is a trap, so a role that runs it from its own bash
# tool runs it detached, the way docs/kubernetes.md, "Running a workload in the cluster from a
# role", sets out — the tool kills a call that runs past its timeout faster than any trap runs:
#
#   log=$(mktemp "${TMPDIR:-/tmp}/leak-guard.XXXXXX")
#   flock "$log" setsid -f bash scripts/e2e/leak-guard-image-job.sh \
#     --image <head digest> --control <base digest> --node-selector <key=value> \
#     > "$log" 2>&1 < /dev/null
#   flock "$log" cat "$log"     # waits for the run to end, then prints everything it printed
#
# The Job is this repository's only workload of its own, and it follows every rule of that section:
# gVisor, the deployment's compute class by `nodeSelector`, a largest container asking four
# processors, `activeDeadlineSeconds` on the Job and on its pod template, `ttlSecondsAfterFinished`
# on the Job, no service account and no projected token, and one `emptyDir` and nothing else. It
# carries no Service and no reachability label, because nothing reaches it: it is read by its log,
# which is what that rule is for ("a workload that asks to be reachable"). Its security context is
# the worker container's, as dispatch://INFRA-257's solutions page prescribes for a Job on this
# image: uid, gid and fsGroup 1000, no privilege escalation, every capability dropped and the
# RuntimeDefault seccomp profile.
set -euo pipefail

# How long the Job may run, how long it is kept after it ends, and how long this script waits. The
# Job's two deadlines are what bounds a run this script lost hold of; the wait is shorter than
# neither, so a run that ends at its deadline is reported by this script rather than waited past.
job_deadline=1800        # on the Job and on its pod template; docs/kubernetes.md allows 43200
job_ttl=3600             # after it finishes; that section allows 86400
pod_wait=600             # for the pod to exist: the compute class makes a machine, about 80 s
verdict_wait=1800        # for the Job to reach a condition, from the moment its pod exists
poll=5                   # how often the wait reads the Job and its pod

# The namespace file a projected credential carries, which is what names the namespace a Job of
# this pod's own goes in.
namespace_file=/var/run/secrets/kubernetes.io/serviceaccount/namespace

image=""
control=""
namespace=""
node_selector=${LEAK_GUARD_NODE_SELECTOR:-}
prefix=infra-1527-leak-guard-
dry_run=0
positional=0

usage() {
  cat <<'USAGE'
usage: scripts/e2e/leak-guard-image-job.sh --image <ref>@sha256:<64 hex>
                                           [--control <ref>@sha256:<64 hex>]
                                           [--node-selector <key=value>[,<key=value>…]]
                                           [--namespace <namespace>] [--prefix <name->]
                                           [--dry-run]

  --image         the worker image to prove, by digest; the first positional argument too.
  --control       the base branch's image, driven as the negative control, which lets all five
                  cases through; the second positional argument too. A run without it proves only
                  the head: it says nothing about whether the cases would have been let through
                  without the change.
  --node-selector the deployment's compute class, which the namespace's admission requires. Read
                  it from the deployment's own declaration; this public repository carries no
                  value of it. LEAK_GUARD_NODE_SELECTOR sets it too.
  --namespace     the namespace to run in; the default is the one the pod's credential names.
  --prefix        the Job's metadata.generateName, ending in a hyphen.
  --dry-run       render the manifest, validate it with kubectl, create nothing, and exit.

exit statuses: 0 every check held; 1 the Job ran and a check did not hold; 2 the harness could not
run and nothing is proved; 3 the Job reached no verdict inside its deadline.
USAGE
}

refuse() {
  echo "leak-guard-image-job: $*" >&2
  echo >&2
  usage >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --image) [ $# -ge 2 ] || refuse "--image takes a value"; image=$2; shift 2 ;;
    --image=*) image=${1#--image=}; shift ;;
    --control) [ $# -ge 2 ] || refuse "--control takes a value"; control=$2; shift 2 ;;
    --control=*) control=${1#--control=}; shift ;;
    --namespace) [ $# -ge 2 ] || refuse "--namespace takes a value"; namespace=$2; shift 2 ;;
    --namespace=*) namespace=${1#--namespace=}; shift ;;
    --node-selector) [ $# -ge 2 ] || refuse "--node-selector takes a value"; node_selector=$2; shift 2 ;;
    --node-selector=*) node_selector=${1#--node-selector=}; shift ;;
    --prefix) [ $# -ge 2 ] || refuse "--prefix takes a value"; prefix=$2; shift 2 ;;
    --prefix=*) prefix=${1#--prefix=}; shift ;;
    --dry-run) dry_run=1; shift ;;
    -h | --help) usage; exit 0 ;;
    -*) refuse "unknown option $1" ;;
    *)
      positional=$((positional + 1))
      case "$positional" in
        1) image=$1 ;;
        2) control=$1 ;;
        *) refuse "too many arguments: $1" ;;
      esac
      shift
      ;;
  esac
done

# A digest and nothing else. A tag is mutable, and a proof of an image nobody can name again is no
# proof: `legion.yaml` refuses a tag for the same reason (docs/kubernetes.md, "Pin by digest").
digest_shape='^[A-Za-z0-9][A-Za-z0-9._:/-]*@sha256:[0-9a-f]{64}$'
[ -n "$image" ] || refuse "no image: name the worker image to prove, by digest"
[[ $image =~ $digest_shape ]] || refuse "the image $image is not a digest reference <ref>@sha256:<64 hex>"
if [ -n "$control" ]; then
  [[ $control =~ $digest_shape ]] || refuse "the control $control is not a digest reference <ref>@sha256:<64 hex>"
  [ "$control" != "$image" ] || refuse "the control is the image under test; the control is the base branch's image"
fi
[ -n "$node_selector" ] || refuse "no --node-selector: the namespace's admission requires the deployment's compute class, which is in the deployment's own declaration"
case "$prefix" in
  *-) ;;
  *) refuse "the prefix $prefix does not end in a hyphen; the API server appends five characters to it" ;;
esac
if [ -z "$namespace" ]; then
  [ -r "$namespace_file" ] || refuse "no --namespace and no $namespace_file: this pod holds no projected Kubernetes credential, so it can start no workload"
  namespace=$(cat "$namespace_file")
fi
[ -n "$namespace" ] || refuse "the namespace is empty"

# The compute class, as the YAML lines a nodeSelector takes. Each pair is checked rather than
# pasted, so a mistyped input is a refusal here and not a Job the scheduler never places.
selector_yaml=""
old_ifs=$IFS
IFS=,
for pair in $node_selector; do
  case "$pair" in
    *=*) ;;
    *) refuse "the node selector entry $pair is no key=value pair" ;;
  esac
  key=${pair%%=*}
  value=${pair#*=}
  [ -n "$key" ] || refuse "the node selector entry $pair has an empty key"
  selector_yaml+=$(printf '        %s: "%s"\n' "$key" "$value")$'\n'
done
IFS=$old_ifs
[ -n "$selector_yaml" ] || refuse "the node selector named no pair"

command -v kubectl > /dev/null 2>&1 || refuse "no kubectl on PATH"

work=$(mktemp -d "${TMPDIR:-/tmp}/leak-guard-image-job.XXXXXX")
# Outside the checkout on purpose: a workspace is a jj working copy of this public repository, so
# a manifest written there would be snapshotted into the role's own commit.
probe=$work/probe.sh

# The Job this run has created, and the label it carries. The trap reads both: the name once the
# create has answered, the label when it has not, which is the window a stop can land in
# (docs/kubernetes.md, the paragraph on a trap that runs before the name was read).
workload=""
run_label=""
cleanup_status=0

cleanup() {
  status=$?
  trap - EXIT INT TERM HUP
  trap '' INT TERM HUP                        # this shell ignores any further stop
  set +e
  # Each kubectl runs under `setsid -w`, in a session of its own, out of this group: kubectl dies
  # of a SIGTERM it inherited ignored, and its waits catch SIGINT and SIGHUP too.
  if [ -n "$workload" ]; then
    echo "== cleanup: deleting Job $workload"
    setsid -w kubectl -n "$namespace" delete job "$workload" --cascade=foreground --ignore-not-found ||
      { echo "cleanup: the Job's delete failed"; [ "$status" -ne 0 ] || status=1; }
    setsid -w kubectl -n "$namespace" get job "$workload" --ignore-not-found
  elif [ -n "$run_label" ]; then
    echo "== cleanup: no Job name was read; deleting by label leak-guard-run=$run_label"
    setsid -w kubectl -n "$namespace" delete job -l "leak-guard-run=$run_label" --cascade=foreground --ignore-not-found ||
      { echo "cleanup: the Job's delete failed"; [ "$status" -ne 0 ] || status=1; }
    # An empty listing is no proof that none is left: a create the API server stored after this
    # listing leaves a Job this never saw. List it again once this script has ended, and delete it
    # with `kubectl -n <namespace> delete job -l leak-guard-run=<the printed value>
    # --cascade=foreground`.
    setsid -w kubectl -n "$namespace" get jobs -l "leak-guard-run=$run_label"
  fi
  rm -rf "$work"
  [ "$cleanup_status" -eq 0 ] || status=$cleanup_status
  echo "leak-guard-image-job: exit $status"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

# ---------------------------------------------------------------------------------------------
# The probe. It runs inside the image, as the Job's one container, and decides its own verdict:
# every check prints `CHECK <name>: got [x] want [y]`, and the last line is `JOB-RESULT pass` or
# `JOB-RESULT fail`. Its one argument is the arm — `head` for an image whose wrappers must refuse,
# `control` for the base branch's image, which has none and must let everything through — so one
# probe serves both and the expectations are stated once.
#
# Read the probe's own verdict and never the Job's phase: a Job reports `Succeeded` when its
# container exited 0 and nothing more (skills/legion-worker/references/tools.md).
# ---------------------------------------------------------------------------------------------
cat > "$probe" <<'PROBE'
#!/usr/bin/env bash
# The probe of scripts/e2e/leak-guard-image-job.sh, run inside the worker image by a Kubernetes
# Job. One argument: `head` or `control`.
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
# name, which the rules pass and which would prove nothing.
tailnet_label=mossy-lantern
magicdns_suffix=ts.net
refusable_host="quill-badger.${tailnet_label}.${magicdns_suffix}"
# The class the rule file calls that match. A refusal has to name it, or the push was refused for
# some other reason and the case proved nothing.
refusable_class='names a Tailscale MagicDNS host'

clean_domain=example.invalid
clean_line='a line of the probe that names nothing'
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
# On the head's image the two wrappers lead PATH with the real binaries behind them, and the
# pre-push hook is root's in a directory no role can write. On the base's image none of that
# exists, which is the whole of why the control lets the five cases through.
if [ "$arm" = head ]; then
  want_git=/opt/legion/bin/git
  want_jj=/opt/legion/bin/jj
  want_hook='0:0 755'
  want_leak_check=present
else
  want_git=/usr/bin/git
  want_jj=/usr/local/bin/jj
  want_hook='no hook'
  want_leak_check=absent
fi
check "the git on PATH" "$(command -v git || echo 'no git at all')" "$want_git"
check "the jj on PATH" "$(command -v jj || echo 'no jj at all')" "$want_jj"
check "the pre-push hook" "$(stat -c '%u:%g %a' /opt/legion/githooks/pre-push 2> /dev/null || echo 'no hook')" "$want_hook"
if legion leak-check --help > /dev/null 2>&1; then
  check "legion leak-check" present "$want_leak_check"
else
  check "legion leak-check" absent "$want_leak_check"
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
# A Job holds no credential, so the private shared check cannot be fetched here. These stand-ins
# are what PUBLIC_LEAK_CHECK names instead. The clearing one passes every call, so every verdict
# below is the embedded rule file's alone; the other two are k7's two shared-check failures.
check_dir=$scratch/leak-check
must mkdir -p "$check_dir"
clears=$check_dir/public-leak-check.sh
internal_error=$check_dir/public-leak-check-internal-error.sh
cat > "$clears" <<'SH'
#!/usr/bin/env bash
# Clears every call: the embedded rule file is what decides each verdict of this run.
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

# fragment_of <case> is the report line the refusal must carry: the record, then the class. The
# records are the guard's own (packages/daemon/internal/leakguard/push.go), so the added-line case
# and the branch-name case are told apart here rather than being read as one refusal.
fragment_of() {
  case "$1" in
    message) printf 'message: %s' "$refusable_class" ;;
    author) printf 'author: %s' "$refusable_class" ;;
    line) printf 'probe.txt: %s' "$refusable_class" ;;
    branch) printf 'the ref name: %s' "$refusable_class" ;;
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
    message) message="probe: the box at $refusable_host answered" ;;
    author) name='Probe Host'; email="probe@$refusable_host" ;;
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
    message) message="probe: the box at $refusable_host answered" ;;
    author) name='Probe Host'; email="probe@$refusable_host" ;;
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

# The measured push itself, one shape at a time. push_env is the environment the push is made in:
# the clearing stand-in for k1's cases, and k7's two failures for the two variants below.
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
# happened is what this arm calls for.
#
#   variant=clears         k1: the stand-in clears, so the rule file decides.
#   variant=no-check       k7: nothing resolves a shared check at all.
#   variant=internal-error k7: the shared check exits 2.
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
    clears) push_env=("PUBLIC_LEAK_CHECK=$clears") ;;
    internal-error) push_env=("PUBLIC_LEAK_CHECK=$internal_error") ;;
    no-check) push_env=(-u PUBLIC_LEAK_CHECK "HOME=$scratch/empty" "XDG_CONFIG_HOME=$scratch/empty") ;;
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

  # What this arm calls for. On the head's image every case but the clean one is refused, and the
  # clean one is refused too under either of k7's two failures, since a check that did not run has
  # cleared nothing. On the base's image there is no wrapper, so everything lands.
  local refuse_it=no
  if [ "$arm" = head ] && { [ "$kase" != clean ] || [ "$variant" != clears ]; }; then
    refuse_it=yes
  fi

  if [ "$refuse_it" = yes ]; then
    check "$id the push" "$status_word" non-zero
    check "$id the remote" "$remote_word" unchanged
    case "$variant" in
      clears)
        check "$id names the refusal" "$(says "$out" 'LEAK CHECK: refusing')" yes
        check "$id names the record and the class" "$(says "$out" "$(fragment_of "$kase")")" yes
        ;;
      no-check)
        check "$id names the failure" "$(says "$out" 'LEAK CHECK SETUP ERROR')" yes
        check "$id names which failure" "$(says "$out" 'the shared public leak check could not be found')" yes
        ;;
      internal-error)
        check "$id names the failure" "$(says "$out" 'LEAK CHECK SETUP ERROR')" yes
        check "$id names which failure" "$(says "$out" 'the shared public leak check could not finish (exit 2)')" yes
        ;;
    esac
  else
    check "$id the push" "$status_word" zero
    check "$id the remote" "$remote_word" changed
    if [ "$shape" = jj-deleted ]; then
      check "$id the ref" "$(ref_word "$after" "$branch")" absent
    else
      check "$id the ref" "$(ref_word "$after" "$branch")" present
    fi
  fi
}

echo
echo "======== k1: the five cases, under git push and under each jj shape"
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
echo "======== k7: the two shared-check failures, at k1's own call sites"
echo "The rule file is embedded in the legion binary (//go:embed private-names.toml) and the"
echo "engine is linked into it as a library, so k7's other two failures — the rule file absent,"
echo "and the rule file present but unparseable — cannot be met in this image at all: there is no"
echo "file for it to lose and no parse for it to fail. They are driven in the release lane, which"
echo "reads the rule file from a checkout. These two can be met here, and both are driven under"
echo "git push and under every jj shape, on the clean case, so the only reason to refuse is the"
echo "failure itself."
for variant in no-check internal-error; do
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
PROBE

bash -n "$probe" || refuse "the probe this script writes is not valid bash"

# ---------------------------------------------------------------------------------------------
# The manifest. Every field below is one of the rules of docs/kubernetes.md, "Running a workload
# in the cluster from a role"; the comments say which.
# ---------------------------------------------------------------------------------------------
render_manifest() { # render_manifest <arm> <image> <run label> <file>
  {
    cat <<YAML
apiVersion: batch/v1
kind: Job
metadata:
  # The API server names the run: five characters it appends at the create, so no name this script
  # picks can be one another workload of the namespace already holds.
  generateName: ${prefix}${1}-
  # On the Job's own metadata and never on its pod template, so no pod carries it and it can never
  # be read as the namespace's reachability label.
  labels:
    leak-guard-run: "${3}"
spec:
  activeDeadlineSeconds: ${job_deadline}   # on the Job; at most 43200
  ttlSecondsAfterFinished: ${job_ttl}      # at most 86400
  backoffLimit: 0                          # one attempt: a second run would prove the same thing twice
  template:
    spec:
      restartPolicy: Never
      activeDeadlineSeconds: ${job_deadline} # on the pod template too; a manifest with one alone is refused
      automountServiceAccountToken: false    # it carries no identity, and needs none
      runtimeClassName: gvisor
      nodeSelector:
${selector_yaml}      securityContext:
        # The worker container's own, as dispatch://INFRA-257 prescribes for a Job on this image.
        runAsNonRoot: true
        runAsUser: 1000
        runAsGroup: 1000
        fsGroup: 1000
      containers:
        - name: probe
          image: ${2}
          imagePullPolicy: IfNotPresent          # a digest names one image; a re-pull would fetch the same bytes
          terminationMessagePolicy: FallbackToLogsOnError
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
            seccompProfile:
              type: RuntimeDefault
          resources:
            requests:
              cpu: "4"                           # the largest container asks at least four processors
              memory: 4Gi
          volumeMounts:
            - name: scratch
              mountPath: /scratch
          # The image's ENTRYPOINT is \`legion\`; this Job runs the probe instead.
          command: ["/bin/bash"]
          args:
            - "-c"
            - |
YAML
    awk '{ if (length($0) > 0) printf "              %s\n", $0; else print "" }' "$probe"
    cat <<YAML
            - "leak-guard-probe"
            - "${1}"
      volumes:
        - name: scratch
          emptyDir: {}                           # the only volume: no disk outlives the workload
YAML
  } > "$4"
}

run_id() { od -An -N4 -tx1 /dev/urandom | tr -d ' \n'; }

echo "== leak-guard-image-job"
echo "namespace: $namespace"
echo "image:     $image"
echo "control:   ${control:-none — this run proves only the head}"
echo "selector:"
printf '%s' "$selector_yaml"

if [ "$dry_run" -eq 1 ]; then
  manifest=$work/job-dry-run.yaml
  render_manifest head "$image" "$(run_id)" "$manifest"
  echo
  echo "== the manifest, rendered and not created"
  cat "$manifest"
  echo
  # A server dry run runs the namespace's own admission and creates nothing, which is what says
  # whether this manifest would be admitted. Where this pod may not create a Job, the client dry
  # run is what is left: it validates the manifest against kubectl's own schema and asks nothing.
  if [ "$(kubectl -n "$namespace" auth can-i create jobs 2> /dev/null || true)" = yes ]; then
    echo "== kubectl -n $namespace create --dry-run=server -f <the manifest above>"
    kubectl -n "$namespace" create --dry-run=server -f "$manifest" || {
      echo "leak-guard-image-job: the namespace's admission refused the manifest" >&2
      exit 2
    }
  else
    echo "== this pod may not create a Job here, so the manifest is validated client-side only"
    echo "== kubectl create --dry-run=client -f <the manifest above>"
    kubectl create --dry-run=client -f "$manifest" || {
      echo "leak-guard-image-job: kubectl refused the manifest" >&2
      exit 2
    }
  fi
  exit 0
fi

# The gate. Only `create jobs` is asked for as a condition: this workload carries no Service,
# because nothing reaches it over the network — it is read by its log. The other three answers are
# printed because a run that cannot read a pod or its log proves nothing either.
echo
echo "== what this pod may do in $namespace"
can_create=$(kubectl -n "$namespace" auth can-i create jobs 2>&1 || true)
echo "create jobs:   $can_create"
echo "get pods:      $(kubectl -n "$namespace" auth can-i get pods 2>&1 || true)"
echo "get pods/log:  $(kubectl -n "$namespace" auth can-i get pods/log 2>&1 || true)"
echo "delete jobs:   $(kubectl -n "$namespace" auth can-i delete jobs 2>&1 || true)"
if [ "$can_create" != yes ]; then
  echo "leak-guard-image-job: this pod may not create a Job in $namespace, so k1 is not run here rather than inferred" >&2
  exit 2
fi

# run_arm drives one image and answers its verdict word on standard output: pass, fail, timeout or
# broken. Everything it prints for a person goes to standard error's twin, this script's own
# output, in the order docs/kubernetes.md asks for: the pod's labels first, then the Job's log,
# then the verdict.
arm_verdict=""
run_arm() { # run_arm <arm> <image>
  local arm=$1 image=$2
  local manifest=$work/job-$arm.yaml created uid phase conditions deadline ran digest

  arm_verdict=broken
  run_label=$(run_id)
  workload=""
  render_manifest "$arm" "$image" "$run_label" "$manifest"

  echo
  echo "======== the $arm arm, on $image"
  echo "run: leak-guard-run=$run_label"
  created=$(kubectl -n "$namespace" create -o jsonpath='{.metadata.name} {.metadata.uid}' -f "$manifest") || {
    echo "the create was refused; nothing of this arm is left"
    return 0
  }
  workload=${created% *}
  uid=${created#* }
  echo "workload: $workload (uid $uid)"

  if ! kubectl -n "$namespace" wait --for=create "pod" -l "job-name=$workload" --timeout="${pod_wait}s"; then
    echo "no pod of $workload exists after ${pod_wait}s"
    kubectl -n "$namespace" get job "$workload" -o wide || true
    kubectl -n "$namespace" get pods -l "job-name=$workload" || true
    return 0
  fi

  # The labels first, as that section asks: they are what a role compares when a workload answers
  # nothing, and they are gone with the pod once the trap has run.
  echo
  echo "== the pod's labels"
  kubectl -n "$namespace" get pods -l "job-name=$workload" --show-labels || true

  echo
  echo "== waiting for the Job to reach a verdict"
  # The Job's phase is a request, not a result: a Job can report Running with no pod at all when
  # the namespace's quota is full. So the pod is read at every poll beside the Job's conditions.
  deadline=$((SECONDS + verdict_wait))
  phase=""
  while :; do
    conditions=$(kubectl -n "$namespace" get job "$workload" \
      -o jsonpath='{range .status.conditions[?(@.status=="True")]}{.type} {end}' 2> /dev/null || true)
    local now
    now=$(kubectl -n "$namespace" get pods -l "job-name=$workload" \
      -o jsonpath='{range .items[*]}{.status.phase}{" "}{end}' 2> /dev/null || true)
    if [ "$now" != "$phase" ]; then
      echo "  pods: [${now:-none}] job conditions: [${conditions:-none}]"
      phase=$now
    fi
    case " $conditions " in
      *" Complete "* | *" Failed "*) break ;;
    esac
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "the Job reached no condition inside ${verdict_wait}s"
      kubectl -n "$namespace" get pods -l "job-name=$workload" -o wide || true
      arm_verdict=timeout
      return 0
    fi
    sleep "$poll"
  done
  echo "  job conditions: [${conditions:-none}]"

  # The image the pod actually ran, which is what ties everything below to the digest named above.

  ran=$(kubectl -n "$namespace" get pods -l "job-name=$workload" \
    -o jsonpath='{.items[0].status.containerStatuses[0].imageID}' 2> /dev/null || true)
  echo "  imageID: ${ran:-unreadable}"

  echo
  echo "== the Job's log"
  local log=$work/$arm.log
  if ! kubectl -n "$namespace" logs "job/$workload" --tail=-1 > "$log" 2>&1; then
    echo "the Job's log could not be read"
    cat "$log" || true
    return 0
  fi
  cat "$log"

  digest=${image#*@}
  case "$ran" in
    *"$digest"*) ;;
    *)
      echo
      echo "the pod ran imageID [${ran:-unreadable}], which does not name the digest $digest"
      return 0
      ;;
  esac

  if grep -q '^JOB-RESULT pass$' "$log"; then
    arm_verdict=pass
  elif grep -q '^JOB-RESULT fail$' "$log"; then
    arm_verdict=fail
  else
    echo
    echo "the Job's log carries no JOB-RESULT line, so it decided nothing"
  fi
  return 0
}

# The Job this arm made, gone on its own rather than at the trap: the next arm would otherwise pay
# for this one's four processors while it runs. A create the server refused leaves no name, and
# the label is what finds a Job it stored after all (docs/kubernetes.md, the recovery line).
delete_arm() {
  if [ -n "$workload" ]; then
    echo
    echo "== deleting Job $workload"
    setsid -w kubectl -n "$namespace" delete job "$workload" --cascade=foreground --ignore-not-found ||
      { echo "the Job's delete failed"; cleanup_status=2; }
    setsid -w kubectl -n "$namespace" get job,pods -l "job-name=$workload" --ignore-not-found || true
  elif [ -n "$run_label" ]; then
    echo
    echo "== no Job name was read; deleting by label leak-guard-run=$run_label"
    setsid -w kubectl -n "$namespace" delete job -l "leak-guard-run=$run_label" --cascade=foreground --ignore-not-found ||
      { echo "the Job's delete failed"; cleanup_status=2; }
    setsid -w kubectl -n "$namespace" get jobs -l "leak-guard-run=$run_label" || true
  fi
  workload=""
  run_label=""
}

head_verdict=""
control_verdict=""

run_arm head "$image"
head_verdict=$arm_verdict
delete_arm

# The control runs only once the head arm reached a verdict of its own. A head arm that never ran
# is a broken harness, and a second Job would spend four more processors proving nothing.
if [ -n "$control" ]; then
  case "$head_verdict" in
    pass | fail)
      run_arm control "$control"
      control_verdict=$arm_verdict
      delete_arm
      ;;
    *) control_verdict="not run: the head arm reached no verdict" ;;
  esac
fi

echo
echo "======== the verdict"
echo "head ($image): $head_verdict"
if [ -n "$control" ]; then
  echo "control ($control): $control_verdict"
else
  echo "control: none was given, so this run proves only that the head's image refuses"
fi

verdict_status() { # verdict_status <word>
  case "$1" in
    pass) echo 0 ;;
    fail) echo 1 ;;
    timeout) echo 3 ;;
    *) echo 2 ;;
  esac
}

status=$(verdict_status "$head_verdict")
if [ -n "$control" ]; then
  control_status=$(verdict_status "$control_verdict")
  [ "$control_status" -eq 0 ] || status=$control_status
fi
exit "$status"
