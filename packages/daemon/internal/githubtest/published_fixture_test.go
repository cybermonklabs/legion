package githubtest

//ci:reads packages/contracts/fixtures/checks-settlement/published.json readPublishedFixture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// publishedFixture is packages/contracts/fixtures/checks-settlement/published.json, which Envoy's
// internal/cistore test reads too: there Envoy's own code makes each value, and here the fake's
// hand copies of that code make the same, so a change of either that the other does not take
// fails one of the two tests.
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
			ID         int64  `json:"id"`
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
		t.Fatalf("UNKNOWN: %s holds %d settlements, %d subject segments and the durable %q, so the copies were checked against nothing",
			path, len(fixture.Settlements), len(fixture.SubjectSegments), fixture.DispatchDeliveryDurable)
	}
	return fixture
}

// The fake publishes each settlement of the shared fixture as Envoy's listener does, on the
// fixture's subject, with its dedupe key and its payload; it writes each name as the subject
// segment the fixture names; and it names the durable Dispatch's delivery intake reads, as the
// fixture names it.
func TestTheFakesCopiesOfEnvoyMakeTheSharedFixturesValues(t *testing.T) {
	t.Parallel()
	fixture := readPublishedFixture(t)
	for _, want := range fixture.Settlements {
		t.Run(fmt.Sprintf("the settlement of %s/%s#%d at %.7s, generation %d", want.Owner, want.Repo, want.Number, want.SHA, want.Generation), func(t *testing.T) {
			t.Parallel()
			s := ChecksSettlement{Repo: want.Owner + "/" + want.Repo, Number: want.Number, SHA: want.SHA, Generation: want.Generation,
				Superseded: want.Superseded}
			for _, run := range want.CheckRuns {
				s.Runs = append(s.Runs, CheckRun{ID: run.ID, Name: run.Name, Status: run.Status, Conclusion: run.Conclusion, HTMLURL: run.URL})
			}
			for _, status := range want.Statuses {
				created, err := time.Parse(time.RFC3339, status.CreatedAt)
				if err != nil {
					t.Fatalf("the fixture's status %s was created at %q, which is no RFC 3339 time: %v", status.Context, status.CreatedAt, err)
				}
				s.Statuses = append(s.Statuses, CommitStatus{Context: status.Context, State: status.State, CreatedAt: created})
			}
			published, err := publishedSettlementOf(s)
			if err != nil {
				t.Fatal(err)
			}
			if published.subject != want.Topic {
				t.Errorf("the subject is %q, want the listener's %q", published.subject, want.Topic)
			}
			if published.dedupeKey != want.DedupeKey {
				t.Errorf("the dedupe key is %q, want the listener's %q", published.dedupeKey, want.DedupeKey)
			}
			payload, err := json.Marshal(published.payload)
			if err != nil {
				t.Fatalf("encode the payload: %v", err)
			}
			if got, wanted := publishedCanonical(t, payload), publishedCanonical(t, want.Payload); got != wanted {
				t.Errorf("the payload is\n%s\nwant the listener's\n%s", got, wanted)
			}
		})
	}
	t.Run("the subject segments", func(t *testing.T) {
		t.Parallel()
		for _, want := range fixture.SubjectSegments {
			if got := subjectSegment(want.Value); got != want.Segment {
				t.Errorf("subjectSegment(%q) = %q, want Envoy's %q", want.Value, got, want.Segment)
			}
		}
	})
	t.Run("the delivery durable", func(t *testing.T) {
		t.Parallel()
		if DispatchDeliveryDurable != fixture.DispatchDeliveryDurable {
			t.Errorf("DispatchDeliveryDurable = %q, want Envoy's %q", DispatchDeliveryDurable, fixture.DispatchDeliveryDurable)
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
