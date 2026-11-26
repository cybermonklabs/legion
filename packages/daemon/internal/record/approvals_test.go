package record

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// approvalsRig is a migrated database and the store over it, with each call of the approvals
// signal's inputs in a transaction of its own, as admission makes each in its fact's.
type approvalsRig struct {
	t       *testing.T
	st      *store.Store
	records *Postgres
}

func newApprovalsRig(t *testing.T) *approvalsRig {
	t.Helper()
	return &approvalsRig{t: t, st: migratedStore(t), records: NewStore()}
}

func (r *approvalsRig) tx(fn func(ctx context.Context, tx pgx.Tx) error) {
	r.t.Helper()
	if err := store.Transact(context.Background(), r.st.Pool(), fn); err != nil {
		r.t.Fatalf("transaction: %v", err)
	}
}

// ask keeps an event of an ask of issue at seq, of project LEGION.
func (r *approvalsRig) ask(seq int64, event signals.AskEvent) {
	r.t.Helper()
	r.tx(func(ctx context.Context, tx pgx.Tx) error {
		return r.records.KeepAskEvent(ctx, tx, "LEGION", event, seq)
	})
}

func (r *approvalsRig) assignee(issue, person string, seq int64) {
	r.t.Helper()
	r.tx(func(ctx context.Context, tx pgx.Tx) error {
		return r.records.KeepIssueAssignee(ctx, tx, "LEGION", IssueAssignee{Issue: issue, Assignee: person, Seq: seq})
	})
}

func (r *approvalsRig) defaultAssignee(person string, seq int64) {
	r.t.Helper()
	r.tx(func(ctx context.Context, tx pgx.Tx) error {
		return r.records.KeepDefaultAssignee(ctx, tx, "LEGION", person, seq, time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC).Add(time.Duration(seq)*time.Minute))
	})
}

func (r *approvalsRig) read(read ApprovalsRead) {
	r.t.Helper()
	r.tx(func(ctx context.Context, tx pgx.Tx) error {
		return r.records.KeepApprovalsRead(ctx, tx, "LEGION", read)
	})
}

func (r *approvalsRig) approvals(project string) Approvals {
	r.t.Helper()
	var got Approvals
	r.tx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = r.records.ApprovalsOf(ctx, tx, project)
		return err
	})
	return got
}

// waiting is the ids of the asks of LEGION the approvals signal counts, and fails where the signal
// is not recorded.
func (r *approvalsRig) waiting() []string {
	r.t.Helper()
	got := r.approvals("LEGION")
	if got.Unrecorded != "" {
		r.t.Fatalf("the approvals signal is not recorded (%s), want a count", got.Unrecorded)
	}
	return got.Waiting
}

func openedAsk(id, issue, askedOf string) signals.AskEvent {
	return signals.AskEvent{Kind: signals.AskOpened, Ask: id, Issue: issue, State: signals.ReportsOpen, AskedOfSet: askedOf != "", AskedOf: askedOf}
}

func askEvent(kind signals.AskEventKind, id, issue string, state signals.AskReport) signals.AskEvent {
	return signals.AskEvent{Kind: kind, Ask: id, Issue: issue, State: state}
}

