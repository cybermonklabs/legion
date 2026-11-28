package replay

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// behindEntryLine is the line of admission's send of the work of the entry a role's phases row
// names to a role whose claim was not given it (admit's resumeSlottedWorkers).
const behindEntryLine = "admission: a role was behind the entry its phases row names, so that entry's work is sent again"

// resumedRootWindow is rootAwaitingPersonWindow with a pass after the resume: at 11:00, after the
// architect's turn of the answer (10:20 to 10:35), a person retitles ACME-1.
func resumedRootWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	rootAwaitingPerson(b, b.session("ACME-1", claim.RoleArchitect), false, nil)
	b.edit(clock(11, 0), "ACME-1", func(issue *builtIssue) { issue.Title = "Export the widget list as a sheet" })
	return b.build()
}

// The resume of a root a person answered (workflow's ResumeAwaitedRoot, and the entry sweep's send
// of the same start, SendArchitectStart) queues its architect a start that carries the wait the
// answer ends (record.SuperviseRequest.Awaiting), the entry the resume raised and no text, which
// the daemon writes at the task's send. The daemon's outbox makes that start a task as it makes a
// start with a text: the claim is queued a delivery of that entry whose Resume is the wait, encoded
// as the outbox encodes it, and is given the entry. The test replays resumedRootWindow under main
// and reads ACME-1's architect at each instant the replay settles. At the first instant whose
// architect's row holds a wait (record.PhaseRow.EntryAwaiting), the 10:20 pass of the person's
// approval, the claim holds the delivery of the row's entry with the row's wait, and from then on
// it is given the row's entry; and no pass, the retitle's at 11:00 among them, sends the architect
// that entry again. A replay that gives a claim only a start with a text leaves the claim at the
// entry of its promotion, and every pass after the resume sends the start again.
func TestAResumedRootsArchitectIsGivenTheEntryOfTheResume(t *testing.T) {
	t.Parallel()
	architect := builtToken(t, "ACME-1", claim.RoleArchitect)
	records := record.NewStore()
	type reading struct {
		at    time.Time
		row   record.PhaseRow
		claim *supervise.Claim
	}
	var readings []reading
	out := t.TempDir()
	cfg := RunConfig{Window: resumedRootWindow(t), Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1, PGDSN: replayDSN(t), Out: out}
	cfg.atSettled = func(ctx context.Context, st *store.Store, at time.Time) error {
		return store.TransactWith(ctx, st.Pool(), pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := records.Phases(ctx, tx, "ACME-1")
			if err != nil {
				return err
			}
			claims, err := st.ClaimsTx(ctx, tx)
			if err != nil {
				return err
			}
			read := reading{at: at, row: record.PhaseRowOf(rows, claim.RoleArchitect)}
			if i := slices.IndexFunc(claims, func(c supervise.Claim) bool { return c.Token == architect }); i >= 0 {
				read.claim = &claims[i]
			}
			readings = append(readings, read)
			return nil
		})
	}
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("replay the window under main: %v", err)
	}
	raised := slices.IndexFunc(readings, func(r reading) bool { return r.row.EntryAwaiting != nil })
	if raised < 0 {
		t.Fatal("no instant of the replay shows ACME-1's architect at an entry the resume of the person's answer raised")
	}
	// waitOf is a wait as the outbox encodes it (internal/daemon's resumeJSON), read from raw: the
	// claim store keeps it as Postgres keeps JSON, so it is decoded and encoded again.
	waitOf := func(raw []byte) string {
		if len(raw) == 0 {
			return ""
		}
		var wait record.Awaiting
		if err := json.Unmarshal(raw, &wait); err != nil {
			t.Fatalf("decode the wait %s: %v", raw, err)
		}
		encoded, err := json.Marshal(wait)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	resume := readings[raised]
	want, err := json.Marshal(resume.row.EntryAwaiting)
	if err != nil {
		t.Fatal(err)
	}
	switch c := resume.claim; {
	case c == nil:
		t.Fatalf("at %s ACME-1's architect stands at entry %d of the resume and has no claim, want one given that entry",
			describeTimes([]time.Time{resume.at}), resume.row.Entry)
	case c.Pending == nil || c.Pending.Entry != resume.row.Entry || waitOf(c.Pending.Resume) != string(want):
		t.Errorf("at %s ACME-1's architect stands at entry %d of the resume and its claim holds %+v, want the delivery of entry %d whose wait is %s",
			describeTimes([]time.Time{resume.at}), resume.row.Entry, c.Pending, resume.row.Entry, want)
	}
	for _, r := range readings[raised:] {
		if r.claim == nil || r.claim.GivenEntry < r.row.Entry {
			given := int64(0)
			if r.claim != nil {
				given = r.claim.GivenEntry
			}
			t.Errorf("at %s ACME-1's architect stands at entry %d and its claim was given entry %d, want it given that entry",
				describeTimes([]time.Time{r.at}), r.row.Entry, given)
		}
	}
	if !slices.ContainsFunc(readings[raised:], func(r reading) bool { return !r.at.Before(clock(11, 0)) }) {
		t.Error("the replay settled at no instant from the retitle at 11:00 on, so no pass ran after the resume")
	}
	behind := linesAt(replayLog(t, out), func(line replayLine) bool {
		return line.Msg == behindEntryLine && line.Issue == "ACME-1" && line.Role == string(claim.RoleArchitect)
	})
	if len(behind) != 0 {
		t.Errorf("the replay sent ACME-1's architect the work of its entry again at %s, want never: its start gave the claim that entry",
			describeTimes(behind))
	}
}
