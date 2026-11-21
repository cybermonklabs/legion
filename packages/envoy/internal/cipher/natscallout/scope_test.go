package natscallout

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// The accounts of two daemons held to their exchange projects: PUB's, and one whose exchange
// entry names a project Dispatch is slow to answer about.
const (
	pubDaemonAccount  = "system:serviceaccount:legion-pub:legion-daemon"
	slowDaemonAccount = "system:serviceaccount:legion-slow:legion-daemon"
)

// entryPerson is the person every exchange entry in these tests acts for.
const entryPerson = "ada@example.com"

// exchangeOf is a Deps.Exchange that gives each account entryPerson and its projects, and no
// entry to any other.
func exchangeOf(projects map[string][]string) func(string) (string, []string, bool) {
	return func(account string) (string, []string, bool) {
		listed, ok := projects[account]
		if !ok {
			return "", nil, false
		}
		return entryPerson, listed, true
	}
}

// dispatchAnswers is a Deps.Access standing in for Dispatch: each project's answer, or
// projectaccess.ErrProjectNotFound for one it does not hold, counting the asks about each. As
// Dispatch answers, a project is visible to no person when it is private, and to entryPerson
// unless unseen names it.
type dispatchAnswers struct {
	mu      sync.Mutex
	answers map[string][]string // project → repositories
	private map[string]bool
	unseen  map[string]bool // private projects entryPerson cannot see
	failing error
	// failingFor fails the asks about one project alone, beside the others' answers.
	failingFor map[string]error
	asked      map[string]int
	// blockAfter is how many asks about a project answer at once; each later ask blocks for
	// block, or until its context ends.
	blockAfter int
	block      time.Duration
}

func (d *dispatchAnswers) access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	d.mu.Lock()
	if d.asked == nil {
		d.asked = map[string]int{}
	}
	d.asked[project]++
	asks, repositories, failing := d.asked[project], d.answers[project], cmp.Or(d.failing, d.failingFor[project])
	_, held := d.answers[project]
	private, unseen := d.private[project], d.unseen[project]
	blocks := d.block > 0 && asks > d.blockAfter
	d.mu.Unlock()
	if blocks {
		select {
		case <-ctx.Done():
			return projectaccess.Answer{}, ctx.Err()
		case <-time.After(d.block):
		}
	}
	switch {
	case failing != nil:
		return projectaccess.Answer{}, failing
	case !held:
		return projectaccess.Answer{}, fmt.Errorf("%w: %s", projectaccess.ErrProjectNotFound, project)
	}
	answer := projectaccess.Answer{Project: project, Visibility: "public", Visible: true, Repositories: slices.Clone(repositories)}
	if private {
		answer.Visibility = "private"
		answer.Visible = person == entryPerson && !unseen
	}
	return answer, nil
}

func (d *dispatchAnswers) set(project string, repositories ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.answers[project] = repositories
}

func (d *dispatchAnswers) fail(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failing = err
}

// drop takes project out of Dispatch, which then answers ErrProjectNotFound about it.
func (d *dispatchAnswers) drop(project string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.answers, project)
}

// hide takes entryPerson off project's access list, or with hidden false puts them back on it.
func (d *dispatchAnswers) hide(project string, hidden bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.unseen == nil {
		d.unseen = map[string]bool{}
	}
	d.unseen[project] = hidden
}

// heldDeps are Deps whose ServiceToken verifies tokens, whose exchange list is exchange, and
// whose Dispatch is dispatch, with one pass over every project services can name made, as Start's
// first pass is.
func heldDeps(t *testing.T, tokens map[string]platformToken, exchange map[string][]string, dispatch *dispatchAnswers, services []ServiceEntry) Deps {
	t.Helper()
	deps := Deps{ServiceToken: serviceTokens(tokens), Exchange: exchangeOf(exchange), Access: dispatch.access}
	deps.held = newHeldAccess(deps, services)
	require.NotNil(t, deps.held, "no service entry names a project to ask Dispatch about")
	deps.held.refresh(t.Context())
	return deps
}

