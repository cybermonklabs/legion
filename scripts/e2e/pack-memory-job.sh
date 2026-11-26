#!/usr/bin/env bash
# The measurement Job of the pack-memory checks: c-git-rows, c-jj-snapshot-memory,
# c-handoff-head-read-memory, c-shim-adoption-memory and c-budget-under-gvisor, and the Job's
# halves of c-pack-window-config and c-release-source. One Kubernetes Job, on the worker image the
# caller names by digest, under gVisor, makes a large repository's clone and jj workspace with
# the image's own code (`legion workspace-init cache`, `fetch` and `provision`) on a disk volume,
# as an issue pod's are made, and measures the memory of git and jj there: each git row with the
# role's own git settings and with git's defaults forced, its fetch from GitHub of the newer
# commits of the default branch into a clone that lacks them, each jj row of R3 to R5 with the
# image's jj and with jj 0.45.1-sami.20260910-043938, the control, and R10's rows with the
# released jj at each JJ_PACK_MAPPED_LIMIT budget and with jj 0.46.0-sami.20261008-234600, their
# control, each on a copy of its own. Each copy's describedArgs, the shim's, runs right after an
# unmeasured `git fetch` of every branch head of the feed into that copy's shared clone, as
# remote-tracking refs it does not hold, and an unmeasured `git commit` in its workspace, with
# both dates fixed, so describedArgs imports new refs and a new HEAD, as a role's jj does once
# git has fetched new branches into the shared clone and committed; its adoptionArgs runs right
# after a second such commit, so it imports a new HEAD too. It reads the image's `git
# config --system` and `jj --version` too. The implementer runs it at the head it measures, and
# the tester runs it again at the head it tests, so this recipe is the one both read.
#
# It is three files: this harness, which renders the Job, creates it and reads its verdict;
# `pack-memory-probe.sh` beside it, the text the Job runs inside the image, whose header says what
# it measures and how it judges each row; and `lib/pack-memory-peak.py`, the process-tree sampler
# the probe runs every measured command under. A Job holds no checkout, so this harness writes
# the probe and the sampler into the Job's command itself; none of the three is run or changed
# without the others.
#
# What the harness adds from its own checkout, so it is run from a checkout of the head it
# measures:
#
#   - the role's GIT_CONFIG block: `go run ./internal/runtime/sandbox/rolegitconfig`, run from
#     packages/daemon, prints sandbox.GitConfigPairs() as GIT_CONFIG_COUNT, GIT_CONFIG_KEY_<n> and
#     GIT_CONFIG_VALUE_<n>, the block a role's start command carries, and this sets each of them on
#     the Job's container, so the git rows "with the role's settings" read that head's block;
#   - a check that the commands the probe copies are still the ones the code builds: the shim's
#     describedArgs and adoptionArgs (packages/daemon/internal/shim/commands.go) and
#     handoff-record.ts's STANDING read (packages/pi-shared/src/handoff-record.ts). A change to
#     any of them is a refusal here, naming the line, until the probe is changed with it;
#   - the jj release this checkout's image installs: the version that the one `ARG
#     JJ_TOOL=<source>@<version>` line of packages/daemon/docker/worker.Dockerfile names, set on
#     the container as PACK_MEMORY_JJ_TOOL_VERSION, which the image's `jj --version` must print
#     (c-release-source).
#
# Inputs. Every value private to a deployment is an input and never a literal here: this
# repository is public, and its deployment's namespace, compute class, storage class and the
# labels and annotations its admission requires are in that deployment's own declaration.
#
#   --image <ref>@sha256:<64 hex>   required: the worker image to measure, by digest, never by
#                                   tag. The digest is in the `Record digest` step of the
#                                   `Worker Image` run of the commit measured (docs/kubernetes.md,
#                                   "Pin by digest, never by tag").
#   --node-selector <key=value>[,…] required: the compute class the Job's pod selects, a class
#                                   that admits `runtimeClassName: gvisor`, which this Job always
#                                   sets, because the cause it measures is gVisor's.
#   --storage-class <name>          required: the storage class of the Job's disk, an ephemeral
#                                   volume claim mounted at /scratch, where the clone goes.
#   --namespace <namespace>         the namespace to run in. The default is the one the pod's own
#                                   credential names (/var/run/secrets/kubernetes.io/
#                                   serviceaccount/namespace).
#   --label <key=value>             repeatable: a label on the Job's metadata and on its pod
#                                   template's, as the namespace's admission requires.
#   --annotation <key=value>        repeatable: an annotation on both, the same way.
#   --cpu <quantity>                the container's CPU, request and limit (default 8).
#   --memory <quantity>             its memory, request and limit (default 32Gi): the controls
#                                   peaked near 13 GB on the largest repository measured.
#   --disk <quantity>               the disk volume's size (default 300Gi): the original root,
#                                   six copies of it whose packs are hardlinks of its own, one
#                                   fresh root of R10's at a time, and the fetch rows' clone that
#                                   lacks the default branch's newer commits, while they run.
#   --deadline <seconds>            activeDeadlineSeconds on the Job and on its pod template
#                                   (default 28800; at most 43200, which docs/kubernetes.md allows).
#   --prefix <generateName prefix>  the Job's metadata.generateName, ending in a hyphen (default
#                                   infra-4036-pack-memory-).
#   --repo <owner>/<name>           the public repository to measure (default
#                                   mozilla-firefox/firefox: one 5.86 GB pack, 489,990 files).
#   --walk-file <path>              the file the git log row walks and the jj rows edit (default
#                                   config/milestone.txt).
#   --dry-run                       render the manifest, validate it with kubectl (the namespace's
#                                   admission where this pod may create a Job, else kubectl's own
#                                   schema), create nothing, and exit.
#
# Fixed here: gVisor; both deadlines; ttlSecondsAfterFinished; backoffLimit 6 with
# podReplacementPolicy Failed, so a pod lost with its machine is followed by a whole new attempt
# and never by a second one beside it; no service account token; the worker container's security
# context (uid, gid and fsGroup 1000, no privilege escalation, every capability dropped, the
# RuntimeDefault seccomp profile), as leak-guard-image-job.sh's Job on this image has it; requests
# equal to limits; and the image reference and its
# org.opencontainers.image.revision label on the container's environment, for the probe to print.
# The label is read with `crane config` where this pod has crane, and is printed as unread where it
# has not.
#
# Exit statuses:
#
#   0  the probe's every verdict held: its log ends `JOB-RESULT pass`. A git or R3 to R5 control
#      that did not reach its bound is a VOID line and no failure, and so is a fetch row whose new
#      pack is under 100 MiB; an R10 control that read under 90% of the pack is a MISMATCH. Read
#      the log for each.
#   1  the Job ran and a verdict did not hold: its log ends `JOB-RESULT fail`, and its `CHECK`
#      lines say which.
#   2  the harness could not run: a usage error, a missing input, a source line the probe copies
#      that changed, no Kubernetes credential or a namespace that refuses the Job, a Job that
#      started no pod, a log that could not be read, an image the pod did not run, or a log with
#      no `JOB-RESULT` line. Nothing is proved either way.
#   3  the Job reached no verdict inside its deadline. Nothing is proved either way.
#
# Read the verdict from the probe's own `JOB-RESULT` line and never from the Job's phase: the probe
# exits 0 on a failed verdict as well, so the Job does not start a whole attempt again to reach the
# same one, and a Job reports `Succeeded` when its container exited 0 and nothing more. With a
# retry, the log read is the newest pod's whole log, read once its container has terminated.
#
# This script is synchronous, runs for as long as the Job does (an hour or more on the default
# repository), and its clean-up is a trap, so a role that runs it from its own bash tool runs it
# detached, the way docs/kubernetes.md, "Running a workload in the cluster from a role", sets out:
# the tool kills a call that runs past its timeout faster than any trap runs. Run it with
# `--dry-run` first:
#
#   log=$(mktemp "${TMPDIR:-/tmp}/pack-memory.XXXXXX")
#   flock "$log" setsid -f bash scripts/e2e/pack-memory-job.sh \
#     --image <digest> --namespace <namespace> --node-selector <key=value> \
#     --storage-class <class> --label <key=value> … --annotation <key=value> … \
#     > "$log" 2>&1 < /dev/null
#   flock "$log" cat "$log"     # waits for the run to end, then prints everything it printed
set -euo pipefail

