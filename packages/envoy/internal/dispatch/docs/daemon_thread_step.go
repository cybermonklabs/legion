package docs

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// DaemonThreadStep is the step that re-places what R2's rule of the Legion daemon left standing
// where no walk reaches it (this issue's spec R2). A message the session `legion-daemon:<PROJECT>`
// posts is no conversation node, and neither is any message of a thread whose root that session
// posted, a person's reply included (refs.IsConversationNode). A block of a document that stands
// under such a message, or under a block of one, because a parent pointer named it before the rule,
// is in no answer of the tree read. The step moves each such block under its issue, on the pattern
// of DecisionStep.
//
// It runs once after the server listens (AfterListen), and then on each tick of the block-index
// reconciliation (Tick), which runs it under that reconciliation's lease. A tick that comes before
// the server listens does nothing, so the step's first run is the one after the listener is ready.
//
// A run takes its nodes in batches of daemonThreadBatch, in the order of their addresses
// (refs.DaemonThreadParents). For each batch it first reads, with no lock held, the nodes, the
// issue each stands under and the room of each of those issues (issue_trees), whose queries the
// batch revises. It then opens one transaction and takes every owner lock of those issues in one
// call of store.LockOwners, and only after that the lock of each of those rooms, in the order of
// the rooms' ids, as docs/persistence.go orders an owner's lock before a room's. Under the locks it
// reads each node's placement again: the step states the placement revision it read, the seq of
// the part_of it retracts, and gives up a node whose part_of moved since that read - a person's
// move of it, another process's run - which a later tick reads again. Its write phase is one batch
// and the COMMIT (queueDaemonThreadMoves): for each node the retraction of that part_of and a
// part_of to its issue's node, dated by the database's clock once the locks are held and written
// by the step (DaemonThreadStepActor), the mark of the queries of each issue a node left or joined
// (QueueQueryMark), and the head rows of the nodes. A block's issue is the one its own document
// belongs to (refs.DaemonThreadParent's Issue), which is the issue that holds the message but for
// a block of an issue below it.
//
// Once the batch has committed, the step drains each issue it marked (Service.DrainOwedQueries),
// the drain a move of the node route runs: where the move changed a query's answer, the drain
// writes that query's version, appends its artifact.version and moves an open approval request to
// it, in the step's name, the writer of the mark's newest tuple. It carries no approval: the drain
// carries one only across appended results, so a person's approval of the version before reads
// changed since. A drain that fails leaves the mark, which the block-index tick drains.
//
// The step holds no session lock: two processes that read one batch each take the same owner
// locks, and the second finds every part_of moved and gives each node up. What the step owes is the
// census kind `daemon_thread_parent` (graphcensus.ReadDaemonThreadCensus), which the pre-deploy
// census prints with each node's address before the release's first boot: the step's dry run.
type DaemonThreadStep struct {
	documents *Service
	logger    *slog.Logger
	// listening is whether the server listens: AfterListen sets it, and a tick before it does
	// nothing.
	listening atomic.Bool
	// running is held by the pass of this process that is running, so a tick that comes while
	// the run after the listen is still at work leaves the work to it.
	running sync.Mutex
}

// DaemonThreadStepActor is the writer of the tuples the step appends, and so of each version its
// drain writes and of that version's artifact.version: a system actor of the step's own, as
// QueryDrainActor is the drain's.
var DaemonThreadStepActor = model.Actor{Kind: "system", ID: "daemon-thread-step"}

// daemonThreadBatch is the most nodes one batch of the step re-places, in one transaction.
const daemonThreadBatch = 100

// DaemonThreadStarted is the message of the line the step logs as its run after the listen
// starts, once for a process: a reader of the process's output holds it to coming after the
// line that says the server listens.
const DaemonThreadStarted = "dispatch: daemon-thread replacement started"

// The two callers of a pass, as its log lines name them.
const (
	daemonThreadAfterListen = "listen"
	daemonThreadOnTick      = "reconciliation"
)

// NewDaemonThreadStep is the step over documents, the document service whose store it writes and
// whose drain writes the versions, logging through logger (slog.Default when nil).
func NewDaemonThreadStep(documents *Service, logger *slog.Logger) *DaemonThreadStep {
	if logger == nil {
		logger = slog.Default()
	}
	return &DaemonThreadStep{documents: documents, logger: logger}
}

// DaemonThreadRun is what one run of the step did: how many nodes it re-placed under their
// issues, and how many it gave up because their part_of had moved since it read them.
type DaemonThreadRun struct {
	Moved   int
	GivenUp int
}

