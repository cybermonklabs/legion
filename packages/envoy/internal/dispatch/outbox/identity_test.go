package outbox

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// An outbox holds the publisher hold under the identity of its store's pool, the one every
// database session of its process names at the end of its application_name, so the hold's row
// and the sessions a standby that takes it over ends name one process. One cannot be built on a
// store whose pool names no identity: it would hold the hold under an identity none of its
// sessions carries, and a standby that took the hold over from it would end none of them.
func TestAnOutboxHoldsThePublisherUnderItsStoresIdentityAndNoOther(t *testing.T) {
	database := storetest.Open(t)
	built := New(Deps{Store: database, Publisher: &recordingPublisher{}})
	if built.instance != database.Pool.Identity() {
		t.Errorf("the outbox holds the publisher hold under %s, want its store's identity %s", built.instance, database.Pool.Identity())
	}

	config := database.Pool.Config().Copy()
	config.ConnConfig.RuntimeParams["application_name"] = "dispatch-no-identity"
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a pool whose connections name no identity: %v", err)
	}
	t.Cleanup(opened.Close)
	nameless := &store.Store{Pool: store.NewPool(opened)}
	defer func() {
		refusal := recover()
		if refusal == nil {
			t.Fatal("an outbox was built on a store whose pool names no identity, want New to refuse it")
		}
		if text, _ := refusal.(string); !strings.Contains(text, "names no process identity") {
			t.Fatalf("New refused the nameless store with %v, want it to name the missing identity", refusal)
		}
	}()
	outbox := New(Deps{Store: nameless, Publisher: &recordingPublisher{}})
	var holder string
	_ = opened.QueryRow(context.Background(), `select current_setting('application_name')`).Scan(&holder)
	t.Logf("the outbox built on the nameless store holds under %s, while its sessions are named %q", outbox.instance, holder)
}
