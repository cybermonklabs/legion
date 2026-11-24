package gcp

import (
	"context"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// RereadBudget is the whole fleet's budget of POST /v1/secrets/{name}/reread on Google Secret
// Manager, which unlike the default store does bound that route by a provider's quota: Secret
// Manager serves 600 read requests a minute per project (cloud.google.com/secret-manager/quotas,
// "Read request"), the whole project shares them, and one reread spends two of them (the secret
// and its version). The budget is a fifth of that quota - 120 metadata reads a minute, one reread a
// second - which is what one pod alone was allowed; the other four fifths are what every pod's
// five-minute namespace reload, its miss-path rereads and its releases spend. Those are multiplied
// by running more pods, not by the fan-out: each pod spends its own, and none of them notifies -
// only the reread route does, while a miss-path reread, a reload and a release send nothing - so
// they too grow with the replica count, and the route's share is not the place to find more room.
//
// internal/cipher/api divides it among the pods (RereadOverallFor) by the replica count squared,
// for the reason its default bucket gives - each accepted reread costs a store read on every pod,
// so the fleet spends replicas² × one pod's rate - which for two pods is 2 pods × 0.25 reread/s ×
// 2 store reads × 2 metadata reads = 2 metadata reads a second, the same 120 a minute. Three pods
// divide the budget by nine rather than by four, one reread every 9 s each, and four by sixteen,
// one every 16 s.
//
// Its burst is the default budget's ten, undivided by the replica count for the reason given
// there: the depth a burst admits is one person's write burst, which the replica count does not
// change, and what the depth guards is a pod's own writer lock rather than the project's quota.
// The whole fleet's transient at that depth is 2 pods × 10 rereads × 2 store reads × 2 metadata
// reads = 80 metadata reads, eight seconds of the shared 600 a minute, after which the rate above
// holds the traffic down again.
var RereadBudget = ratelimit.Limit{Every: time.Second, Burst: 10}

// Backend is the GCP store's registry entry.
type Backend struct{}

func (Backend) Name() string { return Name }
func (Backend) ID() string   { return "gcp" }

// ServiceSDKs is Secret Manager's client, which agent-secrets and cipherd import for this store
// alone.
func (Backend) ServiceSDKs() []string {
	return []string{"cloud.google.com/go/secretmanager"}
}

func (Backend) Development() bool             { return false }
func (Backend) RereadBudget() ratelimit.Limit { return RereadBudget }
func (Backend) Variables() []string           { return []string{keyVariable} }

// OpenStore opens Secret Manager under Application Default Credentials (NewClient) and reads the
// agent secrets on the key cfg names.
func (Backend) OpenStore(ctx context.Context, cfg secretstore.Config, _ secretstore.OpenEnv) (secretstore.Opened, error) {
	client, err := NewClient(ctx)
	if err != nil {
		return secretstore.Opened{}, err
	}
	return secretstore.Opened{Store: New(client, cfg.(config).keyName), Credentials: "application-default", Close: client.Close}, nil
}
