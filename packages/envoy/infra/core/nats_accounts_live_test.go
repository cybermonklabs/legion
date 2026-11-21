package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// These rows run nats-server v2.15.1 in this process on core's own render (Render), the text a hub
// reads, with a stand-in auth service that admits each client by the token it presents and answers
// with that client's grant, placed in the account the deployment's broker places it in: ENVOY on a
// render with privateProjects (t-e-73's BROKER_NATS_ACCOUNT), $G on one
// without. Every grant is natsgrant's own function for the class, never a set written here; where a
// row needs a grant as plan E's t-e-74 will build it, only the one inbox entry that plan names is
// moved, and the row says so. The held daemon's grant (natsgrant.WithinProjects) is not on this
// branch's base, so its class has no row yet.

// The stream subjects a row aims a JetStream answer at: a private project's issue subject, and a key
// of a bucket no class may write.
const (
	sessionConsumersBucket = "envoy_session_consumers"
	privateIssuePrefix     = "notifications.dispatch.issue.PRIV."
)

// grantClass is one kind of client, its token at the stand-in auth service, and its grant.
type grantClass struct {
	name  string
	grant natsgrant.Permissions
}

// grantClasses are the classes of natsgrant's static sets: every set a service or a session holds
// today, each as its function returns it.
func grantClasses(t *testing.T) []grantClass {
	t.Helper()
	session, err := natsgrant.Session("s1")
	require.NoError(t, err)
	return []grantClass{
		{"Host", natsgrant.Host("m1")},
		{"Listener", natsgrant.Listener("m1")},
		{"ListenerHost", natsgrant.ListenerHost("m1")},
		{"Dispatch", natsgrant.Dispatch("m1")},
		{"Daemon", natsgrant.Daemon()},
		{"Measure", natsgrant.Measure()},
		{"Session", session},
	}
}

// movedInbox is grant with its one inbox subscribe entry replaced by prefix's, the only entry plan
// E's t-e-74 moves (natsgrant.InboxPrefix and the measure grant under _INBOX_envoy).
func movedInbox(t *testing.T, grant natsgrant.Permissions, prefix string) natsgrant.Permissions {
	t.Helper()
	moved := grant
	moved.Subscribe = nil
	replaced := 0
	for _, entry := range grant.Subscribe {
		if strings.HasPrefix(entry, "_INBOX") && strings.HasSuffix(entry, ".>") {
			entry = prefix + ".>"
			replaced++
		}
		moved.Subscribe = append(moved.Subscribe, entry)
	}
	require.Equal(t, 1, replaced, "the grant subscribes %d inbox prefixes: %v", replaced, grant.Subscribe)
	return moved
}

// inboxPrefix is the reply prefix grant subscribes, which its client connects with: the first
// subscribe entry naming an inbox prefix, or nats.go's default where it names none.
func inboxPrefix(grant natsgrant.Permissions) string {
	for _, entry := range grant.Subscribe {
		if strings.HasPrefix(entry, "_INBOX") && strings.HasSuffix(entry, ".>") {
			return strings.TrimSuffix(entry, ".>")
		}
	}
	return "_INBOX"
}

// permits reports whether a publish to subject passes grant: an allow entry matches it and no deny
// entry does.
func permits(grant natsgrant.Permissions, subject string) bool {
	matches := func(entries []string) bool {
		for _, entry := range entries {
			if subjectMatches(entry, subject) {
				return true
			}
		}
		return false
	}
	return matches(grant.Publish) && !matches(grant.PublishDeny)
}

// subjectMatches reports whether the NATS pattern matches the literal subject.
func subjectMatches(pattern, subject string) bool {
	p, s := strings.Split(pattern, "."), strings.Split(subject, ".")
	for i, token := range p {
		if token == ">" {
			return len(s) > i
		}
		if i >= len(s) || (token != "*" && token != s[i]) {
			return false
		}
	}
	return len(p) == len(s)
}

// accountHub is a hub started on core's render, with the stand-in auth service connected and the
// render's operator: in JETSTREAM beside a private render, a static user of $G beside the merge
// base's.
type accountHub struct {
	url      string
	accounts bool
	operator natsgo.JetStreamContext
	operConn *natsgo.Conn
}

// hubAuthorization is the authorization a row's hub renders: the auth service's user, the
// operator, and the callout, with privateProjects as accounts says. Beside privateProjects the
// operator is the jetStreamOperator, in JETSTREAM; without it, it is a static user of $G, where
// the merge base's render puts every user.
func hubAuthorization(keys testnats.CalloutKeys, operatorKey string, accounts bool) core.Authorization {
	a := core.Authorization{
		Users: []core.User{
			{Name: "example-auth", NKey: keys.ServicePublic, Publish: &core.Rules{Allow: []string{"$SYS._INBOX.>"}},
				Subscribe: &core.Rules{Allow: []string{"$SYS.REQ.USER.AUTH"}}, HasSeed: true},
			{Name: "example-jetstream", NKey: operatorKey, Publish: &core.Rules{Allow: []string{">"}},
				Subscribe: &core.Rules{Allow: []string{">"}}, HasSeed: true},
		},
		Callout:         &core.AuthCallout{Issuer: keys.IssuerPublic, XKey: keys.XKeyPublic, ServiceUser: "example-auth"},
		PrivateProjects: accounts,
	}
	if accounts {
		a.JetStreamOperator = "example-jetstream"
	}
	return a
}

