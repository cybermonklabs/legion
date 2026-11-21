package natscallout

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// TestNoNonOperatorGrantCanSourceTheNotificationStreamInProcess is
// TestNoNonOperatorGrantCanSourceTheNotificationStream on the nats-server this process links.
func TestNoNonOperatorGrantCanSourceTheNotificationStreamInProcess(t *testing.T) {
	requireNoGrantSourcesTheNotificationStream(t, func(keys testnats.CalloutKeys, static ...testnats.StaticUser) string {
		return testnats.StartWithCallout(t, keys, static...)
	})
}

// privateSubject and privateBody are the stored message the attack below reads when it can: a
// private project's issue event on the notification stream.
const (
	privateSubject = "notifications.dispatch.issue.PRIV.1"
	privateBody    = "PRIV-SECRET-BODY"
)

// sourceAttack is one connection the attack runs under, decided by the service as name.
type sourceAttack struct {
	name string
	// connect connects as the row's credential, handing the server's asynchronous errors to errs.
	connect func(t *testing.T, url string, errs chan<- error) *nats.Conn
	// raw is the row's kind's grant as natsgrant builds it, before the service narrows it: the
	// streams it named a create or an update of are the ones the row tries.
	raw natsgrant.Permissions
	// inbox is a subject the row's grant subscribes, and src the republish source the row asks
	// for: every subject of the stream, mapped under inbox, or the one private subject, mapped onto
	// inbox itself. reply is the subject the row's requests ask their answer on, one its grant
	// subscribes, or empty for a session, which subscribes no reply inbox: its requests are sent
	// with no reply, and only the refusal is read.
	inbox, src, reply string
}

