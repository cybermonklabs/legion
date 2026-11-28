package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/roleholder"
	"github.com/sjawhar/envoy/subjectforms"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// noticeExceptionSubjects is where the Envoy listener reports a role-lane forward it could not
// complete: notifications.envoy.exceptions.<the original role topic>
// (packages/envoy/cmd/listener/delivery.go, publishDeliveryException). A role topic is outside the
// notification stream, so the report goes over core NATS and reaches only a subscriber connected
// when it is sent. The role topic has two forms while every reader takes both
// (dispatch://INFRA-3862), the role alone and its project first, so the subscription takes every
// name under the role lanes' prefix, which the daemon's grant names whole (natsgrant.Daemon), and
// exceptionNotice reads either form.
const noticeExceptionSubjects = "notifications.envoy.exceptions." + notify.RoleTopicPrefix + ">"

// errNoticeException is a report the listener sent that cannot be read as the failed forward of
// a notice.
var errNoticeException = errors.New("malformed role-lane exception")

// roleLaneException is the listener's report of one failed role-lane forward, the payload of its
// exception envelope: the original envelope's topic, summary, payload and dedupe key, the session
// the forward went to, and why it failed.
type roleLaneException struct {
	OriginalTopic    string `json:"original_topic"`
	EventID          string `json:"event_id"`
	Reason           string `json:"reason"`
	RecipientSession string `json:"recipient_session"`
	PayloadSummary   string `json:"payload_summary"`
	Payload          string `json:"payload"`
	DedupeKey        string `json:"dedupe_key"`
}

// subscribeNoticeExceptions subscribes rehold to every role-lane exception on conn.
func subscribeNoticeExceptions(conn *nats.Conn, rehold func(data []byte)) (*nats.Subscription, error) {
	sub, err := conn.Subscribe(noticeExceptionSubjects, func(msg *nats.Msg) { rehold(msg.Data) })
	if err != nil {
		return nil, fmt.Errorf("subscribe to role-lane exceptions: %w", err)
	}
	return sub, nil
}

// rehold queues a notice again when a listener of a release before the confirmed publish route
// (packages/envoy/cmd/listener/confirmed.go) reports that its forward to the owning architect's
// session did not complete after it answered the publish 200: the forward failed
// (`delivery_failed`), the holder lapsed between the two (`no_holder`), or the session the
// listener forwarded to did not confirm it in time (`receipt_timeout`). No deployed listener
// answers `receipt_timeout` any more: no session confirms a forward, and every forward the stream
// did not store is `delivery_failed` (dispatch://INFRA-771). The reason is still read here because
// a deployment can roll its listener back to an older build, which raises it. A listener before
// the confirmed route sends a role message on its role lane, which forwards it once and reports a
// failure on noticeExceptionSubjects instead of answering it, and the row has finished by then, so
// the report is answered with a copy of its own (reholdNotice). A listener of the confirmed route
// answers the publish itself, and the notice executor keeps the row from that answer and runs it
// again (noticeAnswered). The subscription stays while a deployment may run an earlier listener.
//
// rehold reads the report: an envelope the listener itself sent, whose payload names a reason it
// knows and a notice of this daemon's outbox to an architect of this project (exceptionNotice). A
// report about another daemon's publish, another project, a role that is not an architect, or
// anything but a notice changes nothing. The exception lane has no redelivery, so a report that
// cannot be read is logged by the caller and dropped.
//
// The listener also reports a publish it refused before forwarding anything, because no live
// session held the role (`unclaimed`, `holder_lapsed`), and the executor's own notice publishes
// are among those. Such a report queues nothing and logs nothing above debug: the publish was
// answered 404, so its row is still held and retries on its own backoff, and a copy would send
// the notice twice. Any reason other than these five is refused as malformed.
func (r *outbox) rehold(ctx context.Context, data []byte) error {
	if r.supervisor == nil {
		return errors.New("notice re-hold has no claim supervisor")
	}
	var envelope struct {
		EventID string `json:"event_id"`
		Source  string `json:"source"`
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("%w: decode the envelope: %w", errNoticeException, err)
	}
	if envelope.Source != "envoy" || strings.TrimSpace(envelope.EventID) == "" {
		return fmt.Errorf("%w: envelope %q from source %q", errNoticeException, envelope.EventID, envelope.Source)
	}
	var exception roleLaneException
	if err := json.Unmarshal([]byte(envelope.Payload), &exception); err != nil {
		return fmt.Errorf("%w: decode exception %s: %w", errNoticeException, envelope.EventID, err)
	}
	reported, ok := r.exceptionNotice(exception)
	if !ok {
		return nil
	}
	if roleholder.IsNoHolderReason(exception.Reason) {
		// The listener refused this publish itself, before forwarding anything, because no live
		// session held the role, and answered it 404 (notify.ErrNoHolder). The executor that sent it
		// still holds the row and retries it on its own backoff (errNoticeWaits), or finishes it once
		// the architect will not hold its role again, so a copy queued here would send the notice
		// twice. These reasons are never a re-hold, and they are not malformed: a positive match on
		// the shared list the listener and notify.Publish both read (roleholder), so the switch below
		// still refuses every other reason. It logs at debug alone: at hundreds of unheld roles,
		// anything louder is a line every few hundred milliseconds about a state the executor
		// already handles.
		r.log.Debug("outbox notice not re-held: the listener refused its publish, and its row retries on its own",
			"issue", reported.issue, "kind", reported.notice.Kind, "topic", exception.OriginalTopic,
			"key", exception.DedupeKey, "reason", exception.Reason)
		return nil
	}
	switch exception.Reason {
	case "delivery_failed", "no_holder", "receipt_timeout":
	default:
		return fmt.Errorf("%w: exception %s names reason %q", errNoticeException, envelope.EventID, exception.Reason)
	}
	if err := r.reholdNotice(ctx, reported, exception.Reason, exception.RecipientSession); err != nil {
		return fmt.Errorf("re-hold the notice of exception %s: %w", envelope.EventID, err)
	}
	return nil
}

