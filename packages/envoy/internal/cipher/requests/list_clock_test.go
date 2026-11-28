// packages/envoy/internal/cipher/requests/list_clock_test.go
// The deep review's round 2 on cybermonklabs/legion#4779 (R2-1): one project Dispatch does not
// answer costs a list the rows that name it, and no other, inside the time Dispatch gives a list.
package requests

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/logtest"
)

// stalledProject is the project the stand-in below does not answer for.
const stalledProject = "SLOW"

// stallFor is a projectaccess.Access that holds every access question about project for hold, or
// until its caller's context ends, and then answers the deadline error the client answers when
// Dispatch says nothing within its own bound; every other question goes to inner. stalled counts
// the questions it held, and arrived, where set, is sent to (without blocking, so give it room)
// once each is counted, so a test can wait for a question to reach Dispatch rather than for the
// rows that asked it. A question on a context that has already ended is answered its error at once
// and not counted, as the client answers one without asking Dispatch.
type stallFor struct {
	inner   projectaccess.Access
	project string
	hold    time.Duration
	stalled atomic.Int32
	arrived chan struct{}
}

func (s *stallFor) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	if project != s.project {
		return s.inner.Access(ctx, project, person)
	}
	if err := ctx.Err(); err != nil {
		return projectaccess.Answer{}, err
	}
	s.stalled.Add(1)
	select {
	case s.arrived <- struct{}{}:
	default:
	}
	select {
	case <-time.After(s.hold):
		return projectaccess.Answer{}, fmt.Errorf("dispatchread: ask dispatch: %w", context.DeadlineExceeded)
	case <-ctx.Done():
		return projectaccess.Answer{}, ctx.Err()
	}
}

func (s *stallFor) MemberKind(ctx context.Context, person string) (string, error) {
	return s.inner.MemberKind(ctx, person)
}

func (s *stallFor) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	return s.inner.States(ctx, projects)
}

// TestAStalledProjectLeavesOffOnlyTheRowsNamingIt: three rows on an outsider's list, one whose
// reason names TEAM-1 (public), one whose reason names no issue, and, newest and so read first,
// one whose reason names SLOW-1. Dispatch holds the question about SLOW for 6 s, past the 5 s
// Dispatch waits for a list, and the client holds no answer yet. The list answers within its clock
// for its reader's own rows (listOwnRowsBound, 3 s), well inside those 5 s: the two other rows
// are listed, the TEAM row's question asked beside the stalled one, and the SLOW row is left off
// and logged. The pending list and the grant list each do so.
func TestAStalledProjectLeavesOffOnlyTheRowsNamingIt(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (logtest.SetDefault, logtest.Swap, slog.SetDefault); it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past (`took >= dispatchListWindow-500*time.Millisecond`).
	reasons := []string{"TEAM-1: rotate the deploy key", "rotate the wiki key", stalledProject + "-1: rotate the slow key"}
	for _, c := range []struct {
		name string
		// rows makes one row of the list for each of reasons, in order, and answers each one's id.
		rows func(t *testing.T, m *Machine) []string
		// listed is the ids the outsider's list answers.
		listed func(t *testing.T, m *Machine) []string
		// logged is the id a rowUnansweredMessage line names under key.
		key string
	}{
		{
			name: "the pending list",
			rows: func(t *testing.T, m *Machine) []string {
				var ids []string
				for i, reason := range reasons {
					pod, key := everyonesPod(t, m, fmt.Sprintf("pod-stall-%d", i), "")
					req, err := m.Create(t.Context(), pod, signRequest(t, m, key, reason, "SHARED_KEY"), "")
					if err != nil || req.RecordID == nil {
						t.Fatalf("Create(SHARED_KEY, %q) = %+v, %v; want it pending", reason, req, err)
					}
					ids = append(ids, *req.RecordID)
				}
				return ids
			},
			listed: func(t *testing.T, m *Machine) []string { return pendingRecords(t, m, outsider) },
			key:    "record",
		},
		{
			name: "the grant list",
			rows: func(t *testing.T, m *Machine) []string {
				var ids []string
				for i, reason := range reasons {
					pod, key := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, fmt.Sprintf("pod-stall-grant-%d", i))
					req, err := m.Create(t.Context(), pod, signRequest(t, m, key, reason, "SERVICE_KEY"), "")
					if err != nil || req.GrantID == nil {
						t.Fatalf("Create(SERVICE_KEY, %q) = %+v, %v; want it granted at once", reason, req, err)
					}
					ids = append(ids, *req.GrantID)
				}
				return ids
			},
			listed: func(t *testing.T, m *Machine) []string { return approverGrants(t, m, outsider) },
			key:    "grant",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
			ids := c.rows(t, m)
			team, plain, slow := ids[0], ids[1], ids[2]
			answerFrom(t, m, dispatchRows(publicTeam))
			stall := &stallFor{inner: m.Projects, project: stalledProject, hold: 6 * time.Second}
			m.Projects = stall
			capture := &logtest.Recorder{}
			logtest.SetDefault(t, capture)

			start := time.Now()
			listed := c.listed(t, m)
			took := time.Since(start)
			t.Logf("the list took %s and answered %v", took.Round(time.Millisecond), listed)

			if stall.stalled.Load() == 0 {
				t.Fatal("the list asked Dispatch nothing about SLOW, so nothing stalled: the test is blind")
			}
			if took >= dispatchListWindow-500*time.Millisecond {
				t.Errorf("the list took %s; want it inside the %s Dispatch gives it, by its %s clock", took, dispatchListWindow, listOwnRowsBound)
			}
			for what, id := range map[string]string{"names TEAM-1": team, "names no issue": plain} {
				if !slices.Contains(listed, id) {
					t.Errorf("the list leaves off the row that %s, %s; want it listed, since Dispatch answers for it", what, id)
				}
			}
			if slices.Contains(listed, slow) {
				t.Errorf("the list holds the row naming SLOW-1, %s; want it left off, since Dispatch never answered for it", slow)
			}
			var logged bool
			for _, line := range linesOf(capture, rowUnansweredMessage) {
				logged = logged || line[c.key] == slow && line["level"] == "WARN"
			}
			if !logged {
				t.Errorf("the log holds %v; want a WARN line of %q naming %s %s", linesOf(capture, rowUnansweredMessage), rowUnansweredMessage, c.key, slow)
			}
		})
	}
}
