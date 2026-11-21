package writerhold

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/sjawhar/envoy/internal/holdrecord"
)

// ErrNoKeyStore is a `hold keygen` run through CommandOn whose HoldDeps name no key store.
var ErrNoKeyStore = errors.New("no key store: HoldDeps.Keys is nil")

// HoldDeps are what CommandOn reaches outside the process, in place of the broker, Secret Manager
// and the hub its flags name: a test of another package runs `envoy-infra hold` on its own stack
// with them (writerhold/holdtest). The hold's flags are still required and checked, and their
// values are not read.
type HoldDeps struct {
	// Records opens the operator's client of the broker's hold routes, signing when signing.
	Records func(ctx context.Context, signing bool) (*holdrecord.Client, error)
	// Keys opens the signing key's store, which `hold keygen` writes; with none, keygen fails
	// with ErrNoKeyStore.
	Keys func(ctx context.Context) (KeyStore, error)
	// Operator is the operator's reach of the hub.
	Operator func(ctx context.Context) (*Operator, error)
	// Now is the clock the minter report is checked by; time.Now when nil.
	Now func() time.Time
}

// CommandOn is Command with deps in place of the broker, Secret Manager and hub its flags name.
func CommandOn(ctx context.Context, args []string, stdout, stderr io.Writer, deps HoldDeps) int {
	return run(ctx, args, stdout, stderr, deps.env())
}

// env is the run's env over d.
func (d HoldDeps) env() env {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	keys := d.Keys
	if keys == nil {
		keys = func(context.Context) (KeyStore, error) { return nil, ErrNoKeyStore }
	}
	return env{
		records: func(ctx context.Context, _ options, signing bool) (*holdrecord.Client, error) {
			return d.Records(ctx, signing)
		},
		keys: keys,
		hub:  func(ctx context.Context, _ options) (*Operator, error) { return d.Operator(ctx) },
		now:  now,
	}
}
