package envoy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// recordedRegistrations is a RegistrationRecorder that keeps each call's answers, and answers fail
// to every call while fail is set.
type recordedRegistrations struct {
	mu    sync.Mutex
	calls [][]model.SessionProjects
	fail  error
}

func (r *recordedRegistrations) Record(_ context.Context, answers []model.SessionProjects) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, slices.Clone(answers))
	return r.fail
}

// pairs is every (session, project) the calls named, as "<session> <project>", in call order.
func (r *recordedRegistrations) pairs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var pairs []string
	for _, call := range r.calls {
		for _, answer := range call {
			for _, project := range answer.Projects {
				pairs = append(pairs, answer.SessionID+" "+project)
			}
		}
	}
	return pairs
}

// registrationsListener answers GET /v1/registry/<id> with projects for every id, and GET
// /v1/sessions with count sessions, s-0 to s-<count-1>, each listing PUB but s-0, which lists none.
func registrationsListener(t *testing.T, projects string, count int) string {
	t.Helper()
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if id, ok := strings.CutPrefix(r.URL.Path, "/v1/registry/"); ok {
			_, _ = fmt.Fprintf(w, `{"session_id":%q,"projects":%s,"updated_at":1}`, id, projects)
			return
		}
		rows := make([]string, count)
		for index := range rows {
			listed := `["PUB"]`
			if index == 0 {
				listed = `[]`
			}
			rows[index] = fmt.Sprintf(`{"session_id":"s-%d","projects":%s,"updated_at":1}`, index, listed)
		}
		_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
	}))
	t.Cleanup(listener.Close)
	return listener.URL
}

// TestTheClientRecordsTheProjectsTheListenerAnswers (WithRegistrations): Session of S, answered
// with ["PRIV"], hands the recorder (S, PRIV), and so does SessionIncludingStopped; Sessions of 50
// sessions makes one call naming the 49 that list a project; a session answered with [] records
// nothing; and a recorder's error is the read's error.
//
// Control, in the test: the same reads through a client with no WithRegistrations record nothing,
// and the check fails naming S.
func TestTheClientRecordsTheProjectsTheListenerAnswers(t *testing.T) {
	ctx := context.Background()
	recorder := &recordedRegistrations{}
	client := New(registrationsListener(t, `["PRIV"]`, 50), WithRegistrations(recorder))
	read := func(c *Client) error {
		if _, err := c.Session(ctx, "S"); err != nil {
			return err
		}
		_, err := c.SessionIncludingStopped(ctx, "S")
		return err
	}
	missing := func(pairs []string) string {
		if want := []string{"S PRIV", "S PRIV"}; !slices.Equal(pairs, want) {
			return fmt.Sprintf("the reads of S recorded %v, want %v", pairs, want)
		}
		return ""
	}
	if err := read(client); err != nil {
		t.Fatalf("read S: %v", err)
	}
	if wrong := missing(recorder.pairs()); wrong != "" {
		t.Fatal(wrong)
	}

	recorder.calls = nil
	sessions, err := client.Sessions(ctx)
	if err != nil || len(sessions) != 50 {
		t.Fatalf("Sessions answered %d sessions (%v), want 50", len(sessions), err)
	}
	if len(recorder.calls) != 1 || len(recorder.calls[0]) != 49 {
		t.Fatalf("Sessions of 50 made %d record calls, the first naming %d sessions, want one call naming the 49 that list a project",
			len(recorder.calls), len(recorder.calls[0]))
	}
	if slices.ContainsFunc(recorder.calls[0], func(answer model.SessionProjects) bool { return answer.SessionID == "s-0" }) {
		t.Fatal("Sessions recorded s-0, whose list is empty, want nothing of it")
	}

	recorder.calls = nil
	empty := New(registrationsListener(t, `[]`, 0), WithRegistrations(recorder))
	if _, err := empty.Session(ctx, "S"); err != nil || len(recorder.calls) != 0 {
		t.Fatalf("a session answered with [] made %d record calls (%v), want none", len(recorder.calls), err)
	}

	recorder.fail = errors.New("the database refused the write")
	if _, err := client.Session(ctx, "S"); !errors.Is(err, recorder.fail) {
		t.Fatalf("a read whose record failed answered %v, want the recorder's error", err)
	}
	if _, err := client.Sessions(ctx); !errors.Is(err, recorder.fail) {
		t.Fatalf("a list whose record failed answered %v, want the recorder's error", err)
	}

	unrecorded := &recordedRegistrations{}
	plain := New(registrationsListener(t, `["PRIV"]`, 50))
	if err := read(plain); err != nil {
		t.Fatalf("control: read S through a client with no recorder: %v", err)
	}
	if missing(unrecorded.pairs()) == "" {
		t.Fatal("control: a client with no recorder recorded S's projects, want the check to fail naming S")
	}
}
