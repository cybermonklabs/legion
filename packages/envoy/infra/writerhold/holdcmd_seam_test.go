package writerhold

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// CommandOn runs `envoy-infra hold` on a stack its caller names: `hold prepare` through it, with
// the stack's broker client and Operator, prepares a hold, and `hold abort` ends it, as Command
// does through the flags. The controls: the same prepare with no Operator to reach stops before
// any hold, and `hold keygen` with no key store fails with ErrNoKeyStore.
func TestCommandOnRunsTheHoldOnTheCallersStack(t *testing.T) {
	h := newHeldStack(t)
	deps := HoldDeps{
		Records:  func(context.Context, bool) (*holdrecord.Client, error) { return h.s.Broker.Client(t), nil },
		Operator: func(context.Context) (*Operator, error) { return h.op, nil },
	}
	runOn := func(deps HoldDeps, args ...string) (int, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := CommandOn(context.Background(), append(args, commonArgs...), &stdout, &stderr, deps)
		t.Logf("envoy-infra hold %s: exit %d\n%s%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
		return code, stdout.String(), stderr.String()
	}
	prepare := []string{actionPrepare, "--workflow", string(holdrecord.WorkflowBucketsCreate), "--reason", "the seam", "--minters-report", h.report}

	code, stdout, _ := runOn(deps, prepare...)
	require.Zero(t, code, "prepare through CommandOn")
	id := strings.TrimSpace(stdout)
	require.Equal(t, natsgrant.OperatorPhasePrepare, h.row().Phase)
	require.Equal(t, id, h.row().HoldID)
	code, _, _ = runOn(deps, actionAbort, "--hold", id, "--reason", "the seam's hold")
	require.Zero(t, code, "abort through CommandOn")
	require.Equal(t, natsgrant.OperatorPhaseReleased, h.row().Phase)

	noHub := deps
	noHub.Operator = func(context.Context) (*Operator, error) { return nil, errors.New("no hub here") }
	code, _, stderr := runOn(noHub, prepare...)
	require.Equal(t, 1, code, "control: prepare with no hub")
	require.Contains(t, stderr, "no hub here")
	require.Equal(t, natsgrant.OperatorPhaseReleased, h.row().Phase, "control: a hold was prepared with no hub")

	code, _, stderr = runOn(deps, actionKeygen)
	require.Equal(t, 1, code, "control: keygen with no key store")
	require.Contains(t, stderr, ErrNoKeyStore.Error())
}
