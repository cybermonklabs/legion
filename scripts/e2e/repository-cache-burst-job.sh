#!/usr/bin/env bash
# The burst Job of check o2 of INFRA-5789 (verification of the repository cache's bucket): with the
# cache's bucket on, 20 `workspace-init fetch` runs at once leave the cache pod's CPU below 0.2 CPU,
# each logging the bucket as its source; with it off, the same 20 read /tar, each logging that /packs
# is not served, and the cache's CPU rises above 0.5 CPU. One Kubernetes Job, on the worker image the
# caller names by digest, under gVisor, runs the image's own `legion workspace-init cache` and
# `workspace-init fetch` against a double of Cloud Storage, IAM signBlob and the metadata server
# (packages/daemon/internal/repocache/fakegcs), because no identity a Job may use can sign an
# address (iam.serviceAccounts.signBlob). What this proves is the cache's CPU and each fetch's
# source; that Cloud Storage accepts a real signature is the production check's to prove, and is no
# claim of this run.
#
# It is three files: this harness, which renders the Job, creates it and reads its verdict;
# `repository-cache-burst-probe.sh` beside it, the text the Job's containers run, whose header says
# what each container does and how the run is judged; and the sources of the double, which this
# harness takes from its own checkout. A Job holds no checkout, so this harness writes the probe and
# the double's sources into the Job's command itself, and the Job compiles the double with the
# worker image's Go. None is run or changed without the others, and the head the checkout holds is
# the head measured: run this from a checkout of the commit whose worker image `--image` names.
#
# The Job is one pod of three containers and two init containers, on one disk volume mounted at
# /scratch in each:
#
#   double    compiles and serves the double on loopback. Its CPU is not measured.
#   cache     the conductor and the measured container: the image's `workspace-init cache`, with a
#             CPU request and limit of exactly the production cache's default, 1 CPU and 4Gi, which
#             nothing here changes, and the sampler of its own cgroup's cumulative CPU counter.
#   fetchers  20 `workspace-init fetch` runs at once, released together by a file on the volume.
#   init      two init containers write the probe and the double's sources onto the volume first.
#
# Inputs. Every value private to a deployment is an input and never a literal here: this repository
# is public, and its deployment's namespace, compute class, storage class and the labels and
# annotations its admission requires are in that deployment's own declaration.
#
#   --image <ref>@sha256:<64 hex>   required: the worker image to test, by digest, never by tag. The
#                                   digest is in the `Record digest` step of the `Worker Image` run
#                                   of the commit measured (docs/kubernetes.md, "Pin by digest,
#                                   never by tag").
#   --node-selector <key=value>[,…] required: the compute class the pod selects, one that admits
#                                   `runtimeClassName: gvisor`, which this Job always sets.
#   --storage-class <name>          required: the storage class of the disk volume at /scratch.
#   --namespace <namespace>         the namespace to run in. The default is the one the pod's own
#                                   credential names (/var/run/secrets/kubernetes.io/
#                                   serviceaccount/namespace).
#   --label <key=value>             repeatable: a label on the Job and on its pod template, as the
#                                   namespace's admission requires. A deployment whose admission
#                                   lowers a CPU request that equals its limit opts a timed run out
#                                   with a pod-template label of its own, which goes here; the cache
#                                   container's request is read back after admission and the run
#                                   refused if it is not its limit.
#   --annotation <key=value>        repeatable: an annotation on both, the same way (the cost
#                                   annotations go here).
#   --job-settings <file>           the deployment's selector, storage class, namespace, labels and
#                                   annotations in one file, one `<name>=<value>` line each, where the
#                                   name is node-selector, storage-class, namespace, service-account,
#                                   label or annotation (the last two repeat); a blank line and a line
#                                   starting with # are skipped. A flag on the command line wins over
#                                   the file for a name that takes one value, and a label or an
#                                   annotation of the file is added to those given as flags.
#   --service-account <name>        the pod's ServiceAccount, a deployment's own account that holds
#                                   no grant where its namespace has one (default none: the
#                                   namespace's default account). The pod mounts no token either way.
#   --disk <quantity>               the disk volume's size (default 120Gi). The default repository's
#                                   one pack and index are 1,777,396,269 bytes (measured 2026-11-26
#                                   in Job infra-5789-cache-burst-fbmgg); the cache's clone, the
#                                   double's copy of both and 20 fresh feeds at once are 22 copies,
#                                   about 39 GB, and the 120Gi (129 GB) leaves room. The probe reads
#                                   the clone's real size and refuses (void) when the volume cannot
#                                   hold 20 feeds beside it.
#   --fetch-cpu <quantity>          the fetchers container's CPU, request and limit (default 16),
#                                   sized for 20 `git fsck --connectivity-only` runs at once: on
#                                   firefox's history, 20 of them on 8 CPU ran out of both the
#                                   bucket attempt's 15-minute bound and /tar's 30-minute bound;
#                                   on the default repository, 16 CPU ran each of the 20 fetches
#                                   in 91 to 208 s (Job infra-5789-cache-burst-fbmgg).
#   --fetch-memory <quantity>       its memory, request and limit (default 32Gi), which is no
#                                   measurement: 20 `git fsck --connectivity-only` and 20 SHA-256
#                                   passes over one pack run in this container at once.
#   --double-cpu <quantity>         the double's CPU (default 4) and memory (--double-memory, default
#                                   4Gi), request and limit: it serves the 20 downloads.
#   --deadline <seconds>            activeDeadlineSeconds on the Job and on its pod template
#                                   (default 21600; at most 43200, which docs/kubernetes.md allows).
#   --prefix <generateName prefix>  the Job's metadata.generateName, ending in a hyphen (default
#                                   infra-5789-cache-burst-).
#   --repo <owner>/<name>           the public repository the cache clones (default nodejs/node:
#                                   one pack and index of 1,777,396,269 bytes).
#   --dry-run                       render the manifest, validate it with kubectl (the namespace's
#                                   admission where this pod may create a Job, else kubectl's own
#                                   schema), read back the cache container's CPU request after
#                                   admission, create nothing, and exit.
#
# Fixed here: gVisor; both deadlines; ttlSecondsAfterFinished; backoffLimit 2 with
# podReplacementPolicy Failed, so a pod lost with its machine is followed by one whole new attempt
# and never by a second beside it, and a kill that repeats does not burn hours; no service account
# token; every container's security context (uid, gid and fsGroup 1000, no privilege escalation,
# every capability dropped, the RuntimeDefault seccomp profile); requests equal to limits on every
# container, and the same for both arms, since the arms run in the one pod; and the image reference,
# the repository, the checkout's commit and the image's org.opencontainers.image.revision label on
# the containers' environment, for the probe to print. That label is read with `crane config` where
# this pod has crane, and printed as unread where it has not. Where it is read and is not the
# checkout's commit, a real run is refused: the double's sources and the image's cache would be two
# heads.
#
# fsGroup: the disk volume is an ephemeral claim, whose root is root:root 0755 unless the pod's
# securityContext sets fsGroup. The namespace's admission refuses an fsGroup only for a pod that
# mounts a persistentVolumeClaim volume, which an ephemeral volume is not, so the pod sets fsGroup
# 1000 as the fsGroup of the other Jobs here does (the server dry run is what says it holds).
#
# Exit statuses:
#
#   0  the probe's every check held: its log ends `JOB-RESULT pass`.
#   1  the Job ran and a check did not hold: its log ends `JOB-RESULT fail`, and its `CHECK` lines
#      say which.
#   2  the harness could not run, or the run proved nothing: a usage error, a missing input, a
#      source the double needs that changed shape, no Kubernetes credential or a namespace that
#      refuses the Job, a Job that started no pod, a log that could not be read, an image the pod did
#      not run, a log with no `JOB-RESULT` line, or `JOB-RESULT void` (the attribution check failed,
#      the control arm did not discriminate, the double or a clone did not come up).
#   3  the Job reached no verdict inside its deadline. Nothing is proved either way.
#
# Read the verdict from the probe's own `JOB-RESULT` line and never from the Job's phase: the probe
# exits 0 on a failed verdict as well, so the Job does not start a whole attempt again to reach the
# same one, and a Job reports `Succeeded` when its containers exited 0 and nothing more
# (skills/legion-worker/references/tools.md). With a retry, the log read is the newest pod's whole
# log, read once its container has terminated.
#
# This script is synchronous, runs for as long as the Job does (about ten minutes on the default
# repository, which is its first clone, two uploads and two bursts), and its clean-up is a trap, so
# a role that runs it from its own bash tool runs it detached, the way docs/kubernetes.md, "Running
# a workload in the cluster from a role", sets out: the tool kills a call that runs past its timeout
# faster than any trap runs. Run it with `--dry-run` first:
#
#   log=$(mktemp "${TMPDIR:-/tmp}/repository-cache-burst.XXXXXX")
#   flock "$log" setsid -f bash scripts/e2e/repository-cache-burst-job.sh \
#     --image <digest> --namespace <namespace> --job-settings <file> \
#     > "$log" 2>&1 < /dev/null
#   flock "$log" cat "$log"     # waits for the run to end, then prints everything it printed
set -euo pipefail

