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
// read through the pod's launcher). The read is what it found, or Failed, the error it failed with
// as the store keeps a far side's error (storetext.FarSideErrorEnds).
type replacementRecord struct {
	Round   string      `json:"round,omitempty"`
	Reasons string      `json:"reasons,omitempty"`
	By      string      `json:"by,omitempty"`
	Claim   claim.Token `json:"claim,omitempty"`
	Why     string      `json:"why,omitempty"`
	Role    claim.Role  `json:"role,omitempty"`
	At      time.Time   `json:"at"`
	Head    string      `json:"head,omitempty"`
	Changed bool        `json:"changed,omitempty"`
	Commits []string    `json:"commits,omitempty"`
	Count   int         `json:"count,omitempty"`
	Failed  string      `json:"failed,omitempty"`
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
		Head: read.work.Head, Changed: read.work.Changed, Commits: read.work.Commits, Count: read.work.Count, Failed: failedText(read.err)})
	if err != nil {
		return store.PodReplacement{}, err
	}
	return store.PodReplacement{Operation: uuid.NewString(), Issue: issue, PodUID: pod, Act: runtime.PodAct(pod), Record: encoded}, nil
}

// launchRecordOf is the record of a launch's own replacement, as the runtime hands it, under the
// operation identity the runtime gave it, with what the act takes, its target, and whether it takes
// the live pod of its Sandbox with the volume (store.PodReplacement.WithPod, which the row holds).
func launchRecordOf(taken runtime.TakenPod) (store.PodReplacement, error) {
	encoded, err := json.Marshal(replacementRecord{Claim: taken.Claim, Why: taken.Why,
		Head: taken.Work.Head, Changed: taken.Work.Changed, Commits: taken.Work.Commits, Count: taken.Work.Count, Failed: failedText(taken.Err)})
	if err != nil {
		return store.PodReplacement{}, err
	}
	return store.PodReplacement{Operation: taken.Operation, Issue: taken.Issue, PodUID: taken.PodUID, Act: taken.Act, WithPod: taken.WithPod,
		Record: encoded}, nil
}

