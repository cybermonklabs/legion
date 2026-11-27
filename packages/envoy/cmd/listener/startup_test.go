package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/listenerwire"
	"github.com/sjawhar/envoy/natsversion"
)

// During the first roll onto the delivery lease, the durable's push binding is a pre-lease build's
// (nats.Bind) until that build stops, so the new pod that takes the lease waits it out; the load
// balancer and the private DNS name already send the new pod traffic by then. Neither a webhook nor
// /v1 reads the durable, so the new pod serves both, and runs its role lane, while it waits:
// GitHub does not redeliver a refused delivery on its own, and a refused /v1 call is a failed
// Dispatch delivery or a lost publish. /healthz answers it 200 degraded, a standby waiting for the
// binding to end, and 200 healthy and active once the old build lets go.
func TestTheV1APIIsServedWhileAPreLeaseBuildHoldsTheDurable(t *testing.T) {
	t.Parallel()
	const (
		machineID = "v1-bind-window"
		secret    = "bind-window-secret"
		delivery  = "delivery-bind-window"
		target    = "ses_bind_window_target"
		holderID  = "ses_bind_window_holder"
		role      = "bind-window-role"
	)
	uri := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect bus: %v", err)
	}
	t.Cleanup(client.Close)
	old, _ := durableHeldElsewhere(t, uri, "listener-"+machineID)

	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), client.Conn.ConnectedUrl(), machineID,
		"ENVOY_WEBHOOKS=github", "ENVOY_GITHUB_WEBHOOK_SECRET="+secret, "ENVOY_REVIEWER_APP_ID=1")
	listener.WaitForOutput(t, "durable is push-bound by a subscriber without the lease; waiting for its binding to end")

	body := []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("b", 40) + `","before":"` + strings.Repeat("a", 40) + `",` +
		`"pusher":{"name":"example-author"},"sender":{"login":"example-author","type":"User"},` +
		`"repository":{"name":"bind-window","owner":{"login":"acme"},"full_name":"acme/bind-window"}}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/webhook/github", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("webhook request: %v", err)
	}
	request.Header.Set("X-GitHub-Delivery", delivery)
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	status, answer := do(t, request)
	if status != http.StatusOK {
		t.Fatalf("webhook while another task holds the durable: status %d %q, want 200\n%s", status, answer, listener.Output.String())
	}
	message, err := client.JS().GetLastMsg(bus.Stream, "notifications.github.acme.bind-window.push.branch.main")
	if err != nil {
		t.Fatalf("the accepted push is not on the stream: %v", err)
	}
	var envelope contracts.Envelope
	if err := json.Unmarshal(message.Data, &envelope); err != nil {
		t.Fatalf("decode the published push: %v", err)
	}
	if envelope.SourceEventID != delivery {
		t.Fatalf("the stream's last push is delivery %q, want %q", envelope.SourceEventID, delivery)
	}

	// A list /v1 answers while the durable is held has its gauge sample: the gauges read the stores
	// /v1 opened onto, not the dependencies /healthz waits for. Over an empty session bucket that
	// sample is 0, and it counts the session registered next.
	assertSessionsMatchTheirSample(t, listener, 0)
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(target, "epoch-1"))
	assertSessionsMatchTheirSample(t, listener, 1)
	status, answer = callListener(t, listener.port, http.MethodPost, "/v1/messages/send", `{"target_session":"`+target+`","message":"sent while the durable is held"}`)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/messages/send while another task holds the durable: status %d %q, want 200\n%s", status, answer, listener.Output.String())
	}
	var sent contracts.Envelope
	if err := json.Unmarshal([]byte(answer), &sent); err != nil {
		t.Fatalf("decode the send answer %q: %v", answer, err)
	}
	message, err = client.JS().GetLastMsg(bus.Stream, contracts.LegacyAgentSubject(target))
	if err != nil {
		t.Fatalf("the sent message is not on the stream: %v", err)
	}
	var stored contracts.Envelope
	if err := json.Unmarshal(message.Data, &stored); err != nil {
		t.Fatalf("decode the stored message: %v", err)
	}
	if stored.EventID != sent.EventID {
		t.Fatalf("the stream's last message for %s is event %q, want the sent %q", target, stored.EventID, sent.EventID)
	}

	// The role lane is open too: a role message published straight onto it, as an operator's or an
	// earlier listener's would be, reaches its holder through this listener's queue subscription
	// before the durable binds.
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(holderID, "epoch-1"))
	postListener(t, listener.port, "/v1/roles/set", `{"session_id":"`+holderID+`","role":"`+role+`"}`)
	frames := receiveAsSession(t, client, holderID)
	onLane := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "publish.bind-window-role")
	onLane.SourceSession = "ses_bind_window_sender"
	if err := client.Publish(onLane); err != nil {
		t.Fatalf("publish a role message onto the lane while another task holds the durable: %v", err)
	}
	select {
	case forwarded := <-frames:
		if forwarded.Topic != contracts.RoleTopicPrefix+role || !strings.HasPrefix(forwarded.DedupeKey, roleForwardDedupePrefix) {
			t.Fatalf("the holder received topic %q dedupe key %q, want the role forward of %s", forwarded.Topic, forwarded.DedupeKey, contracts.RoleTopicPrefix+role)
		}
	case <-time.After(listenerWaitBound(t)):
		t.Fatalf("the role holder received no forward while another task holds the durable:\n%s", listener.Output.String())
	}

	// The waiting pod is ready, a degraded standby until it receives: it holds the lease and
	// delivers nothing, which is no healthy answer, but a 503 would have the runtime kill it while
	// the old build still holds the durable.
	status, answer = do(t, mustRequest(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/healthz"))
	var health map[string]any
	_ = json.Unmarshal([]byte(answer), &health)
	if status != http.StatusOK || health["status"] != "degraded" || health["role"] != "standby" || health["receive"] != receiveAwaitingBinding.String() {
		t.Fatalf("/healthz while the old build holds the durable: status %d %q, want 200 degraded, a standby waiting for the binding", status, answer)
	}

	// The old build exits, which releases its binding.
	old.Close()
	listener.waitHealthy(t)
	listener.WaitForOutput(t, "durable receiving")
}

// assertSessionsMatchTheirSample fails the test unless the listener's GET /v1/sessions answers a
// 200 list of want rows and its /metrics has the envoy_active_sessions sample want.
func assertSessionsMatchTheirSample(t *testing.T, listener *listenerProcess, want int) {
	t.Helper()
	status, answer := callListener(t, listener.port, http.MethodGet, "/v1/sessions", "")
	var rows []listenerwire.SessionInfo
	if status != http.StatusOK || json.Unmarshal([]byte(answer), &rows) != nil || len(rows) != want {
		t.Fatalf("GET /v1/sessions: status %d %q, want a 200 list of %d\n%s", status, answer, want, listener.Output.String())
	}
	status, scrape := do(t, mustRequest(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/metrics"))
	if sample := "\nenvoy_active_sessions " + strconv.Itoa(want) + "\n"; status != http.StatusOK || !strings.Contains(scrape, sample) {
		t.Fatalf("/metrics beside a %d-row session list: status %d, want the sample %q:\n%s", want, status, strings.TrimSpace(sample), scrape)
	}
}

// A listener started before its NATS answers waits for it, however long that takes: the process
// stays up past the ten connect tries that once ended the start, /healthz answers 503 "starting"
// and /v1 503 "service starting" meanwhile, and once NATS answers the listener is ready, 200 as
// the machine's active pod, within 10 s. A second pod of the machine started then is ready too,
// 200 as the standby naming the first.
func TestAListenerStartedBeforeItsNATSWaitsForItThenServes(t *testing.T) {
	t.Parallel()
	server, uri := testnats.Start(t)
	server.Stop(t)
	binary := cmdtest.Build(t, "envoy-listener")
	listener := startListenerProcess(t, binary, uri, "box-late", "ENVOY_INSTANCE_ID=pod-late")
	// The first retry comes after the connect's own ten tries, where the start used to exit.
	listener.WaitForOutput(t, "NATS connect failed; retrying")
	for until := time.Now().Add(3 * time.Second); time.Now().Before(until); time.Sleep(250 * time.Millisecond) {
		if status, body := callListener(t, listener.port, http.MethodGet, "/healthz", ""); status != http.StatusServiceUnavailable || !strings.Contains(body, `"status":"starting"`) {
			t.Fatalf("/healthz while NATS is away = %d %s, want 503 starting", status, body)
		}
	}
	// A closed /v1 answers "service starting", except to the two list reads, which get the
	// refilling answer their routes give while a copy cannot be served, so a caller reads one
	// answer for "the list is not ready" whichever gives it.
	if status, body := callListener(t, listener.port, http.MethodGet, "/v1/sessions", ""); status != http.StatusServiceUnavailable || !strings.Contains(body, "refilling") {
		t.Fatalf("/v1/sessions while NATS is away = %d %s, want the refilling 503", status, body)
	}
	if status, body := callListener(t, listener.port, http.MethodGet, "/v1/roles/none", ""); status != http.StatusServiceUnavailable || !strings.Contains(body, "service starting") {
		t.Fatalf("/v1 while NATS is away = %d %s, want 503 service starting", status, body)
	}
	select {
	case <-listener.Exited:
		t.Fatalf("the listener exited while NATS was away:\n%s", listener.Output.String())
	default:
	}

	server.Start(t)
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, body := callListener(t, listener.port, http.MethodGet, "/healthz", "")
		var health map[string]any
		_ = json.Unmarshal([]byte(body), &health)
		if status == http.StatusOK && health["status"] == "healthy" && health["role"] == "active" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/healthz 10 s after NATS answered = %d %s, want 200 healthy and active:\n%s", status, body, listener.Output.String())
		}
		time.Sleep(200 * time.Millisecond)
	}

	standby := startListenerProcess(t, binary, uri, "box-late", "ENVOY_INSTANCE_ID=pod-standby")
	standby.waitStandby(t, "pod-late")
	status, body := callListener(t, standby.port, http.MethodGet, "/healthz", "")
	var health map[string]any
	_ = json.Unmarshal([]byte(body), &health)
	if status != http.StatusOK || health["role"] != "standby" || health["active_instance"] != "pod-late" {
		t.Fatalf("the standby's /healthz = %d %s, want 200, role standby, active_instance pod-late", status, body)
	}
}

// A SIGTERM while the start waits for NATS ends the process at once: the start installs its own
// handler only once it serves /v1, so until then the signal's default action ends it.
func TestASIGTERMEndsAListenerWaitingForItsNATS(t *testing.T) {
	t.Parallel()
	server, uri := testnats.Start(t)
	server.Stop(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "box-signalled")
	listener.WaitForOutput(t, "envoy-listener listening")
	if status, body := callListener(t, listener.port, http.MethodGet, "/healthz", ""); status != http.StatusServiceUnavailable || !strings.Contains(body, `"status":"starting"`) {
		t.Fatalf("/healthz while NATS is away = %d %s, want 503 starting", status, body)
	}
	listener.Terminate(t)
	select {
	case <-listener.Exited:
	case <-time.After(3 * time.Second):
		t.Fatalf("the listener was still running 3 s after SIGTERM:\n%s", listener.Output.String())
	}
}

// A listener whose NATS cluster has fewer members than the copies ENVOY_NATS_REPLICAS asks for
// stays up and not ready until they join, then serves. On a hub of two servers that a third joins
// later (testnats.StartClusterPart), the meta group has its leader and can place no stream of three
// copies: the server answers the start's create of ENVOY_NOTIFICATIONS "no suitable peers for
// placement" (JSClusterNoPeersErrF, 10005), an answer of the cluster's state and not of the
// listener's settings (bus.ClusterStateAnswer). The listener logs clusterStateLine at ERROR at
// each try, naming the stream, the setting and the server's text, after a wait of 1 s and then 2 s
// (nextRefusedWait), and answers /healthz 503 "starting" and /livez 204 throughout. Once the third
// server joins, a later try creates the stream at three copies and the listener is ready. Before,
// the start exited 1 at the refusal (connectListener): the answer is a 400, which
// natsConnectionError does not retry.
func TestAListenerWhoseClusterHasTooFewMembersForItsCopiesStaysUnreadyUntilTheyJoin(t *testing.T) {
	t.Parallel()
	cluster := testnats.StartClusterPart(t, 3, 2)
	urls := cluster.URLs()
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), urls[0]+","+urls[1], "box-cluster-forming",
		"ENVOY_NATS_REPLICAS=3", "ENVOY_INSTANCE_ID=pod-cluster-forming")

	const while = "its NATS cluster had two members for the three copies it asks"
	refusals := func() []map[string]any {
		return linesIn(listener.Output.String(), clusterStateLine,
			map[string]any{"level": "ERROR", "stream": bus.Stream, "setting": bus.ReplicasEnvVar + "=3"})
	}
	for deadline := time.Now().Add(30 * time.Second); len(refusals()) < 2; time.Sleep(200 * time.Millisecond) {
		requireLiveAndStarting(t, listener, while)
		if time.Now().After(deadline) {
			t.Fatalf("the listener named the refused create of %s %d times at ERROR within 30 s, want it named at each try:\n%s",
				bus.Stream, len(refusals()), listener.Output.String())
		}
	}
	for index, wait := range []string{"1s", "2s"} {
		record := refusals()[index]
		text, _ := record["error"].(string)
		if record["retry_in"] != wait || record["code"] != float64(10005) || !strings.Contains(text, "no suitable peers for placement") {
			t.Fatalf("refusal %d = %v, want retry_in %s, code 10005 and the server's text", index+1, record, wait)
		}
	}
	requireLiveAndStarting(t, listener, while)

	cluster.Servers[2].Start(t)
	cluster.AwaitReady(t)
	listener.waitHealthy(t)
	if info := testnats.AwaitStreamCopies(t, cluster.JetStream(t), bus.Stream, 3); testnats.Copies(info.Cluster) != 3 {
		t.Fatalf("the listener is ready and %s holds %d copies, want the 3 it asks for", bus.Stream, testnats.Copies(info.Cluster))
	}
}

// bucketDeniedConfig is a NATS server whose one nkey user may publish to anything but KV bucket
// bucket: the grant of a hub that does not grant the bucket to its listeners.
func bucketDeniedConfig(user, bucket string) string {
	return publishDeniedConfig(user, "$JS.API.STREAM.*."+"KV_"+bucket, "$JS.API.CONSUMER.*."+"KV_"+bucket+".>", "$KV."+bucket+".>")
}

// publishDeniedConfig is a NATS server whose one nkey user may publish to anything but the
// subjects deny names, and subscribe to anything.
func publishDeniedConfig(user string, deny ...string) string {
	quoted := make([]string, len(deny))
	for i, subject := range deny {
		quoted[i] = strconv.Quote(subject)
	}
	return "jetstream {}\nauthorization {\n  users = [ { nkey: " + strconv.Quote(user) + ", permissions: { publish: { allow: [\">\"], deny: [" +
		strings.Join(quoted, ", ") + "] } } } ]\n}\n"
}

// A store bucket the listener's NATS user may not use follows the rule the durable and the
// spent-proof bucket follow (TestAListenerWhoseGrantRefusesItsSpentProofBucketStaysUnready): the
// listener stays up and not ready, /livez 204 and /healthz 503 "starting", and tries the store's
// open again (openStartStore), naming the refusal at ERROR at each try (storeBucketRefusedLine)
// with the bucket, the grant that lacks it, the refused subject and the server's text, after a
// wait of 1 s and then 2 s (nextRefusedWait). (The listener state bucket is the exception: without
// it the listener delivers without a lease.) Before, the start exited 1 at the refusal.
func TestAListenerWhoseGrantRefusesAStoreBucketNamesItAtEachTryAndStaysUnready(t *testing.T) {
	t.Parallel()
	seed, public := testnats.User(t)
	_, uri := testnats.StartConfig(t, bucketDeniedConfig(public, bus.SessionBucket), []string{"-js"}, false)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "box-refused", "NATS_NKEY_SEED="+seed)
	const while = "the NATS grant refused its " + bus.SessionBucket + " bucket"
	refusals := func() []map[string]any {
		return linesIn(listener.Output.String(), storeBucketRefusedLine,
			map[string]any{"level": "ERROR", "bucket": bus.SessionBucket, "grant": listenerGrant})
	}
	for deadline := time.Now().Add(30 * time.Second); len(refusals()) < 2; time.Sleep(200 * time.Millisecond) {
		requireLiveAndStarting(t, listener, while)
		if time.Now().After(deadline) {
			t.Fatalf("the listener named the refused open of %s %d times at ERROR within 30 s, want it named at each try:\n%s",
				bus.SessionBucket, len(refusals()), listener.Output.String())
		}
	}
	for index, wait := range []string{"1s", "2s"} {
		record := refusals()[index]
		subject, _ := record["subject"].(string)
		ofBucket := strings.HasSuffix(subject, ".KV_"+bus.SessionBucket) || strings.Contains(subject, ".KV_"+bus.SessionBucket+".") ||
			strings.HasPrefix(subject, "$KV."+bus.SessionBucket+".")
		text, _ := record["error"].(string)
		if record["retry_in"] != wait || !ofBucket || !strings.Contains(text, `Permissions Violation for Publish to "`+subject+`"`) {
			t.Fatalf("refusal %d = %v, want retry_in %s, a subject of %s and the server's violation of it", index+1, record, wait, bus.SessionBucket)
		}
	}
	requireLiveAndStarting(t, listener, while)
}

// The notification stream's read and create follow the rule a refused store bucket follows
// (TestAListenerWhoseGrantRefusesAStoreBucketNamesItAtEachTryAndStaysUnready). A NATS user whose
// grant lacks the stream's API subjects, its read ($JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS) and its
// create, connects, and the start's ensure of the stream is refused: the listener stays up and not
// ready, /livez 204 and /healthz 503 "starting", and connects again (connectListener), naming the
// refusal at ERROR at each try (streamRefusedLine) with the stream, the grant that lacks it, the
// refused read and the server's text, after a wait of 1 s and then 2 s (nextRefusedWait). Every pod
// of a machine connects as the same NATS user, so each would be refused alike. Once the server
// restarts with a grant that has them, a later try creates the stream and the listener is ready.
// Before, the start exited 1 at the refusal, logging "NATS connect failed".
func TestAListenerWhoseGrantRefusesTheNotificationStreamStaysUnreadyUntilTheGrantHasIt(t *testing.T) {
	t.Parallel()
	seed, public := testnats.User(t)
	read, create := "$JS.API.STREAM.INFO."+bus.Stream, "$JS.API.STREAM.CREATE."+bus.Stream
	server, uri := testnats.StartConfig(t, publishDeniedConfig(public, read, create), []string{"-js"}, false)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "box-stream-refused",
		"NATS_NKEY_SEED="+seed, "ENVOY_INSTANCE_ID=pod-stream-refused")
	const while = "the NATS grant refused the read and create of " + bus.Stream
	refusals := func() []map[string]any {
		return linesIn(listener.Output.String(), streamRefusedLine,
			map[string]any{"level": "ERROR", "stream": bus.Stream, "grant": listenerGrant, "subject": read})
	}
	for deadline := time.Now().Add(30 * time.Second); len(refusals()) < 2; time.Sleep(200 * time.Millisecond) {
		requireLiveAndStarting(t, listener, while)
		if time.Now().After(deadline) {
			t.Fatalf("the listener named the refused read of %s %d times at ERROR within 30 s, want it named at each try:\n%s",
				bus.Stream, len(refusals()), listener.Output.String())
		}
	}
	for index, wait := range []string{"1s", "2s"} {
		if record := refusals()[index]; record["retry_in"] != wait || !strings.Contains(fmt.Sprint(record["error"]), `Permissions Violation for Publish to "`+read+`"`) {
			t.Fatalf("refusal %d = %v, want retry_in %s and the server's violation of %s", index+1, record, wait, read)
		}
	}
	requireLiveAndStarting(t, listener, while)

	server.Restart(t, publishDeniedConfig(public))
	listener.waitHealthy(t)
}

// A connection the start opens once its stores are open, the session consumers' and then the role
// lane's, that no server lets through for longer than its ten dials (bus.Connect) is waited for,
// as the start's own connect is (TestAListenerStartedBeforeItsNATSWaitsForItThenServes). A relay
// (startGate) lets the listener's own connection and its lease's through and closes every dial of
// the session consumers' connection, then of the role lane's, until the test lets each through:
// the listener stays up and not ready, /livez 204 and /healthz 503 "starting", past the eleventh
// refused dial of each, logs the failure at WARN after each ten dials with the 2 s wait
// (startRetry), and is ready once both have connected. Before, the start exited 1 after either
// connection's tenth dial.
func TestAListenerWhoseLaterConnectionsCannotDialWaitsForThemThenServes(t *testing.T) {
	t.Parallel()
	gate := startStartGate(t, testnats.URL(t))
	steps := []struct{ name, line string }{
		{"envoy-session-consumers", "session consumer connection failed; retrying"},
		{"envoy-role-lane", "role lane connection failed; retrying"},
	}
	gate.refuse(steps[0].name, steps[1].name)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), gate.url, "box-late-lanes", "ENVOY_INSTANCE_ID=pod-late-lanes")
	for _, step := range steps {
		while := "no dial of its " + step.name + " connection was let through"
		failures := func(level string) []map[string]any {
			return linesIn(listener.Output.String(), step.line, map[string]any{"level": level})
		}
		for deadline := time.Now().Add(45 * time.Second); gate.refused(step.name) <= 10 || len(failures("WARN")) == 0; time.Sleep(200 * time.Millisecond) {
			requireLiveAndStarting(t, listener, while)
			if time.Now().After(deadline) {
				t.Fatalf("within 45 s the relay refused %d dials of %s and the listener logged %d WARN %q, want past ten dials and a WARN:\n%s",
					gate.refused(step.name), step.name, len(failures("WARN")), step.line, listener.Output.String())
			}
		}
		if record := failures("WARN")[0]; record["retry_in"] != startRetry.String() {
			t.Fatalf("the first failure of %s = %v, want retry_in %s", step.name, record, startRetry)
		}
		if erred := failures("ERROR"); len(erred) != 0 {
			t.Fatalf("the listener logged %q at ERROR for dials no server let through, want WARN: %v", step.line, erred)
		}
		requireLiveAndStarting(t, listener, while)
		gate.admit(step.name)
	}
	listener.waitHealthy(t)
}

// A nats-server release the listener refuses (natsversion's floor, asked as the session
// consumers' connection opens) keeps the pod up and not ready, and the start tries again, serving
// nothing meanwhile. A relay (startGate) reports the server as nats-server 2.10.29, a release
// that can skip a message on a session consumer's filters: the listener logs
// natsReleaseRefusedLine at ERROR at each try, naming the release and the floor
// (natsversion.FixedReleases), after a wait of 1 s and then 2 s (nextRefusedWait), while /livez
// answers 204, /healthz 503 "starting" and /v1 503 "service starting"; no connection of it
// subscribes the role lane or a relay's replies, and the stream holds no consumer, its durable's
// or a session's, so nothing is delivered. Once the relay reports the server's own release, a later
// try connects and the listener is ready. Before, the start exited 1 at the refusal.
func TestAListenerWhoseNATSReleaseIsBelowItsFloorServesNothingUntilTheServerIsOneItTakes(t *testing.T) {
	t.Parallel()
	server, uri := testnats.Start(t)
	gate := startStartGate(t, uri)
	const release = "2.10.29"
	gate.reportRelease(release)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), gate.url, "box-below-floor", "ENVOY_INSTANCE_ID=pod-below-floor")
	const while = "its NATS server reported a release below the floor"
	refusals := func() []map[string]any {
		return linesIn(listener.Output.String(), natsReleaseRefusedLine,
			map[string]any{"level": "ERROR", "release": release, "floor": natsversion.FixedReleases})
	}
	for deadline := time.Now().Add(30 * time.Second); len(refusals()) < 2; time.Sleep(200 * time.Millisecond) {
		requireLiveAndStarting(t, listener, while)
		if time.Now().After(deadline) {
			t.Fatalf("the listener named the refused release %s %d times at ERROR within 30 s, want it named at each try:\n%s",
				release, len(refusals()), listener.Output.String())
		}
	}
	for index, wait := range []string{"1s", "2s"} {
		if record := refusals()[index]; record["retry_in"] != wait || !strings.Contains(fmt.Sprint(record["error"]), "nats-server "+release) {
			t.Fatalf("refusal %d = %v, want retry_in %s and the refusal's text naming nats-server %s", index+1, record, wait, release)
		}
	}
	if status, body := callListener(t, listener.port, http.MethodGet, "/v1/roles/none", ""); status != http.StatusServiceUnavailable || !strings.Contains(body, "service starting") {
		t.Fatalf("/v1 while %s = %d %s, want 503 service starting", while, status, body)
	}
	for _, subject := range serverSubscriptions(t, server) {
		if subject == contracts.RoleTopicPrefix+">" || strings.HasPrefix(subject, "_INBOX.") && strings.HasSuffix(subject, ".>") {
			t.Fatalf("a connection subscribes %s while %s, want no role lane and no relay's replies", subject, while)
		}
	}
	if info, err := connectPublisher(t, uri).JS().StreamInfo(bus.Stream); err != nil || info.State.Consumers != 0 {
		t.Fatalf("%s while %s = %v, %v, want no consumer on it", bus.Stream, while, info, err)
	}
	requireLiveAndStarting(t, listener, while)
	gate.reportRelease("")
	listener.waitHealthy(t)
}

// JetStream's "temporarily unavailable" (JSClusterNotAvailErr, 10008), what a routine JetStream
// leader change answers until its election ends, is logged at WARN for its first 30 s
// (clusterUnavailableWarnFor) and at ERROR at each try after, with the backoff of every answer of
// the cluster's state. Of a hub of three servers two stop, so the meta group has no leader, and
// the one left answers the start's stream lookup 10008: the listener logs clusterStateLine at WARN
// at each try after a wait of 1 s, 2 s, 4 s, 8 s and 16 s, then at ERROR at the two tries after,
// each 30 s or more after the first answer and with a wait of 30 s, and stays up and not ready
// throughout. Before, every such answer was logged at ERROR, the first included.
func TestAListenerWhoseJetStreamIsTemporarilyUnavailableWarnsForThirtySecondsThenErrsAtEachTry(t *testing.T) {
	t.Parallel()
	cluster := testnats.StartCluster(t, 3)
	cluster.Servers[1].Stop(t)
	cluster.Servers[2].Stop(t)
	left := cluster.Servers[0].URL()
	probe, err := natsgo.Connect(left)
	if err != nil {
		t.Fatalf("connect to the member left: %v", err)
	}
	t.Cleanup(probe.Close)
	js, err := probe.JetStream()
	if err != nil {
		t.Fatalf("open JetStream on the member left: %v", err)
	}
	waitFor(t, "the member left to answer 10008", func() bool {
		_, err := js.AccountInfo()
		code, ok := bus.ClusterStateAnswer(err)
		return ok && code == int(natsgo.JSErrCodeJetStreamNotAvailable)
	})
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), left, "box-unavailable", "ENVOY_INSTANCE_ID=pod-unavailable")
	const while = "its JetStream had no meta leader"
	answers := func(level string) []map[string]any {
		return linesIn(listener.Output.String(), clusterStateLine,
			map[string]any{"level": level, "stream": bus.Stream, "code": float64(natsgo.JSErrCodeJetStreamNotAvailable)})
	}
	for deadline := time.Now().Add(100 * time.Second); len(answers("ERROR")) < 2; time.Sleep(time.Second) {
		requireLiveAndStarting(t, listener, while)
		if time.Now().After(deadline) {
			t.Fatalf("the listener logged %d WARN and %d ERROR answers of 10008 within 100 s, want five WARN and then two ERROR:\n%s",
				len(answers("WARN")), len(answers("ERROR")), listener.Output.String())
		}
	}
	warned, erred := answers("WARN"), answers("ERROR")
	if all := linesIn(listener.Output.String(), clusterStateLine, nil); len(all) != len(warned)+len(erred) {
		t.Fatalf("the listener logged %d %q lines, %d of them WARN or ERROR of 10008 on %s, want each such:\n%s",
			len(all), clusterStateLine, len(warned)+len(erred), bus.Stream, listener.Output.String())
	}
	waits := []string{"1s", "2s", "4s", "8s", "16s"}
	if len(warned) != len(waits) {
		t.Fatalf("the listener logged %d WARN answers of 10008 before its ERRORs, want %d, one per try in the first 30 s:\n%s",
			len(warned), len(waits), listener.Output.String())
	}
	first := recordTime(t, warned[0])
	for index, wait := range waits {
		if record := warned[index]; record["retry_in"] != wait || recordTime(t, record).Sub(first) >= clusterUnavailableWarnFor {
			t.Fatalf("WARN answer %d = %v, want retry_in %s and less than %s after the first", index+1, record, wait, clusterUnavailableWarnFor)
		}
	}
	for index, record := range erred[:2] {
		if since := recordTime(t, record).Sub(first); record["retry_in"] != "30s" || since < clusterUnavailableWarnFor {
			t.Fatalf("ERROR answer %d = %v, %s after the first answer, want retry_in 30s and %s or more after it", index+1, record, since, clusterUnavailableWarnFor)
		}
	}
	requireLiveAndStarting(t, listener, while)
}

// recordTime is when the listener logged record, its ts field.
func recordTime(t *testing.T, record map[string]any) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(record["ts"]))
	if err != nil {
		t.Fatalf("the record %v carries no ts the listener writes: %v", record, err)
	}
	return at
}

// serverSubscriptions is every subject a client connection of server subscribes, as its /connz
// lists them.
func serverSubscriptions(t *testing.T, server *testnats.Server) []string {
	t.Helper()
	response, err := http.Get(server.MonitorURL() + "/connz?subs=1")
	if err != nil {
		t.Fatalf("read the server's /connz: %v", err)
	}
	defer response.Body.Close()
	var connz struct {
		Connections []struct {
			Subscriptions []string `json:"subscriptions_list"`
		} `json:"connections"`
	}
	if err := json.NewDecoder(response.Body).Decode(&connz); err != nil {
		t.Fatalf("decode the server's /connz: %v", err)
	}
	var subjects []string
	for _, connection := range connz.Connections {
		subjects = append(subjects, connection.Subscriptions...)
	}
	return subjects
}

// startGate stands between a listener and its NATS server and relays every connection, one NATS
// protocol operation at a time, except that it closes at once, before the server reads it, each
// connection whose CONNECT names a client it refuses (refuse, admit), as a link that refuses some
// connections and lets others through does; that it reports in each of the server's INFOs the
// release reportRelease names, as an older server does; and that it answers a subscription
// refuseSubscriptions names as a server whose grant does not carry it answers, with its
// violation, and passes it no further.
type startGate struct {
	listener net.Listener
	target   string
	url      string

	mu       sync.Mutex
	refusing map[string]bool
	refusals map[string]int
	release  string
	// subscriptions refuses, for the connections a client name keys, each subscription whose
	// subject it reports true for.
	subscriptions map[string]func(subject string) bool
	links         []net.Conn
	closed        bool
}

// connectName is the client name a CONNECT line carries.
var connectName = regexp.MustCompile(`"name":"([^"]*)"`)

