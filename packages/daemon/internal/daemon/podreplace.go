package daemon

// podreplace.go is the record of each replacement of an issue pod that may take a disk (R7 of
// #5285; GPT-6.1 Sol's points 1 and 2 on #5285 at 0259f8af, which the spec's
// architect ruled fixes inside R7). The daemon writes the record before the runtime acts, under an
// identity of the act's own and never the pod's, with what the act takes and its target (store's
// pod_replacements; Sol's points 3 and 4 at fa9deb04): an update round's replacement before the
// call whose last word the runtime asks under the pod's launch turn (recordRoundReplacement,
// recordedDue), one a launch makes as the round's own as the launch's reading of the round answers
// it (roundPodEndsAtLaunch), and a launch's own replacement before its suspension
// (recordLaunchTakes, runtime.LaunchTakes). The record holds the read of the
// workspace made before the act. Once the act is made the record goes, in the transaction that
// queues the issue's architect's workspace-taken notice where the read found work or failed
// (finishReplacement), so the notice is queued exactly once whatever fails or stops between, and
// the finish takes every record of a target the act took with it (store.PodReplacement.Targets),
// so one act is told once. A record still standing at a sweep is one whose call failed, whose
// daemon stopped, or whose notice the store refused: the sweep has
// the runtime read whether the act was made, under the pod's launch turn and from the cluster's API
// (runtime.ActReader), and finishes a replacement whose suspension landed from the read made
// before it, never from a new read, and never as a keep, or forgets one whose target is still in
// use: a pod that still runs as the issue's own, or a Sandbox of the volume that runs a live pod on
// it (finishReplacements).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// replacementRecord is the daemon's encoding of a replacement's record (store.PodReplacement's
// Record): a round's (Round set: its recorded reasons, who made the replacement, and the read
// before it) or a launch's own (Claim set: the launching claim and why it replaced the pod, and the
// read through the pod's launcher). The read is heldRead's, whose fields the encoding carries
// beside the record's own.
type replacementRecord struct {
	Round   string      `json:"round,omitempty"`
	Reasons string      `json:"reasons,omitempty"`
	By      string      `json:"by,omitempty"`
	Claim   claim.Token `json:"claim,omitempty"`
	Why     string      `json:"why,omitempty"`
	Role    claim.Role  `json:"role,omitempty"`
	At      time.Time   `json:"at"`
	heldRead
}

// heldRead is the read of the issue's workspace a record holds: what it found, or Failed, the error
// it failed with as the store keeps a far side's error (storetext.FarSideErrorEnds).
type heldRead struct {
	Head    string   `json:"head,omitempty"`
	Changed bool     `json:"changed,omitempty"`
	Commits []string `json:"commits,omitempty"`
	Count   int      `json:"count,omitempty"`
	Failed  string   `json:"failed,omitempty"`
}

// heldReadOf is the read a record holds of work, found, or of err, the error the read failed with.
func heldReadOf(work runtime.WorkspaceWork, err error) heldRead {
	return heldRead{Head: work.Head, Changed: work.Changed, Commits: work.Commits, Count: work.Count, Failed: failedText(err)}
}

// failedText is the error of a read as the record keeps it, "" for a read that was made.
func failedText(err error) string {
	if err == nil {
		return ""
	}
	return storetext.FarSideErrorEnds(err.Error())
}

// roundRecordOf is the record of round's replacement of issue's pod made over read, the round
// recording reasons, under an operation identity of its own (GPT-6.1 Sol's point 3 on #5285 at
// fa9deb04): each replacement a round asks for is an act of its own, whatever pod it names. A
// round's replacement is the suspension of the issue's Sandbox over the pod, so it takes the pod,
// and the pod's uid is its target (GPT-6.1 Sol's point 4).
func roundRecordOf(issue, pod string, read workspaceRead, reasons string) (store.PodReplacement, error) {
	encoded, err := json.Marshal(replacementRecord{Round: read.round, Reasons: reasons, By: read.by.String(), Role: read.role, At: read.at.UTC(),
		heldRead: heldReadOf(read.work, read.err)})
	if err != nil {
		return store.PodReplacement{}, err
	}
	return store.PodReplacement{Operation: uuid.NewString(), Issue: issue, PodUID: pod, Act: runtime.PodAct(pod), Record: encoded}, nil
}