// noticeAnswered is what the listener's answer to one notice publish decides for its row:
// published is that answer, payload the row's own Notice, which gives the notice as it went out
// and its dedupe key (Notice.Published), and tree the snapshot the row was routed against.
//
// A publish the listener took writes one line naming the architect it reached (noticePublished).
//
// A delivery its architect's session did not confirm (504 `receipt_timeout`, which only a listener
// rolled back to a build before dispatch://INFRA-771 answers) keeps its row: the row waits for its
// architect (roleWait) and is published again on the capped backoff of a role wait, under its own
// id and key, with the claims a catch-up holds on the decisions it names and the moments its
// pacing reserved, until a publish is taken, which tells those decisions once (toldKept), or its
// architect will not hold its role again (architectEnded), which finishes it undelivered as a role
// no session holds does. No copy is queued and no count of publishes gives it up
// (dispatch://INFRA-4131/spec#b-r-never-given-up). A holder that does not advertise the notice's
// delivery mode, and a forward the listener's own NATS grant denies, refuse every attempt until
// something outside this daemon changes, so each finishes undelivered with one log line rather
// than holding every later notice to that architect behind the fence. A holder only unresponsive —
// its session past the listener's live window and still inside its gone horizon — holds the role
// yet, so its notice is kept whole, and the row waits for its architect (roleWait), tried again on
// the capped backoff of a role wait until the session's heartbeats land, which is what reaches a
// session stalled under box load once it answers again (dispatch://INFRA-392,
// dispatch://INFRA-4131/spec#b-r-capped). A forward the listener could not make at all (504
// `delivery_failed`) waits the same way, since the next attempt can make it. Each is logged as the
// wait it is (settle's logWait), naming the architect and the listener's answer: a wait costs no
// error line per attempt, where a publish that did fail still costs one. A role no session holds
// is held for its architect while that architect's claim can hold it again (errNoticeWaits, a role
// wait too), and finishes undelivered once nobody will. Any other failure, a publish that reached
// no answer of the listener's, is the row's, retried on the outbox's backoff.
func (r *outbox) noticeAnswered(ctx context.Context, row record.OutboxRow, payload record.Notice,
	tree treeSnapshot, architect claim.Token, view supervise.ClaimView, published error,
) error {
	// The dedupe key the publish went out under (Notice.Published), derived here rather than passed
	// in, so no caller can hand this answer one notice's telling under another's key.
	_, key := payload.Published(row.ID)
	var notDelivered notify.ErrNotDelivered
	var refused notify.ErrRefusedByHolder
	var denied notify.ErrPublishDenied
	switch {
	case published == nil:
		r.noticePublished(row, payload, architect)
		// The architect has the catch-up and reads its whole context from it, so this attempt is
		// the wake the pacing held the row for, and the line it writes is written here: once for
		// the row, whatever the attempts before it met (wakeRan).
		if !payload.PacedAt.IsZero() {
			r.wakeRan(row, payload)
		}
		// The decisions the catch-up named and this row claims are told: the telling is recorded
		// under the key it went out on, before anything forgets them, and the settle after this
		// run forgets them (toldKept, settleTold).
		if payload.CatchUp != nil && payload.CatchUp.Kept != nil {
			return r.toldKept(ctx, tree.root.Key, key)
		}
		return nil
	case errors.As(published, &notDelivered) && notDelivered.Reason == notify.ReasonReceiptTimeout:
		// The architect's session did not confirm the notice, and nothing says whether it took it.
		// The row stays as it is, its claims and its reserved moments with it, and runs again until
		// a publish is taken; a claim that ended while the listener waited for the receipt ends it
		// here, as a role nobody holds does.
		if ended := architectEnded(tree, r.claimState(architect)); ended != "" {
			return r.noticeArchitectEnded(ctx, row, payload, tree, architect, ended)
		}
		return r.roleWait(notify.RoleTopicPrefix+string(architect), view, published,
			wait.Errorf("the notice for %s waits for its architect %s, whose session did not confirm it: %w",
				row.Issue, architect, published))
	case errors.As(published, &refused):
		r.log.Warn("outbox notice finished undelivered: its architect's session does not advertise the delivery mode",
			"row", row.ID, "kind", payload.Kind, "issue", row.Issue, "architect", architect, "holder", refused.Holder, "error", published)
		return nil
	case errors.As(published, &denied):
		r.log.Warn("outbox notice finished undelivered: the listener's NATS grant denied the forward to its architect",
			"row", row.ID, "kind", payload.Kind, "issue", row.Issue, "architect", architect, "reason", denied.Message)
		return nil
	case errors.Is(published, notify.ErrHolderUnresponsive):
		return r.roleWait(notify.RoleTopicPrefix+string(architect), view, published,
			wait.Errorf("the notice for %s waits for its architect %s, whose session the listener reports %s: %w",
				row.Issue, architect, notify.StateUnresponsive, published))
	case errors.As(published, &notDelivered):
		// The listener could not forward the notice at all (`delivery_failed`): no live holder to
		// forward to, a read that failed or ran out of time, a forward not shown to reach the
		// server. It is no notice held for an architect that holds no role (errNoticeWaits), and the
		// row keeps its own place: no copy is queued, as none is for `receipt_timeout`.
		return r.roleWait(notify.RoleTopicPrefix+string(architect), view, published,
			wait.Errorf("the notice for %s waits for its architect %s, to whom the listener could not forward it: %w",
				row.Issue, architect, published))
	case !errors.Is(published, notify.ErrNoHolder):
		return fmt.Errorf("publish notice for %s to its architect %s: %w", row.Issue, architect, published)
	}
	if ended := architectEnded(tree, r.claimState(architect)); ended != "" {
		return r.noticeArchitectEnded(ctx, row, payload, tree, architect, ended)
	}
	return r.roleWait(notify.RoleTopicPrefix+string(architect), view, published,
		fmt.Errorf("%w: %s holds no role for %s: %w", errNoticeWaits, architect, row.Issue, published))
}

