package store

import (
	"context"
	"slices"
	"testing"
)

// Which of R6's lines are open outlives the daemon that wrote them (fleetalert.Writer.Load): each
// project keeps its own, a line is kept once however often it is put, and a deleted line, or one of
// another project, is read back by nobody.
func TestFleetAlertLinesAreKeptOpenAcrossARestartUntilDeleted(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	for _, line := range []FleetAlertLine{
		{Project: "legion", Case: "controller_unreachable", Claim: "legion-legion-controller"},
		{Project: "legion", Case: "envoy_no_live_session"},
		{Project: "legion", Case: "envoy_no_live_session"},
		{Project: "other", Case: "envoy_no_live_session"},
	} {
		if err := st.PutFleetAlertLine(ctx, line); err != nil {
			t.Fatalf("put the line %+v: %v", line, err)
		}
	}

	read, err := reopened(t, st).FleetAlertLines(ctx, "legion")
	if err != nil {
		t.Fatalf("read the open lines after a restart: %v", err)
	}
	want := []FleetAlertLine{
		{Project: "legion", Case: "controller_unreachable", Claim: "legion-legion-controller"},
		{Project: "legion", Case: "envoy_no_live_session"},
	}
	if !slices.Equal(read, want) {
		t.Fatalf("the open lines of legion after a restart = %+v, want %+v", read, want)
	}

	if err := st.DeleteFleetAlertLine(ctx, want[0]); err != nil {
		t.Fatalf("delete the line %+v: %v", want[0], err)
	}
	if err := st.DeleteFleetAlertLine(ctx, want[0]); err != nil {
		t.Fatalf("delete a line no longer open: %v", err)
	}
	if read, err = reopened(t, st).FleetAlertLines(ctx, "legion"); err != nil || !slices.Equal(read, want[1:]) {
		t.Fatalf("the open lines of legion after a delete = %+v, %v; want %+v", read, err, want[1:])
	}
}