job_ttl=3600             # after it finishes; docs/kubernetes.md allows 86400
pod_wait=900             # for the pod to exist: the compute class makes a machine first
terminated_wait=300      # for the newest pod's container to have terminated once the Job ended
poll=10                  # how often the wait reads the Job and its pods
ephemeral_storage=10Gi   # the container's own disk; the clone and every copy go on the volume

namespace_file=/var/run/secrets/kubernetes.io/serviceaccount/namespace
run_label_key=pack-memory-run

image=""
namespace=""
node_selector=""
storage_class=""
labels=()
annotations=()
cpu=8
memory=32Gi
disk=300Gi
deadline=28800
prefix=infra-4036-pack-memory-
repo=mozilla-firefox/firefox
walk_file=config/milestone.txt
dry_run=0

usage() {
  cat << 'USAGE'
usage: scripts/e2e/pack-memory-job.sh --image <ref>@sha256:<64 hex>
                                      --node-selector <key=value>[,<key=value>…]
                                      --storage-class <name>
                                      [--namespace <namespace>]
                                      [--label <key=value>]… [--annotation <key=value>]…
                                      [--cpu <quantity>] [--memory <quantity>] [--disk <quantity>]
                                      [--deadline <seconds>] [--prefix <name->]
                                      [--repo <owner>/<name>] [--walk-file <path>] [--dry-run]

  --image          the worker image to measure, by digest.
  --node-selector  the compute class the pod selects; it must admit runtimeClassName: gvisor.
  --storage-class  the storage class of the disk volume the clone goes on.
  --namespace      the namespace to run in; the default is the one the pod's credential names.
  --label          a label on the Job and on its pod template; repeatable.
  --annotation     an annotation on the Job and on its pod template; repeatable.
  --cpu            CPU, request and limit (default 8).
  --memory         memory, request and limit (default 32Gi).
  --disk           the disk volume's size (default 300Gi).
  --deadline       activeDeadlineSeconds of the Job and its pod, at most 43200 (default 28800).
  --prefix         the Job's metadata.generateName, ending in a hyphen.
  --repo           the public repository to measure (default mozilla-firefox/firefox).
  --walk-file      the file git log walks and the jj rows edit (default config/milestone.txt).
  --dry-run        render the manifest, validate it with kubectl (the namespace's admission where
                   this pod may create a Job, else kubectl's own schema), create nothing, and exit.

The values of the first three, of --namespace and of the labels and annotations are the
deployment's own; this public repository carries none of them.

exit statuses: 0 every verdict held; 1 the Job ran and a verdict did not hold; 2 the harness could
not run and nothing is proved; 3 the Job reached no verdict inside its deadline.
USAGE
}

refuse() {
  echo "pack-memory-job: $*" >&2
  echo >&2
  usage >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) dry_run=1; shift; continue ;;
    -h | --help) usage; exit 0 ;;
    --*=*) option=${1%%=*}; value=${1#*=}; shift ;;
    --*) [ $# -ge 2 ] || refuse "$1 takes a value"; option=$1; value=$2; shift 2 ;;
    *) refuse "unknown argument $1" ;;
  esac
  case "$option" in
    --image) image=$value ;;
    --namespace) namespace=$value ;;
    --node-selector) node_selector=$value ;;
    --storage-class) storage_class=$value ;;
    --label) labels+=("$value") ;;
    --annotation) annotations+=("$value") ;;
    --cpu) cpu=$value ;;
    --memory) memory=$value ;;
    --disk) disk=$value ;;
    --deadline) deadline=$value ;;
    --prefix) prefix=$value ;;
    --repo) repo=$value ;;
    --walk-file) walk_file=$value ;;
    *) refuse "unknown option $option" ;;
  esac
