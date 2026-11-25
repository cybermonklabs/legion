#!/usr/bin/env bash
# The probe of scripts/e2e/pack-memory-job.sh, run inside the worker image by a Kubernetes Job. It
# measures the memory git and jj take in a large workspace under gVisor, for the checks
# c-git-rows, c-jj-snapshot-memory, c-handoff-head-read-memory, c-shim-adoption-memory and
# c-budget-under-gvisor of the measurement run, on a clone and a workspace made by the image's own
# code, and reads the image for the Job's halves of c-pack-window-config and c-release-source:
#
#   1. GNU time 1.9 from Debian trixie's package, checked by its sha256, since the image has no
#      /usr/bin/time; and two jj controls from the fork's releases, each checked by the asset's
#      sha256 and by its binary's: jj 0.45.1-sami.20260910-043938, R3 to R5's control, and jj
#      0.46.0-sami.20261008-234600, R10's, which maps each pack whole and must print its version.
#   2. The volume's /proc/mounts line, the image and its revision, `legion version`, git's
#      version and every jj's. The image reads: `git config --system --get` of the four pack
#      settings, in c-pack-window-config's order, and the image's `jj --version`, which must print
#      the release this checkout's worker.Dockerfile names (ARG JJ_TOOL, which the harness passes
#      as PACK_MEMORY_JJ_TOOL_VERSION). R10's released jj is the image's own: the binary its jj
#      wrapper runs, /usr/local/bin/jj, so a later pin needs no change here.
#   3. `legion workspace-init cache` in the background, `workspace-init fetch` from it, and
#      `workspace-init provision`, which looks jj up on PATH once (cmd/legion/workspace_init.go,
#      provisioningTools) and so runs every jj command of its own through the measuring jj this
#      probe puts first on PATH: one RESULT line each, with its argv, peak and wall time.
#   4. Under the role's own GIT_CONFIG block, which the harness put on this container, the four
#      pack settings at command scope, with the clone's local value beside each.
#   5. The git rows, each once with the role's settings and once with git's defaults forced (the
#      block unset, GIT_CONFIG_NOSYSTEM=1 and four -c values): in the workspace, `git log
#      --first-parent -- <walk file>`, `git status` and a push of a new commit to a `git daemon`
#      started before any measured command and outside every measured tree, which serves a bare
#      copy of the feed; and a fetch from GitHub of the newer commits of the clone's own default
#      branch into a clone that lacks them. K is the smallest step of a fixed ladder, 4096, 6400,
#      8192 and 12800 first-parent commits back from the branch's tip in the workspace's clone,
#      for which `git rev-list --objects --no-object-names <tip>~K..<tip>` take 100 MiB on the
#      clone's disk by `git cat-file --batch-check='%(objectsize:disk)'`; a bare repository then
#      holds the branch at <tip>~K alone, fetched by id from GitHub in one pack, and must lack
#      <tip> (a setup verdict), and each row fetches `+refs/heads/<branch>:refs/remotes/origin/
#      <branch>` from GitHub into a fresh `git clone --local --no-checkout` of it. The log prints
#      K, the estimate, and each new pack's bytes and objects. A row whose new pack is under
#      100 MiB is void; when no step of the ladder reaches 100 MiB, both rows and the setup
#      verdict are void, saying why, and the run goes on.
#   6. The copies of the provisioning root, repos/ and workspaces/ both: one for jj 0.45.1 (R3 to
#      R5's control), one for R10's control (jj 0.46.0 as sami's 28c47103 released it), and one for
#      the released jj at each JJ_PACK_MAPPED_LIMIT budget of R10, 64m, 256m, 512m and 1g. `df` and
#      the original's `du -s`
#      come first, and the run stops when the volume cannot hold every copy and one fresh root. A
#      copy hardlinks the clone's objects/pack/* from the original, since git never writes a pack
#      in place, and copies every other file with `cp -a`, objects/info/alternates among them, so a
#      copy of a clone provisioning made with `git clone --shared` reads the feed's objects through
#      the same alternates as the original; then its workspace's .git is pointed at
#      its own clone and `git worktree repair` run in it. Each workspace's `git rev-parse
#      --git-common-dir` must name its own clone's .git, and jj 0.45.1 must read its copied store
#      (`op log`, exit 0 and no `Reindexing`); the run stops otherwise. Each R10 copy's jj reads
#      its store the same way, and a copy whose jj cannot runs none of R10's role commands; its
#      provisioning rows, on a fresh root, still run.
#   7. On the original and the 0.45.1 copy, the same edits to the walk file and to a file of another
#      directory, with JJ_TIMESTAMP and JJ_OP_TIMESTAMP fixed and JJ_RANDOMNESS_SEED fixed per
#      command, then `jj diff`, `jj diff --name-only`, a commit made with `git commit` and pushed
#      with `git push`, handoff-record.ts's STANDING read, the fetch of the feed's heads, a commit,
#      the shim's describedArgs, a second commit and the shim's adoptionArgs as
#      packages/daemon/internal/shim/commands.go builds them: with the image's jj,
#      JJ_PACK_MAPPED_LIMIT unset, on the original, with jj 0.45.1 on its copy. The fetch is one
#      unmeasured `git fetch --no-tags <the feed> +refs/heads/*:refs/remotes/feed/*` into the copy's
#      shared clone, by the setup git and outside every measured tree: every branch head of the feed
#      as a remote-tracking ref the clone does not hold, since the clone, made from the feed, holds
#      each as origin's. The clone holds their objects, so the fetch writes refs and no pack, which
#      it prints; its setup verdict holds when it exits 0 and writes at least one ref the clone did
#      not hold. Each commit, by the setup git too, appends one line to a file, the walk file before
#      describedArgs and third_file, of a third top-level directory, before adoptionArgs, and
#      commits it with `git add` and `git commit`, as the handoff commit is made, with the probe's
#      identity and both of git's dates fixed; its setup verdict holds when HEAD moved to it, a
#      child of the HEAD before. The two copies' commits before each command differ only in their
#      parents, which a verdict checks: the commit before describedArgs is made on each copy's own
#      handoff commit, and the one before adoptionArgs on that copy's commit before describedArgs.
#      describedArgs, the next command, imports the new refs and the new HEAD at its start and
#      snapshots against that HEAD's tree, and adoptionArgs, the command after the second commit,
#      imports that one's HEAD the same way and reads the old tree of third_file's directory from
#      the pack, as jj does in a workspace colocated with git, which a workspace whose .git is a
#      worktree of the clone is: what a role's jj does once git has fetched new branches into the
#      shared clone and committed in the workspace. That import makes the new working-copy commit
#      under the command's own identity, so the adopter is @'s author already when the measured
#      adoptionArgs's metaedit runs, which then changes nothing. The author is shown on an
#      unmeasured run instead, after the measured one and outside every measured tree: `jj new` as
#      the probe, @'s author read with --ignore-working-copy, the same adoptionArgs argv as the
#      adopter, @'s author read again, and `jj op log -n1`'s description. c-shim-adoption-memory's
#      author verdict holds when @'s author was the probe's before that adoptionArgs and is the
#      adopter's after it, and the operation is metaedit's, `edit commit metadata for commit <id>`.
#   8. R3 to R5's verdicts.
#   9. R10's role commands on each R10 copy, with its jj and budget, JJ_TIMESTAMP and
#      JJ_OP_TIMESTAMP fixed, and a JJ_RANDOMNESS_SEED for each command, the same on every copy:
#      `jj --ignore-working-copy file list -r @-`, the STANDING read, step 7's fetch of the feed's
#      heads and its two commits, describedArgs, adoptionArgs, `jj new`, `jj diff` after the same
#      edits in two directories, and `jj --ignore-working-copy log -r '::@-' -- <walk file>`. The
#      five copies' HEADs are one commit before the commits, so each commit is one id on all five,
#      which a verdict checks, and each released row's stdout can equal its control's.
#  10. R10's provisioning rows, for R10's control and for each budget, each on a fresh root: a plain
#      `git clone --no-checkout` of the feed, its packs hardlinked into the clone's own object
#      store, where provisioning's clone is `git clone --shared` and reads them from the feed
#      through alternates, then every jj command the measuring
#      jj recorded for the original's provision, in its order and with its argv, run with that jj
#      and budget. Each path of the original root in an argv is rewritten to the fresh root, and
#      provisioning's temporary clone (<clone>.clone-<digits>, which it renames into place once
#      the clone is made) is named as the clone it becomes, which the fresh clone already is. The
#      root is removed once its rows are read.
#  11. R10's verdicts, each beside one summary RESULT line for its command and budget.
#
# Every measured command runs under pack-memory-peak.py beside this file, which prints one
# `RESULT {json}` line for it: label, argv, exit, wall seconds, GNU time's maximum resident set,
# the largest sampled total of its process tree and, from that same sample, the pack mappings' Rss,
# the anonymous memory and the mapping and file counts; over every sample, the largest pack Rss,
# anonymous memory and other-file Rss, and the most mappings one sample held of each pack file;
# each process of the tree, with its .pack, .idx, other-file and anonymous memory at its own
# largest total; and the pod's charge: the cgroup's memory charge read just before the command
# starts and at every sample, its peak less that baseline, and the charge over the baseline at
# the sample of the tree's largest total. Under gVisor a file two processes map is charged once,
# where the tree's total counts it in each, so the charge differs from the total. A push talks to
# the git daemon, which runs in the pod outside the measured tree, so its rows also sample the
# daemon's tree and report its largest total. The sampler stops the measured tree (SIGSTOP, and
# SIGCONT once read) while it reads each sample, the git daemon's tree aside: a jj that drops one
# chunk and maps the next thousands of times a second, read running over the milliseconds one
# smaps read takes, is counted holding both. It reports how many samples it stopped the tree in,
# the summed, median and longest time the tree was stopped, and how many samples held a process
# that did not stop in time; the wall time includes the stops. Each verdict of a git or jj row
# prints the stopped time beside the wall time, and the pod's charge after the row's figures,
# with the daemon's total beside it for a row the daemon served.
# Every verdict prints one `CHECK <name>: got [x] want [y]` line, followed by ` MISMATCH` when it
# failed and ` VOID` when a control did not reach its bound; the last line is `JOB-RESULT pass`,
# when no verdict failed, or `JOB-RESULT fail`.
#
# A git row (c-git-rows) prints each process of its tree on a line of its own before its verdict.
# With the role's settings, git log, git status and the fetch hold when they exit 0 and the pod's
# charge is under 2 GiB (2,097,152 KiB), and the push when it exits 0 and its own process tree's
# largest sampled total is, since its receiving daemon shares the pod. Each row with git's
# defaults forced is a control that must reach 2 GiB by its settings row's figure, or its line is
# VOID. A row of the image's jj (c-jj-snapshot-memory, c-handoff-head-read-memory,
# c-shim-adoption-memory) holds when it exits 0 and both GNU time's figure and the sampled total
# are under 2 GiB; its jj 0.45.1 control must reach the size of the pack, read from that copy's
# clone, by either figure, or its line is VOID.
#
# R10's rows have two figures. The pack figure is the row's largest sampled pack Rss, read with
# the tree stopped; the derived figure is GNU time's figure less its largest sampled anonymous
# memory and less its largest sampled other-file Rss, which catches a mapping a sample missed. A
# released row holds when it exits 0, its pack figure is within its budget plus one 8 MiB chunk
# for each object store plus 32 MiB, its derived figure is within that bound plus 128 MiB, and its
# stdout equals the control row's. The count of object stores is read from the control row of the
# same command: the most mappings one sample held of its largest pack file, since that build maps
# a pack whole once for each store. A control row's larger figure must reach 90% of the .pack
# file's size, or its line is a MISMATCH: a control that reads no pack proves nothing about its
# patched row. Each R10 RESULT line carries `r10` in place of a label: the command, the copy, the
# budget, whether its stdout equals the control row's, its largest sampled pack, other-file and
# anonymous memory, GNU time's figure, its wall time and stopped time, the pod's charge, its pack
# and derived figures, the store count and both bounds.
#
# It is no proof of its own and nothing runs it from a checkout: the harness beside it writes this
# text and pack-memory-peak.py into the Job's command, and the Job writes both onto its volume and
# runs this. The three files are one recipe; read them together, and change none alone.
#
# Arguments: --repo <owner>/<name> (default mozilla-firefox/firefox) and --walk-file <path in
# the repository> (default config/milestone.txt), the one file the git log row walks and the
# jj rows edit first.
#
# No `set -e`: a step that fails is reported, and the probe goes on with every later row that can
# still run. A step every later row needs, and the checks the run stops on, stop it instead.
# Either way every verdict is printed, a verdict whose row never ran as a MISMATCH, before the
# JOB-RESULT line, and then the probe exits 0. It exits 0 on `JOB-RESULT fail` too: the harness
# reads the verdict from the log, and a non-zero exit would have the Job start the whole run again
# on another pod, which only a lost machine is for.
set -u

scratch=/scratch
here=$scratch/pack-memory
peak=$here/pack-memory-peak.py

repo=mozilla-firefox/firefox
walk_file=config/milestone.txt
# The jj rows' second edit, in another directory than the walk file's. Firefox holds this file; a
# repository that does not gets it as a new file, which is an edit of its own directory as well.
second_file=browser/config/version.txt
# The file the commit before adoptionArgs changes: one of a third top-level directory, which no
# earlier commit of a copy touches, so the tree jj compares it against comes from the pack.
third_file=toolkit/moz.build