// noticeArchitectEnded ends a notice row undelivered because its owning architect will not hold
// its role again, ended being why (architectEnded, read once by the caller so the reason it judged
// by is the one relayed and logged): before the publish when roleWaitGate finds no machine or a
// failed or retired claim (dispatch://INFRA-4131/spec#b-r-wait-ready), or after a publish refused
// notify.ErrNoHolder or not confirmed (notify.ReasonReceiptTimeout) when the claim ended in between
// or the tree lingers. It relays any person's spec decision to the controller (parkDecision) and
// any kept-pod disk notice (tellControllerOfKeptPod), then logs the one Info line.
func (r *outbox) noticeArchitectEnded(ctx context.Context, row record.OutboxRow, payload record.Notice,
	tree treeSnapshot, architect claim.Token, ended string,
) error {
	notice, key := payload.Published(row.ID)
	if err := r.parkDecision(ctx, row.Issue, tree.root.Key, payload, key, row.ID); err != nil {
		return err
	}
	if err := r.tellControllerOfKeptPod(ctx, row.Issue, row.ID, notice, key, architect, ended); err != nil {
		return err
	}
	r.log.Info("outbox notice finished undelivered: its architect will not hold its role again",
		"row", row.ID, "kind", payload.Kind, "issue", row.Issue, "architect", architect, "because", ended)
	return nil
}