job_ttl=3600             # after it finishes; docs/kubernetes.md allows 86400
pod_wait=900             # for the pod to exist: the compute class makes a machine first
terminated_wait=300      # for the newest pod's cache container to have terminated once the Job ended
poll=10                  # how often the wait reads the Job and its pods
script_limit=120000      # bytes of one container's script: one argument is at most 128 KiB (131072)
ephemeral_storage=2Gi    # each container's own disk; every tree goes on the volume
cache_cpu=1              # the cache container's CPU and memory, each request equal to its limit: the
cache_memory=4Gi         # production default (runtime.kubernetes.repository_cache.resources)

namespace_file=/var/run/secrets/kubernetes.io/serviceaccount/namespace
run_label_key=repository-cache-burst-run

image=""
namespace=""
node_selector=""
storage_class=""
service_account=""
settings_file=""
labels=()
annotations=()
disk=120Gi
fetch_cpu=16
fetch_memory=32Gi
double_cpu=4
double_memory=4Gi
deadline=21600
prefix=infra-5789-cache-burst-
repo=nodejs/node
dry_run=0

usage() {
  cat << 'USAGE'
usage: scripts/e2e/repository-cache-burst-job.sh --image <ref>@sha256:<64 hex>
                                                 --node-selector <key=value>[,<key=value>…]
                                                 --storage-class <name>
                                                 [--namespace <namespace>] [--job-settings <file>]
                                                 [--label <key=value>]… [--annotation <key=value>]…
                                                 [--service-account <name>] [--disk <quantity>]
                                                 [--fetch-cpu <quantity>] [--fetch-memory <quantity>]
                                                 [--double-cpu <quantity>] [--double-memory <quantity>]
                                                 [--deadline <seconds>] [--prefix <name->]
                                                 [--repo <owner>/<name>] [--dry-run]

  --image            the worker image to test, by digest.
  --node-selector    the compute class the pod selects; it must admit runtimeClassName: gvisor.
  --storage-class    the storage class of the disk volume the clone and the feeds go on.
  --namespace        the namespace to run in; the default is the one the pod's credential names.
  --job-settings     a file of `<name>=<value>` lines holding the deployment's node-selector,
                     storage-class, namespace, service-account, label and annotation values.
  --label            a label on the Job and on its pod template; repeatable.
  --annotation       an annotation on the Job and on its pod template; repeatable.
  --service-account  the pod's ServiceAccount (default none: the namespace's default account).
  --disk             the disk volume's size (default 120Gi).
  --fetch-cpu        the fetchers container's CPU, request and limit (default 16).
  --fetch-memory     its memory, request and limit (default 32Gi).
  --double-cpu       the double's CPU, request and limit (default 4).
  --double-memory    its memory, request and limit (default 4Gi).
  --deadline         activeDeadlineSeconds of the Job and its pod, at most 43200 (default 21600).
  --prefix           the Job's metadata.generateName, ending in a hyphen.
  --repo             the public repository the cache clones (default nodejs/node).
  --dry-run          render the manifest, validate it with kubectl (the namespace's admission where
                     this pod may create a Job, else kubectl's own schema), create nothing, and exit.

The values of the first three, of --namespace, of --service-account and of the labels and
annotations are the deployment's own; this public repository carries none of them.

exit statuses: 0 every check held; 1 the Job ran and a check did not hold; 2 the harness could not
run, or the run proved nothing; 3 the Job reached no verdict inside its deadline.
USAGE
}