bound_kib=2097152            # 2 GiB, the bound of every row of the image's jj and of the settings
fetch_floor=104857600        # 100 MiB: a fetch row whose new pack is smaller exercises nothing
# The fetch row's ladder: the first-parent steps K back from the default branch's tip, the smallest
# of which whose <tip>~K..<tip> objects take fetch_floor on the clone's disk makes the clone that
# lacks them.
fetch_ladder=(4096 6400 8192 12800)
row_timeout=7200             # seconds any one measured command may run

# The four pack settings and their values (R1), and git's defaults as c-git-rows forces them.
# git 2.47.3 takes the suffixes k, m and g only, so the limit's 32 TiB is written in g.
setting_keys=(core.packedGitWindowSize core.packedGitLimit pack.windowMemory core.deltaBaseCacheLimit)
setting_values=(32m 256m 64m 64m)
defaults=(-c core.packedGitWindowSize=1g -c core.packedGitLimit=32768g -c pack.windowMemory=0 -c core.deltaBaseCacheLimit=96m)

time_url=https://deb.debian.org/debian/pool/main/t/time/time_1.9-0.2_amd64.deb
time_sha256=10257c6fe888e6578550b1f20705d282dd630b76f8df0c006aec3031fefe5e23
control_version=0.45.1-sami.20260910-043938
control_url=https://github.com/sjawhar/jj/releases/download/v${control_version}/jj-v${control_version}-x86_64-unknown-linux-musl.tar.gz
control_asset_sha256=19f6ffdecb4b3eb9e15bdeb3248f891c9e2f26fffe816c2771171243f96e01db
control_binary_sha256=fe32ffcd64432000027c87030af074eb48e55181ca389e09d3ab4b44200aba7a
# R10's control: jj 0.46.0 as the fork released it from sami's 28c47103, which maps each pack whole.
r10_control_version=0.46.0-sami.20261008-234600
r10_control_url=https://github.com/sjawhar/jj/releases/download/v${r10_control_version}/jj-v${r10_control_version}-x86_64-unknown-linux-musl.tar.gz
r10_control_asset_sha256=7ee6a7f6fd9f80285a0bee4e1f44d71264fca4c290c1893b9db4b6c811644a9a
r10_control_binary_sha256=3ac2d61600a732171179d3d94c432d98aa28233c0db206a96f97290d54689a62
r10_control_prints="jj 0.46.0-sami.20261008-234600-28c47103ae4e0ea8f8ab770de1422ad6f8a8c9ef"
# R10's released jj: the image's own, the binary its jj wrapper (/opt/legion/bin/jj) runs.
released_jj=/usr/local/bin/jj

issue=PACKMEM-1               # the issue key provisioning names the workspace after
cache_listen=127.0.0.1:8418
daemon_port=9418
jj_timestamp=2026-11-19T00:00:00+00:00
adopter_name='Pack Memory Adopter'
adopter_email=adopter@example.invalid

# The remote of the refs the fetch before each describedArgs writes, one for each branch head of
# the feed: a remote the clone does not hold, since the clone, made from the feed, holds each of
# those heads as origin's already.
feed_remote=feed

# R10: the four JJ_PACK_MAPPED_LIMIT budgets, each in KiB; one chunk of the chunked mapping; the
# 32 MiB the pack figure's bound allows beyond the budget and the chunks; the 128 MiB the derived
# figure's bound allows beyond that, for an other-file or Anonymous peak a sample missed; and the
# share of the .pack file's size a control row's larger figure must reach, in percent.
budgets=(64m 256m 512m 1g)
declare -A budget_kib=([64m]=65536 [256m]=262144 [512m]=524288 [1g]=1048576)
chunk_kib=8192
slack_kib=32768
derived_slack_kib=131072
control_share=90
# The copies of the provisioning root, each at $scratch/<copy>: jj 0.45.1's for R3 to R5, then
# R10's, R10's control's and the released jj's at each budget, with the jj and the budget each runs
# (copy_jj is filled once the binaries are read). R10's commands, by the names its labels and
# verdicts take: the role's seven, then provisioning's three.
r10_copies=(r10-control "${budgets[@]/#/r10-}")
copies=(control "${r10_copies[@]}")
declare -A copy_jj=() copy_budget=()
for budget in "${budgets[@]}"; do
  copy_budget[r10-$budget]=$budget
done
r10_role_commands=(file-list standing described adoption new diff log-walk)
r10_provision_commands=(git-init workspace-add sparse-reset)

# --------------------------------------------------------------------------------------------
# The verdicts. `expected` is every verdict this probe gives, in order; finish prints each one no
# row decided as a MISMATCH, so a run that stopped early can never read as a pass.
# --------------------------------------------------------------------------------------------
expected=(
  "setup arguments"
  "setup GNU time"
  "setup control jj asset"
  "setup control jj binary"
  "setup r10-control jj asset"
  "setup r10-control jj binary"
  "setup r10-control jj version"
  "setup r10 released jj"
)
for key in "${setting_keys[@]}"; do
  expected+=("c-pack-window-config system $key")
done
expected+=(
  "c-release-source image jj version"
  "setup git daemon"
  "setup cache ready"
  "setup fetch"
  "setup provision"
  "setup walk file"
  "setup image workspace's git common dir"
)
for key in "${setting_keys[@]}"; do
  expected+=("c-git-rows role scope $key" "c-git-rows clone local $key")
done
for row in log status push; do
  expected+=("c-git-rows $row settings" "c-git-rows $row defaults")
done
expected+=("setup fetch lacking clone" "c-git-rows fetch settings" "c-git-rows fetch defaults")
expected+=("setup room for the copies")
for copy in "${copies[@]}"; do
  expected+=("setup $copy workspace's git common dir")
done
for copy in "${copies[@]}"; do
  expected+=("setup $copy jj reads the copied store")
done
for copy in image control; do
  expected+=("setup $copy fetch of the feed's heads" "setup $copy commit before describedArgs" "setup $copy commit before adoptionArgs")
done
expected+=("setup image and control commits before describedArgs" "setup image and control commits before adoptionArgs")
expected+=(
  "c-jj-snapshot-memory diff image"
  "c-jj-snapshot-memory diff control"
  "c-jj-snapshot-memory name-only image"
  "c-jj-snapshot-memory name-only control"
  "c-jj-snapshot-memory name-only outputs"
  "c-handoff-head-read-memory push image"
  "c-handoff-head-read-memory standing image"
  "c-handoff-head-read-memory standing image commit"
  "c-handoff-head-read-memory push control"
  "c-handoff-head-read-memory standing control"
  "c-handoff-head-read-memory standing control commit"
  "c-shim-adoption-memory undescribed image"
  "c-shim-adoption-memory described image"
  "c-shim-adoption-memory adoption image"
  "c-shim-adoption-memory author image"
  "c-shim-adoption-memory undescribed control"
  "c-shim-adoption-memory described control"
  "c-shim-adoption-memory adoption control"
  "c-shim-adoption-memory author control"
)
for copy in "${r10_copies[@]}"; do
  expected+=("setup $copy fetch of the feed's heads" "setup $copy commit before describedArgs" "setup $copy commit before adoptionArgs")
done
expected+=("setup r10 copies' commit id before describedArgs" "setup r10 copies' commit id before adoptionArgs")
for copy in "${r10_copies[@]}"; do
  expected+=("setup $copy fresh root")
done
for command in "${r10_role_commands[@]}" "${r10_provision_commands[@]}"; do
  expected+=("c-budget-under-gvisor $command control")
  for budget in "${budgets[@]}"; do
    expected+=("c-budget-under-gvisor $command $budget" "c-budget-under-gvisor $command $budget output")
  done
done

declare -A decided=()
passes=0
fails=0
voids=0

say() { printf '[%s] %s\n' "$(date -u +%FT%TZ)" "$*"; }

