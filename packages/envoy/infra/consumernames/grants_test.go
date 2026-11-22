package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nuid"
	"github.com/stretchr/testify/require"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/holdrecord/holdrecordtest"
	"github.com/sjawhar/envoy/internal/streamset"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// grantsHub is a hub of the test's own, a lone server or a cluster of three, with an admin as its
// no-auth user, which sets the scene and sees every subject published in the account, and the
// user "operator", granted exactly publish and subscribe. The static user is how a test holds a
// grant one subject short, which the stack's auth service never answers; with the whole of
// natsgrant.Operator(phase) it holds the grant the auth service answers the operator under that
// phase (natscallout decide.go's operator decision, which its decide_test.go rows hold equal to
// natsgrant.Operator(phase) for each phase).
type grantsHub struct {
	url     string
	cluster *testnats.Cluster
	admin   *nats.Conn

	mu   sync.Mutex
	seen map[string]bool
}

// grantsStartHub starts a hub whose operator user holds exactly grant: servers 1, or 3 with the
// writer hold's reach of their monitoring ports (holdtest.Kube), which convert's meta barrier reads.
func grantsStartHub(t *testing.T, servers int, grant natsgrant.Permissions) *grantsHub {
	t.Helper()
	quote := func(subjects []string) string {
		quoted := make([]string, len(subjects))
		for i, subject := range subjects {
			quoted[i] = fmt.Sprintf("%q", subject)
		}
		return strings.Join(quoted, ", ")
	}
	// nats-server reads an empty subscribe allow as every subject, so a grant with no subscribe is
	// written as a deny of every subject. A grant's deny lists (PublishDeny, SubscribeDeny) are
	// written beside its allow lists, as the auth service's user JWT carries them.
	publish := fmt.Sprintf("allow: [%s]", quote(grant.Publish))
	if len(grant.PublishDeny) > 0 {
		publish += fmt.Sprintf(", deny: [%s]", quote(grant.PublishDeny))
	}
	subscribe := fmt.Sprintf("allow: [%s]", quote(grant.Subscribe))
	if len(grant.SubscribeDeny) > 0 {
		subscribe += fmt.Sprintf(", deny: [%s]", quote(grant.SubscribeDeny))
	}
	if len(grant.Subscribe) == 0 {
		subscribe = `deny: [">"]`
	}
	config := fmt.Sprintf(`authorization {
  users = [
    { user: "admin", password: "admin" },
    { user: "operator", password: "op-pass", permissions: { publish: { %s }, subscribe: { %s } } }
  ]
}
no_auth_user: "admin"
`, publish, subscribe)
	h := &grantsHub{seen: map[string]bool{}}
	if servers > 1 {
		h.cluster = testnats.StartClusterConfig(t, servers, config)
		h.url = h.cluster.URL()
	} else {
		_, h.url = testnats.StartConfig(t, "jetstream {}\n"+config, []string{"-js"}, true)
	}
	h.admin = testnats.Connect(t, h.url)
	t.Cleanup(h.admin.Close)
	return h
}

// grantsIgnored are the subjects the watcher sees that the operator's grant does not decide: the
// replies the servers send to the operator's inbox and to the admin's, and the server's own
// advisories.
var grantsIgnored = []string{natsgrant.OperatorInbox + ".", nats.InboxPrefix, "$JS.EVENT.", "$SYS."}

// watch records every subject published in the account from now on, until the returned stop,
// which answers them sorted. stop flushes the watcher's connection, so every message the server
// sent it before stop is in its subscription's queue, then drains the subscription and waits until
// it is closed, so the callback has recorded each of them before stop reads what it saw: an
// unsubscribe dropped the messages still queued, and under load the last subject went missing.
func (h *grantsHub) watch(t *testing.T) func() []string {
	t.Helper()
	sub, err := h.admin.Subscribe(">", func(msg *nats.Msg) {
		for _, prefix := range grantsIgnored {
			if strings.HasPrefix(msg.Subject, prefix) {
				return
			}
		}
		h.mu.Lock()
		h.seen[msg.Subject] = true
		h.mu.Unlock()
	})
	require.NoError(t, err)
	require.NoError(t, h.admin.Flush())
	return func() []string {
		require.NoError(t, h.admin.Flush())
		require.NoError(t, sub.Drain())
		require.Eventually(t, func() bool { return !sub.IsValid() }, 30*time.Second, 10*time.Millisecond,
			"the watcher's subscription never finished draining")
		h.mu.Lock()
		defer h.mu.Unlock()
		return slices.Sorted(maps.Keys(h.seen))
	}
}