// The approvals signal counts the open asks that wait on a person who is the project's default
// assignee, by the one rule package signals owns: an ask waits on the person it was handed to, else
// its issue's assignee, else the project's default assignee, and counts where that is the default
// assignee and the ask waits on a person rather than its agent. Each ask below differs from the
// next in one input of that rule.
func TestApprovalsCountTheOpenAsksThatWaitOnTheDefaultAssignee(t *testing.T) {
	r := newApprovalsRig(t)
	r.assignee("LEGION-2", "alice", 1)
	r.ask(1, openedAsk("unassigned-unhanded", "LEGION-1", ""))
	r.ask(1, openedAsk("assigned-elsewhere", "LEGION-2", ""))
	r.ask(2, openedAsk("handed-to-the-default", "LEGION-2", "dana"))
	r.ask(2, openedAsk("handed-to-another", "LEGION-1", "bob"))
	r.ask(3, openedAsk("agents-turn", "LEGION-1", ""))
	r.ask(4, signals.AskEvent{Kind: signals.AskCommented, Ask: "agents-turn", Issue: "LEGION-1", WaitingOn: signals.WaitsOnAgent})
	r.ask(5, openedAsk("answered", "LEGION-1", ""))
	r.ask(6, askEvent(signals.AskAnswered, "answered", "LEGION-1", signals.ReportsClosed))

	// No default assignee is known yet: the signal is not recorded, with the reason, never 0.
	if got := r.approvals("LEGION"); got.Unrecorded != NoDefaultAssignee || got.Value().Recorded {
		t.Fatalf("before a default assignee is known the signal = %+v (value %+v), want it not recorded for %q", got, got.Value(), NoDefaultAssignee)
	}
	r.defaultAssignee("dana", 1)
	if got, want := r.waiting(), []string{"handed-to-the-default", "unassigned-unhanded"}; !slices.Equal(got, want) {
		t.Fatalf("the asks that wait on dana = %v, want %v", got, want)
	}
	if got := r.approvals("LEGION").Value(); got != (signals.Value{Recorded: true, Value: 2}) {
		t.Fatalf("the signal = %+v, want 2 recorded", got)
	}
	// Another project's rows are nobody's here, and its signal has no default assignee of its own.
	if got := r.approvals("OTHER"); got.Unrecorded != NoDefaultAssignee {
		t.Fatalf("another project's signal = %+v, want it not recorded: no default assignee of its own is known", got)
	}

	// Each input of the rule moves the count: the default assignee, the issue's assignee and the
	// person the ask was handed to.
	r.defaultAssignee("alice", 2)
	if got, want := r.waiting(), []string{"assigned-elsewhere", "unassigned-unhanded"}; !slices.Equal(got, want) {
		t.Fatalf("with alice the default the asks that wait on her = %v, want %v: the one on her issue, and the one on an issue nobody holds, which waits on the default", got, want)
	}
	r.defaultAssignee("dana", 3)
	r.assignee("LEGION-1", "carol", 2)
	if got, want := r.waiting(), []string{"handed-to-the-default"}; !slices.Equal(got, want) {
		t.Fatalf("once LEGION-1 is carol's the asks that wait on dana = %v, want %v", got, want)
	}
	r.ask(7, askEvent(signals.AskAskedOf, "handed-to-the-default", "LEGION-2", signals.ReportsOpen))
	r.ask(8, signals.AskEvent{Kind: signals.AskAskedOf, Ask: "handed-to-the-default", Issue: "LEGION-2", State: signals.ReportsOpen, AskedOfSet: true, AskedOf: "erin"})
	if got := r.waiting(); len(got) != 0 {
		t.Fatalf("once the ask is handed to erin the asks that wait on dana = %v, want none", got)
	}
	// An ask handed back to its issue's assignee (null) waits on that assignee again.
	r.ask(9, signals.AskEvent{Kind: signals.AskAskedOf, Ask: "handed-to-the-default", Issue: "LEGION-2", State: signals.ReportsOpen, AskedOfSet: true})
	r.assignee("LEGION-2", "dana", 3)
	if got, want := r.waiting(), []string{"assigned-elsewhere", "handed-to-the-default"}; !slices.Equal(got, want) {
		t.Fatalf("with the ask handed back to LEGION-2's assignee, dana, the asks that wait on her = %v, want %v: that ask and the one on LEGION-2 handed to nobody", got, want)
	}
}

// An event is kept by its sequence in its issue's log: one delivered twice, or after a newer one,
// changes nothing, and a newer one is applied. The same holds for an issue's assignee.
func TestAnAskEventOlderThanWhatTheStoreHoldsChangesNothing(t *testing.T) {
	r := newApprovalsRig(t)
	r.defaultAssignee("dana", 1)
	r.ask(10, openedAsk("ask", "LEGION-1", ""))
	if got := r.waiting(); !slices.Equal(got, []string{"ask"}) {
		t.Fatalf("the open ask waits on dana: %v", got)
	}
	closed := askEvent(signals.AskAnswered, "ask", "LEGION-1", signals.ReportsClosed)
	r.ask(11, closed)
	if got := r.waiting(); len(got) != 0 {
		t.Fatalf("the answered ask waits on %v, want nobody", got)
	}
	// The opening delivered again, and an older edit: neither reopens the closed ask.
	r.ask(10, openedAsk("ask", "LEGION-1", ""))
	r.ask(11, openedAsk("ask", "LEGION-1", ""))
	r.ask(9, askEvent(signals.AskEdited, "ask", "LEGION-1", signals.ReportsOpen))
	if got := r.waiting(); len(got) != 0 {
		t.Fatalf("after an older event of the ask was delivered it waits on %v, want it still closed", got)
	}
	r.ask(12, askEvent(signals.AskOpened, "ask", "LEGION-1", signals.ReportsOpen))
	if got := r.waiting(); !slices.Equal(got, []string{"ask"}) {
		t.Fatalf("a newer event reopens the ask: it waits on %v", got)
	}
	// A reply to an ask no event of the store's made is no ask.
	r.ask(13, signals.AskEvent{Kind: signals.AskCommented, Ask: "unknown", Issue: "LEGION-1", WaitingOn: signals.WaitsOnHuman})
	if got := r.waiting(); !slices.Equal(got, []string{"ask"}) {
		t.Fatalf("a reply to an unknown ask moved the count: %v", got)
	}

	r.assignee("LEGION-1", "carol", 20)
	r.assignee("LEGION-1", "", 19)
	if got := r.waiting(); len(got) != 0 {
		t.Fatalf("an older assignee event moved the issue back to nobody: the ask waits on %v, want none", got)
	}
	r.assignee("LEGION-1", "", 21)
	if got := r.waiting(); !slices.Equal(got, []string{"ask"}) {
		t.Fatalf("a newer assignee event naming nobody: the ask waits on %v, want it back on dana", got)
	}
}

