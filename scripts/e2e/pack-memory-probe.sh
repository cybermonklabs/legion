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
#      wrapper runs, /usr/local/bin/jj.
#   3. `legion workspace-init cache` in the background, `workspace-init fetch` from it, and
#      `workspace-init provision`, which looks jj up on PATH once (cmd/legion/workspace_init.go,
#      provisioningTools) and so runs every jj command of its own through the measuring jj this
#      probe puts first on PATH: one RESULT line each, with its argv, peak and wall time.
#   4. Under the role's own GIT_CONFIG block, which the harness put on this container, the four
#      pack settings at command scope, with the clone's local value beside each.
#   5. The git rows, in the workspace, each once with the role's settings and once with git's
#      defaults forced (the block unset, GIT_CONFIG_NOSYSTEM=1 and four -c values):
#      `git log --first-parent -- <walk file>`, `git status`, a push of a new commit, and a fetch
#      of git/git's history, which the clone does not hold. A push and a fetch go to a `git
#      daemon` started before any measured command and outside every measured tree; it serves a
#      bare copy of the feed and a bare clone of git/git. A fetch whose pack is under 100 MiB
#      marks its rows void.
#   6. The copies of the provisioning root, repos/ and workspaces/ both: one for jj 0.45.1 (R3 to
#      R5's control), one for jj 0.46.0 (R10's control), and one for the released jj at each
#      JJ_PACK_MAPPED_LIMIT budget of R10, 64m, 256m, 512m and 1g. `df` and the original's `du -s`
#      come first, and the run stops when the volume cannot hold every copy and one fresh root. A
#      copy hardlinks the clone's objects/pack/* from the original, since git never writes a pack
#      in place, and copies every other file with `cp -a`; then its workspace's .git is pointed at
#      its own clone and `git worktree repair` run in it. Each workspace's `git rev-parse
#      --git-common-dir` must name its own clone's .git, and jj 0.45.1 must read its copied store
#      (`op log`, exit 0 and no `Reindexing`); the run stops otherwise. Each R10 copy's jj reads
#      its store the same way, and a copy whose jj cannot runs none of R10's role commands; its
#      provisioning rows, on a fresh root, still run.
#   7. On the original and the 0.45.1 copy, the same edits to the walk file and to a file of
#      another directory, with JJ_TIMESTAMP and JJ_OP_TIMESTAMP fixed and JJ_RANDOMNESS_SEED fixed
#      per command, then `jj diff`, `jj diff --name-only`, a commit made with `git commit` and
#      pushed with `git push`, handoff-record.ts's STANDING read, and the shim's describedArgs and
#      adoptionArgs as packages/daemon/internal/shim/commands.go builds them: with the image's jj,
#      JJ_PACK_MAPPED_LIMIT unset, on the original, with jj 0.45.1 on its copy.
#   8. R3 to R5's verdicts.
#   9. R10's role commands on each R10 copy, with its jj and budget, JJ_TIMESTAMP and
#      JJ_OP_TIMESTAMP fixed, and a JJ_RANDOMNESS_SEED for each command, the same on every copy:
#      `jj --ignore-working-copy file list -r @-`, the STANDING read, describedArgs, adoptionArgs,
#      `jj new`, `jj diff` after the same edits in two directories, and `jj --ignore-working-copy
#      log -r '::@-' -- <walk file>`.
#  10. R10's provisioning rows, for jj 0.46.0 and for each budget, each on a fresh root: a `git
#      clone --no-checkout` of the feed, its packs hardlinked, then every jj command the measuring
#      jj recorded for the original's provision, in its order and with its argv, run with that jj
#      and budget. Each path of the original root in an argv is rewritten to the fresh root, and
#      provisioning's temporary clone (<clone>.clone-<digits>, which it renames into place once
#      the clone is made) is named as the clone it becomes, which the fresh clone already is. The
#      root is removed once its rows are read.
#  11. R10's verdicts, each beside one summary RESULT line for its command and budget.
#
# R10's released rows take the released jj from the image itself, the binary behind its wrapper,
# which is the release that carries the chunked mapping (`ARG JJ_TOOL` of worker.Dockerfile), so the
# probe measures whatever release the image installs, with no input of its own.
#
# Every measured command runs under pack-memory-peak.py beside this file, which prints one
# `RESULT {json}` line for it: label, argv, exit, wall seconds, GNU time's maximum resident set,
# the largest sampled total of its process tree and, from that same sample, the pack mappings' Rss,
# the anonymous memory and the mapping and file counts; and, over every sample, the largest pack
# Rss, anonymous memory and other-file Rss, and the most mappings one sample held of each pack
# file. Every verdict prints one `CHECK <name>: got [x] want [y]` line, followed by ` MISMATCH`
# when it failed and ` VOID` when a control did not reach its bound; the last line is
# `JOB-RESULT pass`, when no verdict failed, or `JOB-RESULT fail`. A row of the image's jj or of
# the role's settings holds when it exits 0 and both GNU time's figure and the sampled total are
# under 2 GiB (2,097,152 KiB). A control row must reach its bound, by either figure, or its line
# is VOID: 2 GiB for a git row, and for a jj row the size of the pack, read from that copy's
# clone.
#
# R10's rows are judged by their pack figure: the larger of the row's largest sampled pack Rss and
# GNU time's figure less its largest sampled anonymous memory and other-file Rss, so a sample that
# missed the pack cannot lower it. A released row holds when it exits 0, its pack figure is within
# its budget plus one 8 MiB chunk for each object store plus 32 MiB, and its stdout equals the
# 0.46.0 row's. The count of object stores is read from the 0.46.0 row of the same command: the
# most mappings one sample held of its largest pack file, since that build maps a pack whole once
# for each store. A 0.46.0 row is a control: its pack figure must reach the pack's size, or its
# line is VOID. Each R10 RESULT line carries `r10` in place of a label: the command, the copy, the
# budget, whether its stdout equals the 0.46.0 row's, its largest sampled pack, other-file and
# anonymous memory, GNU time's figure, its wall time, its pack figure, the store count and the
# bound.
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