// withAccessRefresh runs the rest of the test with the service asking Dispatch again every
// refresh.
func withAccessRefresh(t *testing.T, refresh time.Duration) {
	t.Helper()
	was := accessRefresh
	accessRefresh = refresh
	t.Cleanup(func() { accessRefresh = was })
}

// requireRefused requires err to be a refusal under label whose reason begins reason.
func requireRefused(t *testing.T, err error, label, reason string) {
	t.Helper()
	var refused *Refusal
	require.True(t, errors.As(err, &refused), "the connection was not refused: %v", err)
	require.Equal(t, label, refused.Label)
	require.True(t, strings.HasPrefix(refused.Reason, reason), "%q does not begin %q", refused.Reason, reason)
}

// A daemon whose entry takes its exchange projects ("-") is answered with the daemon's grant held
// to the projects the broker's exchange list gives its account: PUB's subjects, every
// repository's GitHub events (a message of no project) and a reply prefix of its own, and nothing
// of PRIV, which its account's entry does not list. Through the hub, its subscribe to PUB's role
// exceptions is served and PRIV's is refused by name, as is a subscribe to every reply inbox.
func TestADaemonEntryIsHeldToItsExchangeProjects(t *testing.T) {
	now := time.Now()
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}}
	tokens := map[string]platformToken{"pub-daemon-token": {account: pubDaemonAccount, issued: now, expiry: now.Add(time.Hour)}}
	exchange := map[string][]string{pubDaemonAccount: {"PUB"}}
	dispatch := &dispatchAnswers{answers: map[string][]string{"PUB": {"acme/pub"}, "PRIV": {"acme/priv"}}}

	decision, err := Decide(t.Context(), heldDeps(t, tokens, exchange, dispatch, services), services, serviceRequest("pub-daemon-token"))
	require.NoError(t, err)
	want, err := natsgrant.WithinProjects(natsgrant.Daemon(), natsgrant.Scope{Projects: []string{"PUB"}, Identity: pubDaemonAccount})
	require.NoError(t, err)
	require.Equal(t, want, decision.Grant)
	require.Equal(t, "daemon/"+pubDaemonAccount, decision.Name)
	pubExceptions := bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + "PUB.>"
	require.Contains(t, decision.Grant.Subscribe, pubExceptions)
	require.Contains(t, decision.Grant.Subscribe, natsgrant.InboxPrefix(pubDaemonAccount)+".>")
	require.NotContains(t, decision.Grant.Subscribe, nats.InboxPrefix+">")
	for _, entry := range slices.Concat(decision.Grant.Publish, decision.Grant.Subscribe) {
		require.NotContains(t, strings.ToLower(entry), "priv", "the PUB daemon's grant names PRIV: %s", entry)
	}

	// The same, through the hub, with the asks to Dispatch the service makes itself, and a static
	// publisher of every notification topic, which proves the served subscribe by a delivery.
	keys := testnats.NewCalloutKeys(t)
	static, staticSeed := publisher(t)
	url := testnats.StartWithCallout(t, keys, static)
	startServiceOf(t, url, keys, services, Deps{ServiceToken: serviceTokens(tokens), Exchange: exchangeOf(exchange), Access: dispatch.access})
	errs := make(chan error, 16)
	var nc *nats.Conn
	testwait.Eventually(t, "the hub to admit the PUB daemon", func() bool {
		var err error
		nc, err = nats.Connect(url, nats.UserInfo(natscred.ServiceUser, "pub-daemon-token"), nats.NoReconnect(),
			nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
				select {
				case errs <- err:
				default:
				}
			}))
		return err == nil
	})
	t.Cleanup(nc.Close)
	served, err := nc.SubscribeSync(pubExceptions)
	require.NoError(t, err)
	_, err = nc.SubscribeSync(bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + "PRIV.>")
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	awaitViolation(t, errs, bus.ExceptionTopicPrefix+contracts.RoleTopicPrefix+"PRIV.>")
	_, err = nc.SubscribeSync(nats.InboxPrefix + ">")
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	awaitViolation(t, errs, nats.InboxPrefix+">")

	pub, err := testnats.ConnectSeed(t, url, staticSeed)
	require.NoError(t, err)
	defer pub.Close()
	pubException := bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + "PUB.implementer"
	require.NoError(t, pub.Publish(pubException, []byte("PUB's exception")))
	require.NoError(t, pub.Flush())
	msg, err := served.NextMsg(5 * time.Second)
	require.NoError(t, err, "the PUB daemon received nothing on PUB's role exceptions: its own subscribe was refused or held")
	require.Equal(t, "PUB's exception", string(msg.Data))
}