// infoVersion is the release an INFO line carries.
var infoVersion = regexp.MustCompile(`"version":"[^"]*"`)

func startStartGate(t *testing.T, uri string) *startGate {
	t.Helper()
	server, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse NATS URL %q: %v", uri, err)
	}
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen for the relay: %v", err)
	}
	relayed := *server
	relayed.Host = listener.Addr().String()
	gate := &startGate{listener: listener, target: server.Host, url: relayed.String(), refusing: map[string]bool{},
		refusals: map[string]int{}, subscriptions: map[string]func(string) bool{}}
	t.Cleanup(gate.close)
	go gate.serve()
	return gate
}

// refuse has the relay close each later connection named one of names.
func (g *startGate) refuse(names ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, name := range names {
		g.refusing[name] = true
	}
}

// admit lets the later connections named name through.
func (g *startGate) admit(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.refusing, name)
}

// refused is how many connections named name the relay has closed.
func (g *startGate) refused(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refusals[name]
}

// reportRelease has the relay report release in the INFO of each later connection, or the
// server's own when release is empty.
func (g *startGate) reportRelease(release string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.release = release
}

// refuseSubscriptions has the relay refuse, on each later connection named name, every
// subscription whose subject refused reports true for; nil refuses none.
func (g *startGate) refuseSubscriptions(name string, refused func(subject string) bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if refused == nil {
		delete(g.subscriptions, name)
		return
	}
	g.subscriptions[name] = refused
}