// startAccountHub starts nats-server on the render of hubAuthorization, with JetStream on, edit
// applied to the rendered text when it is not nil (a control's change of the layout), and the
// stand-in auth service answering each token grants names with its grant.
func startAccountHub(t *testing.T, accounts bool, edit func(t *testing.T, text string) string, grants map[string]natsgrant.Permissions) *accountHub {
	t.Helper()
	keys := testnats.NewCalloutKeys(t)
	operatorSeed, operatorKey := testnats.User(t)
	text, err := hubAuthorization(keys, operatorKey, accounts).Render(nil)
	require.NoError(t, err)
	if edit != nil {
		text = edit(t, text)
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "nats-server.conf")
	config := fmt.Sprintf("listen: \"127.0.0.1:-1\"\njetstream: {store_dir: %q}\n%s", filepath.Join(dir, "jetstream"), text)
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))
	opts, err := natsserver.ProcessConfigFile(configPath)
	require.NoError(t, err, "nats-server refuses the rendered configuration")
	opts.NoSigs, opts.NoLog = true, true
	server, err := natsserver.NewServer(opts)
	require.NoError(t, err)
	go server.Start()
	t.Cleanup(func() {
		server.Shutdown()
		server.WaitForShutdown()
	})
	require.True(t, server.ReadyForConnections(10*time.Second), "nats-server did not start")

	place := natsserver.DEFAULT_GLOBAL_ACCOUNT
	if accounts {
		place = core.ClientAccount
	}
	serveCallout(t, server.ClientURL(), keys, place, grants)
	operConn, err := testnats.ConnectSeed(t, server.ClientURL(), operatorSeed)
	require.NoError(t, err, "the operator, an auth user, connects without the auth service")
	t.Cleanup(operConn.Close)
	operator, err := operConn.JetStream()
	require.NoError(t, err)
	return &accountHub{url: server.ClientURL(), accounts: accounts, operator: operator, operConn: operConn}
}

// serveCallout connects the stand-in auth service as the rendered service user and answers each
// authorization request: the token's grant from grants, placed in place, or a refusal for a token
// grants does not name. It answers as natscallout does (callout.go admit and sign): the user JWT's
// audience is the account, its permissions the grant, both signed by the issuer and sealed to the
// server's curve key.
func serveCallout(t *testing.T, url string, keys testnats.CalloutKeys, place string, grants map[string]natsgrant.Permissions) {
	t.Helper()
	issuer, err := nkeys.FromSeed([]byte(keys.IssuerSeed))
	require.NoError(t, err)
	curve, err := nkeys.FromSeed([]byte(keys.XKeySeed))
	require.NoError(t, err)
	service, err := testnats.ConnectSeed(t, url, keys.ServiceSeed)
	require.NoError(t, err, "the auth service's user, an auth user, connects without a callout")
	t.Cleanup(service.Close)
	_, err = service.Subscribe("$SYS.REQ.USER.AUTH", func(m *natsgo.Msg) {
		opened, err := curve.Open(m.Data, m.Header.Get("Nats-Server-Xkey"))
		if err != nil {
			t.Errorf("open the sealed authorization request: %v", err)
			return
		}
		request, err := jwt.DecodeAuthorizationRequestClaims(string(opened))
		if err != nil {
			t.Errorf("decode the authorization request: %v", err)
			return
		}
		response := jwt.NewAuthorizationResponseClaims(request.UserNkey)
		response.Audience = request.Server.ID
		token := request.ConnectOptions.Token
		if grant, ok := grants[token]; ok {
			user := jwt.NewUserClaims(request.UserNkey)
			user.Audience = place
			user.Name = token
			user.Pub.Allow.Add(grant.Publish...)
			user.Sub.Allow.Add(grant.Subscribe...)
			user.Pub.Deny.Add(grant.PublishDeny...)
			user.Sub.Deny.Add(grant.SubscribeDeny...)
			if response.Jwt, err = user.Encode(issuer); err != nil {
				t.Errorf("sign the user: %v", err)
				return
			}
		} else {
			response.Error = "example: no grant for this token"
		}
		answer, err := response.Encode(issuer)
		if err != nil {
			t.Errorf("sign the answer: %v", err)
			return
		}
		sealed, err := curve.Seal([]byte(answer), request.Server.XKey)
		if err != nil {
			t.Errorf("seal the answer: %v", err)
			return
		}
		if err := m.Respond(sealed); err != nil {
			t.Errorf("answer the authorization request: %v", err)
		}
	})
	require.NoError(t, err)
	require.NoError(t, service.Flush())
}

