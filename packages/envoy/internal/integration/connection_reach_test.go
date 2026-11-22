//go:build natslive

package integration

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// The accounts of the test's services: the Legion daemon the exchange list gives PUB, another
// daemon's, whose reply prefix the first must not reach, and an edge listener's, which the exchange
// list also gives PUB and the auth service grants nothing.
const (
	reachDaemonAccount      = "system:serviceaccount:legion-pub:legion-daemon"
	reachOtherDaemonAccount = "system:serviceaccount:legion-priv:legion-daemon"
	reachEdgeAccount        = "system:serviceaccount:example-ns:example-edge-listener"
)

// The GitHub events the daemon's GitHub consumer must deliver, by subject: one of acme/priv, a
// repository only PRIV's issues resolve to, and one of acme/none, which no issue names. A GitHub
// event is a message of no project.
var reachGitHubEvents = map[string]string{
	contracts.GithubTopicPrefix + "acme.priv.pull_request": "acme/priv's pull request",
	contracts.GithubTopicPrefix + "acme.none.pull_request": "acme/none's pull request",
}

// reachWait bounds a message or a violation the server is to send; reachRefusalWait bounds a
// JetStream call the server refuses, which nats.go waits on until its context ends, since the
// server sends no answer to a publish it refused.
const (
	reachWait        = 10 * time.Second
	reachRefusalWait = 3 * time.Second
)