// No admission waits on Dispatch: the service makes a pass at its start and every 20 s after,
// and a decision reads only the pass it holds. Dispatch here answers its first ask about PUB at
// once and blocks three seconds on every later one. With a second pass blocked, PUB's daemon is
// admitted in under 100 ms from the first pass. The control reads Access inside the decision: the
// admission waits the three seconds, and this test fails naming how long it took.
func TestAnAdmissionNeverWaitsOnDispatch(t *testing.T) {
	now := time.Now()
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}}
	tokens := map[string]platformToken{"pub-daemon-token": {account: pubDaemonAccount, issued: now, expiry: now.Add(time.Hour)}}
	dispatch := &dispatchAnswers{answers: map[string][]string{"PUB": {"acme/pub"}}, blockAfter: 1, block: 3 * time.Second}
	deps := Deps{
		ServiceToken: serviceTokens(tokens), Exchange: exchangeOf(map[string][]string{pubDaemonAccount: {"PUB"}}),
		Access: dispatch.access,
	}
	deps.held = newHeldAccess(deps, services)
	require.NotNil(t, deps.held)
	asks, stop := context.WithCancel(t.Context())
	t.Cleanup(stop)
	go deps.held.refresh(asks)
	testwait.Eventually(t, "the first pass held", func() bool { return deps.held.latest() != nil })
	go deps.held.refresh(asks) // its ask about PUB blocks three seconds
	testwait.Eventually(t, "a second ask about PUB in flight", func() bool {
		dispatch.mu.Lock()
		defer dispatch.mu.Unlock()
		return dispatch.asked["PUB"] >= 2
	})

	began := time.Now()
	decision, err := Decide(t.Context(), deps, services, serviceRequest("pub-daemon-token"))
	took := time.Since(began)
	require.NoError(t, err)
	require.Less(t, took, 100*time.Millisecond, "the PUB daemon's admission took %s: it waited on Dispatch", took)
	require.Contains(t, decision.Grant.Subscribe, bus.ExceptionTopicPrefix+contracts.RoleTopicPrefix+"PUB.>")
}

// A complete pass once held is kept while Dispatch cannot be reached, and the next complete pass
// replaces it: here PUB's daemon is admitted through an outage on the pass from before it, and,
// once Dispatch answers that the entry's person no longer sees PUB, refused naming PUB.
func TestAHeldAnswerFollowsDispatchAndOutlivesItsOutage(t *testing.T) {
	now := time.Now()
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}}
	tokens := map[string]platformToken{"pub-daemon-token": {account: pubDaemonAccount, issued: now, expiry: now.Add(time.Hour)}}
	dispatch := &dispatchAnswers{answers: map[string][]string{"PUB": {"acme/pub"}}, private: map[string]bool{"PUB": true}}
	deps := heldDeps(t, tokens, map[string][]string{pubDaemonAccount: {"PUB"}}, dispatch, services)

	dispatch.fail(errors.New("dial tcp: connection refused"))
	deps.held.refresh(t.Context())
	_, err := Decide(t.Context(), deps, services, serviceRequest("pub-daemon-token"))
	require.NoError(t, err, "an outage of Dispatch dropped the answer held about PUB")

	dispatch.fail(nil)
	dispatch.hide("PUB", true)
	deps.held.refresh(t.Context())
	_, err = Decide(t.Context(), deps, services, serviceRequest("pub-daemon-token"))
	requireRefused(t, err, labelProjectNotVisible, "project not visible to the account's person: PUB")
}

