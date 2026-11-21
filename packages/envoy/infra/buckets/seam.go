package buckets

import (
	"context"
	"io"
	"time"

	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// Deps are what CommandOn reaches outside the process, in place of the broker and the hub its
// flags name: a test of another package runs `envoy-infra buckets` on its own stack with them
// (writerhold/holdtest). The hold's flags are still required and checked, and their values are
// not read. A break-glass create reads --connz as Command does.
type Deps struct {
	// Records opens the operator's client of the broker's hold routes, signing when signing.
	Records func(ctx context.Context, signing bool) (*holdrecord.Client, error)
	// Operator is the operator's reach of the hub.
	Operator func(ctx context.Context) (*writerhold.Operator, error)
	// Now is the clock the minter report is checked by; time.Now when nil.
	Now func() time.Time
}

// CommandOn is Command with deps in place of the broker and hub its flags name.
func CommandOn(ctx context.Context, args []string, stdout, stderr io.Writer, deps Deps) int {
	e := realEnv()
	e.records = func(ctx context.Context, _ writerhold.HubFlags, signing bool) (*holdrecord.Client, error) {
		return deps.Records(ctx, signing)
	}
	e.hub = func(ctx context.Context, _ writerhold.HubFlags) (*writerhold.Operator, error) {
		return deps.Operator(ctx)
	}
	if deps.Now != nil {
		e.now = deps.Now
	}
	return command(ctx, args, stdout, stderr, e)
}