// TestEachConnectionReceivesOnlyItsTokensProjects is the check that each connection the auth
// service admits reaches only its token's projects: on testnats.HubImage, the nats-server image
// the hub deploys, behind the real auth service (natscallout.Start, deciding with its own Decide and
// scopeGrant), whose broker answers are the test's: Introspect as the broker answers two sessions'
// tokens, ServiceToken as it verifies the daemon's and the edge listener's platform tokens, and
// Exchange and Access as the broker's exchange list and Dispatch answer, the exchange list giving
// the daemon's account and the edge listener's PUB.
//
//   - A session's connection, presenting its token and a proof over the server's nonce, receives on
//     its own delivery channel and is refused the other session's by name. The control grants a
//     session every session's delivery channel in the auth service (natsgrant.Session); the
//     subscribe to the other's channel is served, and the test fails naming that session.
//   - The daemon, connecting as the Legion daemon does (user natscred.ServiceUser, its token, and
//     its account's reply prefix, natsgrant.InboxPrefix), subscribes PUB's role exceptions and
//     receives on them, creates each consumer it opens under PUB, pulls PUB's issue event from its
//     Dispatch consumer, and pulls every repository's GitHub events from its one GitHub consumer
//     (reachGitHubEvents). It is refused, each by name: PRIV's role exceptions; the listener's and
//     Dispatch's reply inboxes (_INBOX.>), every service's reply root (natsgrant.ReplyRoot) and
//     another daemon's reply prefix; the stream's stored-message get and its info; each consumer it
//     would open under PRIV, and its own Dispatch consumer on PRIV's lane. It receives nothing of
//     PRIV. The control has the auth service answer the daemon's grant whole, as an all-projects
//     entry; the subscribe to PRIV's role exceptions is served, and the test fails naming PRIV's
//     subject.
//   - An edge listener's entry is answered no grant, so the hub refuses its connection.
//
// The session's GitHub row (a session whose token lists only PRIV receives, through an interest
// that names acme/pub, that repository's event and none of acme/priv's) is in cmd/listener's test
// of this name, since the relay it drives is the listener's. The listener's own rows (a listener
// in required mode with a privacy stand-in, and its all-public control; called with a token
// listing only PUB, it refuses PRIV's interest, role claim, session consumer's resume and send, and
// serves each of PUB's) join that test with the listener's project checks, which are not on main.
func TestEachConnectionReceivesOnlyItsTokensProjects(t *testing.T) {
	sessions := []*reachSession{newReachSession(t, "ses_pub", "PUB"), newReachSession(t, "ses_priv", "PRIV")}
	daemonToken := unsignedPlatformToken(reachDaemonAccount, time.Now().Add(time.Hour))

	keys := testnats.NewCalloutKeys(t)
	adminSeed, adminPublic := testnats.User(t)
	url := testnats.StartImageWithCallout(t, keys, testnats.StaticUser{NKey: adminPublic, Publish: []string{">"}, Subscribe: []string{">"}})
	edgeToken := unsignedPlatformToken(reachEdgeAccount, time.Now().Add(time.Hour))
	startReachService(t, url, keys, "daemon/-/-="+reachDaemonAccount+" edge-listener/edge-a/PUB="+reachEdgeAccount, natscallout.Deps{
		Introspect:   introspectSessions(sessions...),
		ServiceToken: verifyPlatformTokens(daemonToken, edgeToken),
		// As the broker's exchange list answers it (exchange.Set.MappedForAccount): the person the
		// account's entry acts for, whom the auth service asks Dispatch about, and its projects.
		Exchange: func(account string) (string, []string, bool) {
			if account == reachDaemonAccount || account == reachEdgeAccount {
				return "ada@example.com", []string{"PUB"}, true
			}
			return "", nil, false
		},
		Access: dispatchAccess(map[string]projectaccess.Answer{
			"PUB":  {Project: "PUB", Visibility: "public", Visible: true},
			"PRIV": {Project: "PRIV", Visibility: "private"},
		}),
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})

	admin, err := testnats.ConnectSeed(t, url, adminSeed)
	if err != nil {
		t.Fatalf("connect as the static admin user: %v", err)
	}
	t.Cleanup(admin.Close)

	t.Run("a session receives its own delivery channel and is refused another's", func(t *testing.T) {
		for i, own := range sessions {
			other := sessions[1-i]
			client := connectReachSession(t, url, own)
			ownSub := client.subscribe(t, bus.DeliverSubject(own.id))
			otherSub := client.subscribe(t, bus.DeliverSubject(other.id))
			// The other session's message goes first, so it is in the client's queue by the
			// time the own one arrives if the server served the subscribe.
			publishCore(t, admin, bus.DeliverSubject(other.id), "for "+other.id)
			publishCore(t, admin, bus.DeliverSubject(own.id), "for "+own.id)
			client.requireReceived(t, ownSub, "for "+own.id, "session %s on its own delivery channel", own.id)
			client.requireNothing(t, otherSub, "session %s on session %s's delivery channel %s", own.id, other.id, bus.DeliverSubject(other.id))
			client.requireRefused(t, "subscription", bus.DeliverSubject(other.id))
		}
	})

	t.Run("a daemon whose exchange entry lists PUB is refused PRIV's names and served PUB's", func(t *testing.T) {
		adminJS, err := jetstream.New(admin)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		if _, err := adminJS.CreateStream(ctx, jetstream.StreamConfig{Name: bus.Stream, Subjects: bus.StreamCarriedSubjects()}); err != nil {
			t.Fatalf("create %s: %v", bus.Stream, err)
		}
		privEvent := contracts.DispatchIssueTopicPrefix + "PRIV.PRIV-1.comment.created"
		stored := map[string]string{privEvent: "PRIV-1", contracts.DispatchIssueTopicPrefix + "PUB.PUB-1.comment.created": "PUB-1"}
		maps.Copy(stored, reachGitHubEvents)
		for subject, body := range stored {
			if _, err := adminJS.Publish(ctx, subject, []byte(body)); err != nil {
				t.Fatalf("publish %s: %v", subject, err)
			}
		}

		daemon := connectReachDaemon(t, url, daemonToken)
		pubExceptions := bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + "PUB.>"
		privExceptions := bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + "PRIV.>"
		pubSub := daemon.subscribe(t, pubExceptions)
		privSub := daemon.subscribe(t, privExceptions)
		// The listener's and Dispatch's replies arrive under _INBOX.>; every service's under the
		// reply root, each under its own account's prefix.
		otherPrefix := natsgrant.InboxPrefix(reachOtherDaemonAccount) + ".>"
		replyRoot := natsgrant.ReplyRoot + ".>"
		inboxSubs := map[string]*natsgo.Subscription{}
		for _, subject := range []string{natsgo.InboxPrefix + ">", replyRoot, otherPrefix} {
			inboxSubs[subject] = daemon.subscribe(t, subject)
		}
		publishCore(t, admin, bus.ExceptionTopicPrefix+contracts.RoleTopicPrefix+"PRIV.controller", "PRIV's exception")
		publishCore(t, admin, natsgo.InboxPrefix+"listener.reply", "a listener's reply")
		publishCore(t, admin, natsgrant.InboxPrefix(reachOtherDaemonAccount)+".reply", "the PRIV daemon's reply")
		publishCore(t, admin, bus.ExceptionTopicPrefix+contracts.RoleTopicPrefix+"PUB.controller", "PUB's exception")
		daemon.requireReceived(t, pubSub, "PUB's exception", "the PUB daemon on PUB's role exceptions %s", pubExceptions)
		daemon.requireNothing(t, privSub, "the PUB daemon on PRIV's role exceptions %s", privExceptions)
		daemon.requireRefused(t, "subscription", privExceptions)
		for subject, sub := range inboxSubs {
			daemon.requireNothing(t, sub, "the PUB daemon on the reply subjects %s", subject)
			daemon.requireRefused(t, "subscription", subject)
		}

		// The stream's stored-message get would read PRIV's issue event, and its info lists every
		// stored subject: the server refuses both, and nats.go waits out a refused request.
		get, err := json.Marshal(map[string]string{"last_by_subj": privEvent})
		if err != nil {
			t.Fatal(err)
		}
		info, err := json.Marshal(map[string]string{"subjects_filter": contracts.DispatchIssueTopicPrefix + "PRIV.>"})
		if err != nil {
			t.Fatal(err)
		}
		for subject, body := range map[string][]byte{streamMsgGet: get, streamInfo: info} {
			if msg, err := daemon.conn.Request(subject, body, reachRefusalWait); err == nil {
				t.Fatalf("the server answered the PUB daemon's %s: %s", subject, msg.Data)
			}
			daemon.requireRefused(t, "publish", subject)
		}

		js, err := jetstream.New(daemon.conn)
		if err != nil {
			t.Fatal(err)
		}
		// Each consumer the daemon opens under PUB but its GitHub one, created and read back
		// through its info; then PUB's issue event pulled from the first, its Dispatch consumer,
		// and acknowledged.
		pubConsumers := daemonConsumers("PUB")
		opened := make([]jetstream.Consumer, len(pubConsumers))
		for i, consumer := range pubConsumers[:githubConsumer] {
			if opened[i], err = js.CreateOrUpdateConsumer(ctx, bus.Stream, consumer.config()); err != nil {
				t.Fatalf("the PUB daemon could not create PUB's consumer %s on %s: %v", consumer.name, consumer.filter, err)
			}
			if _, err := opened[i].Info(ctx); err != nil {
				t.Fatalf("the PUB daemon could not read PUB's consumer %s: %v", consumer.name, err)
			}
		}
		batch, err := opened[0].Fetch(2, jetstream.FetchMaxWait(2*time.Second))
		if err != nil {
			t.Fatalf("the PUB daemon's pull of %s: %v", pubConsumers[0].name, err)
		}
		var pulled []string
		for msg := range batch.Messages() {
			pulled = append(pulled, string(msg.Data()))
			if err := msg.DoubleAck(ctx); err != nil {
				t.Fatalf("the PUB daemon's acknowledgement of %q: %v", msg.Data(), err)
			}
		}
		if err := batch.Error(); err != nil {
			t.Fatalf("the PUB daemon's pull of %s: %v", pubConsumers[0].name, err)
		}
		if !slices.Equal(pulled, []string{"PUB-1"}) {
			t.Fatalf("the PUB daemon pulled %q from %s, want only PUB-1", pulled, pubConsumers[0].name)
		}
		daemon.requireGitHubEvents(t, js, pubConsumers[githubConsumer])

		// Each consumer the daemon would open under PRIV, and its own Dispatch consumer on PRIV's
		// lane: the server refuses each create, so no PRIV message reaches the daemon through one.
		refused := append(daemonConsumers("PRIV"),
			daemonConsumer{name: pubConsumers[0].name, filter: contracts.DispatchIssueTopicPrefix + "PRIV.>"})
		created := make([]error, len(refused))
		var calls sync.WaitGroup
		for i, consumer := range refused {
			calls.Go(func() {
				call, cancel := context.WithTimeout(ctx, reachRefusalWait)
				defer cancel()
				_, created[i] = js.CreateOrUpdateConsumer(call, bus.Stream, consumer.config())
			})
		}
		calls.Wait()
		for i, consumer := range refused {
			if created[i] == nil {
				t.Fatalf("the PUB daemon created consumer %s on PRIV's %s", consumer.name, consumer.filter)
			}
			daemon.requireRefused(t, "publish", consumer.createSubject())
		}
	})

	t.Run("an edge listener's entry is granted nothing, and the hub refuses its connection", func(t *testing.T) {
		credential, _ := natscred.Service(fixedToken(edgeToken))
		conn, err := natsgo.Connect(url, credential, natsgo.Timeout(5*time.Second), natsgo.NoReconnect())
		if err == nil {
			conn.Close()
			t.Fatalf("the hub admitted the edge listener edge-listener/edge-a: the auth service granted its entry a grant")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "authorization violation") {
			t.Fatalf("the edge listener's connection failed for another reason than a refusal: %v", err)
		}
	})
}