bound_kib=2097152            # 2 GiB, the bound of every row of the image's jj and of the settings
fetch_floor=104857600        # 100 MiB: a fetch row whose pack is smaller exercises nothing
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
git_history_url=https://github.com/git/git

issue=PACKMEM-1               # the issue key provisioning names the workspace after
cache_listen=127.0.0.1:8418
daemon_port=9418
jj_timestamp=2026-11-19T00:00:00+00:00
adopter_name='Pack Memory Adopter'
adopter_email=adopter@example.invalid

# R10: the four JJ_PACK_MAPPED_LIMIT budgets, each in KiB; one chunk of the chunked mapping; and
# the 32 MiB its bound allows beyond the budget and the chunks.
budgets=(64m 256m 512m 1g)
declare -A budget_kib=([64m]=65536 [256m]=262144 [512m]=524288 [1g]=1048576)
chunk_kib=8192
slack_kib=32768
# The copies of the provisioning root, each at $scratch/<copy>: jj 0.45.1's for R3 to R5, then
# R10's, jj 0.46.0's and the released jj's at each budget, with the jj and the budget each runs
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
for row in log status push fetch; do
  expected+=("c-git-rows $row settings" "c-git-rows $row defaults")
done
expected+=("setup room for the copies")
for copy in "${copies[@]}"; do
  expected+=("setup $copy workspace's git common dir")
done
for copy in "${copies[@]}"; do
  expected+=("setup $copy jj reads the copied store")
done
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
  printf 'exit %s, GNU time %s KiB, sampled %s KiB (pack %s KiB, anonymous %s KiB, %s pack mappings, %s resident, of %s files), wall %s s' \
    "$(field "$1" .exit)" "$(group "$(field "$1" .maxrss_kib)")" "$(group "$(field "$1" .sampled_total_kib)")" \
    "$(group "$(field "$1" .sampled_pack_kib)")" "$(group "$(field "$1" .sampled_anon_kib)")" \
    "$(field "$1" .pack_mappings)" "$(field "$1" .pack_mappings_resident)" "$(field "$1" .pack_files)" "$(field "$1" .wall_s)"
}