func (g *startGate) serve() {
	for {
		client, err := g.listener.Accept()
		if err != nil {
			return
		}
		go g.relay(client)
	}
}

// track keeps conn for close, or reports false once the relay is closed.
func (g *startGate) track(conn net.Conn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.links = append(g.links, conn)
	return true
}

// readProtocolUnit reads one NATS protocol operation from r: its line and, for an operation that
// carries a payload (MSG, HMSG, PUB, HPUB), the payload and the CRLF after it, whose size the
// line ends with.
func readProtocolUnit(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return line, nil
	}
	switch strings.ToUpper(fields[0]) {
	case "MSG", "HMSG", "PUB", "HPUB":
		size, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			return "", err
		}
		payload := make([]byte, size+2)
		if _, err := io.ReadFull(r, payload); err != nil {
			return "", err
		}
		return line + string(payload), nil
	}
	return line, nil
}

// relay relays one connection. The server sends an INFO as the client connects and more after
// it, one with the client's own details once it has taken the CONNECT among them, and nats.go
// reports the version of the last it read, so the relay rewrites each.
func (g *startGate) relay(client net.Conn) {
	defer func() { _ = client.Close() }()
	if !g.track(client) {
		return
	}
	server, err := net.Dial("tcp", g.target)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()
	if !g.track(server) {
		return
	}
	g.mu.Lock()
	release := g.release
	g.mu.Unlock()
	fromServer, fromClient := bufio.NewReader(server), bufio.NewReader(client)
	// Both directions write to the client, the server's operations and the relay's refusals, so
	// each write is one whole operation.
	var toClient sync.Mutex
	send := func(unit string) bool {
		if release != "" && strings.HasPrefix(unit, "INFO ") {
			unit = infoVersion.ReplaceAllLiteralString(unit, `"version":"`+release+`"`)
		}
		toClient.Lock()
		defer toClient.Unlock()
		_, err := io.WriteString(client, unit)
		return err == nil
	}
	info, err := readProtocolUnit(fromServer)
	if err != nil || !send(info) {
		return
	}
	connect, err := readProtocolUnit(fromClient)
	if err != nil {
		return
	}
	name := ""
	if match := connectName.FindStringSubmatch(connect); match != nil {
		name = match[1]
	}
	g.mu.Lock()
	refused, refusesSubscription := g.refusing[name], g.subscriptions[name]
	if refused {
		g.refusals[name]++
	}
	g.mu.Unlock()
	if refused {
		return
	}
	if _, err := io.WriteString(server, connect); err != nil {
		return
	}
	go func() {
		defer func() { _ = server.Close() }()
		for {
			unit, err := readProtocolUnit(fromClient)
			if err != nil {
				return
			}
			if fields := strings.Fields(unit); refusesSubscription != nil && len(fields) >= 3 && fields[0] == "SUB" && refusesSubscription(fields[1]) {
				if !send(fmt.Sprintf("-ERR 'Permissions Violation for Subscription to %q'\r\n", fields[1])) {
					return
				}
				continue
			}
			if _, err := io.WriteString(server, unit); err != nil {
				return
			}
		}
	}()
	for {
		unit, err := readProtocolUnit(fromServer)
		if err != nil || !send(unit) {
			return
		}
	}
}

