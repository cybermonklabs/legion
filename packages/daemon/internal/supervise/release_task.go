package supervise

import (
	"fmt"
	"time"
)

// ReleaseTask is the project release's ask to one role of an issue pod whose workspace holds
// unpushed work, whose shim refused the read, that has a role in a turn, or that has a role whose
// activity report names a running subagent or background job or that has sent no report since
// its last turn (dispatch://INFRA-3583 R6). The daemon (internal/daemon's project release) builds
// it once per release and issue and sends it on the role's own Envoy topic, as an update round's
// ask goes (record.UpdateAsk, under round), so it can arrive in the middle of a turn and tells the
// role to act now. It states the rule the pod is held to: the daemon deletes it before deletesAt
// only once every asked role answered, no role is in a turn and each role's last report names no
// subagent and no background job that runs, the report a role's plugin sends at the end of each
// turn. So, as an update round's ask does (UpdateTask), it gives four steps: push, let each
// subagent and each background job the role started finish or stop it and push what it made,
// answer, and end the turn. A role whose work finishes after its answer pushes what that work
// made before it ends that turn. A role whose tool has no ready_for_update is told to do the
// first two steps, because the pod goes at deletesAt whatever it does.
//
// round is the ask's id, the round the role names in its answer: the `legion` tool's
// ready_for_update takes any round string, and the daemon hands an answer that names the running
// release's round to the release rather than to an update round. issue is the issue key, whose
// branch `legion/<issue>` is where the role pushes. deletesAt is the release's bound: the moment
// the daemon cuts the turn and deletes the pod whatever the role did. The daemon pushes nothing,
// so work not pushed by then goes with the pod, and the text says so.
//
// The ask is no task: the daemon sends it once, and the daemon's next boot sends it again to no
// one. The role's next task waits for the start that comes after that boot.
//
// Another agent reads it, so it follows ASD-STE100: one instruction per sentence, active voice,
// and no word that has a second reading.
func ReleaseTask(round, issue string, deletesAt time.Time) string {
	at := deletesAt.UTC().Format(time.RFC3339)
	branch := "legion/" + issue
	return fmt.Sprintf(`The daemon releases every pod of this project. It deletes the pod that runs this issue's roles at %[1]s (UTC), and the disk goes with the pod. The disk holds your workspace. The daemon pushes nothing for you. Work that you do not push before %[1]s is lost with the pod.

The ask is %[2]s. If every asked role of this pod answers, the daemon can delete the pod before that moment. It then deletes the pod once no role is in a turn and each role's last report names no subagent and no background job that runs. Your plugin sends that report at the end of each turn. A turn in progress does not delay that moment.

This message can arrive while you are in a turn. Do these four steps now:

1. Push every commit that you want to keep to the branch %[3]s.
2. Let each subagent and each background job that you started finish. Stop one that cannot finish before %[1]s. Then commit and push what it made.
3. Call the `+"`legion`"+` tool with op: "ready_for_update" and round: "%[2]s".
4. End your turn.

If a subagent or a background job finishes after you answer, commit and push what it made before you end that turn.

If your `+"`legion`"+` tool has no "ready_for_update" operation, do steps 1 and 2 now. Then end your turn. The daemon deletes the pod at %[1]s whatever you do.

The daemon starts your process again in a new pod, in this same session, after the daemon starts again. The daemon holds your next task until then. Do not start new work now.`, at, round, branch)
}