// grantsRig is one run's reach: the broker's hold routes on a Postgres schema of its own
// (holdrecordtest), whose row starts released at gen 1, a hub, the family databases a run names,
// and the permissions violations the server reported on the command's connection (refused).
type grantsRig struct {
	t       *testing.T
	broker  *holdrecordtest.Broker
	records *holdrecord.Client
	hub     *grantsHub
	dbs     map[string]*pgxpool.Pool

	mu      sync.Mutex
	refused []string
}

// refusals are the permissions violations the server reported on the command's connection.
func (g *grantsRig) refusals() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.refused)
}

func grantsNewRig(t *testing.T, servers int, grant natsgrant.Permissions) *grantsRig {
	t.Helper()
	broker := holdrecordtest.Start(t)
	records := broker.Client(t)
	_, err := records.RestoreRow(context.Background(), 1, natsgrant.OperatorPhaseReleased, "", "the grant test's bootstrap")
	require.NoError(t, err)
	return &grantsRig{t: t, broker: broker, records: records, hub: grantsStartHub(t, servers, grant), dbs: map[string]*pgxpool.Pool{}}
}

// hold moves the row to a hold of workflow in phase (prepare, or held as hold set writes its
// journal), and answers its id.
func (g *grantsRig) hold(w holdrecord.Workflow, phase natsgrant.OperatorPhase) string {
	g.t.Helper()
	ctx := context.Background()
	row, err := g.records.Row(ctx)
	require.NoError(g.t, err)
	id := nuid.Next()
	_, err = g.records.MoveRow(ctx, row.Gen, natsgrant.OperatorPhasePrepare, id, "the grant test's hold")
	require.NoError(g.t, err)
	require.NoError(g.t, g.records.StartJournal(ctx, id, w, nil))
	if phase == natsgrant.OperatorPhaseHeld {
		_, err = g.records.Admit(ctx, id, holdrecord.Request{Command: holdrecord.CommandSet}, nil)
		require.NoError(g.t, err)
		row, err = g.records.Row(ctx)
		require.NoError(g.t, err)
		_, err = g.records.MoveRow(ctx, row.Gen, natsgrant.OperatorPhaseHeld, id, "the grant test's hold")
		require.NoError(g.t, err)
		require.NoError(g.t, g.records.AppendDone(ctx, id, holdrecord.CommandSet, nil))
	}
	return id
}

// database makes family's store a database of its own, migrated with migrations
// (migratedDatabase).
func (g *grantsRig) database(family string, migrations ...string) {
	g.t.Helper()
	_, g.dbs[family] = migratedDatabase(g.t, "cgrants", migrations...)
}

// env is the run's env: the broker's routes, the hub as the static operator user with the
// operator's inbox prefix, the rig's databases, and, on a hub of three, the writer hold's reach of
// its servers (convert's meta barrier).
func (g *grantsRig) env() env {
	e := env{
		records: func(context.Context, options, bool) (*holdrecord.Client, error) { return g.broker.Client(g.t), nil },
		// The server reports a refused publish or subscription at once, on the connection's async
		// error handler (nats.go's processTransientError), before any request it refused times out:
		// the handler records it and closes the connection, which releases every request in flight
		// (nats.Conn.close, clearPendingRequestCalls), so a removed-grant control ends on the
		// refusal itself. The connection keeps the refusal as its last error, which the command
		// names (apiRefusal).
		connect: func(context.Context, options) (*nats.Conn, error) {
			return nats.Connect(g.hub.url, nats.UserInfo("operator", "op-pass"), nats.CustomInboxPrefix(natsgrant.OperatorInbox),
				nats.Name("envoy-infra consumer-names"), nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond), nats.Timeout(10*time.Second),
				nats.ErrorHandler(func(nc *nats.Conn, _ *nats.Subscription, err error) {
					if !errors.Is(err, nats.ErrPermissionViolation) {
						return
					}
					g.mu.Lock()
					g.refused = append(g.refused, err.Error())
					g.mu.Unlock()
					go nc.Close()
				}))
		},
		database: func(_ context.Context, _ options, family string) (Database, func(), error) {
			db, ok := g.dbs[family]
			if !ok {
				return nil, nil, fmt.Errorf("the test made no database for %s", family)
			}
			return db, nil, nil
		},
		// The hubs of static users have no callout stack, which the held re-read reads (its
		// render, declared pods and report); the rows measure the command's own subjects, and
		// each command's held rows on newStack run the re-read under the callout's exact grant.
		reread: func(context.Context, *run) error { return nil },
		now:    time.Now,
	}
	if g.hub.cluster != nil {
		kube := holdtest.NewKube(g.t, g.hub.cluster)
		op := &writerhold.Operator{Hub: writerhold.KubeHub{Client: kube.Client, Namespace: holdtest.Namespace, StatefulSet: holdtest.StatefulSet,
			ConfigMap: holdtest.ConfigMap, Forward: kube}, URLs: g.hub.cluster.URLs()}
		e.hub = func(context.Context, options) (*writerhold.Operator, error) { return op, nil }
	}
	return e
}

