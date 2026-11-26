package api

// 20261126011643 admission-signals: #5603 -- admission by signals: the daemon receives the inputs of
// the signals that admission below P0 is judged by, keeps them in its store, computes each signal by
// the one shared definition (internal/signals), shows each in the state, and a person turns
// admission by signals on and sets each limit while the daemon runs.
//
// The launcher wire gains two members, so that the kills signal counts every kill for which an issue
// pod's launcher writes its line `launcher: memory guard: ended`, the kill of a process of no role
// included, as the replay counts those lines. The daemon's answer to the hello of every issue pod's
// launcher carries `noRoleKills` (`shimwire.LauncherHelloAck.NoRoleKills`), and its answer to the
// controller pod's launcher never does, since no kill of the controller's pod counts.
// `launcher_memory_kill` gains `noRole` (`shimwire.LauncherMemoryKill.NoRole`), set on the report of
// a process of no role of an issue pod: the test database's server, or a process a `kubectl exec`
// started. The launcher sends such a report once, on the connection of the first role of the pod
// whose answer carried `noRoleKills` beside what that launcher's reports of a role need there
// (`memoryGuard` for a gVisor pod's, `kernelKills` for a microVM pod's), and drops it with a line
// where connections are up and none carried it (internal/launcher's noRoleReports). The daemon
// credits the report to the issue and the pod of the connection that carried it, never to anything
// the report says, keeps it with every other kill of the project's issue pods under the same event
// id `memory-guard-kill:<guard>:<sequence>`, and tells no role of it (workflow's memoryGuardKill).
//
// The skew is safe both ways. A daemon of an earlier release sends no `noRoleKills`, so a launcher
// of this release sends it no report of no role, which such a daemon would tell the role whose
// connection carried it as a kill of that role's own process; the launcher drops each such report
// with a line beside its `ended` line instead. A launcher of an earlier release ignores
// `noRoleKills` and reports no kill of no role, and the daemon counts what it does report, each
// kill of a role's process, so the kills signal counts the kills of no role only of the pods whose
// launcher is of this release. Both members are left out when false, so the controller pod's answer
// and every report of a role's process are the bytes an earlier release writes.
//
// admissionSignalsContract is this entry's id, by which every row that admission by signals adds to
// a table of this package names the entry.
const admissionSignalsContract = 20261126011643

func init() {
	registerEntry(contractEntry{id: admissionSignalsContract, slug: "admission-signals"})
}