// Dispatch refusing the broker's own account token (a 401 or 403, projectaccess.ErrBrokerRefused)
// is no outage to admit through: the token exchange refuses every trade that asks Dispatch then
// (oauth's refusedByDispatch), so a pass holding that refusal is complete, the auth service
// refuses a connection held to what it refused, under its own label and naming why, and logs it
// at error once a pass. Here Dispatch answers that PUB's person sees OPS and PUB, then refuses
// the broker's token while that person is off both access lists: PUB's daemon is refused, and one
// error line names the refusal for the pass's two questions. The next pass replaces the refusal:
// once Dispatch answers again, with the person back, the daemon is admitted. The control keeps the
// last pass on a refusal, as on an outage: the daemon is admitted on the pass from before.
func TestDispatchRefusingTheBrokersTokenRefusesAHeldConnection(t *testing.T) {
	now := time.Now()
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}}
	tokens := map[string]platformToken{"pub-daemon-token": {account: pubDaemonAccount, issued: now, expiry: now.Add(time.Hour)}}
	dispatch := &dispatchAnswers{
		answers: map[string][]string{"OPS": {"acme/ops"}, "PUB": {"acme/pub"}},
		private: map[string]bool{"OPS": true, "PUB": true},
	}
	var logged bytes.Buffer
	deps := Deps{
		ServiceToken: serviceTokens(tokens), Exchange: exchangeOf(map[string][]string{pubDaemonAccount: {"OPS", "PUB"}}),
		Access: dispatch.access,
		Log:    slog.New(slog.NewTextHandler(&logged, nil)),
	}
	deps.held = newHeldAccess(deps, services)
	require.NotNil(t, deps.held)
	deps.held.refresh(t.Context())
	_, err := Decide(t.Context(), deps, services, serviceRequest("pub-daemon-token"))
	require.NoError(t, err, "PUB's daemon was refused while its person sees both of its projects")

	dispatch.hide("OPS", true)
	dispatch.hide("PUB", true)
	dispatch.fail(fmt.Errorf("%w: dispatch answered /api/v1/access/projects/PUB 403 with the code %q", projectaccess.ErrBrokerRefused, "ACCESS_ACCOUNT_REQUIRED"))
	deps.held.refresh(t.Context())
	_, err = Decide(t.Context(), deps, services, serviceRequest("pub-daemon-token"))
	requireRefused(t, err, labelAccessRefused, "Dispatch refuses the broker's own account token")
	require.Equal(t, 1, strings.Count(logged.String(), "level=ERROR"), "want one error line for the pass's two refused questions:\n%s", logged.String())
	require.Contains(t, logged.String(), "ACCESS_ACCOUNT_REQUIRED")
	require.NotContains(t, logged.String(), "level=WARN", "a refusal of the broker's token was logged as an outage:\n%s", logged.String())

	dispatch.fail(nil)
	dispatch.hide("OPS", false)
	dispatch.hide("PUB", false)
	deps.held.refresh(t.Context())
	_, err = Decide(t.Context(), deps, services, serviceRequest("pub-daemon-token"))
	require.NoError(t, err, "Dispatch's answer after its refusal did not replace it")
}

