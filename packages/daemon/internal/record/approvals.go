package record

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/signals"
)

// ApprovalAsk is one ask of a project's issue as the approvals signal keeps it: the ask as its
// issue's events leave it (signals.NextAsk), and Seq, the sequence in its issue's Dispatch event
// log of the event that last changed it, or, for an ask the boot read, the sequence the issue stood
// at when the read began, which is never above the sequence its state stands at.
type ApprovalAsk struct {
	signals.Ask
	Seq int64
}

// IssueAssignee is the assignee of one issue, "" for none, as the issue's Dispatch state at Seq in
// its event log named it.
type IssueAssignee struct {
	Issue, Assignee string
	Seq             int64
}

// ApprovalsRead is what the boot's read of Dispatch gives the approvals signal: each ask Dispatch
// lists open on an issue and each kept ask that read found closed, the assignee of each issue
// listed, and the project's default assignee as the read of the project's open asks answered it, at
// that read's own time (DefaultAt), "" where Dispatch answered none (a Dispatch before the member,
// or a project it stores no default of). DefaultSeq is the sequence of the newest settings event of
// the default assignee the store held when the read began (DefaultAssigneeSeq), so the read's
// default is kept only where no settings event was kept since.
type ApprovalsRead struct {
	Asks            []ApprovalAsk
	Assignees       []IssueAssignee
	DefaultAssignee string
	DefaultAt       time.Time
	DefaultSeq      int64
}

// NoDefaultAssignee is why the approvals signal of a project cannot be computed while no default
// assignee of the project is known: Dispatch's read of the project's open asks answered none (a
// Dispatch before the member), and no event that moves it has arrived.
const NoDefaultAssignee = "Dispatch has not given the project's default assignee: its read of the project's open asks carries none, and no change of it has arrived"

// ApprovalsNotRead is why the approvals signal of a project cannot be computed from the first boot
// of the release that keeps its inputs until that boot's read of Dispatch succeeds: nothing of the
// asks open before the boot is kept until then, so a count would leave them out.
const ApprovalsNotRead = "the daemon has not yet read the project's open asks from Dispatch since it began keeping them"

// Approvals is the approvals signal's input as the store holds it for a project: the id of each
// kept ask that waits on the project's default assignee, sorted, and Unrecorded, why the signal
// cannot be computed, "" where it can.
type Approvals struct {
	Waiting    []string
	Unrecorded string
}

// Value is the approvals signal: the count of the asks waiting, and not recorded where Unrecorded
// says why, never 0 then.
func (a Approvals) Value() signals.Value {
	if a.Unrecorded != "" {
		return signals.Value{}
	}
	return signals.Value{Recorded: true, Value: float64(len(a.Waiting))}
}

// KeepAskEvent keeps event, one event of an ask of project's issue at seq in its issue's Dispatch
// event log, as signals.NextAsk turns the kept ask by it. An event at or below the sequence the ask
// was last changed at is older than what the store holds, and changes nothing: the same event
// brought twice (by the stream and by the boot's catch-up), an older one delivered after a newer,
// and one the boot's read already stands past are each kept once and in order. An event NextAsk
// changes nothing by (a comment on an ask no event made) writes nothing.
func (s *Postgres) KeepAskEvent(ctx context.Context, tx pgx.Tx, project string, event signals.AskEvent, seq int64) error {
	if event.Ask == "" || seq < 1 {
		return fmt.Errorf("keep the ask event %+v of %s at %d: it needs an ask and a positive sequence", event, project, seq)
	}
	prev := signals.Ask{ID: event.Ask}
	var kept int64
	err := tx.QueryRow(ctx, "select issue, open, human, asked_of, seq from approval_asks where project = $1 and ask = $2",
		project, event.Ask).Scan(&prev.Issue, &prev.Open, &prev.Human, &prev.AskedOf, &kept)
	known := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read the ask %s of %s: %w", event.Ask, project, err)
	}
	if known && seq <= kept {
		return nil
	}
	next, changed := signals.NextAsk(prev, known, event)
	if !changed {
		return nil
	}
	return s.putApprovalAsks(ctx, tx, project, []ApprovalAsk{{Ask: next, Seq: seq}}, false)
}

