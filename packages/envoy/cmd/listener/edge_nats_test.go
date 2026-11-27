package main

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// edgeProject is the one project this package's edge listeners serve.
const edgeProject = "PRIV"

// startEdgeListenerProcess starts binary as an edge listener of projects (ENVOY_EDGE_PROJECTS),
// machine machineID, against the NATS at uri, with env added to its environment: on a loopback port
// the test holds (startListenerProcess says why), taking broker access tokens from the broker at
// brokerURL, which it asks with accountToken, and no other credential, as an edge listener takes
// them.
func startEdgeListenerProcess(t *testing.T, binary, uri, machineID, projects, brokerURL, accountToken string, env ...string) *listenerProcess {
	t.Helper()
	held := cmdtest.HoldPort(t)
	account := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(account, []byte(accountToken), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary)
	cmd.Env = append([]string{
		"PORT=" + strconv.Itoa(held.Port),
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=" + machineID,
		"NATS_URLS=" + uri,
		config.EdgeProjectsEnvVar + "=" + projects,
		EnvBrokerURL + "=" + brokerURL,
		EnvBrokerAccountTokenFile + "=" + account,
		EnvPublicURL + "=http://127.0.0.1:" + strconv.Itoa(held.Port),
	}, env...)
	held.Pass(cmd, "ENVOY_LISTEN_FD")
	return &listenerProcess{Process: cmdtest.Start(t, "the edge listener "+machineID, cmd), port: held.Port}
}

// edgeServerViolation matches a permissions violation nats-server logs, capturing its kind and
// the subject it names.
var edgeServerViolation = regexp.MustCompile(`(Publish|Subscription) Violation - .*?Subject "([^"]*)"`)

// listenerStateRequests are the requests on the listener state bucket (bus.ListenerStateBucket)
// a nats-server's log holds refused (edgeServerViolation), each as its kind and subject: a
// JetStream API call that names the bucket's stream, or a write of one of its keys.
func listenerStateRequests(log string) []string {
	stream := "KV_" + bus.ListenerStateBucket
	var found []string
	for _, match := range edgeServerViolation.FindAllStringSubmatch(log, -1) {
		if subject := match[2]; strings.HasPrefix(subject, "$KV."+bus.ListenerStateBucket+".") || slices.Contains(strings.Split(subject, "."), stream) {
			found = append(found, match[1]+" "+subject)
		}
	}
	return found
}

// An edge listener reads the stream and never creates it, so on a server that holds none its start
// stays up and not ready, /livez 204 and /healthz 503 "starting", and reads the stream again at
// each try, logging edgeStreamMissingLine at ERROR naming the stream, 1 s and then 2 s apart
// (connectListener); once a shared listener, which owns the stream, has created it, the waiting
// edge serves, and an edge listener started on the same server after that serves too. Under the
// former rule the edge's start ended at its first try, exit 1, naming the missing stream.
func TestAnEdgeListenerWaitsForTheStreamUntilASharedListenerCreatesIt(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)
	broker := ciphertokentest.NewStubBroker(t)

	waiting := startEdgeListenerProcess(t, binary, uri, "edge-no-stream", edgeProject, broker.Server.URL, edgeAccountToken)
	missing := func() []map[string]any {
		return linesIn(waiting.Output.String(), edgeStreamMissingLine, map[string]any{"level": "ERROR", "stream": bus.Stream})
	}
	for deadline := time.Now().Add(30 * time.Second); len(missing()) < 2; time.Sleep(200 * time.Millisecond) {
		requireLiveAndStarting(t, waiting, "the server held no stream")
		if time.Now().After(deadline) {
			t.Fatalf("the edge listener logged %d of the two %q it owes within 30 s:\n%s", len(missing()), edgeStreamMissingLine, waiting.Output.String())
		}
	}
	want := "the server holds no " + bus.Stream + " stream, and an edge listener may not create it"
	lines := missing()
	if text, _ := lines[0]["error"].(string); lines[0]["retry_in"] != "1s" || lines[1]["retry_in"] != "2s" || !strings.Contains(text, want) {
		t.Fatalf("the edge listener's first lines for the missing stream = %v, want the read's error naming %q, retry_in 1s then 2s", lines[:2], want)
	}
	requireLiveAndStarting(t, waiting, "the server held no stream")

	shared := startListenerProcess(t, binary, uri, "shared-makes-the-stream")
	shared.waitHealthy(t)
	shared.Terminate(t)
	shared.WaitExit(t, "SIGTERM")
	waiting.waitHealthy(t)

	started := startEdgeListenerProcess(t, binary, uri, "edge-with-stream", edgeProject, broker.Server.URL, edgeAccountToken)
	started.waitHealthy(t)
	if output := started.Output.String(); strings.Contains(output, "role lane subscribed") || !strings.Contains(output, "role lane not subscribed") {
		t.Fatalf("the edge listener opened a role lane, or did not say it opened none:\n%s", output)
	}
}

