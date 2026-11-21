package natscallout

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// The accounts of Dispatch's own ServiceAccount and of Dispatch's listener Deployment once it runs
// as an account of its own, and the machine id both name their grants by.
const (
	dispatchAccount         = "system:serviceaccount:dispatch:dispatch"
	dispatchListenerAccount = "system:serviceaccount:dispatch:dispatch-listener"
	dispatchMachineID       = "example-dispatch"
	// natsAudience is the audience an ordinary service token carries (BROKER_NATS_K8S_AUDIENCE).
	natsAudience = "envoy-nats"
)

// serviceJWT is an unsigned JWT naming account as its subject and audience as its audience,
// issued now and expiring at expiry, distinct from every other one by its jti. The auth service
// reads its audience, unverified, to choose the verifier it goes to (hubAdmissionToken); natscred
// reads its expiry; the stub verifiers below are keyed by it whole and verify nothing.
func serviceJWT(t testing.TB, audience, account string, expiry time.Time) string {
	t.Helper()
	encode := func(v any) string {
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(map[string]string{"alg": "none"}) + "." + encode(map[string]any{
		"iss": "https://cluster.example", "sub": account, "aud": audience,
		"iat": time.Now().Unix(), "exp": expiry.Unix(), "jti": t.Name() + "/" + account + "/" + audience,
	}) + ".unsigned"
}

// proofTokens is a Deps.HubAdmissionToken that verifies each raw token tokens names, as the
// broker's verifier for natsgrant.HubAdmissionAudience would, and refuses every other.
func proofTokens(tokens map[string]platformToken) func(context.Context, string) (string, time.Time, time.Time, error) {
	return func(_ context.Context, raw string) (string, time.Time, time.Time, error) {
		token, ok := tokens[raw]
		if !ok {
			return "", time.Time{}, time.Time{}, errors.New("the hub-admission token did not verify")
		}
		return token.account, token.issued, token.expiry, nil
	}
}

// tagged is req as a hub rendered with private-project support sends it: its server's tags hold
// natsgrant.PrivateProjectsServerTag.
func tagged(req Request) Request {
	req.ServerTags = []string{"some-other-tag", natsgrant.PrivateProjectsServerTag}
	return req
}

// named is req from a client whose connection is named name.
func named(req Request, name string) Request {
	req.ClientName = name
	return req
}

// union is a Deps.PrivateProjects that answers projects on every read.
func union(projects PrivateProjects) func(context.Context) (PrivateProjects, error) {
	return func(context.Context) (PrivateProjects, error) { return projects, nil }
}

// privateHubKeys are the callout keys of a hub rendered with private-project support: its
// configuration carries natsgrant.PrivateProjectsServerTag in server_tags.
func privateHubKeys(t *testing.T) testnats.CalloutKeys {
	t.Helper()
	keys := testnats.NewCalloutKeys(t)
	keys.ServerTags = []string{natsgrant.PrivateProjectsServerTag}
	return keys
}

// proofServices are the services the fixture tests admit: Dispatch's own account and a listener's.
var proofServices = []ServiceEntry{
	{Kind: KindDispatch, MachineID: dispatchMachineID, Account: dispatchAccount, AllProjects: true},
	{Kind: KindListener, MachineID: "example-hub", Account: listenerAccount, AllProjects: true},
}

// connectProof connects to url with Dispatch's hub-admission token raw, taking the reply prefix
// the grant names, and hands every asynchronous error the server sends it to errs.
func connectProof(t *testing.T, url, raw string, errs chan<- error) (*nats.Conn, error) {
	t.Helper()
	options := []nats.Option{
		nats.UserInfo(natscred.ServiceUser, raw), nats.Name("dispatch hub admission"),
		nats.CustomInboxPrefix(natsgrant.InboxPrefix(dispatchAccount)), nats.NoReconnect(), nats.Timeout(5 * time.Second),
	}
	if errs != nil {
		options = append(options, nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			select {
			case errs <- err:
			default:
			}
		}))
	}
	nc, err := nats.Connect(url, options...)
	if err == nil {
		t.Cleanup(nc.Close)
	}
	return nc, err
}

