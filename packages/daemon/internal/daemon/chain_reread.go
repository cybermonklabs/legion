package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// chainRereadFirst and chainRereadMost bound the backoff of the re-read of one chain the copy
// cannot read to its top (rereadChains): its first read follows the note of it at once, and each
// read that leaves it unknown doubles the wait before the next, up to five minutes (decision 20 of
// dispatch://INFRA-4730). The controller tick, an hour apart, is too slow for R16 of
// dispatch://INFRA-3320's "until the chain is known".
const (
	chainRereadFirst = time.Second
	chainRereadMost  = 5 * time.Minute
)

// chainReread is one key the re-read holds: when it is read next, the wait after that read should
// it leave the chain unknown, and the keys whose 404 the re-read has logged for it, so a link
// Dispatch no longer serves is logged once while it is read again.
type chainReread struct {
	due    time.Time
	delay  time.Duration
	logged map[string]bool
}

// rereadChains reads Dispatch again, until ctx ends, for every key noted on a chain the copy cannot
// read to its top — a row missing, a key repeated, more than record.MaxChainLinks links (R16 of
// dispatch://INFRA-3320, admit.Admission.UnknownChains): an issue an admission pass found on one,
// and the parent a waiting re-parent seed waits on, which the outbox notes (seedReparent) — outside
// any transaction, until each chain is known: woken by each note (chainWake), and again at the due
// time of the first key it holds, each key on a backoff of its own from chainRereadFirst to
// chainRereadMost (rereadDue).
func (w *workflowRuntime) rereadChains(ctx context.Context) {
	pending := map[string]*chainReread{}
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		next := w.rereadDue(ctx, pending, time.Now())
		if !next.IsZero() {
			timer.Reset(time.Until(next))
		}
		select {
		case <-ctx.Done():
			return
		case <-w.chainWake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// rereadDue is one turn of the re-read: it takes the keys noted since the last turn
// (admit.Admission.UnknownChains) into pending, holding each key once, reads again each key due by
// now (rereadChain), drops each whose chain is known after its read, and puts each other one off
// by its backoff. It answers when the next key is due, the zero time for none.
func (w *workflowRuntime) rereadDue(ctx context.Context, pending map[string]*chainReread, now time.Time) time.Time {
	for _, key := range w.admission.UnknownChains() {
		if _, held := pending[key]; !held {
			pending[key] = &chainReread{due: now, delay: chainRereadFirst, logged: map[string]bool{}}
		}
	}
	var next time.Time
	for _, key := range slices.Sorted(maps.Keys(pending)) {
		reread := pending[key]
		if reread.due.After(now) {
			if next.IsZero() || reread.due.Before(next) {
				next = reread.due
			}
			continue
		}
		known, err := w.rereadChain(ctx, key, reread)
		if err != nil && ctx.Err() == nil {
			w.log.Warn("the re-read of a Dispatch chain the copy cannot read to its top failed; it is read again", "issue", key, "error", err)
		}
		if known {
			delete(pending, key)
			continue
		}
		reread.due = now.Add(reread.delay)
		reread.delay = min(2*reread.delay, chainRereadMost)
		if next.IsZero() || reread.due.Before(next) {
			next = reread.due
		}
	}
	return next
}

// rereadChain reads key's chain from Dispatch and applies what it read, and says whether the copy
// holds the chain known afterwards. It reads the issue and then each issue above it, by the parent
// each answer names, one GetIssue each, up to record.MaxChainLinks links and never one key twice:
// every row of the chain is read, a present row included, since a stale row and a loop that two
// late moves made are present rows whose values are wrong. A parent of another project ends the
// read as no parent does (record.InProjectParent), since it ends the chain (R19 of
// dispatch://INFRA-3320): the re-read reads and writes nothing of another project. A link Dispatch
// no longer serves (404) ends the read there and leaves the chain unknown, logged once per key
// (reread.logged); any other failed read ends it too, and the chain is read again at its next turn.
//
// The readings are applied through the Mirror as the controller tick's re-read of Dispatch is
// (intake.ControllerTick.Observed, blockers.Mirror): each link's reading in one fact, before the
// issue's own, so the copy holds the chain when the issue's own reading is decided. That one is
// applied as Dispatch's view of the issue (intake.ObservedIssue), readied as an event of it is
// (prepareReread), through every handler: for an issue the daemon records nothing of, it is the
// admission event that decides it on the chain now known, an unrecorded todo child by the
// transition that wrote its todo, read from its event log as an event's preparation reads it; for
// a recorded one, a reading of the copy alone (intake's settleObserved). Each reading writes only
// what is newer than the copy holds (record.PutDispatchIssue).
func (w *workflowRuntime) rereadChain(ctx context.Context, key string, reread *chainReread) (bool, error) {
	var read []dispatch.Issue
	seen := map[string]bool{}
	for next := key; next != "" && !seen[next] && len(read) <= record.MaxChainLinks; {
		seen[next] = true
		issue, err := w.dispatch.GetIssue(ctx, next)
		if err != nil {
			if !dispatch.Missing(err) {
				return false, w.applyChainReadings(ctx, key, read, fmt.Errorf("read %s: %w", next, err))
			}
			if !reread.logged[next] {
				reread.logged[next] = true
				w.log.Info("a link of a Dispatch chain the copy cannot read to its top is no issue Dispatch serves; the chain stays unknown and is read again",
					"issue", key, "link", next)
			}
			break
		}
		if issue.Key == "" {
			issue.Key = next
		}
		read = append(read, issue)
		next = ""
		if issue.Parent != nil {
			next = record.InProjectParent(issue.Key, *issue.Parent)
		}
	}
	if err := w.applyChainReadings(ctx, key, read, nil); err != nil {
		return false, err
	}
	return w.chainKnown(ctx, key)
}

// applyChainReadings applies the readings rereadChain made of key's chain, the issue's own first
// (read[0]) and its links after it, as rereadChain says, and answers failed: the error that ended
// the read, nil for one that ended as the chain did. A reading that names no project is left out,
// as the tick's re-read leaves one out (readBlocker).
func (w *workflowRuntime) applyChainReadings(ctx context.Context, key string, read []dispatch.Issue, failed error) error {
	if len(read) == 0 {
		return failed
	}
	at := time.Now().UnixNano()
	var links []intake.IssueStatus
	for _, issue := range read[1:] {
		if reading, ok := intake.ReadingOf(issue.Key, issue); ok {
			links = append(links, reading)
		}
	}
	if len(links) > 0 {
		id := fmt.Sprintf("chain-reread:%s:%s:%s:%d", w.dispatchProject, w.bootID, key, at)
		if _, err := intake.ApplyFact(ctx, w.pool, "dispatch", id, intake.ControllerTick{Observed: links},
			blockers.NewMirror(w.records, w.dispatchProject)); err != nil {
			return fmt.Errorf("apply the re-read of the chain of %s: %w", key, err)
		}
	}
	prepared, err := w.prepareReread(ctx, intake.ObservedIssue(read[0]))
	if err != nil {
		return fmt.Errorf("prepare the re-read of %s: %w", key, err)
	}
	id := fmt.Sprintf("chain-reread:%s:%s:%s:%d:issue", w.dispatchProject, w.bootID, key, at)
	if _, err := intake.ApplyFact(ctx, w.pool, "dispatch", id, prepared, w.handlers...); err != nil {
		return fmt.Errorf("apply the re-read of %s: %w", key, err)
	}
	return failed
}

// chainKnown says whether the copy holds key's chain known, read in a read-only transaction of
// its own (record.ReadChainsOf).
func (w *workflowRuntime) chainKnown(ctx context.Context, key string) (bool, error) {
	var known bool
	if err := store.TransactWith(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		chains, err := record.ReadChainsOf(ctx, tx, w.records, w.dispatchProject, []string{key})
		if err != nil {
			return err
		}
		known = chains.Chain(key).Known()
		return nil
	}); err != nil {
		return false, fmt.Errorf("read the chain of %s: %w", key, err)
	}
	return known, nil
}
