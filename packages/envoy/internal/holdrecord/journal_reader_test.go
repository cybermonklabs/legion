package holdrecord_test

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/holdrecord"
)

// readerRows are TestTheHoldJournal's rows of the journal's reader (Done.Latest, Entry.Left): what
// the writers wrote reads back typed, so no caller reads the journal's strings itself.
func readerRows(t *testing.T) {
	ctx := context.Background()

	t.Run("each stop's record of what it left round-trips through the reader, LeftUnchanged included", func(t *testing.T) {
		// Five stops of one command, LeftUnchanged first, one with nothing recorded (AppendStop), and
		// the last at index 11, so a reader that orders the abort's records as strings ("left.11"
		// before "left.3") fails, as does one that drops an item, the last among them.
		r := newRig(t)
		c := r.client(t)
		const holdID, command = "h-reader", "create-bucket"
		hold(t, c, holdID, holdrecord.WorkflowCutover, nil)
		recorded := []string{holdrecord.LeftUnchanged, "", "deleted KV_a", "deleted KV_a and KV_b", "deleted KV_c"}
		for _, left := range recorded {
			if _, err := c.Admit(ctx, holdID, cmd(command), nil); err != nil {
				t.Fatal(err)
			}
			var err error
			if left == "" {
				err = c.AppendStop(ctx, holdID, command, "refused")
			} else {
				err = c.AppendStopLeft(ctx, holdID, command, "refused", left)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := c.Admit(ctx, holdID, cmd(holdrecord.CommandAbort), map[string]string{"left.99": "not a stop's"}); err != nil {
			t.Fatal(err)
		}
		d, _ := journal(t, c, holdID)
		var want []holdrecord.LeftItem
		for i, left := range recorded {
			want = append(want, holdrecord.LeftItem{Stop: 3 + 2*i, Command: command, Left: left})
		}

		stop, ok := d.Latest(command)
		if !ok || stop.State != holdrecord.StateStop || stop.Index != 11 {
			t.Fatalf("Latest(%q) = %+v, %v; want the stop at index 11", command, stop, ok)
		}
		if got, ok := stop.Left(); !ok || !slices.Equal(got, want[len(want)-1:]) {
			t.Errorf("the latest stop's Left() = %+v, %v; want %+v", got, ok, want[len(want)-1:])
		}
		abort, ok := d.Latest(holdrecord.CommandAbort, holdrecord.StateIntent)
		if !ok {
			t.Fatalf("Latest(%q, intent) found no entry in %+v", holdrecord.CommandAbort, d.Ops)
		}
		if got, ok := abort.Left(); !ok || !slices.Equal(got, want) {
			t.Errorf("hold abort's intent's Left() =\n%+v, %v; want\n%+v", got, ok, want)
		}
		if got, ok := abort.Left(); ok && len(got) > 0 && got[0].Left != holdrecord.LeftUnchanged {
			t.Errorf("the first stop's record reads %q, want LeftUnchanged", got[0].Left)
		}
		if _, ok := d.Latest(command, holdrecord.StateDone); ok {
			t.Errorf("Latest(%q, done) found an entry; the command never ran to its done", command)
		}
		if set, ok := d.Latest(holdrecord.CommandSet); !ok || set.State != holdrecord.StateDone {
			t.Errorf("Latest(%q) = %+v, %v; want its done entry", holdrecord.CommandSet, set, ok)
		}
		if items, ok := set0(d).Left(); ok {
			t.Errorf("hold set's intent answers Left() %+v; only a stop or hold abort's intent carries a record", items)
		}
		if d.CutRun(command) {
			t.Errorf("CutRun(%q) on five runs that each ended with a stop answered true", command)
		}
	})

	t.Run("CutRun answers a run that ended with no entry, and the stop after it cannot record LeftUnchanged", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		const holdID, command = "h-reader-cut", "create-bucket"
		hold(t, c, holdID, holdrecord.WorkflowCutover, nil)
		for range 2 { // the second admission is the rerun of a run cut after its intent
			if _, err := c.Admit(ctx, holdID, cmd(command), nil); err != nil {
				t.Fatal(err)
			}
		}
		d, _ := journal(t, c, holdID)
		if !d.CutRun(command) || d.CutRun(holdrecord.CommandSet) {
			t.Errorf("CutRun(%q) = %v and CutRun(%q) = %v; want true and false", command, d.CutRun(command), holdrecord.CommandSet, d.CutRun(holdrecord.CommandSet))
		}
		if err := c.AppendStopLeft(ctx, holdID, command, "refused", holdrecord.LeftUnchanged); err == nil {
			t.Errorf("AppendStopLeft(LeftUnchanged) after a cut run answered nil")
		}
	})
}

// set0 is the journal's first entry, hold set's intent.
func set0(d holdrecord.Done) holdrecord.Entry {
	return holdrecord.Entry{Index: 0, Operation: d.Ops[0]}
}