// The service asks Dispatch again every accessRefresh on its own (heldAccess.run, which Start
// runs), the one thing that bounds how old a held answer is, so an answer changed at Dispatch
// reaches a decision with no ask the test makes. With the re-ask every 100 ms, PUB's daemon is
// admitted through the hub on Start's first pass; then PUB's person is taken off
// its access list at Dispatch, and within seconds the daemon's connection is refused, naming PUB as
// one its person cannot see. The control asks once at Start and never again: the daemon is
// admitted on that first answer for the whole wait. The refusal's reason is read from the line the
// replica writes as it closes, which holds each refusal label's first detail (reportWhileRunning).
func TestTheServiceAsksDispatchAgainOnItsOwn(t *testing.T) {
	withAccessRefresh(t, 100*time.Millisecond)
	now := time.Now()
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}}
	tokens := map[string]platformToken{"pub-daemon-token": {account: pubDaemonAccount, issued: now, expiry: now.Add(time.Hour)}}
	dispatch := &dispatchAnswers{answers: map[string][]string{"PUB": {"acme/pub"}}, private: map[string]bool{"PUB": true}}
	log := &logtest.Buffer{}
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	service := startServiceOf(t, url, keys, services, Deps{
		ServiceToken: serviceTokens(tokens), Exchange: exchangeOf(map[string][]string{pubDaemonAccount: {"PUB"}}),
		Access: dispatch.access,
		Log:    slog.New(slog.NewTextHandler(log, nil)),
	})
	connect := func() error {
		nc, err := nats.Connect(url, nats.UserInfo(natscred.ServiceUser, "pub-daemon-token"), nats.NoReconnect())
		if err == nil {
			nc.Close()
		}
		return err
	}
	testwait.Eventually(t, "the hub to admit PUB's daemon on the first answer", func() bool { return connect() == nil })

	dispatch.hide("PUB", true)
	testwait.WithinPolling(t, 5*time.Second, 50*time.Millisecond, "the service to ask Dispatch again and refuse PUB's daemon", func() bool {
		return errors.Is(connect(), nats.ErrAuthorization)
	})
	service.Close()
	require.Contains(t, log.String(), "project not visible to the account's person: PUB", "the refusal was not the changed answer's:\n%s", log.String())
}

// A service held to its exchange projects whose account no exchange entry lists is refused,
// naming the account: there is no list of projects to hold it to, and holding it to none is not
// what its entry says.
func TestAnAccountWithNoExchangeEntryIsRefused(t *testing.T) {
	now := time.Now()
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}, {Kind: KindDaemon, Account: slowDaemonAccount}}
	tokens := map[string]platformToken{"slow-daemon-token": {account: slowDaemonAccount, issued: now, expiry: now.Add(time.Hour)}}
	dispatch := &dispatchAnswers{answers: map[string][]string{"PUB": {"acme/pub"}}}
	deps := heldDeps(t, tokens, map[string][]string{pubDaemonAccount: {"PUB"}}, dispatch, services)

	_, err := Decide(t.Context(), deps, services, serviceRequest("slow-daemon-token"))
	requireRefused(t, err, labelNoExchangeEntry, "account has no exchange entry: "+slowDaemonAccount)
}

// A project the exchange list gives an account and Dispatch holds no answer for, because it holds
// no such project, refuses the connection naming the project.
func TestAnUnknownProjectRefusesTheConnection(t *testing.T) {
	now := time.Now()
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}}
	tokens := map[string]platformToken{"pub-daemon-token": {account: pubDaemonAccount, issued: now, expiry: now.Add(time.Hour)}}
	dispatch := &dispatchAnswers{answers: map[string][]string{"PUB": {"acme/pub"}}}
	deps := heldDeps(t, tokens, map[string][]string{pubDaemonAccount: {"PUB", "GONE"}}, dispatch, services)

	_, err := Decide(t.Context(), deps, services, serviceRequest("pub-daemon-token"))
	requireRefused(t, err, labelProjectNotFound, "project not found: GONE")
}

// The connection is held to what the token exchange holds the account's token to: the person its
// exchange entry acts for must see each project (projectaccess.Visible). A private project that
// person sees is served, which an ask naming no person would not have found visible; one that
// person cannot see refuses the connection, naming the project, as the exchange refuses the
// token. The control drops the person's check: the PRIV2 row is served.
func TestAProjectItsPersonCannotSeeRefusesTheConnection(t *testing.T) {
	now := time.Now()
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}, {Kind: KindDaemon, Account: slowDaemonAccount}}
	tokens := map[string]platformToken{
		"seen-token":   {account: pubDaemonAccount, issued: now, expiry: now.Add(time.Hour)},
		"unseen-token": {account: slowDaemonAccount, issued: now, expiry: now.Add(time.Hour)},
	}
	dispatch := &dispatchAnswers{
		answers: map[string][]string{"PRIV": {"acme/priv"}, "PRIV2": {"acme/priv2"}},
		private: map[string]bool{"PRIV": true, "PRIV2": true},
		unseen:  map[string]bool{"PRIV2": true},
	}
	deps := heldDeps(t, tokens, map[string][]string{pubDaemonAccount: {"PRIV"}, slowDaemonAccount: {"PRIV2"}}, dispatch, services)

	decision, err := Decide(t.Context(), deps, services, serviceRequest("seen-token"))
	require.NoError(t, err, "a private project the entry's person sees was refused: the auth service asked about no person")
	want, err := natsgrant.WithinProjects(natsgrant.Daemon(), natsgrant.Scope{Projects: []string{"PRIV"}, Identity: pubDaemonAccount})
	require.NoError(t, err)
	require.Equal(t, want, decision.Grant)

	_, err = Decide(t.Context(), deps, services, serviceRequest("unseen-token"))
	requireRefused(t, err, labelProjectNotVisible, "project not visible to the account's person: PRIV2")
}

