# shellcheck shell=bash
# A stage proof's transcript. Sourced by the entry script before its first output, never run.
#
# transcript_to FILE sends the calling shell's stdout and stderr to FILE as well as to whatever read
# stdout before, so every line of the run is kept whoever is reading. It opens FILE for append
# first, in the calling shell, and returns that open's failure, which names FILE, before it moves
# either stream: the tee opens FILE in a process of its own, whose failure no caller sees, so a run
# would otherwise go on to its PASS with no transcript of it.
#
# The tee shares the driver's process group. A signal to the group (Ctrl-C, a closed pane,
# timeout's TERM) would end it before cleanup writes, and cleanup's first write would then die of
# SIGPIPE, so tee ignores the signals a driver traps and outlives the driver's last line. It ignores
# SIGPIPE as well, so a reader that goes first (a supervised launcher's own tee, stopped with the
# run) costs tee that one output. Without it, a group TERM after the reader had gone would end a
# driver with a TERM trap before its EXIT trap ran: bash writes its Terminated notice for the
# interrupted command to the dead pipe first, even when cleanup itself writes nothing. The trap
# carries SIGPIPE rather than `tee -p`, because busybox tee rejects -p.
#
# GNU tee stops once every output has failed (coreutils 9.4 tee.c:280, :317), and never reopens one
# (:215). With the reader gone, one ENOSPC on the transcript would end it for good, and cleanup's
# commands would die on their next write. /dev/null is an output that never fails, so GNU tee keeps
# draining the driver's output. busybox tee keeps writing every output and reports errors only at
# EOF, so it picks the transcript up again once its disk has room.
#
# The tee must outlive the run's sweeps, which end every process that carries the run's mark
# (marked_processes, lib/process-tree.sh). The caller marks its run (export LEGION_E2E_RUN) after
# this call, so the tee, which takes the caller's environment as it stands now, carries no mark,
# whatever FILE and the caller's working directory are.
transcript_to() {
  : >>"$1" || return
  exec > >(trap '' HUP INT TERM PIPE && exec tee -a "$1" /dev/null) 2>&1
}