// requireNoGrantSourcesTheNotificationStream is the measurement of 2026-11-17 made a row: a user
// holding a stream create or update can make a stream that sources ENVOY_NOTIFICATIONS and
// republishes every message to its own inbox, and so reads every stored message of the account,
// because nats-server checks a republish destination only against the stream's own subjects. On
// the server start returns, under the service's answer for a session and for each service kind in
// turn, each create or update its kind's grant named, of a stream sourcing ENVOY_NOTIFICATIONS
// with that republish, and a create of a stream no grant names, is refused with a publish
// permissions violation and draws no answer, and the connection receives nothing. The control row
// is the measurement itself: the measurement job answered with the grant it held before it
// configured no stream (measureGrantBefore), unnarrowed, creates ENVOY_MEASURE so and receives the
// private message.
func requireNoGrantSourcesTheNotificationStream(t *testing.T, start func(testnats.CalloutKeys, ...testnats.StaticUser) string) {
	t.Helper()
	keys := testnats.NewCalloutKeys(t)
	adminSeed, adminKey := testnats.User(t)
	url := start(keys, testnats.StaticUser{NKey: adminKey, Publish: []string{"$JS.API.>", "notifications.>"}, Subscribe: []string{"_INBOX.>"}})
	admin, err := testnats.ConnectSeed(t, url, adminSeed)
	require.NoError(t, err)
	defer admin.Close()
	adminJS, err := admin.JetStream()
	require.NoError(t, err)
	_, err = adminJS.AddStream(&nats.StreamConfig{Name: bus.Stream, Subjects: []string{"notifications.dispatch.>"}, Storage: nats.MemoryStorage})
	require.NoError(t, err)
	_, err = adminJS.Publish(privateSubject, []byte(privateBody))
	require.NoError(t, err)

	now := time.Now()
	tokens := map[string]platformToken{
		"dispatch-token": {account: "system:serviceaccount:dispatch:dispatch", issued: now, expiry: now.Add(time.Hour)},
		"listener-token": {account: listenerAccount, issued: now, expiry: now.Add(time.Hour)},
		"daemon-token":   {account: daemonAccount, issued: now, expiry: now.Add(time.Hour)},
		"measure-token":  {account: measureAccount, google: true, issued: now, expiry: now.Add(time.Hour)},
	}
	a := newSession(t, "ses_a", 15*time.Minute)
	deps := Deps{Introspect: introspector(a), ServiceToken: serviceTokens(tokens)}
	// unnarrowed answers the measurement job with the grant it held before it configured no
	// stream, every other connection as the service does: the control row's answer.
	unnarrowed := func(ctx context.Context, deps Deps, services []ServiceEntry, req Request) (Decision, error) {
		decision, err := Decide(ctx, deps, services, req)
		if err == nil && strings.HasPrefix(decision.Name, string(KindMeasure)+"/") {
			decision.Grant = measureGrantBefore()
		}
		return decision, err
	}
	service := func(raw string) func(*testing.T, string, chan<- error) *nats.Conn {
		return func(t *testing.T, url string, errs chan<- error) *nats.Conn {
			// The measurement job asks on an inbox of its own, the one its grant subscribes.
			prefix := nats.InboxPrefix[:len(nats.InboxPrefix)-1]
			if raw == "measure-token" {
				prefix = "_INBOX_envoy_measure"
			}
			nc, err := nats.Connect(url, nats.UserInfo("service", raw), nats.NoReconnect(), nats.Timeout(10*time.Second), nats.CustomInboxPrefix(prefix),
				nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
					select {
					case errs <- err:
					default:
					}
				}))
			require.NoError(t, err, "the service did not admit %s", raw)
			t.Cleanup(nc.Close)
			return nc
		}
	}
	session := func(t *testing.T, url string, errs chan<- error) *nats.Conn {
		nc, err := connectSession(t, url, a, errs)
		require.NoError(t, err, "the service did not admit session %s", a.id)
		return nc
	}
	rows := []sourceAttack{
		{name: "session", connect: session, inbox: bus.DeliverSubject(a.id), src: privateSubject},
		{name: "dispatch", connect: service("dispatch-token"), raw: natsgrant.Dispatch("example-dispatch"), inbox: "_INBOX.attack.dispatch", src: ">", reply: "_INBOX.reply.dispatch"},
		{name: "listener", connect: service("listener-token"), raw: natsgrant.Listener("example-hub"), inbox: "_INBOX.attack.listener", src: ">", reply: "_INBOX.reply.listener"},
		{name: "daemon", connect: service("daemon-token"), raw: natsgrant.Daemon(), inbox: "_INBOX.attack.daemon", src: ">", reply: "_INBOX.reply.daemon"},
		{name: "measure", connect: service("measure-token"), raw: measureGrantBefore(), inbox: "_INBOX_envoy_measure.attack", src: ">", reply: "_INBOX_envoy_measure.reply"},
	}

	t.Run("control: the measurement job's grant before it configured no stream reads the private message", func(t *testing.T) {
		startService(t, url, keys, deps, unnarrowed)
		errs := make(chan error, 16)
		nc := service("measure-token")(t, url, errs)
		received, err := nc.SubscribeSync("_INBOX_envoy_measure.control.>")
		require.NoError(t, err)
		require.NoError(t, nc.Flush())
		t.Cleanup(func() { _ = adminJS.DeleteStream("ENVOY_MEASURE") })
		answer, err := nc.Request("$JS.API.STREAM.CREATE.ENVOY_MEASURE", attackConfig(t, "ENVOY_MEASURE", ">", "_INBOX_envoy_measure.control.>"), 5*time.Second)
		require.NoError(t, err, "the unnarrowed measurement grant's create drew no answer: %v", drained(errs))
		require.NotContains(t, string(answer.Data), `"error"`, "the unnarrowed measurement grant's create was refused")
		msg, err := received.NextMsg(5 * time.Second)
		require.NoError(t, err, "the control read nothing: the instrument cannot see a leak")
		require.Equal(t, privateBody, string(msg.Data))
	})

	t.Run("the service's answers", func(t *testing.T) {
		startService(t, url, keys, deps, Decide)
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				errs := make(chan error, 64)
				nc := row.connect(t, url, errs)
				dest := row.inbox
				if row.src == ">" {
					dest += ".>"
				}
				received, err := nc.SubscribeSync(dest)
				require.NoError(t, err)
				require.NoError(t, nc.Flush())
				for _, target := range attackTargets(row.raw) {
					verb, body := "CREATE", attackConfig(t, target, row.src, dest)
					if target == bus.Stream {
						info, err := adminJS.StreamInfo(bus.Stream)
						require.NoError(t, err)
						update := info.Config
						update.RePublish = &nats.RePublish{Source: row.src, Destination: dest}
						verb, body = "UPDATE", mustJSON(t, update)
					}
					subject := "$JS.API.STREAM." + verb + "." + target
					var answers *nats.Subscription
					if row.reply != "" {
						answers, err = nc.SubscribeSync(row.reply)
						require.NoError(t, err)
					}
					if row.reply == "" {
						require.NoError(t, nc.Publish(subject, body))
					} else {
						require.NoError(t, nc.PublishRequest(subject, row.reply, body))
					}
					awaitPublishViolation(t, errs, subject)
					if answers != nil {
						if answer, err := answers.NextMsg(time.Second); err == nil {
							t.Fatalf("%s's %s was answered %s", row.name, subject, answer.Data)
						}
						require.NoError(t, answers.Unsubscribe())
					}
				}
				_, err = adminJS.Publish(privateSubject, []byte(privateBody))
				require.NoError(t, err)
				if msg, err := received.NextMsg(time.Second); err == nil {
					t.Fatalf("%s received %q on %s", row.name, msg.Data, msg.Subject)
				}
				names := adminJS.StreamNames()
				var streams []string
				for name := range names {
					streams = append(streams, name)
				}
				require.Equal(t, []string{bus.Stream}, streams, "a stream was made under %s's answer", row.name)
				info, err := adminJS.StreamInfo(bus.Stream)
				require.NoError(t, err)
				require.Nil(t, info.Config.RePublish, "%s's answer changed the notification stream", row.name)
			})
		}
	})
}