# group <n>: n with its digits grouped in threes, or n as it came when it is no whole number.
group() {
  local n=$1 grouped=""
  [[ $n =~ ^[0-9]+$ ]] || { printf '%s' "$n"; return; }
  while [ "${#n}" -gt 3 ]; do
    grouped=",${n: -3}$grouped"
    n=${n:0:${#n}-3}
  done
  printf '%s' "$n$grouped"
}

verdict() { # verdict <name> <pass|fail|void> <got> <want> [why]
  local name=$1 word=$2 got=${3//$'\n'/ } want=${4//$'\n'/ } why=${5:-} known=0 each
  why=${why//$'\n'/ }
  for each in "${expected[@]}"; do
    [ "$each" = "$name" ] && known=1 && break
  done
  if [ "$known" -eq 0 ] || [ -n "${decided[$name]:-}" ]; then
    echo "CHECK probe: got [a verdict named $name] want [each expected name decided once] MISMATCH: this probe decides a verdict it does not list, or one twice"
    fails=$((fails + 1))
    return
  fi
  decided[$name]=1
  case "$word" in
    pass)
      passes=$((passes + 1))
      echo "CHECK $name: got [$got] want [$want]"
      ;;
    void)
      voids=$((voids + 1))
      echo "CHECK $name: got [$got] want [$want] VOID${why:+: $why}"
      ;;
    *)
      fails=$((fails + 1))
      echo "CHECK $name: got [$got] want [$want] MISMATCH${why:+: $why}"
      ;;
  esac
}

same() { # same <name> <got> <want>
  if [ "$2" = "$3" ]; then verdict "$1" pass "$2" "$3"; else verdict "$1" fail "$2" "$3"; fi
}

# The run stops at a step every later row needs; going says whether it has not.
stopped=""
stop() {
  stopped=$1
  say "STOPPED: $1"
}
going() { [ -z "$stopped" ]; }

# --------------------------------------------------------------------------------------------
# The rows' figures, read back from the results file the sampler appends to.
# --------------------------------------------------------------------------------------------
out=$scratch/rows
results=$scratch/results.jsonl
time_bin=""

field() { # field <label> <jq path>: the value in the label's last result, "null" when absent
  jq -r --arg label "$1" "select(.label == \$label) | ($2 | tostring)" "$results" 2> /dev/null | tail -n 1
}

has_result() { [ -n "$(field "$1" .label)" ]; }

figures() { # figures <label>: one line of a row's figures, for a verdict's got
  printf 'exit %s, GNU time %s KiB, sampled %s KiB (pack %s KiB, anonymous %s KiB, %s pack mappings, %s resident, of %s files), wall %s s (%s); %s' \
    "$(field "$1" .exit)" "$(group "$(field "$1" .maxrss_kib)")" "$(group "$(field "$1" .sampled_total_kib)")" \
    "$(group "$(field "$1" .sampled_pack_kib)")" "$(group "$(field "$1" .sampled_anon_kib)")" \
    "$(field "$1" .pack_mappings)" "$(field "$1" .pack_mappings_resident)" "$(field "$1" .pack_files)" "$(field "$1" .wall_s)" \
    "$(stops "$1")" "$(charge "$1")"
}

# stops <label>: the time the sampler held a row's tree stopped to read it, which its wall time
# includes, for a verdict's got beside that wall time. No verdict reads it.
stops() {
  printf '%s ms of it stopped, in %s samples, %s of them with a process not stopped in time' \
    "$(field "$1" .stop_ms_total)" "$(field "$1" .samples_stopped)" "$(field "$1" .samples_not_stopped_in_time)"
}

# charge <label>: the pod's charge of a row, for a verdict's got after its figures: the cgroup's
# peak less its baseline from just before the command started, then the peak, the baseline and
# the charge over the baseline at the sample of the tree's largest total; and, for a row the git
# daemon served, the daemon's largest tree total, which the charge holds and the row's tree does
# not. The c-git-rows verdicts of git log, git status and the fetch read that charge, the
# result's charge_field; no verdict reads the git daemon's total.
charge() {
  local less daemon text
  less=$(field "$1" .cgroup_peak_less_baseline_kib)
  if signed "$less"; then
    text="pod charge $(group "$less") KiB (cgroup peak $(group "$(field "$1" .cgroup_peak_kib)") KiB less baseline $(group "$(field "$1" .cgroup_baseline_kib)") KiB; $(group "$(field "$1" .cgroup_at_largest_sample_kib)") KiB over the baseline at the tree's largest sample)"
  elif [ "$less" = null ]; then
    text="pod charge unread: no sample was taken"
  else
    text="pod charge ${less:-unread}"
  fi
  daemon=$(field "$1" .daemon_tree_peak_kib)
  [ "$daemon" = null ] || [ -z "$daemon" ] || text+=", git daemon's tree $(group "$daemon") KiB"
  printf '%s' "$text"
}

whole() { [[ $1 =~ ^[0-9]+$ ]]; }

# under_bound <name> <label>: a row of the image's jj: it exits 0 and both figures are under the
# bound.
under_bound() {
  local name=$1 label=$2 maxrss sampled want
  want="exit 0, GNU time and sampled both under $(group "$bound_kib") KiB"
  if ! has_result "$label"; then
    verdict "$name" fail "no result for $label" "$want" "the row did not run"
    return
  fi
  maxrss=$(field "$label" .maxrss_kib)
  sampled=$(field "$label" .sampled_total_kib)
  if [ "$(field "$label" .exit)" != 0 ]; then
    verdict "$name" fail "$(figures "$label")" "$want" "the command failed"
  elif ! whole "$maxrss" || ! whole "$sampled"; then
    verdict "$name" fail "$(figures "$label")" "$want" "a figure is missing"
  elif [ "$maxrss" -lt "$bound_kib" ] && [ "$sampled" -lt "$bound_kib" ]; then
    verdict "$name" pass "$(figures "$label")" "$want"
  else
    verdict "$name" fail "$(figures "$label")" "$want" "over the bound"
  fi
}

# reaches_bound <name> <label> <bound KiB> <what the bound is>: a control row. It must reach its
# bound by either figure, or its line is VOID: a control that stays under its bound shows nothing
# about the row it controls. A control that never ran is a broken run, a MISMATCH.
reaches_bound() {
  local name=$1 label=$2 bound=$3 what=$4 maxrss sampled top want
  want="GNU time or sampled at or over $(group "$bound") KiB ($what)"
  if ! has_result "$label"; then
    verdict "$name" fail "no result for $label" "$want" "the control did not run"
    return
  fi
  maxrss=$(field "$label" .maxrss_kib)
  sampled=$(field "$label" .sampled_total_kib)
  whole "$maxrss" || maxrss=0
  whole "$sampled" || sampled=0
  top=$maxrss
  [ "$sampled" -gt "$top" ] && top=$sampled
  if ! whole "$bound"; then
    verdict "$name" fail "$(figures "$label")" "$want" "the bound could not be read"
  elif [ "$(field "$label" .exit)" != 0 ]; then
    verdict "$name" void "$(figures "$label")" "$want" "the control did not run to its end"
  elif [ "$top" -ge "$bound" ]; then
    verdict "$name" pass "$(figures "$label")" "$want"
  else
    verdict "$name" void "$(figures "$label")" "$want" "the control stayed under its bound"
  fi
}

# exits_zero <name> <label> <what>: a row whose verdict is that it ran, not what it held.
exits_zero() {
  if ! has_result "$2"; then
    verdict "$1" fail "no result for $2" "$3 exits 0" "the row did not run"
  elif [ "$(field "$2" .exit)" = 0 ]; then
    verdict "$1" pass "$(figures "$2")" "$3 exits 0"
  else
    verdict "$1" fail "$(figures "$2")" "$3 exits 0" "$(field "$2" .stderr_tail)"
  fi
}

# print_processes <label>: each process of a row's tree on a line of its own, largest first: its
# pid, the first three words of its cmdline and, at the sample where its own total was largest,
# that total and its .pack, .idx, other file and anonymous memory.
print_processes() {
  jq -r --arg label "$1" 'select(.label == $label) | .processes // [] | .[] |
    "  \($label) process \(.pid) [\(.name)]: largest total \(.total_kib) KiB at \(.at_s) s: .pack \(.pack_kib) KiB, .idx \(.idx_kib) KiB, other file \(.other_file_kib) KiB, anonymous \(.anon_kib) KiB"' \
    "$results" 2> /dev/null | awk -v label="$1" 'BEGIN { n = 0 } { lines[n++] = $0 } END { if (n == 0) print "  " label ": no process was sampled"; for (i = 0; i < n; i++) print lines[i] }'
}

signed() { [[ $1 =~ ^-?[0-9]+$ ]]; }

# git_holds <name> <label> <field> <what> [void reason]: a git row with the role's settings
# (c-git-rows): it exits 0 and its figure, the result's field, is under 2 GiB. Its processes are
# printed before its line. A reason makes the row void instead, whatever its figures.
git_holds() {
  local name=$1 label=$2 key=$3 what=$4 void=${5:-} value want
  want="exit 0, $what under $(group "$bound_kib") KiB"
  print_processes "$label"
  if ! has_result "$label"; then
    verdict "$name" fail "no result for $label" "$want" "the row did not run"
    return
  fi
  value=$(field "$label" "$key")
  if [ "$(field "$label" .exit)" != 0 ]; then
    verdict "$name" fail "$(figures "$label")" "$want" "the command failed"
  elif ! signed "$value"; then
    verdict "$name" fail "$(figures "$label")" "$want" "$what could not be read: $value"
  elif [ -n "$void" ]; then
    verdict "$name" void "$(figures "$label")" "$want" "$void"
  elif [ "$value" -lt "$bound_kib" ]; then
    verdict "$name" pass "$(figures "$label")" "$want"
  else
    verdict "$name" fail "$(figures "$label")" "$want" "over the bound"
  fi
}

# git_control <name> <label> <field> <what> [void reason]: a git row with git's defaults forced, the
# control of the settings row judged by the same figure: that figure must reach 2 GiB, or the line
# is VOID. A control that never ran, or whose figure cannot be read, is a MISMATCH.
git_control() {
  local name=$1 label=$2 key=$3 what=$4 void=${5:-} value want
  want="$what at or over $(group "$bound_kib") KiB (2 GiB)"
  print_processes "$label"
  if ! has_result "$label"; then
    verdict "$name" fail "no result for $label" "$want" "the control did not run"
    return
  fi
  value=$(field "$label" "$key")
  if [ -n "$void" ]; then
    verdict "$name" void "$(figures "$label")" "$want" "$void"
  elif [ "$(field "$label" .exit)" != 0 ]; then
    verdict "$name" void "$(figures "$label")" "$want" "the control did not run to its end"
  elif ! signed "$value"; then
    verdict "$name" fail "$(figures "$label")" "$want" "$what could not be read: $value"
  elif [ "$value" -ge "$bound_kib" ]; then
    verdict "$name" pass "$(figures "$label")" "$want"
  else
    verdict "$name" void "$(figures "$label")" "$want" "the control stayed under its bound"
  fi
}

measure() { # measure <label> [sampler options…] -- <command…>: one measured command, in this directory
  local label=$1
  shift
  python3 "$peak" --label "$label" --time-bin "$time_bin" --out-dir "$out" --results "$results" \
    --timeout "$row_timeout" "$@"
}

# The git that sets the run up rather than being measured: the image's own, behind the wrapper,
# with the role's block left out, so no pin of the role's reaches a step that is not a row.
setup_git() { env -u GIT_CONFIG_COUNT /usr/bin/git "$@"; }

largest_pack_bytes() { # largest_pack_bytes <clone>: its largest pack's size, in bytes
  local bytes
  bytes=$(find "$1/.git/objects/pack" -maxdepth 1 -name '*.pack' -printf '%s\n' 2> /dev/null | sort -n | tail -n 1)
  whole "$bytes" && echo "$bytes"
}

largest_pack_kib() { # largest_pack_kib <clone>: its largest pack's size, in KiB
  local bytes
  bytes=$(largest_pack_bytes "$1") && echo $((bytes / 1024))
}

common_dir() { # common_dir <workspace>: the git common dir git names for it, resolved
  local dir
  dir=$(setup_git -C "$1" rev-parse --path-format=absolute --git-common-dir 2>&1) && dir=$(realpath -m "$dir")
  printf '%s' "$dir"
}

common_dir_matches() { # common_dir_matches <name> <workspace> <clone>: the guard, as a verdict
  local got want
  got=$(common_dir "$2")
  want=$(realpath -m "$3/.git")
  same "$1" "$got" "$want"
  [ "$got" = "$want" ]
}

copy_clone() { printf '%s' "$scratch/$1/repos/github.com/$owner/$name"; } # copy_clone <copy>
copy_ws() { printf '%s' "$scratch/$1/workspaces/$owner/$name/${issue,,}"; } # copy_ws <copy>

# copy_root <to>: a copy of the provisioning root at <to>. The clone's objects/pack/* are hardlinks
# of the original's, since git writes a pack once and never in place; every other file is copied
# with `cp -a`, entry by entry beside the path down to that directory. Each directory on the path
# takes the original's mode and times once it is filled, as `cp -a` would have given it.
copy_root() {
  local to=$1 from=$root part entry i
  local -a parts made
  IFS=/ read -r -a parts <<< "${clone#"$root"/}/.git/objects/pack"
  mkdir "$to" || return 1
  made=("$from" "$to")
  for part in "${parts[@]}"; do
    for entry in "$from"/* "$from"/.[!.]* "$from"/..?*; do
      [ -e "$entry" ] || [ -L "$entry" ] || continue
      [ "${entry##*/}" != "$part" ] || continue
      cp -a "$entry" "$to/" || return 1
    done
    from=$from/$part
    to=$to/$part
    mkdir "$to" || return 1
    made+=("$from" "$to")
  done
  for entry in "$from"/* "$from"/.[!.]* "$from"/..?*; do
    [ -e "$entry" ] || [ -L "$entry" ] || continue
    ln "$entry" "$to/" || return 1
  done
  for ((i = 0; i < ${#made[@]}; i += 2)); do
    chmod --reference="${made[i]}" "${made[i + 1]}" || return 1
    touch -r "${made[i]}" "${made[i + 1]}" || return 1
  done
}

# R10's two figures of a row, in KiB. Its pack figure is its largest sampled pack Rss
# (max_pack_kib), each sample read with the tree stopped. derived_figure <label> prints its
# derived figure: GNU time's peak less its largest sampled Anonymous memory and less its largest
# sampled Rss of other file mappings (the jj binary, git's index, GNU time's own), which can be
# negative; nothing when a figure is missing. A sample can miss a mapping that comes and goes
# between two reads, and the derived figure catches it.
derived_figure() {
  local maxrss anon other
  maxrss=$(field "$1" .maxrss_kib)
  anon=$(field "$1" .max_anon_kib)
  other=$(field "$1" .max_other_file_kib)
  if whole "$maxrss" && whole "$anon" && whole "$other"; then
    echo $((maxrss - anon - other))
  fi
}

r10_figures() { # r10_figures <label> <derived figure>: one line of an R10 row's figures, for a verdict's got
  printf 'exit %s, pack figure %s KiB (its largest sampled pack Rss), derived figure %s KiB (GNU time %s KiB less anonymous %s KiB and other file %s KiB); %s mappings of its largest pack file; wall %s s (%s); %s' \
    "$(field "$1" .exit)" "$(group "$(field "$1" .max_pack_kib)")" "$(group "${2:-unread}")" \
    "$(group "$(field "$1" .maxrss_kib)")" "$(group "$(field "$1" .max_anon_kib)")" \
    "$(group "$(field "$1" .max_other_file_kib)")" "$(field "$1" .largest_pack_file_mappings)" "$(field "$1" .wall_s)" \
    "$(stops "$1")" "$(charge "$1")"
}

# same_output <label> <control label>: 0 when both rows ran and their stdout is the same bytes, 1
# when it is not, 2 when either row has no result.
same_output() {
  has_result "$1" && has_result "$2" || return 2
  cmp -s "$out/$1.out" "$out/$2.out"
}

# r10_result <command> <copy> <label> <outputs equal: true, false or null> <derived figure> <stores>
# <bound>: R10's RESULT line for one command and budget, built from the row's own result. A
# released row's derived figure is bounded by its bound plus 128 MiB.
r10_result() {
  local line
  line=$(jq -c --arg command "$1" --arg copy "$2" --arg label "$3" --argjson equal "$4" \
    --arg budget "${copy_budget[$2]:-unset}" --arg derived "$5" --arg stores "$6" --arg bound "$7" \
    --argjson slack "$derived_slack_kib" \
    'select(.label == $label) | {r10: $command, copy: $copy, jj: .argv[0], budget: $budget, label,
      exit, outputs_equal: $equal, max_pack_kib, max_other_file_kib, max_anon_kib, maxrss_kib, wall_s,
      stop_ms_total, cgroup_peak_less_baseline_kib, pack_figure_kib: .max_pack_kib,
      derived_figure_kib: ($derived | tonumber? // null), stores: ($stores | tonumber? // null),
      bound_kib: ($bound | tonumber? // null),
      derived_bound_kib: ($bound | tonumber? // null | if . == null then null else . + $slack end)}' \
    "$results" 2> /dev/null | tail -n 1)
  [ -n "$line" ] || line="{\"r10\": \"$1\", \"copy\": \"$2\", \"label\": \"$3\", \"ran\": false}"
  echo "RESULT $line"
}

# r10_control <name> <label> <pack bytes> <derived figure>: a row of R10's control (r10-control).
# Its larger figure, the larger of its pack and derived figures, must reach 90% of the .pack file's
# size, or the check fails: a control row that reads no pack proves nothing about its patched row.
r10_control() {
  local name=$1 label=$2 bytes=$3 derived=$4 pack least larger want
  if whole "$bytes"; then
    least=$(((bytes * control_share + 102399) / 102400))
    want="larger figure at or over $(group "$least") KiB, $control_share% of the .pack file's $(group "$bytes") bytes"
  else
    want="larger figure at or over $control_share% of the .pack file's size"
  fi
  if ! has_result "$label"; then
    verdict "$name" fail "no result for $label" "$want" "the control did not run"
    return
  fi
  pack=$(field "$label" .max_pack_kib)
  larger=$pack
  if signed "$derived" && whole "$pack" && [ "$derived" -gt "$pack" ]; then
    larger=$derived
  fi
  if ! whole "$bytes"; then
    verdict "$name" fail "$(r10_figures "$label" "$derived")" "$want" "the .pack file's size could not be read"
  elif [ "$(field "$label" .exit)" != 0 ]; then
    verdict "$name" fail "$(r10_figures "$label" "$derived")" "$want" "the control did not run to its end"
  elif ! whole "$pack" || ! signed "$derived"; then
    verdict "$name" fail "$(r10_figures "$label" "$derived")" "$want" "a figure is missing"
  elif [ "$larger" -ge "$least" ]; then
    verdict "$name" pass "$(r10_figures "$label" "$derived"); larger figure $(group "$larger") KiB" "$want"
  else
    verdict "$name" fail "$(r10_figures "$label" "$derived"); larger figure $(group "$larger") KiB" "$want" \
      "the control read less than $control_share% of the pack, so it proves nothing about its patched row"
  fi
}

# r10_bound <name> <label> <budget> <stores> <bound KiB> <derived figure>: a released row at its
# budget. It exits 0, its pack figure is within the budget plus one chunk for each object store
# plus 32 MiB, and its derived figure within that bound plus 128 MiB, which covers a missed
# other-file or Anonymous peak and not a pack mapped whole.
r10_bound() {
  local name=$1 label=$2 budget=$3 stores=$4 bound=$5 derived=$6 pack want derived_bound=unread
  whole "$bound" && derived_bound=$(group $((bound + derived_slack_kib)))
  want="exit 0, pack figure within $(group "${bound:-unread}") KiB ($budget, ${stores:-unread} stores of one $chunk_kib KiB chunk, and $slack_kib KiB) and derived figure within $derived_bound KiB (that and $derived_slack_kib KiB)"
  if ! has_result "$label"; then
    verdict "$name" fail "no result for $label" "$want" "the row did not run"
    return
  fi
  pack=$(field "$label" .max_pack_kib)
  if [ "$(field "$label" .exit)" != 0 ]; then
    verdict "$name" fail "$(r10_figures "$label" "$derived")" "$want" "the command failed"
  elif ! whole "$pack" || ! signed "$derived"; then
    verdict "$name" fail "$(r10_figures "$label" "$derived")" "$want" "a figure is missing"
  elif ! whole "$bound"; then
    verdict "$name" fail "$(r10_figures "$label" "$derived")" "$want" "the store count could not be read from the control row"
  elif [ "$pack" -gt "$bound" ]; then
    verdict "$name" fail "$(r10_figures "$label" "$derived")" "$want" "the pack figure is over its bound"
  elif [ "$derived" -gt "$((bound + derived_slack_kib))" ]; then
    verdict "$name" fail "$(r10_figures "$label" "$derived")" "$want" "the derived figure is over its bound"
  else
    verdict "$name" pass "$(r10_figures "$label" "$derived")" "$want"
  fi
}

# r10_output <name> <label> <control label> <same_output's status>: the released row's stdout is
# the control row's.
r10_output() {
  local name=$1 label=$2 control=$3 same=$4 want
  want="stdout the same bytes as $control's"
  case $same in
    0) verdict "$name" pass "the same $(wc -c < "$out/$label.out") bytes" "$want" ;;
    1) verdict "$name" fail "$(cmp "$out/$label.out" "$out/$control.out" 2>&1 | head -n 1)" "$want" ;;
    *) verdict "$name" fail "no result for $(has_result "$label" && echo "$control" || echo "$label")" "$want" "a row did not run" ;;
  esac
}

# fetch_jj <copy> <url> <asset sha256> <binary sha256>: a release's musl jj at
# $scratch/<copy>-jj/jj, the asset and its ./jj member each checked by sha256; fetched names the
# binary when both hold.
fetched=""
fetch_jj() {
  local copy=$1 url=$2 asset_sha256=$3 binary_sha256=$4 dir=$scratch/$1-jj asset_got binary_got
  fetched=""
  mkdir -p "$dir"
  asset_got=$( (curl -fsSL --retry 3 -o "$dir/jj.tar.gz" "$url" && sha256sum "$dir/jj.tar.gz" | cut -d' ' -f1) 2>&1)
  same "setup $copy jj asset" "$asset_got" "$asset_sha256"
  [ "$asset_got" = "$asset_sha256" ] || return 0
  # Python's tarfile, not GNU tar: a tar built on glibc 2.43 or later calls openat2, which gVisor
  # does not implement.
  binary_got=$(python3 - "$dir/jj.tar.gz" "$dir/jj" 2>&1 << 'PY'
import hashlib, sys, tarfile
with tarfile.open(sys.argv[1]) as archive:
    data = archive.extractfile(archive.getmember("./jj")).read()
with open(sys.argv[2], "wb") as f:
    f.write(data)
print(hashlib.sha256(data).hexdigest())
PY
)
  same "setup $copy jj binary" "$binary_got" "$binary_sha256"
  if [ "$binary_got" = "$binary_sha256" ] && chmod 0755 "$dir/jj"; then
    fetched=$dir/jj
  fi
}

cache_pid=""
daemon_pid=""
# The sampler's option that samples the git daemon's tree beside a row the daemon serves, set once
# the daemon answers.
daemon_tree=()
stop_background() {
  local pid
  for pid in "$cache_pid" "$daemon_pid"; do
    [ -n "$pid" ] || continue
    kill -TERM -- "-$pid" 2> /dev/null || kill -TERM "$pid" 2> /dev/null
    wait "$pid" 2> /dev/null
  done
  cache_pid=""
  daemon_pid=""
}

finish() {
  stop_background
  echo
  echo "======== the rows"
  if [ -s "$results" ]; then
    printf 'label\texit\twall_s\tgnu_time_kib\tsampled_kib\tpack_kib\tanonymous_kib\tpack_mappings\tpack_files\tmax_pack_kib\tmax_other_file_kib\tmax_anonymous_kib\tlargest_pack_file_mappings\tcgroup_peak_less_baseline_kib\tdaemon_tree_peak_kib\tsamples_stopped\tstop_ms_total\tstop_ms_median\tstop_ms_max\tsamples_not_stopped_in_time\n'
    jq -r '[.label, .exit, .wall_s, .maxrss_kib, .sampled_total_kib, .sampled_pack_kib, .sampled_anon_kib, .pack_mappings, .pack_files, .max_pack_kib, .max_other_file_kib, .max_anon_kib, .largest_pack_file_mappings, .cgroup_peak_less_baseline_kib, .daemon_tree_peak_kib, .samples_stopped, .stop_ms_total, .stop_ms_median, .stop_ms_max, .samples_not_stopped_in_time] | @tsv' "$results"
  else
    echo "no row was measured"
  fi
  echo
  echo "======== the verdicts no row reached"
  local name missing=0
  for name in "${expected[@]}"; do
    if [ -z "${decided[$name]:-}" ]; then
      missing=$((missing + 1))
      verdict "$name" fail "no result" "a result" "${stopped:-the step that decides it did not run}"
    fi
  done
  [ "$missing" -gt 0 ] || echo "none"
  echo
  echo "======== summary"
  echo "repository: $repo"
  echo "image: ${PACK_MEMORY_IMAGE:-unnamed}"
  echo "verdicts: ${#expected[@]}; held: $passes; void: $voids; mismatched: $fails"
  [ -z "$stopped" ] || echo "stopped: $stopped"
  if [ "$fails" -eq 0 ]; then
    echo "JOB-RESULT pass"
  else
    echo "JOB-RESULT fail"
  fi
  exit 0
}

# --------------------------------------------------------------------------------------------
# Arguments.
# --------------------------------------------------------------------------------------------
bad_arguments=""
while [ $# -gt 0 ]; do
  case "$1" in
    --repo | --walk-file)
      if [ $# -lt 2 ]; then
        bad_arguments="$1 takes a value"
        break
      fi
      if [ "$1" = --repo ]; then repo=$2; else walk_file=$2; fi
      shift 2
      ;;
    *)
      bad_arguments="unknown argument $1"
      break
      ;;
  esac
done
if [ -z "$bad_arguments" ] && ! [[ $repo =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
  bad_arguments="--repo $repo is no <owner>/<name>"
fi
if [ -z "$bad_arguments" ]; then
  case "/$walk_file/" in
    //* | */../* | */./* | *//*) bad_arguments="--walk-file $walk_file is no relative path inside the repository" ;;
  esac
fi
owner=${repo%%/*}
name=${repo#*/}