// notifications makes every protected stream the build declares as the admin, at one copy, as a
// hub holds them before a hold: each copied bucket (bus.CreateCopiedBucket, as `envoy-infra
// buckets create` makes it), then the rest (streamset.EnsureStreams: ENVOY_NOTIFICATIONS and every
// other bucket but the selection bucket); and publishes messages messages on one agent subject.
func (g *grantsRig) notifications(messages int) {
	g.t.Helper()
	ctx := context.Background()
	legacy, err := g.hub.admin.JetStream()
	require.NoError(g.t, err)
	for _, bucket := range bus.CopiedBuckets() {
		_, err := bus.CreateCopiedBucket(ctx, g.hub.admin, legacy, bucket, 1)
		require.NoError(g.t, err, "create the copied bucket %s", bucket)
	}
	require.NoError(g.t, streamset.EnsureStreams(ctx, g.hub.admin, streamset.Settings{}))
	js, err := jetstream.New(g.hub.admin)
	require.NoError(g.t, err)
	for i := range messages {
		_, err := js.Publish(ctx, "notifications.agent.ses_grant", []byte(fmt.Sprintf("message %d", i)))
		require.NoError(g.t, err)
	}
}

// fixed creates the fixed session consumer s-ses_grant as the admin, as a build before the fresh
// names did, with an inactive threshold below prepare's.
func (g *grantsRig) fixed() {
	g.t.Helper()
	_, err := writeConsumer(context.Background(), g.hub.admin, notifications, jetstream.ConsumerConfig{Durable: sessionFixedPrefix + "ses_grant",
		AckPolicy: jetstream.AckExplicitPolicy, FilterSubject: "notifications.agent.ses_grant", InactiveThreshold: time.Hour}, actionCreateConsumer)
	require.NoError(g.t, err)
}

// grantsCommand is one command of the grant test: the phase its grant is, the workflow of its hold
// ("" for none, status), its journal name ("" for one that writes none, prepare and status), the
// hub's size, and its scene, which answers its arguments; success checks what its run left.
type grantsCommand struct {
	name     string
	phase    natsgrant.OperatorPhase
	workflow holdrecord.Workflow
	journal  string
	servers  int
	scene    func(g *grantsRig, id string) []string
	success  func(g *grantsRig, id string)
}

// run sets a rig with grant, sets the scene, and runs the command, answering its exit code, its
// output and every subject published while it ran.
func (c grantsCommand) run(t *testing.T, grant natsgrant.Permissions) (*grantsRig, string, int, string, []string) {
	t.Helper()
	g := grantsNewRig(t, c.servers, grant)
	id := ""
	if c.workflow != "" {
		id = g.hold(c.workflow, c.phase)
	}
	args := c.scene(g, id)
	stop := g.hub.watch(t)
	code, stdout, stderr := createBucketRun(t, context.Background(), g.env(), args...)
	return g, id, code, stdout + stderr, stop()
}

// grantsSessionBucket makes the selection bucket with its sentinel as create-bucket made it for
// hold id, as the admin.
func grantsSessionBucket(g *grantsRig, id string) {
	g.t.Helper()
	createBucketMakeBucket(g.t, g.hub.admin, selection.StoreState{Boundary: 1, Hold: id})
}