// noticePublished is the one line a notice the listener took writes: its kind, its issue, its row
// and the architect claim it reached, and for a catch-up the generation it states, the launch it
// was written for and how many issues it lists. It is written on the attempt that publishes, so
// once per publish: a row that waited or failed before writes nothing for those attempts, and a
// copy a re-hold queues is a row of its own, which writes its own line when it publishes. It is
// how the daemon's log shows which architect each notice reached, a sub-architect's catch-up at
// its launch among them.
func (r *outbox) noticePublished(row record.OutboxRow, payload record.Notice, architect claim.Token) {
	fields := []any{"row", row.ID, "kind", payload.Kind, "issue", row.Issue, "architect", architect}
	if payload.CatchUp != nil {
		fields = append(fields, "generation", payload.CatchUp.Generation, "launch", payload.CatchUp.Launch, "issues", len(payload.CatchUp.Issues))
	}
	r.log.Info("outbox notice published", fields...)
}

// reholdNotice queues reported's notice again after an earlier listener reported that its
// architect's session did not take a publish the listener had answered 200 (rehold): the publish,
// under the dedupe key of reported's row (record.OutboxKey), went to the architect's role topic,
// the listener named reason, and recipient is the session it forwarded to ("" when it named none).
// Only a listener before the confirmed publish route, older than legion-envoy-v22.3.0, reports a
// daemon publish so; a listener of that route answers the publish itself, and the notice executor
// keeps the row from that answer (noticeAnswered). A session that stopped running but is still
// registered produced a receipt_timeout there: the listener forwarded to it and waited for a
// receipt that never came. So does the relaunch window, until the claim's agent has taken the
// Envoy role back. Every such failure is queued, a late receipt from the claim's own session too: a
// Go relaunch resumes the claim's session id, so a failure naming it cannot tell the stopped
// process from the relaunched one, and a copy the session already has costs one frame its plugin
// drops, where a skipped copy the relaunched process never saw is a notice lost.
//
// A report whose key names a row the outbox still holds queues nothing: that row runs again by
// itself, on its own wait, and a copy beside it would send the notice twice. Such a report comes
// while a deployment runs listeners of both kinds, one answering a publish receipt_timeout and the
// other reporting its own forward of the same row, or for a publish whose 200 the daemon lost.
//
// The notice goes back through the executor as a new row of its issue, counted in its Resends,
// so it is routed, fenced and held as any notice is, and it is published under the dedupe key of
// the row it copies (Notice.Published), so a session that did get the forward recognises the copy.
// A copy carries what the publish carried and nothing more: no moment the wake pacing reserved,
// since that publish was answered and wrote its wake line already, and no claim on a decision a
// catch-up names, since the publish told it (toldKept, settleTold). The copies have no limit
// (dispatch://INFRA-4131/spec#b-r-never-given-up): each is due at once when copyDueAtOnce says
// so, and otherwise after the capped backoff of the copies sent before it (record.RoleWaitBackoff
// of Resends: 1 s, doubling, to RoleWaitBackoffMost). Either way it waits for its architect's ready
// (record.WaitForReady), so the architect's next ready makes it due at once (outbox.releaseReady)
// and a turn start of a session that took nothing does not, and it ends, as every notice ends,
// once its architect will not hold its role again (the executor's architectEnded). Each copy is a
// row of its own, with a wait and lines of its own. Each published copy of a row is queued again
// once, marked processed under the row's dedupe key and the copy's count: a publish whose 200 was
// lost is retried under the same key, and an earlier listener reports each failed forward under a
// fresh event id.
//
// The publish's row finishes once the listener answers, and an earlier listener reports a failed
// forward only after its receipt window (two seconds, longer while forwards queue behind others on
// its role lane). A notice to the same architect written before the copy is queued is not held
// behind the failed one, so it can arrive before the copy. A catch-up a newer one superseded
// (catchUpSuperseded) is not queued again: its copy would arrive after the fresh catch-up, and its
// own publish already told what it named. Nor is a notice whose architect stopped with its finished
// tree (stoppedWithTree): nothing of the close is sent to it, and a person's spec decision such a
// notice reports goes to the controller instead (parkDecision), as does a notice of the issue
// pod's disk (tellControllerOfKeptPod). Those checks read the claim from memory, outside
// ApplyFact's lock, which the ready handler holds, so a copy can still lose the race to a ready;
// the notice executor checks again when it runs the row.
func (r *outbox) reholdNotice(ctx context.Context, reported reportedNotice, reason, recipient string) error {
	key := record.OutboxKey(reported.row)
	issue, notice := reported.issue, reported.notice
	topic := notify.RoleTopicPrefix + string(reported.architect)
	kept, err := store.ReadOnly(ctx, r.pool, "read whether the outbox still holds the reported notice",
		func(ctx context.Context, tx pgx.Tx) (bool, error) {
			return r.records.NoticeQueued(ctx, tx, reported.row)
		})
	if err != nil {
		return err
	}
	if kept {
		r.log.Info("outbox notice not re-held: its row is still in the outbox and runs again by itself",
			"row", reported.row, "issue", issue, "kind", notice.Kind, "topic", topic, "key", key, "reason", reason)
		return nil
	}
	held := r.supervisedClaim(reported.architect)
	if !holdsRole(held) {
		root, lingers, err := r.issueTree(ctx, issue)
		if err != nil {
			return err
		}
		if stoppedWithTree(lingers, held) {
			if err := r.parkDecision(ctx, issue, root, notice, key, reported.row); err != nil {
				return err
			}
			if err := r.tellControllerOfKeptPod(ctx, issue, reported.row, notice, key, reported.architect, "it stopped with its finished tree"); err != nil {
				return err
			}
			r.log.Info("outbox notice not re-held: its architect stopped with its finished tree",
				"issue", issue, "kind", notice.Kind, "topic", topic, "key", key, "reason", reason, "state", held.State, "stopping", held.Stopping)
			return nil
		}
	}
	due := r.now().Add(record.RoleWaitBackoff(notice.Resends))
	if copyDueAtOnce(held.Claim, notice.Resends, recipient) {
		due = r.now()
	}
	mark := fmt.Sprintf("%s#%d", key, notice.Resends)
	notice.Resends++
	notice.ResendOf = reported.row
	row, err := record.NewOutboxRow(issue, notice, due)
	if err != nil {
		return fmt.Errorf("the copy is a notice the outbox refuses: %w", err)
	}
	row.Wait.WaitFor = record.WaitForReady(reported.architect)
	superseded := false
	// The re-hold is its own transaction, so it owns the commit hook the row's write registers
	// (projectRecords, store.OnCommit), as ApplyFact does for a fact's rows, and the hook that
	// logs the copy it queued.
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		if notice.CatchUp != nil {
			recorded, err := r.records.Issue(ctx, tx, issue)
			if err != nil {
				return err
			}
			if superseded = catchUpSuperseded(*notice.CatchUp, held.Claim, recorded); superseded {
				return nil
			}
		}
		fresh, err := r.records.MarkProcessed(ctx, tx, "envoy-exception", mark)
		if err != nil || !fresh {
			return err
		}
		store.OnCommit(ctx, func() {
			r.log.Info("outbox notice re-held: the listener could not forward it to its architect's session",
				"issue", issue, "kind", notice.Kind, "topic", topic, "key", key, "reason", reason)
		})
		return r.records.Enqueue(ctx, tx, row)
	}); err != nil {
		return err
	}
	if superseded {
		r.log.Info("outbox catch-up not re-held: a newer catch-up supersedes it",
			"issue", issue, "key", key, "generation", notice.CatchUp.Generation, "launch", notice.CatchUp.Launch, "reason", reason)
	}
	return nil
}