// An exchange entry that names no project (the shared public-session account's form) gives the
// connection nothing to hold it to, and is refused saying so under a label of its own, not by the
// grant's emptiness.
func TestAnEntryNamingNoProjectIsRefusedByName(t *testing.T) {
	now := time.Now()
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}, {Kind: KindDaemon, Account: slowDaemonAccount}}
	tokens := map[string]platformToken{"empty-token": {account: slowDaemonAccount, issued: now, expiry: now.Add(time.Hour)}}
	dispatch := &dispatchAnswers{answers: map[string][]string{"PUB": {"acme/pub"}}}
	deps := heldDeps(t, tokens, map[string][]string{pubDaemonAccount: {"PUB"}, slowDaemonAccount: nil}, dispatch, services)

	_, err := Decide(t.Context(), deps, services, serviceRequest("empty-token"))
	requireRefused(t, err, labelNoProjects, "account's exchange entry names no project to hold it to: "+slowDaemonAccount)
}

// A grant natsgrant.WithinProjects cannot hold to the entry's projects is refused naming why,
// under a label of its own: here the broker's exchange list gives the account "pub", which is no
// project key.
func TestAGrantThatCannotBeHeldToItsProjectsIsRefusedByName(t *testing.T) {
	now := time.Now()
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}}
	tokens := map[string]platformToken{"pub-daemon-token": {account: pubDaemonAccount, issued: now, expiry: now.Add(time.Hour)}}
	dispatch := &dispatchAnswers{answers: map[string][]string{"pub": {"acme/pub"}}}
	deps := heldDeps(t, tokens, map[string][]string{pubDaemonAccount: {"pub"}}, dispatch, services)

	_, err := Decide(t.Context(), deps, services, serviceRequest("pub-daemon-token"))
	requireRefused(t, err, labelGrantNotHoldable, "grant cannot be held to its projects: service daemon/"+pubDaemonAccount+`: natsgrant: scope project "pub" is not a project key`)
}

// Through the auth service, Dispatch's grant may write the private-projects bucket and a
// listener's may read it, and nothing more of it: a listener's holds no write to its keys and no
// create of its stream. The static sets a hub renders hold neither, so a hub with no auth service
// renders what it rendered before (c-hub-config-unchanged), and the measurement job's grant is
// unchanged.
func TestDispatchAndListenerGrantsCarryThePrivateProjectsBucket(t *testing.T) {
	now := time.Now()
	tokens := map[string]platformToken{
		"dispatch-token": {account: "system:serviceaccount:dispatch:dispatch", issued: now, expiry: now.Add(time.Hour)},
		"listener-token": {account: listenerAccount, issued: now, expiry: now.Add(time.Hour)},
		"daemon-token":   {account: daemonAccount, issued: now, expiry: now.Add(time.Hour)},
		"measure-token":  {account: measureAccount, google: true, issued: now, expiry: now.Add(time.Hour)},
	}
	deps := Deps{ServiceToken: serviceTokens(tokens)}
	decide := func(raw string) natsgrant.Permissions {
		decision, err := Decide(t.Context(), deps, exampleServices, serviceRequest(raw))
		require.NoError(t, err)
		return decision.Grant
	}
	bucketKeys := "$KV." + bus.PrivateProjectsBucket + ".>"
	bucketCreate := "$JS.API.STREAM.CREATE.KV_" + bus.PrivateProjectsBucket

	dispatch := decide("dispatch-token")
	require.Equal(t, dispatchCalloutGrant("example-dispatch", "system:serviceaccount:dispatch:dispatch"), dispatch)
	require.Contains(t, dispatch.Publish, bucketKeys)

	listener := decide("listener-token")
	require.Equal(t, listenerCalloutGrant("example-hub", listenerAccount), listener)
	require.Contains(t, listener.Publish, "$JS.API.DIRECT.GET.KV_"+bus.PrivateProjectsBucket+".>")
	require.NotContains(t, listener.Publish, bucketKeys)
	require.NotContains(t, listener.Publish, bucketCreate)

	require.Equal(t, daemonCalloutGrant(daemonAccount), decide("daemon-token"))
	require.Equal(t, natsgrant.Measure(), decide("measure-token"))
	for name, static := range map[string]natsgrant.Permissions{
		"dispatch": natsgrant.Dispatch("example-dispatch"), "listener": natsgrant.Listener("example-hub"),
		"listener host": natsgrant.ListenerHost("example-hub"),
	} {
		for _, entry := range slices.Concat(static.Publish, static.Subscribe) {
			require.NotContains(t, entry, bus.PrivateProjectsBucket, "the static %s set a hub renders names the private-projects bucket", name)
		}
	}
}