// The grant test, by phase: prepare runs as a user granted exactly Operator(OperatorPhasePrepare)
// and succeeds; create-bucket, convert, replay and create-bucket --after-loss each run as a user
// granted exactly Operator(OperatorPhaseHeld), succeed, and write their intent and done entries;
// status runs under exactly Operator(OperatorPhaseReleased). The static user holds the list
// natsgrant.Operator renders, which is the auth service's answer to the operator under that phase.
// Control: for each subject the command published under the whole grant, the grant with every
// entry that admits that subject removed (and, as one more, the grant with no subscribe of the
// operator's inbox) fails the command, and its error names the refused subject in the server's
// words.
func TestTheGrantByPhase(t *testing.T) {
	restoreTimeout := createBucketRestoreTimeout
	createBucketRestoreTimeout = 5 * time.Second
	t.Cleanup(func() { createBucketRestoreTimeout = restoreTimeout })
	// The backup create-bucket --after-loss restores, made once, on the parent test, so its
	// directory outlives every subtest.
	backup, _ := createBucketBackupOf(t, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1, Hold: "the-cutover"},
		map[string]selection.Record{"ses_grant": {Start: 1}}, 0)
	commands := []grantsCommand{
		{name: "prepare", phase: natsgrant.OperatorPhasePrepare, workflow: holdrecord.WorkflowCutover, servers: 1,
			scene: func(g *grantsRig, id string) []string {
				g.notifications(3)
				g.fixed()
				return []string{actionPrepare, "--hold", id}
			},
			success: func(g *grantsRig, id string) {
				_, _, found, err := readPrepareCohort(context.Background(), g.records, id)
				require.NoError(g.t, err)
				require.True(g.t, found, "prepare wrote no cohort record")
			}},
		{name: "create-bucket", phase: natsgrant.OperatorPhaseHeld, workflow: holdrecord.WorkflowCutover, journal: createBucketCommand, servers: 1,
			scene: func(g *grantsRig, id string) []string {
				createBucketCohort(g.t, g.records, id, 1)
				return []string{actionCreateBucket, "--hold", id}
			}},
		{name: "convert", phase: natsgrant.OperatorPhaseHeld, workflow: holdrecord.WorkflowCutover, journal: actionConvert, servers: 3,
			scene: func(g *grantsRig, id string) []string {
				g.notifications(3)
				g.fixed()
				value, err := json.Marshal(prepareCohort{Boundary: 1, Members: []prepareCohortMember{{Family: familySession, Logical: "ses_grant", Fixed: sessionFixedPrefix + "ses_grant"}}})
				require.NoError(g.t, err)
				_, err = g.records.PutRecord(context.Background(), id, prepareCohortKind, prepareCohortKey, 0, value)
				require.NoError(g.t, err)
				grantsSessionBucket(g, id)
				return []string{actionConvert, "--hold", id}
			}},
		{name: "replay", phase: natsgrant.OperatorPhaseHeld, workflow: holdrecord.WorkflowCutover, journal: replayCommand, servers: 1,
			scene: func(g *grantsRig, id string) []string {
				g.notifications(3)
				grantsSessionBucket(g, id)
				return []string{actionReplay, "--hold", id, "--key", "ses_grant", "--from", "2"}
			}},
		{name: "create-bucket --after-loss", phase: natsgrant.OperatorPhaseHeld, workflow: holdrecord.WorkflowSelectionRecovery, journal: createBucketAfterLossCommand, servers: 1,
			scene: func(g *grantsRig, id string) []string {
				g.notifications(3)
				createBucketConsumer(g.t, g.hub.admin, selection.FreshName(sessionFreshPrefix), createBucketSessionMetadata("ses_grant"))
				return []string{actionCreateBucket, "--hold", id, "--after-loss", "--backup", backup}
			}},
		// The recovery from the released operator's walk, replayed with puts: no RESTORE subject.
		{name: "create-bucket --after-loss from a walk", phase: natsgrant.OperatorPhaseHeld, workflow: holdrecord.WorkflowSelectionRecovery, journal: createBucketAfterLossCommand, servers: 1,
			scene: func(g *grantsRig, id string) []string {
				g.notifications(3)
				createBucketConsumer(g.t, g.hub.admin, selection.FreshName(sessionFreshPrefix), createBucketSessionMetadata("ses_grant"))
				messages := createBucketWalkOfRecords(g.t, 2)
				walk := filepath.Join(g.t.TempDir(), "walk")
				createBucketWriteWalk(g.t, walk, messages, 1, uint64(len(messages)))
				return []string{actionCreateBucket, "--hold", id, "--after-loss", "--backup", walk}
			}},
		{name: "status", phase: natsgrant.OperatorPhaseReleased, servers: 1,
			// Status reads every family's store: the selection bucket (made by the admin, as
			// create-bucket would have made it) and Dispatch's intake_consumers, which the rig's
			// env hands it for any --dispatch-database-file.
			scene: func(g *grantsRig, _ string) []string {
				g.notifications(3)
				grantsSessionBucket(g, "the-cutover")
				g.database(familyDispatch, dispatchMigrations...)
				return []string{actionStatus, "--dispatch-database-file", "/dev/null"}
			}},
	}
	for _, c := range commands {
		t.Run(c.name, func(t *testing.T) {
			whole := natsgrant.Operator(c.phase)
			g, id, code, output, published := c.run(t, whole)
			require.Equal(t, 0, code, "%s under exactly Operator(%s)\n%s", c.name, c.phase, output)
			if c.journal != "" {
				require.Equal(t, []string{"intent", "done"}, createBucketEntries(readJournal(g.t, g.records, id), c.journal), "%s's journal entries", c.name)
			}
			if c.success != nil {
				c.success(g, id)
			}
			t.Logf("%s under exactly Operator(%s) published %s", c.name, c.phase, strings.Join(published, " "))

			// The entries each published subject needs: those of the whole grant that admit it.
			// Subjects one set of entries admits (each key of the selection bucket under
			// $KV.envoy_session_consumers.>) share one control.
			var groups [][]string
			for _, subject := range published {
				var admits []string
				for _, entry := range whole.Publish {
					if grantsAdmits(entry, subject) {
						admits = append(admits, entry)
					}
				}
				require.NotEmpty(t, admits, "%s published %s, which no entry of Operator(%s) admits", c.name, subject, c.phase)
				if !slices.ContainsFunc(groups, func(group []string) bool { return slices.Equal(group, admits) }) {
					groups = append(groups, admits)
				}
			}
			// Each control must end on the server's refusal, never on a timeout alone: a timeout also
			// comes when a request never left, so the connection's own report of the refused publish
			// (the rig's refusals) has to name a subject the removed entries admit.
			for _, admits := range groups {
				less := whole
				less.Publish = slices.DeleteFunc(slices.Clone(whole.Publish), func(entry string) bool { return slices.Contains(admits, entry) })
				g, _, code, output, _ := c.run(t, less)
				require.NotEqual(t, 0, code, "control: %s succeeded without %s\n%s", c.name, strings.Join(admits, ", "), output)
				require.True(t, grantsNamesOneOf(strings.Join(g.refusals(), "\n"), admits),
					"control: the server reported no refused publish of a subject %s admit: %q\n%s", strings.Join(admits, ", "), g.refusals(), output)
				require.True(t, grantsNamesOneOf(output, admits), "control: %s without %s names no refused subject those entries admit\n%s", c.name, strings.Join(admits, ", "), output)
				t.Logf("control: %s without %s exits %d, naming the server's refusal", c.name, strings.Join(admits, ", "), code)
			}
			less := whole
			less.Subscribe = nil
			g, _, code, output, _ = c.run(t, less)
			require.NotEqual(t, 0, code, "control: %s succeeded without its inbox's subscribe\n%s", c.name, output)
			refusedSubscribe := `Permissions Violation for Subscription to "` + natsgrant.OperatorInbox + `.`
			require.True(t, slices.ContainsFunc(g.refusals(), func(r string) bool { return strings.Contains(r, refusedSubscribe) }),
				"control: the server reported no refused subscribe of the operator's inbox: %q", g.refusals())
			require.Contains(t, output, refusedSubscribe, "control: %s without its inbox's subscribe\n%s", c.name, output)
		})
	}
}

