#!/usr/bin/env bash
# The Job of check c-adopt-large-checkout (dispatch://INFRA-3916, R4): the adoption of a fresh
# working copy ends within 10 s on a checkout of 342,000 files. One Kubernetes Job, under gVisor as
# an issue pod runs, clones cybermonklabs/legion at the commit the caller names (the repository is
# public, so the Job holds no GitHub token) and, from that clone alone:
#
#   - downloads the x86_64 musl archive of the jj release that the one `ARG JJ_TOOL` line of its
#     packages/daemon/docker/worker.Dockerfile names, from the release's GitHub assets as the
#     image's tools stage does, and checks it against that file's `ARG JJ_SHA256`;
#   - downloads the Go release its packages/daemon/go.mod names (its `go` line) from go.dev and
#     checks it against the Dockerfile's `ARG GO_SHA256`, whose `ARG GO_VERSION` must name the same
#     release;
#   - writes a bare git repository of 342,000 tracked files on main, in nested directories of at
#     most 1,000 entries, over 20 commits, each tagged, so the repository has a history and refs to
#     load (make_repository below);
#   - runs `go test -count=1 -v -timeout 3600s -run '^TestAdoptionOnALargeCheckout$' ./internal/shim/`
#     from packages/daemon, with LEGION_TEST_LARGE_CHECKOUT naming that repository and the
#     downloaded jj first on PATH.
#
# That test (packages/daemon/internal/shim/adoption_large_test.go) provisions an issue pod's
# workspace from the repository with workspace.Provision, has the real shim adopt it once right
# after the checkout, three times warm under a new identity each time and once on the `jj new`
# branch, then runs main's pair of adoption commands, which snapshot, as the baseline, and logs
# each time on a line beginning `adopt-large-checkout time:`. The commands timed are the code's
# own, so nothing here copies them and no line of the checkout is guarded.
#
# The Job's verdict is the last line of its log: `adopt-large-checkout: pass` where go test passed
# TestAdoptionOnALargeCheckout, the test logged its five adoption times and each is within 10 s,
# and its repository held the 342,000 files; `adopt-large-checkout: fail: <reason>` otherwise, a
# step of the Job's own that failed included, which the reason names. The line before it lists the
# times. The Job's command exits 0 once it has written its verdict, so the Job never starts a whole
# attempt again to reach the same one; only a pod lost with its machine is followed by another.
#
# This harness renders the Job, dry-runs it on the server, creates it, follows the newest pod's
# log as the deployment's runbook for a role's heavy work says (the container's state read, its
# log followed, and read again whole once the container has terminated, the follow of a running
# container being able to end early), judges that log's last line, and deletes the Job. The
# implementer runs it at the head it hands on, and the tester again at the head it tests.
#
# The manifest is the deployment's own Job template for I/O-heavy work on a disk at /scratch, with
# every field it sets kept:
# backoffLimit 6 and podReplacementPolicy Failed, ttlSecondsAfterFinished 3600,
# activeDeadlineSeconds on the Job and on its pod template, restartPolicy Never,
# automountServiceAccountToken false, fsGroup 1000, runtimeClassName gvisor,
# terminationGracePeriodSeconds 60, the node selector, the ephemeral claim at /scratch, `bash -c`
# under `set -euo pipefail` with the template's stack-limit lines, and the container's CPU, memory
# and ephemeral-storage. Its bucket checkpoint is left out: the work is idempotent and short, a
# lost attempt starts again from nothing, and the result is the log. Added to it: a non-root
# security context (uid and gid 1000, no privilege escalation, every capability dropped, the
# RuntimeDefault seccomp profile), as pack-memory-job.sh's Job has it.
#
# Sizing. CPU 4, request equal to limit: the CPU request of the deployment's template for a build
# or a test suite, the least docs/kubernetes.md, "Running a workload in the cluster from a role",
# admits, and equal to its limit because this is a timed run: a lower request lets the timed
# commands share the machine. The namespace lowers a CPU request equal to its limit
# unless the pod template carries its guaranteed-CPU label, so give that label with --label; this
# harness reads the CPU back from the server dry run and from the created Job, and refuses (and
# deletes the Job) where the request came back lower than the limit. Memory 16Gi, request equal to
# limit: twice an issue pod's whole 8 GiB, in which a role runs this adoption, a provision and a
# compile of this package alike. The disk, 50Gi of the storage class --storage-class names, holds
# everything the Job writes (HOME, TMPDIR, Go and its caches, the clone of this repository, the
# generated repository, the provisioned clone and the workspace, whose 342,000 files take a block
# each); that is estimated at under 10 GiB from those parts, and the size is five times that, a
# persistent disk's speed growing with its size. The container's own ephemeral-storage, 10Gi, is
# the template's: nothing but its logs is written outside /scratch.
#
# Inputs. Every value private to a deployment is an input and never a literal here: this
# repository is public, and its deployment's namespace, compute class, storage class, image
# mirror and the labels and annotations its admission and its watchers read are in that
# deployment's own declaration.
#
#   --commit <40 hex>               required: the commit of cybermonklabs/legion the Job clones and
#                                   tests, pushed: the harness fetches it from GitHub first.
#   --image <ref>@sha256:<64 hex>   required: the image the Job runs, by digest, never by tag. It
#                                   carries bash, coreutils, awk, curl, git 2.42 or later (jj's git
#                                   backend needs it) and a GNU tar built on a glibc before 2.43
#                                   (gVisor has no openat2), and lets uid 1000 run them (the Job's
#                                   test builds with CGO_ENABLED=0, as the worker image's Go does);
#                                   a mirror of google-cloud-cli on Debian 13 carries them all.
#   --namespace <namespace>         required: the namespace the Job runs in.
#   --node-selector <key=value>[,…] required: the compute class the Job's pod selects, a class
#                                   that admits `runtimeClassName: gvisor`, which this Job always
#                                   sets, as issue pods run.
#   --storage-class <name>          required: the storage class of the Job's disk, an ephemeral
#                                   volume claim mounted at /scratch.
#   --label <key=value>             repeatable: a label on the Job's metadata and on its pod
#                                   template's: the job kind, adopt-large-checkout; the submitting
#                                   session, "${OMP_SESSION_ID:-${DISPATCH_SESSION_ID:-$PI_SESSION_ID}}",
#                                   36 characters; the guaranteed-CPU label; and what the namespace's
#                                   admission requires.
#   --annotation <key=value>        repeatable: an annotation on both, the same way, such as the
#                                   session's title, INFRA-3916 adopt-large-checkout.
#   --cpu <quantity>                the container's CPU, request and limit (default 4).
#   --memory <quantity>             its memory, request and limit (default 16Gi).
#   --disk <quantity>               the disk volume's size (default 50Gi).
#   --deadline <seconds>            activeDeadlineSeconds on the Job and on its pod template
#                                   (default 7200, at least 4500: go test's own 3600 s and the
#                                   setup before it; at most 43200).
#   --prefix <generateName prefix>  the Job's metadata.generateName, ending in a hyphen (default
#                                   infra-3916-adopt-large-checkout-).
#   --out <directory>               where the rendered manifest (job.yaml) and the newest pod's
#                                   whole log (<pod>.log) are kept, a directory outside this
#                                   checkout; by default they go in a temporary one this removes.
#   --dry-run                       render the manifest, dry-run it on the server, create nothing,
#                                   and exit.
#   --make-repository <directory>   write the generated bare repository at <directory> on this
#                                   machine, as the Job writes it, and exit; only --files and
#                                   --commits go with it, for a local run of the test:
#   --files <n>, --commits <n>      the repository's files and commits (default 342000 and 20).
#
# Fixed here: gVisor; both deadlines; ttlSecondsAfterFinished 3600; backoffLimit 6 with
# podReplacementPolicy Failed, so a pod lost with its machine is followed by a whole new attempt
# and never by a second one beside it; no service account token; the security context above;
# requests equal to limits; the repository, cybermonklabs/legion; and the generated repository's
# 342,000 files over 20 commits, the checkout the check names.
#
# Exit statuses:
#
#   0  the Job's last line is `adopt-large-checkout: pass`.
#   1  the Job's last line is `adopt-large-checkout: fail: <reason>`: the reason, printed, says
#      whether the test failed (an adoption past 10 s among them) or a step of the Job's own did.
#   2  the harness could not run: a usage error, a missing input, a commit GitHub does not hold, a
#      namespace that refuses the Job or lowers its CPU request, a Job that started no pod, a log
#      that could not be read, an image the pod did not run, or a log whose last line is no
#      verdict. Nothing is proved either way.
#   3  the Job reached no verdict inside its deadline. Nothing is proved either way.
#
# Read the verdict from the log's last line and never from the Job's phase: the Job's command exits
# 0 on a failed verdict as well. A log the kubelet no longer serves (its node gone) is read from
# Cloud Logging by the Job's name, which this harness prints, as the deployment's runbook says.
#
# This script is synchronous, runs for as long as the Job does (up to its deadline), and its clean-up
# is a trap, so a role that runs it from its own bash tool runs it detached, the way
# docs/kubernetes.md, "Running a workload in the cluster from a role", sets out: the tool kills a
# call that runs past its timeout faster than any trap runs. Run it with `--dry-run` first:
#
#   sid=${OMP_SESSION_ID:-${DISPATCH_SESSION_ID:-$PI_SESSION_ID}}; [ ${#sid} -eq 36 ]
#   log=$(mktemp "${TMPDIR:-/tmp}/adopt-large-checkout.XXXXXX")
#   flock "$log" setsid -f bash scripts/e2e/adopt-large-checkout-job.sh \
#     --commit <40 hex> --image <digest> --namespace <namespace> --node-selector <key=value> \
#     --storage-class <class> --label <key=value> … --annotation <key=value> … --out <directory> \
#     > "$log" 2>&1 < /dev/null
#   flock "$log" cat "$log"     # waits for the run to end, then prints everything it printed
set -euo pipefail

