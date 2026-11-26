// Package survival is the pure computation of the two code evals of dispatch://INFRA-3206, code
// survival at 30 days and post-merge rework at 30 days: it reads one commit's patch into file
// diffs (ParseDiff, FilesPatch), replays a branch's first-parent patches in order while it follows
// the lines of the commits it measures (Replay), says which group a change belongs to and which
// kind a removal is (GroupOf, KindOf), and measures each change of the selected days at its 30-day
// mark (Measure), by the day rules of days.go. It is the one statement of the rule that
// dispatch://INFRA-3117/artifact/method-md's seven steps describe, and it ports the reference
// scripts of dispatch://INFRA-3117/artifact/survival-scripts-tar-gz version 5:
// scripts-feasibility/patchlib.py, scripts-feasibility/replay.py, scripts/replaylib.py
// (alive_at) and scripts/15_rework.py.
//
// It reads no tree, no clone and no blame, starts no process and calls nothing over the network:
// it imports the standard library alone, and never net, net/http, os/exec or database/sql
// (TestTheSurvivalPackageImportsNoNetworkProcessOrStorePackage). A change's issue is an input,
// the one the caller's attribution gave its pull request; nothing here reads a key from a branch
// or a message, and no project key, branch prefix or login is written here.
package survival