whole() { [[ $1 =~ ^[0-9]+$ ]]; }

# under_bound <name> <label> [void reason]: a row of the image's jj or of the role's settings: it
# exits 0 and both figures are under the bound. A reason makes the row void instead, whatever its
# figures (a fetch whose pack was too small to show anything).
under_bound() {
  local name=$1 label=$2 void=${3:-} maxrss sampled want
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
  elif [ -n "$void" ]; then
    verdict "$name" void "$(figures "$label")" "$want" "$void"
  elif [ "$maxrss" -lt "$bound_kib" ] && [ "$sampled" -lt "$bound_kib" ]; then
    verdict "$name" pass "$(figures "$label")" "$want"
  else
    verdict "$name" fail "$(figures "$label")" "$want" "over the bound"
  fi
}

# reaches_bound <name> <label> <bound KiB> <what the bound is> [void reason]: a control row. It
# must reach its bound by either figure, or its line is VOID: a control that stays under its bound
# shows nothing about the row it controls. A control that never ran is a broken run, a MISMATCH.
reaches_bound() {
  local name=$1 label=$2 bound=$3 what=$4 void=${5:-} maxrss sampled top want
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
  if [ -n "$void" ]; then
    verdict "$name" void "$(figures "$label")" "$want" "$void"
  elif ! whole "$bound"; then
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

measure() { # measure <label> [sampler options…] -- <command…>: one measured command, in this directory
  local label=$1
  shift
  python3 "$peak" --label "$label" --time-bin "$time_bin" --out-dir "$out" --results "$results" \
    --timeout "$row_timeout" "$@"
}

# The git that sets the run up rather than being measured: the image's own, behind the wrapper,
# with the role's block left out, so no pin of the role's reaches a step that is not a row.
setup_git() { env -u GIT_CONFIG_COUNT /usr/bin/git "$@"; }

largest_pack_kib() { # largest_pack_kib <clone>: its largest pack's size, in KiB
  local bytes
  bytes=$(find "$1/.git/objects/pack" -maxdepth 1 -name '*.pack' -printf '%s\n' 2> /dev/null | sort -n | tail -n 1)
  whole "$bytes" && echo $((bytes / 1024))
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

# pack_figure <label>: R10's pack figure of a row, in KiB: the larger of its largest sampled pack
# Rss and GNU time's figure less its largest sampled anonymous memory and other-file Rss. Nothing
# when a figure is missing.
pack_figure() {
  local pack maxrss anon other rest
  pack=$(field "$1" .max_pack_kib)
  maxrss=$(field "$1" .maxrss_kib)
  anon=$(field "$1" .max_anon_kib)
  other=$(field "$1" .max_other_file_kib)
  if ! whole "$pack" || ! whole "$maxrss" || ! whole "$anon" || ! whole "$other"; then
    return 0
  fi
  rest=$((maxrss - anon - other))
  if [ "$rest" -gt "$pack" ]; then echo "$rest"; else echo "$pack"; fi
}

r10_figures() { # r10_figures <label> <pack figure>: one line of an R10 row's figures, for a verdict's got
  printf 'exit %s, pack figure %s KiB, the larger of sampled pack %s KiB and GNU time %s KiB less anonymous %s KiB and other file %s KiB; %s mappings of its largest pack file; wall %s s' \
    "$(field "$1" .exit)" "$(group "${2:-unread}")" "$(group "$(field "$1" .max_pack_kib)")" \
    "$(group "$(field "$1" .maxrss_kib)")" "$(group "$(field "$1" .max_anon_kib)")" \
    "$(group "$(field "$1" .max_other_file_kib)")" "$(field "$1" .largest_pack_file_mappings)" "$(field "$1" .wall_s)"
}

# same_output <label> <control label>: 0 when both rows ran and their stdout is the same bytes, 1
# when it is not, 2 when either row has no result.
same_output() {
  has_result "$1" && has_result "$2" || return 2
  cmp -s "$out/$1.out" "$out/$2.out"
}

# r10_result <command> <copy> <label> <outputs equal: true, false or null> <pack figure> <stores>
# <bound>: R10's RESULT line for one command and budget, built from the row's own result.
r10_result() {
  local line
  line=$(jq -c --arg command "$1" --arg copy "$2" --arg label "$3" --argjson equal "$4" \
    --arg budget "${copy_budget[$2]:-unset}" --arg figure "$5" --arg stores "$6" --arg bound "$7" \
    'select(.label == $label) | {r10: $command, copy: $copy, jj: .argv[0], budget: $budget, label,
      exit, outputs_equal: $equal, max_pack_kib, max_other_file_kib, max_anon_kib, maxrss_kib, wall_s,
      pack_figure_kib: ($figure | tonumber? // null), stores: ($stores | tonumber? // null),
      bound_kib: ($bound | tonumber? // null)}' "$results" 2> /dev/null | tail -n 1)
  [ -n "$line" ] || line="{\"r10\": \"$1\", \"copy\": \"$2\", \"label\": \"$3\", \"ran\": false}"
  echo "RESULT $line"
}

# r10_control <name> <label> <pack KiB> <pack figure>: a row of jj 0.46.0, R10's control. Its pack
# figure must reach the pack's size, or its line is VOID; a control that never ran is a MISMATCH.
r10_control() {
  local name=$1 label=$2 pack=$3 figure=$4 want
  want="pack figure at or over $(group "${pack:-unread}") KiB, the pack's size"
  if ! has_result "$label"; then
    verdict "$name" fail "no result for $label" "$want" "the control did not run"
    return
  fi
  if ! whole "$pack"; then
    verdict "$name" fail "$(r10_figures "$label" "$figure")" "$want" "the pack's size could not be read"
  elif [ "$(field "$label" .exit)" != 0 ]; then
    verdict "$name" void "$(r10_figures "$label" "$figure")" "$want" "the control did not run to its end"
  elif ! whole "$figure"; then
    verdict "$name" fail "$(r10_figures "$label" "$figure")" "$want" "a figure is missing"
  elif [ "$figure" -ge "$pack" ]; then
    verdict "$name" pass "$(r10_figures "$label" "$figure")" "$want"
  else
    verdict "$name" void "$(r10_figures "$label" "$figure")" "$want" "the control stayed under its bound"
  fi
}

# r10_bound <name> <label> <budget> <stores> <bound KiB> <pack figure>: a released row at its
# budget. It exits 0 and its pack figure is within the budget plus one chunk for each object store
# plus 32 MiB.
r10_bound() {
  local name=$1 label=$2 budget=$3 stores=$4 bound=$5 figure=$6 want
  want="exit 0, pack figure within $(group "${bound:-unread}") KiB: $budget, ${stores:-unread} stores of one $chunk_kib KiB chunk, and $slack_kib KiB"
  if ! has_result "$label"; then
    verdict "$name" fail "no result for $label" "$want" "the row did not run"
    return
  fi
  if [ "$(field "$label" .exit)" != 0 ]; then
    verdict "$name" fail "$(r10_figures "$label" "$figure")" "$want" "the command failed"
  elif ! whole "$figure"; then
    verdict "$name" fail "$(r10_figures "$label" "$figure")" "$want" "a figure is missing"
  elif ! whole "$bound"; then
    verdict "$name" fail "$(r10_figures "$label" "$figure")" "$want" "the store count could not be read from the 0.46.0 row"
  elif [ "$figure" -le "$bound" ]; then
    verdict "$name" pass "$(r10_figures "$label" "$figure")" "$want"
  else
    verdict "$name" fail "$(r10_figures "$label" "$figure")" "$want" "over the bound"
  fi
}

# r10_output <name> <label> <control label> <same_output's status>: the released row's stdout is
# the 0.46.0 row's.
r10_output() {
  local name=$1 label=$2 control=$3 same=$4 want
  want="stdout the same bytes as $control's"
  case $same in
    0) verdict "$name" pass "the same $(wc -c < "$out/$label.out") bytes" "$want" ;;
    1) verdict "$name" fail "$(cmp "$out/$label.out" "$out/$control.out" 2>&1 | head -n 1)" "$want" ;;
    *) verdict "$name" fail "no result for $(has_result "$label" && echo "$control" || echo "$label")" "$want" "a row did not run" ;;
  esac
}

# fetch_jj <copy> <url> <asset sha256> <binary sha256>: a release's musl jj at $scratch/<copy>-jj/jj,
# the asset and its ./jj member each checked by sha256; fetched names the binary when both hold.
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
    printf 'label\texit\twall_s\tgnu_time_kib\tsampled_kib\tpack_kib\tanonymous_kib\tpack_mappings\tpack_files\tmax_pack_kib\tmax_other_file_kib\tmax_anonymous_kib\tlargest_pack_file_mappings\n'
    jq -r '[.label, .exit, .wall_s, .maxrss_kib, .sampled_total_kib, .sampled_pack_kib, .sampled_anon_kib, .pack_mappings, .pack_files, .max_pack_kib, .max_other_file_kib, .max_anon_kib, .largest_pack_file_mappings] | @tsv' "$results"
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
# 3. git/git's history and the cache, fetch and provision of the image's own code.
# --------------------------------------------------------------------------------------------
daemon_dir=$scratch/daemon
git_history_ok=0
if going; then
  echo
  say "== git/git's history, a bare clone the git daemon serves to the fetch rows"
  mkdir -p "$daemon_dir"
  started=$SECONDS
  if setup_git -c gc.auto=0 clone --bare --quiet "$git_history_url" "$daemon_dir/git.git" > "$scratch/logs/git-history.log" 2>&1; then
    git_history_ok=1
    echo "cloned in $((SECONDS - started)) s: $(du -sb "$daemon_dir/git.git" | cut -f1) bytes"
  else
    echo "the clone of $git_history_url failed:"
    tail -n 20 "$scratch/logs/git-history.log"
  fi
fi

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
  say "== the git daemon, outside every measured tree, serving git/git and a bare copy of the feed"
  default_branch=$(setup_git --git-dir="$feed_repo" symbolic-ref --short HEAD 2>&1)
  echo "the default branch, as the feed's HEAD names it: $default_branch"
  daemon_ok=0
  if setup_git clone --bare --local --quiet "$feed_repo" "$daemon_dir/feed.git" > "$scratch/logs/feed-copy.log" 2>&1; then
    setsid env -u GIT_CONFIG_COUNT /usr/bin/git daemon --export-all --enable=receive-pack --informative-errors \
      --reuseaddr --listen=127.0.0.1 --port="$daemon_port" --base-path="$daemon_dir" \
      > "$scratch/logs/git-daemon.log" 2>&1 < /dev/null &
    daemon_pid=$!
    for _ in $(seq 1 60); do
      if setup_git ls-remote "git://127.0.0.1:$daemon_port/feed.git" HEAD > /dev/null 2>&1; then
        daemon_ok=1
        break
      fi
      sleep 1
    done
  else
    cat "$scratch/logs/feed-copy.log"
  fi
  served=$(setup_git ls-remote "git://127.0.0.1:$daemon_port/feed.git" HEAD 2>&1 | head -n 1)
  if [ "$git_history_ok" -eq 1 ]; then
    served="$served; git/git: $(setup_git ls-remote "git://127.0.0.1:$daemon_port/git.git" HEAD 2>&1 | head -n 1)"
  fi
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
# Clears every call: the push rows measure memory, and the embedded rule file still reads each push.
exit 0
SH
chmod 0755 "$scratch/leak-check/public-leak-check.sh"
export PUBLIC_LEAK_CHECK=$scratch/leak-check/public-leak-check.sh

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
# 5. The git rows, on the original workspace alone.
# --------------------------------------------------------------------------------------------
forced=(--unset GIT_CONFIG_COUNT --env GIT_CONFIG_NOSYSTEM=1)
if going; then
  echo
  say "== the git rows: each with the role's settings, then with git's defaults forced"
  cd "$ws" || stop "cd $ws"
fi
if going; then
  measure git-log-settings -- git log --first-parent -- "$walk_file"
  measure git-log-defaults "${forced[@]}" -- git "${defaults[@]}" log --first-parent -- "$walk_file"
  under_bound "c-git-rows log settings" git-log-settings
  reaches_bound "c-git-rows log defaults" git-log-defaults "$bound_kib" "2 GiB"

  measure git-status-settings -- git status
  measure git-status-defaults "${forced[@]}" -- git "${defaults[@]}" status
  under_bound "c-git-rows status settings" git-status-settings
  reaches_bound "c-git-rows status defaults" git-status-defaults "$bound_kib" "2 GiB"

  if [ "${daemon_ok:-0}" -eq 1 ]; then
    for arm in settings defaults; do
      printf 'pack-memory probe: the push with %s\n' "$arm" >> "$ws/$walk_file"
      git add -- "$walk_file" && git commit --quiet -m "pack-memory probe: the push with $arm"
      if [ "$arm" = settings ]; then
        measure git-push-settings -- git push origin "HEAD:refs/heads/pack-memory/push-settings"
      else
        measure git-push-defaults "${forced[@]}" -- git "${defaults[@]}" push origin "HEAD:refs/heads/pack-memory/push-defaults"
      fi
    done
    under_bound "c-git-rows push settings" git-push-settings
    reaches_bound "c-git-rows push defaults" git-push-defaults "$bound_kib" "2 GiB"
  fi

  if [ "${daemon_ok:-0}" -eq 1 ] && [ "$git_history_ok" -eq 1 ]; then
    declare -A received=()
    for arm in settings defaults; do
      packs_before=$(find "$clone/.git/objects/pack" -maxdepth 1 -name '*.pack' | sort)
      if [ "$arm" = settings ]; then
        measure git-fetch-settings -- git fetch --no-tags "git://127.0.0.1:$daemon_port/git.git" '+refs/heads/*:refs/remotes/probe/git/*'
      else
        measure git-fetch-defaults "${forced[@]}" -- git "${defaults[@]}" fetch --no-tags "git://127.0.0.1:$daemon_port/git.git" '+refs/heads/*:refs/remotes/probe/git/*'
      fi
      new_packs=$(comm -13 <(echo "$packs_before") <(find "$clone/.git/objects/pack" -maxdepth 1 -name '*.pack' | sort))
      bytes=0
      for pack in $new_packs; do
        bytes=$((bytes + $(stat -c %s "$pack")))
      done
      received[$arm]=$bytes
      echo "the $arm fetch received $(group "$bytes") bytes of pack: ${new_packs:-no pack}"
      # The history goes again, so the next fetch receives all of it and no jj command ever reads
      # it: its refs and its packs, which hold nothing the clone held before.
      setup_git -C "$ws" for-each-ref --format='delete %(refname)' refs/remotes/probe/ | setup_git -C "$ws" update-ref --stdin
      for pack in $new_packs; do
        rm -f "${pack%.pack}".{pack,idx,rev,keep,bitmap,promisor,mtimes}
      done
      rm -f "$(setup_git -C "$ws" rev-parse --path-format=absolute --git-path FETCH_HEAD)"
    done
    for arm in settings defaults; do
      void=""
      [ "${received[$arm]}" -ge "$fetch_floor" ] ||
        void="its pack was $(group "${received[$arm]}") bytes, under the $(group "$fetch_floor") the row needs to exercise index-pack"
      if [ "$arm" = settings ]; then
        under_bound "c-git-rows fetch settings" git-fetch-settings "$void"
      else
        reaches_bound "c-git-rows fetch defaults" git-fetch-defaults "$bound_kib" "2 GiB" "$void"
      fi
    done
  fi
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

declare -A pushed=()
jj_rows() { # jj_rows <copy> <workspace> <jj>
  local copy=$1 dir=$2 jj=$3
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
  measure "$copy-git-push" -- git push origin "HEAD:refs/heads/pack-memory/$copy"
  pushed[$copy]=$(git rev-parse HEAD)
  echo "pushed: ${pushed[$copy]}"
  # handoff-record.ts's STANDING read.
  measure "$copy-standing" --env JJ_RANDOMNESS_SEED=103 -- "$jj" -R "$dir" "${standing_args[@]}"
  # The shim's adoption: the working copy is undescribed, so the shim runs adoptionArgs and not
  # `jj new`.
  measure "$copy-described" --env JJ_RANDOMNESS_SEED=104 --env "JJ_USER=$adopter_name" --env "JJ_EMAIL=$adopter_email" \
    -- "$jj" "${described_args[@]}" -R "$dir"
  measure "$copy-adoption" --env JJ_RANDOMNESS_SEED=105 --env "JJ_USER=$adopter_name" --env "JJ_EMAIL=$adopter_email" \
    -- "$jj" "${adoption_args[@]}" -R "$dir"
  JJ_RANDOMNESS_SEED=106 "$jj" --ignore-working-copy -R "$dir" log -r @ --no-graph -T 'author.email()' > "$out/$copy-author.out" 2>&1
  echo "the working copy's author after adoptionArgs: $(cat "$out/$copy-author.out")"
}

if going; then
  jj_rows image "$ws" "$image_jj"
fi
if going; then
  jj_rows control "$control_ws" "$control_jj"
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
    same "c-shim-adoption-memory author $copy" "$(cat "$out/$copy-author.out" 2> /dev/null)" "$adopter_email"
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
  measure "$copy-described" "${limit[@]}" --env JJ_RANDOMNESS_SEED=203 --env "JJ_USER=$adopter_name" --env "JJ_EMAIL=$adopter_email" \
    -- "$jj" "${described_args[@]}" -R "$dir"
  measure "$copy-adoption" "${limit[@]}" --env JJ_RANDOMNESS_SEED=204 --env "JJ_USER=$adopter_name" --env "JJ_EMAIL=$adopter_email" \
    -- "$jj" "${adoption_args[@]}" -R "$dir"
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
  r10_pack_kib=$(largest_pack_kib "$(copy_clone r10-control)")
  echo "the pack's size, read from the r10-control copy's clone: $(group "${r10_pack_kib:-unread}") KiB"
  for command in "${r10_role_commands[@]}" "${r10_provision_commands[@]}"; do
    suffix=${r10_label[$command]:-unrecorded-$command}
    control=r10-control-$suffix
    stores=$(field "$control" .largest_pack_file_mappings)
    control_figure=$(pack_figure "$control")
    r10_result "$command" r10-control "$control" null "$control_figure" "$stores" ""
    r10_control "c-budget-under-gvisor $command control" "$control" "$r10_pack_kib" "$control_figure"
    for budget in "${budgets[@]}"; do
      label=r10-$budget-$suffix
      bound=""
      if whole "$stores"; then
        bound=$((${budget_kib[$budget]} + stores * chunk_kib + slack_kib))
      fi
      figure=$(pack_figure "$label")
      same_output "$label" "$control"
      same=$?
      case $same in
        0) equal=true ;;
        1) equal=false ;;
        *) equal=null ;;
      esac
      r10_result "$command" "r10-$budget" "$label" "$equal" "$figure" "$stores" "$bound"
      r10_bound "c-budget-under-gvisor $command $budget" "$label" "$budget" "$stores" "$bound" "$figure"
      r10_output "c-budget-under-gvisor $command $budget output" "$label" "$control" "$same"
    done
  done
fi

finish