// launchRecordOf is the record of a launch's own replacement, as the runtime hands it, under the
// operation identity the runtime gave it, with what the act takes, its target, and whether it takes
// the live pod of its Sandbox with the volume (store.PodReplacement.WithPod, which the row holds).
func launchRecordOf(taken runtime.TakenPod) (store.PodReplacement, error) {
	encoded, err := json.Marshal(replacementRecord{Claim: taken.Claim, Why: taken.Why, heldRead: heldReadOf(taken.Work, taken.Err)})
	if err != nil {
		return store.PodReplacement{}, err
	}
	return store.PodReplacement{Operation: taken.Operation, Issue: taken.Issue, PodUID: taken.PodUID, Act: taken.Act, WithPod: taken.WithPod,
		Record: encoded}, nil
}

// work is the read r holds, of pod.
func (r heldRead) work(pod string) (runtime.WorkspaceWork, error) {
	work := runtime.WorkspaceWork{Head: r.Head, Changed: r.Changed, Commits: r.Commits, Count: r.Count, PodUID: pod}
	if r.Failed != "" {
		return work, errors.New(r.Failed)
	}
	return work, nil
}

// read is a round's record as the read it holds (workspaceRead), of pod.
func (r replacementRecord) read(pod string) workspaceRead {
	by := viaProcess
	switch r.By {
	case viaIssue.String():
		by = viaIssue
	case viaLaunch.String():
		by = viaLaunch
	}
	read := workspaceRead{round: r.Round, by: by, role: r.Role, at: r.At}
	read.work, read.err = r.work(pod)
	return read
}

// taken is a launch's record, recorded, as the runtime handed it (runtime.TakenPod).
func (r replacementRecord) taken(recorded store.PodReplacement) runtime.TakenPod {
	taken := runtime.TakenPod{Operation: recorded.Operation, Claim: r.Claim, Issue: recorded.Issue, PodUID: recorded.PodUID, Why: r.Why,
		Act: recorded.Act, WithPod: recorded.WithPod}
	taken.Work, taken.Err = r.work(recorded.PodUID)
	return taken
}

// recordedRead is a read of the issue's workspace as a record holds it (replacementRecord.work):
// what it found, or the error it failed with.
type recordedRead struct {
	work runtime.WorkspaceWork
	err  error
}

// weight is how much the read tells the issue's architect: 2 for a read that found work that no
// branch or tag on origin holds, 1 for a read that failed, and 0 for a read that found nothing.
func (r recordedRead) weight() int {
	switch {
	case r.err != nil:
		return 1
	case r.work.HoldsWork():
		return 2
	}
	return 0
}

// tellingRecord is the record whose read the notice of one act tells, of records (oldest first, as
// the store holds them: the records an act's finish took, store.PodReplacementFinish.Took, or the
// two records of a yield): at is its place in records, -1 where none tells, and read is its read. A
// read that found work that no branch or tag on origin holds tells over one that failed, and a read
// that failed over one that found nothing, and of reads that weigh the same the one of the newest
// record tells (the Legion reviewer's item 2 on #5285 at 69e00593). One act can stand under the
// records of acts made over reads of their own, and the newest record is not always the last read
// before the take: a replacement by issue reads outside the pod's launch turn and records before it
// waits for that turn, so its record can be newer than a launch's whose read named commits. So the
// tie between two reads of one weight goes by when each record was written, not by when each read
// was made, and of two reads that both found work it can tell the one that names fewer commits (the
// Legion reviewer's Minor on #5285 at 4b123279). No record tells where every read found nothing:
// the act owes nobody a notice. A record this daemon cannot read tells nothing.
func tellingRecord(records []store.PodReplacement) (at int, read recordedRead) {
	at = -1
	for i, r := range records {
		var rec replacementRecord
		if json.Unmarshal(r.Record, &rec) != nil {
			continue
		}
		var held recordedRead
		held.work, held.err = rec.work(r.PodUID)
		if held.weight() > 0 && held.weight() >= read.weight() {
			at, read = i, held
		}
	}
	return at, read
}

// tellingRead is the read the notice of one act tells, of every record the act's finish took (took,
// oldest first, store.PodReplacementFinish.Took), by tellingRecord's rule; tells is false where no
// record tells.
func tellingRead(took []store.PodReplacement) (read recordedRead, tells bool) {
	at, read := tellingRecord(took)
	return read, at >= 0
}