echo "== the run"
echo "repository: $repo"
echo "walk file: $walk_file"
echo "second edit: $second_file"
echo "the file of the commit before adoptionArgs: $third_file"
echo "image: ${PACK_MEMORY_IMAGE:-unnamed}"
echo "image revision label: ${PACK_MEMORY_IMAGE_REVISION:-unread}"
echo "uname: $(uname -a)"
echo "id: $(id)"
echo "processors: $(nproc)"
echo "memory: $(grep MemTotal /proc/meminfo); cgroup limit $(cat /sys/fs/cgroup/memory.max 2> /dev/null || echo unread)"
if [ -n "$bad_arguments" ]; then
  verdict "setup arguments" fail "$bad_arguments" "--repo <owner>/<name> and --walk-file <relative path>"
  stop "the arguments: $bad_arguments"
  finish
fi
verdict "setup arguments" pass "--repo $repo --walk-file $walk_file" "--repo <owner>/<name> and --walk-file <relative path>"

# The run's own environment, all of it on the Job's volume. JJ_PACK_MAPPED_LIMIT is unset so jj
# runs with the budget it builds in, as a role's does.
export HOME=$scratch/home
export XDG_CONFIG_HOME=$scratch/config
export TMPDIR=$scratch/tmp
export GIT_AUTHOR_NAME='Pack Memory Probe' GIT_COMMITTER_NAME='Pack Memory Probe'
export GIT_AUTHOR_EMAIL=probe@example.invalid GIT_COMMITTER_EMAIL=probe@example.invalid
export JJ_USER='Pack Memory Probe' JJ_EMAIL=probe@example.invalid
unset JJ_PACK_MAPPED_LIMIT JJ_TIMESTAMP JJ_OP_TIMESTAMP JJ_RANDOMNESS_SEED
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$TMPDIR" "$out" "$scratch/logs" || {
  stop "the run's directories on $scratch could not be made"
  finish
}
: > "$results"

# --------------------------------------------------------------------------------------------
# 1. GNU time, and the two control jj.
# --------------------------------------------------------------------------------------------
echo
say "== GNU time 1.9, from Debian trixie's package"
time_deb=$scratch/time_1.9-0.2_amd64.deb
time_got=$( (curl -fsSL --retry 3 -o "$time_deb" "$time_url" && sha256sum "$time_deb" | cut -d' ' -f1) 2>&1)
if [ "$time_got" = "$time_sha256" ] && dpkg-deb -x "$time_deb" "$scratch/time-package" && [ -x "$scratch/time-package/usr/bin/time" ]; then
  time_bin=$scratch/time-package/usr/bin/time
  # Debian's build prints `time (GNU Time) UNKNOWN` for --version, so the package says which it is.
  version=$(dpkg-deb -f "$time_deb" Package Version 2>&1 | tr '\n' ' ')
  same "setup GNU time" "sha256 $time_got, ${version% }" "sha256 $time_sha256, Package: time Version: 1.9-0.2"
else
  verdict "setup GNU time" fail "$time_got" "sha256 $time_sha256, extracted"
fi
[ -n "$time_bin" ] || stop "no GNU time: no row can be measured"

echo
say "== jj $control_version, R3 to R5's control, from the fork's release"
fetch_jj control "$control_url" "$control_asset_sha256" "$control_binary_sha256"
control_jj=$fetched

echo
say "== jj $r10_control_version, R10's control, from the fork's release"
fetch_jj r10-control "$r10_control_url" "$r10_control_asset_sha256" "$r10_control_binary_sha256"
r10_control_jj=""
if [ -n "$fetched" ]; then
  printed=$("$fetched" --version 2>&1)
  same "setup r10-control jj version" "$printed" "$r10_control_prints"
  [ "$printed" != "$r10_control_prints" ] || r10_control_jj=$fetched
fi

# --------------------------------------------------------------------------------------------
# 2. The volume, the image and the versions, and the image reads.
# --------------------------------------------------------------------------------------------
echo
say "== the volume, the image and the versions"
echo "the volume: $(awk -v at="$scratch" '$2 == at' /proc/mounts)"
df -B1 "$scratch" | tail -n 1
echo "image: ${PACK_MEMORY_IMAGE:-unnamed} (org.opencontainers.image.revision: ${PACK_MEMORY_IMAGE_REVISION:-unread})"
echo "legion version: $(legion version 2>&1)"
echo "git: $(command -v git) -> $(/usr/bin/git --version)"
image_jj=$(command -v jj)
image_jj_version=$("$image_jj" --version 2>&1)
echo "the image's jj: $image_jj -> $image_jj_version"
echo "R3 to R5's control jj: ${control_jj:-none} -> $([ -n "$control_jj" ] && "$control_jj" --version 2>&1)"
echo "R10's control jj: ${r10_control_jj:-none} -> $([ -n "$r10_control_jj" ] && "$r10_control_jj" --version 2>&1)"
echo "JJ_PACK_MAPPED_LIMIT: ${JJ_PACK_MAPPED_LIMIT-unset}"
echo "the role's git block:"
env | grep '^GIT_CONFIG_' | sort -V | sed 's/^/  /'

# R10's released jj is the binary the image's jj wrapper runs, as the wrapper's own line names it.
if [ -x "$released_jj" ] && grep -qxF "real=$released_jj" "$image_jj" 2> /dev/null; then
  verdict "setup r10 released jj" pass "$image_jj runs $released_jj: $("$released_jj" --version 2>&1)" "the binary the image's jj wrapper runs"
else
  verdict "setup r10 released jj" fail "$image_jj names $(grep -m 1 '^real=' "$image_jj" 2>&1 || echo 'no real= line'); $released_jj is $([ -x "$released_jj" ] || echo 'not ')executable" \
    "the binary the image's jj wrapper runs, $released_jj"
  released_jj=""
fi
copy_jj[control]=$control_jj
copy_jj[r10-control]=$r10_control_jj
for budget in "${budgets[@]}"; do
  copy_jj[r10-$budget]=$released_jj
done

echo
say "== the image reads: git config --system --get of the four pack settings, and the image's jj --version"
for i in "${!setting_keys[@]}"; do
  same "c-pack-window-config system ${setting_keys[$i]}" "$(git config --system --get "${setting_keys[$i]}" 2>&1)" "${setting_values[$i]}"
