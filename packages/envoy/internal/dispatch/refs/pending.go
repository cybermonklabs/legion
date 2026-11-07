package refs

import "context"

// Pending is what one transaction's write phase writes and no read of it can answer yet
// (INFRA-636: a route decides everything in its read phase and sends every statement at Commit).
// A write that indexes text naming a row it creates in the same transaction - the issue it opens,
// the document it seeds, the ask settlement makes for a new question block, the version it writes
// over a document - answers for that row here, so the reference resolves exactly as it will once
// the batch has run. It is the ledger's overlays read from this side: docs.Ledger is the one
// implementation, and every context carrying a ledger carries it (docs.withLedger), so a write
// registers a row it queues in one place and every read of it answers the same.
//
// A read with no pending rows - a read-only route, a background pass, a transaction that joined
// no ledger - sees none of this and reads the stored rows alone.
type Pending interface {
	// PendingIssue reports whether this transaction writes the issues row of key.
	PendingIssue(key string) bool
	// PendingArtifact is the uuid and the dispatch:// address (ArtifactRef) of the artifacts row
	// of reference key refKey this transaction writes, false when it writes none.
	PendingArtifact(refKey string) (id, ref string, ok bool)
	// PendingMarkdown is the markdown of the latest version of document artifactID as this
	// transaction leaves it: the version it has queued, false when it has queued none and the
	// stored version is the latest (docs.latestVersion answers from the same entry).
	PendingMarkdown(artifactID string) (string, bool)
	// PendingBlockAsks is every question block's ask of document artifactID whose row this
	// transaction writes, as that row will stand (docs.loadAskBlocks reads the same entries).
	PendingBlockAsks(artifactID string) []PendingBlockAsk
}

// PendingBlockAsk is one question block's ask whose row a transaction has queued, named by the
// columns the question map reads of it (QuestionMap): the block it is the ask of, its id, and
// the issue it is on, nil for an ask on a project's or a session's document.
type PendingBlockAsk struct {
	BlockID  string
	ID       string
	IssueKey *string
}

type pendingContextKey struct{}

// WithPending marks ctx with the rows pending writes, so every reference read made under it
// answers for the rows this transaction has queued as well as for the stored ones. A nil pending
// leaves ctx as it is.
func WithPending(ctx context.Context, pending Pending) context.Context {
	if pending == nil {
		return ctx
	}
	return context.WithValue(ctx, pendingContextKey{}, pending)
}

// pendingOf is the queued rows of the transaction ctx reads in, nil where ctx carries none.
func pendingOf(ctx context.Context) Pending {
	pending, _ := ctx.Value(pendingContextKey{}).(Pending)
	return pending
}