refuse() {
  echo "repository-cache-burst-job: $*" >&2
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
    --service-account) service_account=$value ;;
    --job-settings) settings_file=$value ;;
    --label) labels+=("$value") ;;
    --annotation) annotations+=("$value") ;;
    --disk) disk=$value ;;
    --fetch-cpu) fetch_cpu=$value ;;
    --fetch-memory) fetch_memory=$value ;;
    --double-cpu) double_cpu=$value ;;
    --double-memory) double_memory=$value ;;
    --deadline) deadline=$value ;;
    --prefix) prefix=$value ;;
    --repo) repo=$value ;;
    *) refuse "unknown option $option" ;;
  esac
done

# The settings file: what a flag of the command line did not give. A name that takes one value is
# the flag's when both name it; labels and annotations are added to the flags'.
if [ -n "$settings_file" ]; then
  [ -r "$settings_file" ] || refuse "--job-settings $settings_file is not a readable file"
  line_number=0
  service_account_flag=0
  [ -z "$service_account" ] || service_account_flag=1
  while IFS= read -r line || [ -n "$line" ]; do
    line_number=$((line_number + 1))
    line=${line%$'\r'}
    [[ $line =~ ^[[:space:]]*(#|$) ]] && continue
    [[ $line == *=* ]] || refuse "$settings_file line $line_number is no <name>=<value> line"
    name=${line%%=*}
    value=${line#*=}
    case "$name" in
      node-selector) [ -n "$node_selector" ] || node_selector=$value ;;
      storage-class) [ -n "$storage_class" ] || storage_class=$value ;;
      namespace) [ -n "$namespace" ] || namespace=$value ;;
      service-account) [ "$service_account_flag" -eq 1 ] || service_account=$value ;;
      label) labels+=("$value") ;;
      annotation) annotations+=("$value") ;;
      *) refuse "$settings_file line $line_number names $name: the names are node-selector, storage-class, namespace, service-account, label and annotation" ;;
    esac
  done < "$settings_file"