// publishRefusals collects the permission violations the server reports to one client.
type publishRefusals struct {
	mu   sync.Mutex
	seen []string
}

func (v *publishRefusals) handler(_ *natsgo.Conn, _ *natsgo.Subscription, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen = append(v.seen, err.Error())
}

// saw reports whether the server refused a publish to subject.
func (v *publishRefusals) saw(subject string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	want := fmt.Sprintf("Permissions Violation for Publish to %q", subject)
	for _, seen := range v.seen {
		if strings.Contains(seen, want) {
			return true
		}
	}
	return false
}

// connect admits a client to hub under token, with the inbox prefix its grant subscribes.
func (h *accountHub) connect(t *testing.T, token string, grant natsgrant.Permissions) (*natsgo.Conn, *publishRefusals) {
	t.Helper()
	refused := &publishRefusals{}
	conn, err := natsgo.Connect(h.url, natsgo.Token(token), natsgo.CustomInboxPrefix(inboxPrefix(grant)),
		natsgo.NoReconnect(), natsgo.Timeout(5*time.Second), natsgo.ErrorHandler(refused.handler))
	require.NoError(t, err, "the auth service did not admit %s", token)
	t.Cleanup(conn.Close)
	return conn, refused
}

// stored is how many messages stream holds on subject, read by the operator.
func (h *accountHub) stored(t *testing.T, stream, subject string) uint64 {
	t.Helper()
	info, err := h.operator.StreamInfo(stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
	require.NoError(t, err)
	return info.State.Subjects[subject]
}

// storedMsg is the last message stream holds on subject, read by the operator.
func (h *accountHub) storedMsg(t *testing.T, stream, subject string) *natsgo.RawStreamMsg {
	t.Helper()
	msg, err := h.operator.GetLastMsg(stream, subject)
	require.NoError(t, err)
	return msg
}

// fence returns once the stream has stored everything that reached it before this call: the
// operator's own publish to a captured subject is acknowledged after every message the stream
// took in before it, since a stream stores what it takes in in order.
func (h *accountHub) fence(t *testing.T) {
	t.Helper()
	_, err := h.operator.Publish("notifications.dispatch.issue.FENCE.1", []byte("fence"))
	require.NoError(t, err)
}

// setUp makes, as the operator, the notification stream on the subjects the stream carries, the
// interest bucket and the session-consumer bucket, and a message on the stream.
func (h *accountHub) setUp(t *testing.T) {
	t.Helper()
	_, err := h.operator.AddStream(&natsgo.StreamConfig{Name: bus.Stream, Subjects: bus.StreamCarriedSubjects()})
	require.NoError(t, err)
	for _, bucket := range []string{bus.InterestBucket, sessionConsumersBucket} {
		_, err := h.operator.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bucket})
		require.NoError(t, err)
	}
	_, err = h.operator.Publish("notifications.dispatch.issue.PUB.1", []byte(`{"event":1}`))
	require.NoError(t, err)
}

// observer is a client of hub granted every subject, in the account the auth service places
// clients in, that sees each message published there on the subjects it subscribes: it sees the
// server's answer reach a target, the reading that says the answer was sent, whatever the stream
// then stores.
func (h *accountHub) observer(t *testing.T, subjects ...string) func(subject string) *natsgo.Msg {
	t.Helper()
	all := natsgrant.Permissions{Publish: []string{">"}, Subscribe: []string{">"}}
	conn, _ := h.connect(t, "observer", all)
	var mu sync.Mutex
	seen := map[string]*natsgo.Msg{}
	for _, subject := range subjects {
		_, err := conn.Subscribe(subject, func(m *natsgo.Msg) {
			mu.Lock()
			defer mu.Unlock()
			seen[m.Subject] = m
		})
		require.NoError(t, err)
	}
	require.NoError(t, conn.Flush())
	return func(subject string) *natsgo.Msg {
		mu.Lock()
		defer mu.Unlock()
		return seen[subject]
	}
}

// consumerCreate is a consumer create the grant permits: on the notification stream under a name
// of its own, or its listener durable's, or on the interest bucket's stream; "" where it permits none.
func consumerCreate(grant natsgrant.Permissions, class string) (stream, name, subject string) {
	for _, candidate := range []struct{ stream, name string }{
		{bus.Stream, "probe-" + strings.ToLower(class)},
		{bus.Stream, bus.ListenerDurable("m1")},
		{"KV_" + bus.InterestBucket, "probe-" + strings.ToLower(class)},
	} {
		subject := "$JS.API.CONSUMER.CREATE." + candidate.stream + "." + candidate.name
		if permits(grant, subject) {
			return candidate.stream, candidate.name, subject
		}
	}
	return "", "", ""
}

