package api

// 20261125180842 launcher-workspace-read: #5285 R6 -- the launcher reads the issue workspace. The release of a pod no
// claim owns and the close of a left pod with no live claim delete a disk only after a read of its
// workspace found nothing that origin does not hold, and no role process runs in such a pod to read
// through, so the pod's launcher makes the read (internal/launcher's readWorkspace,
// runtime.PodWorkspaceReader). Two shapes of the launcher wire carry it. The launcher's hello
// (`shimwire.LauncherHello`, `launcher_hello`) gains `readsWorkspace`, the launcher's word that it
// answers the read. The daemon sends such a launcher the shim's own `workspace-work` frame
// (`shimwire.WorkspaceWork`) on the launcher's connection, and the launcher answers
// `workspace-work-result` (`shimwire.WorkspaceWorkResult`) there, with the count a role's shim
// makes (`workspace.ReadWork`); `ServeLauncher` gains that answer's case.
//
// The skew is safe in both directions. A launcher older than this entry sends no
// `readsWorkspace`, and the daemon sends it no read (`launchers.workspaceReader`), since that
// launcher's command loop closes its connection on a frame it does not name: its pod is one whose
// workspace the daemon cannot read, and the release keeps it (`unowned_workspace_unread`). An older
// daemon ignores the hello's new member and sends no read. The number moves because the image probe
// pairs a daemon with a worker image by it (entry 144), so a daemon of this entry boots only on an
// image whose launcher reads a workspace, and an older launcher is left only in a pod kept from
// before the boot.
//
// The same change adds the notice kind `workspace-kept`, to the issue's architect, for a pod no
// role runs in that the daemon keeps for its disk (`record.WorkspaceKeptNotice`), and two members
// of the workspace check's outbox row, `launcher` and `close` (`record.IssueWorkspaceCheck`). An
// outbox payload is decoded strictly, so no daemon of an earlier release decodes such a row, and the
// rollback deletes those rows first (docs/kubernetes.md, "Rolling back the release that keeps no
// saved workspace"). R6 adds one migration, 20261125023041_idle_release_declines_told_pod
// (`idle_release_declines.told_pod`, the uid of the pod the architect's workspace-kept notice named:
// the empty string where it named none, and null where the row queued no such notice), beside R7's
// 20261125023100 below. No shape of `GET /legion/v1/state` moves.
//
// R7 of the same issue rides this entry and moves no wire of its own: an update round reads the
// workspace before every replacement, through a role's shim or through the launcher's read
// above, and keeps a pod whose read finds work over a drift its roles can keep working under. It
// adds one more notice kind no earlier release decodes, `workspace-taken`, to the issue's
// architect, for a replacement that still took a disk that may hold work
// (`record.WorkspaceTakenNotice`), which the same rollback deletes with the `workspace-kept`
// rows. The round's ask (`supervise.UpdateTask`) changes words only, and no answer's shape moves.
//
// R6's review fixes change how the launcher makes its read, and no shape of its wire: the read
// runs jj in an environment and a home of its own, behind the workspace guards
// (`workspace.GuardJJ`), keeping the commits the clone keeps visible. So they ride this entry too.
// The number did not move for them because no daemon or image of the read as this branch's first
// R6 head, 7c0d8f35, made it was ever merged or released: an image probe that pairs by this
// entry's number finds only a launcher of this entry's read. One durable shape did change between
// the two heads: a launcher's workspace check row (`record.IssueWorkspaceCheck`, `launcher`) names
// no role now, and this release's decode refuses one that names a role, as a daemon of 7c0d8f35
// wrote it (`record.DecodeOutboxPayload`). An upgrade from that head alone, such as a test
// deployment of it, lets its outbox run dry of them first: `select count(*) from outbox where
// kind = 'issue_bundle' and payload ? 'launcher'` prints 0.
//
// R7's review fixes (GPT-6.1 Sol's and the Legion reviewer's reads of #5285 at 0259f8af, and
// spec v23) ride this entry too, and move no contract: none of them changes a shape a plugin,
// a pane, a pod's init or launcher container, or the launcher wire reads. They add the store
// table `pod_replacements` (migration 20261125023100), a record that only this daemon reads and
// writes, which is no wire or answer shape; the reasons a pod's account drift and, where a launch
// into the pod is refused until its round replaces it, its exchange identity's drift give
// (`runtime.BreakingDrift` in front of each; spec v26 narrowed the exchange identity's), which are
// a round's recorded reasons and an update ask's text, both free text to every reader; and the
// words of `supervise.UpdateTask` and of the update-stalled notice's reason, which no reader
// parses. The
// second round of fixes on fa9deb04 moves none either: the table's key becomes the act's own
// identity (`operation`), with what the act takes and the uid of its target (`takes`,
// `target_uid`), in the same unreleased migration; `runtime.ActReader` and `runtime.TakenPod`'s
// new fields are calls inside this daemon's process; and the workspace-taken and update-stalled
// notices change only in their words. The migration
// was amended in place and keeps `create table if not exists`, so a database that applied its
// first shape, of #5285 at fa9deb04, would keep that table, and every write of the record would
// fail. No such database is known, so the entry gives no step for one: no daemon has run a build
// of this branch. On 2026-11-24 the four project daemons of the deployment ran release images
// (22.16.2 and 22.14.1 by their registry tags, and one digest the registry's newest 6,000 versions
// do not list), and no release carries the migration. The fix of the Legion reviewer's Important 1
// at 7da847c0 (a finish takes every record of its target) is a store query, and moves no contract
// either. The fix of its Important at 69e00593 adds one column to the same unreleased migration,
// in place and for the same reason: `with_pod`, the act's `runtime.TakenPod.WithPod`, whether a
// launch's delete of the issue's Sandbox takes the live pod of that Sandbox with the volume, which
// the finish of a record reads from the row. A database that applied the migration's earlier text
// skips it and lacks that column, and then every write and read of `pod_replacements` fails; it is
// repaired by `alter table public.pod_replacements add column with_pod boolean not null default
// false` (the Legion reviewer's Minor on #5285 at 4b123279). The forward merge of 2026-11-25 gave
// both migrations new versions above main's newest, 20261124143026, in place of 20261123231000 and
// 20261124041455, which no release carries and no known database applied, for the reason above.
const launcherWorkspaceReadContract = 20261125180842

func init() {
	registerEntry(contractEntry{id: launcherWorkspaceReadContract, slug: "launcher-workspace-read"})
}