// The project's default assignee is kept by the sequence of the settings event in the project's
// own log. A default only the boot's read gave holds none: the first event after it is kept, and
// the read is kept over an event only where none was kept since the read began.
func TestTheDefaultAssigneeIsKeptByTheSequenceOfItsEventAndTheBootsReadYieldsToAnEventThatLandedDuringIt(t *testing.T) {
	r := newApprovalsRig(t)
	defaultOf := func() string {
		t.Helper()
		var person string
		r.tx(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, "select assignee from approval_default_assignees where project = 'LEGION'").Scan(&person)
		})
		return person
	}
	seqOf := func() int64 {
		t.Helper()
		var seq int64
		r.tx(func(ctx context.Context, tx pgx.Tx) (err error) {
			seq, err = r.records.DefaultAssigneeSeq(ctx, tx, "LEGION")
			return err
		})
		return seq
	}
	readAt := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)

	// Events alone: a newer one is kept, an older or repeated one is not.
	r.defaultAssignee("dana", 5)
	r.defaultAssignee("erin", 4)
	r.defaultAssignee("frank", 5)
	if got := defaultOf(); got != "dana" || seqOf() != 5 {
		t.Fatalf("after events 5, 4 and 5 the default = %q at %d, want dana at 5: an older and a repeated event change nothing", got, seqOf())
	}
	r.defaultAssignee("erin", 6)
	if got := defaultOf(); got != "erin" {
		t.Fatalf("after event 6 the default = %q, want erin", got)
	}

	// A read begun at sequence 6, with no event since, is kept, and holds that sequence: an older
	// event delivered after it changes nothing, and a newer one is applied.
	r.read(ApprovalsRead{DefaultAssignee: "gina", DefaultAt: readAt, DefaultSeq: seqOf()})
	if got := defaultOf(); got != "gina" || seqOf() != 6 {
		t.Fatalf("after a read at 6 the default = %q at %d, want gina at 6", got, seqOf())
	}
	r.defaultAssignee("harry", 5)
	if got := defaultOf(); got != "gina" {
		t.Fatalf("an event older than the read changed the default to %q, want gina", got)
	}
	r.defaultAssignee("harry", 7)
	if got := defaultOf(); got != "harry" {
		t.Fatalf("a newer event: the default = %q, want harry", got)
	}

	// An event kept while the read ran stands over what the read gives: the read began at 7 and an
	// event 8 landed before it was kept.
	began := seqOf()
	r.defaultAssignee("iris", 8)
	r.read(ApprovalsRead{DefaultAssignee: "stale", DefaultAt: readAt, DefaultSeq: began})
	if got := defaultOf(); got != "iris" {
		t.Fatalf("a read that began before event 8 replaced its default with %q, want iris", got)
	}

	// A Dispatch that answers no default assignee changes none; where none is kept the signal is not
	// recorded, and a read with one makes it recorded, a read-sourced default holding no sequence.
	r.read(ApprovalsRead{DefaultAt: readAt, DefaultSeq: seqOf()})
	if got := defaultOf(); got != "iris" {
		t.Fatalf("a read with no default changed it to %q, want iris", got)
	}
	other := newApprovalsRig(t)
	other.read(ApprovalsRead{DefaultAt: readAt})
	if got := other.approvals("LEGION"); got.Unrecorded != NoDefaultAssignee {
		t.Fatalf("a read of a Dispatch with no default assignee gives the signal %+v, want it not recorded for %q", got, NoDefaultAssignee)
	}
	other.read(ApprovalsRead{DefaultAssignee: "dana", DefaultAt: readAt})
	other.ask(1, openedAsk("ask", "LEGION-1", "dana"))
	if got := other.waiting(); !slices.Equal(got, []string{"ask"}) {
		t.Fatalf("with a default from the read the ask waits on %v, want it counted", got)
	}
	other.defaultAssignee("erin", 3)
	if got := other.waiting(); len(got) != 0 {
		t.Fatalf("the first event after a read-sourced default is kept whatever its sequence: the asks that wait on dana = %v, want none", got)
	}
}

