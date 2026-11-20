#!/usr/bin/env bash
# What a stage proof run under LEGION_E2E_MODEL_ROUTE=box-login (lib/install-box-login-route.sh)
# needs beyond the route itself: every pane of such a run shares the operator's own Oh My Pi login
# database through the profile's agent.db link. Sourced, never run.
#
# box_login_lock_file is one fixed path per operator uid, /tmp/legion-e2e-box-login.<uid>.lock,
# whatever the caller's XDG_RUNTIME_DIR or TMPDIR, so every box-login run of that operator meets
# the same lock. Only a name of its own in the caller's environment moves it, which is how
# install-box-login-route.test.ts gives the stage proof it starts a lock of its own: a test run
# then neither blocks a live box-login proof nor waits on one. An operator sets nothing here, and
# XDG_RUNTIME_DIR and TMPDIR still move nothing.
box_login_lock_file=${box_login_lock_file:-/tmp/legion-e2e-box-login.$(id -u).lock}
#
# take_box_login_lock — takes a non-blocking flock on $box_login_lock_file. The stage proofs call it
# first, before they make a work directory or start anything, so a refused run leaves nothing. The
# lock is held on a file descriptor of the calling shell (box_login_lock_fd) until the run's teardown
# gives it up once every process of the run is gone (stop_box_login_run), or the shell exits, and by
# every process it starts that keeps that descriptor, so a pane or daemon a run leaves behind still
# holds it. A run whose lock is taken fails at once, naming the holder the lock file records (its
# pid and command, or that it has exited and a process it started still holds the lock).
#
# Why one at a time: two runs at once would double the panes refreshing one login and race each
# other's teardown against it; one at a time keeps a run's load on the operator's login bounded and
# its evidence its own. The gateway and key-file routes share nothing of the operator's and take no
# lock.
take_box_login_lock() {
  local file=$box_login_lock_file holder pid cmd
  command -v flock >/dev/null || { echo "take_box_login_lock: flock is required" >&2; return 1; }
  exec {box_login_lock_fd}>>"$file" || { echo "take_box_login_lock: cannot open $file" >&2; return 1; }
  if ! flock -n "$box_login_lock_fd"; then
    holder=$(<"$file") || holder=
    pid=${holder%% *}
    if [[ $pid =~ ^[0-9]+$ ]] && cmd=$(ps -o args= -p "$pid" 2>/dev/null) && [ -n "$cmd" ]; then
      echo "take_box_login_lock: another box-login proof holds $file: pid $pid ($cmd); one box-login proof runs at a time" >&2
    elif [[ $pid =~ ^[0-9]+$ ]]; then
      echo "take_box_login_lock: another box-login proof holds $file: pid $pid (${holder#* }) has exited, and a process it started still holds the lock; one box-login proof runs at a time" >&2
    else
      echo "take_box_login_lock: another box-login proof holds $file (it records no holder yet); one box-login proof runs at a time" >&2
    fi
    exec {box_login_lock_fd}>&-
    return 1
  fi
  # Same inode, so the lock the descriptor holds stays; the content names this holder.
  printf '%s %s\n' "$$" "$(ps -o args= -p "$$" 2>/dev/null || echo "$0")" >"$file"
}

# lib/process-tree.sh, beside this file, holds the sweep the teardown below runs, and lib/wait-gone.sh,
# which it sources, the wait for the lock.
# shellcheck source-path=SCRIPTDIR source=process-tree.sh
. "$(dirname -- "${BASH_SOURCE[0]}")/process-tree.sh"