func (g *startGate) close() {
	g.mu.Lock()
	g.closed = true
	links := g.links
	g.links = nil
	g.mu.Unlock()
	_ = g.listener.Close()
	for _, link := range links {
		_ = link.Close()
	}
}

// Every pod of one machine id runs the role lane, the active pod and each standby alike, so both
// subscribe notifications.role.> at once. They are members of one core-NATS queue group
// (envoy-roles, which every listener shares, or this machine's group on a hub whose grant
// predates it), and NATS hands each message to one member of a group, so a role message published
// on the lane reaches its holder once whichever pod takes it: no publish is forwarded by both. A
// re-publish under the same dedupe key that lands on the other pod finds the forward recorded in
// the delivery ledger both pods share and is skipped; a copy that does reach the holder is
// dropped by its pump by its dedupe key, as it drops the copy each other machine's listener
// forwards. The messages go straight onto the lane, as an operator's or an earlier listener's do:
// the publish route forwards a role message itself.
func TestARoleMessageReachesItsHolderOnceAcrossTwoTasksOfOneMachine(t *testing.T) {
	t.Parallel()
	const (
		machineID = "two-tasks"
		holderID  = "ses_two_tasks_holder"
		role      = "two-tasks-role"
		messages  = 64
	)
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect bus: %v", err)
	}
	t.Cleanup(client.Close)
	binary := cmdtest.Build(t, "envoy-listener")
	uri := client.Conn.ConnectedUrl()
	old := startListenerProcess(t, binary, uri, machineID, "ENVOY_INSTANCE_ID=task-old")
	old.waitHealthy(t)
	replacement := startListenerProcess(t, binary, uri, machineID, "ENVOY_INSTANCE_ID=task-new")
	replacement.waitStandby(t, "task-old")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("old task:\n%s\nreplacement:\n%s", old.Output.String(), replacement.Output.String())
		}
	})

	postListener(t, replacement.port, "/v1/interests/subscribe", consumerRegistration(holderID, "epoch-1"))
	postListener(t, replacement.port, "/v1/roles/set", `{"session_id":"`+holderID+`","role":"`+role+`"}`)
	frames := receiveAsSession(t, client, holderID)
	// The old task reads the claim from the role bucket and the holder from its session cache, which
	// can trail the bucket and not yet hold the session the replacement just registered; it then reads
	// the holder from the session bucket itself (liveSessionEntry), so its first lookup answers the
	// claim rather than releasing it as lapsed.
	if status, answer := callListener(t, old.port, http.MethodGet, "/v1/roles/"+role, ""); status != http.StatusOK {
		t.Fatalf("the old task's lookup of %s right after the replacement accepted %s's claim: status %d %s, want 200", role, holderID, status, answer)
	}

	publish := func(dedupeKey string) contracts.Envelope {
		t.Helper()
		item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, dedupeKey)
		item.SourceSession = "ses_two_tasks_sender"
		if err := client.Publish(item); err != nil {
			t.Fatalf("publish %s onto the role lane: %v", dedupeKey, err)
		}
		return item
	}
	for index := range messages {
		publish("publish.two-tasks-" + strconv.Itoa(index))
	}
	keys := map[string]int{}
	for received := range messages {
		select {
		case frame := <-frames:
			if !strings.HasPrefix(frame.DedupeKey, roleForwardDedupePrefix) {
				t.Fatalf("the holder received dedupe key %q, want one prefixed %s", frame.DedupeKey, roleForwardDedupePrefix)
			}
			keys[frame.DedupeKey]++
		case <-time.After(listenerWaitBound(t)):
			t.Fatalf("the holder received %d of %d role messages", received, messages)
		}
	}
	select {
	case frame := <-frames:
		t.Fatalf("the holder received a frame past the %d published: %+v", messages, frame)
	// A quiet window: the second this waits is what the row asserts, that no frame comes past the
	// ones published, so it keeps its own span.
	case <-time.After(time.Second):
	}
	if len(keys) != messages {
		t.Fatalf("the holder received %d distinct dedupe keys over %d frames, want %d: a publish was forwarded twice", len(keys), messages, messages)
	}
	roleForwards := func(p *listenerProcess) int {
		return countRecords(p, "listener role forwarded", func(record map[string]any) bool {
			return record["topic"] == contracts.RoleTopicPrefix+role
		})
	}
	byOld, byReplacement := roleForwards(old), roleForwards(replacement)
	if byOld+byReplacement != messages {
		t.Fatalf("the two tasks logged %d + %d role forwards, want %d in all", byOld, byReplacement, messages)
	}
	// NATS spreads a group's messages over its members; all 64 on one has probability 2^-63.
	if byOld == 0 || byReplacement == 0 {
		t.Fatalf("the active pod forwarded %d and the standby %d of %d: the standby's role lane is not a member of the machine's queue group", byOld, byReplacement, messages)
	}

	// A re-publish under one dedupe key: either the task that forwarded the first copy skips it,
	// or the other task forwards it again, for the holder's pump to drop.
	const repeatKey = "two-tasks-repeat"
	events := map[string]bool{}
	for range 2 {
		published := publish(repeatKey)
		events[published.EventID] = true
	}
	ofRepeat := func(record map[string]any) bool { return events[record["event_id"].(string)] }
	handled := func() int {
		total := 0
		for _, p := range []*listenerProcess{old, replacement} {
			total += countRecords(p, "listener role forwarded", ofRepeat) + countRecords(p, "listener role dedupe skip", ofRepeat)
		}
		return total
	}
	waitFor(t, "the two tasks to handle the repeated key twice", func() bool { return handled() == 2 })
	repeats := 0
	for collecting := true; collecting; {
		select {
		case frame := <-frames:
			if frame.DedupeKey != roleForwardDedupePrefix+repeatKey {
				t.Fatalf("the holder received dedupe key %q, want %s", frame.DedupeKey, roleForwardDedupePrefix+repeatKey)
			}
			repeats++
		case <-time.After(time.Second):
			collecting = false
		}
	}
	if repeats != 1 && repeats != 2 {
		t.Fatalf("the holder received %d frames for the repeated key, want 1 or 2", repeats)
	}

	old.Terminate(t)
	old.WaitExit(t, "SIGTERM")
	replacement.waitHealthy(t)
	replacement.WaitForOutput(t, "durable receiving")
}