fi

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
[ -n "$image" ] || refuse "no --image: name the worker image to test, by digest"
[[ $image =~ $digest_shape ]] || refuse "the image $image is not a digest reference <ref>@sha256:<64 hex>"
[ -n "$node_selector" ] || refuse "no --node-selector: the namespace's admission requires the deployment's compute class"
[ -n "$storage_class" ] || refuse "no --storage-class: the clone goes on a disk volume of the deployment's storage class"
[[ $storage_class =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]] || refuse "the storage class $storage_class is no Kubernetes name"
[ -z "$service_account" ] || [[ $service_account =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]] || refuse "the service account $service_account is no Kubernetes name"
# The pod template's ServiceAccount line, none where no account was given: the namespace's default.
service_account_yaml=""
[ -z "$service_account" ] || service_account_yaml="      serviceAccountName: ${service_account}  # the deployment's account that holds no grant: the cache signs with the double"$'\n'
cpu_shape='^([0-9]+(\.[0-9]+)?|[0-9]+m)$'
quantity='^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|k|M|G|T|P)?$'
[[ $fetch_cpu =~ $cpu_shape ]] || refuse "--fetch-cpu $fetch_cpu is no CPU quantity"
[[ $double_cpu =~ $cpu_shape ]] || refuse "--double-cpu $double_cpu is no CPU quantity"
[[ $fetch_memory =~ $quantity ]] || refuse "--fetch-memory $fetch_memory is no quantity"
[[ $double_memory =~ $quantity ]] || refuse "--double-memory $double_memory is no quantity"
[[ $disk =~ $quantity ]] || refuse "--disk $disk is no quantity"
[[ $deadline =~ ^[1-9][0-9]*$ ]] || refuse "--deadline $deadline is no whole number of seconds"
[ "$deadline" -le 43200 ] || refuse "--deadline $deadline is over 43200, the longest docs/kubernetes.md allows a workload"
case "$prefix" in
  *-) ;;
  *) refuse "the prefix $prefix does not end in a hyphen; the API server appends five characters to it" ;;