// reachSession is one session's key, the broker access token bound to it, and the broker's answer
// about that token, which names the session's one project.
type reachSession struct {
	id     string
	key    *ecdsa.PrivateKey
	token  string
	answer oauthwire.Introspection
}

func newReachSession(t *testing.T, id, project string) *reachSession {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	sum, err := (&jose.JSONWebKey{Key: key.Public()}).Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return &reachSession{
		id: id, key: key, token: "access-token-of-" + id,
		answer: oauthwire.Introspection{
			Active: true, Session: id, Person: "person@example.com", ClientID: ciphertoken.ExchangeClientID,
			ProjectScope: ciphertoken.ProjectScopePerson, Projects: []string{project},
			Thumbprint: base64.RawURLEncoding.EncodeToString(sum), ExpiresAt: time.Now().Add(15 * time.Minute).Unix(),
		},
	}
}

// introspectSessions answers each session's token with its answer, and every other token as not
// live.
func introspectSessions(sessions ...*reachSession) func(context.Context, string) (oauthwire.Introspection, error) {
	return func(_ context.Context, token string) (oauthwire.Introspection, error) {
		for _, s := range sessions {
			if s.token == token {
				return s.answer, nil
			}
		}
		return oauthwire.Introspection{}, nil
	}
}

// unsignedPlatformToken is a platform identity token naming account, issued now and lapsing at
// expiry, unsigned: verifyPlatformToken stands in for its verification, and the daemon reads its
// account unverified (natscred.Identity), as the Legion daemon reads its own.
func unsignedPlatformToken(account string, expiry time.Time) string {
	encode := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(map[string]string{"alg": "none"}) + "." +
		encode(map[string]any{"sub": account, "iat": time.Now().Unix(), "exp": expiry.Unix()}) + ".unsigned"
}