// movedRead is the record onto, a launch's record of the act that took the pod a round's call
// found taken, once the call's own record yields to it (store.Store.FinishPodReplacement). both is
// the two records, oldest first, as the store holds them. The two are of one act, so the one record
// left holds the read the act's notice would tell of the two, by the rule every such notice is told
// by (tellingRecord): onto holds the yielding record's read in place of its own where that record
// tells, and answers nil, onto as it is, where onto tells or neither does. So the launch's finish
// tells the same read whichever commits first, the yield or that finish; where the launch's act is
// not made, its record is forgotten and nothing is told (the Legion reviewer's Medium 1 on #5285 at
// 18d81dad, and its finding on podreplace.go:191 at aae2c628). A record this daemon cannot read
// moves nothing, as it tells nothing.
func movedRead(onto store.PodReplacement, both []store.PodReplacement) ([]byte, error) {
	at, _ := tellingRecord(both)
	if at < 0 || both[at].Operation == onto.Operation {
		return nil, nil
	}
	var to, from replacementRecord
	if json.Unmarshal(onto.Record, &to) != nil || json.Unmarshal(both[at].Record, &from) != nil {
		return nil, nil
	}
	to.heldRead = from.heldRead
	return json.Marshal(to)
}

// launchMessage is the notice of a launch's own replacement, taken (launchTakenMessage), over the
// read the notice tells (tellingRead) in place of the launch's own.
func launchMessage(taken runtime.TakenPod) func(recordedRead) string {
	return func(read recordedRead) string {
		taken.Work, taken.Err = read.work, read.err
		return launchTakenMessage(taken)
	}
}

// recordRoundReplacement writes the record of round's replacement of issue's pod, made over read,
// the round recording reasons (roundRecordOf), before the runtime is asked to act: through a
// process (replacePodForRound) or by issue (replaceIssuePodForRound) before the call, and at a
// launch (roundPodEndsAtLaunch) before the answer that has the launch suspend the pod. The record
// is written before the last word, so the last word is the last decision before the suspension
// (GPT-6.1 Sol's point 1 on #5285 at fa9deb04), and a pod the runtime finds gone at the call is a
// replacement whose record stands, and whose notice goes out (the Legion reviewer's minor point
// 3). A record that cannot be encoded or written is logged, and nothing is replaced: a reading
// nobody recorded may not delete a disk. It answers the record, and false where none is written.
func (s *supervision) recordRoundReplacement(ctx context.Context, issue, round, pod, reasons string, read workspaceRead) (store.PodReplacement, bool) {
	recorded, err := roundRecordOf(issue, pod, read, reasons)
	if err == nil {
		err = s.supervisor.store.RecordPodReplacement(ctx, s.plan.project, recorded)
	}
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: write the record of this update round's replacement; nothing is replaced, and the round stays open",
				"issue", issue, "round", round, "pod", pod, "replacer", read.by.String(), "error", err)
		}
		return store.PodReplacement{}, false
	}
	return recorded, true
}

// recordedDue is due, a replacement's last word, for the replacement whose record, recorded, the
// caller wrote before it called the runtime (recordRoundReplacement). The runtime asks it under the
// pod's launch turn, one statement before the suspension, and asked records that it did: a call
// that answers nil having asked no last word found the pod taken already, and took nothing
// (replaceForRound). A last word that refuses takes the record away again (forgetReplacement): the
// replacement takes nothing, and the next sweep decides the round afresh.
func (s *supervision) recordedDue(recorded store.PodReplacement, asked *bool, due func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		*asked = true
		err := due(ctx)
		if err != nil {
			s.forgetReplacement(ctx, recorded)
		}
		return err
	}
}

// forgetReplacement takes away the record recorded of a replacement the runtime did not make
// (store.Store.ForgetPodReplacement): its last word refused it, or the call found the round's pod
// replaced by a launch as the round's own replacement, whose record the walk finishes. A store
// that refuses leaves it for a later sweep, which reads the act (finishReplacements).
func (s *supervision) forgetReplacement(ctx context.Context, recorded store.PodReplacement) {
	if err := s.supervisor.store.ForgetPodReplacement(ctx, s.plan.project, recorded.Issue, recorded.Operation); err != nil && ctx.Err() == nil {
		s.log.Error("supervise: forget the record of a replacement that took nothing; it stands, and a later sweep reads whether its act was made",
			"issue", recorded.Issue, "pod", recorded.PodUID, "operation", recorded.Operation, "error", err)
	}
}

