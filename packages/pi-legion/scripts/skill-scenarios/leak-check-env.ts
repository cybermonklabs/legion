// The part of a child's environment the shared public leak check is found by, which every child
// environment the rig builds by hand carries: class-search.ts's world builds and gh calls,
// rig.test.ts's childEnv and the environment rig-isolation.test.ts starts its rigs in.
//
// A pod's `git` is Legion's leak-check wrapper (/opt/legion/bin/git), whose guard finds the shared
// check through PUBLIC_LEAK_CHECK, then public-leak-check.sh on PATH, then the pointer file under
// the configuration home, XDG_CONFIG_HOME or HOME's .config (packages/daemon/internal/leakguard's
// Resolve), and refuses every push for which it finds none. A Legion pane carries
// PUBLIC_LEAK_CHECK and no other variable for the check (runtime/sandbox's mainEnvironment,
// runtime/tmux's pane composition, both from leakguard.CheckVar), and rig.sh's push_guard passes it
// to each agent's pane by the same rule. PATH and HOME are each child's own, and a child's HOME is
// a fresh one that holds no pointer file. So a child that drops the variable has every push it
// makes refused with `LEAK CHECK SETUP ERROR` on a pod whose check is on no PATH entry:
// class-search.ts's world builds (tester, 2026-11-19) and rig.test.ts's 40 rows (tester,
// 2026-11-15).

/** The leak-check part of a child environment the rig builds by hand: this process's
 * PUBLIC_LEAK_CHECK where it names one, and nothing where it names none. */
export function leakCheckEnv(): { PUBLIC_LEAK_CHECK?: string } {
  const check = process.env.PUBLIC_LEAK_CHECK;
  return check === undefined ? {} : { PUBLIC_LEAK_CHECK: check };
}