// verifyPlatformTokens is a Deps.ServiceToken that verifies each of tokens alone: the account it
// names, a Kubernetes one, issued now and lapsing in an hour.
func verifyPlatformTokens(tokens ...string) func(context.Context, string) (string, bool, time.Time, time.Time, error) {
	issued := time.Now()
	return func(_ context.Context, raw string) (string, bool, time.Time, time.Time, error) {
		if !slices.Contains(tokens, raw) {
			return "", false, time.Time{}, time.Time{}, errors.New("the token did not verify")
		}
		account, err := natscred.Identity(raw)
		return account, false, issued, issued.Add(time.Hour), err
	}
}

// dispatchAccess is a Deps.Access answering as Dispatch does about each project of answers, for
// the person the exchange stub names (ada@example.com), and projectaccess.ErrProjectNotFound about
// any other project. Asked about any other person, it fails the ask, since the auth service asks
// about the person the account's exchange entry acts for.
func dispatchAccess(answers map[string]projectaccess.Answer) func(context.Context, string, string) (projectaccess.Answer, error) {
	return func(_ context.Context, project, person string) (projectaccess.Answer, error) {
		if person != "ada@example.com" {
			return projectaccess.Answer{}, fmt.Errorf("the auth service asked about person %q, not the account's entry's", person)
		}
		answer, ok := answers[project]
		if !ok {
			return projectaccess.Answer{}, fmt.Errorf("%w: %s", projectaccess.ErrProjectNotFound, project)
		}
		return answer, nil
	}
}