// measureGrantBefore is the measurement job's grant before it configured no stream: Measure() with
// the create, update and delete of ENVOY_MEASURE it once held. The control row reads the private
// message under it, and the measure row tries its creates under the service's answer.
func measureGrantBefore() natsgrant.Permissions {
	grant := natsgrant.Measure()
	grant.Publish = append(slices.Clone(grant.Publish),
		"$JS.API.STREAM.CREATE.ENVOY_MEASURE", "$JS.API.STREAM.UPDATE.ENVOY_MEASURE", "$JS.API.STREAM.DELETE.ENVOY_MEASURE")
	return grant
}

// attackTargets are the streams raw names a create or an update of, and ATTACK, which no grant
// names: the requests a connection under raw's narrowed answer tries.
func attackTargets(raw natsgrant.Permissions) []string {
	targets := []string{"ATTACK"}
	for _, entry := range raw.Publish {
		for _, verb := range []string{"$JS.API.STREAM.CREATE.", "$JS.API.STREAM.UPDATE."} {
			if name, found := strings.CutPrefix(entry, verb); found && !strings.ContainsAny(name, "*>") {
				if !slices.Contains(targets, name) {
					targets = append(targets, name)
				}
			}
		}
	}
	return targets
}

// attackConfig is a stream named name, in memory, that sources ENVOY_NOTIFICATIONS and republishes
// what it stores from src to dest.
func attackConfig(t *testing.T, name, src, dest string) []byte {
	t.Helper()
	return mustJSON(t, nats.StreamConfig{
		Name: name, Storage: nats.MemoryStorage,
		Sources:   []*nats.StreamSource{{Name: bus.Stream}},
		RePublish: &nats.RePublish{Source: src, Destination: dest},
	})
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
}

// awaitPublishViolation waits for the server's publish permissions violation naming subject
// among errs, failing the test when it does not come.
func awaitPublishViolation(t *testing.T, errs <-chan error, subject string) {
	t.Helper()
	want := `Permissions Violation for Publish to "` + subject + `"`
	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-errs:
			if strings.Contains(err.Error(), want) {
				return
			}
		case <-deadline:
			t.Fatalf("no %s: the request drew no answer and no refusal", want)
		}
	}
}

// drained is every error errs holds now, for a failure's message.
func drained(errs <-chan error) []error {
	var all []error
	for {
		select {
		case err := <-errs:
			all = append(all, err)
		default:
			return all
		}
	}
}