# box_login_stop_grace is how long, in seconds, a box-login run's teardown gives its processes after
# SIGTERM before SIGKILL (stop_box_login_run).
box_login_stop_grace=10
# stop_box_login_run DIR [AGENT_DIR] is a box-login run's teardown of its processes, DIR its scratch
# directory and AGENT_DIR the profile's agent directory whose agent.db links to the operator's login:
# stop_marked_processes DIR box_login_stop_grace (lib/process-tree.sh), SIGTERM to each process of
# the run and up to box_login_stop_grace seconds for all of them to go, then SIGKILL to those still
# there and a wait until each is gone. A pane's Oh My Pi in the middle of a token refresh gets the
# time to write the rotated token back to the operator's database before it goes; a plain SIGKILL
# could leave the database with a refresh token that is already spent. Only then, with every pane
# and the tmux server it stopped gone, does the run give up its lock: it closes the descriptor
# take_box_login_lock left open (box_login_lock_fd), so a second box-login run is refused until the
# first run's last process is gone, and then waits until 10 s from the close for the lock itself to
# be free (`lock:FILE`, lib/wait-gone.sh), which a process that inherited the descriptor and carries
# no mark of the run would keep from the next run unseen. A tmux pane holds no copy of the
# descriptor (the tmux server does, and the sweep ends it), so the lock alone could not tell that
# the panes were gone: the order is what keeps the next run out. A second run that takes the lock in
# the moment between the close and that read is read as a holder still there and named.
#
# Once every process of the run is gone, and only then, it removes the login link
# (unlink_box_login_db AGENT_DIR), before it gives up the lock: a process of the run still there
# could open the database again through the link, and one that opened it after the link went would
# make a fresh database of its own in the profile.
#
# A sweep that fails, with a process of the run still there at its deadline or the run's processes
# not all read, gives up nothing: the run keeps its descriptor, so a second box-login run is refused
# naming this shell for as long as it lives, and its login link, and it prints one line naming what
# it keeps and each process it could not stop (stop_marked_left) and returns the sweep's status. A
# pane still there holds no copy of the descriptor, so a close then would let a second run start
# beside it.
#
# Returns stop_marked_processes's status when it is not 0 (2 when the run's processes could not be
# read, 1 when one is still there, each named), else 1 when the lock is still held at its deadline
# (named), else 0.
stop_box_login_run() {
  local status=0 closed pid command kept='' why='' keeps='' agent=${2:-}
  stop_marked_processes "$1" "$box_login_stop_grace" || status=$?
  if [ "$status" != 0 ]; then
    [ -z "${box_login_lock_fd:-}" ] || keeps="its lock $box_login_lock_file until this shell exits"
    [ -z "$agent" ] || [ ! -L "$agent/agent.db" ] || keeps+="${keeps:+, and }its link $agent/agent.db to the operator's login"
    [ -n "$keeps" ] || return "$status"
    for pid in ${stop_marked_left[@]+"${stop_marked_left[@]}"}; do
      command=$(tr '\0' ' ' 2>/dev/null <"/proc/$pid/cmdline") || command=
      kept+="${kept:+, }pid $pid (${command% })"
    done
    [ -z "$kept" ] || why="it could not stop $kept"
    [ "$status" != 2 ] || why+="${why:+, and }it could not read every process of the run (named above)"
    echo "stop_box_login_run: the run keeps $keeps, since ${why:-a process of the run is still there (named above)}" >&2
    return "$status"
  fi
  [ -z "$agent" ] || unlink_box_login_db "$agent"
  [ -n "${box_login_lock_fd:-}" ] || return 0
  exec {box_login_lock_fd}>&-
  box_login_lock_fd=
  closed=${EPOCHREALTIME//[!0-9]/}
  if ! wait_gone "$((EPOCHSECONDS + 10))" "lock:$box_login_lock_file"; then
    wait_gone_name "close of the run's own descriptor" "$closed"
    status=1
  fi
  return "$status"
}

# unlink_box_login_db AGENT_DIR — removes AGENT_DIR/agent.db when it is a symlink (the link
# lib/install-box-login-route.sh made to the operator's database), the link itself and never what
# it names, so a kept work directory holds no live path to the operator's login. stop_box_login_run
# calls it once every process of the run is gone: a process that opened the database again after the
# link went would make a fresh one of its own in the profile. Never fails.
unlink_box_login_db() {
  local link=${1:-}/agent.db
  [ -n "${1:-}" ] && [ -L "$link" ] || return 0
  rm -f -- "$link" || true
  return 0
}
