package natscallout

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// connectDispatch connects to url as Dispatch's service under the connection name name, handing
// every asynchronous error the server sends it to errs.
func connectDispatch(t *testing.T, url, name string, errs chan<- error) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(url,
		nats.UserInfo(natscred.ServiceUser, "dispatch-token"),
		nats.Name(name),
		nats.Timeout(10*time.Second),
		nats.NoReconnect(),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			select {
			case errs <- err:
			default:
			}
		}),
	)
	require.NoError(t, err, "Dispatch's service connection %q was not admitted", name)
	t.Cleanup(nc.Close)
	return nc
}

// awaitPublishViolation waits for the server's permissions violation naming a publish to subject
// among errs, failing the test when it does not come.
func awaitPublishViolation(t *testing.T, errs <-chan error, subject string) {
	t.Helper()
	want := `Permissions Violation for Publish to "` + subject + `"`
	deadline := time.After(10 * time.Second)
	var seen []string
	for {
		select {
		case err := <-errs:
			if strings.Contains(err.Error(), want) {
				return
			}
			seen = append(seen, err.Error())
		case <-deadline:
			t.Fatalf("no %s: the server took the publish (other errors: %q)", want, seen)
		}
	}
}

// The grant rows of a fresh generation, through the hub and the auth service as Dispatch connects:
// a connection named for generation now over generation old creates now's consumer, is delivered
// its message and acknowledges it, and deletes old's; it is refused another generation's create,
// a session's consumer, another machine's listener durable and a Legion daemon's durable, and its
// own listener durable is still its own. The control runs first: the same rows under the fixed
// intake's name are refused the fresh generation's create, so the grant comes from the name.
func TestAnIntakeConnectionCreatesAcknowledgesAndDeletesOnlyItsOwnGenerations(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	adminSeed, adminKey := testnats.User(t)
	admin := testnats.StaticUser{NKey: adminKey, Publish: []string{">"}, Subscribe: []string{">"}}
	url := testnats.StartWithCallout(t, keys, admin)
	now := time.Now()
	startService(t, url, keys, Deps{ServiceToken: serviceTokens(map[string]platformToken{
		"dispatch-token": {account: "system:serviceaccount:dispatch:dispatch", issued: now, expiry: now.Add(time.Hour)},
	})}, Decide)

	adminConn, err := testnats.ConnectSeed(t, url, adminSeed)
	require.NoError(t, err)
	t.Cleanup(adminConn.Close)
	adminJS, err := adminConn.JetStream(nats.MaxWait(10 * time.Second))
	require.NoError(t, err)
	_, err = adminJS.AddStream(&nats.StreamConfig{Name: bus.Stream, Subjects: []string{"notifications.>"}})
	require.NoError(t, err)
	const logical, gen, old, other = "delivery-events", "GenNowA1b2", "GenOldA1b2", "GenElseA1b2"
	const filter = "notifications.github.example"
	pushed := func(name string) *nats.ConsumerConfig {
		return &nats.ConsumerConfig{Durable: name, DeliverSubject: nats.NewInbox(), FilterSubject: filter, AckPolicy: nats.AckExplicitPolicy}
	}
	for _, name := range []string{logical + "~" + old, logical + "~" + other, "s-x", "listener-m2", "legion-go-p-github"} {
		_, err := adminJS.AddConsumer(bus.Stream, pushed(name))
		require.NoError(t, err, "the admin creates %s", name)
	}

	// apiCall sends one JetStream API request on nc, as nats.go would send it, and answers its error:
	// a refused publish draws no answer, so the request ends at its timeout, and the server's
	// violation, naming the subject, reaches the connection's error handler.
	apiCall := func(nc *nats.Conn, call, name string) (string, func() error) {
		subject := "$JS.API.CONSUMER." + call + "." + bus.Stream + "." + name
		var body []byte
		if call == "CREATE" {
			subject += "." + filter
			body = []byte(`{"stream_name":"` + bus.Stream + `","config":{"durable_name":"` + name + `","deliver_subject":"` + nats.NewInbox() +
				`","filter_subject":"` + filter + `","ack_policy":"explicit"},"action":""}`)
		}
		return subject, func() error {
			_, err := nc.Request(subject, body, 2*time.Second)
			return err
		}
	}

	t.Run("control: the fixed intake's name is refused the fresh generation", func(t *testing.T) {
		errs := make(chan error, 16)
		subject, call := apiCall(connectDispatch(t, url, "dispatch-intake", errs), "CREATE", logical+"~"+gen)
		require.Error(t, call(), "%s was served", subject)
		awaitPublishViolation(t, errs, subject)
	})

	errs := make(chan error, 16)
	nc := connectDispatch(t, url, natsgrant.DispatchIntakeConnection(logical, gen, old), errs)
	js, err := nc.JetStream(nats.MaxWait(2 * time.Second))
	require.NoError(t, err)

	own := pushed(logical + "~" + gen)
	delivered, err := nc.SubscribeSync(own.DeliverSubject)
	require.NoError(t, err)
	_, err = js.AddConsumer(bus.Stream, own)
	require.NoError(t, err, "the intake creates its own generation")
	_, err = adminJS.Publish(filter, []byte("event"))
	require.NoError(t, err)
	msg, err := delivered.NextMsg(10 * time.Second)
	require.NoError(t, err, "its own generation delivers to it")
	require.NoError(t, msg.AckSync(), "it acknowledges its own generation")
	info, err := adminJS.ConsumerInfo(bus.Stream, own.Durable)
	require.NoError(t, err)
	require.Equal(t, uint64(1), info.AckFloor.Stream, "its acknowledgement reached its generation")
	require.NoError(t, js.DeleteConsumer(bus.Stream, logical+"~"+old), "it deletes the generation it replaces")
	_, err = js.ConsumerInfo(bus.Stream, bus.ListenerDurable("example-dispatch"))
	require.True(t, errors.Is(err, nats.ErrConsumerNotFound), "its own listener durable answers (not found, here): %v", err)

	for _, row := range []struct{ call, name string }{
		{"CREATE", logical + "~" + other},
		{"INFO", logical + "~" + other},
		{"DELETE", logical + "~" + other},
		{"CREATE", logical + "~" + old},
		{"DELETE", logical + "~" + gen},
		{"CREATE", "s-x"},
		{"INFO", "s-x"},
		{"DELETE", "s-x"},
		{"CREATE", "listener-m2"},
		{"INFO", "listener-m2"},
		{"CREATE", "legion-go-p-github"},
		{"INFO", "legion-go-p-github"},
	} {
		subject, call := apiCall(nc, row.call, row.name)
		require.Error(t, call(), "%s was served", subject)
		awaitPublishViolation(t, errs, subject)
	}
	for _, name := range []string{logical + "~" + other, "s-x", "listener-m2", "legion-go-p-github", logical + "~" + gen} {
		_, err := adminJS.ConsumerInfo(bus.Stream, name)
		require.NoError(t, err, "%s is still there after the refused calls", name)
	}
}
