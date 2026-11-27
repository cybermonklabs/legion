package holdrecord_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/holdrecord/holdrecordtest"
	"github.com/sjawhar/envoy/natsgrant"
)

// signedMove sends PUT /v1/nats-hold as an operator process whose clock reads signedAt would: the
// move from expectedGen to phase and holdID, signed at signedAt with the rig's key. It answers the
// status and the refusal's code and message ("" on a 200).
func signedMove(t *testing.T, r rig, expectedGen uint64, phase natsgrant.OperatorPhase, holdID string, signedAt time.Time) (int, string, string) {
	t.Helper()
	signedAt = signedAt.UTC().Truncate(time.Microsecond)
	sig, err := signedrecord.Sign(r.SigningKey, signedrecord.HoldRow(expectedGen+1, string(phase), holdID, signedAt))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"expected_gen": expectedGen, "phase": phase, "hold_id": holdID, "reason": "a skewed clock",
		"signed_at": signedAt.Format(time.RFC3339Nano), "sig": sig})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, r.URL+"/v1/nats-hold", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+holdrecordtest.OperatorToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var refusal struct{ Error, Message string }
	if resp.StatusCode != http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&refusal); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, refusal.Error, refusal.Message
}

// clockRows are TestTheHoldJournal's rows of the clock-skew allowance (signedrecord.SignedAtSkew):
// a row signed further than it from the broker's clock, or further than it before the latest row
// the broker took, is refused 409 clock_skew before it is written, so the table is unchanged and
// no client is refused; a row within it is stored.
func clockRows(t *testing.T) {
	ctx := context.Background()
	table := func(t *testing.T, r rig) (int64, string) {
		t.Helper()
		var gen int64
		var phase string
		if err := r.Pool.QueryRow(ctx, `select gen, phase from nats_hold`).Scan(&gen, &phase); err != nil {
			t.Fatal(err)
		}
		return gen, phase
	}

	t.Run("a move signed 10 minutes fast is refused before it is written, and no client is refused", func(t *testing.T) {
		// Fails while the route judges the new row's signed_at only after the commit: the move is
		// stored, the next correct-clock move is refused 409, and the view reads unknown.
		r := newRig(t)
		c := r.client(t)
		current := row(t, c)
		status, code, message := signedMove(t, r, current.Gen, natsgrant.OperatorPhasePrepare, "h-fast", time.Now().Add(10*time.Minute))
		if status != http.StatusConflict || code != "clock_skew" || !strings.Contains(message, "signed at") {
			t.Errorf("a move signed 10 minutes fast answered %d %s %q, want 409 clock_skew naming both times", status, code, message)
		}
		if gen, phase := table(t, r); gen != int64(current.Gen) || phase != "released" {
			t.Errorf("after the refused move the table holds %s at gen %d, want released at gen %d", phase, gen, current.Gen)
		}
		if phase, known := r.HoldView(false).State(ctx); !known || phase != natsgrant.OperatorPhaseReleased {
			t.Errorf("after the refused move the view reads %q, known %v; want released, known", phase, known)
		}
		if moved, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhasePrepare, "h-fast", "a correct clock"); err != nil {
			t.Errorf("the correct-clock move after it answered %+v, %v; want it stored", moved, err)
		}
	})

	t.Run("a move signed 6 minutes before the latest row the broker took is refused before it is written", func(t *testing.T) {
		// The floor's row is signed 4 minutes fast, inside the allowance of the broker's clock; the
		// next is signed 2 minutes slow, inside it too, but 6 minutes before the floor's row. Fails
		// while the route judges that only after the commit: held is stored at gen 3, and the move
		// is answered 409 below_floor.
		r := newRig(t)
		current := row(t, r.client(t))
		fast := time.Now().Add(4 * time.Minute)
		if status, code, message := signedMove(t, r, current.Gen, natsgrant.OperatorPhasePrepare, "h-floor", fast); status != http.StatusOK {
			t.Fatalf("the floor's row, signed 4 minutes fast, answered %d %s %q", status, code, message)
		}
		status, code, message := signedMove(t, r, current.Gen+1, natsgrant.OperatorPhaseHeld, "h-floor", time.Now().Add(-2*time.Minute))
		if status != http.StatusConflict || code != "clock_skew" || !strings.Contains(message, fast.UTC().Format(time.RFC3339)) {
			t.Errorf("a move signed 6 minutes before the floor's row answered %d %s %q, want 409 clock_skew naming the floor's time", status, code, message)
		}
		if gen, phase := table(t, r); gen != int64(current.Gen+1) || phase != "prepare" {
			t.Errorf("after the refused move the table holds %s at gen %d, want prepare at gen %d", phase, gen, current.Gen+1)
		}
		if phase, known := r.HoldView(false).State(ctx); !known || phase != natsgrant.OperatorPhasePrepare {
			t.Errorf("after the refused move the view reads %q, known %v; want prepare, known", phase, known)
		}
	})

	t.Run("a move signed 4 minutes before the latest row the broker took is stored", func(t *testing.T) {
		r := newRig(t)
		current := row(t, r.client(t))
		if status, code, message := signedMove(t, r, current.Gen, natsgrant.OperatorPhasePrepare, "h-within", time.Now().Add(2*time.Minute)); status != http.StatusOK {
			t.Fatalf("the floor's row, signed 2 minutes fast, answered %d %s %q", status, code, message)
		}
		if status, code, message := signedMove(t, r, current.Gen+1, natsgrant.OperatorPhaseHeld, "h-within", time.Now().Add(-2*time.Minute)); status != http.StatusOK {
			t.Errorf("a move signed 4 minutes before the floor's row answered %d %s %q, want it stored", status, code, message)
		}
		if gen, phase := table(t, r); gen != int64(current.Gen+2) || phase != "held" {
			t.Errorf("the table holds %s at gen %d, want held at gen %d", phase, gen, current.Gen+2)
		}
	})

	t.Run("three clocks across two replicas: a row the other replica would read as unknown is refused before it is written", func(t *testing.T) {
		// Legion N-A, Claude P3-1: through the second replica, machine A (4m30s fast) writes gen 2
		// and machine B (correct) gen 3, so the second replica's floor holds A's time; the first
		// replica restarts, and machine C (4m54s slow) moves through it. Fails while the route judges
		// C's row against the serving replica's floor alone: it is stored, and the second replica's
		// view then reads it as unknown.
		r := newRig(t)
		current := row(t, r.client(t))
		r.UseReplica(true)
		if status, code, message := signedMove(t, r, current.Gen, natsgrant.OperatorPhasePrepare, "h-three", time.Now().Add(4*time.Minute+30*time.Second)); status != http.StatusOK {
			t.Fatalf("machine A's move, 4m30s fast, answered %d %s %q", status, code, message)
		}
		if status, code, message := signedMove(t, r, current.Gen+1, natsgrant.OperatorPhaseHeld, "h-three", time.Now()); status != http.StatusOK {
			t.Fatalf("machine B's move, correct, answered %d %s %q", status, code, message)
		}
		view := r.HoldView(true)
		if phase, known := view.State(ctx); !known || phase != natsgrant.OperatorPhaseHeld {
			t.Fatalf("the second replica's view reads %q, known %v; want held", phase, known)
		}
		r.UseReplica(false)
		r.Restart()
		status, code, message := signedMove(t, r, current.Gen+2, natsgrant.OperatorPhaseReleased, "h-three", time.Now().Add(-4*time.Minute-54*time.Second))
		if status != http.StatusConflict || code != "clock_skew" || !strings.Contains(message, "before gen 2 signed at") {
			t.Errorf("machine C's move, 4m54s slow, through the restarted replica answered %d %s %q, want 409 clock_skew naming gen 2", status, code, message)
		}
		if gen, phase := table(t, r); gen != int64(current.Gen+2) || phase != "held" {
			t.Errorf("after machine C's move the table holds %s at gen %d, want held at gen %d", phase, gen, current.Gen+2)
		}
		if phase, known := view.State(ctx); !known || phase != natsgrant.OperatorPhaseHeld {
			t.Errorf("after machine C's move the second replica's view reads %q, known %v; want held, known", phase, known)
		}
	})

	t.Run("a row signed 20 minutes ahead, stored before the routes judged signing times, holds no correct clock back", func(t *testing.T) {
		// Claude P3-2: a row and its log entry signed far ahead (a row stored before this check, or
		// after the broker's clock jumped back) are read, so the floor and the log hold that time.
		// Fails while a correct-clock move is judged against it as it is: 409 clock_skew, on every
		// move, and no restart clears it, since the log keeps the time.
		r := newRig(t)
		c := r.client(t)
		current := row(t, c)
		ahead := time.Now().Add(20 * time.Minute).UTC().Truncate(time.Microsecond)
		gen := current.Gen + 1
		sig, err := signedrecord.Sign(r.SigningKey, signedrecord.HoldRow(gen, string(natsgrant.OperatorPhasePrepare), "h-ahead", ahead))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Pool.Exec(ctx, `with moved as (
				update nats_hold set gen = $1, phase = 'prepare', hold_id = 'h-ahead', signed_at = $2, sig = $3 returning gen, phase, hold_id, reason, updated_at, updated_by, signed_at, sig)
			insert into nats_hold_log (gen, phase, hold_id, reason, written_at, written_by, signed_at, sig)
			select gen, phase, hold_id, reason, updated_at, updated_by, signed_at, sig from moved`, int64(gen), ahead, sig); err != nil {
			t.Fatal(err)
		}
		if got, err := c.Row(ctx); err != nil || got.Gen != gen {
			t.Fatalf("Row of the row signed ahead answered %+v, %v", got, err)
		}
		r.Restart()
		if _, err := c.Row(ctx); err != nil {
			t.Fatal(err)
		}
		if moved, err := c.MoveRow(ctx, gen, natsgrant.OperatorPhaseHeld, "h-ahead", "a correct clock"); err != nil {
			t.Errorf("a correct-clock move after the row signed ahead answered %+v, %v; want it stored", moved, err)
		}
		if phase, known := r.HoldView(false).State(ctx); !known || phase != natsgrant.OperatorPhaseHeld {
			t.Errorf("after the move the view reads %q, known %v; want held, known", phase, known)
		}
	})
}