done

# yaml_quote <text>: the text as one YAML double-quoted scalar. A control character could change
# what the manifest says, so it is refused rather than escaped.
yaml_quote() {
  local text=$1
  [[ ! $text =~ [[:cntrl:]] ]] || return 1
  text=${text//\\/\\\\}
  text=${text//\"/\\\"}
  printf '"%s"' "$text"
}

# A digest and nothing else. A tag is mutable, and a measurement of an image nobody can name again
# is no measurement (docs/kubernetes.md, "Pin by digest").
digest_shape='^[A-Za-z0-9][A-Za-z0-9._:/-]*@sha256:[0-9a-f]{64}$'
[ -n "$image" ] || refuse "no --image: name the worker image to measure, by digest"
[[ $image =~ $digest_shape ]] || refuse "the image $image is not a digest reference <ref>@sha256:<64 hex>"
[ -n "$node_selector" ] || refuse "no --node-selector: the namespace's admission requires the deployment's compute class"
[ -n "$storage_class" ] || refuse "no --storage-class: the clone goes on a disk volume of the deployment's storage class"
[[ $storage_class =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]] || refuse "the storage class $storage_class is no Kubernetes name"
[[ $cpu =~ ^([0-9]+(\.[0-9]+)?|[0-9]+m)$ ]] || refuse "--cpu $cpu is no CPU quantity"
quantity='^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|k|M|G|T|P)?$'
[[ $memory =~ $quantity ]] || refuse "--memory $memory is no quantity"
[[ $disk =~ $quantity ]] || refuse "--disk $disk is no quantity"
[[ $deadline =~ ^[1-9][0-9]*$ ]] || refuse "--deadline $deadline is no whole number of seconds"
[ "$deadline" -le 43200 ] || refuse "--deadline $deadline is over 43200, the longest docs/kubernetes.md allows a workload"
case "$prefix" in
  *-) ;;
  *) refuse "the prefix $prefix does not end in a hyphen; the API server appends five characters to it" ;;
