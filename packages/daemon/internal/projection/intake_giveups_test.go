package projection

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// The state always carries the project's open give-ups, read from the record in the state's own
// snapshot: each consumer's count of the records no replay or discard has closed and the newest
// stream sequence among them, and the newest stateNewestGiveUps of them, latest given up first,
// each with the start of its error, cut on a rune boundary. A closed record is neither counted nor
// listed, and another project's daemon's records are not this project's. A project with none open
// carries the key with its two lists empty.
func TestTheStateCountsAndListsTheProjectsOpenGiveUps(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.Database(t, "legion_projection_giveups"))
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	records := record.NewStore()
	const consumer = "legion-go-LEGION-dispatch"
	base := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	message := func(project string, seq uint64) record.IntakeMessage {
		return record.IntakeMessage{Project: project, Consumer: "legion-go-" + project + "-dispatch",
			Subject: fmt.Sprintf("notifications.dispatch.issue.%s-%d.issue.updated", project, seq), StreamSeq: seq,
			StoredAt: base.Add(-time.Hour), Payload: []byte(`{}`), EventSource: "dispatch", EventID: fmt.Sprintf("dispatch-%d", seq)}
	}
	giveUp := func(m record.IntakeMessage, at time.Time) int64 {
		t.Helper()
		var id int64
		if err := st.Tx(ctx, func(tx pgx.Tx) error {
			failure, err := records.NoteFailure(ctx, tx, m, 21, "the handler failed", base)
			if err != nil {
				return err
			}
			given, gaveUp, err := records.GiveUpIntakeMessage(ctx, tx, m.Project, failure.ID, record.GiveUpBound, "the handler failed", at)
			if err == nil && !gaveUp {
				err = fmt.Errorf("message %d had no open failure", m.StreamSeq)
			}
			id = given.ID
			return err
		}); err != nil {
			t.Fatalf("give up %s's message %d: %v", m.Project, m.StreamSeq, err)
		}
		return id
	}

	// Fifty-one decoded messages of one consumer, given up a minute apart, then the newest of all,
	// a message that did not decode, whose error is 400 characters of three bytes each.
	var decoded []int64
	for i := range 51 {
		decoded = append(decoded, giveUp(message("LEGION", uint64(100+i)), base.Add(time.Duration(i)*time.Minute)))
	}
	longError := strings.Repeat("日", 400)
	undecodable := message("LEGION", 200)
	undecodable.EventSource, undecodable.EventID = "", ""
	var newest record.IntakeFailure
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		newest, _, err = records.RecordUndecodable(ctx, tx, undecodable, 1, longError, base.Add(time.Hour))
		return err
	}); err != nil {
		t.Fatalf("record the undecodable message: %v", err)
	}
	// One more of the same consumer, given up after them all and then discarded: its stream
	// sequence, 300, is above every open one's.
	closed := giveUp(message("LEGION", 300), base.Add(2*time.Hour))
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		_, err := records.DiscardIntakeGiveUps(ctx, tx, "LEGION", record.IntakeSelector{ID: closed}, "operator", "a test discard", time.Minute, base.Add(3*time.Hour))
		return err
	}); err != nil {
		t.Fatalf("discard give-up %d: %v", closed, err)
	}
	// Another project's daemon's give-up, in the same database.
	theirs := giveUp(message("OTHER", 150), base.Add(4*time.Hour))

	project := func(name string) api.State {
		t.Helper()
		var state api.State
		if err := st.Tx(ctx, func(tx pgx.Tx) error {
			var err error
			state, err = Project(ctx, tx, records, name, nil, nil, nil, nil, nil, base.Add(5*time.Hour))
			return err
		}); err != nil {
			t.Fatalf("project %s's state: %v", name, err)
		}
		return state
	}
	state := project("LEGION")
	listed := make([]int64, len(state.IntakeGiveUps.Newest))
	for i, view := range state.IntakeGiveUps.Newest {
		listed[i] = view.ID
	}

	t.Run("none open answers both lists empty", func(t *testing.T) {
		encoded, err := json.Marshal(project("EMPTY").IntakeGiveUps)
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"consumers":[],"newest":[]}`; string(encoded) != want {
			t.Errorf("a project with no give-up open carries %s, want %s", encoded, want)
		}
	})
	t.Run("fifty-two open of one consumer are counted whole and the fifty newest listed", func(t *testing.T) {
		want := []api.IntakeGiveUpCountView{{Name: consumer, Open: 52, NewestSeq: 200}}
		if !slices.Equal(state.IntakeGiveUps.Consumers, want) {
			t.Fatalf("the consumers = %+v, want %+v", state.IntakeGiveUps.Consumers, want)
		}
		if len(listed) != stateNewestGiveUps {
			t.Fatalf("the state lists %d give-ups, want the %d newest", len(listed), stateNewestGiveUps)
		}
		wantListed := []int64{newest.ID}
		for i := len(decoded) - 1; len(wantListed) < stateNewestGiveUps; i-- {
			wantListed = append(wantListed, decoded[i])
		}
		if !slices.Equal(listed, wantListed) {
			t.Fatalf("the state lists %v, want the newest given up first, %v", listed, wantListed)
		}
		eventID := "dispatch-150"
		wantView := api.IntakeGiveUpView{ID: decoded[50], Consumer: consumer, Subject: "notifications.dispatch.issue.LEGION-150.issue.updated",
			StreamSeq: 150, StoredAt: base.Add(-time.Hour), EventID: &eventID, Deliveries: 21, GaveUpAt: base.Add(50 * time.Minute),
			Reason: string(record.GiveUpBound), LastError: "the handler failed"}
		got := state.IntakeGiveUps.Newest[1]
		if got.EventID == nil || *got.EventID != eventID {
			t.Fatalf("the second listed give-up's event id = %v, want %q", got.EventID, eventID)
		}
		got.EventID = wantView.EventID
		if got != wantView {
			t.Errorf("the second listed give-up = %+v, want %+v", got, wantView)
		}
	})
	t.Run("a closed record is neither counted nor listed", func(t *testing.T) {
		if slices.Contains(listed, closed) {
			t.Errorf("the state lists the discarded give-up %d", closed)
		}
	})
	t.Run("another project's record is neither counted nor listed", func(t *testing.T) {
		if slices.Contains(listed, theirs) {
			t.Errorf("LEGION's state lists OTHER's give-up %d", theirs)
		}
		for _, count := range state.IntakeGiveUps.Consumers {
			if count.Name != consumer {
				t.Errorf("LEGION's state counts the give-ups of %s", count.Name)
			}
		}
	})
	t.Run("an error of 400 multi-byte characters is cut to 300 on a rune boundary", func(t *testing.T) {
		got := state.IntakeGiveUps.Newest[0]
		if got.ID != newest.ID || got.EventID != nil || got.Reason != string(record.GiveUpUndecodable) || got.Deliveries != 1 {
			t.Fatalf("the newest listed give-up = %+v, want the undecodable message %d with a null event id", got, newest.ID)
		}
		if !utf8.ValidString(got.LastError) || got.LastError != strings.Repeat("日", stateGiveUpErrorRunes) {
			t.Errorf("the listed error is %d bytes, %d runes, valid UTF-8 %t; want the first %d characters whole",
				len(got.LastError), utf8.RuneCountInString(got.LastError), utf8.ValidString(got.LastError), stateGiveUpErrorRunes)
		}
	})
}
