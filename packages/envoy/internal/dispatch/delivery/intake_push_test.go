package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// pushFixture is GitHub's push event as the webhook's fixtures hold it (packages/envoy/scripts).
const pushFixture = "../../../scripts/fixtures/github/push.json"

// recordedPush is one push the intake handed to its importer, and whether the context it came
// with held a connection of the pool, under which the importer's read of its sources through the
// pool would be refused (store.ErrNestedAcquire).
type recordedPush struct {
	repo, branch    string
	holdsConnection bool
}

// recordingPushes is a PushImports that records each push it is handed and imports nothing.
type recordingPushes struct {
	mu     sync.Mutex
	pushes []recordedPush
}

func (r *recordingPushes) Pushed(ctx context.Context, repo, branch string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pushes = append(r.pushes, recordedPush{repo: repo, branch: branch, holdsConnection: store.HoldsPooledConnection(ctx)})
	return nil
}

// recorded is the pushes handed over so far, in the order they were.
func (r *recordingPushes) recorded() []recordedPush {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.pushes)
}

// pushEnvelope is the one envelope Envoy's webhook publishes for GitHub's push of ref to
// owner/name: the push fixture with its repository and ref set to these, normalized and checked
// as the webhook normalizes and checks it (contracts.GithubEnvelopes, Envelope.Validate). Each
// call takes a delivery id of its own, since the bus stores a GitHub envelope once per delivery.
func pushEnvelope(t *testing.T, owner, name, ref string) contracts.Envelope {
	t.Helper()
	raw, err := os.ReadFile(pushFixture)
	if err != nil {
		t.Fatalf("read the push fixture: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode the push fixture: %v", err)
	}
	body["ref"] = ref
	body["repository"] = map[string]any{
		"name": name, "full_name": owner + "/" + name, "owner": map[string]any{"login": owner},
	}
	delivery := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
	items := contracts.GithubEnvelopes(contracts.GithubEnvelopeInput{
		Event: "push", Delivery: delivery, Body: body, EventID: "event-" + delivery, TraceID: "trace-" + delivery,
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("a push of %s to %s/%s makes %d envelopes, want 1", ref, owner, name, len(items))
	}
	if err := items[0].Validate(); err != nil {
		t.Fatalf("the envelope of a push of %s to %s/%s: %v", ref, owner, name, err)
	}
	return items[0]
}

// A push of a branch reaches the intake on notifications.github.<owner>.<repo>.push.branch.<branch>,
// and the intake hands it to its importer, once, with the payload's repository and the branch its
// ref names, dots and slashes included, on a context that holds no connection of the pool. A push
// of a tag is handed nowhere. The intake binds its durable and routes a message only while its
// settings cache holds settings (Run, deliver), so delivery settings are saved before the cache's
// one read, though a push's handler reads none of them (handlePushEnvelope). A push is no
// delivery fact, so neither push moves the Delivery page's event freshness (last_event_at). A
// workflow run of a repository other than the settings' deploy repository does move it: route
// answers such a run handled, and routeWorkflowEnvelope fetches nothing for it, so it costs no
// GitHub call. So the freshness row is one the intake writes, and its staying null after the
// pushes shows that they did not write it.
func TestTheIntakeHandsABranchPushToItsImporter(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	natsClient := intakeTestClient(t)
	// The cache reads delivery_settings once, here, and never again, and the intake binds its
	// durable only while the cache holds settings: so the row is saved before that read. It is
	// also the row the event freshness is stamped on, which RecordEventAt updates and never
	// inserts, and PutSettings saves it with last_event_at null.
	seedDeliverySettings(t, ctx, pool)
	settings := loadedSettingsCache(t, ctx, pool)
	if settings.Load() == nil {
		t.Fatal("the intake's settings cache holds no settings in a database where a row is saved")
	}
	fake := newFakeGitHub(t)
	pushes := &recordingPushes{}
	intake := newIntake(natsClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker(), settings)
	intake.pushes = pushes
	startIntake(t, intake)
	awaitBoundDurable(t, natsClient)

	branch := pushEnvelope(t, "cybermonklabs", "legion", "refs/heads/release/1.2")
	if want := "notifications.github.cybermonklabs.legion.push.branch.release/1_2"; branch.Topic != want {
		t.Fatalf("the push of release/1.2 publishes on %s, want %s", branch.Topic, want)
	}
	if err := natsClient.Publish(branch); err != nil {
		t.Fatalf("publish %s: %v", branch.Topic, err)
	}
	awaitAcknowledged(t, natsClient)
	want := []recordedPush{{repo: "cybermonklabs/legion", branch: "release/1.2"}}
	if got := pushes.recorded(); !slices.Equal(got, want) {
		t.Fatalf("the importer was handed %+v for a push of refs/heads/release/1.2, want %+v: once, with the branch its ref names, on a context that holds no connection of the pool",
			got, want)
	}

	tag := pushEnvelope(t, "cybermonklabs", "legion", "refs/tags/v1.2.0")
	if want := "notifications.github.cybermonklabs.legion.push.tag.v1_2_0"; tag.Topic != want {
		t.Fatalf("the push of tag v1.2.0 publishes on %s, want %s", tag.Topic, want)
	}
	if err := natsClient.Publish(tag); err != nil {
		t.Fatalf("publish %s: %v", tag.Topic, err)
	}
	awaitAcknowledged(t, natsClient)
	if got := pushes.recorded(); !slices.Equal(got, want) {
		t.Fatalf("the importer was handed %+v once a push of a tag followed, want %+v: a tag starts no import", got, want)
	}

	stamped, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if stamped.LastEventAt != nil {
		t.Fatalf("last_event_at = %v after a push of a branch and one of a tag, want null: a push is no delivery fact", *stamped.LastEventAt)
	}

	// A workflow run of acme/gadgets, which is not the deploy repository the settings name
	// (acme/widgets): routeWorkflowEnvelope answers that it stored nothing, with no GitHub call,
	// and route answers it handled, so deliver stamps the freshness row once that handler has
	// returned. The stamp is awaited on a deadline rather than read once.
	publishWorkflowEnvelope(t, natsClient, "acme", "gadgets", ".github/workflows/deploy.yml", 1)
	waitFor(t, 10*time.Second, func() bool {
		stamped, err = GetSettings(ctx, pool)
		return err == nil && stamped.LastEventAt != nil
	}, func() string {
		return fmt.Sprintf("last_event_at is still null 10s after a workflow run of acme/gadgets, want its stamp: the check of the pushes above saw no row the intake writes (read: %v)", err)
	})
	if mints := fake.tokenMints.Load(); mints != 0 {
		t.Fatalf("minted %d installation tokens, want none: no envelope of this test is one the intake reads GitHub for", mints)
	}
}