// An edge listener opens no delivery lease and sends nothing on the listener state bucket, as the
// server records it. The edge runs under its rendered grant (natsgrant.Edge), which holds no call
// of envoy_listener, so the server refuses, and logs, every request the listener makes there: the
// lease's open, a delivery ledger or last-seen read or write. The admin made the bucket before the
// edge started, as a shared listener's start does, so it is there to be used. The edge starts,
// registers a session under a broker token of its project (a listener with the lease records the
// session's last-seen time in the bucket here) and relays a send to it, and the server logs no
// refusal on the bucket but one the edge's own user is refused, made last, which shows the log is
// read. A request is the one route to the bucket, so a listener that sent none holds no lease.
func TestAnEdgeListenerOpensNoLeaseAndSendsNothingOnTheListenerStateBucket(t *testing.T) {
	t.Parallel()
	const (
		machineID = "example-edge-nolease"
		sessionID = "ses_edge_nolease"
		token     = "access-token-of-the-edge-session"
		message   = "example message to the edge"
	)
	grant, err := natsgrant.Edge(machineID, []string{edgeProject})
	if err != nil {
		t.Fatalf("the edge's grant: %v", err)
	}
	seed, public := testnats.User(t)
	granted := testnats.StartGranted(t, testnats.GrantedUser{Public: public, Grant: testnats.Grant{
		Publish:   testnats.GrantRules{Allow: grant.Publish},
		Subscribe: testnats.GrantRules{Allow: grant.Subscribe},
	}})
	admin, err := bus.ConnectOwningStream([]string{granted.AdminURL}, bus.WithReplicas(1), bus.WithName("edge-lease-test admin"))
	if err != nil {
		t.Fatalf("the admin creates the stream: %v", err)
	}
	t.Cleanup(admin.Close)
	for _, bucket := range append(bus.ListenerBuckets(), bus.ListenerStateBucket) {
		if _, err := admin.JS().CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bucket, Storage: natsgo.FileStorage}); err != nil {
			t.Fatalf("the admin makes the %s bucket: %v", bucket, err)
		}
	}
	broker := ciphertokentest.NewStubBroker(t)
	caller := ciphertokentest.NewCaller(t)
	broker.Says(token, ciphertoken.Introspection{
		Active:       true,
		Session:      sessionID,
		Person:       "outside-person@example.test",
		ProjectScope: ciphertoken.ProjectScopeNamed,
		Projects:     []string{edgeProject},
		ClientID:     ciphertoken.ExchangeClientID,
		Thumbprint:   caller.JKT(),
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	})
	// The edge's public origin is the one signedListenerCall signs for: of a variable named twice,
	// the process reads the last.
	edge := startEdgeListenerProcess(t, cmdtest.Build(t, "envoy-listener"), granted.URL(), machineID, edgeProject, broker.Server.URL, edgeAccountToken,
		bus.NkeySeedEnvVar+"="+seed, EnvPublicURL+"="+listenerBrokerOrigin)
	edge.waitHealthy(t)

	standIn := startSessionStandIn(t, granted.AdminURL, sessionID, answerDone)
	if status, _, answer := signedListenerCall(t, edge.port, caller, token, "/v1/interests/subscribe",
		`{"session_id":"`+sessionID+`","topics":[],"self_subscribed":true,"delivery":{"consumer":true,"epoch":"edge-epoch"}}`); status != http.StatusOK || !strings.Contains(answer, `"mode":"consumer"`) {
		t.Fatalf("the edge's registration of %s answered %d %s, want 200 consumer delivery:\n%s", sessionID, status, answer, edge.Output.String())
	}
	if status, _, answer := signedListenerCall(t, edge.port, caller, token, "/v1/messages/send",
		`{"target_session":"`+sessionID+`","source_session":"`+sessionID+`","message":"`+message+`"}`); status < 200 || status > 299 {
		t.Fatalf("the edge's send to %s answered %d %s:\n%s", sessionID, status, answer, edge.Output.String())
	}
	if frame := standIn.collect(t, 1, 15*time.Second)[0]; frame.item.PayloadSummary != message {
		t.Fatalf("the frame relayed to %s carries %q, want the send", sessionID, frame.item.PayloadSummary)
	}

	// The refusal provoked last, by the edge's own user, on a key no listener writes: a reading
	// that finds it and no other request on the bucket saw the listener send none.
	provoked := "$KV." + bus.ListenerStateBucket + ".edge-test-probe"
	probe, err := testnats.ConnectSeed(t, granted.URL(), seed)
	if err != nil {
		t.Fatalf("connect as the edge's user: %v", err)
	}
	t.Cleanup(probe.Close)
	if err := probe.Publish(provoked, nil); err != nil {
		t.Fatal(err)
	}
	_ = probe.Flush()
	waitFor(t, "the server to log its refusal of "+provoked+", so its log is read", func() bool {
		return slices.Contains(listenerStateRequests(granted.Log()), "Publish "+provoked)
	})
	for _, request := range listenerStateRequests(granted.Log()) {
		if request != "Publish "+provoked {
			t.Fatalf("the server refused the edge listener %s on the %s bucket: an edge listener opens no lease and sends nothing there:\n%s",
				request, bus.ListenerStateBucket, edge.Output.String())
		}
	}
}