job_ttl=3600             # after it finishes; the cluster admits at most 86400
pod_wait=1800            # for the pod to exist: the queue may hold the Job, and the class makes a machine
terminated_wait=300      # for a pod's container to have terminated once its follow ended
poll=10                  # how often the harness reads the Job and its pods
ephemeral_storage=10Gi   # the container's own disk; everything the Job writes goes on the volume
go_test_timeout=3600     # go test's -timeout, in seconds, inside the Job's deadline
repository=cybermonklabs/legion

run_label_key=adopt-large-checkout-run

commit=""
image=""
namespace=""
node_selector=""
storage_class=""
labels=()
annotations=()
cpu=4
memory=16Gi
disk=50Gi
deadline=7200
prefix=infra-3916-adopt-large-checkout-
out=""
dry_run=0
make_dir=""
files=342000
commits=20
counts_given=0

usage() {
  cat << 'USAGE'
usage: scripts/e2e/adopt-large-checkout-job.sh --commit <40 hex> --image <ref>@sha256:<64 hex>
                                              --namespace <namespace>
                                              --node-selector <key=value>[,<key=value>…]
                                              --storage-class <name>
                                              [--label <key=value>]… [--annotation <key=value>]…
                                              [--cpu <quantity>] [--memory <quantity>] [--disk <quantity>]
                                              [--deadline <seconds>] [--prefix <name->] [--out <dir>]
                                              [--dry-run]
       scripts/e2e/adopt-large-checkout-job.sh --make-repository <dir> [--files <n>] [--commits <n>]

  --commit           the pushed commit of cybermonklabs/legion the Job clones and tests.
  --image            the image the Job runs, by digest (bash, coreutils, awk, curl, git 2.42 or
                     later, a GNU tar gVisor runs; uid 1000).
  --namespace        the namespace the Job runs in.
  --node-selector    the compute class the pod selects; it must admit runtimeClassName: gvisor.
  --storage-class    the storage class of the disk volume mounted at /scratch.
  --label            a label on the Job and on its pod template; repeatable.
  --annotation       an annotation on the Job and on its pod template; repeatable.
  --cpu              CPU, request and limit (default 4).
  --memory           memory, request and limit (default 16Gi).
  --disk             the disk volume's size (default 50Gi).
  --deadline         activeDeadlineSeconds of the Job and its pod, 4500 to 43200 (default 7200).
  --prefix           the Job's metadata.generateName, ending in a hyphen.
  --out              a directory outside this checkout that keeps the manifest and the pod's log.
  --dry-run          render the manifest, dry-run it on the server, create nothing, and exit.
  --make-repository  write the generated bare repository at <dir> here and exit, for a local run
                     of TestAdoptionOnALargeCheckout; --files and --commits (default 342000 and 20)
                     go with it alone.

The values of --image, --namespace, --node-selector, --storage-class and of the labels and
annotations are the deployment's own; this public repository carries none of them.

exit statuses: 0 the Job's verdict is pass; 1 it is fail, with its reason; 2 the harness could not
run and nothing is proved; 3 the Job reached no verdict inside its deadline.
USAGE
}

