package graphcensus

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// DaemonThreadParentKind is the kind the census names the count of R2's daemon-thread step by
// (this issue's spec R2): each current part_of of a block of a document whose far end is a message
// of a thread the Legion daemon's session began, or that session's own message, or a block of
// one. Such a message is no conversation node (refs.IsConversationNode), so no walk of the
// issue's tree reaches what stands under it, and docs.DaemonThreadStep re-places each such block
// under its issue: the count is what the step owes before it runs, and zero after it.
const DaemonThreadParentKind = "daemon_thread_parent"

// DaemonThreadCensus is the dry run of R2's daemon-thread step: the placements the step would
// move (refs.DaemonThreadParents), in the order of their nodes' addresses, read and nothing
// written. It refuses no census: the step moves them at the first boot of the release, and a
// deployment reads the count and the addresses before that boot.
type DaemonThreadCensus struct {
	// Absent says the database holds no node and tuple store yet, so no node stands under
	// anything there.
	Absent bool
	// Parents is each placement the step would move.
	Parents []refs.DaemonThreadParent
}

// Count is how many placements the step would move, the n of `daemon_thread_parent=<n>`.
func (c DaemonThreadCensus) Count() int { return len(c.Parents) }

// ReadDaemonThreadCensus reads the dry run of R2's daemon-thread step through q, which a census
// hands it as its read-only transaction, so the read writes no tuple, no version and no approval.
// Over a database the store's first migration has not reached - one that holds no graph_tuples
// table, or neither of the tables a placement is read against - it answers the census Absent and
// reads nothing else: the pre-deploy census runs before the release migrates.
func ReadDaemonThreadCensus(ctx context.Context, q graphstore.Queryer) (DaemonThreadCensus, error) {
	var stored bool
	if err := q.QueryRow(ctx, `
		select to_regclass('graph_tuples') is not null and to_regclass('messages') is not null
		   and to_regclass('artifacts') is not null
	`).Scan(&stored); err != nil {
		return DaemonThreadCensus{}, fmt.Errorf("read whether the database holds the store the daemon-thread census reads: %w", err)
	}
	if !stored {
		return DaemonThreadCensus{Absent: true}, nil
	}
	parents, err := refs.DaemonThreadParents(ctx, q, "", 0)
	if err != nil {
		return DaemonThreadCensus{}, err
	}
	return DaemonThreadCensus{Parents: parents}, nil
}

// DaemonThreadNodeLine is how the line of one placement starts, up to the node's address, which
// follows it quoted: a reader of the census takes the addresses the step would move from the
// lines that start so.
const DaemonThreadNodeLine = "census: " + DaemonThreadParentKind + " node="

// Write prints the dry run: one line holding `daemon_thread_parent=<n>`, the count written whole,
// and one line for each placement (DaemonThreadNodeLine) naming the node the step would move by
// its store address, the message or the block of one it stands under, each quoted, since a block's
// id holds any text, and the issue the step would place it under. It prints counts and node
// addresses alone, as every form of the census does.
func (c DaemonThreadCensus) Write(w io.Writer) {
	if c.Absent {
		fmt.Fprintf(w, "census: %s=0 (the database holds no node and tuple store yet, so no node stands under a message)\n", DaemonThreadParentKind)
		return
	}
	fmt.Fprintf(w, "census: %s=%d (blocks that stand under a message of a thread the Legion daemon's session began, or under a block of one: the daemon-thread step re-places each under its issue at the first boot of this release and writes a version of each query it moves, which a person approves again; it refuses nothing)\n",
		DaemonThreadParentKind, c.Count())
	for _, parent := range c.Parents {
		under := parent.Parent.ID
		if !strings.Contains(under, "#") {
			under = parent.Parent.Kind + "/" + under
		}
		fmt.Fprintf(w, "%s%q under=%q issue=%s\n", DaemonThreadNodeLine, parent.Node, under, parent.Issue)
	}
}