// An edge listener names its own grant, natsgrant.Edge ("infra/core EdgePermissions",
// edgeListenerGrant), in the refusal lines of its start's bucket opens (grantName): the
// spent-proof bucket's (openSpentProofBucket) and a store's (openStartStore). That grant creates
// no bucket, so an edge started where no full listener made the buckets is refused at each open,
// and a line naming the listener's grant would send its operator to fix a grant the edge does not
// run under. The edge runs under its rendered grant, and the admin makes the stream alone; once
// the spent-proof bucket's refusal is logged the admin makes that bucket, so the start goes on to
// the interest registry's buckets and is refused there. Control: with grantName naming
// listenerGrant for every listener, the lines name the listener's grant and the row fails.
func TestAnEdgeListenerNamesItsOwnGrantInItsBucketRefusals(t *testing.T) {
	t.Parallel()
	const machineID = "example-edge-refused"
	grant, err := natsgrant.Edge(machineID, []string{edgeProject})
	if err != nil {
		t.Fatalf("the edge's grant: %v", err)
	}
	seed, public := testnats.User(t)
	granted := testnats.StartGranted(t, testnats.GrantedUser{Public: public, Grant: testnats.Grant{
		Publish:   testnats.GrantRules{Allow: grant.Publish},
		Subscribe: testnats.GrantRules{Allow: grant.Subscribe},
	}})
	admin, err := bus.ConnectOwningStream([]string{granted.AdminURL}, bus.WithReplicas(1), bus.WithName("edge-grant-name-test admin"))
	if err != nil {
		t.Fatalf("the admin creates the stream: %v", err)
	}
	t.Cleanup(admin.Close)
	edge := startEdgeListenerProcess(t, cmdtest.Build(t, "envoy-listener"), granted.URL(), machineID, edgeProject, ciphertokentest.NewStubBroker(t).Server.URL, edgeAccountToken,
		bus.NkeySeedEnvVar+"="+seed)
	// refusal is the first ERROR line the edge logged at line for one of buckets.
	refusal := func(line string, buckets ...string) map[string]any {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
			for _, record := range linesIn(edge.Output.String(), line, map[string]any{"level": "ERROR"}) {
				if bucket, _ := record["bucket"].(string); slices.Contains(buckets, bucket) {
					return record
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("the edge listener logged no %q for %v within 30 s:\n%s", line, buckets, edge.Output.String())
			}
		}
	}

	if record := refusal(spentProofBucketRefusedLine, bus.DPoPProofBucket); record["grant"] != edgeListenerGrant {
		t.Fatalf("the edge listener's refusal of %s names the grant %v, want its own, %q: %v", bus.DPoPProofBucket, record["grant"], edgeListenerGrant, record)
	}
	if _, err := admin.JS().CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.DPoPProofBucket, Storage: natsgo.FileStorage}); err != nil {
		t.Fatalf("the admin makes the %s bucket: %v", bus.DPoPProofBucket, err)
	}
	if record := refusal(storeBucketRefusedLine, bus.InterestBucket, bus.RoleClaimBucket); record["grant"] != edgeListenerGrant {
		t.Fatalf("the edge listener's refusal of %v names the grant %v, want its own, %q: %v", record["bucket"], record["grant"], edgeListenerGrant, record)
	}
}