// receiveAsSession stands in for session sessionID's agent pump on its agent subject: it hands
// each frame's envelope to the channel and answers nothing, as the pump answers nothing.
func receiveAsSession(t *testing.T, client *bus.Client, sessionID string) <-chan contracts.Envelope {
	t.Helper()
	frames := make(chan contracts.Envelope, 256)
	subscription, err := client.Conn.Subscribe(contracts.LegacyAgentSubject(sessionID), func(msg *natsgo.Msg) {
		var envelope contracts.Envelope
		_ = json.Unmarshal(msg.Data, &envelope)
		frames <- envelope
	})
	if err != nil {
		t.Fatalf("subscribe as %s: %v", sessionID, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscription as %s: %v", sessionID, err)
	}
	return frames
}

// callListener calls a listener /v1 route as the test's shared-token caller and returns its status
// and body.
func callListener(t *testing.T, port int, method, path, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(method, "http://127.0.0.1:"+strconv.Itoa(port)+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	request.Header.Set("Authorization", "Bearer "+listenerTestToken)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	return do(t, request)
}

// countRecords counts the JSON records in the listener's output whose msg is name and that match
// accepts.
func countRecords(p *listenerProcess, name string, match func(map[string]any) bool) int {
	count := 0
	for line := range strings.SplitSeq(p.Output.String(), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil || record["msg"] != name {
			continue
		}
		if match(record) {
			count++
		}
	}
	return count
}

// A listener built before the KV key check stored role claims, interests, sessions and CI records
// under keys this one refuses to write (testnats.LegacyKeys), and every listener on that NATS reads
// the first three buckets as it starts, and the CI bucket when it mounts the GitHub webhook route.
// It must start healthy over them and serve the ordinary keys beside them: one stored role must not
// keep every listener from starting.
func TestTheListenerStartsOverKeysAnEarlierListenerStored(t *testing.T) {
	t.Parallel()
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect bus: %v", err)
	}
	t.Cleanup(client.Close)
	bucket := func(name string, ttl time.Duration) natsgo.KeyValue {
		t.Helper()
		kv, err := client.JS().KeyValue(name)
		if errors.Is(err, natsgo.ErrBucketNotFound) {
			kv, err = client.JS().CreateKeyValue(&natsgo.KeyValueConfig{Bucket: name, TTL: ttl, Storage: natsgo.FileStorage})
		}
		if err != nil {
			t.Fatalf("open bucket %s: %v", name, err)
		}
		return kv
	}
	put := func(kv natsgo.KeyValue, key string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("encode %s value: %v", kv.Bucket(), err)
		}
		if _, err := kv.Put(key, data); err != nil {
			t.Fatalf("store the %d-byte %s key: %v", len(key), kv.Bucket(), err)
		}
	}
	now := time.Now().UnixMilli()
	roles, interests := bucket(bus.RoleClaimBucket, 0), bucket(bus.InterestBucket, 0)
	sessions, records := bucket(bus.SessionBucket, 5*time.Minute), bucket(bus.CIStateBucket, 0)
	put(roles, "reviewer", store.RoleClaim{HolderSessionID: "ses_live", ClaimedAt: now})
	put(interests, "ses_live", store.Interest{SessionID: "ses_live", MachineID: "startup-legacy", Topics: []string{contracts.LegacyAgentSubject("ses_live")}, UpdatedAt: now})
	// Written straight to the bucket as an earlier build's listener wrote it, serving port and
	// all: no registration carries a port since dispatch://INFRA-889, and this test is about
	// what a listener makes of the records it finds on its start.
	put(sessions, "ses_live", session.SessionEntry{Port: 1, MachineID: "startup-legacy", UpdatedAt: now})
	for _, kv := range []natsgo.KeyValue{roles, interests, sessions, records} {
		readable, unreadable := testnats.LegacyKeys(kv.Bucket(), "k")
		for _, key := range []string{readable, unreadable} {
			switch kv {
			case roles:
				put(kv, key, store.RoleClaim{HolderSessionID: "ses_gone", ClaimedAt: now})
			case interests:
				put(kv, key, store.Interest{SessionID: key, MachineID: "earlier", UpdatedAt: now})
			case sessions:
				put(kv, key, session.SessionEntry{Port: 2, MachineID: "earlier", UpdatedAt: now})
			default:
				put(kv, key, cistore.State{Owner: "acme", Repo: "widgets", Number: "7", SHA: strings.Repeat("c", 40)})
			}
		}
	}

	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), client.Conn.ConnectedUrl(), "startup-legacy", githubWebhookEnv...)
	listener.waitHealthy(t)
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/v1/roles/reviewer", nil)
	if err != nil {
		t.Fatalf("role request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+listenerTestToken)
	if status, answer := do(t, request); status != http.StatusOK || !strings.Contains(answer, `"holder":"ses_live"`) {
		t.Fatalf("GET /v1/roles/reviewer beside the earlier listener's keys: status %d %q, want 200 naming ses_live\n%s", status, answer, listener.Output.String())
	}
}

// githubWebhookEnv mounts the listener's GitHub webhook route, the one route that uses the CI store.
var githubWebhookEnv = []string{"ENVOY_WEBHOOKS=github", "ENVOY_GITHUB_WEBHOOK_SECRET=ci-route-secret", "ENVOY_REVIEWER_APP_ID=1"}

// seedDueCIRecord creates the CI bucket as the listener creates it and stores one commit of
// owner/repo#number at sha whose checks finished a minute ago and have not settled, so any
// summary loop reading the bucket settles it at its first pass after the record reaches its
// cache, on the next tick of the loop's grid. It returns the bucket and the record's key.
func seedDueCIRecord(t *testing.T, client *bus.Client, owner, repo, number, sha string) (natsgo.KeyValue, string) {
	t.Helper()
	kv, err := client.JS().CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.CIStateBucket, Replicas: 1, Storage: natsgo.FileStorage, TTL: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("create the CI bucket: %v", err)
	}
	key := cistore.Key(owner, repo, number, sha)
	record := `{"owner":"` + owner + `","repo":"` + repo + `","number":"` + number + `","sha":"` + sha + `",` +
		`"checks":{"build":{"name":"build","check_run_id":42,"url":"https://github.com/` + owner + `/` + repo + `/runs/42","status":"completed","conclusion":"success","observed_at":"2026-09-28T13:00:00Z"}},` +
		`"suites":{},"last_event_at":` + strconv.FormatInt(time.Now().Add(-time.Minute).UnixMilli(), 10) + `,` +
		`"generation":1,"emitted_count":0,"settled_emitted":false,"schema":1}`
	if _, err := kv.Put(key, []byte(record)); err != nil {
		t.Fatalf("store the CI record of %s/%s#%s: %v", owner, repo, number, err)
	}
	return kv, key
}

