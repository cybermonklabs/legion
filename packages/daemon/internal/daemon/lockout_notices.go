package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fleetalert"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// lockOutNotices keeps the project controller's notices of R3's lock-outs: a claim whose newer
// process the worker stream has refused for stream.LockOutReportAfter, because an older
// generation's connection will not close, which only a person can end
// (record.WorkerLockedOutNotice). The stream reports a lock-out once (report), and each notice
// is told once whatever the daemon's restarts: it is written with the project's
// record that it was told (store.ReachProjectNotice), so no daemon tells the same lock-out — the
// same newer process refused for the same older one — twice. A notice the controller's lane did
// not queue is kept, in this daemon's memory and in that record, and told again at each minute
// sweep (resend) until it is queued or the stream no longer refuses that newer process.
//
// It reads nothing of a claim's reach, and holds a lock of its own (mu), apart from the reach
// keeper's.
type lockOutNotices struct {
	log     *slog.Logger
	project string
	// tell queues a controller notice on an issue with its marks, in one transaction
	// (workflowRuntime.tellControllerQueued); nil in a deployment with no workflow, which tells
	// nobody. records is the store of the project's own notice records; nil keeps them in this
	// daemon's memory alone. lockedOut is the worker stream's open lock-out of a claim
	// (stream.Listener.LockedOut); nil reads none open. claimOf is the claim the daemon
	// supervises for a token, as its machine holds it. alerts writes R6's lines.
	tell      func(context.Context, record.ControllerNotice, string, []reachMark) (bool, error)
	records   reachNotices
	lockedOut func(claim.Token) (stream.LockOut, bool)
	claimOf   func(claim.Token) (supervise.Claim, bool)
	alerts    *fleetalert.Writer

	mu sync.Mutex
	// loaded is whether this daemon has read the project's notice records yet (load); kept is
	// each notice the controller's lane has not queued yet, by claim; told is the two
	// generations of the lock-out each claim's told notice named.
	loaded bool
	kept   map[claim.Token]untoldLockOut
	told   map[claim.Token][2]uint64
}

// untoldLockOut is a lock-out notice the controller's lane did not queue, of the lock-out that
// names generations newer and older, told again at each sweep while the worker stream still
// refuses that newer process for that older one.
type untoldLockOut struct {
	notice       record.ControllerNotice
	issue        string
	newer, older uint64
}

// lockOutOptions are what lockOutNotices are built from (lockOutNotices says what each is).
type lockOutOptions struct {
	log       *slog.Logger
	project   string
	tell      func(context.Context, record.ControllerNotice, string, []reachMark) (bool, error)
	records   reachNotices
	lockedOut func(claim.Token) (stream.LockOut, bool)
	claimOf   func(claim.Token) (supervise.Claim, bool)
	alerts    *fleetalert.Writer
}

func newLockOutNotices(o lockOutOptions) *lockOutNotices {
	return &lockOutNotices{log: o.log, project: o.project, tell: o.tell, records: o.records, lockedOut: o.lockedOut,
		claimOf: o.claimOf, alerts: o.alerts, kept: map[claim.Token]untoldLockOut{}, told: map[claim.Token][2]uint64{}}
}

// report is the worker stream's report of a claim whose newer process it has refused for
// stream.LockOutReportAfter because an older generation's connection will not close (R3,
// stream.Options.ReportLockOut): only a person can end that process. The stream calls it once per
// lock-out, on a goroutine of its own, so the notice is written here with a budget of its own. A
// claim the daemon does not supervise, and a deployment with no workflow, tell nobody, and the
// stream's own R6 line stands for it.
func (l *lockOutNotices) report(out stream.LockOut) {
	if l.tell == nil {
		l.log.Warn("worker stream: a lock-out has nobody to tell it to", "claim", out.Claim, "newer", out.Newer, "older", out.Older)
		return
	}
	c, ok := l.claimOf(out.Claim)
	if !ok {
		l.log.Warn("worker stream: a lock-out of a claim this daemon does not supervise tells nobody", "claim", out.Claim)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reachNoticeBudget)
	defer cancel()
	l.notify(ctx, out, record.ControllerNotice{Kind: record.WorkerLockedOutNotice, Role: c.Role, Claim: string(out.Claim), Reason: out.Reason()}, c.Issue)
}

// notify tells the controller of the lock-out out, on issue, as notice. One its lane did not
// queue — no controller registered, a notice of the same claim still waiting unsent, a store that
// refused it — is kept, and told again at each sweep (resend): the stream reports a lock-out
// once, so a notice not kept would be lost. Whether it was told already is not known until the
// records are read, so until then it is kept and told at the sweep that reads them.
func (l *lockOutNotices) notify(ctx context.Context, out stream.LockOut, notice record.ControllerNotice, issue string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	untold := untoldLockOut{notice: notice, issue: issue, newer: out.Newer, older: out.Older}
	l.kept[out.Claim] = untold
	if !l.load(ctx) {
		return
	}
	l.tellNow(ctx, out.Claim, untold, true)
}

