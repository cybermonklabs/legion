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

	"github.com/sjawhar/envoy/internal/broker/natscallout"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/projectaccess"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// reachDaemonAccount is the service account of the Legion daemon the exchange list gives PUB.
const reachDaemonAccount = "system:serviceaccount:legion-pub:legion-daemon"

// reachWait bounds a message or a violation the server is to send; reachRefusalWait bounds a
// JetStream call the server refuses, which nats.go waits on until its context ends, since the
// server sends no answer to a publish it refused.
const (
	reachWait        = 10 * time.Second
	reachRefusalWait = 3 * time.Second
)

// TestEachConnectionReceivesOnlyItsTokensProjects is check c-callout-image of dispatch://INFRA-840
// and c-private-session-reach of dispatch://INFRA-3862: on testnats.HubImage, the nats-server image
// the hub deploys, behind the real auth service (natscallout.Start, deciding with its own Decide and
// scopeGrant), whose broker answers are the test's: Introspect as the broker answers two sessions'
// tokens, ServiceToken as it verifies the daemon's platform token, and Exchange and Access as the
// broker's exchange list and Dispatch answer, the exchange list giving the daemon's account PUB.
//
//   - A session's connection, presenting its token and a proof over the server's nonce, receives on
//     its own delivery channel and is refused the other session's by name. The control grants a
//     session every session's delivery channel in the auth service (natsgrant.Session); the
//     subscribe to the other's channel is served, and the test fails naming that session.
//   - The daemon, connecting as the Legion daemon does (user natscred.ServiceUser, its token, and
//     its account's reply prefix, natsgrant.InboxPrefix), subscribes PUB's role exceptions and
//     receives on them, creates each consumer it opens under PUB and pulls PUB's issue event from
//     its Dispatch consumer; it is refused PRIV's role exceptions, every reply inbox (_INBOX.>),
//     each consumer it would open under PRIV, and its own Dispatch consumer on PRIV's lane, each by
//     name, and receives nothing of PRIV. The control has the auth service answer the daemon's grant
//     whole, as an all-projects entry; the subscribe to PRIV's role exceptions is served, and the
//     test fails naming PRIV's subject.
//
// The check's listener rows (a listener called with a token listing PUB refuses PRIV's interest,
// role claim, session consumer's resume and send, and serves PUB's) are plan C's t-c-8
// (dispatch://INFRA-3884), which has no branch yet; they join this test when t-c-8 has one.
func TestEachConnectionReceivesOnlyItsTokensProjects(t *testing.T) {
	sessions := []*reachSession{newReachSession(t, "ses_pub", "PUB"), newReachSession(t, "ses_priv", "PRIV")}
	daemonToken := unsignedPlatformToken(reachDaemonAccount, time.Now().Add(time.Hour))

	keys := testnats.NewCalloutKeys(t)
	adminSeed, adminPublic := testnats.User(t)
	url := testnats.StartImageWithCallout(t, keys, testnats.StaticUser{NKey: adminPublic, Publish: []string{">"}, Subscribe: []string{">"}})
	startReachService(t, url, keys, "daemon/-/-="+reachDaemonAccount, natscallout.Deps{
		Introspect:   introspectSessions(sessions...),
		ServiceToken: verifyPlatformToken(daemonToken),
		// As the broker's exchange list answers it (exchange.Set.MappedForAccount): the person the
		// account's entry acts for, whom the auth service asks Dispatch about, and its projects.
		Exchange: func(account string) (string, []string, bool) {
			if account == reachDaemonAccount {
				return "ada@example.com", []string{"PUB"}, true
			}
			return "", nil, false
		},
		Access: dispatchAccess(map[string]projectaccess.Answer{
			"PUB":  {Project: "PUB", Visibility: "public", Visible: true, Repositories: []string{"acme/pub"}},
			"PRIV": {Project: "PRIV", Visibility: "private", Repositories: []string{"acme/priv"}},
		}),
		// Dispatch's whole repository mapping, which each pass reads beside the answers: PUB and
		// PRIV share no GitHub lane, so nothing of PUB's is withheld.
		RepoProjects: func(context.Context) ([]projectaccess.RepoProject, error) {
			return []projectaccess.RepoProject{{Repo: "acme/pub", Project: "PUB"}, {Repo: "acme/priv", Project: "PRIV"}}, nil
		},
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
		for subject, body := range map[string]string{
			contracts.DispatchIssueTopicPrefix + "PRIV.PRIV-1.comment.created": "PRIV-1",
			contracts.DispatchIssueTopicPrefix + "PUB.PUB-1.comment.created":   "PUB-1",
		} {
			if _, err := adminJS.Publish(ctx, subject, []byte(body)); err != nil {
				t.Fatalf("publish %s: %v", subject, err)
			}
		}

		daemon := connectReachDaemon(t, url, daemonToken)
		pubExceptions := bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + "PUB.>"
		privExceptions := bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + "PRIV.>"
		pubSub := daemon.subscribe(t, pubExceptions)
		privSub := daemon.subscribe(t, privExceptions)
		inboxSub := daemon.subscribe(t, natsgo.InboxPrefix+">")
		publishCore(t, admin, bus.ExceptionTopicPrefix+contracts.RoleTopicPrefix+"PRIV.controller", "PRIV's exception")
		publishCore(t, admin, natsgo.InboxPrefix+"priv-daemon.reply", "PRIV's reply")
		publishCore(t, admin, bus.ExceptionTopicPrefix+contracts.RoleTopicPrefix+"PUB.controller", "PUB's exception")
		daemon.requireReceived(t, pubSub, "PUB's exception", "the PUB daemon on PUB's role exceptions %s", pubExceptions)
		daemon.requireNothing(t, privSub, "the PUB daemon on PRIV's role exceptions %s", privExceptions)
		daemon.requireNothing(t, inboxSub, "the PUB daemon on every reply inbox %s", natsgo.InboxPrefix+">")
		daemon.requireRefused(t, "subscription", privExceptions)
		daemon.requireRefused(t, "subscription", natsgo.InboxPrefix+">")

		js, err := jetstream.New(daemon.conn)
		if err != nil {
			t.Fatal(err)
		}
		// Each consumer the daemon opens under PUB, created and read back through its info; then
		// PUB's issue event pulled from the first, its Dispatch consumer, and acknowledged.
		pubConsumers := daemonConsumers("PUB", "acme/pub")
		opened := make([]jetstream.Consumer, len(pubConsumers))
		for i, consumer := range pubConsumers {
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

		// Each consumer the daemon would open under PRIV, and its own Dispatch consumer on PRIV's
		// lane: the server refuses each create, so no PRIV message reaches the daemon through one.
		refused := append(daemonConsumers("PRIV", "acme/priv"),
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
			Active: true, Session: id, Person: "person@example.com", ClientID: brokertoken.ExchangeClientID,
			ProjectScope: brokertoken.ProjectScopePerson, Projects: []string{project},
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

// verifyPlatformToken is a Deps.ServiceToken that verifies token alone: the account it names, a
// Kubernetes one, issued now and lapsing in an hour.
func verifyPlatformToken(token string) func(context.Context, string) (string, bool, time.Time, time.Time, error) {
	issued := time.Now()
	return func(_ context.Context, raw string) (string, bool, time.Time, time.Time, error) {
		if raw != token {
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
	entries, err := natscallout.ParseServices(services)
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
		signed, err := brokerclient.ConnectionProof(s.key, s.token, string(nonce))
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

// daemonConsumers are the consumers a Legion daemon of project opens (natsgrant.WithinProjects
// holds a daemon's grant to these): its issue events first, then its repository mapping changes,
// its wake lane, the shared wake lane, and repository's GitHub events.
func daemonConsumers(project, repository string) []daemonConsumer {
	named := func(kind, filter string) daemonConsumer {
		return daemonConsumer{name: "legion-go-" + project + "-" + kind, filter: filter}
	}
	return []daemonConsumer{
		named("dispatch", contracts.DispatchIssueTopicPrefix+project+".>"),
		named("mapping", "notifications.dispatch.project."+project+".settings.repo_project.updated"),
		named("wake", contracts.LegionWakeSubject(strings.ToLower(project))),
		named("ready", bus.LegionWakeTopic),
		{name: natsgrant.GitHubConsumerName(project, repository), filter: natsgrant.GitHubFilter(repository)},
	}
}

func (c daemonConsumer) config() jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{Durable: c.name, FilterSubject: c.filter, AckPolicy: jetstream.AckExplicitPolicy}
}

// createSubject is the subject the consumer's create is sent on, which names its one filter.
func (c daemonConsumer) createSubject() string {
	return "$JS.API.CONSUMER.CREATE." + bus.Stream + "." + c.name + "." + c.filter
}