// TestNoJetStreamAnswerIsStoredOnACapturedSubject: no grant class can have
// a JetStream answer stored on a subject a stream captures. Each class connects through the
// auth service holding exactly its natsgrant set, with the inbox prefix that set subscribes to, and
// each row reads only what that set permits:
//   - its ordinary request, the first of STREAM.INFO.ENVOY_NOTIFICATIONS its grant permits, is
//     answered;
//   - the same request with its reply aimed at a private project's issue subject and at a key of
//     a bucket no class may write: the observer sees each answer sent there;
//   - a class that holds a key write and DIRECT.GET on the interest bucket (Host, Listener,
//     ListenerHost, Dispatch) writes a key and asks DIRECT.GET for it with the reply aimed at an
//     issue subject, so the stored bytes would be its own choice;
//   - a class allowed a consumer create makes a push consumer whose deliver subject is another
//     stream's captured subject, which the consumer is seen to deliver to;
//   - every class's plain publish to the bucket key is refused, and stores nothing.
//
// A class holding no JetStream API subject (Session) is refusal only, in both layouts: its request
// is refused, so it is never counted a stored or a clean reading.
//
// Control, in the same run: the merge base's render, every user in $G, where each class holding
// the API has the answer stored on each target its grant reaches, and the DIRECT.GET classes their
// chosen bytes; a row that stores nothing there fails naming the class, so a probe that reached no
// stream cannot pass the private render's row. The private render stores nothing on any target.
func TestNoJetStreamAnswerIsStoredOnACapturedSubject(t *testing.T) {
	for _, layout := range []struct {
		name     string
		accounts bool
	}{
		{"the merge base's render, every user in $G (control)", false},
		{"the private render, JetStream in an account of its own", true},
	} {
		t.Run(layout.name, func(t *testing.T) {
			classes := grantClasses(t)
			grants := map[string]natsgrant.Permissions{"observer": {Publish: []string{">"}, Subscribe: []string{">"}}}
			for _, class := range classes {
				grants[class.name] = class.grant
			}
			hub := startAccountHub(t, layout.accounts, nil, grants)
			hub.setUp(t)
			seen := hub.observer(t, privateIssuePrefix+">", "$KV."+sessionConsumersBucket+".>")
			want := uint64(0)
			if !layout.accounts {
				want = 1
			}
			for _, class := range classes {
				t.Run(class.name, func(t *testing.T) {
					requireNoStoredAnswer(t, hub, seen, class, want)
				})
			}
		})
	}
}