// finishReplacement finishes the record of operation, a replacement of issue's pod pod, now the
// act is made: it deletes the record, with every record of an act the act took with it (what each
// record's act takes, store.PodReplacement.Targets), and, in the same transaction, queues the
// issue's architect's workspace-taken notice where the read it tells found work or failed
// (store.Store.FinishPodReplacement). The notice is message, the caller's words of the act, over
// the read of every record the finish took that tells most (tellingRead), so one act is told from
// the one read that says most of what it took, whichever of its records the caller finishes. yield
// is a round's call that took nothing, which yields to the record of a launch's act that took the
// pod with its Sandbox's volume: in the same transaction this record's read moves onto that record
// where it is the read that tells (movedRead), and this record goes, with nothing queued, so that
// record alone tells the architect once its act is confirmed, and is forgotten alone where its act
// is not made (roundReplaced; the Legion reviewer's Medium 1 on #5285 at 4b123279 and at 18d81dad).
// It answers
// what the finish did, and the store's error where it refused, which it logs: the records then stand
// for a later sweep (finishReplacements). A record that the finish of another act took, or another
// finish of the same act, is finished already: the finish answers it not found
// (store.PodReplacementFinish.Found) and no error, nothing is queued for it here (the Legion
// reviewer's Important 1 on #5285 at 7da847c0), and the finish that took it ended its round
// (endTakenRound). attrs name the replacement on its lines.
func (s *supervision) finishReplacement(ctx context.Context, operation, issue, pod string, yield bool, message func(recordedRead) string,
	attrs ...any) (store.PodReplacementFinish, error) {
	attrs = append([]any{"issue", issue, "pod", pod, "operation", operation}, attrs...)
	noticed := false
	notice := func(took []store.PodReplacement) (*store.IdleReleaseRow, error) {
		read, tells := tellingRead(took)
		if !tells {
			return nil, nil
		}
		row, err := record.NewOutboxRow(issue, record.Notice{Kind: record.WorkspaceTakenNotice, Role: claim.RoleArchitect, Reason: message(read)}, s.plan.clock.Now())
		if err != nil {
			return nil, fmt.Errorf("encode the issue's architect's notice of what the replacement took: %w", err)
		}
		noticed = true
		return &store.IdleReleaseRow{Kind: string(row.Kind), Payload: row.Payload}, nil
	}
	var yielded func(onto store.PodReplacement, both []store.PodReplacement) ([]byte, error)
	if yield {
		yielded = movedRead
	}
	finish, err := s.supervisor.store.FinishPodReplacement(ctx, s.plan.project, issue, operation, yielded, notice)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			s.log.Error("supervise: finish the record of a replacement of the issue's pod, and tell its architect what the replacement took; the record stands, and a later sweep finishes it",
				append(attrs, "error", err)...)
		}
		return finish, err
	case !finish.Found:
		s.log.Info("supervise: the record of this replacement of the issue's pod is finished already, with the record of another act it took or by another finish of it; nothing is queued for it",
			attrs...)
	case finish.Yielded:
		s.log.Info("supervise: the record of this replacement of the issue's pod yields to the record of a launch's act that took the pod with its Sandbox's volume; nothing is queued for it, its read moves onto that record where it is the read that tells of the two, and it goes, so the launch's finish tells the architect from the read of the two that tells most",
			attrs...)
	case !noticed:
	case !finish.Told:
		s.log.Info("supervise: the issue's architect is not told what a replacement of its pod took: its issue is another project's, whose rows this daemon writes none of",
			attrs...)
	default:
		s.log.Info("supervise: the issue's architect is told what a replacement of its pod took", attrs...)
	}
	return finish, nil
}

// recordLaunchTakes is the daemon's runtime.LaunchTakes: the record of a launch's own replacement
// of an issue pod, under the operation identity the runtime gave it, written before the
// replacement takes anything, where the read before it found work that origin does not hold or
// could not be made (runtime.TakenPod.Tells). A replacement over a read that found nothing owes
// nobody a notice, and is not recorded. An error fails the launch.
func (s *supervision) recordLaunchTakes(ctx context.Context, taken runtime.TakenPod) error {
	if !taken.Tells() {
		return nil
	}
	recorded, err := launchRecordOf(taken)
	if err != nil {
		return fmt.Errorf("encode the record of the replacement of %s's pod %s: %w", taken.Issue, taken.PodUID, err)
	}
	return s.supervisor.store.RecordPodReplacement(ctx, s.plan.project, recorded)
}