// grantsRefusal is the server's refusal of a publish as an error carries it (nats-server writes the
// subject with %q).
var grantsRefusal = regexp.MustCompile(`Permissions Violation for Publish to ("(?:[^"\\]|\\.)*")`)

// grantsNamesOneOf reports whether output names, as the server refuses it, a publish to a subject
// one of entries admits.
func grantsNamesOneOf(output string, entries []string) bool {
	for _, match := range grantsRefusal.FindAllStringSubmatch(output, -1) {
		subject, err := strconv.Unquote(match[1])
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if grantsAdmits(entry, subject) {
				return true
			}
		}
	}
	return false
}

// grantsAdmits reports whether grant entry admits the literal subject, by NATS's token rules: a
// '*' token is any one token, and a last '>' token is one token or more. It is the test's own, so
// the package builds against every nats-server its rows run on (2.10.29 has no
// server.SubjectMatchesFilter).
func grantsAdmits(entry, subject string) bool {
	filter, tokens := strings.Split(entry, "."), strings.Split(subject, ".")
	for i, token := range filter {
		switch {
		case token == ">" && i == len(filter)-1:
			return len(tokens) > i
		case i >= len(tokens):
			return false
		case token != "*" && token != tokens[i]:
			return false
		}
	}
	return len(tokens) == len(filter)
}