// requireNoStoredAnswer runs one class's rows on hub, each target holding want messages after it:
// 1 on the merge base's render, 0 on the private one.
func requireNoStoredAnswer(t *testing.T, hub *accountHub, seen func(string) *natsgo.Msg, class grantClass, want uint64) {
	conn, refused := hub.connect(t, class.name, class.grant)
	kvStream := "KV_" + sessionConsumersBucket

	plain := "$KV." + sessionConsumersBucket + ".plain-" + class.name
	require.NoError(t, conn.Publish(plain, []byte("plain")))
	require.NoError(t, conn.Flush())
	testwait.Eventually(t, class.name+"'s plain publish to "+plain+" refused", func() bool { return refused.saw(plain) })

	ordinary := "$JS.API.STREAM.INFO." + bus.Stream
	if !permits(class.grant, ordinary) {
		_, err := conn.Request(ordinary, nil, time.Second)
		require.Error(t, err, "%s holds no JetStream API subject and was answered", class.name)
		testwait.Eventually(t, class.name+"'s request refused", func() bool { return refused.saw(ordinary) })
		hub.fence(t)
		require.Zero(t, hub.stored(t, kvStream, plain), "%s's refused publish stored", class.name)
		return
	}
	answer, err := conn.Request(ordinary, nil, 5*time.Second)
	require.NoError(t, err, "%s's ordinary request, which its grant permits, was not answered", class.name)
	var info struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
	}
	require.NoError(t, json.Unmarshal(answer.Data, &info))
	require.Equal(t, "io.nats.jetstream.api.v1.stream_info_response", info.Type, "%s: %s", class.name, answer.Data)
	require.Empty(t, info.Error, "%s's ordinary request answered an error: %s", class.name, answer.Data)

	targets := []struct{ stream, subject string }{
		{bus.Stream, privateIssuePrefix + class.name},
		{kvStream, "$KV." + sessionConsumersBucket + "." + class.name},
	}
	for _, target := range targets {
		require.NoError(t, conn.PublishRequest(ordinary, target.subject, nil))
	}
	require.NoError(t, conn.Flush())
	for _, target := range targets {
		testwait.Eventually(t, "the answer to "+class.name+"'s request sent to "+target.subject, func() bool { return seen(target.subject) != nil })
	}

	chosen := privateIssuePrefix + "D" + class.name
	direct := "$JS.API.DIRECT.GET.KV_" + bus.InterestBucket + ".$KV." + bus.InterestBucket + ".chosen-" + class.name
	writes := permits(class.grant, "$KV."+bus.InterestBucket+".chosen-"+class.name) && permits(class.grant, direct)
	if writes {
		js, err := conn.JetStream()
		require.NoError(t, err)
		kv, err := js.KeyValue(bus.InterestBucket)
		require.NoError(t, err)
		_, err = kv.Put("chosen-"+class.name, []byte(`{"forged":"`+class.name+`"}`))
		require.NoError(t, err, "%s writes a key it holds", class.name)
		require.NoError(t, conn.PublishRequest(direct, chosen, nil))
		require.NoError(t, conn.Flush())
		testwait.Eventually(t, "the DIRECT.GET answer sent to "+chosen, func() bool { return seen(chosen) != nil })
		targets = append(targets, struct{ stream, subject string }{bus.Stream, chosen})
	}

	pushed := ""
	if stream, name, create := consumerCreate(class.grant, class.name); create != "" {
		pushed = "$KV." + sessionConsumersBucket + ".P" + class.name
		pushedStream := kvStream
		if stream != bus.Stream {
			pushed, pushedStream = privateIssuePrefix+"P"+class.name, bus.Stream
		}
		// A push consumer delivers only while its deliver subject has a literal subscriber in the
		// consumer's own account (measured on v2.15.1: a wildcard subscriber alone left a consumer
		// at 0 delivered), so the operator subscribes it, where JetStream sits; JetStream's own
		// delivery then reaches the subject, which the row reads as delivered.
		delivered, err := hub.operConn.SubscribeSync(pushed)
		require.NoError(t, err)
		require.NoError(t, hub.operConn.Flush())
		body, err := json.Marshal(map[string]any{"stream_name": stream, "config": map[string]any{
			"durable_name": name, "deliver_subject": pushed, "ack_policy": "none", "deliver_policy": "all",
		}})
		require.NoError(t, err)
		created, err := conn.Request(create, body, 5*time.Second)
		require.NoError(t, err, "%s's consumer create, which its grant permits, was not answered", class.name)
		require.NotContains(t, string(created.Data), `"error"`, "%s's consumer create: %s", class.name, created.Data)
		t.Cleanup(func() { _ = hub.operator.DeleteConsumer(stream, name) })
		_, err = delivered.NextMsg(testwait.Bounded(t, time.Minute, time.Second))
		require.NoError(t, err, "%s's push consumer delivered nothing to %s", class.name, pushed)
		require.NoError(t, delivered.Unsubscribe())
		targets = append(targets, struct{ stream, subject string }{pushedStream, pushed})
	}

	if want == 1 {
		for _, target := range targets {
			if target.subject == pushed {
				continue
			}
			testwait.Eventually(t, fmt.Sprintf("%s: the merge base's render stores the answer on %s", class.name, target.subject),
				func() bool { return hub.stored(t, target.stream, target.subject) == 1 })
		}
		if writes {
			require.Equal(t, `{"forged":"`+class.name+`"}`, string(hub.storedMsg(t, bus.Stream, chosen).Data),
				"%s's chosen bytes are not what the merge base's render stored", class.name)
		}
	}
	hub.fence(t)
	for _, target := range targets {
		expected := want
		if target.subject == pushed {
			// A push consumer's deliveries are JetStream's own publishes, which no stream stores, in
			// either layout (measured); the row holds that they stay so.
			expected = 0
		}
		require.Equal(t, expected, hub.stored(t, target.stream, target.subject),
			"%s: %s holds %d answers", class.name, target.subject, hub.stored(t, target.stream, target.subject))
	}
	require.Zero(t, hub.stored(t, kvStream, plain), "%s's refused publish stored", class.name)
}

// dropLines is text without each of lines, every one of which it must hold: a control's change of
// the rendered layout, which fails the test where the render no longer holds what it removes.
func dropLines(t *testing.T, text string, lines ...string) string {
	t.Helper()
	for _, line := range lines {
		line = "      " + line + "\n"
		require.Contains(t, text, line, "the render holds no line %q to drop", line)
		text = strings.Replace(text, line, "", 1)
	}
	return text
}

// withoutStreamExport drops the export of stream from JETSTREAM and its import into ENVOY.
func withoutStreamExport(stream string) func(t *testing.T, text string) string {
	return func(t *testing.T, text string) string {
		return dropLines(t, text, `{"stream": "`+stream+`"}`, `{"stream": {"account": "JETSTREAM", "subject": "`+stream+`"}}`)
	}
}

// singletonAPI exports $JS.API.> with nats-server's default response, a singleton.
func singletonAPI(t *testing.T, text string) string {
	t.Helper()
	const streamed = `{"response_type": "stream", "service": "$JS.API.>"}`
	require.Contains(t, text, streamed)
	return strings.Replace(text, streamed, `{"service": "$JS.API.>"}`, 1)
}

