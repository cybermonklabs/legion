package buckets

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// CommandOn runs `envoy-infra buckets` on a stack its caller names: in a set buckets-create hold
// whose KV_envoy_roles is gone, `buckets create --hold` through it, with the stack's broker client
// and Operator, makes the bucket again and writes its done entry, as Command does through the
// flags. The control is the same create with no Operator to reach, which stops before its intent.
func TestCommandOnRunsABucketsCommandOnTheCallersStack(t *testing.T) {
	h := newHeldBuckets(t)
	ctx := context.Background()
	id := h.s.Prepare(t, holdrecord.WorkflowBucketsCreate)
	h.s.HoldWithoutTheSteps(t, id)
	nc, err := h.s.ConnectOperator(ctx)
	if err != nil {
		t.Fatal(err)
	}
	js, _ := jetstream.New(nc)
	if err := js.DeleteStream(ctx, "KV_"+bus.RoleClaimBucket); err != nil {
		t.Fatal(err)
	}
	nc.Close()
	deps := Deps{
		Records:  func(context.Context, bool) (*holdrecord.Client, error) { return h.s.Broker.Client(t), nil },
		Operator: func(context.Context) (*writerhold.Operator, error) { return h.op, nil },
	}
	args := []string{"create", "--hold", id, "--bucket", bus.RoleClaimBucket, "--reason", "the seam",
		"--broker-url", "https://broker.holdtest", "--token-file", "/dev/null", "--verify-keys", "AAAA",
		"--signing-key-secret", "projects/holdtest/secrets/" + writerhold.SigningKeySecret, "--nats", h.s.Cluster.URL()}
	runOn := func(deps Deps) (int, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := CommandOn(ctx, args, &stdout, &stderr, deps)
		t.Logf("envoy-infra buckets create through CommandOn: exit %d\n%s%s", code, stdout.String(), stderr.String())
		return code, stderr.String()
	}
	ops := func() []holdrecord.Operation {
		t.Helper()
		d, _, err := h.s.Records.Journal(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return d.Ops
	}
	set := len(ops())

	noHub := deps
	noHub.Operator = func(context.Context) (*writerhold.Operator, error) { return nil, errors.New("no hub here") }
	if code, stderr := runOn(noHub); code != 1 || !strings.Contains(stderr, "no hub here") {
		t.Fatalf("control: a create with no hub = %d, want 1 naming it\n%s", code, stderr)
	}
	if got := ops(); len(got) != set {
		t.Fatalf("control: the create with no hub wrote %+v after the hold's set", got[set:])
	}

	if code, stderr := runOn(deps); code != 0 {
		t.Fatalf("a create through CommandOn = %d, want 0\n%s", code, stderr)
	}
	got := ops()
	if last := got[len(got)-1]; last.Command != CommandCreate || last.State != "done" {
		t.Fatalf("the create's last entry is %+v, want its done entry", last)
	}
	h.s.Release(t, id)
}
