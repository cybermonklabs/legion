package delivery

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testnats/servicecallout"
	"github.com/sjawhar/envoy/natscred"
)

// A Dispatch connected by its service token takes over a delivery durable a process before it
// made on an _INBOX. subject, and delivers from it: the auth service grants a service no inbox but
// its own reply prefix (natscred.InboxPrefix), so the take-over creates the durable again on an
// inbox of its own connection. The control is the take-over drawing nats.go's package inbox, as
// before, whose subscription the grant refuses: the durable is never push-bound and nothing is
// delivered.
func TestATakeOverOnAServiceTokenDeliversUnderItsOwnPrefix(t *testing.T) {
	const account = "system:serviceaccount:dispatch:dispatch"
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	adminSeed, adminKey := testnats.User(t)
	url := servicecallout.Start(t, "dispatch/example-dispatch/*="+account,
		testnats.StaticUser{NKey: adminKey, Publish: []string{">"}, Subscribe: []string{">"}})
	owner, err := bus.ConnectOwningStream([]string{url}, bus.WithEnvironment(func(name string) (string, bool) {
		if name == bus.NkeySeedEnvVar {
			return adminSeed, true
		}
		return "", false
	}))
	if err != nil {
		t.Fatalf("the admin creates the stream: %v", err)
	}
	t.Cleanup(owner.Close)
	old := deliveryConsumerConfig(owner.Connection().NewInbox())
	if _, err := owner.JS().AddConsumer(bus.Stream, &old); err != nil {
		t.Fatalf("the old process's unbound durable: %v", err)
	}
	if !strings.HasPrefix(old.DeliverSubject, "_INBOX.") {
		t.Fatalf("the old durable delivers on %s, want an _INBOX. subject", old.DeliverSubject)
	}

	t.Setenv(bus.ServiceTokenFileEnvVar, servicecallout.TokenFile(t, account, 50*time.Minute))
	process := startIntakeProcess(t, pool, url, nil)
	info := awaitPushBound(t, owner, "Dispatch on its service token never took the durable over")
	if !strings.HasPrefix(info.Config.DeliverSubject, natscred.InboxPrefix(account)+".") {
		t.Fatalf("the taken-over durable delivers on %s, want under Dispatch's own prefix %s", info.Config.DeliverSubject, natscred.InboxPrefix(account))
	}
	publishMergedPullRequest(t, owner, 7)
	waitFor(t, 30*time.Second, func() bool {
		process.mu.Lock()
		defer process.mu.Unlock()
		return process.fetched[7] > 0
	}, func() string { return "the merged pull request was never delivered: " + consumerState(owner) })
}