// clientGrants are the grants of the clients TestTheClientsStillWorkThroughTheAccountBoundary
// admits, each natsgrant's own: ListenerEnvoy and Measure with only their inbox entry moved under
// _INBOX_envoy, as plan E's t-e-74 moves it (natsgrant.InboxPrefix of the listener's identity, the
// measure grant's ReplyRoot + "._measure.>"), every other one as it is today.
func clientGrants(t *testing.T) map[string]natsgrant.Permissions {
	t.Helper()
	return map[string]natsgrant.Permissions{
		"Dispatch":      natsgrant.Dispatch("m1"),
		"Listener":      natsgrant.Listener("m1"),
		"ListenerEnvoy": movedInbox(t, natsgrant.Listener("m1"), "_INBOX_envoy.listener-m1"),
		"Daemon":        natsgrant.Daemon(),
		"Host":          natsgrant.Host("m1"),
		"Measure":       movedInbox(t, natsgrant.Measure(), "_INBOX_envoy._measure"),
	}
}

// clientHub is a private render's hub, its layout changed by edit when that is not nil, with the
// notification stream and the interest bucket made by the operator.
func clientHub(t *testing.T, edit func(t *testing.T, text string) string) (*accountHub, map[string]natsgrant.Permissions) {
	t.Helper()
	grants := clientGrants(t)
	hub := startAccountHub(t, true, edit, grants)
	_, err := hub.operator.AddStream(&natsgo.StreamConfig{Name: bus.Stream, Subjects: bus.StreamCarriedSubjects()})
	require.NoError(t, err)
	_, err = hub.operator.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.InterestBucket})
	require.NoError(t, err)
	return hub, grants
}

// watchSees reports whether token's KV watch of the interest bucket receives a put that token
// makes and reads back: the watch's create and the put, each a call through the $JS.API.> and
// $KV.> imports, must succeed; what is asked is whether the watch's delivery, published to the
// connection's inbox prefix inside JETSTREAM, reaches the client. It waits 5 s for it, where every
// row that receives it does in milliseconds.
func watchSees(t *testing.T, hub *accountHub, grants map[string]natsgrant.Permissions, token string) bool {
	t.Helper()
	conn, _ := hub.connect(t, token, grants[token])
	js, err := conn.JetStream()
	require.NoError(t, err)
	kv, err := js.KeyValue(bus.InterestBucket)
	require.NoError(t, err)
	watch, err := kv.WatchAll()
	require.NoError(t, err, "%s's watch create, through the $JS.API.> import", token)
	t.Cleanup(func() { _ = watch.Stop() })
	select {
	case entry := <-watch.Updates():
		require.Nil(t, entry, "the bucket was empty, so the watch's first update is the end of its initial values")
	case <-time.After(10 * time.Second):
		t.Fatalf("%s's watch never reported the end of its initial values", token)
	}
	_, err = kv.Put("s1", []byte(`{"topics":["notifications.agent.s1"]}`))
	require.NoError(t, err, "%s's put, through the $KV.> import", token)
	got, err := kv.Get("s1")
	require.NoError(t, err, "%s's get, through the $JS.API.> import", token)
	require.Equal(t, `{"topics":["notifications.agent.s1"]}`, string(got.Value()))
	select {
	case entry := <-watch.Updates():
		return entry != nil && entry.Key() == "s1"
	case <-time.After(5 * time.Second):
		return false
	}
}

// fetched is how many of five messages the daemon's pull consumer, legion-go-PUB-dispatch,
// fetches in one fetch of five, each acknowledged; Dispatch publishes the five first.
func fetched(t *testing.T, hub *accountHub, grants map[string]natsgrant.Permissions) int {
	t.Helper()
	dispatch, _ := hub.connect(t, "Dispatch", grants["Dispatch"])
	djs, err := dispatch.JetStream()
	require.NoError(t, err)
	for i := range 5 {
		_, err := djs.Publish(fmt.Sprintf("notifications.dispatch.issue.PUB.%d", i+1), []byte(fmt.Sprintf(`{"event":%d}`, i+1)))
		require.NoError(t, err)
	}
	daemon, _ := hub.connect(t, "Daemon", grants["Daemon"])
	js, err := daemon.JetStream()
	require.NoError(t, err)
	sub, err := js.PullSubscribe("notifications.dispatch.issue.PUB.>", "legion-go-PUB-dispatch", natsgo.BindStream(bus.Stream))
	require.NoError(t, err)
	msgs, err := sub.Fetch(5, natsgo.MaxWait(3*time.Second))
	if len(msgs) == 0 {
		require.NoError(t, err)
	}
	for _, m := range msgs {
		require.NoError(t, m.AckSync(), "the daemon's acknowledgement, through the $JS.ACK.> import")
	}
	return len(msgs)
}