// proveRoundTrip makes the proof's one round trip on nc: a publish on natsgrant.HubAdmissionPing,
// which the server delivers back to nc's own subscription to it under nc's own reply prefix. The
// grant lets the connection publish nothing else, a reply included, so the round trip is the ping
// coming back.
func proveRoundTrip(t *testing.T, nc *nats.Conn) error {
	t.Helper()
	ping := natsgrant.HubAdmissionPing(dispatchAccount)
	sub, err := nc.SubscribeSync(ping)
	if err != nil {
		return err
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := nc.Flush(); err != nil {
		return err
	}
	body := []byte(nats.NewInbox())
	if err := nc.Publish(ping, body); err != nil {
		return err
	}
	msg, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		return err
	}
	if string(msg.Data) != string(body) {
		return errors.New("the ping came back as " + string(msg.Data))
	}
	return nil
}

// awaitPublishViolation waits for the server's permissions violation of a publish to subject among
// errs, failing the test when it does not come.
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
			t.Fatalf("no %s: the server took the publish to %s", want, subject)
		}
	}
}

// Every proof is decided afresh: two proofs on two new connections with the same token make two
// decisions, counted at the auth service, and with the auth service stopped a third proof is
// refused while the connection the second proof left open still answers the server's PING. So a
// proof that dials a new connection each time reads the hub's admission now, never a decision
// some earlier connection was given. The control, a decision cache keyed on the token, makes the
// second proof without a decision, and the test fails naming the count.
func TestEveryAdmissionProofIsDecidedAfresh(t *testing.T) {
	now := time.Now()
	raw := serviceJWT(t, natsgrant.HubAdmissionAudience, dispatchAccount, now.Add(time.Hour))
	keys := privateHubKeys(t)
	url := testnats.StartWithCallout(t, keys)
	var decided atomic.Int64
	counting := func(ctx context.Context, deps Deps, services []ServiceEntry, req Request) (Decision, error) {
		if req.Password == raw {
			decided.Add(1)
		}
		return Decide(ctx, deps, services, req)
	}
	deps := Deps{HubAdmissionToken: proofTokens(map[string]platformToken{raw: {account: dispatchAccount, issued: now, expiry: now.Add(time.Hour)}})}
	service := startServiceAdmitting(t, url, keys, proofServices, deps, counting)

	first, err := connectProof(t, url, raw, nil)
	require.NoError(t, err, "the first proof was refused")
	require.NoError(t, proveRoundTrip(t, first))
	first.Close()
	held, err := connectProof(t, url, raw, nil)
	require.NoError(t, err, "the second proof was refused")
	require.NoError(t, proveRoundTrip(t, held))
	require.EqualValues(t, 2, decided.Load(), "two proofs on two new connections made %d decisions", decided.Load())

	service.Close()
	require.NoError(t, held.FlushTimeout(5*time.Second), "the held connection no longer answers PING")
	_, err = connectProof(t, url, raw, nil)
	require.Error(t, err, "a proof was admitted with the auth service stopped")
	require.EqualValues(t, 2, decided.Load())
}