// Serial (listenerTestParallelism): it changes something the whole process shares: it calls
// t.Setenv.
//
// Check l16 of dispatch://INFRA-3589 (R18) at the edge's start: an edge listener whose machine id
// is a fully qualified host name takes the grant natsgrant.Edge builds for that id, and its start
// names every inbox under the prefix that grant subscribes, the id spelled as its durable spells it
// (bus.ListenerSpelling), each dot as %2E. The start connects as a user holding that grant, on a
// server with no stream. Its read of the stream is answered on its inbox, so the start ends naming
// the missing stream: a reply under any other prefix would be a subscription the server refuses.
// An id with a space is refused naming edge mode, since its spelling, a hyphen, is another id's.
func TestADottedEdgeMachineIDStartsUnderItsSpelledPrefix(t *testing.T) {
	const (
		machineID = "host.example.internal"
		prefix    = "_INBOX_envoy_edge_host%2Eexample%2Einternal"
	)
	edge, err := newEdgeGrant(machineID, []string{edgeProject}, metrics.New())
	if err != nil {
		t.Fatalf("newEdgeGrant(%q, [%s]): %v", machineID, edgeProject, err)
	}
	grant, err := natsgrant.Edge(machineID, []string{edgeProject})
	if err != nil {
		t.Fatalf("natsgrant.Edge(%q, [%s]): %v", machineID, edgeProject, err)
	}
	if edge.inboxPrefix != prefix || grant.Subscribe[0] != prefix+".>" {
		t.Fatalf("the start's inbox prefix is %q and the grant subscribes %q, want %q under both", edge.inboxPrefix, grant.Subscribe[0], prefix)
	}
	seed, public := testnats.User(t)
	_, uri := testnats.StartConfig(t, testnats.GrantedUsersConfig(testnats.StaticUser{NKey: public, Publish: grant.Publish, Subscribe: grant.Subscribe}), []string{"-js"}, false)
	t.Setenv(bus.NkeySeedEnvVar, seed)
	client, err := connectListenerBus(config.Service{NATSURLs: []string{uri}}, edge, listenerTryInstance, newNATSStart(listenerCredentialWait))
	if err == nil {
		client.Close()
		t.Fatal("the edge's start connected on a server with no stream")
	}
	if want := "the server holds no " + bus.Stream + " stream"; !strings.Contains(err.Error(), want) {
		t.Fatalf("the edge's start ended with %q, want it to name the missing stream (%q): its read was not answered under %s", err, want, prefix)
	}
	if refused, err := newEdgeGrant("host example", []string{edgeProject}, metrics.New()); err == nil || refused != nil || !strings.Contains(err.Error(), "edge mode") {
		t.Fatalf("newEdgeGrant(host example, [%s]) = %v, %v; want no grant and a refusal naming edge mode", edgeProject, refused, err)
	}
}