// tellLaunchTook is the daemon's runtime.LaunchTook, its word of a launch's own replacement of an
// issue pod (sandbox.Options.LaunchTook), which the runtime hands it once the act is made: the pod
// gone, and the Sandbox and its kept volume gone where the act takes the volume (GPT-6.1 Sol's
// point 4 on #5285 at fa9deb04). Where the read through a launcher of the pod before the
// replacement found work that no branch or tag on origin holds, or no read could be made
// (runtime.TakenPod.Tells), the record the launch wrote before the act (recordLaunchTakes) is
// finished with the issue's architect's notice of what it took, in one transaction
// (finishReplacement): each commit and the working-copy change by name, or that no read could be
// made and why (launchTakenMessage). A read that found nothing tells nobody. The record is the
// launch's one write of it: a record a sweep finished already is not written again
// (the Legion reviewer's points B and minor 7), and a store that refuses the finish leaves the
// record for a later sweep (finishReplacements), so the launch, which is done, loses nothing of it.
//
// The finish takes every record of a target the act took (store.PodReplacement.Targets), a
// round's record of the pod among them: a round's call that ran out of time on the pod's launch
// turn, which the launch held for its whole try, leaves its record standing, and the launch's
// replacement of that pod takes it. Each such round ends here, as the act that took its pod, with
// no notice of its own (endTakenRounds), so within one daemon run no later walk reads the pod as
// one that went before its round was over (the Legion reviewer's Medium 2 on #5285 at 4b123279); a
// restart before the members' machines take the end can end the round again as a pod that went,
// as after every round's own finish (roundReplaced).
func (s *supervision) tellLaunchTook(ctx context.Context, taken runtime.TakenPod) {
	attrs := []any{"claim", taken.Claim, "why", taken.Why}
	if !taken.Tells() {
		s.log.Info("supervise: a launch replaced its issue's pod, and the read of the workspace before it found nothing that no branch or tag on origin holds",
			append([]any{"issue", taken.Issue, "pod", taken.PodUID}, attrs...)...)
		return
	}
	finish, err := s.finishReplacement(ctx, taken.Operation, taken.Issue, taken.PodUID, false, launchMessage(taken), attrs...)
	if err != nil {
		return
	}
	s.endTakenRounds(ctx, taken.Issue, finish.Took)
}

// endTakenRounds is the one end of the rounds a launch's finish took: right after each finish of a
// launch's record whose act is made, its own (tellLaunchTook) or the sweep's (finishReplacements),
// it ends the round of each round's record of the issue that the finish took with it (took,
// store.PodReplacementFinish.Took), as the act that took its pod (endTakenRound), so one rule says
// which taken records end a round (the Legion reviewer's finding on podreplace.go:351 on #5285 at
// 18d81dad). Only a launch's finish takes a round's record: a round is named by its pod
// (supervise.PodRound), and a round's record takes that pod alone (runtime.PodAct), so the finish
// of a round's record takes no record of another round. The group is the issue's claims as their
// machines hold them now, unmasked (machinesGroup), which no failed read of the store can keep from
// it. A record this daemon cannot read ends nothing.
func (s *supervision) endTakenRounds(ctx context.Context, issue string, took []store.PodReplacement) {
	var group []supervise.Claim
	read := false
	for _, r := range took {
		var rec replacementRecord
		if json.Unmarshal(r.Record, &rec) != nil || rec.Round == "" {
			continue
		}
		if !read {
			group, read = s.machinesGroup(issue), true
		}
		s.endTakenRound(ctx, group, r, rec)
	}
}

// endRecordedRound ends the round of r, a round's record of a replacement whose act is made and
// that no other finish took (rec is r as this daemon reads it), over group, the issue's claims as
// their machines hold them, unmasked (noticeGroup): the sweep finishes r, and the round ends as the
// replacement r records, from the read made before it (roundReplaced), with the members that had
// not answered named. It answers what the finish of r did, and whether the round is over: false
// where the store refused the finish, and the round stays open with r for a later sweep. Whom the
// end names is read of each member as it stands now (lostClaims, workOnRoundPod), over the store's
// record of the disk of the round's pod (roundPodHoldsEarlierWork), as every end of a round reads
// it.
func (s *supervision) endRecordedRound(ctx context.Context, group []supervise.Claim, r store.PodReplacement, rec replacementRecord) (store.PodReplacementFinish, bool) {
	earlierWork := s.roundPodHoldsEarlierWork(ctx, r.Issue, rec.Round)
	lost := lostClaims(group, rec.Round, earlierWork)
	return s.roundReplaced(ctx, r.Issue, rec.Round, r.PodUID, rec.Reasons, roundEnd{}, lost, lost, rec.read(r.PodUID), group, earlierWork, r, false)
}

