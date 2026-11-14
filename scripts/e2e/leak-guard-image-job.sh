#!/usr/bin/env bash
# The live proof that the published worker image's own `git` and `jj` refuse a push that names
# something private, and that nothing else is changed: one Kubernetes Job on the image digest the
# caller names, driving the image's wrappers, its `legion leak-check`, the gitleaks engine linked
# into that binary and the rule file embedded in it
# (`packages/daemon/internal/leakguard/private-names.toml`). It is check k1 of
# dispatch://INFRA-1527 and, for the two shared-check failures at k1's own call sites, part of k7;
# the tester runs it before the merge and the production check runs it again after the rollout, so
# this recipe is the one all three read. It is two files: this harness, which renders the Job and
# reads its verdict, and `leak-guard-image-probe.sh` beside it, the text the Job runs inside the
# image. A Job holds no checkout, so the harness writes that text into the Job's command itself;
# neither file is run or changed without the other.
#
# The Job holds no Kubernetes credential and runs no launcher, so it is not given the private
# shared public leak check, which the daemon reads and pushes to a pod's launcher. It writes a
# stand-in of its own into its `emptyDir` instead and names it in `PUBLIC_LEAK_CHECK`. That
# stand-in clears every call, so the rule file alone decides each verdict. Every one of k1's five
# cases turns on the Tailscale MagicDNS class, which only the rule file holds, which is why a
# stand-in is enough here. The real shared check is driven after the rollout, in a real pod, whose
# launcher the daemon pushes it to.
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

# The probe this Job runs, in a file of its own beside this script. The Job holds no checkout, so
# the probe reaches the container as the text of the command itself and no other way: it is read
# from here and written into the manifest below (render_manifest). The two files are one recipe.
probe=$(dirname -- "${BASH_SOURCE[0]}")/leak-guard-image-probe.sh
[ -r "$probe" ] || refuse "the probe is not beside this script at $probe; it is the other half of this recipe"

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
#
# Its text is the file beside this script, which render_manifest below writes into the Job's
# command, since a Job holds no checkout to read it from. It is checked here, before anything is
# created, so a probe that is not valid bash is a refusal of this harness rather than a Job that
# starts and reaches no verdict.
# ---------------------------------------------------------------------------------------------
bash -n "$probe" || refuse "the probe beside this script, $probe, is not valid bash"

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
