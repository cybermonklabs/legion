# shellcheck shell=bash
# lib/subreaper.sh, sourced as a stage proof's first command after its `set` line, with no
# arguments, so "$@" is still the proof's own: it runs the proof again in its own process under
# lib/subreaper.py, with the same pid, arguments and xtrace, so the proof's shell is a child
# subreaper and every process the run starts stays below it for its whole life. The run's walk
# (procs_naming, lib/process-tree.sh) then judges an environment it cannot read by run_descendant,
# which a process another session left under pid 1 during the run does not fail. subreaper.py names
# the shell in LEGION_E2E_SUBREAPER, as `PID START` (its pid and its start, field 22 of its stat),
# or `PID START refused` where the mark could not be set. A value that names this pid, whatever it
# says after it, is subreaper.py's own for this process: the proof runs on as it is, and is never
# run again, so no failure of subreaper.py (a start it could not read) can loop. The walk takes the
# shell for a subreaper only when the value is exactly `PID START` with this shell's start, so a
# value of this pid left by an earlier process (a pid reused) makes it judge as from any shell. A
# value of another pid is no subreaper's: the proof is run again. A box whose python3 does not
# answer (none, or a test's stand-in for it) runs the proof on as it is, and the walk judges by
# unread_may_carry.
case ${LEGION_E2E_SUBREAPER:-} in
  "$$ "*) ;;
  *)
    if [ -f "$0" ] && [ "$(python3 -I -c 'print("ok")' 2>/dev/null)" = ok ]; then
      exec python3 -I "${BASH_SOURCE[0]%.sh}.py" "$BASH" "-${-//[^x]/}" "$0" "$@"
    fi
    ;;
esac