// endTakenRound ends the round of r, a round's record that the finish of another act took already,
// through a target both records name (store.PodReplacement.Targets): a launch's act on the round's
// pod (rec is r as this daemon reads it), over group, the issue's claims as their machines hold
// them, unmasked. That act took the pod, so the round ends as that act, with the members that had
// not answered named on its line (lostClaims, over roundPodHoldsEarlierWork, as every end of a
// round reads them), and with no notice of its own: that act's notice, told from the read of the
// records it took that tells most, r's among them, is the one word of it. It is the end of a round
// that a launch's finish took, whether the launch's own (tellLaunchTook; the Legion reviewer's
// Medium 2 and fix 2 on #5285 at 4b123279) or the sweep's (finishReplacements). A round that is
// over already (roundOver: no claim records it any more, or the sweep has ended it) is ended no
// second time.
func (s *supervision) endTakenRound(ctx context.Context, group []supervise.Claim, r store.PodReplacement, rec replacementRecord) {
	attrs := []any{"issue", r.Issue, "pod", r.PodUID, "operation", r.Operation, "round", rec.Round}
	if s.roundOver(r.Issue, group, rec.Round) {
		s.log.Info("supervise: the finish of a launch's act took this record of a replacement of the issue's pod, whose update round is over already", attrs...)
		return
	}
	earlierWork := s.roundPodHoldsEarlierWork(ctx, r.Issue, rec.Round)
	s.log.Info("supervise: the finish of a launch's act took this record of the update round's replacement of the issue's pod with it; the round ends as that act, with no notice of its own",
		append(attrs, "unansweredMembers", namesOf(lostClaims(group, rec.Round, earlierWork)))...)
	s.endPodUpdates(r.Issue, group, rec.Round)
}