// putApprovalAsks writes asks over the kept rows of their ids where a kept row's sequence is below
// the ask's, or, where inclusive, at or below it: an event must be newer than what the row holds,
// and the boot's read may restate the event its sequence is. A row that holds the ask as given is
// left alone, so a read that restates what the store holds writes nothing. One statement writes
// them all, so no id may repeat among them. An ask it writes is a write admission reads
// (RecordsWritten), so the fact's pass counts it.
func (s *Postgres) putApprovalAsks(ctx context.Context, tx pgx.Tx, project string, asks []ApprovalAsk, inclusive bool) error {
	if len(asks) == 0 {
		return nil
	}
	ids, issues, open, human, askedOf, seqs := make([]string, len(asks)), make([]string, len(asks)), make([]bool, len(asks)),
		make([]bool, len(asks)), make([]string, len(asks)), make([]int64, len(asks))
	for i, ask := range asks {
		ids[i], issues[i], open[i], human[i], askedOf[i], seqs[i] = ask.ID, ask.Issue, ask.Open, ask.Human, ask.AskedOf, ask.Seq
	}
	tag, err := tx.Exec(ctx, `insert into approval_asks (project, ask, issue, open, human, asked_of, seq)
		select $1, * from unnest($2::text[], $3::text[], $4::boolean[], $5::boolean[], $6::text[], $7::bigint[])
		on conflict (project, ask) do update set issue = excluded.issue, open = excluded.open, human = excluded.human,
			asked_of = excluded.asked_of, seq = excluded.seq
		where (approval_asks.seq < excluded.seq or ($8::boolean and approval_asks.seq = excluded.seq))
			and (approval_asks.issue, approval_asks.open, approval_asks.human, approval_asks.asked_of, approval_asks.seq)
				is distinct from (excluded.issue, excluded.open, excluded.human, excluded.asked_of, excluded.seq)`,
		project, ids, issues, open, human, askedOf, seqs, inclusive)
	if err != nil {
		return fmt.Errorf("keep %d asks of %s: %w", len(asks), project, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

// KeepIssueAssignee keeps the assignee of issue of project, "" for none, as its issue event at seq
// named it, where no newer event of the issue named one.
func (s *Postgres) KeepIssueAssignee(ctx context.Context, tx pgx.Tx, project string, assignee IssueAssignee) error {
	return s.putIssueAssignees(ctx, tx, project, []IssueAssignee{assignee}, false)
}

// putIssueAssignees writes assignees over the kept rows of their issues where a kept row's
// sequence is below the assignee's, or, where inclusive, at or below it, as putApprovalAsks writes
// asks. A row that holds the same assignee at the same sequence is left alone, so the boot's read
// of every issue the project lists writes the rows that moved and no other. One statement writes
// them all, so no issue may repeat among them. An assignee it writes is a write admission reads
// (RecordsWritten).
func (s *Postgres) putIssueAssignees(ctx context.Context, tx pgx.Tx, project string, assignees []IssueAssignee, inclusive bool) error {
	if len(assignees) == 0 {
		return nil
	}
	issues, people, seqs := make([]string, len(assignees)), make([]string, len(assignees)), make([]int64, len(assignees))
	for i, assignee := range assignees {
		if assignee.Issue == "" || assignee.Seq < 1 {
			return fmt.Errorf("keep the assignee %+v of %s: it needs an issue and a positive sequence", assignee, project)
		}
		issues[i], people[i], seqs[i] = assignee.Issue, assignee.Assignee, assignee.Seq
	}
	tag, err := tx.Exec(ctx, `insert into approval_assignees (project, issue, assignee, seq)
		select $1, * from unnest($2::text[], $3::text[], $4::bigint[])
		on conflict (project, issue) do update set assignee = excluded.assignee, seq = excluded.seq
		where (approval_assignees.seq < excluded.seq or ($5::boolean and approval_assignees.seq = excluded.seq))
			and (approval_assignees.assignee, approval_assignees.seq) is distinct from (excluded.assignee, excluded.seq)`,
		project, issues, people, seqs, inclusive)
	if err != nil {
		return fmt.Errorf("keep the assignees of %d issues of %s: %w", len(assignees), project, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

// KeepDefaultAssignee keeps assignee as the default assignee of project, as Dispatch's
// settings.project_default_assignee.updated at seq in the project's own event log, at at, moved
// it, where no newer such event was kept. A default only the boot's read gave holds sequence 0, so
// the first event after it is kept whatever its sequence: a read carries no sequence of the
// project's log, and an event older than the read it follows is one of those the stream delivers
// before the newest, so the newest it ends on is the one Dispatch holds. A default it writes is a
// write admission reads (RecordsWritten).
func (s *Postgres) KeepDefaultAssignee(ctx context.Context, tx pgx.Tx, project, assignee string, seq int64, at time.Time) error {
	if assignee == "" || seq < 1 {
		return fmt.Errorf("keep the default assignee of %s: it needs an assignee and a positive sequence", project)
	}
	tag, err := tx.Exec(ctx, `insert into approval_default_assignees (project, assignee, seq, at) values ($1, $2, $3, $4)
		on conflict (project) do update set assignee = excluded.assignee, seq = excluded.seq, at = excluded.at
		where approval_default_assignees.seq < excluded.seq`,
		project, assignee, seq, at.UTC())
	if err != nil {
		return fmt.Errorf("keep the default assignee of %s at %d: %w", project, seq, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

// KeepApprovalsRead keeps read, the boot's read of Dispatch for project: each ask and each
// assignee where no event newer than the sequence the read stood at is kept (an event that landed
// while the read ran, already kept, stands), and the default assignee, where Dispatch answered
// one, where no settings event was kept since the read began (read.DefaultSeq): such an event is
// newer than, or the same as, what the read gives. The read's default keeps the sequence of the
// newest settings event kept, so an older one delivered after it changes nothing, and a default
// the store holds already keeps the moment it was first kept, so a read that restates every row
// the store holds writes none.
func (s *Postgres) KeepApprovalsRead(ctx context.Context, tx pgx.Tx, project string, read ApprovalsRead) error {
	asks := newestEach(read.Asks, func(a ApprovalAsk) (string, int64) { return a.ID, a.Seq })
	for _, ask := range asks {
		if ask.ID == "" || ask.Issue == "" || ask.Seq < 1 {
			return fmt.Errorf("keep the ask %+v of %s the boot read: it needs an id, an issue and a positive sequence", ask, project)
		}
	}
	if err := s.putApprovalAsks(ctx, tx, project, asks, true); err != nil {
		return err
	}
	assignees := newestEach(read.Assignees, func(a IssueAssignee) (string, int64) { return a.Issue, a.Seq })
	if err := s.putIssueAssignees(ctx, tx, project, assignees, true); err != nil {
		return err
	}
	if read.DefaultAssignee == "" {
		return nil
	}
	tag, err := tx.Exec(ctx, `insert into approval_default_assignees (project, assignee, seq, at) values ($1, $2, 0, $3)
		on conflict (project) do update set assignee = excluded.assignee, at = excluded.at
		where approval_default_assignees.seq <= $4 and approval_default_assignees.assignee <> excluded.assignee`,
		project, read.DefaultAssignee, read.DefaultAt.UTC(), read.DefaultSeq)
	if err != nil {
		return fmt.Errorf("keep the default assignee of %s the boot read: %w", project, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

// newestEach is items with one of each key, the one of the highest sequence, in the order of the
// keys: a read may name an issue by its listing and again by its own read.
func newestEach[T any](items []T, keyOf func(T) (string, int64)) []T {
	newest := make(map[string]T, len(items))
	for _, item := range items {
		key, seq := keyOf(item)
		if kept, seen := newest[key]; seen {
			if _, keptSeq := keyOf(kept); keptSeq >= seq {
				continue
			}
		}
		newest[key] = item
	}
	return slices.Collect(func(yield func(T) bool) {
		for _, key := range slices.Sorted(maps.Keys(newest)) {
			if !yield(newest[key]) {
				return
			}
		}
	})
}

// OpenApprovalAsks is each ask of project the store holds open, by ask id: the asks the boot reads
// the issues of beside those Dispatch lists open, so one that closed while the daemon was down is
// found closed.
func (s *Postgres) OpenApprovalAsks(ctx context.Context, tx pgx.Tx, project string) ([]ApprovalAsk, error) {
	rows, err := tx.Query(ctx, "select ask, issue, open, human, asked_of, seq from approval_asks where project = $1 and open order by ask", project)
	if err != nil {
		return nil, fmt.Errorf("read the open asks of %s: %w", project, err)
	}
	asks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ApprovalAsk, error) {
		var ask ApprovalAsk
		err := row.Scan(&ask.ID, &ask.Issue, &ask.Open, &ask.Human, &ask.AskedOf, &ask.Seq)
		return ask, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the open asks of %s: %w", project, err)
	}
	return asks, nil
}

// DefaultAssigneeSeq is the sequence of the newest settings event of project's default assignee
// the store holds, 0 where it holds none or only the boot's read of it: what the boot's read
// carries (ApprovalsRead.DefaultSeq), read before it reads Dispatch.
func (s *Postgres) DefaultAssigneeSeq(ctx context.Context, tx pgx.Tx, project string) (int64, error) {
	var seq int64
	err := tx.QueryRow(ctx, "select seq from approval_default_assignees where project = $1", project).Scan(&seq)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("read the default assignee of %s: %w", project, err)
	}
	return seq, nil
}

// ApprovalsOf is the approvals signal's input of project as the store holds it: not recorded, with
// the reason, where the boot's read of Dispatch failed (SetSignalInputReadError) or no default
// assignee of the project is known (NoDefaultAssignee); else each open ask kept that the approvals
// signal counts, by the replay's own rule (signals.Ask.WaitsOnDefaultOf, signals.ApprovalState).
func (s *Postgres) ApprovalsOf(ctx context.Context, tx pgx.Tx, project string) (Approvals, error) {
	_, _, readError, err := s.SignalInputStatus(ctx, tx, project, signals.Approvals)
	if err != nil {
		return Approvals{}, err
	}
	if readError != "" {
		return Approvals{Unrecorded: readError}, nil
	}
	var defaultAssignee string
	err = tx.QueryRow(ctx, "select assignee from approval_default_assignees where project = $1", project).Scan(&defaultAssignee)
	if errors.Is(err, pgx.ErrNoRows) {
		return Approvals{Unrecorded: NoDefaultAssignee}, nil
	}
	if err != nil {
		return Approvals{}, fmt.Errorf("read the default assignee of %s: %w", project, err)
	}
	rows, err := tx.Query(ctx, `select a.ask, a.issue, a.open, a.human, a.asked_of, coalesce(i.assignee, '')
		from approval_asks a
		left join approval_assignees i on i.project = a.project and i.issue = a.issue
		where a.project = $1 and a.open
		order by a.ask`, project)
	if err != nil {
		return Approvals{}, fmt.Errorf("read the open asks of %s: %w", project, err)
	}
	approvals := Approvals{Waiting: []string{}}
	var ask signals.Ask
	var assignee string
	if _, err := pgx.ForEachRow(rows, []any{&ask.ID, &ask.Issue, &ask.Open, &ask.Human, &ask.AskedOf, &assignee}, func() error {
		if ask.WaitsOnDefaultOf(assignee, defaultAssignee) {
			approvals.Waiting = append(approvals.Waiting, ask.ID)
		}
		return nil
	}); err != nil {
		return Approvals{}, fmt.Errorf("read the open asks of %s: %w", project, err)
	}
	return approvals, nil
}
