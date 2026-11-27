package cistore

//ci:reads packages/contracts/fixtures/checks-settlement/published.json readPublishedFixture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// publishedFixture is packages/contracts/fixtures/checks-settlement/published.json, which the
// Legion daemon's fake GitHub test reads too (packages/daemon/internal/githubtest): that fake
// copies the listener's settlement, the subject segment and Dispatch's delivery durable by hand,
// since the daemon module cannot import this module's internal packages. Here the listener's own
// code makes each value, and there the copies make the same, so a change of either that the other
// does not take fails one of the two tests.
type publishedFixture struct {
	Settlements []struct {
		Owner      string `json:"owner"`
		Repo       string `json:"repo"`
		Number     int    `json:"number"`
		SHA        string `json:"sha"`
		Generation uint64 `json:"generation"`
		Superseded bool   `json:"superseded"`
		CheckRuns  []struct {
			Name       string `json:"name"`
			ID         uint64 `json:"id"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			URL        string `json:"url"`
		} `json:"check_runs"`
		Statuses []struct {
			Context   string `json:"context"`
			State     string `json:"state"`
			CreatedAt string `json:"created_at"`
		} `json:"statuses"`
		Topic     string          `json:"topic"`
		DedupeKey string          `json:"dedupe_key"`
		Payload   json.RawMessage `json:"payload"`
	} `json:"settlements"`
	SubjectSegments []struct {
		Value   string `json:"value"`
		Segment string `json:"segment"`
	} `json:"subject_segments"`
	DispatchDeliveryDurable string `json:"dispatch_delivery_durable"`
}

// readPublishedFixture reads the shared fixture, and fails the test where it holds no settlement,
// no subject segment or no durable, since a check of nothing would pass.
func readPublishedFixture(t *testing.T) publishedFixture {
	t.Helper()
	path := filepath.Join("..", "..", "..", "contracts", "fixtures", "checks-settlement", "published.json")
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var fixture publishedFixture
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if len(fixture.Settlements) == 0 || len(fixture.SubjectSegments) == 0 || fixture.DispatchDeliveryDurable == "" {
		t.Fatalf("UNKNOWN: %s holds %d settlements, %d subject segments and the durable %q, so nothing was checked against it",
			path, len(fixture.Settlements), len(fixture.SubjectSegments), fixture.DispatchDeliveryDurable)
	}
	return fixture
}

// The listener publishes each settlement of the shared fixture, from a record of its check runs
// and commit statuses, on the fixture's subject, with its dedupe key and its payload
// (settlementEnvelope); the contracts write each name as the subject segment the fixture names;
// and Dispatch's delivery intake reads the durable the fixture names.
func TestTheListenerPublishesTheSharedFixturesSettlements(t *testing.T) {
	t.Parallel()
	fixture := readPublishedFixture(t)
	for _, want := range fixture.Settlements {
		t.Run(fmt.Sprintf("the settlement of %s/%s#%d at %.7s, generation %d", want.Owner, want.Repo, want.Number, want.SHA, want.Generation), func(t *testing.T) {
			t.Parallel()
			state := State{Owner: want.Owner, Repo: want.Repo, Number: strconv.Itoa(want.Number), SHA: want.SHA,
				Checks: map[string]Check{}, Statuses: map[string]Status{}, Generation: want.Generation}
			if want.Superseded {
				state.EmittedCount = 1
			}
			for _, run := range want.CheckRuns {
				state.Checks[run.Name] = Check{Name: run.Name, CheckRunID: checkRunID(run.ID), URL: run.URL, Status: run.Status,
					Conclusion: run.Conclusion}
			}
			for _, status := range want.Statuses {
				state.Statuses[status.Context] = Status{Context: status.Context, State: status.State, CreatedAt: status.CreatedAt}
			}
			envelope, err := settlementEnvelope(state, time.Now().UnixMilli())
			if err != nil {
				t.Fatalf("make the settlement's envelope: %v", err)
			}
			if envelope.Topic != want.Topic {
				t.Errorf("the subject is %q, want the fixture's %q", envelope.Topic, want.Topic)
			}
			if envelope.DedupeKey != want.DedupeKey {
				t.Errorf("the dedupe key is %q, want the fixture's %q", envelope.DedupeKey, want.DedupeKey)
			}
			if got, wanted := publishedCanonical(t, []byte(envelope.Payload)), publishedCanonical(t, want.Payload); got != wanted {
				t.Errorf("the payload is\n%s\nwant the fixture's\n%s", got, wanted)
			}
		})
	}
	t.Run("the subject segments", func(t *testing.T) {
		t.Parallel()
		for _, want := range fixture.SubjectSegments {
			if got := contracts.SanitizeSubjectSegment(want.Value); got != want.Segment {
				t.Errorf("SanitizeSubjectSegment(%q) = %q, want the fixture's %q", want.Value, got, want.Segment)
			}
		}
	})
	t.Run("the delivery durable", func(t *testing.T) {
		t.Parallel()
		if bus.DispatchDeliveryDurable != fixture.DispatchDeliveryDurable {
			t.Errorf("DispatchDeliveryDurable = %q, want the fixture's %q", bus.DispatchDeliveryDurable, fixture.DispatchDeliveryDurable)
		}
	})
}

// publishedCanonical is the JSON document raw, indented with its members in name order, so two
// documents that hold the same values compare equal whatever order each wrote its members in.
func publishedCanonical(t *testing.T, raw []byte) string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	canonical, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("encode %s: %v", raw, err)
	}
	return string(canonical)
}
