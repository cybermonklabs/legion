package requests

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/logtest"
)

// goneOnFirstAsk is a Dispatch whose caller goes away as it is asked its first question: the
// first question, of whatever kind, ends the list's context (cancel), and every question,
// that one included, waits for the context it was asked on to end and answers its error, as a
// Dispatch read cut short by its caller does. No clock decides when the caller leaves, so a slow
// database read before the first question changes nothing. asked counts the questions.
type goneOnFirstAsk struct {
	cancel context.CancelFunc
	once   sync.Once
	asked  atomic.Int32
}

func (g *goneOnFirstAsk) wait(ctx context.Context) error {
	g.asked.Add(1)
	g.once.Do(g.cancel)
	<-ctx.Done()
	return ctx.Err()
}

func (g *goneOnFirstAsk) Access(ctx context.Context, _, _ string) (projectaccess.Answer, error) {
	return projectaccess.Answer{}, g.wait(ctx)
}

func (g *goneOnFirstAsk) MemberKind(ctx context.Context, _ string) (string, error) {
	return "", g.wait(ctx)
}

func (g *goneOnFirstAsk) States(ctx context.Context, _ []string) (map[string]projectaccess.Answer, error) {
	return nil, g.wait(ctx)
}

// TestAListWhoseCallerWentAwayLogsNothingOfTheRowsItLeftOff: the caller of a pending or a grant
// list goes away while the list still asks Dispatch about its rows: an Inbox tab, an approval
// page or the Live grants page closed. Nobody reads that list, Dispatch did not fail to answer and
// no clock ran out, so none of the lines a list writes for a row it leaves off is written: not
// that Dispatch could not be asked about the reader's own row (rowUnansweredMessage), and not that
// the fallback's clock cut other people's rows (fallbackCutMessage) or that a fallback read failed
// (fallbackUnreadMessage). Each row of the table is a different read of the list: the reader's
// own pending rows, rows whose responsible person is someone else (the fallback), and the grant
// list.
func TestAListWhoseCallerWentAwayLogsNothingOfTheRowsItLeftOff(t *testing.T) {
	const rows = 5
	for _, list := range []struct {
		name string
		make func(t *testing.T, m *Machine, i int)
		read func(ctx context.Context, m *Machine) error
	}{
		{
			name: "the reader's own pending rows",
			make: func(t *testing.T, m *Machine, i int) {
				pod, key := everyonesPod(t, m, fmt.Sprintf("gone-own-%02d", i), "")
				if req, err := m.Create(t.Context(), pod, signRequest(t, m, key, "TEAM-7: rotate the deploy key", "SHARED_KEY"), ""); err != nil || req.State != "pending" {
					t.Fatalf("Create = %+v, %v; want it pending", req, err)
				}
			},
			read: func(ctx context.Context, m *Machine) error {
				_, err := m.PendingForApprover(ctx, insider, PendingStates{})
				return err
			},
		},
		{
			name: "pending rows whose responsible person is someone else",
			make: func(t *testing.T, m *Machine, i int) {
				operator := fmt.Sprintf("gone%02d@example.com", i)
				enr, key := newEnrollment(t, m.Store, "session", fmt.Sprintf("box-gone-%02d", i), new(operator), nil)
				if req, err := m.Create(t.Context(), enr, signRequest(t, m, key, "TEAM-7: rotate the deploy key", "SHARED_KEY"), ""); err != nil || req.State != "pending" {
					t.Fatalf("Create = %+v, %v; want it pending", req, err)
				}
			},
			read: func(ctx context.Context, m *Machine) error {
				_, err := m.PendingForApprover(ctx, insider, PendingStates{})
				return err
			},
		},
		{
			name: "the grant list",
			make: func(t *testing.T, m *Machine, i int) {
				pod, key := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, fmt.Sprintf("pod-gone-own-%02d-%s", i, t.Name()))
				grantOf(t, m, pod, key, "TEAM-7: deploy the broker", "SERVICE_KEY")
			},
			read: func(ctx context.Context, m *Machine) error { _, err := m.GrantsForApprover(ctx, insider); return err },
		},
	} {
		t.Run(list.name, func(t *testing.T) {
			m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
			for i := range rows {
				list.make(t, m, i)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			gone := &goneOnFirstAsk{cancel: cancel}
			m.Projects = gone
			capture := &logtest.Recorder{}
			logtest.SetDefault(t, capture)
			_ = list.read(ctx, m)
			if gone.asked.Load() == 0 {
				t.Fatal("the list asked Dispatch nothing, so its caller never left while it read and the row proves nothing")
			}
			wantNoLine(t, capture, rowUnansweredMessage, fallbackCutMessage, fallbackUnreadMessage)
		})
	}
}