// startReachService runs the auth service against the server at url with keys' seeds, admitting
// the services BROKER_NATS_SERVICES would list as services, stopped when the test ends, and
// returns once its subscription has reached the server.
func startReachService(t *testing.T, url string, keys testnats.CalloutKeys, services string, deps natscallout.Deps) {
	t.Helper()
	entries, err := config.ParseNATSServices(services)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, seed string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(seed+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	service, err := natscallout.Start(t.Context(), natscallout.Settings{
		URLs:     []string{url},
		SeedFile: write("service.seed", keys.ServiceSeed), IssuerSeedFile: write("issuer.seed", keys.IssuerSeed),
		XKeySeedFile: write("xkey.seed", keys.XKeySeed), Services: entries,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := service.Ready(ctx); err != nil {
		t.Fatalf("the auth service's subscription never reached the server: %v", err)
	}
}

// reachClient is a connection the auth service admitted, with every permissions violation the
// server reported to it.
type reachClient struct {
	conn       *natsgo.Conn
	mu         sync.Mutex
	violations []string
}

// connectReach connects to url with options until the server admits the connection: the auth
// service refuses a service whose projects it holds no answer about yet, and asks Dispatch about
// them only once it has started.
func connectReach(t *testing.T, url, who string, options ...natsgo.Option) *reachClient {
	t.Helper()
	client := &reachClient{}
	options = append(options, natsgo.Timeout(5*time.Second), natsgo.NoReconnect(),
		natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) {
			client.mu.Lock()
			client.violations = append(client.violations, err.Error())
			client.mu.Unlock()
		}))
	var last error
	testwait.Eventually(t, "the hub to admit "+who, func() bool {
		client.conn, last = natsgo.Connect(url, options...)
		if last != nil {
			t.Logf("%s not admitted yet: %v", who, last)
		}
		return last == nil
	})
	t.Cleanup(client.conn.Close)
	return client
}

// connectReachSession connects as s does: its access token in the CONNECT's token field and, as
// its signature, the proof of its key over the nonce the server sends this connection.
func connectReachSession(t *testing.T, url string, s *reachSession) *reachClient {
	t.Helper()
	sign := func(nonce []byte) ([]byte, error) {
		signed, err := cipherclient.ConnectionProof(s.key, s.token, string(nonce))
		return []byte(signed), err
	}
	return connectReach(t, url, "session "+s.id, natsgo.UserJWT(func() (string, error) { return "-", nil }, sign), natsgo.Token(s.token))
}

// connectReachDaemon connects as the Legion daemon does (natsauth.ConnectService): user
// natscred.ServiceUser with token as its password (natscred.Service), and the reply prefix of the
// account token names.
func connectReachDaemon(t *testing.T, url, token string) *reachClient {
	t.Helper()
	identity, err := natscred.Identity(token)
	if err != nil {
		t.Fatal(err)
	}
	credential, _ := natscred.Service(fixedToken(token))
	return connectReach(t, url, "the daemon "+identity, credential, natsgo.CustomInboxPrefix(natsgrant.InboxPrefix(identity)))
}

// fixedToken is a natscred.TokenReader that always reads the one token.
type fixedToken string

func (f fixedToken) Read() (string, error) { return string(f), nil }

// subscribe subscribes subject and waits for the server to have read the subscribe.
func (c *reachClient) subscribe(t *testing.T, subject string) *natsgo.Subscription {
	t.Helper()
	sub, err := c.conn.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("subscribe %s: %v", subject, err)
	}
	if err := c.conn.Flush(); err != nil {
		t.Fatalf("flush the subscribe of %s: %v", subject, err)
	}
	return sub
}

// requireReceived requires sub to receive want next.
func (c *reachClient) requireReceived(t *testing.T, sub *natsgo.Subscription, want, where string, args ...any) {
	t.Helper()
	msg, err := sub.NextMsg(reachWait)
	if err != nil {
		t.Fatalf("nothing arrived for %s: %v; the server reported %v", fmt.Sprintf(where, args...), err, c.reported())
	}
	if string(msg.Data) != want {
		t.Fatalf("%s received %q, want %q", fmt.Sprintf(where, args...), msg.Data, want)
	}
}

// requireNothing requires sub to hold no message: called once a later message on the same
// connection has arrived, by which time one served to sub would have too.
func (c *reachClient) requireNothing(t *testing.T, sub *natsgo.Subscription, where string, args ...any) {
	t.Helper()
	if msg, err := sub.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("the server served %s: it received %q", fmt.Sprintf(where, args...), msg.Data)
	}
}

// requireRefused waits for the server to report a permissions violation of kind (publish or
// subscription) naming subject.
func (c *reachClient) requireRefused(t *testing.T, kind, subject string) {
	t.Helper()
	want := strings.ToLower(`permissions violation for ` + kind + ` to "` + subject + `"`)
	if !testwait.Holds(reachWait, testwait.Poll, func() bool {
		return slices.ContainsFunc(c.reported(), func(violation string) bool { return strings.Contains(strings.ToLower(violation), want) })
	}) {
		t.Fatalf("the server reported no %s violation naming %s; it reported %v", kind, subject, c.reported())
	}
}