done
tool_version=${PACK_MEMORY_JJ_TOOL_VERSION:-}
want="jj $tool_version-<commit>, the release worker.Dockerfile's ARG JJ_TOOL names"
if [ -z "$tool_version" ]; then
  verdict "c-release-source image jj version" fail "$image_jj_version" "$want" "the harness passed no PACK_MEMORY_JJ_TOOL_VERSION"
else
  case "$image_jj_version" in
    "jj $tool_version" | "jj $tool_version-"*) verdict "c-release-source image jj version" pass "$image_jj_version" "$want" ;;
    *) verdict "c-release-source image jj version" fail "$image_jj_version" "$want" ;;
  esac
fi

# --------------------------------------------------------------------------------------------
# 3. The cache, fetch and provision of the image's own code, and the git daemon the pushes go to.
# --------------------------------------------------------------------------------------------
daemon_dir=$scratch/daemon
github_url=https://github.com/$repo

root=$scratch/legion
feed=$scratch/feed
feed_repo=$feed/$owner/$name.git
clone=$root/repos/github.com/$owner/$name
ws=$root/workspaces/$owner/$name/${issue,,}
default_branch=""
if going; then
  echo
  say "== legion workspace-init cache --repo $repo"
  secrets=$scratch/secrets
  mkdir -p "$secrets" "$scratch/cache"
  chmod 0700 "$secrets"
  # The provisioning token is a throwaway: a public repository's clone and fetch never send it.
  # The bearer the cache checks on /tar is a random one the fetch below presents.
  od -An -N16 -tx1 /dev/urandom | tr -d ' \n' > "$secrets/provision-token"
  od -An -N16 -tx1 /dev/urandom | tr -d ' \n' > "$secrets/cache-token"
  chmod 0600 "$secrets/provision-token" "$secrets/cache-token"
  started=$SECONDS
  # --refresh is a day so no refresh of its own runs while the fetch below reads it; the cache is
  # stopped once the fetch is done.
  LEGION_PROVISION_TOKEN_FILE=$secrets/provision-token LEGION_REPOSITORY_CACHE_TOKEN_FILE=$secrets/cache-token \
    setsid legion workspace-init cache --repo "$repo" --root "$scratch/cache" --listen "$cache_listen" --refresh 24h \
    > "$scratch/logs/cache.log" 2>&1 < /dev/null &
  cache_pid=$!
  health=""
  while :; do
    if health=$(curl -fsS --max-time 10 "http://$cache_listen/healthz" 2> /dev/null) &&
      [ "$(jq -r .ready <<< "$health" 2> /dev/null)" = true ]; then
      break
    fi
    health=""
    if ! kill -0 "$cache_pid" 2> /dev/null; then
      echo "the cache's process ended before it answered ready"
      break
    fi
    if [ $((SECONDS - started)) -ge "$row_timeout" ]; then
      echo "the cache did not answer ready in $row_timeout s"
      break
    fi
    sleep 5
  done
  tail -n 20 "$scratch/logs/cache.log"
  if [ -n "$health" ]; then
    verdict "setup cache ready" pass "ready after $((SECONDS - started)) s: $health" "/healthz answers ready"
  else
    verdict "setup cache ready" fail "no ready answer" "/healthz answers ready"
    stop "the repository cache never answered ready"
  fi
fi

if going; then
  echo
  say "== legion workspace-init fetch --repo $repo --feed $feed --cache-url http://$cache_listen"
  started=$SECONDS
  LEGION_PROVISION_TOKEN_FILE=$secrets/provision-token LEGION_REPOSITORY_CACHE_TOKEN_FILE=$secrets/cache-token \
    legion workspace-init fetch --repo "$repo" --feed "$feed" --cache-url "http://$cache_listen" \
    > "$scratch/logs/fetch.log" 2>&1 < /dev/null
  status=$?
  cat "$scratch/logs/fetch.log"
  echo "exit $status in $((SECONDS - started)) s"
  # The feed must have come from the cache, as an issue pod's does, and not from GitHub alone.
  from_cache=$(grep -c 'from the repository cache' "$scratch/logs/fetch.log")
  if [ "$status" -eq 0 ] && [ "$from_cache" -gt 0 ]; then
    verdict "setup fetch" pass "exit 0, filled from the repository cache" "exit 0, filled from the repository cache"
  else
    verdict "setup fetch" fail "exit $status, $from_cache lines naming the repository cache" "exit 0, filled from the repository cache"
    stop "workspace-init fetch did not fill the feed from the cache"
  fi
  # The cache has done its part: stopped, it holds no memory of the pod's while the rows run.
  stop_background
fi

if going; then
  echo
  say "== the git daemon, outside every measured tree, serving a bare copy of the feed to the push rows"
  default_branch=$(setup_git --git-dir="$feed_repo" symbolic-ref --short HEAD 2>&1)
  echo "the default branch, as the feed's HEAD names it: $default_branch"
  daemon_ok=0
  mkdir -p "$daemon_dir"
  if setup_git clone --bare --local --quiet "$feed_repo" "$daemon_dir/feed.git" > "$scratch/logs/feed-copy.log" 2>&1; then
    setsid env -u GIT_CONFIG_COUNT /usr/bin/git daemon --export-all --enable=receive-pack --informative-errors \
      --reuseaddr --listen=127.0.0.1 --port="$daemon_port" --base-path="$daemon_dir" \
      > "$scratch/logs/git-daemon.log" 2>&1 < /dev/null &
    daemon_pid=$!
    for _ in $(seq 1 60); do
      if setup_git ls-remote "git://127.0.0.1:$daemon_port/feed.git" HEAD > /dev/null 2>&1; then
        daemon_ok=1
        daemon_tree=(--daemon "$daemon_pid")
        break
      fi
      sleep 1
    done
  else
    cat "$scratch/logs/feed-copy.log"
  fi
  served=$(setup_git ls-remote "git://127.0.0.1:$daemon_port/feed.git" HEAD 2>&1 | head -n 1)
  if [ "$daemon_ok" -eq 1 ]; then
    verdict "setup git daemon" pass "serving the feed's HEAD: $served" "the daemon serves the feed's copy"
  else
    verdict "setup git daemon" fail "${served:-nothing}" "the daemon serves the feed's copy"
    tail -n 20 "$scratch/logs/git-daemon.log"
  fi
fi

if going; then
  echo
  say "== legion workspace-init provision, its jj commands through the measuring jj"
  # Provisioning resolves jj from PATH once, at its start (provisioningTools), and passes that path
  # to every jj command it runs, so this jj, first on PATH for provisioning alone, is the one it
  # runs: each command runs the image's jj under the sampler, keeps its own stdin, stdout and
  # stderr for provisioning to read, and appends its RESULT line to the results.
  mkdir -p "$here/bin" "$root"
  {
    echo '#!/usr/bin/env bash'
    echo '# The measuring jj of pack-memory-probe.sh: the image'"'"'s jj under the sampler.'
    printf 'image_jj=%q\npeak=%q\nout=%q\nresults=%q\ntime_bin=%q\ncounter=%q\ntimeout=%q\n' \
      "$image_jj" "$peak" "$out" "$results" "$time_bin" "$here/provision-count" "$row_timeout"
    cat << 'SH'
n=$(($(cat "$counter" 2> /dev/null || echo 0) + 1))
echo "$n" > "$counter"
# The label names the subcommand: the first two words that are no option or an option's value.
words=""
count=0
skip=0
for arg; do
  if [ "$skip" -eq 1 ]; then
    skip=0
    continue
  fi
  case "$arg" in
    -R | --repository | --config | --config-file | --at-operation | --at-op | --color | \
      -r | --revision | --revisions | -m | --message | -T | --template | --name | --sparse-patterns) skip=1 ;;
    -*) ;;
    *[!a-z-]*) break ;;
    *)
      words="$words-$arg"
      count=$((count + 1))
      [ "$count" -lt 2 ] || break
      ;;
  esac
done
label=$(printf 'provision-%02d-jj%s' "$n" "$words")
exec python3 "$peak" --label "$label" --time-bin "$time_bin" --out-dir "$out" --results "$results" \
  --timeout "$timeout" --inherit -- "$image_jj" "$@"
SH
  } > "$here/bin/jj"
  chmod 0755 "$here/bin/jj"
  before=$(wc -l < "$results")
  started=$SECONDS
  (cd "$root" && PATH=$here/bin:$PATH legion workspace-init provision --issue "$issue" --repo "$repo" \
    --default-branch "$default_branch" --root "$root" --credential-helper '!legion git-credential' --feed "$feed") \
    > "$scratch/logs/provision.log" 2>&1 < /dev/null
  status=$?
  cat "$scratch/logs/provision.log"
  echo "exit $status in $((SECONDS - started)) s"
  tail -n +"$((before + 1))" "$results" | sed 's/^/RESULT /'
  provisioned=$(wc -l < "$results")
  if [ "$status" -eq 0 ] && [ -d "$ws" ] && [ "$provisioned" -gt "$before" ]; then
    verdict "setup provision" pass "exit 0, $ws, $((provisioned - before)) jj commands measured" "exit 0, the workspace, its jj commands measured"
  else
    verdict "setup provision" fail "exit $status, $((provisioned - before)) jj commands measured" "exit 0, the workspace, its jj commands measured"
    stop "workspace-init provision made no workspace"
  fi
fi

if going; then
  echo
  say "== the workspace"
  if common_dir_matches "setup image workspace's git common dir" "$ws" "$clone"; then
    echo "files: $(setup_git -C "$ws" ls-files | wc -l); HEAD $(setup_git -C "$ws" rev-parse HEAD); .git: $(cat "$ws/.git")"
    echo "the clone's packs:"
    find "$clone/.git/objects/pack" -maxdepth 1 -type f -printf '  %s %f\n' | sort -k2
    tracked=$(setup_git -C "$ws" ls-files --error-unmatch -- "$walk_file" 2>&1)
    same "setup walk file" "$tracked" "$walk_file"
    # A role pushes to origin, so the leak guard reads only what origin's remote-tracking refs do
    # not hold. The push rows go there too, with origin's push URL the daemon's copy of the feed.
    setup_git --git-dir="$clone/.git" config remote.origin.pushurl "git://127.0.0.1:$daemon_port/feed.git"
  else
    stop "the workspace's git common dir is not its own clone's .git"
  fi
fi

# The pushes run the image's wrapper, its pre-push hook and the leak guard, as a role's do. A Job
# runs no launcher, so the daemon pushes it no copy of the shared public leak check: this stand-in,
# which clears every call, is what PUBLIC_LEAK_CHECK names, as leak-guard-image-probe.sh writes it.
mkdir -p "$scratch/leak-check"
cat > "$scratch/leak-check/public-leak-check.sh" << 'SH'
#!/usr/bin/env bash
# Clears every call: the push rows measure memory, and the rule pass still reads each push.
exit 0
SH
chmod 0755 "$scratch/leak-check/public-leak-check.sh"
export PUBLIC_LEAK_CHECK=$scratch/leak-check/public-leak-check.sh
# Nor does the daemon hand it the deployment's private-name rule file, and a guard with no rule file
# runs no rule pass, so a push would skip work a role's push does. This file of one rule, for a
# host no push here carries, is what LEGION_PRIVATE_NAME_RULES_FILE names, so the rule pass reads
# each push and refuses none.
mkdir -p "$scratch/rules"
cat > "$scratch/rules/private-name-rules.toml" << 'TOML'
title = "the pack-memory probe's own rules"

[[rules]]
id = "pack_memory_probe_rule"
description = "a host no push of this probe carries"
regex = '''never-pushed-host[.]pack-memory[.]invalid'''
TOML
export LEGION_PRIVATE_NAME_RULES_FILE=$scratch/rules/private-name-rules.toml

# --------------------------------------------------------------------------------------------
# 4. The four pack settings under the role's block.
# --------------------------------------------------------------------------------------------
if going; then
  echo
  say "== the four pack settings, in the role's environment"
  cd "$ws" || stop "cd $ws"
  for i in "${!setting_keys[@]}"; do
    key=${setting_keys[$i]}
    value=${setting_values[$i]}
    scoped=$(git config --show-scope --get "$key" 2>&1 | tr '\t' ' ')
    clone_local=$(git --git-dir="$clone/.git" config --local --get "$key" 2>&1)
    same "c-git-rows role scope $key" "$scoped" "command $value"
    same "c-git-rows clone local $key" "$clone_local" "$value"
  done
fi

# --------------------------------------------------------------------------------------------
# 5. The git rows: git log, git status and the push on the original workspace, and the fetch into a
# clone that lacks the newer commits of the default branch, each with the role's settings and with
# git's defaults forced.
# --------------------------------------------------------------------------------------------
forced=(--unset GIT_CONFIG_COUNT --env GIT_CONFIG_NOSYSTEM=1)
# The figure each git row is judged by: the pod's charge, but for the push, whose receiving daemon
# shares the pod, its own process tree.
charge_field=.cgroup_peak_less_baseline_kib
charge_what="the pod's charge"
tree_field=.sampled_total_kib
tree_what="the push's own process tree (its largest sampled total)"