// dispatchCalloutGrant is the grant the auth service answers Dispatch's connection with as
// machine machineID, of account: natsgrant.Dispatch, then the private-projects bucket's write,
// reading only the account's own replies (ownReplies).
func dispatchCalloutGrant(machineID, account string) natsgrant.Permissions {
	grant := natsgrant.Dispatch(machineID)
	grant.Publish = append(grant.Publish, natsgrant.KVWrite(bus.PrivateProjectsBucket)...)
	return withOwnReplies(grant, account, true)
}

// listenerCalloutGrant is the grant the auth service answers a listener's connection with as
// machine machineID, of account: natsgrant.Listener, then the private-projects bucket's read,
// reading only the account's own replies (ownReplies).
func listenerCalloutGrant(machineID, account string) natsgrant.Permissions {
	grant := natsgrant.Listener(machineID)
	grant.Publish = append(grant.Publish, natsgrant.KVRead(bus.PrivateProjectsBucket)...)
	return withOwnReplies(grant, account, true)
}

// daemonCalloutGrant is the grant the auth service answers a daemon whose entry takes the whole
// grant (*) with: natsgrant.Daemon, its _INBOX.> written as its account's own reply prefix.
func daemonCalloutGrant(account string) natsgrant.Permissions {
	return withOwnReplies(natsgrant.Daemon(), account, false)
}

// A Legion daemon presenting its token takes its account's own reply prefix whatever its entry's
// scope (the daemon's natsauth.ConnectService), so a daemon whose entry takes the whole grant (*)
// is granted that prefix too. Through the hub, its JetStream request is answered on that prefix:
// the stream it asks about does not exist, and it reads that answer. The control drops the prefix
// from the whole grant: the hub refuses the subscription to the prefix, and the request gets no
// answer.
func TestAWholeDaemonGrantTakesItsAccountsReplyPrefix(t *testing.T) {
	now := time.Now()
	tokens := map[string]platformToken{"daemon-token": {account: daemonAccount, issued: now, expiry: now.Add(time.Hour)}}
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	startServiceOf(t, url, keys, []ServiceEntry{{Kind: KindDaemon, Account: daemonAccount, AllProjects: true}}, Deps{ServiceToken: serviceTokens(tokens)})
	nc, err := nats.Connect(url, nats.UserInfo(natscred.ServiceUser, "daemon-token"), nats.NoReconnect(),
		nats.CustomInboxPrefix(natsgrant.InboxPrefix(daemonAccount)))
	require.NoError(t, err, "the hub did not admit the daemon")
	t.Cleanup(nc.Close)
	reply, err := nc.Request("$JS.API.STREAM.INFO."+bus.Stream, nil, 5*time.Second)
	require.NoError(t, err, "the daemon's JetStream request got no answer on its own reply prefix")
	require.Contains(t, string(reply.Data), "stream not found")
}