// ciChecksSubject is the settlement topic of seedDueCIRecord's commit.
var ciChecksSubject = contracts.GithubSubject("acme", "widgets", "pr.7.checks")

// ciSettled reports whether the stream holds a settlement of seedDueCIRecord's commit.
func ciSettled(t *testing.T, client *bus.Client) bool {
	t.Helper()
	_, err := client.JS().GetLastMsg(bus.Stream, ciChecksSubject)
	if errors.Is(err, natsgo.ErrMsgNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("read the stream's last %s: %v", ciChecksSubject, err)
	}
	return true
}

// A listener that mounts no GitHub webhook route receives no check webhook, so it has nothing to
// record in the CI bucket and leaves it alone: no watch (whose initial scan of every record, 62 MB
// in production, is what a listener reaching NATS over a relayed link cannot take), no summary loop
// (the listeners that receive GitHub webhooks settle every record), and a /healthz that calls the CI
// cache not applicable instead of waiting on it. Seeded with a record any summary loop would settle,
// it creates no consumer on the CI bucket's stream, publishes no settlement, and reports healthy.
func TestAListenerWithoutAGitHubRouteLeavesTheCIBucketAlone(t *testing.T) {
	t.Parallel()
	client := setupTestNATS(t)
	kv, key := seedDueCIRecord(t, client, "acme", "widgets", "7", strings.Repeat("d", 40))

	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), client.Conn.ConnectedUrl(), "no-github-route")
	listener.waitHealthy(t)
	// The positive control, TestAListenerWithAGitHubRouteSettlesTheCIBucket, settles this seed
	// within a few one-second ticks of turning healthy; five seconds covers them. This loop's only
	// exit is its deadline, so what it proves is an absence and the five seconds are the row's
	// subject: it keeps its own span, as the quiet windows do.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		consumers := 0
		for range client.JS().ConsumerNames("KV_" + bus.CIStateBucket) {
			consumers++
		}
		if consumers != 0 {
			t.Fatalf("the CI bucket's stream has %d consumers under a listener with no GitHub route, want 0: it watches the bucket\n%s", consumers, listener.Output.String())
		}
		if ciSettled(t, client) {
			t.Fatalf("a listener with no GitHub route published a settlement on %s: it runs the CI summary loop\n%s", ciChecksSubject, listener.Output.String())
		}
	}
	entry, err := kv.Get(key)
	if err != nil {
		t.Fatalf("read the seeded CI record: %v", err)
	}
	if entry.Revision() != 1 {
		t.Fatalf("the seeded CI record is at revision %d, want 1: a listener with no GitHub route wrote it", entry.Revision())
	}

	status, health := do(t, mustRequest(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/healthz"))
	if status != http.StatusOK || !strings.Contains(health, `"status":"healthy"`) || !strings.Contains(health, `"ci_cache":"not_applicable"`) {
		t.Fatalf("/healthz under a listener with no GitHub route: %d %s, want 200 healthy with ci_cache not_applicable", status, health)
	}
	if _, metrics := do(t, mustRequest(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/metrics")); strings.Contains(metrics, "envoy_ci_legacy_records_held") {
		t.Fatalf("/metrics under a listener with no GitHub route carries envoy_ci_legacy_records_held, which only a summary loop sets:\n%s", metrics)
	}
}

// A listener that mounts the GitHub webhook route opens the CI store and runs the summary loop, so
// the record seedDueCIRecord leaves settles: the settlement reaches the stream and the record is
// marked settled. It is the positive control for the test above.
func TestAListenerWithAGitHubRouteSettlesTheCIBucket(t *testing.T) {
	t.Parallel()
	client := setupTestNATS(t)
	kv, key := seedDueCIRecord(t, client, "acme", "widgets", "7", strings.Repeat("d", 40))

	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), client.Conn.ConnectedUrl(), "github-route", githubWebhookEnv...)
	listener.waitHealthy(t)
	healthy := time.Now()
	for !ciSettled(t, client) {
		if time.Since(healthy) > 15*time.Second {
			t.Fatalf("a listener with the GitHub route published no settlement on %s within 15s of turning healthy\n%s", ciChecksSubject, listener.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("settled %s after turning healthy", time.Since(healthy).Round(10*time.Millisecond))
	for deadline := time.Now().Add(listenerWaitBound(t)); ; time.Sleep(100 * time.Millisecond) {
		entry, err := kv.Get(key)
		if err != nil {
			t.Fatalf("read the seeded CI record: %v", err)
		}
		var state cistore.State
		if err := json.Unmarshal(entry.Value(), &state); err != nil {
			t.Fatalf("decode the seeded CI record: %v", err)
		}
		if state.SettledEmitted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the settled CI record is not marked settled: %s", entry.Value())
		}
	}
	if _, health := do(t, mustRequest(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/healthz")); strings.Contains(health, "ci_cache") {
		t.Fatalf("/healthz under a listener with the GitHub route: %s, want no ci_cache field, as before", health)
	}
}

// mustRequest builds an unauthenticated request.
func mustRequest(t *testing.T, method, url string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return request
}

// do sends request and returns its status and body.
func do(t *testing.T, request *http.Request) (int, string) {
	t.Helper()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", request.Method, request.URL.Path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("%s %s: read the answer: %v", request.Method, request.URL.Path, err)
	}
	return response.StatusCode, string(answer)
}

// Two packages the listener calls into log two different ways, and both are load-bearing.
// internal/kvwatch's lines, which internal/store's and internal/session's caches log through the
// logger the listener hands them, exist for the SRE to query a restart (every cache's warm-up
// cost), so they go through the listener's own handler and
// arrive as JSON records with this machine's id. internal/bus's lines, and the stdlib log
// package's, must stay in Go's text format: the deployed CloudWatch metric filters for publish
// failures, webhook refusals and dropped stream subjects are space-delimited patterns anchored on
// that format's date and time prefix (defined in the deployment repository's listener infrastructure), so
// routing them into the JSON handler with slog.SetDefault stops three alarms without failing
// anything. This test holds both halves, so reintroducing that SetDefault reds it. Both halves
// also name the process (ENVOY_INSTANCE_ID): the JSON records in an instance field, the bus's text
// lines in their client name, with no word between the date and time and the message, since the
// text filters match by position. The listener mounts the GitHub route, so all three caches warm up.
func TestListenerLogsFromOtherPackagesKeepTheirHandlers(t *testing.T) {
	t.Parallel()
	const machineID = "logger-split"
	const instance = "logger-split-pod"
	server, uri := testnats.Start(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, machineID,
		append([]string{"ENVOY_INSTANCE_ID=" + instance}, githubWebhookEnv...)...)
	listener.waitHealthy(t)

	// Every cache's warm-up line, which names what a restart spent before it served. The CI
	// cache's readiness is not on the healthy path, so its line can arrive just after.
	for _, cache := range []string{"interest registry", "session registry", "cistore"} {
		msg := cache + " cache warm-up"
		warmUp := listenerJSONRecord(t, listener, msg)
		for deadline := time.Now().Add(listenerWaitBound(t)); warmUp == nil && time.Now().Before(deadline); {
			time.Sleep(100 * time.Millisecond)
			warmUp = listenerJSONRecord(t, listener, msg)
		}
		if warmUp == nil {
			t.Fatalf("internal/kvwatch's %q line is not a JSON record in the listener's output:\n%s", msg, listener.Output.String())
		}
		if warmUp["machine_id"] != machineID || warmUp["instance"] != instance {
			t.Fatalf("the %q record's machine_id, instance = %v, %v, want %q, %q", msg, warmUp["machine_id"], warmUp["instance"], machineID, instance)
		}
	}

	// internal/bus logs its connection lines through the default logger. Taking the server away is
	// how this test gets one to read.
	server.Stop(t)
	textLine := regexp.MustCompile(`(?m)^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} .*envoy nats disconnected`)
	deadline := time.Now().Add(listenerWaitBound(t))
	for !textLine.MatchString(listener.Output.String()) {
		if time.Now().After(deadline) {
			if listenerJSONRecord(t, listener, "envoy nats disconnected") != nil {
				t.Fatalf("internal/bus's disconnect line is a JSON record: the default logger was replaced, and the deployed text metric filters for publish failures, webhook refusals and dropped stream subjects no longer match:\n%s", listener.Output.String())
			}
			t.Fatalf("no disconnect line in Go's text format after the server stopped:\n%s", listener.Output.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	// The text line keeps its shape (date, time, then the message), since the deployed filters are
	// positional; the process is named there by its NATS client name.
	instanceLine := regexp.MustCompile(`(?m)^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} INFO envoy nats disconnected name="envoy-listener ` + instance + `"`)
	if !instanceLine.MatchString(listener.Output.String()) {
		t.Fatalf("internal/bus's disconnect line is not date, time, message, or does not name the client envoy-listener %s:\n%s", instance, listener.Output.String())
	}
}

// listenerJSONRecord is the first line of the listener's output that parses as a JSON object whose
// msg is name, or nil when no line does.
func listenerJSONRecord(t *testing.T, p *listenerProcess, name string) map[string]any {
	t.Helper()
	for line := range strings.SplitSeq(p.Output.String(), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		if record["msg"] == name {
			return record
		}
	}
	return nil
}

// bucketGrant is the publish grant a NATS user needs to open a KV bucket, watch it and write it.
func bucketGrant(bucket string) []string {
	stream := "KV_" + bucket
	return []string{
		"$JS.API.STREAM.INFO." + stream,
		"$JS.API.STREAM.CREATE." + stream,
		"$JS.API.DIRECT.GET." + stream + ".>",
		"$JS.API.STREAM.MSG.GET." + stream,
		"$KV." + bucket + ".>",
		"$JS.API.CONSUMER.CREATE." + stream + ".>",
		"$JS.API.CONSUMER.DELETE." + stream + ".>",
		"$JS.FC." + stream + ".>",
	}
}

// grantBeforeKnownSessions is the grant of a listener whose hub's grant does not carry the
// known-session bucket: every bucket the listener opens (the legion_agents bucket, whose denied
// open would cost the start its own 10-second JetStream wait, and the spent-proof bucket every
// listener opens at start, included) except the known-session one.
func grantBeforeKnownSessions() []string {
	grant := append(listenerGrantBeforeSessionConsumers(), bucketGrant(bus.LegionAgentBucket)...)
	return append(grant, bucketGrant(bus.DPoPProofBucket)...)
}

// v1OpenWithoutTheBucket is how long after it began listening a listener whose NATS user may not
// open the known-session bucket may take to open /v1. Every caller through the port is refused
// 503 "service starting" until it does, and a listener runs this way on any hub whose grant does
// not carry the bucket, so the start must not wait the bucket out: a denied JetStream request is
// dropped rather than refused, so waiting it out costs the whole JetStream wait. A healthy
// listener opens /v1 in tens of milliseconds; this bound is well above one attempt's wait
// (session.OpenKnownSessions) and well below the 10 seconds the default wait cost.
const v1OpenWithoutTheBucket = 5000

// A listener whose NATS user may not open the known-session bucket opens /v1 without waiting the
// bucket out, keeps running, and opens the bucket itself once the grant arrives - the order
// production applies them in - with no restart.
func TestTheListenerOpensV1AndPicksUpTheBucketItMayNotOpenYet(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv of
	// NATS_NKEY_SEED).
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, grantBeforeKnownSessions()...)
	t.Setenv("NATS_NKEY_SEED", seed)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), grant.URL, "test-machine", "NATS_NKEY_SEED="+seed)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the listener's output:\n%s", listener.Output.String())
		}
	})

	listener.WaitForOutput(t, "envoy-listener /v1 open")
	record := listenerJSONRecord(t, listener, "envoy-listener /v1 open")
	if record == nil {
		t.Fatalf("the listener logged no JSON record for its /v1 open:\n%s", listener.Output.String())
	}
	opened, ok := record["since_listening_ms"].(float64)
	if !ok {
		t.Fatalf("the /v1 open record %v carries no since_listening_ms", record)
	}
	if opened > v1OpenWithoutTheBucket {
		t.Fatalf("/v1 opened %.0f ms after this listener began listening, want within %d: every caller is refused 503 service starting until it does",
			opened, v1OpenWithoutTheBucket)
	}
	t.Logf("/v1 opened %.0f ms after the listener began listening, over a bucket its user may not open", opened)
	if !strings.Contains(listener.Output.String(), bus.KnownSessionBucket) {
		t.Fatalf("the listener never named the bucket it could not open, so this test proved nothing about it:\n%s", listener.Output.String())
	}

	grant.Grant(t, append(grantBeforeKnownSessions(), bucketGrant(bus.KnownSessionBucket)...)...)
	listener.WaitForOutput(t, "the known-session bucket is open")
}