// Only a hub rendered with private-project support admits Dispatch's hub-admission proof. On a
// server whose configuration carries natsgrant.PrivateProjectsServerTag the proof connection is
// admitted, its ping on its own prefix comes back to it, and its subscribe to every notification
// topic and its publish of a JetStream call are refused by the server; on a server without the tag
// the auth service refuses the connection. The control, an admission that ignores the tag, admits
// the connection on the untagged server, and the test fails naming the server.
func TestOnlyAHubRenderedPrivateAdmitsTheProof(t *testing.T) {
	now := time.Now()
	raw := serviceJWT(t, natsgrant.HubAdmissionAudience, dispatchAccount, now.Add(time.Hour))
	deps := Deps{HubAdmissionToken: proofTokens(map[string]platformToken{raw: {account: dispatchAccount, issued: now, expiry: now.Add(time.Hour)}})}

	t.Run("a hub rendered private", func(t *testing.T) {
		keys := privateHubKeys(t)
		url := testnats.StartWithCallout(t, keys)
		startServiceOf(t, url, keys, proofServices, deps)
		errs := make(chan error, 16)
		nc, err := connectProof(t, url, raw, errs)
		require.NoError(t, err, "the hub rendered private refused Dispatch's proof")
		require.NoError(t, proveRoundTrip(t, nc), "the proof's ping on its own prefix did not come back")
		_, err = nc.SubscribeSync(bus.NotificationTopicPrefix + ">")
		require.NoError(t, err)
		require.NoError(t, nc.Flush())
		awaitViolation(t, errs, bus.NotificationTopicPrefix+">")
		require.NoError(t, nc.Publish("$JS.API.INFO", nil))
		require.NoError(t, nc.Flush())
		awaitPublishViolation(t, errs, "$JS.API.INFO")
	})
	t.Run("a hub not rendered private", func(t *testing.T) {
		keys := testnats.NewCalloutKeys(t)
		url := testnats.StartWithCallout(t, keys)
		startServiceOf(t, url, keys, proofServices, deps)
		nc, err := connectProof(t, url, raw, nil)
		require.Error(t, err, "the hub at %s, not rendered private, admitted Dispatch's proof", url)
		require.Nil(t, nc)
		require.ErrorContains(t, err, "Authorization Violation")
	})
}

// The admitted proof connection holds nothing: a subscribe to anything outside its own reply
// prefix is refused, and so is a publish to any subject but natsgrant.HubAdmissionPing, each tried
// and refused in turn: JetStream's API, a KV bucket's keys, every client's reply inboxes, a server
// monitoring request, project subjects and another subject under its own prefix. The control,
// natsgrant.HubAdmission with $KV.> added to its publish, takes the publish to a bucket's key, and
// the test fails naming it.
func TestTheProofConnectionHoldsNothing(t *testing.T) {
	now := time.Now()
	raw := serviceJWT(t, natsgrant.HubAdmissionAudience, dispatchAccount, now.Add(time.Hour))
	deps := Deps{HubAdmissionToken: proofTokens(map[string]platformToken{raw: {account: dispatchAccount, issued: now, expiry: now.Add(time.Hour)}})}
	keys := privateHubKeys(t)
	url := testnats.StartWithCallout(t, keys)
	startServiceOf(t, url, keys, proofServices, deps)
	errs := make(chan error, 64)
	nc, err := connectProof(t, url, raw, errs)
	require.NoError(t, err, "the hub rendered private refused Dispatch's proof")
	require.NoError(t, proveRoundTrip(t, nc))

	for _, subject := range []string{
		"$JS.API.>", "$KV.>", "_INBOX.>", bus.NotificationTopicPrefix + ">",
		"notifications.dispatch.issue.PUB.>", natsgrant.InboxPrefix(listenerAccount) + ".>", ">",
	} {
		_, err := nc.SubscribeSync(subject)
		require.NoError(t, err)
		require.NoError(t, nc.Flush())
		awaitViolation(t, errs, subject)
	}
	for _, subject := range []string{
		"$JS.API.INFO", "$JS.API.STREAM.INFO." + bus.Stream, "$JS.API.CONSUMER.CREATE." + bus.Stream + ".x",
		"$KV." + bus.RoleClaimBucket + ".PUB.implementer", "_INBOX.someone-else", "$SYS.REQ.SERVER.PING.CONNZ",
		"notifications.dispatch.issue.PUB.PUB-1.created", "notifications.agent.PUB.ses_a", "notifications.role.PUB.implementer",
		natsgrant.InboxPrefix(dispatchAccount) + ".other",
	} {
		require.NoError(t, nc.Publish(subject, []byte("x")))
		require.NoError(t, nc.Flush())
		awaitPublishViolation(t, errs, subject)
	}
	require.NoError(t, proveRoundTrip(t, nc), "the ping itself is no longer answered")
}