// AfterListen is the step's run once the server listens: a process calls it after the line that
// says it listens, on a goroutine of its own, so no request waits for it. It logs that it starts
// (DaemonThreadStarted), runs the step once, and from then on lets each tick run it (Tick).
func (s *DaemonThreadStep) AfterListen(ctx context.Context) {
	s.listening.Store(true)
	s.logger.Info(DaemonThreadStarted, "trigger", daemonThreadAfterListen)
	s.pass(ctx, daemonThreadAfterListen)
}

// Tick is the step's run on a tick of the block-index reconciliation, which calls it once its own
// tick has returned, under its lease. It does nothing before the server listens.
func (s *DaemonThreadStep) Tick(ctx context.Context) {
	if !s.listening.Load() {
		return
	}
	s.pass(ctx, daemonThreadOnTick)
}

// pass runs the step once for trigger and logs what the run did, where it did anything, and its
// failure, which the next tick tries again. A pass that finds another of this process's running
// does nothing.
func (s *DaemonThreadStep) pass(ctx context.Context, trigger string) {
	if !s.running.TryLock() {
		return
	}
	defer s.running.Unlock()
	run, err := s.Run(ctx)
	if run.Moved > 0 || run.GivenUp > 0 {
		s.logger.Info("dispatch: daemon-thread replacement", "trigger", trigger, "moved", run.Moved, "given_up", run.GivenUp)
	}
	if err != nil && ctx.Err() == nil {
		s.logger.Error("dispatch: daemon-thread replacement; the next reconciliation tick tries again", "trigger", trigger, "error", err)
	}
}

// Run runs the step once: batch after batch, in the order of the nodes' addresses, until a batch
// reads fewer nodes than it holds. A node a batch gives up stays behind the read's cursor, so the
// run passes it and a later run reads it again. It answers what it did, and the first failure of
// a batch, which ends the run with the batches before it committed. Its context is marked for the
// pool's nested-acquire guard (store.WithTransactionTracking), as every entry point of the pool's
// transactions marks its own.
func (s *DaemonThreadStep) Run(ctx context.Context) (DaemonThreadRun, error) {
	ctx = store.WithTransactionTracking(ctx)
	var run DaemonThreadRun
	after := ""
	for {
		read, err := refs.DaemonThreadParents(ctx, s.documents.store.Pool, after, daemonThreadBatch)
		if err != nil {
			return run, err
		}
		if len(read) == 0 {
			return run, nil
		}
		moved, err := s.replaceBatch(ctx, read)
		run.Moved += len(moved)
		if err != nil {
			return run, err
		}
		run.GivenUp += len(read) - len(moved)
		for _, parent := range moved {
			s.logger.Info("dispatch: daemon-thread replacement moved a node under its issue",
				"node", parent.Node, "from", parent.Parent.Kind+"/"+parent.Parent.ID, "issue", parent.Issue, "placement_seq", parent.Seq)
		}
		if len(read) < daemonThreadBatch {
			return run, nil
		}
		after = read[len(read)-1].Node
	}
}

// daemonThreadRoomsStatement is the room of each issue $1 names that has converted: its tree's
// artifact (issue_trees), whose lock a version of one of its queries is written under
// (LockQueryRooms).
const daemonThreadRoomsStatement = `select t.artifact_id::text from issue_trees t where t.issue_key = any($1::text[])`

// daemonThreadPlacementsStatement is, of each block $1 names by its address, the seq of its
// current part_of (refs.BlockPlacementSQL), and the database's clock as the read runs, under the
// batch's locks, which dates what the step writes: clock_timestamp(), never now(), the start of
// a transaction that waited for those locks. A block that stands under nothing answers no row.
var daemonThreadPlacementsStatement = `
	select named.node, standing.seq, clock_timestamp()
	from unnest($1::text[]) as named (node)
	cross join lateral (` + refs.BlockPlacementSQL(`named.node`) + `) standing`

