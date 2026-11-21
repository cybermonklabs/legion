package natscallout

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// A held daemon hears every repository's GitHub events: a GitHub event names no project, so the
// grant names no repository and keeps notifications.github.> whole. PUB's held daemon, whose
// exchange entry lists PUB alone, is admitted, and on the hub it creates legion-go-PUB-github on
// notifications.github.> and pulls the GitHub events a static publisher put on
// notifications.github.acme.priv.pull_request, a repository only PRIV's issues resolve to, and on
// notifications.github.acme.none.pull_request, which no issue names. Dispatch's answers carry no
// repository (projectaccess.Answer has no such field), so nothing a refresh reads can change what
// the grant holds of GitHub. The control builds the held daemon's grant without its GitHub
// consumer's create, pull and acknowledgements: the hub refuses the create, the daemon pulls no
// GitHub event, and the test fails naming the create subject.
func TestAHeldDaemonHearsEveryRepositorysGitHubEvents(t *testing.T) {
	const (
		consumer = "legion-go-PUB-github"
		family   = "notifications.github.>"
		create   = "$JS.API.CONSUMER.CREATE." + bus.Stream + "." + consumer + "." + family
	)
	now := time.Now()
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}}
	tokens := map[string]platformToken{"pub-daemon-token": {account: pubDaemonAccount, issued: now, expiry: now.Add(time.Hour)}}
	exchange := map[string][]string{pubDaemonAccount: {"PUB"}}
	dispatch := &dispatchAnswers{
		projects: map[string]bool{"PUB": true, "PRIV": true},
		private:  map[string]bool{"PRIV": true},
	}

	// Through the hub: a static user that creates the notification stream and publishes the two
	// events, and PUB's daemon admitted by the service on its token.
	keys := testnats.NewCalloutKeys(t)
	adminSeed, adminKey := testnats.User(t)
	admin := testnats.StaticUser{NKey: adminKey, Publish: []string{"$JS.API.>", "notifications.>"}, Subscribe: []string{"_INBOX.>"}}
	url := testnats.StartWithCallout(t, keys, admin)
	startServiceOf(t, url, keys, services, Deps{ServiceToken: serviceTokens(tokens), Exchange: exchangeOf(exchange), Access: dispatch.access})
	adminConn, err := testnats.ConnectSeed(t, url, adminSeed)
	require.NoError(t, err)
	t.Cleanup(adminConn.Close)
	adminJS, err := jetstream.New(adminConn)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	_, err = adminJS.CreateStream(ctx, jetstream.StreamConfig{Name: bus.Stream, Subjects: bus.StreamCarriedSubjects()})
	require.NoError(t, err)
	events := []struct{ subject, body string }{
		{"notifications.github.acme.priv.pull_request", "acme/priv's pull request"},
		{"notifications.github.acme.none.pull_request", "acme/none's pull request"},
	}
	var want []string
	for _, event := range events {
		_, err := adminJS.Publish(ctx, event.subject, []byte(event.body))
		require.NoError(t, err, "publish %s", event.subject)
		want = append(want, event.body)
	}

	var daemon *nats.Conn
	testwait.Eventually(t, "the hub to admit the PUB daemon", func() bool {
		var err error
		daemon, err = nats.Connect(url, nats.UserInfo(natscred.ServiceUser, "pub-daemon-token"), nats.NoReconnect(),
			nats.CustomInboxPrefix(natsgrant.InboxPrefix(pubDaemonAccount)))
		return err == nil
	})
	t.Cleanup(daemon.Close)
	js, err := jetstream.New(daemon)
	require.NoError(t, err)
	// The hub sends no answer to a create it refuses, so the create gets a bound of its own.
	createCtx, cancelCreate := context.WithTimeout(ctx, 5*time.Second)
	github, err := js.CreateOrUpdateConsumer(createCtx, bus.Stream, jetstream.ConsumerConfig{
		Durable: consumer, FilterSubject: family, AckPolicy: jetstream.AckExplicitPolicy,
	})
	cancelCreate()
	require.NoError(t, err, "the PUB daemon could not create its GitHub consumer: %s", create)
	batch, err := github.Fetch(len(events), jetstream.FetchMaxWait(5*time.Second))
	require.NoError(t, err)
	var got []string
	for msg := range batch.Messages() {
		got = append(got, string(msg.Data()))
		require.NoError(t, msg.DoubleAck(ctx))
	}
	require.NoError(t, batch.Error())
	require.ElementsMatch(t, want, got, "the PUB daemon's GitHub consumer did not deliver every repository's events")
}