// The boot's read of an ask is kept where no event past the sequence it stands at was kept, and
// each ask the store held open that the read found closed is closed. An issue's assignee is read
// the same way, and the rows the read names twice are one, the newer.
func TestTheBootsReadOfAsksAndAssigneesYieldsToWhatTheStreamKeptPastIt(t *testing.T) {
	r := newApprovalsRig(t)
	r.defaultAssignee("dana", 1)
	r.ask(10, openedAsk("past-the-read", "LEGION-1", ""))
	r.ask(5, openedAsk("closed-while-down", "LEGION-1", ""))
	r.ask(5, openedAsk("handed-on-while-down", "LEGION-1", ""))
	ask := func(id string, open bool, askedOf string, seq int64) ApprovalAsk {
		return ApprovalAsk{Ask: signals.Ask{ID: id, Issue: "LEGION-1", Open: open, Human: true, AskedOf: askedOf}, Seq: seq}
	}
	var held []ApprovalAsk
	r.tx(func(ctx context.Context, tx pgx.Tx) (err error) {
		held, err = r.records.OpenApprovalAsks(ctx, tx, "LEGION")
		return err
	})
	if len(held) != 3 || held[0].ID != "closed-while-down" || held[1].ID != "handed-on-while-down" || held[2].ID != "past-the-read" || held[0].Seq != 5 {
		t.Fatalf("the open asks held = %+v, want the three by id, the first at sequence 5", held)
	}
	r.read(ApprovalsRead{
		Asks: []ApprovalAsk{
			ask("past-the-read", false, "", 8), // the read stood at 8, and event 10 was kept: the event stands
			ask("closed-while-down", false, "", 8),
			ask("handed-on-while-down", true, "erin", 8),
			ask("opened-while-down", true, "", 8),
		},
		Assignees: []IssueAssignee{{Issue: "LEGION-1", Assignee: "old", Seq: 4}, {Issue: "LEGION-1", Assignee: "", Seq: 8}, {Issue: "LEGION-2", Assignee: "carol", Seq: 3}},
	})
	if got, want := r.waiting(), []string{"opened-while-down", "past-the-read"}; !slices.Equal(got, want) {
		t.Fatalf("after the read the asks that wait on dana = %v, want %v: the one an event 10 kept, the one the read found open, none of the closed or handed on", got, want)
	}
	// What the read held at 8 is stood on by a repeat of it, and an event 9 older than event 10 is
	// refused for the ask it was kept past.
	r.ask(9, askEvent(signals.AskAnswered, "past-the-read", "LEGION-1", signals.ReportsClosed))
	if got := r.waiting(); !slices.Equal(got, []string{"opened-while-down", "past-the-read"}) {
		t.Fatalf("an event older than the one kept moved the count: %v", got)
	}
	r.ask(9, askEvent(signals.AskAnswered, "opened-while-down", "LEGION-1", signals.ReportsClosed))
	if got := r.waiting(); !slices.Equal(got, []string{"past-the-read"}) {
		t.Fatalf("an event past the read's sequence closes the ask the read opened: %v", got)
	}
	var assignee string
	r.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "select assignee from approval_assignees where project = 'LEGION' and issue = 'LEGION-1'").Scan(&assignee)
	})
	if assignee != "" {
		t.Fatalf("LEGION-1's assignee = %q, want the newer of the two the read named, nobody", assignee)
	}
}

// A failed boot read of Dispatch is the approvals input's read_error, and the signal is not
// recorded with that reason while it stands; a read that succeeded clears it, and the count the
// store held stands again.
func TestAFailedBootReadMakesTheApprovalsSignalUnrecordedWithItsReason(t *testing.T) {
	r := newApprovalsRig(t)
	r.defaultAssignee("dana", 1)
	r.ask(1, openedAsk("ask", "LEGION-1", ""))
	at := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	r.tx(func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.BeginSignalInput(ctx, tx, "LEGION", signals.Approvals, at); err != nil {
			return err
		}
		return r.records.SetSignalInputReadError(ctx, tx, "LEGION", signals.Approvals, "read the open asks of LEGION: Dispatch is down", at.Add(time.Hour))
	})
	got := r.approvals("LEGION")
	if got.Unrecorded != "read the open asks of LEGION: Dispatch is down" || got.Value().Recorded {
		t.Fatalf("with a failed read the signal = %+v, want it not recorded for the read's reason", got)
	}
	r.tx(func(ctx context.Context, tx pgx.Tx) error {
		return r.records.SetSignalInputReadError(ctx, tx, "LEGION", signals.Approvals, "", at.Add(2*time.Hour))
	})
	if got := r.waiting(); !slices.Equal(got, []string{"ask"}) {
		t.Fatalf("after a read succeeded the asks that wait on dana = %v, want the one kept", got)
	}
	var since time.Time
	r.tx(func(ctx context.Context, tx pgx.Tx) (err error) {
		since, _, err = r.records.SignalInputSince(ctx, tx, "LEGION", signals.Approvals)
		return err
	})
	if !since.Equal(at) {
		t.Fatalf("the approvals input has been kept since %s, want %s: a read's outcome never moves it", since, at)
	}
}