func (c *reachClient) reported() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.violations)
}

// publishCore publishes body on subject over core NATS and waits for the server to have read it.
func publishCore(t *testing.T, conn *natsgo.Conn, subject, body string) {
	t.Helper()
	if err := conn.Publish(subject, []byte(body)); err != nil {
		t.Fatalf("publish %s: %v", subject, err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the publish of %s: %v", subject, err)
	}
}

// daemonConsumer is one durable consumer a Legion daemon opens on the notification stream, with
// the one filter its create names.
type daemonConsumer struct{ name, filter string }

// githubConsumer is where daemonConsumers puts a project's GitHub consumer.
const githubConsumer = 3

// The notification stream's two reads no consumer names: its stored-message get, which reads any
// stored message, and its info, which lists every stored subject with its count.
const (
	streamMsgGet = "$JS.API.STREAM.MSG.GET." + bus.Stream
	streamInfo   = "$JS.API.STREAM.INFO." + bus.Stream
)

// daemonConsumers are the consumers a Legion daemon of project opens (natsgrant.WithinProjects
// holds a daemon's grant to these): its issue events first, then its wake lane, the shared wake
// lane, and, at githubConsumer, every repository's GitHub events, a message of no project.
func daemonConsumers(project string) []daemonConsumer {
	named := func(kind, filter string) daemonConsumer {
		return daemonConsumer{name: "legion-go-" + project + "-" + kind, filter: filter}
	}
	return []daemonConsumer{
		named("dispatch", contracts.DispatchIssueTopicPrefix+project+".>"),
		named("wake", contracts.LegionWakeSubject(strings.ToLower(project))),
		named("ready", bus.LegionWakeTopic),
		named("github", contracts.GithubTopicPrefix+">"),
	}
}

// requireGitHubEvents creates consumer, the daemon's GitHub consumer, reads its info, pulls the
// stream's GitHub events (reachGitHubEvents) through it and acknowledges each, and fails naming
// each GitHub subject the daemon did not receive. Every call is bounded by reachWait, so a server
// that refuses the create or the pull, and so sends no answer, fails the test in seconds.
func (c *reachClient) requireGitHubEvents(t *testing.T, js jetstream.JetStream, consumer daemonConsumer) {
	t.Helper()
	received, why := c.pullGitHubEvents(t.Context(), js, consumer)
	var missing []string
	for subject, body := range reachGitHubEvents {
		if !slices.Contains(received, body) {
			missing = append(missing, subject)
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		t.Fatalf("the PUB daemon did not receive the GitHub event on %s through %s (%s); it received %q, and the server reported %v",
			strings.Join(missing, " and "), consumer.name, why, received, c.reported())
	}
}

// pullGitHubEvents is the bodies consumer delivers, each acknowledged, and why it stopped when it
// delivered fewer than reachGitHubEvents holds.
func (c *reachClient) pullGitHubEvents(ctx context.Context, js jetstream.JetStream, consumer daemonConsumer) ([]string, string) {
	call, cancel := context.WithTimeout(ctx, reachWait)
	defer cancel()
	opened, err := js.CreateOrUpdateConsumer(call, bus.Stream, consumer.config())
	if err != nil {
		return nil, "its create on " + consumer.createSubject() + " failed: " + err.Error()
	}
	if _, err := opened.Info(call); err != nil {
		return nil, "its info failed: " + err.Error()
	}
	batch, err := opened.Fetch(len(reachGitHubEvents), jetstream.FetchMaxWait(reachWait))
	if err != nil {
		return nil, "its pull failed: " + err.Error()
	}
	var received []string
	for msg := range batch.Messages() {
		received = append(received, string(msg.Data()))
		if err := msg.DoubleAck(call); err != nil {
			return received, fmt.Sprintf("its acknowledgement of %q failed: %v", msg.Data(), err)
		}
	}
	if err := batch.Error(); err != nil {
		return received, "its pull failed: " + err.Error()
	}
	return received, "its pull delivered only these"
}

func (c daemonConsumer) config() jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{Durable: c.name, FilterSubject: c.filter, AckPolicy: jetstream.AckExplicitPolicy}
}

// createSubject is the subject the consumer's create is sent on, which names its one filter.
func (c daemonConsumer) createSubject() string {
	return "$JS.API.CONSUMER.CREATE." + bus.Stream + "." + c.name + "." + c.filter
}