// resend is the minute sweep's pass over the lock-outs: each kept notice is told again, once the
// project's records are read, and each that is queued now or whose lock-out has ended — the
// stream no longer refuses that newer process for that older one, which is how a daemon that
// restarted finds a notice the last one kept, since every connection of the last daemon ended with
// it — is forgotten. The record of one that ended untold is deleted, since there is nothing to
// keep. R6's line of each lock-out the stream no longer holds is ended too: a line the last daemon
// opened names a connection that ended with it, and this daemon's stream tells that older process
// to stop at its hello rather than binding it.
func (l *lockOutNotices) resend(ctx context.Context) {
	l.mu.Lock()
	if l.load(ctx) {
		for token, untold := range l.kept {
			if !l.open(token, untold) {
				l.log.Info("lock-out: a lock-out ended before the controller's notice of it was queued; it is told no more", "claim", token)
				delete(l.kept, token)
				if l.records != nil {
					if err := l.records.DeleteReachProjectNotice(ctx, l.project, string(record.WorkerLockedOutNotice), token); err != nil && ctx.Err() == nil {
						l.log.Error("lock-out: the record of a lock-out that ended untold was not deleted", "claim", token, "error", err)
					}
				}
				continue
			}
			l.tellNow(ctx, token, untold, false)
		}
	}
	l.mu.Unlock()
	l.alerts.CloseEnded(fleetalert.NewerProcessRefused, func(token string) bool {
		_, locked := l.held(claim.Token(token))
		return locked
	})
}

// tellNow tells untold, the kept notice of token's lock-out, written with its told record. A
// notice of a lock-out already told is forgotten, and one the lane did not queue stays kept,
// written to the project's record as kept where keep says so. The caller holds mu.
func (l *lockOutNotices) tellNow(ctx context.Context, token claim.Token, untold untoldLockOut, keep bool) {
	if l.told[token] == [2]uint64{untold.newer, untold.older} {
		l.log.Info("lock-out: the controller was told of this lock-out already; it is not told again", "claim", token,
			"newer", untold.newer, "older", untold.older)
		delete(l.kept, token)
		return
	}
	kept := store.ReachProjectNotice{Project: l.project, Kind: string(untold.notice.Kind), Claim: token,
		Newer: untold.newer, Older: untold.older, Issue: untold.issue}
	told := kept
	told.Told = true
	if tellQueued(ctx, l.log, l.tell, untold.notice, untold.issue, projectMark(l.log, told, untold.notice)...) {
		l.told[token] = [2]uint64{untold.newer, untold.older}
		delete(l.kept, token)
		return
	}
	if !keep || l.records == nil {
		return
	}
	marks := projectMark(l.log, kept, untold.notice)
	if len(marks) == 0 {
		return
	}
	if err := l.records.PutReachProjectNotice(ctx, *marks[0].project); err != nil && ctx.Err() == nil {
		l.log.Error("lock-out: a lock-out notice the controller's lane did not queue was not kept in the project's record; this daemon still keeps it",
			"claim", token, "error", err)
	}
}

// load reads the project's lock-out records, once, and answers whether they are read: the
// lock-out each claim's told notice named, and each notice kept untold, which this daemon tells
// while its worker stream still refuses the same newer process. Until the records are read, no
// notice is told, since whether one was told is not known; a read that fails is made again at the
// next sweep. The caller holds mu.
func (l *lockOutNotices) load(ctx context.Context) bool {
	if l.loaded {
		return true
	}
	if l.records == nil {
		l.loaded = true
		return true
	}
	records, err := l.records.ReachProjectNotices(ctx, l.project)
	if err != nil {
		if ctx.Err() == nil {
			l.log.Error("lock-out: the project's lock-out records could not be read; no lock-out notice is told until they are", "error", err)
		}
		return false
	}
	for _, n := range records {
		if record.NoticeKind(n.Kind) != record.WorkerLockedOutNotice {
			continue
		}
		if n.Told {
			l.told[n.Claim] = [2]uint64{n.Newer, n.Older}
			delete(l.kept, n.Claim)
			continue
		}
		var notice record.ControllerNotice
		if err := json.Unmarshal(n.Notice, &notice); err != nil {
			l.log.Error("lock-out: a kept lock-out notice did not decode; it is told no more", "claim", n.Claim, "error", err)
			continue
		}
		if _, kept := l.kept[n.Claim]; !kept {
			l.kept[n.Claim] = untoldLockOut{notice: notice, issue: n.Issue, newer: n.Newer, older: n.Older}
		}
	}
	l.loaded = true
	return true
}

// open is whether the worker stream still refuses the newer process untold names for the older
// one it names.
func (l *lockOutNotices) open(token claim.Token, untold untoldLockOut) bool {
	open, locked := l.held(token)
	return locked && open.Newer == untold.newer && open.Older == untold.older
}

// held is the worker stream's open lock-out of token, if it holds one; a stream that is not
// read holds none.
func (l *lockOutNotices) held(token claim.Token) (stream.LockOut, bool) {
	if l.lockedOut == nil {
		return stream.LockOut{}, false
	}
	return l.lockedOut(token)
}

// supervisedClaimOf is sup's claim of a token, as its machine holds it, for the lock-out
// notices' claimOf: false for a claim sup does not supervise.
func supervisedClaimOf(sup *supervisor) func(claim.Token) (supervise.Claim, bool) {
	return func(token claim.Token) (supervise.Claim, bool) {
		m, ok := sup.Machine(token)
		if !ok {
			return supervise.Claim{}, false
		}
		return m.Claim(), true
	}
}