esac
[[ $repo =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || refuse "--repo $repo is no <owner>/<name>"
case "/$walk_file/" in
  //* | */../* | */./* | *//*) refuse "--walk-file $walk_file is no relative path inside the repository" ;;
esac
yaml_quote "$walk_file" > /dev/null || refuse "--walk-file holds a control character"
if [ -z "$namespace" ]; then
  [ -r "$namespace_file" ] || refuse "no --namespace and no $namespace_file: this pod holds no projected Kubernetes credential, so it can start no workload"
  namespace=$(cat "$namespace_file")
fi
[[ $namespace =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || refuse "the namespace $namespace is no Kubernetes name"

# The compute class, as the YAML lines a nodeSelector takes. Each pair is checked rather than
# pasted, so a mistyped input is a refusal here and not a Job the scheduler never places.
qualified='^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$'
label_value='^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$'
selector_yaml=""
IFS=, read -r -a selector_pairs <<< "$node_selector"
for pair in "${selector_pairs[@]}"; do
  [[ $pair == *=* ]] || refuse "the node selector entry $pair is no key=value pair"
  [[ ${pair%%=*} =~ $qualified ]] || refuse "the node selector key ${pair%%=*} is no label key"
  [[ ${pair#*=} =~ $label_value ]] || refuse "the node selector value ${pair#*=} is no label value"
  selector_yaml+="        ${pair%%=*}: $(yaml_quote "${pair#*=}")"$'\n'
done
[ -n "$selector_yaml" ] || refuse "the node selector named no pair"

# The labels and annotations, each on the Job and on its pod template. A key given twice is
# refused: which of the two the manifest kept would be a guess.
map_yaml() { # map_yaml <indent> <field> [<key=value>…]: one map field's YAML, `{}` when it is empty
  local indent=$1 field=$2 pair
  shift 2
  if [ $# -eq 0 ]; then
    printf '%s%s: {}\n' "$indent" "$field"
    return
  fi
  printf '%s%s:\n' "$indent" "$field"
  for pair; do
    printf '%s  %s: %s\n' "$indent" "${pair%%=*}" "$(yaml_quote "${pair#*=}")"
  done
}
declare -A seen=()
for pair in "${labels[@]}"; do
  [[ $pair == *=* ]] || refuse "the label $pair is no key=value pair"
  key=${pair%%=*}
  [[ $key =~ $qualified ]] || refuse "the label key $key is no label key"
  [ "$key" != "$run_label_key" ] || refuse "the label $run_label_key is this harness's own"
  if ! [[ ${pair#*=} =~ $label_value ]] || [ "${#pair}" -gt $((${#key} + 64)) ]; then
    refuse "the label value of $key is no label value: at most 63 letters, digits, -, _ and ."
  fi
  [ -z "${seen[label:$key]:-}" ] || refuse "the label $key is given twice"
  seen[label:$key]=1
done
for pair in "${annotations[@]}"; do
  [[ $pair == *=* ]] || refuse "the annotation $pair is no key=value pair"
  key=${pair%%=*}
  [[ $key =~ $qualified ]] || refuse "the annotation key $key is no annotation key"
  yaml_quote "${pair#*=}" > /dev/null || refuse "the annotation value of $key holds a control character"
  [ -z "${seen[annotation:$key]:-}" ] || refuse "the annotation $key is given twice"
  seen[annotation:$key]=1
done

command -v kubectl > /dev/null 2>&1 || refuse "no kubectl on PATH"
command -v go > /dev/null 2>&1 || refuse "no go on PATH: the role's git block is built from this checkout with go run"

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$here/../.." && pwd)
probe=$here/pack-memory-probe.sh
sampler=$here/lib/pack-memory-peak.py
[ -r "$probe" ] || refuse "the probe is not beside this script at $probe; it is part of this recipe"
[ -r "$sampler" ] || refuse "the sampler is not at $sampler; it is part of this recipe"
# Checked before anything is created, so a probe that is no valid bash or a sampler that is no
# valid Python is a refusal of this harness and not a Job that starts and reaches no verdict.
bash -n "$probe" || refuse "the probe $probe is not valid bash"
python3 -c 'import ast, sys; ast.parse(open(sys.argv[1]).read(), sys.argv[1])' "$sampler" ||
  refuse "the sampler $sampler is not valid Python"

# The commands the probe copies, held to the lines that build them. The probe runs each exactly as
# that line builds it; a line that changed is a probe that would measure another command.
holds_line() { # holds_line <file> <line>
  grep -qxF -- "$2" "$repo_root/$1" || refuse "$1 no longer holds the line the probe copies: [${2#"${2%%[![:space:]]*}"}]; change the probe with it"
}
holds_line packages/daemon/internal/shim/commands.go \
  $'\treturn []string{jj, "log", "-r", `@ ~ description(exact:"")`, "--no-graph", "-T", "commit_id", "-R", dir}'
holds_line packages/daemon/internal/shim/commands.go \
  $'\treturn []string{jj, "metaedit", "--update-author", "-r", `@ & description(exact:"")`, "-R", dir}'
holds_line packages/pi-shared/src/handoff-record.ts \
  'const STANDING = ["log", "-r", "@-", "--no-graph", "-T", "commit_id"] as const;'

# The jj release this checkout's image installs, which the image's `jj --version` must print
# (c-release-source): the version of worker.Dockerfile's one `ARG JJ_TOOL=<source>@<version>`.
dockerfile=packages/daemon/docker/worker.Dockerfile
mapfile -t jj_tool < <(grep -E '^ARG JJ_TOOL=' "$repo_root/$dockerfile" || true)
[ "${#jj_tool[@]}" -eq 1 ] || refuse "$dockerfile holds ${#jj_tool[@]} lines ARG JJ_TOOL=…, not one; the probe checks the image's jj --version against it"
jj_tool_version=${jj_tool[0]##*@}
[[ $jj_tool_version =~ ^[0-9A-Za-z][0-9A-Za-z.+-]*$ ]] || refuse "$dockerfile's [${jj_tool[0]}] names no version after its @"

# The role's GIT_CONFIG block, from this checkout's own Go list. Each line is NAME=value, the value
# everything after the first `=`; the names must come in the order a role's block has them.
block=$(cd "$repo_root/packages/daemon" && go run ./internal/runtime/sandbox/rolegitconfig) ||
  refuse "go run ./internal/runtime/sandbox/rolegitconfig failed in $repo_root/packages/daemon"
git_env_yaml=""
line_number=0
count=""
while IFS= read -r line; do
  variable=${line%%=*}
  value=${line#*=}
  [[ $line == *=* ]] || refuse "rolegitconfig printed [$line], which is no NAME=value line"
  if [ "$line_number" -eq 0 ]; then
    if [ "$variable" != GIT_CONFIG_COUNT ] || ! [[ $value =~ ^[0-9]+$ ]]; then
      refuse "rolegitconfig's first line is [$line], not GIT_CONFIG_COUNT=<n>"
    fi
    count=$value
  else
    pair=$(((line_number - 1) / 2))
    if [ $(((line_number - 1) % 2)) -eq 0 ]; then want=GIT_CONFIG_KEY_$pair; else want=GIT_CONFIG_VALUE_$pair; fi
    [ "$variable" = "$want" ] || refuse "rolegitconfig's line $((line_number + 1)) names $variable, not $want"
  fi
  quoted=$(yaml_quote "$value") || refuse "rolegitconfig's $variable holds a control character"
  git_env_yaml+="            - name: $variable"$'\n'"              value: $quoted"$'\n'
  line_number=$((line_number + 1))
done <<< "$block"
if [ -z "$count" ] || [ "$line_number" -ne $((1 + 2 * count)) ]; then
  refuse "rolegitconfig printed $line_number lines for GIT_CONFIG_COUNT=$count"
fi

# The image's revision label, which ties the measurement to a commit, where this pod can read it.
image_revision="unread: no crane on this pod's PATH"
if command -v crane > /dev/null 2>&1; then
  if config=$(crane config "$image" 2>&1); then
    image_revision=$(jq -r '.config.Labels["org.opencontainers.image.revision"] // "unread: the image carries no such label"' <<< "$config" 2>&1) ||
      image_revision="unread: its configuration could not be parsed"
  else
    image_revision="unread: crane config failed: $(head -n 1 <<< "$config")"
  fi
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/pack-memory-job.XXXXXX")
# Outside the checkout on purpose: a workspace is a jj working copy of this public repository, so
# a manifest written there would be snapshotted into the role's own commit.

# The two files written into the Job's command, each by a heredoc whose terminator is no line of it.
for terminator in PACK_MEMORY_PEAK_PY PACK_MEMORY_PROBE_SH; do
  ! grep -qx "$terminator" "$probe" "$sampler" || refuse "a line of the probe or the sampler is $terminator, the heredoc terminator the manifest writes them with"
done

workload=""
run_label=""

# shellcheck disable=SC2317 # the EXIT trap below is what runs it
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
      { echo "cleanup: the Job's delete failed"; [ "$status" -ne 0 ] || status=2; }
    setsid -w kubectl -n "$namespace" get job "$workload" --ignore-not-found
  elif [ -n "$run_label" ]; then
    echo "== cleanup: no Job name was read; deleting by label $run_label_key=$run_label"
    setsid -w kubectl -n "$namespace" delete job -l "$run_label_key=$run_label" --cascade=foreground --ignore-not-found ||
      { echo "cleanup: the Job's delete failed"; [ "$status" -ne 0 ] || status=2; }
    # An empty listing is no proof that none is left: a create the API server stored after this
    # listing leaves a Job this never saw. List it again once this script has ended, and delete it
    # with `kubectl -n <namespace> delete job -l pack-memory-run=<the printed value>
    # --cascade=foreground`.
    setsid -w kubectl -n "$namespace" get jobs -l "$run_label_key=$run_label"
  fi
  rm -rf "$work"
  echo "pack-memory-job: exit $status"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

# ---------------------------------------------------------------------------------------------
# The manifest. Every field below is one of the rules of docs/kubernetes.md, "Running a workload
# in the cluster from a role", or one the namespace's admission requires; the comments say which.
# ---------------------------------------------------------------------------------------------
render_manifest() { # render_manifest <run label> <file>
  {
    cat << YAML
apiVersion: batch/v1
kind: Job
metadata:
  # The API server names the run: five characters it appends at the create, so no name this script
  # picks can be one another workload of the namespace already holds. The label ${run_label_key}
  # is this run's own, on the Job alone: what the clean-up finds a Job by whose name it never read.
  generateName: ${prefix}
$(map_yaml "  " labels "$run_label_key=$1" "${labels[@]}")
$(map_yaml "  " annotations "${annotations[@]}")
spec:
  activeDeadlineSeconds: ${deadline}   # on the Job; at most 43200
  ttlSecondsAfterFinished: ${job_ttl}      # at most 86400
  backoffLimit: 6                          # a pod lost with its machine starts the run again elsewhere
  podReplacementPolicy: Failed             # and only once the lost pod has ended
  template:
    metadata:
$(map_yaml "      " labels "${labels[@]}")
$(map_yaml "      " annotations "${annotations[@]}")
    spec:
      restartPolicy: Never
      activeDeadlineSeconds: ${deadline} # on the pod template too; a manifest with one alone is refused
      terminationGracePeriodSeconds: 60
      automountServiceAccountToken: false    # it carries no identity, and needs none
      runtimeClassName: gvisor               # the cause measured is gVisor's
      nodeSelector:
${selector_yaml}      securityContext:
        # The worker container's own, as leak-guard-image-job.sh's Job on this image has it.
        runAsNonRoot: true
        runAsUser: 1000
        runAsGroup: 1000
        fsGroup: 1000                        # the disk volume's root is writable by the probe
      containers:
        - name: probe
          image: ${image}
          imagePullPolicy: IfNotPresent      # a digest names one image; a re-pull would fetch the same bytes
          terminationMessagePolicy: FallbackToLogsOnError
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
            seccompProfile:
              type: RuntimeDefault
          resources:
            requests:
              cpu: "${cpu}"
              memory: "${memory}"
              ephemeral-storage: "${ephemeral_storage}"
            limits:
              cpu: "${cpu}"
              memory: "${memory}"
              ephemeral-storage: "${ephemeral_storage}"
          env:
            - name: PACK_MEMORY_IMAGE
              value: $(yaml_quote "$image")
            - name: PACK_MEMORY_IMAGE_REVISION
              value: $(yaml_quote "${image_revision//$'\n'/ }")
            - name: PACK_MEMORY_JJ_TOOL_VERSION
              value: $(yaml_quote "$jj_tool_version")
            # The role's GIT_CONFIG block, built from this checkout (rolegitconfig).
${git_env_yaml}          volumeMounts:
            - name: scratch
              mountPath: /scratch
          # The image's ENTRYPOINT is \`legion\`; this Job writes the sampler and the probe onto its
          # volume and runs the probe instead.
          command: ["/bin/bash"]
          args:
            - "-c"
            - |
              set -eu
              mkdir -p /scratch/pack-memory
              cat > /scratch/pack-memory/pack-memory-peak.py << 'PACK_MEMORY_PEAK_PY'
YAML
    awk '{ if (length($0) > 0) printf "              %s\n", $0; else print "" }' "$sampler"
    cat << 'YAML'
              PACK_MEMORY_PEAK_PY
              cat > /scratch/pack-memory/pack-memory-probe.sh << 'PACK_MEMORY_PROBE_SH'
YAML
    awk '{ if (length($0) > 0) printf "              %s\n", $0; else print "" }' "$probe"
    cat << YAML
              PACK_MEMORY_PROBE_SH
              exec bash /scratch/pack-memory/pack-memory-probe.sh "\$@"
            - "pack-memory-probe"
            - "--repo"
            - $(yaml_quote "$repo")
            - "--walk-file"
            - $(yaml_quote "$walk_file")
      volumes:
        - name: scratch
          ephemeral:                         # the disk is made with the pod and deleted with it
            volumeClaimTemplate:
              spec:
                accessModes: ["ReadWriteOnce"]
                storageClassName: $(yaml_quote "$storage_class")
                resources:
                  requests:
                    storage: "${disk}"
YAML
  } > "$2"
}

run_id() { od -An -N4 -tx1 /dev/urandom | tr -d ' \n'; }

echo "== pack-memory-job"
echo "namespace:     $namespace"
echo "image:         $image"
echo "revision:      $image_revision"
echo "checkout:      $repo_root (git HEAD $(git -C "$repo_root" rev-parse HEAD 2> /dev/null || echo unread))"
echo "repository:    $repo, walking $walk_file"
echo "jj release:    $jj_tool_version, as $dockerfile's ARG JJ_TOOL names it"
echo "resources:     $cpu CPU, $memory memory, $disk disk of class $storage_class, deadline $deadline s"
echo "selector:"
printf '%s' "$selector_yaml"
echo "the role's git block, from this checkout:"
printf '  %s\n' "${block//$'\n'/$'\n'  }"

if [ "$dry_run" -eq 1 ]; then
  manifest=$work/job-dry-run.yaml
  render_manifest "$(run_id)" "$manifest"
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
      echo "pack-memory-job: the namespace's admission refused the manifest" >&2
      exit 2
    }
  else
    echo "== this pod may not create a Job here, so the manifest is validated client-side only"
    echo "== kubectl create --dry-run=client -f <the manifest above>"
    kubectl create --dry-run=client -f "$manifest" || {
      echo "pack-memory-job: kubectl refused the manifest" >&2
      exit 2
    }
  fi
  exit 0
fi

# The gate. Only `create jobs` is asked for as a condition: this workload carries no Service,
# because nothing reaches it over the network; it is read by its log. The other three answers are
# printed because a run that cannot read a pod or its log proves nothing either.
echo
echo "== what this pod may do in $namespace"
can_create=$(kubectl -n "$namespace" auth can-i create jobs 2>&1 || true)
echo "create jobs:   $can_create"
echo "get pods:      $(kubectl -n "$namespace" auth can-i get pods 2>&1 || true)"
echo "get pods/log:  $(kubectl -n "$namespace" auth can-i get pods/log 2>&1 || true)"
echo "delete jobs:   $(kubectl -n "$namespace" auth can-i delete jobs 2>&1 || true)"
if [ "$can_create" != yes ]; then
  echo "pack-memory-job: this pod may not create a Job in $namespace, so nothing is measured here" >&2
  exit 2
fi

run_label=$(run_id)
manifest=$work/job.yaml
render_manifest "$run_label" "$manifest"
echo
echo "== creating the Job ($run_label_key=$run_label)"
created=$(kubectl -n "$namespace" create -o jsonpath='{.metadata.name} {.metadata.uid}' -f "$manifest") || {
  echo "pack-memory-job: the create was refused; nothing of this run is left" >&2
  exit 2
}
workload=${created% *}
echo "workload: $workload (uid ${created#* })"

if ! kubectl -n "$namespace" wait --for=create pod -l "job-name=$workload" --timeout="${pod_wait}s"; then
  echo "no pod of $workload exists after ${pod_wait}s"
  kubectl -n "$namespace" get job "$workload" -o wide || true
  kubectl -n "$namespace" describe job "$workload" | tail -n 20 || true
  exit 2
fi

# The labels first: they are what a role compares when a workload answers nothing, and they are
# gone with the pod once the trap has run.
echo
echo "== the pod's labels"
kubectl -n "$namespace" get pods -l "job-name=$workload" --show-labels || true

# pods_line: every attempt, oldest first, with its phase and its container's state.
pods_line() {
  kubectl -n "$namespace" get pods -l "job-name=$workload" --sort-by=.metadata.creationTimestamp \
    -o jsonpath='{range .items[*]}{.metadata.name}={.status.phase}/{.status.containerStatuses[0].state.terminated.reason}{" "}{end}' 2> /dev/null || true
}

echo
echo "== waiting for the Job to end (at most $deadline s, its deadline, and $pod_wait s more)"
# The Job's phase is a request, not a result: a Job can report Running with no pod at all when the
# namespace's quota is full. So the pods are read at every poll beside the Job's conditions.
wait_until=$((SECONDS + deadline + pod_wait))
seen_pods=""
conditions=""
while :; do
  conditions=$(kubectl -n "$namespace" get job "$workload" \
    -o jsonpath='{range .status.conditions[?(@.status=="True")]}{.type}:{.reason} {end}' 2> /dev/null || true)
  now=$(pods_line)
  if [ "$now" != "$seen_pods" ]; then
    echo "  [$(date -u +%FT%TZ)] pods: [${now:-none}] job conditions: [${conditions:-none}]"
    seen_pods=$now
  fi
  case " $conditions " in
    *" Complete:"* | *" Failed:"*) break ;;
  esac
  if [ "$SECONDS" -ge "$wait_until" ]; then
    echo "the Job reached no condition inside $((deadline + pod_wait)) s"
    exit 3
  fi
  sleep "$poll"
done
echo "  job conditions: [$conditions]"

# The newest attempt is the one whose log holds the verdict. It is read once its container has
# terminated: a read of a running container can end early and look like a run that stopped.
pod=$(kubectl -n "$namespace" get pods -l "job-name=$workload" --sort-by=.metadata.creationTimestamp \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2> /dev/null | tail -n 1)
[ -n "$pod" ] || {
  echo "the Job $workload ended with no pod left to read"
  exit 2
}
echo "the newest attempt: $pod"
terminated=""
terminated_until=$((SECONDS + terminated_wait))
while [ "$SECONDS" -lt "$terminated_until" ]; do
  terminated=$(kubectl -n "$namespace" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].state.terminated.reason}' 2> /dev/null || true)
  [ -z "$terminated" ] || break
  sleep 5
done
echo "  its container: ${terminated:-not terminated after $terminated_wait s}"

# The image the pod actually ran, which is what ties everything below to the digest named above.
ran=$(kubectl -n "$namespace" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].imageID}' 2> /dev/null || true)
echo "  imageID: ${ran:-unreadable}"

echo
echo "== the log of $pod"
log=$work/probe.log
if ! kubectl -n "$namespace" logs "pod/$pod" --tail=-1 > "$log" 2>&1; then
  echo "the pod's log could not be read"
  cat "$log" || true
  exit 2
fi
cat "$log"

digest=${image#*@}
case "$ran" in
  *"$digest"*) ;;
  *)
    echo
    echo "the pod ran imageID [${ran:-unreadable}], which does not name the digest $digest"
    exit 2
    ;;
esac

echo
echo "======== the verdict"
echo "image: $image"
echo "attempts: $(pods_line)"
if grep -qx 'JOB-RESULT pass' "$log"; then
  echo "pass: every verdict held"
  exit 0
elif grep -qx 'JOB-RESULT fail' "$log"; then
  echo "fail: the log's MISMATCH lines say which verdicts did not hold"
  exit 1
fi
echo "the log carries no JOB-RESULT line, so it decided nothing"
case " $conditions " in
  *" Failed:DeadlineExceeded "*) exit 3 ;;
esac
exit 2
