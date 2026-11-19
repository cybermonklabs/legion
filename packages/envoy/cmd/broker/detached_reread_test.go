package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// thePeersTold is the reread's notice to the other pods, as TestStopWaitsForARereadRunAfterItsAnswer
// records it beside the stop's own steps.
const thePeersTold = "the other pods were told to reread"

// heldDescribe is a store whose every DescribeSecret says on entered that a reread reached it and
// then waits for release before it answers as the store below it does.
type heldDescribe struct {
	secrets.SecretsManagerAPI
	entered chan<- struct{}
	release <-chan struct{}
}

func (h heldDescribe) DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	h.entered <- struct{}{}
	select {
	case <-h.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return h.SecretsManagerAPI.DescribeSecret(ctx, in, opts...)
}

// TestStopWaitsForARereadRunAfterItsAnswer: a reread the API answered 202 for runs after its
// answer, which srv.Shutdown does not wait for. With the store holding that reread, the stop
// sequence shuts the server down and then waits: it does not end the serving context or close the
// store while the reread is still held. Once the store answers, the reread tells the other pods,
// and only then does the serving context end and the store close. Before, Shutdown returned with
// the reread in flight, the store closed under it, and the pods were never told.
//
// The named revert: drop the WaitDetached call from stop.
func TestStopWaitsForARereadRunAfterItsAnswer(t *testing.T) {
	local := secrets.NewLocal(policytest.Secret("WORKER_TOKEN", policy.OwnerShared, policy.TierAgent, "v1"))
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	loader := policytest.Loader(local)
	loader.Store = policytest.StoreThrough(local, heldDescribe{SecretsManagerAPI: local, entered: entered, release: release})
	current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var ran steps
	detached := &api.Detached{}
	mux := http.NewServeMux()
	api.Register(mux, api.Deps{Policy: current, SecretsStore: secrets.StoreAWS, SecretsPrefix: policytest.Prefix, SecretsKMSKey: policytest.KeyARN,
		Detached: detached, NotifyReread: func(context.Context, string) { ran.record(thePeersTold) }})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/secrets/WORKER_TOKEN/reread", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a reread with no proof = %d; want 202", resp.StatusCode)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the reread run after the 202 never reached the store")
	}

	shutdownReturned, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		stop(t.Context(), brokerStop{
			DrainNow: make(chan struct{}),
			Draining: func(bool) {},
			Shutdown: func(ctx context.Context) error {
				err := srv.Config.Shutdown(ctx)
				ran.record(theShutdownReturned)
				close(shutdownReturned)
				return err
			},
			WaitDetached: detached.Wait,
			EndServing:   func() { ran.record(theServingEnded) },
			CloseStore:   func() { ran.record(theStoreClosed) },
			Log:          orderingLog(),
		})
	}()
	<-shutdownReturned
	// One-sided: a stop that does not wait for the held reread has nothing left to do once its
	// shutdown returns, and returns at once; a stop that waits cannot return here however long
	// this is, since the store answers only once it is released below.
	select {
	case <-stopped:
		t.Fatalf("the stop ran %s and returned with the reread still held in the store; want it to wait for that reread and its notice to the other pods", &ran)
	case <-time.After(2 * time.Second):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatalf("the stop ran %s and had not returned 10s after the store answered", &ran)
	}
	want := []string{theShutdownReturned, thePeersTold, theServingEnded, theStoreClosed}
	if !ran.is(want...) {
		t.Fatalf("the stop ran %s; want %s: the reread run after its answer finishes, and tells the other pods, before the serving context ends and the store closes", &ran, strings.Join(want, " -> "))
	}
}