// republished reports whether the stream's republish of Measure's publish reaches Measure, under
// the measure grant's reply root, after the publish on envoy-measure.> is stored in ENVOY_MEASURE,
// the stream Measure makes with its own grant. It waits 5 s for the republish, where the row that
// receives it does in milliseconds.
func republished(t *testing.T, hub *accountHub, grants map[string]natsgrant.Permissions) bool {
	t.Helper()
	conn, _ := hub.connect(t, "Measure", grants["Measure"])
	// The republish's own subject, which no request reply takes: Measure's replies come to
	// _INBOX_envoy._measure.<its inbox>, under the same root.
	got := make(chan *natsgo.Msg, 1)
	_, err := conn.Subscribe("_INBOX_envoy._measure.probe", func(m *natsgo.Msg) {
		select {
		case got <- m:
		default:
		}
	})
	require.NoError(t, err)
	js, err := conn.JetStream()
	require.NoError(t, err)
	_, err = js.AddStream(&natsgo.StreamConfig{Name: "ENVOY_MEASURE", Subjects: []string{"envoy-measure.>"},
		RePublish: &natsgo.RePublish{Source: "envoy-measure.>", Destination: "_INBOX_envoy._measure.>"}})
	require.NoError(t, err, "Measure makes its stream, through the $JS.API.> import")
	ack, err := js.Publish("envoy-measure.probe", []byte("measure"))
	require.NoError(t, err, "Measure's publish, through the envoy-measure.> import")
	require.Equal(t, "ENVOY_MEASURE", ack.Stream)
	require.Equal(t, uint64(1), hub.stored(t, "ENVOY_MEASURE", "envoy-measure.probe"))
	select {
	case m := <-got:
		require.Equal(t, "measure", string(m.Data))
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

// TestTheClientsStillWorkThroughTheAccountBoundary: on the private render every client keeps
// working through ENVOY's imports, each as its natsgrant set: Dispatch's publish gets its stream
// acknowledgement; a service's KV put, get and watch on t-e-74's _INBOX_envoy prefix, and, as a
// separate case, a client's on nats.go's default prefix; the listener's push durable on an
// _INBOX. deliver subject delivers five messages and takes their acknowledgements; the daemon's
// pull consumer fetches five and acknowledges them; Measure's publish on envoy-measure.> is stored
// in ENVOY_MEASURE and its republish reaches Measure under _INBOX_envoy.; the listener keeper's
// stream and bucket create and update go through the API (bus.EnsureKeyValue, the keeper's own
// call); and a core subscriber on a captured subject receives a plain publish, which the stream
// also stores.
//
// Controls, each on its own hub: $JS.API.> exported with the default singleton response, under
// which the fetch gets one of five; no _INBOX_envoy.> export, under which the service's watch on
// t-e-74's prefix gets no update and Measure's republish does not reach Measure; and no _INBOX.>
// export, under which the default-prefix client's watch gets no update.
func TestTheClientsStillWorkThroughTheAccountBoundary(t *testing.T) {
	t.Run("a publish gets its stream acknowledgement", func(t *testing.T) {
		hub, grants := clientHub(t, nil)
		conn, _ := hub.connect(t, "Dispatch", grants["Dispatch"])
		js, err := conn.JetStream()
		require.NoError(t, err)
		ack, err := js.Publish("notifications.dispatch.issue.PUB.1", []byte(`{"event":1}`))
		require.NoError(t, err)
		require.Equal(t, bus.Stream, ack.Stream)
		require.Equal(t, uint64(1), ack.Sequence)
	})

	t.Run("a service's KV put, get and watch on the _INBOX_envoy prefix", func(t *testing.T) {
		hub, grants := clientHub(t, nil)
		require.True(t, watchSees(t, hub, grants, "ListenerEnvoy"), "the watch on _INBOX_envoy got no update")
	})
	t.Run("control: no _INBOX_envoy.> export, and the service's watch gets no update", func(t *testing.T) {
		hub, grants := clientHub(t, withoutStreamExport("_INBOX_envoy.>"))
		require.False(t, watchSees(t, hub, grants, "ListenerEnvoy"), "the watch on _INBOX_envoy got its update with no export of its prefix")
	})
	t.Run("a default-prefix client's KV watch", func(t *testing.T) {
		hub, grants := clientHub(t, nil)
		require.True(t, watchSees(t, hub, grants, "Listener"), "the watch on _INBOX got no update")
	})
	t.Run("control: no _INBOX.> export, and the default-prefix client's watch gets no update", func(t *testing.T) {
		hub, grants := clientHub(t, withoutStreamExport("_INBOX.>"))
		require.False(t, watchSees(t, hub, grants, "Listener"), "the watch on _INBOX got its update with no export of its prefix")
	})

	t.Run("the listener's push durable delivers five and takes their acknowledgements", func(t *testing.T) {
		hub, grants := clientHub(t, nil)
		dispatch, _ := hub.connect(t, "Dispatch", grants["Dispatch"])
		djs, err := dispatch.JetStream()
		require.NoError(t, err)
		for i := range 5 {
			_, err := djs.Publish(fmt.Sprintf("notifications.dispatch.issue.PUB.%d", i+1), []byte("event"))
			require.NoError(t, err)
		}
		listener, _ := hub.connect(t, "Listener", grants["Listener"])
		js, err := listener.JetStream()
		require.NoError(t, err)
		deliver := listener.NewInbox()
		require.True(t, strings.HasPrefix(deliver, "_INBOX."), "%s", deliver)
		durable := bus.ListenerDurable("m1")
		acked := make(chan uint64, 5)
		_, err = listener.Subscribe(deliver, func(m *natsgo.Msg) {
			meta, err := m.Metadata()
			if err != nil {
				return
			}
			if m.AckSync() == nil {
				acked <- meta.Sequence.Stream
			}
		})
		require.NoError(t, err)
		require.NoError(t, listener.Flush())
		_, err = js.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{Durable: durable, DeliverSubject: deliver, AckPolicy: natsgo.AckExplicitPolicy})
		require.NoError(t, err, "the listener's durable create, through the $JS.API.> import")
		for range 5 {
			select {
			case <-acked:
			case <-time.After(10 * time.Second):
				t.Fatal("the listener's push durable delivered and took fewer than five acknowledgements")
			}
		}
		testwait.Eventually(t, "the durable's ack floor at the fifth message", func() bool {
			info, err := hub.operator.ConsumerInfo(bus.Stream, durable)
			return err == nil && info.AckFloor.Stream == 5 && info.NumAckPending == 0
		})
	})

	t.Run("the daemon's pull consumer fetches five and acknowledges them", func(t *testing.T) {
		hub, grants := clientHub(t, nil)
		require.Equal(t, 5, fetched(t, hub, grants))
		testwait.Eventually(t, "the pull consumer's ack floor at the fifth message", func() bool {
			info, err := hub.operator.ConsumerInfo(bus.Stream, "legion-go-PUB-dispatch")
			return err == nil && info.AckFloor.Stream == 5 && info.NumAckPending == 0
		})
	})
	t.Run("control: a singleton $JS.API.> export, and the fetch gets one of five", func(t *testing.T) {
		hub, grants := clientHub(t, singletonAPI)
		require.Equal(t, 1, fetched(t, hub, grants))
	})

	t.Run("Measure's publish is stored and its republish reaches Measure", func(t *testing.T) {
		hub, grants := clientHub(t, nil)
		require.True(t, republished(t, hub, grants), "Measure's republish did not reach Measure")
	})
	t.Run("control: no _INBOX_envoy.> export, and the republish does not reach Measure", func(t *testing.T) {
		hub, grants := clientHub(t, withoutStreamExport("_INBOX_envoy.>"))
		require.False(t, republished(t, hub, grants), "Measure's republish reached Measure with no export of its prefix")
	})

	t.Run("the listener keeper's stream and bucket create and update go through the API", func(t *testing.T) {
		grants := clientGrants(t)
		hub := startAccountHub(t, true, nil, grants)
		conn, _ := hub.connect(t, "Listener", grants["Listener"])
		js, err := conn.JetStream()
		require.NoError(t, err)
		_, err = js.AddStream(&natsgo.StreamConfig{Name: bus.Stream, Subjects: bus.StreamCarriedSubjects()})
		require.NoError(t, err, "the listener's stream create")
		info, err := js.StreamInfo(bus.Stream)
		require.NoError(t, err)
		config := info.Config
		config.Metadata = map[string]string{bus.StreamIDMetadataKey: "example-stream-id"}
		_, err = js.UpdateStream(&config)
		require.NoError(t, err, "the listener's stream update")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for range 2 {
			_, err := bus.EnsureKeyValue(ctx, conn, js, &natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Storage: natsgo.FileStorage})
			require.NoError(t, err, "the keeper's bucket ensure (a create, then the same bucket found)")
		}
		kept, err := hub.operator.StreamInfo(bus.Stream)
		require.NoError(t, err, "the stream the listener made is in JETSTREAM")
		require.Equal(t, "example-stream-id", kept.Config.Metadata[bus.StreamIDMetadataKey])
		_, err = hub.operator.StreamInfo("KV_" + bus.RoleClaimBucket)
		require.NoError(t, err, "the bucket the keeper made is in JETSTREAM")
	})

	t.Run("a core subscriber on a captured subject receives a plain publish, which the stream stores", func(t *testing.T) {
		hub, grants := clientHub(t, nil)
		host, _ := hub.connect(t, "Host", grants["Host"])
		got := make(chan *natsgo.Msg, 1)
		_, err := host.Subscribe("notifications.dispatch.issue.PUB.>", func(m *natsgo.Msg) { got <- m })
		require.NoError(t, err)
		require.NoError(t, host.Flush())
		dispatch, _ := hub.connect(t, "Dispatch", grants["Dispatch"])
		require.NoError(t, dispatch.Publish("notifications.dispatch.issue.PUB.core", []byte("core")))
		require.NoError(t, dispatch.Flush())
		select {
		case m := <-got:
			require.Equal(t, "core", string(m.Data))
		case <-time.After(10 * time.Second):
			t.Fatal("the core subscriber did not receive the plain publish")
		}
		testwait.Eventually(t, "the stream storing the plain publish", func() bool {
			return hub.stored(t, bus.Stream, "notifications.dispatch.issue.PUB.core") == 1
		})
	})
}