refuse() {
  echo "adopt-large-checkout-job: $*" >&2
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
    --commit) commit=$value ;;
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
    --out) out=$value ;;
    --make-repository) make_dir=$value ;;
    --files) files=$value; counts_given=1 ;;
    --commits) commits=$value; counts_given=1 ;;
    *) refuse "unknown option $option" ;;
  esac
done

# ---------------------------------------------------------------------------------------------
# The Job's program. Each function below runs inside the Job, which holds no checkout: the
# manifest carries them as `declare -f` prints them, so this file is the one source of each, and
# make_repository also runs here for --make-repository. None of them may use a heredoc.
# ---------------------------------------------------------------------------------------------

# make_repository <directory> <files> <commits>: a new bare git repository at <directory> whose
# main holds <files> tracked files over <commits> commits, written by git fast-import with no
# working tree. File n is d<n/50000>/d<n/500 % 100>/f<n % 500>.txt, so no directory holds more
# than 1,000 entries (at most 7, 100 and 500 at the three levels for 342,000 files); each commit
# adds its share of the files and, from the second on, rewrites 100 files of the earlier ones, and
# each is tagged commit-<n>. It checks what it wrote: the file count, the commit count and the
# largest directory.
make_repository() {
  local dir=$1 files=$2 commits=$3 got largest
  git init --quiet --bare --initial-branch=main "$dir"
  awk -v files="$files" -v commits="$commits" '
    function path(n) { return sprintf("d%d/d%02d/f%03d.txt", int(n / 50000), int(n / 500) % 100, n % 500) }
    function blob(p, body) { printf "M 100644 inline %s\ndata %d\n%s", p, length(body), body }
    BEGIN {
      share = int((files + commits - 1) / commits); n = 0
      for (c = 1; c <= commits; c++) {
        message = sprintf("commit %d of %d\n", c, commits)
        printf "commit refs/heads/main\nmark :%d\n", c
        printf "committer Large Checkout <large-checkout@example.test> %d +0000\n", 1767225600 + 3600 * c
        printf "data %d\n%s", length(message), message
        if (c > 1) printf "from :%d\n", c - 1
        if (c > 1) for (k = 0; k < 100; k++) { m = int(k * n / 100); blob(path(m), sprintf("file %d, changed in commit %d\n", m, c)) }
        for (k = 0; k < share && n < files; k++) { blob(path(n), sprintf("file %d, added in commit %d\n", n, c)); n++ }
        printf "\nreset refs/tags/commit-%d\nfrom :%d\n\n", c, c
      }
    }' | git -C "$dir" fast-import --quiet
  got=$(git -C "$dir" ls-tree -r --name-only main | wc -l)
  [ "$got" -eq "$files" ] || { echo "make_repository: main holds $got files, want $files" >&2; return 1; }
  got=$(git -C "$dir" rev-list --count main)
  [ "$got" -eq "$commits" ] || { echo "make_repository: main has $got commits, want $commits" >&2; return 1; }
  largest=$(git -C "$dir" ls-tree -r -t --name-only main | awk '{ parent = $0; sub("/?[^/]*$", "", parent); count[parent]++ } END { for (p in count) if (count[p] > max) max = count[p]; print max + 0 }')
  [ "$largest" -le 1000 ] || { echo "make_repository: a directory holds $largest entries, want at most 1000" >&2; return 1; }
  echo "make_repository: $dir: $files files on main over $commits commits, at most $largest entries in a directory"
}

# adopt_step <what>: the step the Job's work is on, logged, and kept in /scratch/adopt/step for
# the verdict of a step that fails.
adopt_step() {
  printf '%s\n' "$*" > /scratch/adopt/step
  echo "== [$(date -u +%FT%TZ)] $*"
}

# adopt_fetch <url> <sha256> <file>: the file at url, which must have that sha256.
adopt_fetch() {
  local url=$1 want=$2 file=$3 got
  printf '%s\n' "$want" | grep -Eqx '[0-9a-f]{64}' || { echo "the pinned sha256 [$want] of $url is no 64-hex digest" >&2; return 1; }
  curl -fsSL --retry 3 --max-time 600 -o "$file" "$url"
  got=$(sha256sum "$file")
  got=${got%% *}
  [ "$got" = "$want" ] || { echo "$url has sha256 $got, and the pin is $want" >&2; return 1; }
  echo "$url: sha256 $got, as pinned"
}