// issueTree is the root of the tree of issue, as recorded, and whether that tree lingers or has
// closed. An issue that is not recorded has no tree to finish.
func (r *outbox) issueTree(ctx context.Context, issue string) (root string, lingers bool, err error) {
	err = store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		recorded, err := r.records.Issue(ctx, tx, issue)
		if err != nil || recorded == nil {
			return err
		}
		root = recorded.Tree
		lingers, err = record.TreeLingers(ctx, r.records, tx, recorded.Tree)
		return err
	})
	if err != nil {
		return "", false, fmt.Errorf("read the tree of %s: %w", issue, err)
	}
	return root, lingers, nil
}

// catchUpSuperseded says whether a newer catch-up than catchUp exists for its architect, the
// architect of the catch-up's own issue: held, that claim as this daemon supervises it, has said it
// is ready at a later launch than the one catchUp was written for, and that launch's ready wrote
// one (workflow's claimReady), queued or delivered; or issue, the catch-up's own issue as recorded
// now, is at a later generation — a root re-admitted, or a child re-entered — whose start writes
// one, whether it finds the claim running or relaunches it. Neither holds for a catch-up that may
// be the architect's only one: a claim relaunching or not yet ready has had no later ready, and
// one its later ready will write drops the queued copy. The newer catch-up was composed from what
// nobody claimed, so the superseded one's decisions are its own to hand over rather than leave
// with its row (the outbox's notice, workflow.HandOverKept).
func catchUpSuperseded(catchUp record.CatchUp, held supervise.Claim, issue *record.Issue) bool {
	if supervise.SaidReady(held.State) && held.Generation > catchUp.Launch {
		return true
	}
	return issue != nil && issue.Generation > catchUp.Generation
}