# fetch_estimate <tip> <k>: <tip>~k, and the commits, the objects and the bytes on the clone's disk
# of <tip>~k..<tip>, on one line; nothing when <tip>~k does not exist. git's defaults make it fast.
fetch_estimate() {
  local tip=$1 k=$2 base
  base=$(setup_git --git-dir="$clone/.git" rev-parse --verify --quiet "$tip~$k^{commit}") || return 0
  printf '%s %s %s\n' "$base" "$(setup_git --git-dir="$clone/.git" rev-list --count "$base..$tip")" \
    "$(setup_git "${defaults[@]}" --git-dir="$clone/.git" rev-list --objects --no-object-names "$base..$tip" |
      setup_git "${defaults[@]}" --git-dir="$clone/.git" cat-file --batch-check='%(objectsize:disk)' |
      awk '/missing/ { next } { n++; s += $1 } END { print n + 0, s + 0 }')"
}

pack_objects() { # pack_objects <pack>: the object count its header holds (bytes 8 to 11)
  od -An -tu1 -j8 -N4 "$1" | awk '{ print $1 * 16777216 + $2 * 65536 + $3 * 256 + $4 }'
}

# fetch_rows: c-git-rows' fetch rows. K is the smallest step of fetch_ladder for which the newer
# objects of the default branch, <tip>~K..<tip> on the workspace's clone, take fetch_floor on its
# disk. A bare repository holds that branch at <tip>~K alone, fetched by id from GitHub in one
# pack, and lacks <tip>; each row fetches the branch from GitHub into a fresh `git clone --local
# --no-checkout` of it. A row whose new pack is under fetch_floor is void, and so are both rows,
# without running, when no step of the ladder reaches it.
fetch_rows() {
  local tip k line base="" commits objects bytes chosen="" lack=$scratch/lacking.git arm row
  local before new pack status held absent got want reason void lack_pack branch=$default_branch
  local -a tried=()
  local -A received=() received_objects=()
  echo
  say "== the fetch rows: the newer commits of $branch from $github_url, into a clone that lacks them"
  tip=$(setup_git --git-dir="$clone/.git" rev-parse --verify --quiet "refs/remotes/origin/$branch^{commit}")
  want="a bare repository holding $branch at <tip>~K alone, fetched by id from GitHub, in one pack, <tip> absent"
  if [ -z "$tip" ]; then
    verdict "setup fetch lacking clone" fail "the workspace's clone has no refs/remotes/origin/$branch" "$want"
    return
  fi
  echo "the default branch's tip in the workspace's clone: $tip"
  for k in "${fetch_ladder[@]}"; do
    line=$(fetch_estimate "$tip" "$k")
    if [ -z "$line" ]; then
      echo "K=$k: $tip~$k does not exist"
      tried+=("K=$k: no such commit")
      continue
    fi
    read -r base commits objects bytes <<< "$line"
    echo "K=$k: $tip~$k is $base; $commits commits, $objects objects, $(group "$bytes") bytes on the clone's disk"
    tried+=("K=$k: $(group "$bytes") bytes")
    if [ "$bytes" -ge "$fetch_floor" ]; then
      chosen=$k
      break
    fi
  done
  if [ -z "$chosen" ]; then
    reason="no step of the ladder has newer objects taking $(group "$fetch_floor") bytes on the clone's disk ($(printf '%s; ' "${tried[@]}" | sed 's/; $//'))"
    echo "$reason, so no fetch row runs"
    verdict "setup fetch lacking clone" void "no repository was made" "$want" "$reason"
    verdict "c-git-rows fetch settings" void "no fetch row ran" "exit 0, $charge_what under $(group "$bound_kib") KiB" "$reason"
    verdict "c-git-rows fetch defaults" void "no fetch row ran" "$charge_what at or over $(group "$bound_kib") KiB (2 GiB)" "$reason"
    return
  fi
  echo "K CHOSEN: $chosen; $tip~$chosen is $base; the estimate: $commits commits, $objects objects, $(group "$bytes") bytes"
  rm -rf "$lack"
  started=$SECONDS
  setup_git init --bare --quiet "$lack" && setup_git --git-dir="$lack" symbolic-ref HEAD "refs/heads/$branch" &&
    setup_git -c fetch.unpackLimit=1 --git-dir="$lack" fetch --no-tags --quiet "$github_url" "$base:refs/heads/$branch" \
      > "$scratch/logs/lacking-fetch.log" 2>&1
  status=$?
  echo "git fetch --no-tags $github_url $base:refs/heads/$branch into a new bare repository: exit $status in $((SECONDS - started)) s"
  tail -n 5 "$scratch/logs/lacking-fetch.log"
  held=$(setup_git --git-dir="$lack" rev-parse --verify --quiet "refs/heads/$branch")
  lack_pack=$(find "$lack/objects/pack" -maxdepth 1 -name '*.pack' 2> /dev/null)
  absent=present
  setup_git --git-dir="$lack" cat-file -e "$tip" 2> /dev/null || absent=absent
  got="exit $status; $branch at ${held:-nothing}; $(grep -c . <<< "$lack_pack") packs; $tip $absent"
  want="exit 0; $branch at $base; 1 packs; $tip absent"
  same "setup fetch lacking clone" "$got" "$want"
  if [ "$got" != "$want" ]; then
    rm -rf "$lack"
    return
  fi
  echo "its pack: $(group "$(stat -c %s "$lack_pack")") bytes, $(group "$(pack_objects "$lack_pack")") objects"
  for arm in settings defaults; do
    row=$scratch/fetch-$arm
    rm -rf "$row"
    if ! setup_git clone --local --no-checkout --quiet "$lack" "$row" > "$scratch/logs/fetch-$arm-copy.log" 2>&1; then
      cat "$scratch/logs/fetch-$arm-copy.log"
      continue
    fi
    before=$(find "$row/.git/objects/pack" -maxdepth 1 -name '*.pack' | sort)
    echo "the $arm row, GitHub's $branch now: $(setup_git ls-remote "$github_url" "refs/heads/$branch" 2>&1)"
    if [ "$arm" = settings ]; then
      (cd "$row" && measure git-fetch-settings -- git fetch --no-tags "$github_url" "+refs/heads/$branch:refs/remotes/origin/$branch")
    else
      (cd "$row" && measure git-fetch-defaults "${forced[@]}" -- git "${defaults[@]}" fetch --no-tags "$github_url" "+refs/heads/$branch:refs/remotes/origin/$branch")
    fi
    new=$(comm -13 <(echo "$before") <(find "$row/.git/objects/pack" -maxdepth 1 -name '*.pack' | sort))
    received[$arm]=0
    received_objects[$arm]=0
    for pack in $new; do
      received[$arm]=$((received[$arm] + $(stat -c %s "$pack")))
      received_objects[$arm]=$((received_objects[$arm] + $(pack_objects "$pack")))
    done
    echo "the $arm fetch: K $chosen, estimate $(group "$bytes") bytes; its new pack $(group "${received[$arm]}") bytes, $(group "${received_objects[$arm]}") objects (${new:-no pack});" \
      "$(setup_git --git-dir="$row/.git" rev-list --count "$base..refs/remotes/origin/$branch" 2>&1) commits it brought"
    rm -rf "$row"
  done
  rm -rf "$lack"
  for arm in settings defaults; do
    void=""
    if [ -n "${received[$arm]:-}" ] && [ "${received[$arm]}" -lt "$fetch_floor" ]; then
      void="its new pack was $(group "${received[$arm]}") bytes, under the $(group "$fetch_floor") the row needs to exercise index-pack"
    fi
    if [ "$arm" = settings ]; then
      git_holds "c-git-rows fetch settings" git-fetch-settings "$charge_field" "$charge_what" "$void"
    else
      git_control "c-git-rows fetch defaults" git-fetch-defaults "$charge_field" "$charge_what" "$void"
    fi
  done
}

if going; then
  echo
  say "== the git rows: each with the role's settings, then with git's defaults forced"
  cd "$ws" || stop "cd $ws"
fi
if going; then
  measure git-log-settings -- git log --first-parent -- "$walk_file"
  measure git-log-defaults "${forced[@]}" -- git "${defaults[@]}" log --first-parent -- "$walk_file"
  git_holds "c-git-rows log settings" git-log-settings "$charge_field" "$charge_what"
  git_control "c-git-rows log defaults" git-log-defaults "$charge_field" "$charge_what"

  measure git-status-settings -- git status
  measure git-status-defaults "${forced[@]}" -- git "${defaults[@]}" status
  git_holds "c-git-rows status settings" git-status-settings "$charge_field" "$charge_what"
  git_control "c-git-rows status defaults" git-status-defaults "$charge_field" "$charge_what"

  if [ "${daemon_ok:-0}" -eq 1 ]; then
    for arm in settings defaults; do
      printf 'pack-memory probe: the push with %s\n' "$arm" >> "$ws/$walk_file"
      git add -- "$walk_file" && git commit --quiet -m "pack-memory probe: the push with $arm"
      if [ "$arm" = settings ]; then
        measure git-push-settings "${daemon_tree[@]}" -- git push origin "HEAD:refs/heads/pack-memory/push-settings"
      else
        measure git-push-defaults "${daemon_tree[@]}" "${forced[@]}" -- git "${defaults[@]}" push origin "HEAD:refs/heads/pack-memory/push-defaults"
      fi
    done
    git_holds "c-git-rows push settings" git-push-settings "$tree_field" "$tree_what"
    git_control "c-git-rows push defaults" git-push-defaults "$tree_field" "$tree_what"
  fi

  fetch_rows
fi