# adopt_pin <file> <arg>: the value of the one `ARG <arg>=<value>` line of file.
adopt_pin() {
  local lines
  lines=$(grep -E "^ARG $2=" "$1" || true)
  [ -n "$lines" ] && [ "$(printf '%s\n' "$lines" | wc -l)" -eq 1 ] || { echo "$1 holds no one line ARG $2=…" >&2; return 1; }
  printf '%s\n' "${lines#*=}"
}

# adopt_work <commit> <files> <commits> <go test timeout>: the Job's work, up to go test's status
# and output in /scratch/adopt, which adopt_judge reads. A step that fails ends it, its step named
# in /scratch/adopt/step.
adopt_work() {
  set -euo pipefail
  local commit=$1 files=$2 commits=$3 timeout=$4
  local legion=/scratch/legion tools=/scratch/tools
  local dockerfile jj_tool jj_sha jj_rest jj_repo jj_version jj_tag go_mod go_version go_sha status

  adopt_step "the stack limit"
  # The template's stack lines: the soft stack limit 131072, read back.
  want=131072
  ulimit -S -s $want 2> /dev/null || { ulimit -H -s $want 2> /dev/null || true; ulimit -S -s $want; }
  { [ "$(ulimit -Hs)" = unlimited ] && ulimit -H -s $want 2> /dev/null; } || true
  soft=$(ulimit -Ss); hard=$(ulimit -Hs)
  [ "$soft" = "$want" ] || { echo "stack soft is $soft, want $want; refusing"; return 2; }
  [ "$hard" != unlimited ] && [ "$hard" -ge "$want" ] || { echo "stack hard is $hard; refusing"; return 2; }

  adopt_step "the machine"
  export HOME=/scratch/home TMPDIR=/scratch/tmp GOPATH=/scratch/go GOCACHE=/scratch/go-build
  export GOMODCACHE=/scratch/go/pkg/mod GOTOOLCHAIN=local GOFLAGS=-modcacherw GIT_TERMINAL_PROMPT=0
  # As the worker image's Go runs (its /opt/go/go.env), and no header of a C library is needed.
  export CGO_ENABLED=0
  mkdir -p "$HOME" "$TMPDIR" "$GOPATH" "$GOCACHE" "$tools/jj"
  echo "kernel: $(uname -srm); cpus: $(nproc); cpu.max: $(cat /sys/fs/cgroup/cpu.max 2> /dev/null || echo unread)"
  echo "uid: $(id -u); $(git --version); $(bash --version | head -n 1)"

  adopt_step "the clone of $repository at $commit"
  git init --quiet "$legion"
  git -C "$legion" fetch --quiet --depth 1 --no-tags "https://github.com/$repository" "$commit"
  git -C "$legion" -c advice.detachedHead=false checkout --quiet --detach FETCH_HEAD
  [ "$(git -C "$legion" rev-parse HEAD)" = "$commit" ] || { echo "the clone's HEAD is $(git -C "$legion" rev-parse HEAD), not $commit" >&2; return 1; }

  adopt_step "the jj release ARG JJ_TOOL names"
  dockerfile=$legion/packages/daemon/docker/worker.Dockerfile
  jj_tool=$(adopt_pin "$dockerfile" JJ_TOOL)
  jj_sha=$(adopt_pin "$dockerfile" JJ_SHA256)
  # The pin's shape as the tools stage reads it: github:<owner>/<repo>@<version>.
  jj_rest=${jj_tool#github:}; jj_repo=${jj_rest%@*}; jj_version=${jj_rest##*@}; jj_tag=v$jj_version
  [[ $jj_tool == github:*/*@* && $jj_repo =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ && $jj_version =~ ^[A-Za-z0-9._-]+$ ]] || { echo "ARG JJ_TOOL is [$jj_tool], which is no github:<owner>/<repo>@<version> pin" >&2; return 1; }
  adopt_fetch "https://github.com/$jj_repo/releases/download/$jj_tag/jj-$jj_tag-x86_64-unknown-linux-musl.tar.gz" "$jj_sha" "$tools/jj.tar.gz"
  tar -xzf "$tools/jj.tar.gz" -C "$tools/jj" --no-same-owner ./jj
  [[ $("$tools/jj/jj" --version) == "jj $jj_version"* ]] || { echo "the downloaded jj says [$("$tools/jj/jj" --version)], not jj $jj_version" >&2; return 1; }
  "$tools/jj/jj" --version

  adopt_step "the Go release packages/daemon/go.mod names"
  go_mod=$(awk '$1 == "go" { print $2; exit }' "$legion/packages/daemon/go.mod")
  go_version=$(adopt_pin "$dockerfile" GO_VERSION)
  go_sha=$(adopt_pin "$dockerfile" GO_SHA256)
  [ -n "$go_mod" ] && [ "$go_mod" = "$go_version" ] || { echo "packages/daemon/go.mod names go [$go_mod], and the Dockerfile's ARG GO_VERSION, whose ARG GO_SHA256 checks the download, names [$go_version]" >&2; return 1; }
  adopt_fetch "https://go.dev/dl/go$go_version.linux-amd64.tar.gz" "$go_sha" "$tools/go.tar.gz"
  tar -xzf "$tools/go.tar.gz" -C "$tools" --no-same-owner
  rm -f "$tools/go.tar.gz" "$tools/jj.tar.gz"
  export PATH=$tools/jj:$tools/go/bin:$PATH
  [ "$(go env GOVERSION)" = "go$go_version" ] || { echo "go env GOVERSION is [$(go env GOVERSION)], not go$go_version" >&2; return 1; }
  echo "$(go version); jj on PATH: $(command -v jj)"

  adopt_step "the repository of $files files over $commits commits"
  SECONDS=0
  make_repository /scratch/large.git "$files" "$commits"
  echo "written in $SECONDS s; $(du -sh /scratch/large.git | cut -f1) on disk; $(df -h /scratch | awk 'NR == 2 { print $3 " of " $2 " of /scratch used" }')"

  adopt_step "go test of TestAdoptionOnALargeCheckout"
  cd "$legion/packages/daemon"
  set +e
  LEGION_TEST_LARGE_CHECKOUT=/scratch/large.git go test -count=1 -v -timeout "${timeout}s" -run '^TestAdoptionOnALargeCheckout$' ./internal/shim/ 2>&1 | tee /scratch/adopt/test.log
  status=${PIPESTATUS[0]}
  set -e
  echo "$status" > /scratch/adopt/test.status
  echo "go test exited $status"
}

# adopt_judge <files>: the verdict of the test's output and status in /scratch/adopt, as the Job's
# last two lines: the times, then `adopt-large-checkout: pass` where go test passed
# TestAdoptionOnALargeCheckout, its five adoption times are there, each within 10 s, and its
# repository held <files> files, else `adopt-large-checkout: fail: ` and the first reason in that
# order.
adopt_judge() {
  awk -v want_files="$1" -v status="$(cat /scratch/adopt/test.status)" '
    /--- SKIP: TestAdoptionOnALargeCheckout / { skipped = 1 }
    /--- PASS: TestAdoptionOnALargeCheckout / { passed = 1 }
    /--- FAIL: TestAdoptionOnALargeCheckout / { failed = 1 }
    /adopt-large-checkout: .* holds [0-9]+ tracked files/ { for (i = 1; i < NF; i++) if ($i == "holds") held = $(i + 1) }
    /adopt-large-checkout time: [a-z0-9-]+ [0-9.]+ s$/ {
      what = $(NF - 2); took = $(NF - 1)
      times = times (times == "" ? "" : ", ") what " " took " s"
      if (what ~ /^adoption-/) {
        adoptions++
        if (took + 0 > 10 && over == "") over = what " took " took " s, past 10 s"
      }
    }
    END {
      print "adopt-large-checkout: times: " (times == "" ? "none" : times)
      if (skipped) reason = "TestAdoptionOnALargeCheckout skipped"
      else if (!passed && !failed) reason = "go test exited " status " and ran no TestAdoptionOnALargeCheckout"
      else if (over != "") reason = over
      else if (failed || status != 0) reason = "TestAdoptionOnALargeCheckout failed, go test exited " status
      else if (adoptions != 5) reason = "the test logged " adoptions + 0 " adoption times, want 5"
      else if (held != want_files) reason = "the repository held " (held == "" ? "no count of" : held) " files, want " want_files
      print "adopt-large-checkout: " (reason == "" ? "pass" : "fail: " reason)
    }' /scratch/adopt/test.log
}

# adopt_main <commit> <files> <commits> <go test timeout>: the Job's command. It runs the work in a
# subshell of its own, so that a step that fails ends the work and not this, and writes the
# verdict as the log's last line, `adopt-large-checkout: pass` or `adopt-large-checkout: fail:
# <reason>`. It exits 0 either way.
adopt_main() {
  local status
  mkdir -p /scratch/adopt
  echo "starting" > /scratch/adopt/step
  set +e
  (adopt_work "$@")
  status=$?
  set -e
  if [ "$status" -ne 0 ]; then
    echo "adopt-large-checkout: fail: $(cat /scratch/adopt/step) failed, exit $status"
    exit 0
  fi
  adopt_judge "$2" || echo "adopt-large-checkout: fail: the test's log in /scratch/adopt could not be judged"
  exit 0
}

# ---------------------------------------------------------------------------------------------
# --make-repository: the generated repository on this machine, for a local run of the test.
# ---------------------------------------------------------------------------------------------
[[ $files =~ ^[1-9][0-9]*$ ]] || refuse "--files $files is no whole number of files"
[[ $commits =~ ^[1-9][0-9]*$ ]] || refuse "--commits $commits is no whole number of commits"
if [ -n "$make_dir" ]; then
  [ "$commits" -le "$files" ] || refuse "--commits $commits is more than --files $files: each commit adds files"
  [ ! -e "$make_dir" ] || refuse "--make-repository $make_dir already exists; it names a directory this writes"
  command -v git > /dev/null 2>&1 || refuse "no git on PATH"
  make_repository "$make_dir" "$files" "$commits"
  exit 0
fi
[ "$counts_given" -eq 0 ] || refuse "--files and --commits go with --make-repository alone: the Job writes the 342,000 files over 20 commits the check names"

# ---------------------------------------------------------------------------------------------
# The inputs.
# ---------------------------------------------------------------------------------------------

# yaml_quote <text>: the text as one YAML double-quoted scalar. A control character could change
# what the manifest says, so it is refused rather than escaped.
yaml_quote() {
  local text=$1
  [[ ! $text =~ [[:cntrl:]] ]] || return 1
  text=${text//\\/\\\\}
  text=${text//\"/\\\"}
  printf '"%s"' "$text"
}

[ -n "$commit" ] || refuse "no --commit: name the pushed commit of $repository the Job tests"
[[ $commit =~ ^[0-9a-f]{40}$ ]] || refuse "--commit $commit is no 40-hex commit id"
# A digest and nothing else: a tag is mutable (docs/kubernetes.md, "Pin by digest").
digest_shape='^[A-Za-z0-9][A-Za-z0-9._:/-]*@sha256:[0-9a-f]{64}$'
[ -n "$image" ] || refuse "no --image: name the image the Job runs, by digest"
[[ $image =~ $digest_shape ]] || refuse "the image $image is not a digest reference <ref>@sha256:<64 hex>"
[ -n "$namespace" ] || refuse "no --namespace: name the namespace the Job runs in"
[[ $namespace =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || refuse "the namespace $namespace is no Kubernetes name"
[ -n "$node_selector" ] || refuse "no --node-selector: the namespace's admission requires the deployment's compute class"
[ -n "$storage_class" ] || refuse "no --storage-class: the Job's work goes on a disk volume of the deployment's storage class"
[[ $storage_class =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]] || refuse "the storage class $storage_class is no Kubernetes name"
[[ $cpu =~ ^([0-9]+(\.[0-9]+)?|[0-9]+m)$ ]] || refuse "--cpu $cpu is no CPU quantity"
quantity='^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|k|M|G|T|P)?$'
[[ $memory =~ $quantity ]] || refuse "--memory $memory is no quantity"
[[ $disk =~ $quantity ]] || refuse "--disk $disk is no quantity"
[[ $deadline =~ ^[1-9][0-9]*$ ]] || refuse "--deadline $deadline is no whole number of seconds"
[ "$deadline" -le 43200 ] || refuse "--deadline $deadline is over 43200, the longest the cluster admits"
[ "$deadline" -ge $((go_test_timeout + 900)) ] || refuse "--deadline $deadline is under $((go_test_timeout + 900)): go test alone has $go_test_timeout s, and the setup before it needs more"
case "$prefix" in
  *-) ;;
  *) refuse "the prefix $prefix does not end in a hyphen; the API server appends five characters to it" ;;
esac

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$here/../.." && pwd)
if [ -n "$out" ]; then
  mkdir -p -- "$out" || refuse "--out $out cannot be made"
  out=$(cd -- "$out" && pwd)
  case "$out/" in
    "$repo_root"/*) refuse "--out $out is inside this checkout, a jj working copy of a public repository, which would take the log into a commit" ;;
  esac
fi

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
command -v git > /dev/null 2>&1 || refuse "no git on PATH: the commit is fetched from GitHub before anything is created"

# The Job's program, each function as bash prints it, checked before anything is created, so a
# program that is no valid bash is a refusal here and not a Job that starts and reaches no verdict.
program=$(declare -f make_repository adopt_step adopt_fetch adopt_pin adopt_work adopt_judge adopt_main)
bash -n <<< "$program" || refuse "the Job's program, as declare -f prints it, is not valid bash"

work=$(mktemp -d "${TMPDIR:-/tmp}/adopt-large-checkout-job.XXXXXX")
# Outside the checkout on purpose: a workspace is a jj working copy of this public repository, so
# a manifest written there would be snapshotted into the role's own commit.
keep=${out:-$work}

workload=""
run_label=""

# shellcheck disable=SC2317,SC2329 # the EXIT trap below is what runs it
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
    # with `kubectl -n <namespace> delete job -l adopt-large-checkout-run=<the printed value>
    # --cascade=foreground`.
    setsid -w kubectl -n "$namespace" get jobs -l "$run_label_key=$run_label"
  fi
  rm -rf "$work"
  [ -z "$out" ] || echo "kept in $out: $(cd "$out" && ls | tr '\n' ' ')"
  echo "adopt-large-checkout-job: exit $status"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

# The commit, fetched from GitHub with no tree and no blob, so a commit GitHub does not hold is a
# refusal here and not a Job that clones nothing.
git init --quiet --bare "$work/commit.git"
GIT_TERMINAL_PROMPT=0 git -C "$work/commit.git" fetch --quiet --depth 1 --filter=tree:0 "https://github.com/$repository" "$commit" ||
  refuse "GitHub's $repository holds no commit $commit that this pod can fetch: push it first"
subject=$(git -C "$work/commit.git" log -1 --format=%s "$commit")

# ---------------------------------------------------------------------------------------------
# The manifest. The fields are the template's (the header says which); the comments say why.
# ---------------------------------------------------------------------------------------------
render_manifest() { # render_manifest <run label> <file>
  {
    cat << YAML
apiVersion: batch/v1
kind: Job
metadata:
  # The API server names the run: five characters it appends at the create. The label
  # ${run_label_key} is this run's own, on the Job alone: what the clean-up finds a Job by whose
  # name it never read.
  generateName: ${prefix}
$(map_yaml "  " labels "$run_label_key=$1" "${labels[@]}")
$(map_yaml "  " annotations "${annotations[@]}")
spec:
  backoffLimit: 6                          # a pod lost with its machine starts the run again elsewhere
  podReplacementPolicy: Failed             # and only once the lost pod has ended
  ttlSecondsAfterFinished: ${job_ttl}      # the log is read as the Job ends; Cloud Logging keeps it
  activeDeadlineSeconds: ${deadline}       # at most 43200, here and on the pod template
  template:
    metadata:
$(map_yaml "      " labels "${labels[@]}")
$(map_yaml "      " annotations "${annotations[@]}")
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false    # it carries no identity, and needs none
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        runAsGroup: 1000
        fsGroup: 1000                        # a fresh volume's root is root's; this makes it writable
      runtimeClassName: gvisor               # as issue pods run
      activeDeadlineSeconds: ${deadline}     # keep equal to the one above
      terminationGracePeriodSeconds: 60
      nodeSelector:
${selector_yaml}      volumes:
        - name: scratch
          ephemeral:                         # the disk is made with the pod and deleted with it
            volumeClaimTemplate:
              spec:
                accessModes: ["ReadWriteOnce"]
                storageClassName: $(yaml_quote "$storage_class")
                resources:
                  requests:
                    storage: "${disk}"
      containers:
        - name: adopt-large-checkout
          image: ${image}
          imagePullPolicy: IfNotPresent      # a digest names one image; a re-pull fetches the same bytes
          terminationMessagePolicy: FallbackToLogsOnError
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
            seccompProfile:
              type: RuntimeDefault
          volumeMounts:
            - name: scratch
              mountPath: /scratch
          resources:
            # A timed run: requests equal to limits, CPU included (the header's "Sizing").
            requests:
              cpu: "${cpu}"
              memory: "${memory}"
              ephemeral-storage: "${ephemeral_storage}"
            limits:
              cpu: "${cpu}"
              memory: "${memory}"
              ephemeral-storage: "${ephemeral_storage}"
          # The program is this harness's functions as declare -f prints them; the arguments after
          # it are \$0 and the commit, the repository's files and commits, and go test's timeout.
          command: ["/bin/bash", "-c"]
          args:
            - |
              set -euo pipefail
              repository=$(yaml_quote "$repository")
YAML
    awk '{ if (length($0) > 0) printf "              %s\n", $0; else print "" }' <<< "$program"
    cat << YAML
              adopt_main "\$@"
            - "adopt-large-checkout"
            - "${commit}"
            - "${files}"
            - "${commits}"
            - "${go_test_timeout}"
YAML
  } > "$2"
}

run_id() { od -An -N4 -tx1 /dev/urandom | tr -d ' \n'; }

# cpu_check <the Job as JSON in a file>: refuses where a container's CPU request is not its limit,
# as the namespace's CPU-burst policy leaves it where the pod template lacks its guaranteed-CPU
# label.
cpu_check() {
  local cpus
  cpus=$(jq -r '.spec.template.spec.containers[] | "\(.name) \(.resources.requests.cpu) \(.resources.limits.cpu)"' "$1") || return 1
  [ -n "$cpus" ] || { echo "the Job names no container's CPU"; return 1; }
  printf '%s\n' "$cpus" | awk 'function cores(q) { if (q ~ /^[+]?[0-9.]+m$/) return substr(q, 1, length(q) - 1) / 1000; if (q ~ /^[+]?[0-9.]+([eE][-+]?[0-9]+)?$/) return q + 0; return -1 }
    { print "  container " $1 ": cpu request " $2 ", limit " $3 }
    NF < 3 || cores($2) < 0 || cores($2) != cores($3) { bad = 1 } END { exit bad }'
}

echo "== adopt-large-checkout-job"
echo "namespace:     $namespace"
echo "image:         $image"
echo "commit:        $commit ($subject) of $repository"
echo "resources:     $cpu CPU, $memory memory, $disk disk of class $storage_class, deadline $deadline s"
echo "repository:    $files files over $commits commits"
echo "selector:"
printf '%s' "$selector_yaml"
command -v jq > /dev/null 2>&1 || refuse "no jq on PATH: the CPU the namespace leaves the Job is read with it"

if [ "$(kubectl -n "$namespace" auth can-i create jobs 2> /dev/null || true)" != yes ]; then
  echo "adopt-large-checkout-job: this pod may not create a Job in $namespace, so nothing runs here" >&2
  exit 2
fi

if [ "$dry_run" -eq 1 ]; then
  manifest=$keep/job-dry-run.yaml
  render_manifest "$(run_id)" "$manifest"
  echo
  echo "== the manifest, rendered and not created ($manifest)"
  cat "$manifest"
  echo
  # A server dry run runs the namespace's own admission, its mutations included, and creates
  # nothing: the first is the answer, the second the Job as the namespace would leave it.
  echo "== kubectl -n $namespace create --dry-run=server -f <the manifest above>"
  kubectl -n "$namespace" create --dry-run=server -f "$manifest" || {
    echo "adopt-large-checkout-job: the namespace's admission refused the manifest" >&2
    exit 2
  }
  kubectl -n "$namespace" create --dry-run=server -o json -f "$manifest" > "$work/dry-run.json" || {
    echo "adopt-large-checkout-job: the second server dry run, which reads the Job as admitted, failed" >&2
    exit 2
  }
  echo "== the CPU the namespace leaves the Job"
  cpu_check "$work/dry-run.json" || {
    echo "adopt-large-checkout-job: the namespace lowers the CPU request below its limit; give the pod template its guaranteed-CPU label (--label)" >&2
    exit 2
  }
  exit 0
fi

run_label=$(run_id)
manifest=$keep/job.yaml
render_manifest "$run_label" "$manifest"
echo
echo "== kubectl -n $namespace create --dry-run=server -f $manifest"
kubectl -n "$namespace" create --dry-run=server -f "$manifest" || {
  echo "adopt-large-checkout-job: the namespace's admission refused the manifest" >&2
  exit 2
}
echo
echo "== creating the Job ($run_label_key=$run_label)"
kubectl -n "$namespace" create -o json -f "$manifest" > "$work/created.json" || {
  echo "adopt-large-checkout-job: the create was refused; nothing of this run is left" >&2
  exit 2
}
workload=$(jq -r .metadata.name "$work/created.json")
echo "workload: $workload (uid $(jq -r .metadata.uid "$work/created.json"))"
echo "== the CPU the namespace left the Job"
cpu_check "$work/created.json" || {
  echo "adopt-large-checkout-job: the namespace lowered the CPU request below its limit, so the run would not be timed alone; give the pod template its guaranteed-CPU label (--label)" >&2
  exit 2
}

# pods: every attempt, oldest first, one line each: its name, creation, phase and container state.
pods() {
  kubectl -n "$namespace" get pods -l "batch.kubernetes.io/job-name=$workload" --sort-by=.metadata.creationTimestamp \
    -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.metadata.creationTimestamp}{" "}{.status.phase}{" "}{.status.containerStatuses[*].state}{"\n"}{end}' 2> /dev/null || true
}
conditions() {
  kubectl -n "$namespace" get job "$workload" \
    -o jsonpath='{range .status.conditions[?(@.status=="True")]}{.type}:{.reason} {end}' 2> /dev/null || true
}

echo
echo "== following the Job (at most $pod_wait s for a pod, then its deadline, $deadline s)"
# Each pass reads the pods, oldest first, and the Job's conditions. The newest pod is followed
# once its container runs or has ended, and once that container has terminated its whole log is
# read again (a follow of a running container can end early, with no error). A newer pod is a
# new attempt, followed the same way. The loop ends once the Job has ended and its newest pod's
# log is read, or that pod never ran, or it ended terminated_wait s ago with no such read.
wait_until=$((SECONDS + pod_wait + deadline))
pod=""
followed=""
read_pod=""
last=""
labelled=0
ended_at=""
waiting_said=""
while :; do
  now=$(pods)
  ended=$(conditions)
  if [ "$now" != "$last" ]; then
    echo "  [$(date -u +%FT%TZ)] pods:"
    printf '    %s\n' "${now:-none}"
    echo "  job conditions: [${ended:-none}] suspended: $(kubectl -n "$namespace" get job "$workload" -o jsonpath='{.spec.suspend}' 2> /dev/null || echo unread)"
    last=$now
  fi
  newest=$(printf '%s\n' "$now" | awk 'NF { line = $0 } END { print line }')
  pod=${newest%% *}
  started=0
  [[ $newest == *'"running"'* || $newest == *'"terminated"'* ]] && started=1
  if [ -n "$pod" ] && [ "$labelled" -eq 0 ]; then
    # The labels first: a role compares them when a pod answers nothing, and they go with the pod.
    kubectl -n "$namespace" get pods -l "batch.kubernetes.io/job-name=$workload" --show-labels || true
    labelled=1
  fi
  if [ -n "$pod" ] && [ "$pod" != "$read_pod" ] && [ "$started" -eq 1 ]; then
    if [ "$pod" != "$followed" ]; then
      echo
      echo "== kubectl -n $namespace logs -f pod/$pod"
      kubectl -n "$namespace" logs -f "pod/$pod" | tee "$work/follow.log" || echo "  the follow of $pod ended with an error"
      followed=$pod
      waiting_said=""
    fi
    terminated=$(kubectl -n "$namespace" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].state.terminated.reason}' 2> /dev/null || true)
    if [ -n "$terminated" ]; then
      echo "  [$(date -u +%FT%TZ)] $pod: its container terminated, $terminated"
      if kubectl -n "$namespace" logs "pod/$pod" > "$keep/$pod.log" 2> "$work/logs.err"; then
        if ! cmp -s "$work/follow.log" "$keep/$pod.log"; then
          echo
          echo "== the follow missed part of the log; the whole log of $pod, read once it terminated"
          cat "$keep/$pod.log"
        fi
      else
        echo "  the whole log of $pod could not be read: $(cat "$work/logs.err")"
        rm -f "$keep/$pod.log"
      fi
      read_pod=$pod
    elif [ -z "$waiting_said" ]; then
      echo "  [$(date -u +%FT%TZ)] the follow ended while $pod's container runs; reading its state until it terminates"
      waiting_said=1
    fi
  fi
  case " $ended " in
    *" Complete:"* | *" Failed:"*)
      [ -n "$ended_at" ] || ended_at=$SECONDS
      if [ -z "$pod" ] || [ "$pod" = "$read_pod" ] || [ "$started" -eq 0 ] || [ "$SECONDS" -ge $((ended_at + terminated_wait)) ]; then
        break
      fi
      ;;
  esac
  if [ "$SECONDS" -ge "$wait_until" ]; then
    echo "the Job reached no end inside $((pod_wait + deadline)) s"
    exit 3
  fi
  if [ -z "$pod" ] && [ "$SECONDS" -ge $((wait_until - deadline)) ]; then
    echo "no pod of $workload exists after ${pod_wait} s"
    kubectl -n "$namespace" describe job "$workload" | tail -n 20 || true
    exit 2
  fi
  sleep "$poll"
done
echo "  job conditions: [$ended]"

if [ -z "$read_pod" ] || [ "$read_pod" != "$pod" ] || [ ! -s "$keep/$read_pod.log" ]; then
  echo "the Job $workload ended with no whole log of its newest pod${pod:+ $pod} read here; read it from Cloud Logging by the Job's name, $workload"
  case " $ended " in
    *" Failed:DeadlineExceeded "*) exit 3 ;;
  esac
  exit 2
fi

# The image the pod actually ran, which ties the run to the digest named above.
ran=$(kubectl -n "$namespace" get pod "$read_pod" -o jsonpath='{.status.containerStatuses[0].imageID}' 2> /dev/null || true)
echo "  imageID: ${ran:-unreadable}"
case "$ran" in
  *"${image#*@}"*) ;;
  *)
    echo "the pod ran imageID [${ran:-unreadable}], which does not name the digest ${image#*@}"
    exit 2
    ;;
esac

verdict=$(tail -n 1 "$keep/$read_pod.log")
echo
echo "======== the verdict"
echo "commit: $commit; image: $image; Job: $workload; pod: $read_pod"
echo "the log's last line: $verdict"
case "$verdict" in
  "adopt-large-checkout: pass") exit 0 ;;
  "adopt-large-checkout: fail: "*) exit 1 ;;
esac
echo "the log's last line is no verdict, so it decided nothing"
case " $ended " in
  *" Failed:DeadlineExceeded "*) exit 3 ;;
esac
exit 2