// replaceBatch re-places the nodes of read, one batch of a run, in one transaction, and answers
// the ones it moved: every one of read whose part_of, under the batch's locks, is still the one
// read named. It reads the rooms of the batch's issues with no lock held, takes the issues' owner
// locks in one call, then each room's lock in the order of the rooms' ids, reads the placements
// again, and writes the moves in one batch (queueDaemonThreadMoves) and the COMMIT. A batch none
// of whose nodes still stands where it was read writes nothing. Once it has committed it drains
// the queries of each issue its batch marked, and publishes what each drain appended.
func (s *DaemonThreadStep) replaceBatch(ctx context.Context, read []refs.DaemonThreadParent) ([]refs.DaemonThreadParent, error) {
	pool := s.documents.store.Pool
	var issues []string
	for _, parent := range read {
		for _, issue := range []string{parent.Holder, parent.Issue} {
			if !slices.Contains(issues, issue) {
				issues = append(issues, issue)
			}
		}
	}
	slices.Sort(issues)
	rows, err := pool.Query(ctx, daemonThreadRoomsStatement, issues)
	if err != nil {
		return nil, fmt.Errorf("read the rooms of %v: %w", issues, err)
	}
	rooms, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the rooms of %v: %w", issues, err)
	}
	slices.Sort(rooms)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin the re-placement of %d nodes that stand under daemon threads: %w", len(read), err)
	}
	defer tx.Rollback(ctx)
	owners := make([]store.Owner, len(issues))
	for index, issue := range issues {
		owners[index] = store.IssueOwner(issue)
	}
	if err := store.LockOwners(ctx, tx, owners...); err != nil {
		return nil, err
	}
	for _, room := range rooms {
		if err := lockDocumentRoom(ctx, tx, room); err != nil {
			return nil, err
		}
	}
	nodes := make([]string, len(read))
	for index, parent := range read {
		nodes[index] = parent.Node
	}
	rows, err = tx.Query(ctx, daemonThreadPlacementsStatement, nodes)
	if err != nil {
		return nil, fmt.Errorf("read where %d nodes stand under their issues' locks: %w", len(nodes), err)
	}
	standing := make(map[string]int64, len(nodes))
	var at time.Time
	for rows.Next() {
		var node string
		var seq int64
		if err := rows.Scan(&node, &seq, &at); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read where %d nodes stand under their issues' locks: %w", len(nodes), err)
		}
		standing[node] = seq
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read where %d nodes stand under their issues' locks: %w", len(nodes), err)
	}
	var moved []refs.DaemonThreadParent
	for _, parent := range read {
		if seq, stands := standing[parent.Node]; stands && seq == parent.Seq {
			moved = append(moved, parent)
		}
	}
	if len(moved) == 0 {
		return nil, nil
	}
	marked := map[string]*bool{}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		queueDaemonThreadMoves(b, moved, at.UTC(), marked)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("re-place %d nodes that stand under daemon threads: %w", len(moved), err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit the re-placement of %d nodes that stand under daemon threads: %w", len(moved), err)
	}
	for _, issue := range issues {
		if took := marked[issue]; took == nil || !*took {
			continue
		}
		for _, event := range s.documents.DrainOwedQueries(ctx, issue) {
			s.documents.events.Publish(event)
		}
	}
	return moved, nil
}

// queueDaemonThreadMoves puts on b the write of one batch of the step: for each node of moved,
// the retraction of the part_of the step read, whose seq is the placement revision it states, and
// one part_of to the node of its issue, each dated at and written by DaemonThreadStepActor at
// graphstore.NoEvent, as a move of the node route writes its own (graphstore.QueueWrite); then
// the mark of the queries of each issue a node left or joined, after the tuples so the mark's
// sequence is the last one the write drew and its tuple names the step (QueueQueryMark); and last
// the head rows of the nodes (graphstore.QueueHeads). marked holds, for each issue it marks,
// whether its mark took, which a converted issue's does.
//
// It writes no document_blocks row: a block's parent_block_id projects a part_of to a block of its
// own artifact alone, so the row of a block that stood under a message holds null, and holds null
// under its issue.
func queueDaemonThreadMoves(b *store.WriteBatch, moved []refs.DaemonThreadParent, at time.Time, marked map[string]*bool) {
	tuples := make([]graphstore.Tuple, 0, 2*len(moved))
	var issues []string
	for _, parent := range moved {
		placed := graphstore.Tuple{
			FromKind: refs.BlockNodeKind, FromID: parent.Node, Kind: graphstore.TuplePartOf,
			At: at, Who: DaemonThreadStepActor, SourceEvent: graphstore.NoEvent,
		}
		retracted, asserted := placed, placed
		retracted.ToKind, retracted.ToID = parent.Parent.Kind, parent.Parent.ID
		asserted.ToKind, asserted.ToID, asserted.Asserted = graphstore.KindIssue, parent.Issue, true
		tuples = append(tuples, retracted, asserted)
		for _, issue := range []string{parent.Holder, parent.Issue} {
			if !slices.Contains(issues, issue) {
				issues = append(issues, issue)
			}
		}
	}
	graphstore.QueueWrite(b, nil, tuples)
	slices.Sort(issues)
	for _, issue := range issues {
		took := false
		marked[issue] = &took
		QueueQueryMark(b, issue, graphstore.NoEvent, &took)
	}
	graphstore.QueueHeads(b, graphstore.HeadsOf(nil, tuples))
}