// work is the read the record holds, of pod.
func (r replacementRecord) work(pod string) (runtime.WorkspaceWork, error) {
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

// tellingRead is the read the notice of one act tells, of every record the act's finish took (took,
// oldest first, store.PodReplacementFinish.Took): a read that found work that no branch or tag on
// origin holds tells over one that failed, and a read that failed over one that found nothing, and
// of reads that weigh the same the newest tells (the Legion reviewer's item 2 on #5285 at
// 69e00593). One act can stand under the records of acts made over reads of their own, and the
// newest of them is not always the last read before the take: a replacement by issue reads outside
// the pod's launch turn and records before it waits for that turn, so its record can be newer than
// a launch's whose read named commits. tells is false where every read found nothing: the act owes
// nobody a notice. A record this daemon cannot read tells nothing.
func tellingRead(took []store.PodReplacement) (read recordedRead, tells bool) {
	for _, r := range took {
		var rec replacementRecord
		if json.Unmarshal(r.Record, &rec) != nil {
			continue
		}
		var held recordedRead
		held.work, held.err = rec.work(r.PodUID)
		if held.weight() > 0 && held.weight() >= read.weight() {
			read, tells = held, true
		}
	}
	return read, tells
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
// pod's launch turn, one statement before the suspension. A last word that refuses takes the
// record away again (forgetReplacement): the replacement takes nothing, and the next sweep decides
// the round afresh.
func (s *supervision) recordedDue(recorded store.PodReplacement, due func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
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
// pod with its Sandbox's volume: that record tells the architect, and this one goes alone, with
// nothing queued (roundReplaced). It answers what the finish did, and false where the store
// refused: the records then stand for a later sweep (finishReplacements). A record that the finish
// of another act took, or another finish of the same act, is finished already, and nothing is
// queued for it here (the Legion reviewer's Important 1 on #5285 at 7da847c0). attrs name the
// replacement on its lines.
func (s *supervision) finishReplacement(ctx context.Context, operation, issue, pod string, yield bool, message func(recordedRead) string,
	attrs ...any) (store.PodReplacementFinish, bool) {
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
	finish, err := s.supervisor.store.FinishPodReplacement(ctx, s.plan.project, issue, operation, yield, notice)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			s.log.Error("supervise: finish the record of a replacement of the issue's pod, and tell its architect what the replacement took; the record stands, and a later sweep finishes it",
				append(attrs, "error", err)...)
		}
		return finish, false
	case !finish.Found:
		s.log.Info("supervise: the record of this replacement of the issue's pod is finished already, with the record of another act it took or by another finish of it; nothing is queued for it",
			attrs...)
	case finish.Yielded:
		s.log.Info("supervise: the record of this replacement of the issue's pod yields to the record of a launch's act that took the pod with its Sandbox's volume; it is forgotten, nothing is queued for it, and the launch's record tells the architect",
			attrs...)
	case !noticed:
	case !finish.Told:
		s.log.Info("supervise: the issue's architect is not told what a replacement of its pod took: its issue is another project's, whose rows this daemon writes none of",
			attrs...)
	default:
		s.log.Info("supervise: the issue's architect is told what a replacement of its pod took", attrs...)
	}
	return finish, true
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
func (s *supervision) tellLaunchTook(ctx context.Context, taken runtime.TakenPod) {
	attrs := []any{"claim", taken.Claim, "why", taken.Why}
	if !taken.Tells() {
		s.log.Info("supervise: a launch replaced its issue's pod, and the read of the workspace before it found nothing that no branch or tag on origin holds",
			append([]any{"issue", taken.Issue, "pod", taken.PodUID}, attrs...)...)
		return
	}
	s.finishReplacement(ctx, taken.Operation, taken.Issue, taken.PodUID, false, launchMessage(taken), attrs...)
}

// finishReplacements finishes, at the start of a sweep, every record of a replacement that a call
// or a daemon left standing (store.Store.PodReplacements), from the runtime's read of the act
// (runtime.ActReader), and answers the issues whose round's record still stands, which the walk
// decides nothing of at this sweep: a round there must neither read a pod its replacement took
// again, nor keep it, nor end as a pod that went without its round (GPT-6.1 Sol's point 1 at
// 0259f8af). claims is the sweep's snapshot.
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
//     ended, went with it. A record of another round, which a launch's finish took, ends that
//     round as the act that took its pod, with no notice of its own: the launch's notice is the
//     one notice of the act.
//   - Stands: the deciding record stands (its turn is held, its read failed, the runtime cannot say
//     whether its act was made, or the store refused its finish), so the later record stands with
//     it, and holds its round.
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
	// went is every record a finish at this sweep took, by operation, with what the sweep decided
	// of the record whose finish took it.
	went := map[string]targetDecided{}
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
		attrs := []any{"issue", r.Issue, "pod", r.PodUID, "operation", r.Operation, "round", rec.Round}
		if by, took := went[r.Operation]; took {
			if !ofRound || rec.Round == by.round {
				s.log.Info("supervise: the finish of a record decided at this sweep took this record of a replacement of the issue's pod with it", attrs...)
				continue
			}
			told := s.noticeGroup(claimsOf(claims, r.Issue))
			s.log.Info("supervise: the finish of a launch's act decided at this sweep took this record of the round's replacement of the issue's pod with it; the round ends as that act, with no notice of its own",
				attrs...)
			if _, ended := s.roundReplaced(ctx, r.Issue, rec.Round, r.PodUID, rec.Reasons, roundEnd{}, lostClaims(told, rec.Round), rec.read(r.PodUID), told, r, false); !ended {
				held[r.Issue] = true
			}
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
				held[r.Issue] = held[r.Issue] || ofRound
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
			held[r.Issue] = held[r.Issue] || ofRound
			decide(decided, keys, targetDecided{})
		case err != nil:
			if ctx.Err() == nil {
				s.log.Error("supervise: read whether a recorded replacement took the issue's pod, or forget the record of one that took nothing; its record stands",
					append(attrs, "error", err)...)
			}
			held[r.Issue] = held[r.Issue] || ofRound
			decide(decided, keys, targetDecided{})
		case state == runtime.ActUncertain:
			s.log.Info("supervise: the runtime cannot say whether a recorded replacement took the issue's pod; its record stands", attrs...)
			held[r.Issue] = held[r.Issue] || ofRound
			decide(decided, keys, targetDecided{})
		case state == runtime.ActNotMade:
			s.log.Info("supervise: a recorded replacement of the issue's pod took nothing: its target is still in use, the pod running as the issue's own or the Sandbox of the volume running a pod on it; its record is forgotten", attrs...)
		case !ofRound:
			taken := rec.taken(r)
			finish, finished := s.finishReplacement(ctx, r.Operation, r.Issue, r.PodUID, false, launchMessage(taken), "claim", taken.Claim, "why", taken.Why)
			decideFinish(decided, went, keys, targetDecided{finished: finished}, finish)
		default:
			told := s.noticeGroup(claimsOf(claims, r.Issue))
			s.log.Info("supervise: a recorded replacement of the issue's pod took it, and its round is not over yet: its call did not answer so, its daemon stopped, or the store refused the architect's workspace-taken notice; the round ends as that replacement, from the read made before it",
				attrs...)
			finish, finished := s.roundReplaced(ctx, r.Issue, rec.Round, r.PodUID, rec.Reasons, roundEnd{}, lostClaims(told, rec.Round), rec.read(r.PodUID), told, r, false)
			if !finished {
				held[r.Issue] = true
			}
			decideFinish(decided, went, keys, targetDecided{round: rec.Round, finished: finished}, finish)
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
// (finishReplacements): the round that record ended, "" for a launch's own record; whether its
// finish committed, which is false for a record that stands; and whether the walk ends the round
// as a launch's replacement (walk).
type targetDecided struct {
	round    string
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
func decideFinish(decided map[actKey]targetDecided, went map[string]targetDecided, keys []actKey, was targetDecided, finish store.PodReplacementFinish) {
	decide(decided, keys, was)
	for _, r := range finish.Took {
		went[r.Operation] = was
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