esac
[[ $repo =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || refuse "--repo $repo is no <owner>/<name>"
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
command -v jq > /dev/null 2>&1 || refuse "no jq on PATH: the cache container's admitted CPU request is read back with it"

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$here/../.." && pwd)
probe=$here/repository-cache-burst-probe.sh
probe_name=repository-cache-burst-probe.sh
[ -r "$probe" ] || refuse "the probe is not beside this script at $probe; it is part of this recipe"
# Checked before anything is created, so a probe that is no valid bash is a refusal of this harness
# and not a Job that starts and reaches no verdict.
bash -n "$probe" || refuse "the probe $probe is not valid bash"

# The double: every non-test Go file of package fakegcs and of its command, as the module path of
# the daemon holds them, which the Job compiles in a scratch module with the worker image's Go. The
# files import the standard library alone, bar the command's import of the package, which is checked
# here so a later import of another module is a refusal and not a build that cannot fetch it.
double_pkg=packages/daemon/internal/repocache/fakegcs
double_files=()
for f in "$repo_root/$double_pkg"/*.go "$repo_root/$double_pkg"/cmd/fakegcs/*.go; do
  [ -e "$f" ] || continue
  case "$f" in *_test.go) continue ;; esac
  double_files+=("$f")
done
[ "${#double_files[@]}" -gt 0 ] || refuse "no Go source of the double under $double_pkg: the burst runs the double the cache's tests use"
[ -e "$repo_root/$double_pkg/cmd/fakegcs/main.go" ] || refuse "$double_pkg/cmd/fakegcs/main.go is missing: the Job builds the double from its command"
for f in "${double_files[@]}"; do
  outside=$(awk '
    /^import \(/ { block = 1; next }
    block && /^\)/ { block = 0 }
    block || /^import "/ { if (match($0, /"[^"]+"/)) print substr($0, RSTART + 1, RLENGTH - 2) }' "$f" |
    grep -E '^[^/]*\.' | grep -v '^github.com/sjawhar/legion/daemon/internal/repocache/fakegcs$' || true)
  [ -z "$outside" ] || refuse "${f#"$repo_root"/} imports $outside, which the Job's scratch module cannot fetch: the double is standard-library only"
done
go_directive=$(grep -m 1 -E '^go [0-9]' "$repo_root/packages/daemon/go.mod" || true)
[[ $go_directive =~ ^go\ [0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || refuse "packages/daemon/go.mod holds no go line the scratch module can repeat"

# The tested revision: the checkout's commit, and whether the files the Job carries are that commit's.
checkout_head=$(git -C "$repo_root" rev-parse HEAD 2> /dev/null || echo unread)
checkout_state=$(git -C "$repo_root" status --porcelain -- "$double_pkg" "scripts/e2e/$probe_name" 2> /dev/null | wc -l || true)
if [ "$checkout_head" = unread ]; then
  tested_revision="unread: $repo_root is no git checkout"
elif [ "${checkout_state// /}" != 0 ]; then
  tested_revision="$checkout_head, with ${checkout_state// /} changed file(s) of the double or the probe in the working copy"
else
  tested_revision=$checkout_head
fi

# The image's revision label, which ties the run to a commit, where this pod can read it.
image_revision="unread: no crane on this pod's PATH"
if command -v crane > /dev/null 2>&1; then
  if config=$(crane config "$image" 2>&1); then
    image_revision=$(jq -r '.config.Labels["org.opencontainers.image.revision"] // "unread: the image carries no such label"' <<< "$config" 2>&1) ||
      image_revision="unread: its configuration could not be parsed"
  else
    image_revision="unread: crane config failed: $(head -n 1 <<< "$config")"
  fi
fi

# The clean-up is installed before anything it removes is made, so a refusal from here on leaves
# nothing behind.
work=""
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
    # with `kubectl -n <namespace> delete job -l repository-cache-burst-run=<the printed value>
    # --cascade=foreground`.
    setsid -w kubectl -n "$namespace" get jobs -l "$run_label_key=$run_label"
  fi
  [ -z "$work" ] || rm -rf "$work"
  echo "repository-cache-burst-job: exit $status"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

# Outside the checkout on purpose: a workspace is a jj working copy of this public repository, so
# a manifest written there would be snapshotted into the role's own commit.
work=$(mktemp -d "${TMPDIR:-/tmp}/repository-cache-burst-job.XXXXXX")

# The two scripts written into the Job's init containers, each file by a heredoc whose terminator is
# no line of it. The first stages the double's module, the second the probe and the run's tokens.
for index in "${!double_files[@]}"; do
  ! grep -qx "BURST_SRC_$index" "${double_files[$index]}" || refuse "a line of ${double_files[$index]#"$repo_root"/} is BURST_SRC_$index, the heredoc terminator the manifest writes it with"
done
! grep -qx BURST_PROBE_SH "$probe" || refuse "a line of the probe is BURST_PROBE_SH, the heredoc terminator the manifest writes it with"

# write_file <path> <terminator> <source>: one file as a heredoc of the script, with its final line
# feed made sure of, so the terminator is a line of its own.
write_file() {
  printf "cat > %s << '%s'\n" "$1" "$2"
  cat "$3"
  if [ -n "$(tail -c 1 "$3")" ]; then printf '\n'; fi
  printf '%s\n' "$2"
}

stage_double_script() {
  local index rel
  printf 'set -eu\n'
  printf 'mkdir -p /scratch/burst/module/internal/repocache/fakegcs/cmd/fakegcs\n'
  printf "cat > /scratch/burst/module/go.mod << 'BURST_GOMOD'\nmodule github.com/sjawhar/legion/daemon\n\n%s\nBURST_GOMOD\n" "$go_directive"
  for index in "${!double_files[@]}"; do
    rel=${double_files[$index]#"$repo_root"/packages/daemon/}
    write_file "/scratch/burst/module/$rel" "BURST_SRC_$index" "${double_files[$index]}"
  done
  printf 'echo "staged the double: %s Go files"\n' "${#double_files[@]}"
}

stage_probe_script() {
  printf 'set -eu\n'
  printf 'mkdir -p /scratch/burst /scratch/run/secrets\n'
  write_file "/scratch/burst/$probe_name" BURST_PROBE_SH "$probe"
  cat << 'SCRIPT'
umask 077
od -An -N16 -tx1 /dev/urandom | tr -d ' \n' > /scratch/run/secrets/provision-token
od -An -N16 -tx1 /dev/urandom | tr -d ' \n' > /scratch/run/secrets/cache-token
printf 'Authorization: Bearer %s\n' "$(cat /scratch/run/secrets/cache-token)" > /scratch/run/secrets/auth-header
echo "staged the probe and two throwaway tokens"
SCRIPT
}

stage_double_script > "$work/stage-double.sh"
stage_probe_script > "$work/stage-probe.sh"
for script in "$work/stage-double.sh" "$work/stage-probe.sh"; do
  size=$(wc -c < "$script")
  [ "$size" -lt "$script_limit" ] || refuse "$(basename "$script") is $size bytes, over $script_limit: a container argument over 131072 bytes is not executed"
done

# indent_script <file>: the script as the lines of a YAML block scalar 14 spaces in, blank lines
# kept blank.
indent_script() {
  awk '{ if (length($0) > 0) printf "              %s\n", $0; else print "" }' "$1"
}

# container_env: the environment every container carries, for the probe to print and judge by.
container_env() {
  cat << YAML
          env:
            - name: BURST_REPO
              value: $(yaml_quote "$repo")
            - name: BURST_IMAGE
              value: $(yaml_quote "$image")
            - name: BURST_IMAGE_REVISION
              value: $(yaml_quote "${image_revision//$'\n'/ }")
            - name: BURST_REVISION
              value: $(yaml_quote "$tested_revision")
            - name: BURST_DEADLINE
              value: "${deadline}"
YAML
}

# container <name> <cpu> <memory> <role>: one regular container, running the probe's role of that name.
container() {
  cat << YAML
        - name: $1
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
              cpu: "$2"
              memory: "$3"
              ephemeral-storage: "${ephemeral_storage}"
            limits:
              cpu: "$2"
              memory: "$3"
              ephemeral-storage: "${ephemeral_storage}"
YAML
  container_env
  cat << YAML
          volumeMounts:
            - name: scratch
              mountPath: /scratch
          # The image's ENTRYPOINT is \`legion\`; the probe's role is run instead.
          command: ["/bin/bash", "/scratch/burst/${probe_name}", "$4"]
YAML
}

# init_container <name> <script file>: an init container that runs the script.
init_container() {
  cat << YAML
        - name: $1
          image: ${image}
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
            seccompProfile:
              type: RuntimeDefault
          resources:
            requests:
              cpu: "250m"
              memory: "256Mi"
              ephemeral-storage: "${ephemeral_storage}"
            limits:
              cpu: "250m"
              memory: "256Mi"
              ephemeral-storage: "${ephemeral_storage}"
          volumeMounts:
            - name: scratch
              mountPath: /scratch
          command: ["/bin/bash", "-c"]
          args:
            - |
YAML
  indent_script "$2"
}

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
  backoffLimit: 2                          # a pod lost with its machine starts the run again elsewhere, once or twice
  podReplacementPolicy: Failed             # and only once the lost pod has ended
  template:
    metadata:
$(map_yaml "      " labels "${labels[@]}")
$(map_yaml "      " annotations "${annotations[@]}")
    spec:
      restartPolicy: Never
      activeDeadlineSeconds: ${deadline} # on the pod template too; a manifest with one alone is refused
      terminationGracePeriodSeconds: 60
${service_account_yaml}      automountServiceAccountToken: false    # it carries no Kubernetes identity either, and needs none
      runtimeClassName: gvisor               # the cause measured is gVisor's
      nodeSelector:
${selector_yaml}      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        runAsGroup: 1000
        fsGroup: 1000                        # the disk volume's root is writable by every container
      initContainers:
YAML
    init_container stage-double "$work/stage-double.sh"
    init_container stage-probe "$work/stage-probe.sh"
    echo "      containers:"
    # The cache's reservation is the production default (cache_cpu and cache_memory), request equal
    # to limit, the same in both arms because both run in this pod.
    container double "$double_cpu" "$double_memory" double
    container cache "$cache_cpu" "$cache_memory" cache
    container fetchers "$fetch_cpu" "$fetch_memory" fetchers
    cat << YAML
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

# print_manifest <file>: the manifest with each embedded file's lines left out, since the server is
# sent the whole of them and a reader of this log has the probe and the double's sources in the
# checkout: the heredoc's first and last line stay, and the count of lines between them is said.
print_manifest() {
  awk '
    skipping {
      t = $0; sub(/^ +/, "", t)
      if (t == terminator) { printf "              # … %d lines of the file above\n", skipped; skipping = 0; print; next }
      skipped++; next
    }
    match($0, /<< .BURST_[A-Z0-9_]+.$/) {
      terminator = substr($0, RSTART + 4, RLENGTH - 5); skipping = 1; skipped = 0
    }
    { print }' "$1"
}

# cpu_milli <quantity> and mem_mib <quantity>: a quantity as whole milli-CPUs and whole MiB.
cpu_milli() {
  if [[ $1 == *m ]]; then echo "${1%m}"; else awk -v q="$1" 'BEGIN { printf "%d", q * 1000 + 0.5 }'; fi
}
mem_mib() {
  awk -v q="$1" 'BEGIN {
    n = q + 0; u = q; sub(/^[0-9.]+/, "", u)
    f["Ki"] = 1 / 1024; f["Mi"] = 1; f["Gi"] = 1024; f["Ti"] = 1048576; f["Pi"] = 1073741824
    f["k"] = 1000 / 1048576; f["M"] = 1e6 / 1048576; f["G"] = 1e9 / 1048576; f["T"] = 1e12 / 1048576; f["P"] = 1e15 / 1048576
    printf "%d", n * ((u == "") ? 1 / 1048576 : f[u]) + 0.5 }'
}

# cache_cpu_read <json file>: the cache container's CPU request and limit in the Job's manifest as
# the namespace admitted it, so a mutation that lowered the request is read here and not guessed.
cache_cpu_read() {
  jq -r '.spec.template.spec.containers[] | select(.name == "cache") | "\(.resources.requests.cpu // "none") \(.resources.limits.cpu // "none")"' "$1"
}

# cache_cpu_holds <request> <limit>: both are cache_cpu, whatever their spelling.
cache_cpu_holds() {
  [ "$(cpu_milli "$1")" = "$(cpu_milli "$cache_cpu")" ] && [ "$(cpu_milli "$2")" = "$(cpu_milli "$cache_cpu")" ]
}

echo "== repository-cache-burst-job"
echo "namespace:     $namespace"
echo "image:         $image"
echo "revision:      $image_revision"
echo "checkout:      $repo_root"
echo "tested:        $tested_revision"
if [ "$image_revision" != "${image_revision#unread}" ]; then
  revision_note="the image's revision label is unread, so the image and the checkout are not compared"
elif [ "$image_revision" = "$checkout_head" ]; then
  revision_note="the image's revision label is the checkout's commit"
else
  revision_note="the image's revision label $image_revision is not the checkout's commit $checkout_head: the double and the image's cache are two heads"
fi
echo "comparison:    $revision_note"
echo "repository:    $repo"
echo "double:        $(printf '%s ' "${double_files[@]#"$repo_root"/}")"
echo "go directive:  $go_directive, as packages/daemon/go.mod holds it"
echo "init scripts:  stage-double $(wc -c < "$work/stage-double.sh") bytes, stage-probe $(wc -c < "$work/stage-probe.sh") bytes (a container argument is at most 131072)"
echo "service acct:  ${service_account:-namespace default}, no token mounted"
echo "disk:          $disk of class $storage_class, deadline $deadline s"
total_milli=$(($(cpu_milli "$cache_cpu") + $(cpu_milli "$double_cpu") + $(cpu_milli "$fetch_cpu")))
total_mib=$(($(mem_mib "$cache_memory") + $(mem_mib "$double_memory") + $(mem_mib "$fetch_memory")))
echo "requests:      cache $cache_cpu CPU $cache_memory (the production default), double $double_cpu CPU $double_memory, fetchers $fetch_cpu CPU $fetch_memory, each request = limit"
echo "pod requests:  $((total_milli / 1000)).$(printf '%03d' $((total_milli % 1000))) CPU, $((total_mib / 1024)) GiB memory, $disk disk (the init containers' 250m each run before the others and add nothing)"
echo "selector:"
printf '%s' "$selector_yaml"

if [ "$dry_run" -eq 1 ]; then
  manifest=$work/job-dry-run.yaml
  render_manifest "$(run_id)" "$manifest"
  echo
  echo "== the manifest, rendered and not created ($(wc -l < "$manifest") lines, $(wc -c < "$manifest") bytes; the embedded files' lines left out below)"
  print_manifest "$manifest"
  echo
  # A server dry run runs the namespace's own admission and creates nothing, which is what says
  # whether this manifest would be admitted. Where this pod may not create a Job, the client dry
  # run is what is left: it validates the manifest against kubectl's own schema and asks nothing.
  if [ "$(kubectl -n "$namespace" auth can-i create jobs 2> /dev/null || true)" = yes ]; then
    echo "== kubectl -n $namespace create --dry-run=server -o json -f <the manifest above>"
    # One dry run, read back: the manifest as admission leaves it, mutations included.
    kubectl -n "$namespace" create --dry-run=server -o json -f "$manifest" > "$work/admitted.json" || {
      echo "repository-cache-burst-job: the namespace's admission refused the manifest" >&2
      exit 2
    }
    echo "job.batch/$(jq -r '.metadata.name // .metadata.generateName' "$work/admitted.json") created (server dry run)"
    read -r cache_request cache_limit <<< "$(cache_cpu_read "$work/admitted.json")"
    echo "== the cache container as admitted: CPU request $cache_request, limit $cache_limit"
    if ! cache_cpu_holds "$cache_request" "$cache_limit"; then
      echo "repository-cache-burst-job: admission left the cache container a CPU request of $cache_request and a limit of $cache_limit, not $cache_cpu and $cache_cpu: pass the deployment's pod-template label that keeps a CPU request (--label), and run it again" >&2
      exit 2
    fi
    echo "== the other containers as admitted (a lowered request there costs the measurement nothing, since only the cache's counter is read):"
    jq -r '.spec.template.spec.containers[] | "   \(.name): request \(.resources.requests.cpu) limit \(.resources.limits.cpu)"' "$work/admitted.json"
    echo "== the pod template's securityContext as admitted: $(jq -c '.spec.template.spec.securityContext' "$work/admitted.json")"
  else
    echo "== this pod may not create a Job here, so the manifest is validated client-side only"
    echo "== kubectl create --dry-run=client -f <the manifest above>"
    kubectl create --dry-run=client -f "$manifest" || {
      echo "repository-cache-burst-job: kubectl refused the manifest" >&2
      exit 2
    }
  fi
  exit 0
fi

# A real run on an image that is not the checkout's commit tests two heads at once.
case "$revision_note" in
  *"are two heads"*)
    echo "repository-cache-burst-job: $revision_note" >&2
    exit 2
    ;;
esac

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
  echo "repository-cache-burst-job: this pod may not create a Job in $namespace, so nothing is run here" >&2
  exit 2
fi

run_label=$(run_id)
manifest=$work/job.yaml
render_manifest "$run_label" "$manifest"
echo
echo "== creating the Job ($run_label_key=$run_label)"
kubectl -n "$namespace" create -o json -f "$manifest" > "$work/created.json" || {
  echo "repository-cache-burst-job: the create was refused; nothing of this run is left" >&2
  exit 2
}
workload=$(jq -r .metadata.name "$work/created.json")
echo "workload: $workload (uid $(jq -r .metadata.uid "$work/created.json"))"
read -r cache_request cache_limit <<< "$(cache_cpu_read "$work/created.json")"
echo "the cache container as admitted: CPU request $cache_request, limit $cache_limit"
if ! cache_cpu_holds "$cache_request" "$cache_limit"; then
  echo "repository-cache-burst-job: admission left the cache container a CPU request of $cache_request and a limit of $cache_limit, not $cache_cpu and $cache_cpu, so the run is deleted: pass the deployment's pod-template label that keeps a CPU request (--label)" >&2
  exit 2
fi

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

# pods_line: every attempt, oldest first, with its phase and its cache container's state.
pods_line() {
  kubectl -n "$namespace" get pods -l "job-name=$workload" --sort-by=.metadata.creationTimestamp \
    -o jsonpath='{range .items[*]}{.metadata.name}={.status.phase}/{.status.containerStatuses[?(@.name=="cache")].state.terminated.reason}{" "}{end}' 2> /dev/null || true
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

# The newest attempt is the one whose log holds the verdict. It is read once its cache container has
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
  terminated=$(kubectl -n "$namespace" get pod "$pod" -o jsonpath='{.status.containerStatuses[?(@.name=="cache")].state.terminated.reason}' 2> /dev/null || true)
  [ -z "$terminated" ] || break
  sleep 5
done
echo "  its cache container: ${terminated:-not terminated after $terminated_wait s}"

# The image each container actually ran, which is what ties everything below to the digest named
# above: the two init containers and the three regular ones.
digest=${image#*@}
ran=$(kubectl -n "$namespace" get pod "$pod" -o jsonpath='{range .status.initContainerStatuses[*]}{.name}={.imageID}{"\n"}{end}{range .status.containerStatuses[*]}{.name}={.imageID}{"\n"}{end}' 2> /dev/null || true)
echo "  imageIDs:"
while IFS= read -r line; do echo "    $line"; done <<< "${ran:-unreadable}"

for container in double fetchers cache; do
  echo
  echo "== the log of $pod, container $container"
  log=$work/$container.log
  if ! kubectl -n "$namespace" logs "pod/$pod" -c "$container" --tail=-1 > "$log" 2>&1; then
    echo "the $container container's log could not be read"
    cat "$log" || true
    [ "$container" != cache ] || exit 2
    continue
  fi
  # The double's and the fetchers' own lines are progress and the double's build; the cache's is the
  # whole record, read in full below. A long one of the first two is cut to its ends.
  if [ "$container" = cache ]; then
    cat "$log"
  elif [ "$(wc -l < "$log")" -gt 120 ]; then
    head -n 50 "$log"
    echo "… $(($(wc -l < "$log") - 100)) lines left out …"
    tail -n 50 "$log"
  else
    cat "$log"
  fi
done

while IFS= read -r line; do
  [ -n "$line" ] || continue
  case "$line" in
    *"$digest"*) ;;
    *)
      echo
      echo "a container of $pod ran imageID [$line], which does not name the digest $digest"
      exit 2
      ;;
  esac
done <<< "$ran"
[ -n "$ran" ] || {
  echo
  echo "the image ids of $pod could not be read, so what it ran is unknown"
  exit 2
}

echo
echo "======== the verdict"
echo "image: $image"
echo "tested revision: $tested_revision"
echo "attempts: $(pods_line)"
result=$(grep -x 'JOB-RESULT \(pass\|fail\|void\)' "$work/cache.log" | tail -n 1 || true)
case "$result" in
  "JOB-RESULT pass")
    echo "pass: every check held"
    exit 0
    ;;
  "JOB-RESULT fail")
    echo "fail: the log's CHECK lines say which did not hold"
    exit 1
    ;;
  "JOB-RESULT void")
    echo "void: the run proved nothing; the log says why (the attribution check, the control arm or the double)"
    exit 2
    ;;
esac
echo "the cache container's log carries no JOB-RESULT line, so it decided nothing"
case " $conditions " in
  *" Failed:DeadlineExceeded "*) exit 3 ;;
esac
exit 2