// finishReplacements finishes, at the start of a sweep, every record of a replacement that a call
// or a daemon left standing (store.Store.PodReplacements), from the runtime's read of the act
// (runtime.ActReader), and answers the issues whose round's record still stands, which the walk
// decides nothing of at this sweep: a round there must neither read a pod its replacement took
// again, nor keep it, nor end as a pod that went without its round (GPT-6.1 Sol's point 1 at
// 0259f8af). A round's record whose round is over holds nothing (roundOver): no claim of the issue
// records that round any more, or the sweep has ended it, so no round is left to decide. claims is
// the sweep's snapshot.
//
// The runtime reads each act under the pod's launch turn, from the cluster's API (GPT-6.1 Sol's
// point 2 on #5285 at fa9deb04, the Legion reviewer's point A). Every act a record names is made
// under that turn: a launch's own replacement (recordLaunchTakes, then tellLaunchTook), a launch's
// replacement as the round's own (roundPodEndsAtLaunch), and a round's replacement by the
// runtime's call, whose last word comes under it (recordedDue). So a turn another holds is an act
// that may be under way, and its record stands for a later sweep; a turn that is free is one under
// which no act of the record's runs, and what the runtime reads then is how the act ended. A record
// is forgotten under that turn, in a short write that waits for no turn.
//
//   - A round's record that a launch made as the round's own (viaLaunch) is the walk's to finish,
//     while this daemon remembers that launch (launchReplaced, endRoundALaunchReplaced).
//   - A turn another holds, a read that fails, and an act the runtime cannot say was made leave
//     the record standing.
//   - An act not made — its target still in use: the issue's Sandbox runs the pod as its own, or
//     the Sandbox of the volume runs a live pod of its own on it — is forgotten, and the round
//     decides again in this sweep's walk, from a new read.
//   - An act made was made by the recorded replacement: its suspension landed, whether or not the
//     call answered so. The round ends as that replacement, from the read made before it
//     (roundReplaced), with the members that had not answered named, and the architect is told
//     what it took. A launch's record tells the architect in the launch's words. Each tells from
//     the read that tells most of the records its finish takes (below).
//
// The sweep finishes each target once (the Legion reviewer's Important 3 on #5285 at acda4d1e). It
// decides the records newest first, but every record of a launch's delete of the issue's Sandbox
// over a live pod of it (store.PodReplacement.WithPod) before every other record, whatever their
// ages (sweepOrder): such a record names that pod beside its own target, and the act that took the
// pod is the launch's, so it decides the pod before a round's record of it does (the Legion
// reviewer's Important on #5285 at 69e00593). A target is taken once, so the finish of the record
// that decides it takes every record of a target its act took (store.PodReplacement.Targets), with
// one notice, in the deciding record's words, from the read of all the records it took that tells
// most: a read that found work that no branch or tag on origin holds over one that failed, a read
// that failed over one that found nothing, and of reads that weigh the same the newest
// (tellingRead; the Legion reviewer's item 2 on #5285 at 69e00593). The newest record is not
// always the last read before the take: a replacement by issue reads outside the pod's launch turn
// and records before it waits for that turn, so its record, over a read that failed, can be newer
// than a launch's whose read named commits. A later record of a target this sweep has decided is
// decided by what that deciding record did:
//
//   - Gone: the deciding record's finish took it. A launch's record, or a record of the round that
//     ended, went with it. A round's record that a launch's finish took ends its round as the act
//     that took its pod, with no notice of its own, right after that finish (endTakenRounds, which
//     a launch's own finish outside a sweep calls too): the launch's notice is the one notice of
//     the act. A round that is over already is ended no second time, and the later record is only
//     logged when the sweep reaches it.
//   - Stands: the deciding record stands (its turn is held, its read failed, the runtime cannot say
//     whether its act was made, or the store refused its finish), so the later record stands with
//     it, and holds its round while that round is not over.
//   - The walk's: the walk ends the target's round as a launch's replacement, so the later record
//     is the walk's too.
//
// A later record that the deciding finish did not take, since it was gone already, is decided
// alone, and so is a record whose deciding record's act was not made.
//
// A runtime that reads no act leaves every record standing; such a runtime replaces no pod.
func (s *supervision) finishReplacements(ctx context.Context, claims []supervise.Claim) (map[string]bool, error) {
	standing, err := s.supervisor.store.PodReplacements(ctx, s.plan.project)
	if err != nil {
		return nil, err
	}
	held := map[string]bool{}
	decided := map[actKey]targetDecided{}
	// went is every record a finish at this sweep took, by operation; a launch's finish ended the
	// round of each round's record it took (endTakenRounds).
	went := map[string]bool{}
	for _, r := range sweepOrder(standing) {
		if ctx.Err() != nil {
			return held, ctx.Err()
		}
		var rec replacementRecord
		if err := json.Unmarshal(r.Record, &rec); err != nil {
			s.log.Error("supervise: the record of a replacement of an issue's pod is one this daemon cannot read; it stands",
				"issue", r.Issue, "pod", r.PodUID, "operation", r.Operation, "error", err)
			continue
		}
		ofRound := rec.Round != ""
		// holds is whether the record holds its issue's rounds at this sweep while it stands: a
		// record whose round is over holds nothing, since no round is left to decide (roundOver).
		holds := ofRound && !s.roundOver(r.Issue, claimsOf(claims, r.Issue), rec.Round)
		attrs := []any{"issue", r.Issue, "pod", r.PodUID, "operation", r.Operation, "round", rec.Round}
		if went[r.Operation] {
			s.log.Info("supervise: the finish of a record decided at this sweep took this record of a replacement of the issue's pod with it", attrs...)
			continue
		}
		keys := targetKeys(r)
		if was, ok := decidedOf(decided, keys); ok {
			switch {
			case was.walk:
				s.log.Info("supervise: the walk ends the round of a record of the same target decided at this sweep as a launch's replacement; this record of a replacement of the issue's pod is the walk's too", attrs...)
				continue
			case !was.finished:
				s.log.Info("supervise: a record of the same target decided at this sweep stands; this record of a replacement of the issue's pod stands with it", attrs...)
				held[r.Issue] = held[r.Issue] || holds
				continue
			}
		}
		if ofRound && rec.By == viaLaunch.String() {
			if _, replaced := s.launchReplaced(r.Issue, rec.Round); replaced {
				decide(decided, keys, targetDecided{walk: true})
				continue
			}
		}
		state, err := s.readAct(ctx, r)
		switch {
		case errors.Is(err, runtime.ErrTurnBusy):
			s.log.Info("supervise: an act on the issue's pod holds its launch turn; the record of a replacement of the pod stands for a later sweep", attrs...)
			held[r.Issue] = held[r.Issue] || holds
			decide(decided, keys, targetDecided{})
		case err != nil:
			if ctx.Err() == nil {
				s.log.Error("supervise: read whether a recorded replacement took the issue's pod, or forget the record of one that took nothing; its record stands",
					append(attrs, "error", err)...)
			}
			held[r.Issue] = held[r.Issue] || holds
			decide(decided, keys, targetDecided{})
		case state == runtime.ActUncertain:
			s.log.Info("supervise: the runtime cannot say whether a recorded replacement took the issue's pod; its record stands", attrs...)
			held[r.Issue] = held[r.Issue] || holds
			decide(decided, keys, targetDecided{})
		case state == runtime.ActNotMade:
			s.log.Info("supervise: a recorded replacement of the issue's pod took nothing: its target is still in use, the pod running as the issue's own or the Sandbox of the volume running a pod on it; its record is forgotten", attrs...)
		case !ofRound:
			taken := rec.taken(r)
			finish, err := s.finishReplacement(ctx, r.Operation, r.Issue, r.PodUID, false, launchMessage(taken), "claim", taken.Claim, "why", taken.Why)
			decideFinish(decided, went, keys, targetDecided{finished: err == nil && finish.Found}, finish)
			s.endTakenRounds(ctx, r.Issue, finish.Took)
		default:
			s.log.Info("supervise: a recorded replacement of the issue's pod took it: its call did not answer so, its daemon stopped, or the store refused the architect's workspace-taken notice; its round, where it is not over, ends as that replacement, from the read made before it",
				attrs...)
			finish, ended := s.endRecordedRound(ctx, s.noticeGroup(claimsOf(claims, r.Issue)), r, rec)
			if !ended {
				held[r.Issue] = held[r.Issue] || holds
			}
			decideFinish(decided, went, keys, targetDecided{finished: ended}, finish)
		}
	}
	return held, nil
}