// copyDueAtOnce says whether the next copy of a notice an earlier listener's role-lane exception
// reported (reholdNotice) is due at once, given held, the architect's claim as this daemon
// supervises it (the zero Claim when it does not), resends, the copies of the notice already sent,
// and recipient, the session the report names. A copy is due at once when the architect's agent
// has said it is ready (supervise.SaidReady), and it is the first copy or its report names a
// session other than the one the claim is ready on: that ready has passed, so the ready key the
// copy waits for (record.WaitForReady, which outbox.releaseReady makes due) is not met again before
// the claim's next ready, and every later notice to the architect would wait behind the copy. The
// first covers a relaunch that reported ready inside the receipt window; a later copy that failed
// on another session went where the role no longer is. A later copy whose report names no session
// (the listener names none for no_holder, or when it could not read the holder) says nothing about
// where the role went. It waits its delay (record.RoleWaitBackoff of resends), as does a later copy
// that failed on the claim's own ready session and every copy while the claim has not reported
// ready, so a claim that reads live while its session hears nothing (a reconnecting connection, a
// death the supervisor has not seen) is sent its copies on the capped backoff, at most 12 an hour,
// rather than one every few seconds.
func copyDueAtOnce(held supervise.Claim, resends int, recipient string) bool {
	if !supervise.SaidReady(held.State) {
		return false
	}
	return resends == 0 || recipient != "" && !held.HoldsSession(recipient)
}

// reportedNotice is the notice a failed forward was, as exceptionNotice reads it from an earlier
// listener's report: its issue, the notice as it was published, the outbox row its dedupe key
// names, and the architect whose role topic it went to.
type reportedNotice struct {
	issue     string
	notice    record.Notice
	row       int64
	architect claim.Token
}

// exceptionNotice reads the notice a report is about: a publish of this daemon's outbox (its
// dedupe key names a row) to the role topic of an architect of this project, in either form of
// the lane's name (the role alone, or this daemon's Dispatch project and then the role), whose
// summary is the executor's own "<kind> on <issue>" for an issue of this project and whose payload
// is that notice. ok is false for a report about anything else, a lane of another project
// among them.
func (r *outbox) exceptionNotice(exception roleLaneException) (reportedNotice, bool) {
	row, ok := record.ParseOutboxKey(exception.DedupeKey)
	if !ok {
		return reportedNotice{}, false
	}
	lane, isRole := strings.CutPrefix(exception.OriginalTopic, notify.RoleTopicPrefix)
	if !isRole {
		return reportedNotice{}, false
	}
	project, token, legacy := subjectforms.CanonicalRole(lane)
	if (!legacy && project != r.dispatchProject) || !strings.HasPrefix(token, "legion-"+r.project+"-") || !strings.HasSuffix(token, "-"+string(claim.RoleArchitect)) {
		return reportedNotice{}, false
	}
	var notice record.Notice
	decoder := json.NewDecoder(strings.NewReader(exception.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&notice); err != nil {
		return reportedNotice{}, false
	}
	kind, issue, found := parseNoticeSummary(exception.PayloadSummary)
	if !found || kind != notice.Kind || !claim.IsIssueKey(issue) || !strings.HasPrefix(issue, r.dispatchProject+"-") {
		return reportedNotice{}, false
	}
	return reportedNotice{issue: issue, notice: notice, row: row, architect: claim.Token(token)}, true
}