# --------------------------------------------------------------------------------------------
# 6. The copies.
# --------------------------------------------------------------------------------------------
control_clone=$(copy_clone control)
control_ws=$(copy_ws control)
export JJ_TIMESTAMP=$jj_timestamp JJ_OP_TIMESTAMP=$jj_timestamp
if going; then
  echo
  say "== the room for ${#copies[@]} copies of the provisioning root, and one fresh root at a time"
  cd "$scratch" || stop "cd $scratch"
  df -k "$scratch"
  root_kib=$(du -sk "$root" | cut -f1)
  packs_kib=$(du -sk "$clone/.git/objects/pack" | cut -f1)
  echo "du -s of the original root $root: $(group "$root_kib") KiB, of which its clone's packs, which every copy hardlinks: $(group "$packs_kib") KiB"
  available_kib=$(df -Pk "$scratch" | awk 'NR == 2 { print $4 }')
  want="room for ${#copies[@]} copies and one fresh root, each the original root less its packs"
  if whole "$root_kib" && whole "$packs_kib" && whole "$available_kib"; then
    need_kib=$(((${#copies[@]} + 1) * (root_kib - packs_kib)))
    if [ "$available_kib" -ge "$need_kib" ]; then
      verdict "setup room for the copies" pass "$(group "$available_kib") KiB available, $(group "$need_kib") KiB needed" "$want"
    else
      verdict "setup room for the copies" fail "$(group "$available_kib") KiB available, $(group "$need_kib") KiB needed" "$want"
      stop "the volume cannot hold the copies: $(group "$available_kib") KiB available, $(group "$need_kib") KiB needed"
    fi
  else
    verdict "setup room for the copies" fail "du or df unread: root [$root_kib], packs [$packs_kib], available [$available_kib]" "$want"
    stop "the room for the copies could not be read"
  fi
fi

if going; then
  pointer=$(cat "$ws/.git")
  entry=${pointer##*/}
  echo "the image workspace's .git: $pointer"
  echo "the original's packs:"
  stat -c '  inode %i, %h links: %n' "$clone/.git/objects/pack"/*
  for copy in "${copies[@]}"; do
    each_clone=$(copy_clone "$copy")
    each_ws=$(copy_ws "$copy")
    echo
    say "== the $copy copy of the provisioning root, its packs hardlinked"
    started=$SECONDS
    if copy_root "$scratch/$copy"; then
      echo "copied in $((SECONDS - started)) s; its packs:"
      stat -c '  inode %i, %h links: %n' "$each_clone/.git/objects/pack"/*
      printf 'gitdir: %s\n' "$each_clone/.git/worktrees/$entry" > "$each_ws/.git"
      echo "the $copy workspace's .git: $(cat "$each_ws/.git")"
      (cd "$each_ws" && setup_git worktree repair 2>&1)
    else
      echo "the copy to $scratch/$copy failed"
    fi
    if ! common_dir_matches "setup $copy workspace's git common dir" "$each_ws" "$each_clone"; then
      stop "the $copy workspace's git common dir is not its own clone's .git"
      break
    fi
  done
fi
if going; then
  # The original is read again once every copy is made: each must name its own clone before any
  # jj row.
  original=$(common_dir "$ws")
  echo "the image workspace's git common dir now: $original"
  [ "$original" = "$(realpath -m "$clone/.git")" ] || stop "the image workspace's git common dir is no longer its own clone's .git"
fi

# Each jj reads its copy's store before any row, so a row measures its command and not a store's
# first read by another build. jj 0.45.1 must read the store the image's jj wrote with no
# `Reindexing`, or the run stops; an R10 copy whose jj cannot read its store runs none of R10's
# role commands; its provisioning rows, on a fresh root, still run.
declare -A ready=()
if going; then
  echo
  say "== each jj reads its copy's store, the working copy untouched"
  echo "the image's jj on the original:"
  (cd "$ws" && JJ_RANDOMNESS_SEED=100 "$image_jj" --ignore-working-copy -R "$ws" op log -n1 2>&1)
  for copy in "${copies[@]}"; do
    jj=${copy_jj[$copy]}
    budget=${copy_budget[$copy]:-}
    each_ws=$(copy_ws "$copy")
    want="exit 0"
    [ "$copy" != control ] || want="exit 0, no Reindexing"
    if [ -z "$jj" ]; then
      verdict "setup $copy jj reads the copied store" fail "no jj for the $copy copy" "$want"
      [ "$copy" != control ] || { stop "no control jj"; break; }
      continue
    fi
    read_back=$(cd "$each_ws" && env JJ_RANDOMNESS_SEED=100 ${budget:+"JJ_PACK_MAPPED_LIMIT=$budget"} "$jj" --ignore-working-copy -R "$each_ws" op log -n1 2>&1)
    status=$?
    echo "$jj ($("$jj" --version 2>&1)) on the $copy copy, JJ_PACK_MAPPED_LIMIT ${budget:-unset}:"
    echo "$read_back"
    got="exit $status"
    ! grep -q Reindexing <<< "$read_back" || got="$got, Reindexing"
    if [ "$status" -eq 0 ] && { [ "$copy" != control ] || [ "$got" = "exit 0" ]; }; then
      verdict "setup $copy jj reads the copied store" pass "$got" "$want"
      ready[$copy]=1
    else
      verdict "setup $copy jj reads the copied store" fail "$got" "$want"
      if [ "$copy" = control ]; then
        stop "jj $control_version cannot read the store the image's jj wrote"
        break
      fi
    fi
  done
fi

# --------------------------------------------------------------------------------------------
# 7. R3 to R5's jj rows, on the original and on the 0.45.1 copy.
# --------------------------------------------------------------------------------------------
# The role commands steps 7 and 9 both run, as the code builds them (the harness holds each to the
# line that builds it): handoff-record.ts's STANDING read, which its pathJj runs as `jj -R
# <workspace> <STANDING>`, in the workspace; and the shim's adoption, the two commands
# packages/daemon/internal/shim/commands.go builds (describedArgs, then adoptionArgs), each ending
# in `-R <workspace>`, which the shim runs under the incoming role's identity, in the workspace.
standing_args=(log -r @- --no-graph -T commit_id)
described_args=(log -r '@ ~ description(exact:"")' --no-graph -T commit_id)
adoption_args=(metaedit --update-author -r '@ & description(exact:"")')

# edit_two_dirs <dir>: the jj rows' edits, to the walk file and to a file of another directory.
edit_two_dirs() {
  printf 'pack-memory probe: an edit the jj rows snapshot\n' >> "$1/$walk_file"
  mkdir -p "$(dirname "$1/$second_file")"
  printf 'pack-memory probe: an edit the jj rows snapshot\n' >> "$1/$second_file"
}

# fetch_feed_heads <copy> <clone>: the unmeasured fetch each copy's describedArgs follows. The setup
# git fetches, outside every measured tree, every branch head of the feed the clone was made from
# into the copy's shared clone, as refs of feed_remote, which the clone does not hold: it holds
# origin's already, at the same tips. So the jj command after it imports new refs at its start, as
# a role's jj does once git has fetched new branches into the shared clone. The clone holds the
# feed's objects, so the fetch should write refs and no object, and the probe prints what it wrote.
# Its setup verdict holds when it exits 0 and writes at least one ref the clone did not hold.
fetch_feed_heads() {
  local copy=$1 git_dir=$2/.git refs_before refs_after brought count packs_before new_packs loose_before loose_after status
  echo "the fetch of the feed's heads into the $copy clone, as refs/remotes/$feed_remote/*, unmeasured:"
  refs_before=$(setup_git --git-dir="$git_dir" for-each-ref --format='%(refname) %(objectname)' | sort)
  packs_before=$(find "$git_dir/objects/pack" -maxdepth 1 -name '*.pack' | sort)
  loose_before=$(setup_git --git-dir="$git_dir" count-objects -v 2>&1 | awk '$1 == "count:" { print $2 }')
  setup_git -c gc.auto=0 -c maintenance.auto=false --git-dir="$git_dir" fetch --no-tags --no-write-fetch-head --quiet \
    "$feed_repo" "+refs/heads/*:refs/remotes/$feed_remote/*" > "$scratch/logs/$copy-feed-heads.log" 2>&1
  status=$?
  cat "$scratch/logs/$copy-feed-heads.log"
  refs_after=$(setup_git --git-dir="$git_dir" for-each-ref --format='%(refname) %(objectname)' | sort)
  brought=$(comm -13 <(echo "$refs_before") <(echo "$refs_after"))
  count=$(grep -c . <<< "$brought")
  echo "exit $status; $count refs the clone did not hold:"
  [ -z "$brought" ] || printf '  %s\n' "${brought//$'\n'/$'\n'  }"
  new_packs=$(comm -13 <(echo "$packs_before") <(find "$git_dir/objects/pack" -maxdepth 1 -name '*.pack' | sort))
  loose_after=$(setup_git --git-dir="$git_dir" count-objects -v 2>&1 | awk '$1 == "count:" { print $2 }')
  if [ -z "$new_packs" ] && [ "$loose_after" = "$loose_before" ]; then
    echo "it wrote no pack and no loose object: the clone held their objects ($loose_after loose objects before and after)"
  else
    echo "it wrote objects: packs [${new_packs:-none}]; loose objects $loose_before before, $loose_after after"
  fi
  if [ "$status" -eq 0 ] && [ "$count" -gt 0 ]; then
    verdict "setup $copy fetch of the feed's heads" pass "exit 0, $count refs the clone did not hold" "exit 0, at least one ref the clone did not hold"
  else
    verdict "setup $copy fetch of the feed's heads" fail "exit $status, $count refs the clone did not hold" "exit 0, at least one ref the clone did not hold"
  fi
}

# commit_before <copy> <workspace> <command> <file>: the commit each copy's describedArgs, and then
# its adoptionArgs, follows, so each of the shim's two commands is the first jj command after a
# commit that moved HEAD. The setup git, outside every measured tree, appends one line to file and
# commits it as the handoff commit is made, `git add` and then `git commit`, with the probe's
# identity and both of git's dates fixed at jj_timestamp, so HEAD moves and the jj command after it
# imports the new HEAD and snapshots against its tree. git writes the commit's own trees loose, so
# the command reads from the pack only what the commit's diff reaches there: describedArgs reads
# the commits of the fetch's new refs, and adoptionArgs the old tree of third_file's directory,
# which no earlier commit of the copy changed (the walk file's tree is loose since the handoff
# commit, so a second commit of it would read nothing from the pack). Each commit is
# one id on every copy whose HEAD was one commit before it. Its setup verdict holds when the commit
# exits 0 and HEAD moved to a child of the HEAD before it; committed and commit_body keep its id and
# its text without the parent line, by copy and command, for the verdicts that compare the copies'
# commits.
declare -A committed=() commit_body=()
commit_before() {
  local copy=$1 dir=$2 command=$3 file=$4 before after parent status want
  want="exit 0, HEAD moved to a new commit whose parent is the HEAD before it"
  echo "the commit before $command in the $copy workspace, of $file, unmeasured:"
  before=$(setup_git -C "$dir" rev-parse HEAD 2>&1)
  mkdir -p "$(dirname "$dir/$file")"
  printf 'pack-memory probe: the commit %s imports\n' "$command" >> "$dir/$file"
  setup_git -C "$dir" add -- "$file" &&
    GIT_AUTHOR_DATE=$jj_timestamp GIT_COMMITTER_DATE=$jj_timestamp \
      setup_git -C "$dir" commit --quiet -m "pack-memory probe: the commit $command imports"
  status=$?
  after=$(setup_git -C "$dir" rev-parse HEAD 2>&1)
  parent=$(setup_git -C "$dir" rev-parse HEAD^ 2>&1)
  echo "exit $status; HEAD $before before, $after after: $(setup_git -C "$dir" log -1 --format='tree %T, author %an <%ae> %aI, committer %cn <%ce> %cI' 2>&1)"
  if [ "$status" -eq 0 ] && [ "$after" != "$before" ] && [ "$parent" = "$before" ]; then
    committed[$copy/$command]=$after
    commit_body[$copy/$command]=$(setup_git -C "$dir" cat-file commit HEAD | grep -v '^parent ')
    verdict "setup $copy commit before $command" pass "exit 0, HEAD moved from $before to $after, its child" "$want"
  else
    verdict "setup $copy commit before $command" fail "exit $status, HEAD $before before, $after after, whose parent is $parent" "$want"
  fi
}

# shim_rows <copy> <workspace> <jj> <clone> <seed> [sampler options…]: the shim's adoption on one
# copy, as steps 7 and 9 both run it: the fetch of the feed's heads, a commit, describedArgs, a
# second commit and adoptionArgs, each of the shim's two commands measured as the adopter, with
# the caller's sampler options first and JJ_RANDOMNESS_SEED <seed> for describedArgs and
# <seed> + 1 for adoptionArgs.
shim_rows() {
  local copy=$1 dir=$2 jj=$3 shared=$4 seed=$5
  local -a adopter=(--env "JJ_USER=$adopter_name" --env "JJ_EMAIL=$adopter_email")
  shift 5
  fetch_feed_heads "$copy" "$shared"
  commit_before "$copy" "$dir" describedArgs "$walk_file"
  measure "$copy-described" "$@" --env "JJ_RANDOMNESS_SEED=$seed" "${adopter[@]}" \
    -- "$jj" "${described_args[@]}" -R "$dir"
  commit_before "$copy" "$dir" adoptionArgs "$third_file"
  measure "$copy-adoption" "$@" --env "JJ_RANDOMNESS_SEED=$((seed + 1))" "${adopter[@]}" \
    -- "$jj" "${adoption_args[@]}" -R "$dir"
}

# pushed keeps the commit each copy pushed, and authored, by <copy>/before, <copy>/after and
# <copy>/operation, what the unmeasured adoptionArgs after the measured one showed.
declare -A pushed=() authored=()
jj_rows() { # jj_rows <copy> <workspace> <jj> <clone>
  local copy=$1 dir=$2 jj=$3 shared=$4
  echo
  say "== the jj rows on the $copy copy, with $jj ($("$jj" --version 2>&1))"
  cd "$dir" || {
    stop "cd $dir"
    return
  }
  edit_two_dirs "$dir"
  measure "$copy-jj-diff" --env JJ_RANDOMNESS_SEED=101 -- "$jj" -R "$dir" diff
  measure "$copy-jj-diff-name-only" --env JJ_RANDOMNESS_SEED=102 -- "$jj" -R "$dir" diff --name-only
  # The commit a role makes with git and pushes with git, which the STANDING read must answer.
  git add -- "$walk_file" "$second_file" && git commit --quiet -m "pack-memory probe: the $copy copy's commit"
  measure "$copy-git-push" "${daemon_tree[@]}" -- git push origin "HEAD:refs/heads/pack-memory/$copy"
  pushed[$copy]=$(git rev-parse HEAD)
  echo "pushed: ${pushed[$copy]}"
  # handoff-record.ts's STANDING read.
  measure "$copy-standing" --env JJ_RANDOMNESS_SEED=103 -- "$jj" -R "$dir" "${standing_args[@]}"
  # The shim's adoption: the working copy is undescribed, so the shim runs adoptionArgs and not
  # `jj new`. describedArgs runs right after the fetch and a commit, so it is the command that
  # imports the fetch's refs and the commit's HEAD, and adoptionArgs right after a second commit,
  # so it imports a new HEAD too: each of the two reads its tree from the pack.
  shim_rows "$copy" "$dir" "$jj" "$shared" 104
  # The author adoptionArgs sets, on an unmeasured run outside every measured tree: the measured
  # adoptionArgs's import of the new HEAD made the working-copy commit under the adopter's
  # identity, so its metaedit had nothing to change. `jj new` as the probe gives @ the probe's
  # author, and the same adoptionArgs argv as the adopter must make it the adopter's, in an
  # operation of metaedit's own.
  echo "the author adoptionArgs sets, unmeasured: jj new as the probe, then adoptionArgs as the adopter:"
  JJ_RANDOMNESS_SEED=106 "$jj" -R "$dir" new 2>&1
  authored[$copy/before]=$(JJ_RANDOMNESS_SEED=107 "$jj" --ignore-working-copy -R "$dir" log -r @ --no-graph -T 'author.email()' 2>&1)
  JJ_RANDOMNESS_SEED=108 JJ_USER=$adopter_name JJ_EMAIL=$adopter_email "$jj" "${adoption_args[@]}" -R "$dir" 2>&1
  echo "adoptionArgs exited $?"
  authored[$copy/after]=$(JJ_RANDOMNESS_SEED=109 "$jj" --ignore-working-copy -R "$dir" log -r @ --no-graph -T 'author.email()' 2>&1)
  authored[$copy/operation]=$(JJ_RANDOMNESS_SEED=110 "$jj" --ignore-working-copy -R "$dir" op log -n1 --no-graph -T description 2>&1)
  echo "@'s author ${authored[$copy/before]} before it and ${authored[$copy/after]} after it; the newest operation: ${authored[$copy/operation]}"
}

if going; then
  jj_rows image "$ws" "$image_jj" "$clone"
fi
if going; then
  jj_rows control "$control_ws" "$control_jj" "$control_clone"
fi

# The image and control copies' commits before each shim command: the one before describedArgs is
# made on its copy's own handoff commit, whose message names the copy and whose dates are the
# clock's, and the one before adoptionArgs on that copy's commit before describedArgs, so their
# ids differ, and everything else of them must be the same.
if going; then
  for command in describedArgs adoptionArgs; do
    case $command in
      describedArgs) want="one commit but for its parent, each copy's own handoff commit" ;;
      adoptionArgs) want="one commit but for its parent, each copy's own commit before describedArgs" ;;
    esac
    image_commit=${committed[image/$command]:-}
    control_commit=${committed[control/$command]:-}
    if [ -z "$image_commit" ] || [ -z "$control_commit" ]; then
      verdict "setup image and control commits before $command" fail "image ${image_commit:-none}, control ${control_commit:-none}" "$want" "a commit was not made"
    elif [ "${commit_body[image/$command]}" = "${commit_body[control/$command]}" ]; then
      verdict "setup image and control commits before $command" pass "image $image_commit and control $control_commit: the same tree, identity, dates and message" "$want"
    else
      verdict "setup image and control commits before $command" fail "image $image_commit, control $control_commit: they differ beyond their parents" "$want"
    fi
  done
fi

# --------------------------------------------------------------------------------------------
# 8. R3 to R5's verdicts.
# --------------------------------------------------------------------------------------------
if going; then
  echo
  say "== the jj verdicts"
  control_pack_kib=$(largest_pack_kib "$control_clone")
  echo "the control clone's pack: $(group "${control_pack_kib:-unread}") KiB"
  edited=$(printf '%s\n' "$walk_file" "$second_file" | sort)

  under_bound "c-jj-snapshot-memory diff image" image-jj-diff
  reaches_bound "c-jj-snapshot-memory diff control" control-jj-diff "${control_pack_kib:-unread}" "the pack's size"
  under_bound "c-jj-snapshot-memory name-only image" image-jj-diff-name-only
  reaches_bound "c-jj-snapshot-memory name-only control" control-jj-diff-name-only "${control_pack_kib:-unread}" "the pack's size"
  image_names=$(sort "$out/image-jj-diff-name-only.out" 2> /dev/null | tr '\n' ' ')
  control_names=$(sort "$out/control-jj-diff-name-only.out" 2> /dev/null | tr '\n' ' ')
  same "c-jj-snapshot-memory name-only outputs" "image: $image_names; control: $control_names" \
    "image: $(tr '\n' ' ' <<< "$edited"); control: $(tr '\n' ' ' <<< "$edited")"

  for copy in image control; do
    exits_zero "c-handoff-head-read-memory push $copy" "$copy-git-push" "git push of the $copy copy's commit"
    standing=$(cat "$out/$copy-standing.out" 2> /dev/null)
    if [[ ${pushed[$copy]:-} =~ ^[0-9a-f]{40}$ ]]; then
      same "c-handoff-head-read-memory standing $copy commit" "$standing" "${pushed[$copy]}"
    else
      verdict "c-handoff-head-read-memory standing $copy commit" fail "$standing" "the pushed commit's 40-character id" "no commit was pushed"
    fi
    undescribed=$(cat "$out/$copy-described.out" 2> /dev/null)
    same "c-shim-adoption-memory undescribed $copy" "describedArgs printed [$undescribed]" "describedArgs printed []"
    got="@'s author ${authored[$copy/before]:-unread} before the unmeasured adoptionArgs and ${authored[$copy/after]:-unread} after it; the newest operation: ${authored[$copy/operation]:-unread}"
    want="@'s author $JJ_EMAIL, the probe's, before the unmeasured adoptionArgs and $adopter_email after it; the newest operation: edit commit metadata for commit <id>, metaedit's"
    if [ "${authored[$copy/before]:-}" = "$JJ_EMAIL" ] && [ "${authored[$copy/after]:-}" = "$adopter_email" ] &&
      [[ ${authored[$copy/operation]:-} == "edit commit metadata for commit "* ]]; then
      verdict "c-shim-adoption-memory author $copy" pass "$got" "$want"
    else
      verdict "c-shim-adoption-memory author $copy" fail "$got" "$want"
    fi
  done
  under_bound "c-handoff-head-read-memory standing image" image-standing
  reaches_bound "c-handoff-head-read-memory standing control" control-standing "${control_pack_kib:-unread}" "the pack's size"
  under_bound "c-shim-adoption-memory described image" image-described
  under_bound "c-shim-adoption-memory adoption image" image-adoption
  reaches_bound "c-shim-adoption-memory described control" control-described "${control_pack_kib:-unread}" "the pack's size"
  reaches_bound "c-shim-adoption-memory adoption control" control-adoption "${control_pack_kib:-unread}" "the pack's size"
fi

# --------------------------------------------------------------------------------------------
# 9. R10's role commands, on each R10 copy with its jj and its budget.
# --------------------------------------------------------------------------------------------
r10_rows() { # r10_rows <copy>
  local copy=$1 dir jj
  local -a limit=()
  dir=$(copy_ws "$copy")
  jj=${copy_jj[$copy]}
  [ -z "${copy_budget[$copy]:-}" ] || limit=(--env "JJ_PACK_MAPPED_LIMIT=${copy_budget[$copy]}")
  echo
  say "== R10's role commands on the $copy copy, with $jj ($("$jj" --version 2>&1)), JJ_PACK_MAPPED_LIMIT ${copy_budget[$copy]:-unset}"
  cd "$dir" || {
    echo "cd $dir failed, so the $copy copy runs no role command"
    return
  }
  measure "$copy-file-list" "${limit[@]}" --env JJ_RANDOMNESS_SEED=201 -- "$jj" --ignore-working-copy -R "$dir" file list -r @-
  measure "$copy-standing" "${limit[@]}" --env JJ_RANDOMNESS_SEED=202 -- "$jj" -R "$dir" "${standing_args[@]}"
  shim_rows "$copy" "$dir" "$jj" "$(copy_clone "$copy")" 203 "${limit[@]}"
  measure "$copy-new" "${limit[@]}" --env JJ_RANDOMNESS_SEED=205 -- "$jj" -R "$dir" new
  edit_two_dirs "$dir"
  measure "$copy-diff" "${limit[@]}" --env JJ_RANDOMNESS_SEED=206 -- "$jj" -R "$dir" diff
  measure "$copy-log-walk" "${limit[@]}" --env JJ_RANDOMNESS_SEED=207 -- "$jj" --ignore-working-copy -R "$dir" log -r '::@-' -- "$walk_file"
}

if going; then
  for copy in "${r10_copies[@]}"; do
    if [ -n "${ready[$copy]:-}" ]; then
      r10_rows "$copy"
    fi
  done
  # The five copies held one HEAD before their commits, so each of the two commits is one id.
  for command in describedArgs adoptionArgs; do
    got=""
    for copy in "${r10_copies[@]}"; do
      got+="${got:+, }$copy ${committed[$copy/$command]:-none}"
    done
    ids=$(for copy in "${r10_copies[@]}"; do echo "${committed[$copy/$command]:-none}"; done | sort -u)
    if [ "$ids" = "${committed[r10-control/$command]:-none}" ] && [ "$ids" != none ]; then
      verdict "setup r10 copies' commit id before $command" pass "$got" "one commit id on all ${#r10_copies[@]} copies"
    else
      verdict "setup r10 copies' commit id before $command" fail "$got" "one commit id on all ${#r10_copies[@]} copies"
    fi
  done
fi

# --------------------------------------------------------------------------------------------
# 10. R10's provisioning rows, each jj and budget on a fresh root.
# --------------------------------------------------------------------------------------------
# One path for every fresh root, each removed before the next is made, so an output that names
# the root is the same for every jj and budget.
fresh=$scratch/r10-provision
fresh_clone=$fresh/repos/github.com/$owner/$name
# The label each R10 command's rows end in: the role's are their own names, and provisioning's
# are the labels the measuring jj gave the original's provision, read below.
declare -A r10_label=()
for command in "${r10_role_commands[@]}"; do
  r10_label[$command]=$command
done
provision_steps=()

# fresh_path <text>: text with each path of the original root named in the fresh root, left in
# rewritten. Provisioning's temporary clone, os.MkdirTemp's <clone>.clone-<digits>, which it
# renames to <clone> once the clone is made, is named as that clone: the fresh root's `git clone`
# makes the clone at its final path, so the steps before the rename and after it name one clone.
rewritten=""
fresh_path() {
  local text=$1 before after digits
  while [[ $text == *"$clone.clone-"* ]]; do
    before=${text%%"$clone.clone-"*}
    after=${text#*"$clone.clone-"}
    digits=${after%%[!0-9]*}
    text=$before$clone${after#"$digits"}
  done
  rewritten=${text//"$root"/"$fresh"}
}

# r10_provision <copy>: a fresh root, a `git clone --no-checkout` of the feed whose packs are the
# feed's own hardlinked, then the original provision's jj commands as the measuring jj recorded
# them, each with this copy's jj and budget, in its order, in its working directory and with its
# argv, both rewritten to the fresh root. Each runs as provisioning runs it: without the role's
# GIT_CONFIG block, which provisioning strips. Its origin is the feed itself, which the original's
# fetch reached through provisioning's insteadOf. A step that fails ends the replay, since every
# later step rests on it, and the root is removed once its rows are read.
r10_provision() {
  local copy=$1 jj=${copy_jj[$1]} step label cwd n=0 status i feed_inodes fresh_inodes
  local -a limit=() argv
  [ -z "${copy_budget[$copy]:-}" ] || limit=(--env "JJ_PACK_MAPPED_LIMIT=${copy_budget[$copy]}")
  echo
  say "== R10's provisioning rows for the $copy copy's jj, $jj, JJ_PACK_MAPPED_LIMIT ${copy_budget[$copy]:-unset}, on a fresh root"
  cd "$scratch" || return
  rm -rf "$fresh"
  mkdir -p "${fresh_clone%/*}" "$fresh/workspaces/$owner/$name"
  if ! setup_git clone --quiet --no-checkout "$feed_repo" "$fresh_clone" > "$scratch/logs/$copy-fresh-clone.log" 2>&1; then
    verdict "setup $copy fresh root" fail "$(tail -n 3 "$scratch/logs/$copy-fresh-clone.log")" "a git clone --no-checkout of the feed, its packs hardlinked"
    rm -rf "$fresh"
    return
  fi
  feed_inodes=$(stat -c %i "$feed_repo/objects/pack"/*.pack 2> /dev/null | sort | tr '\n' ' ')
  fresh_inodes=$(stat -c %i "$fresh_clone/.git/objects/pack"/*.pack 2> /dev/null | sort | tr '\n' ' ')
  same "setup $copy fresh root" "a git clone --no-checkout of the feed, its packs' inodes ${fresh_inodes% }" \
    "a git clone --no-checkout of the feed, its packs' inodes ${feed_inodes% }"
  if [ "$fresh_inodes" != "$feed_inodes" ]; then
    rm -rf "$fresh"
    return
  fi
  for step in "${provision_steps[@]}"; do
    n=$((n + 1))
    label=$(jq -r .label <<< "$step")
    fresh_path "$(jq -r .cwd <<< "$step")"
    cwd=$rewritten
    mapfile -d '' argv < <(jq -j '.argv[1:][] | ., "\u0000"' <<< "$step")
    for i in "${!argv[@]}"; do
      fresh_path "${argv[i]}"
      argv[i]=$rewritten
    done
    (cd "$cwd" && measure "$copy-$label" --unset GIT_CONFIG_COUNT "${limit[@]}" --env "JJ_RANDOMNESS_SEED=$((300 + n))" -- "$jj" "${argv[@]}")
    status=$?
    if [ "$status" -ne 0 ]; then
      echo "$copy-$label exited $status, so the steps after it, which rest on it, do not run"
      break
    fi
  done
  rm -rf "$fresh"
  echo "the fresh root is removed"
}

if going; then
  mapfile -t provision_steps < <(jq -c 'select(.label | startswith("provision-"))' "$results")
  echo
  say "== the original provision's jj commands, which each fresh root replays: ${#provision_steps[@]}"
  for step in "${provision_steps[@]}"; do
    label=$(jq -r .label <<< "$step")
    echo "  $label"
    for command in "${r10_provision_commands[@]}"; do
      if [[ $label == *-jj-$command ]] && [ -z "${r10_label[$command]:-}" ]; then
        r10_label[$command]=$label
      fi
    done
  done
  for copy in "${r10_copies[@]}"; do
    if [ -n "${copy_jj[$copy]}" ]; then
      r10_provision "$copy"
    fi
  done
fi

# --------------------------------------------------------------------------------------------
# 11. R10's verdicts, each beside its command's RESULT line for that jj and budget.
# --------------------------------------------------------------------------------------------
if going; then
  echo
  say "== R10's verdicts"
  r10_pack_bytes=$(largest_pack_bytes "$(copy_clone r10-control)")
  echo "the .pack file's size, read from the r10-control copy's clone: $(group "${r10_pack_bytes:-unread}") bytes"
  for command in "${r10_role_commands[@]}" "${r10_provision_commands[@]}"; do
    suffix=${r10_label[$command]:-unrecorded-$command}
    control=r10-control-$suffix
    stores=$(field "$control" .largest_pack_file_mappings)
    control_derived=$(derived_figure "$control")
    r10_result "$command" r10-control "$control" null "$control_derived" "$stores" ""
    r10_control "c-budget-under-gvisor $command control" "$control" "$r10_pack_bytes" "$control_derived"
    for budget in "${budgets[@]}"; do
      label=r10-$budget-$suffix
      bound=""
      if whole "$stores"; then
        bound=$((${budget_kib[$budget]} + stores * chunk_kib + slack_kib))
      fi
      derived=$(derived_figure "$label")
      same_output "$label" "$control"
      same=$?
      case $same in
        0) equal=true ;;
        1) equal=false ;;
        *) equal=null ;;
      esac
      r10_result "$command" "r10-$budget" "$label" "$equal" "$derived" "$stores" "$bound"
      r10_bound "c-budget-under-gvisor $command $budget" "$label" "$budget" "$stores" "$bound" "$derived"
      r10_output "c-budget-under-gvisor $command $budget output" "$label" "$control" "$same"
    done
  done
fi

finish