// sweepOrder is standing, which the store reads oldest first, in the order the sweep decides it
// (finishReplacements): newest first, with every record of a launch's delete of the issue's Sandbox
// over a live pod of it (store.PodReplacement.WithPod) before every other record, whatever their
// ages.
func sweepOrder(standing []store.PodReplacement) []store.PodReplacement {
	order := slices.Clone(standing)
	slices.Reverse(order)
	slices.SortStableFunc(order, func(a, b store.PodReplacement) int {
		switch {
		case a.WithPod == b.WithPod:
			return 0
		case a.WithPod:
			return -1
		}
		return 1
	})
	return order
}

// actKey is one target of an issue's acts: what an act takes and the uid of what it takes.
type actKey struct {
	issue string
	act   runtime.RecordedAct
}

// targetDecided is what a sweep decided of a target from the record that decided it
// (finishReplacements): whether its finish committed, which is false for a record that stands, and
// whether the walk ends the round as a launch's replacement (walk).
type targetDecided struct {
	finished bool
	walk     bool
}

// targetKeys is the key of every target the act r records takes (store.PodReplacement.Targets),
// the one rule the store's finish reads too.
func targetKeys(r store.PodReplacement) []actKey {
	targets := r.Targets()
	keys := make([]actKey, len(targets))
	for i, act := range targets {
		keys[i] = actKey{issue: r.Issue, act: act}
	}
	return keys
}

// decidedOf is what the sweep decided of the first of keys it decided, and whether it decided any.
func decidedOf(decided map[actKey]targetDecided, keys []actKey) (targetDecided, bool) {
	for _, key := range keys {
		if was, ok := decided[key]; ok {
			return was, true
		}
	}
	return targetDecided{}, false
}

// decide records was as what the sweep decided of each of keys that it has not decided yet: the
// first record that decides a target decides it for the sweep.
func decide(decided map[actKey]targetDecided, keys []actKey, was targetDecided) {
	for _, key := range keys {
		if _, ok := decided[key]; !ok {
			decided[key] = was
		}
	}
}

// decideFinish records was, the finish of a record, as what the sweep decided of each of keys
// (decide), and each record the finish took as one that went with it (went).
func decideFinish(decided map[actKey]targetDecided, went map[string]bool, keys []actKey, was targetDecided, finish store.PodReplacementFinish) {
	decide(decided, keys, was)
	for _, r := range finish.Took {
		went[r.Operation] = true
	}
}

// readAct is the runtime's read of the act that r records (runtime.ActReader), under the pod's
// launch turn, where the record is forgotten in the same turn if the act was not made
// (store.Store.ForgetPodReplacement). A runtime that reads no act answers runtime.ActUncertain.
func (s *supervision) readAct(ctx context.Context, r store.PodReplacement) (runtime.ActState, error) {
	reader, ok := s.runtime.(runtime.ActReader)
	if !ok {
		return runtime.ActUncertain, nil
	}
	state := runtime.ActUncertain
	err := reader.ReadAct(ctx, r.Issue, r.Act, func(ctx context.Context, read runtime.ActState) error {
		state = read
		if read != runtime.ActNotMade {
			return nil
		}
		return s.supervisor.store.ForgetPodReplacement(ctx, s.plan.project, r.Issue, r.Operation)
	})
	return state, err
}
