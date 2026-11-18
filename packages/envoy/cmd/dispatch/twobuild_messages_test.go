//go:build twobuild

// The bus half of the two-build suite: the messages and subscription settings the two builds pass
// and reconcile while both serve one NATS (twobuild_harness_test.go holds the harness both halves
// start their builds with). It also holds the attribution row: both builds name one issue for a
// pull request from one set of facts, through a backlog and a stop of this tree's build.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// ───── messages and subscription settings ───────────────────────────────────

// settingsPolls is two of the delivery intake's settings polls (delivery.settingsPollInterval,
// 30 s, unexported) and a margin: the window a standby of the build before generations compares
// the consumers' settings with its own, twice.
const settingsPolls = 2*30*time.Second + 5*time.Second

// consumerGenerationKey is the consumer metadata key this tree stamps its generation under
// (delivery.consumerGenerationKey, unexported).
const consumerGenerationKey = "dispatch.consumer-generation"

// consumerSettings is what a delivery durable's settings must keep while two builds run.
type consumerSettings struct {
	FilterSubject string
	AckWait       time.Duration
	MaxAckPending int
	Generation    string
}

// streamSettings is what the notification stream's settings must keep while two builds run.
type streamSettings struct {
	MaxAge     time.Duration
	Duplicates time.Duration
	Subjects   []string
	Replicas   int
}

type busSettings struct {
	Consumers map[string]consumerSettings
	Stream    streamSettings
}

func readBusSettings(t *testing.T, js natsgo.JetStreamContext) busSettings {
	t.Helper()
	settings := busSettings{Consumers: map[string]consumerSettings{}}
	for _, durable := range deliveryDurables {
		info, err := js.ConsumerInfo(bus.Stream, durable)
		if err != nil {
			t.Fatalf("read the durable %s: %v", durable, err)
		}
		settings.Consumers[durable] = consumerSettings{
			FilterSubject: info.Config.FilterSubject,
			AckWait:       info.Config.AckWait,
			MaxAckPending: info.Config.MaxAckPending,
			Generation:    info.Config.Metadata[consumerGenerationKey],
		}
	}
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the notification stream: %v", err)
	}
	settings.Stream = streamSettings{
		MaxAge:     info.Config.MaxAge,
		Duplicates: info.Config.Duplicates,
		Subjects:   slices.Clone(info.Config.Subjects),
		Replicas:   info.Config.Replicas,
	}
	return settings
}

func requireBusSettings(t *testing.T, js natsgo.JetStreamContext, want busSettings, when string) {
	t.Helper()
	if got := readBusSettings(t, js); fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Errorf("%s, the bus settings are\n%+v\nwant them unchanged from\n%+v", when, got, want)
	}
}

// pullRequestFake is the loopback GitHub both builds' intakes call while they handle a merged pull
// request: the App's installation and token, the pull request, and its first commit. It is one
// state served by one server per build (serve), so each handling is recorded with the build that
// did it.
//
// A handling is counted at the pull request's own fetch, GET /repos/{owner}/{repo}/pulls/{number}
// (delivery.FetchPullRequest), as the multiprocess suite's mergedPullRequestFake counts one: the
// merge path's first GitHub call after its head write (recordPullRequestHead pages the commits
// before it), and the one call it makes exactly once however the handling then ends. The commit
// listing is counted by neither suite, and a merged pull request whose
// payload names its head is paged twice for it -- once to attribute the head row, once for the
// completion's session trailers.
//
// The fake takes delay to answer that listing, which spreads the messages' handling over a stop,
// and a listing whose process went away while it waited (the request's context ended) is counted
// as abandoned. A handling counted is therefore not a handling finished: one a stop gave up
// unfinished is counted at its fetch all the same, and NATS delivers that message again, which
// is one of the repeats a hand-over's in-flight window bounds. Anything else is answered with an
// empty list.
type pullRequestFake struct {
	delay time.Duration

	mu        sync.Mutex
	done      map[int][]handling
	abandoned map[int]int
}

// handling is one done handling of a pull request: the build whose server answered it, and when.
type handling struct {
	build string
	at    time.Time
}

func (h handling) String() string { return h.build + " at " + h.at.Format("15:04:05.000") }

func startPullRequestFake(delay time.Duration) *pullRequestFake {
	return &pullRequestFake{delay: delay, done: map[int][]handling{}, abandoned: map[int]int{}}
}

// serve starts a server on the fake's state whose done handlings are build's, and returns its URL:
// the DISPATCH_GITHUB_API_BASE of build's processes.
func (f *pullRequestFake) serve(t *testing.T, build string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":1,"app_slug":"twobuild","permissions":{"contents":"read","actions":"read","pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":"ghs_twobuild","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		f.done[number] = append(f.done[number], handling{build: build, at: time.Now()})
		f.mu.Unlock()
		// The pull request as GitHub holds it: merged, at the head publishMergedPullRequest names,
		// with its update time. This tree writes its head row from this answer alone, and a row
		// written from an answer that names its head and its update time is complete, stamped read
		// and marked a read of GitHub, so no reconcile pass of either build reads it again.
		_, _ = fmt.Fprintf(w, `{"number":%d,"title":"A pull request","user":{"login":"octocat"},"labels":[],"state":"closed","merged":true,`+
			`"merged_at":"2026-10-27T00:00:00Z","updated_at":"2026-10-27T00:00:00Z","additions":1,"deletions":1,`+
			`"head":{"ref":"a-branch","sha":"%040d","repo":{"full_name":"acme/deploy"}}}`, number, number)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			f.mu.Lock()
			f.abandoned[number]++
			f.mu.Unlock()
			return
		}
		_, _ = io.WriteString(w, `[{"sha":"0000000000000000000000000000000000000001","commit":{"message":"A change.","author":{"date":"2026-10-26T00:00:00Z"},"committer":{"date":"2026-10-26T00:00:00Z"}}}]`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "[]")
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

// handled is each pull request's done handlings, by number, in the order they were done, and how
// many times a stopping process abandoned it.
func (f *pullRequestFake) handled() (done map[int][]handling, abandoned map[int]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	done = make(map[int][]handling, len(f.done))
	for number, handlings := range f.done {
		done[number] = slices.Clone(handlings)
	}
	return done, maps.Clone(f.abandoned)
}

// seedPullRequestSettings writes the delivery settings the intakes route by: acme/deploy as the
// deploy repository and octocat as the one author whose merged pull requests are read.
func seedPullRequestSettings(t *testing.T, builds *twoBuilds) {
	t.Helper()
	if _, err := delivery.PutSettings(context.Background(), builds.database.Pool, delivery.DeliverySettings{
		DeployRepo:           "acme/deploy",
		DeployWorkflowPath:   ".github/workflows/deploy.yaml",
		PRChecksWorkflowPath: ".github/workflows/pr.yaml",
		ProductionJobNames:   []string{"production"},
		PopulationAuthors:    []string{"octocat"},
	}, model.Actor{Kind: "user", ID: dispatchTestLogin}); err != nil {
		t.Fatalf("seed the delivery settings: %v", err)
	}
}

// publishMergedPullRequest publishes a GitHub-shaped message, a merge of pull request number of
// acme/deploy by octocat, on notifications.github.acme.deploy.pr.<number>, with delivery as its
// message id, and answers the sequence the stream stored it at. That sequence is what tells a
// repeat of the message from a replay of settled work: a take-over creates the durable again at
// the ack floor it found, so the messages it delivers again are exactly those above that floor.
//
// The payload names the head -- its repository, its branch and its commit -- as
// publishMergedPullRequests does in multiprocess_test.go, and the fake answers the same head, so
// that handling it once settles the pull request's head row for good in either build: a build
// before this change writes the row from this envelope, and this tree from the fake's
// answer. A reconcile pass would still read some of these pull requests again, through the call
// this row counts as a handling: this tree's merged arm offers every merged row an older rule
// judged once (delivery.AttributionRuleVersion), which is every row the previous build writes. So
// the row runs neither build's delivery reconcile inside it (holdTheDeliveryReconcile).
func publishMergedPullRequest(t *testing.T, js natsgo.JetStreamContext, number int, delivery string) uint64 {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"kind": "pr", "action": "closed", "merged": "true",
		"repo": "acme/deploy", "number": strconv.Itoa(number), "author": "octocat",
		"head_repo": "acme/deploy", "head_ref": "a-branch", "head_sha": fmt.Sprintf("%040d", number),
	})
	if err != nil {
		t.Fatalf("encode the payload: %v", err)
	}
	envelope := contracts.Envelope{
		EventID:       delivery,
		Source:        "github",
		SourceEventID: delivery,
		Topic:         contracts.GithubResourceSubject("acme", "deploy", "pr", strconv.Itoa(number)),
		DedupeKey:     delivery,
		IssuedAt:      time.Now().Unix(),
		Payload:       string(payload),
		TraceID:       delivery,
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode the envelope: %v", err)
	}
	ack, err := js.Publish(envelope.Topic, data, natsgo.MsgId(delivery))
	if err != nil {
		t.Fatalf("publish %s: %v", envelope.Topic, err)
	}
	return ack.Sequence
}

// holdTheDeliveryReconcile holds the delivery reconcile's lease (delivery.ReconcileLease) on the
// builds' database from before either build starts until the row ends, so neither build runs a
// reconcile pass inside the row: every pass of either build takes that lease first
// (duty.Exclusive) and skips where another session holds it. The lease is taken with no period,
// as the reconcile's own lease test takes it (delivery/reconcile_lease_test.go). No process of the
// row runs yet, so a lease held elsewhere fails the row rather than being waited out.
func holdTheDeliveryReconcile(t *testing.T, builds *twoBuilds) {
	t.Helper()
	leases := storetest.Leases(t, builds.database.Pool)
	taken, release, ran := make(chan struct{}), make(chan struct{}), make(chan bool, 1)
	go func() {
		ran <- duty.Exclusive(context.Background(), leases, delivery.ReconcileLease, time.Time{}, 0, func(context.Context) {
			close(taken)
			<-release
		})
	}()
	select {
	case <-taken:
	case <-ran:
		t.Fatalf("the %s lease was held elsewhere, or could not be taken, before either build started", delivery.ReconcileLease)
	case <-time.After(20 * time.Second):
		t.Fatalf("the %s lease was not taken within 20 s", delivery.ReconcileLease)
	}
	t.Cleanup(func() {
		close(release)
		<-ran
	})
}

// boundLines is how many times p has logged that it bound durable.
func boundLines(p *dispatchProcess, durable string) int {
	return len(p.logLines(`msg="dispatch delivery: NATS consumer bound"`, "name="+durable))
}

// waitForBound waits for p to have logged that it bound durable more than before times.
func waitForBound(t *testing.T, p *dispatchProcess, name, durable string, before int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for boundLines(p, durable) <= before {
		if time.Now().After(deadline) {
			t.Fatalf("the %s did not bind %s within %v:\n%s", name, durable, within, p.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// standbyWindow is how long a restarted process of this tree is watched while a process of the
// build before holds delivers from a durable: a hold's whole TTL (delivery.intakeHoldTTL, 15 s),
// two bind attempts (delivery.durableBindInterval, 2 s each) and a margin. A hold the process left
// behind when it stopped has lapsed within it, so a take-over that waits for that lapse is seen.
const standbyWindow = 15*time.Second + 2*2*time.Second + 3*time.Second

// consumerBinding is where the durable delivers and whether a subscriber is bound there.
func consumerBinding(t *testing.T, js natsgo.JetStreamContext, durable string) string {
	t.Helper()
	info, err := js.ConsumerInfo(bus.Stream, durable)
	if err != nil {
		t.Fatalf("read the durable %s: %v", durable, err)
	}
	return fmt.Sprintf("delivering to %s (bound %t)", info.Config.DeliverSubject, info.PushBound)
}

// pullRequestMessages is how many GitHub-shaped messages the row publishes: half across a stop of
// each build while it holds the intake.
const pullRequestMessages = 50

// handOver is one stop of the process holding the events durable and the take-over that follows
// it: which build stopped and which took over, when the stopping process had exited, the stream
// sequence each of that half's messages was stored at, the sequence the durable delivered from
// once the take-over had bound it, the messages the stopping process logged handing back, and
// the durable's in-flight window.
//
// Those two last are what bound the messages a hand-over may have handled twice, and between
// them they are the whole of it, since a message is delivered a second time only where the
// durable was made to deliver it again:
//
//   - A take-over of this tree's deletes the durable and creates it again at the ack floor it
//     read plus one (delivery.takeOverConfig), and every message from that sequence on is
//     delivered to the new holder. The floor is contiguous and four handlers run at once (the
//     durable's MaxAckPending), so the messages above it are the ones the stopping process had
//     in its handlers and the ones whose acknowledgement landed while an earlier message's had
//     not. The stopping process can still be finishing those handlers while the take-over runs:
//     a build before holds unsubscribes first and acknowledges after (its own StopDelivering
//     says so), which is what leaves its last handlings on both sides of the hand-over.
//   - A hand-back is the other way, and the stopping process says so in its log, naming the
//     message's subject: a handler the stop gave up, or an acknowledgement neither it nor the
//     stop could publish.
//
// A take-over that binds the durable as it stands, which is what a build before holds does
// when it finds it unbound, delivers nothing again: start is then past every sequence, and a
// hand-back is the only repeat that half allows.
type handOver struct {
	stopping, taking string
	at               time.Time
	sequences        map[int]uint64
	start            uint64
	handedBack       map[int]bool
	window           int
}

// holds reports whether number is one of the messages this hand-over's half published.
func (h handOver) holds(number int) bool { _, published := h.sequences[number]; return published }

// replayed reports whether this hand-over had number delivered a second time: the take-over
// created the durable again at or below its sequence, or the stopping process handed it back.
func (h handOver) replayed(number int) bool {
	return h.sequences[number] >= h.start || h.handedBack[number]
}

// publishAcrossStop publishes a merged pull request for each of numbers while holder delivers from
// the events durable, stops holder gracefully once ten of them are handled, and waits for standby to
// take both delivery durables over and for every one of numbers to be handled. It returns that
// hand-over: its time is after holder's process exited, so every handling holder did is before it.
//
// Nothing reads a durable from holder's stop until standby has bound it. A take-over of this
// tree's deletes the durable and creates it again (delivery.takeOverConfig), so between those two
// calls the durable does not exist, and standby can be in that gap as holder's process exits:
// holder lets go of its hold before it exits, and standby's next poll takes the hold and the
// durable. Every release from legion-envoy-v30.21.0 on takes the durables over that way, so the
// gap comes in both hand-overs. The window is therefore read while holder still delivers.
func publishAcrossStop(t *testing.T, builds *twoBuilds, js natsgo.JetStreamContext, fake *pullRequestFake, numbers []int, holder, standby *dispatchProcess) handOver {
	t.Helper()
	handledOf := func() int {
		done, _ := fake.handled()
		count := 0
		for _, number := range numbers {
			if len(done[number]) > 0 {
				count++
			}
		}
		return count
	}
	sequences := make(map[int]uint64, len(numbers))
	for _, number := range numbers {
		sequences[number] = publishMergedPullRequest(t, js, number, fmt.Sprintf("twobuild-delivery-%d-%s", number, strings.ToLower(uniqueProjectKey(t, ""))))
	}
	// The holder stops with the messages part handled: past the first ten, well before the last,
	// at 300 ms a message.
	deadline := time.Now().Add(60 * time.Second)
	for handledOf() < 10 {
		if time.Now().After(deadline) {
			t.Fatalf("the %s handled %d of %d pull requests within 60s:\n%s", builds.buildName(holder), handledOf(), len(numbers), holder.Output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	standbyBound := make(map[string]int, len(deliveryDurables))
	for _, durable := range deliveryDurables {
		standbyBound[durable] = boundLines(standby, durable)
	}
	before := deliverSubjectOf(t, js, bus.DispatchDeliveryDurable)
	window := readBusSettings(t, js).Consumers[bus.DispatchDeliveryDurable].MaxAckPending
	stop(t, holder)
	over := handOver{
		stopping: builds.buildName(holder), taking: builds.buildName(standby), at: time.Now(),
		sequences: sequences, window: window,
	}
	t.Logf("stopped the %s with %d of %d pull requests handled", over.stopping, handledOf(), len(numbers))
	for _, durable := range deliveryDurables {
		waitForBound(t, standby, over.taking, durable, standbyBound[durable], 60*time.Second)
	}
	// Read once the take-over has bound the durable: a take-over that created it again delivers
	// from the sequence it created it at, and one that bound the durable as it stands -- which
	// leaves the deliver subject it had -- delivers nothing again.
	over.start = startedFrom(t, js, bus.DispatchDeliveryDurable, before)
	over.handedBack = handedBackNumbers(holder)
	t.Logf("the %s took the durable over delivering from sequence %d; the %s had logged handing back %v",
		over.taking, over.start, over.stopping, slices.Sorted(maps.Keys(over.handedBack)))
	deadline = time.Now().Add(120 * time.Second)
	for handledOf() < len(numbers) {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d pull requests were handled within 120s of the %s's stop:\n%s",
				handledOf(), len(numbers), over.stopping, standby.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	return over
}

// deliverSubjectOf is where durable delivers now.
func deliverSubjectOf(t *testing.T, js natsgo.JetStreamContext, durable string) string {
	t.Helper()
	info, err := js.ConsumerInfo(bus.Stream, durable)
	if err != nil {
		t.Fatalf("read the durable %s: %v", durable, err)
	}
	return info.Config.DeliverSubject
}

// startedFrom is the stream sequence durable delivers from after a take-over that left it
// delivering somewhere other than before: its OptStartSeq, which a take-over of this tree sets
// to the ack floor it read plus one. A durable still delivering at before was bound as it
// stands and delivers nothing again, which is every sequence past the stream's last.
func startedFrom(t *testing.T, js natsgo.JetStreamContext, durable, before string) uint64 {
	t.Helper()
	info, err := js.ConsumerInfo(bus.Stream, durable)
	if err != nil {
		t.Fatalf("read the durable %s: %v", durable, err)
	}
	if info.Config.DeliverSubject == before || info.Config.OptStartSeq == 0 {
		return math.MaxUint64
	}
	return info.Config.OptStartSeq
}

// handedBackNumbers is every pull request p logged handing back rather than acknowledging: a
// handler its stop gave up, and an acknowledgement neither the handler nor the stop could
// publish. Both lines name the message's subject, whose last token is the pull request number.
func handedBackNumbers(p *dispatchProcess) map[int]bool {
	numbers := map[int]bool{}
	lines := append(
		p.logLines(`msg="dispatch delivery: the intake stopped before the event's handling finished; handing it back"`),
		p.logLines(`msg="dispatch delivery: answer the held message"`)...)
	for _, line := range lines {
		_, after, found := strings.Cut(line, "subject=notifications.github.acme.deploy.pr.")
		if !found {
			continue
		}
		digits := after
		if end := strings.IndexAny(digits, " \t\""); end >= 0 {
			digits = digits[:end]
		}
		if number, err := strconv.Atoi(digits); err == nil {
			numbers[number] = true
		}
	}
	return numbers
}

// pullRequestNumbers is count pull request numbers from first.
func pullRequestNumbers(first, count int) []int {
	numbers := make([]int, count)
	for index := range numbers {
		numbers[index] = first + index
	}
	return numbers
}

// Messages and subscription settings pass between the two builds with none skipped, and no
// message handled twice but where at-least-once delivery makes it so. Issues created on either
// build produce outbox events whose ids are exactly the ids the stream holds. GitHub-shaped
// messages are each handled, the set of their delivery ids the set handled, across a graceful
// stop of the build that holds the intake: this tree first, then the previous build.
//
// A hand-over is where a repeat can come from, and the durable's ack floor is what bounds it. A
// take-over creates the durable again at the floor it found plus one
// (delivery.takeOverConfig), so the next holder is delivered every message above that floor.
// Four handlers run at once (the durable's MaxAckPending), each finishing in its own time, so
// at the moment a holder's process exits the messages above its floor are the ones it had in
// its handlers and the ones whose acknowledgement landed while an earlier message's had not.
// Each of those may be handled a second time, by the build that took the durable over, after
// the stopping process exited.
//
// So a repeat is allowed only where the row can show it was one of those: the message's stream
// sequence is above the floor the stopping process left (handOver.replayed), its first handling
// is the stopping build's before that process exited, its second is the taking build's after,
// and there are no more of them at one hand-over than the durable's in-flight window. Every
// other repeat fails the row — a message handled twice by one build, a third handling, a
// handling once the hand-over has settled, and above all a repeat of a message at or below the
// floor, which is work the hand-over had no reason to deliver again. A message no build handled
// fails it too.
//
// The delivery durables' filter, ack wait, ack bound and generation, and the stream's age,
// duplicate window, subjects and replicas, stay as they are across two settings polls with both
// builds running, and across a restart of each build in turn. While a process of the previous
// build delivers from the durable, a restarted process of this tree stands by rather than take
// the durable from it. From legion-envoy-v30.21.0 on, the previous build holds the durable's
// hold, and the restarted process stands by because takeHold refuses a live hold. A build before
// holds, such as the first-rollout build, delivers under no hold, and the restarted process
// stands by because it finds the durable bound to a subject no hold recorded (boundElsewhere).
func TestTwoBuildsPassMessagesBothWays(t *testing.T) {
	builds := newTwoBuilds(t)
	seedPullRequestSettings(t, builds)
	// One App key for both builds; each build calls its own server of the one fake, so each
	// handling is recorded with the build that did it.
	fake := startPullRequestFake(300 * time.Millisecond)
	app := githubAppEnv(t, "")
	previousEnv, currentEnv := maps.Clone(app), maps.Clone(app)
	previousEnv["DISPATCH_GITHUB_API_BASE"] = fake.serve(t, builds.previousName())
	currentEnv["DISPATCH_GITHUB_API_BASE"] = fake.serve(t, builds.currentName())
	holdTheDeliveryReconcile(t, builds)

	// This tree creates the durables, stamped with its generation, and holds them; the previous
	// build starts beside it as a standby, which is a rolling release's first moment.
	builds.startCurrent(t, currentEnv)
	for _, durable := range deliveryDurables {
		waitForBound(t, builds.current(), builds.currentName(), durable, 0, 60*time.Second)
	}
	builds.startPrevious(t, previousEnv)

	js, err := testnats.Connect(t, builds.nats).JetStream()
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	head := streamHead(t, js)
	baseline := readBusSettings(t, js)
	t.Logf("bus settings with both builds running: %+v", baseline)

	t.Run("outbox events", func(t *testing.T) {
		project := uniqueProjectKey(t, "OBX")
		builds.current().createProject(t, project, "Two builds")
		for _, writer := range []*dispatchProcess{builds.previous(), builds.current(), builds.previous(), builds.current()} {
			key, _ := createDocument(t, writer, project, "Created on the "+builds.buildName(writer), "spec")
			writer.call(t, http.MethodPost, "/api/v1/issues/"+key+"/comments", `{"body":"A comment."}`, http.StatusCreated)
		}
		waitForEveryEventPublished(t, builds.database, 60*time.Second)
		events := publishedEventIDs(t, builds)
		streamed := map[string]bool{}
		for _, item := range readStreamFrom(t, js, head) {
			if item.Source == "dispatch" {
				streamed[item.SourceEventID] = true
			}
		}
		if got, want := slices.Sorted(maps.Keys(streamed)), slices.Sorted(maps.Keys(events)); !slices.Equal(got, want) {
			t.Errorf("the stream holds the events\n%v\nwant exactly the events both builds wrote\n%v", got, want)
		}
	})

	time.Sleep(settingsPolls)
	requireBusSettings(t, js, baseline, "after two settings polls with both builds running")

	if boundLines(builds.previous(), bus.DispatchDeliveryDurable) != 0 {
		t.Fatalf("the %s bound %s while the %s held it:\n%s", builds.previousName(), bus.DispatchDeliveryDurable,
			builds.currentName(), builds.previous().Output.String())
	}
	half := pullRequestMessages / 2
	currentHandOver := publishAcrossStop(t, builds, js, fake, pullRequestNumbers(1001, half), builds.current(), builds.previous())

	// This tree, stopped above, comes back while the previous build delivers from the durable: it
	// stands by and never takes the binding from it. A previous build from legion-envoy-v30.21.0 on
	// holds the durable's hold, which takeHold refuses while it is live; a build before holds
	// delivers under no hold, and the restarted build finds the durable bound elsewhere.
	binding := consumerBinding(t, js, bus.DispatchDeliveryDurable)
	builds.startCurrent(t, currentEnv)
	time.Sleep(standbyWindow)
	if bound, now := boundLines(builds.current(), bus.DispatchDeliveryDurable), consumerBinding(t, js, bus.DispatchDeliveryDurable); bound > 0 || now != binding {
		t.Errorf("the restarted %s took %s from the %s, which delivers from it under no hold: the durable was %s and is %s, and the restarted build logged %d binds:\n%s",
			builds.currentName(), bus.DispatchDeliveryDurable, builds.previousName(), binding, now, bound, builds.current().Output.String())
	}
	requireBusSettings(t, js, baseline, "after the current build restarted")

	// The previous build, now the holder, stops with the second half part handled; this tree
	// takes the durables over, and the previous build comes back.
	previousHandOver := publishAcrossStop(t, builds, js, fake, pullRequestNumbers(1001+half, pullRequestMessages-half), builds.previous(), builds.current())
	builds.startPrevious(t, previousEnv)
	requireBusSettings(t, js, baseline, "after the previous build restarted")

	// Read last, after both restarts, so a message either hand-over delivered again and had
	// handled again is counted. A handling a stopping process abandoned is one of them: it is
	// counted at the pull request's fetch, which it made, and NATS delivers that message again,
	// so the build that took the durable over handles it once more -- a repeat the hand-over's
	// in-flight window bounds, with the messages whose acknowledgement landed above the floor.
	time.Sleep(5 * time.Second)
	done, abandoned := fake.handled()
	handOvers := []handOver{currentHandOver, previousHandOver}
	var twice, again, unhandled []string
	repeats := map[string]int{}
	for number, handlings := range done {
		over, found := handOverOf(handOvers, number)
		switch {
		case len(handlings) == 1 && found:
		case len(handlings) == 2 && found && over.replayed(number) &&
			handlings[0].build == over.stopping && handlings[0].at.Before(over.at) &&
			handlings[1].build == over.taking && handlings[1].at.After(over.at):
			repeats[over.stopping]++
			again = append(again, fmt.Sprintf("#%d %v", number, handlings))
		default:
			twice = append(twice, fmt.Sprintf("#%d seq %d %d time(s) %v (the %s's hand-over delivered from sequence %d and handed back %v)",
				number, over.sequences[number], len(handlings), handlings, over.stopping, over.start,
				slices.Sorted(maps.Keys(over.handedBack))))
		}
	}
	for _, number := range pullRequestNumbers(1001, pullRequestMessages) {
		if len(done[number]) == 0 {
			unhandled = append(unhandled, "#"+strconv.Itoa(number))
		}
	}
	if len(done) != pullRequestMessages || len(twice) > 0 || len(unhandled) > 0 {
		slices.Sort(twice)
		t.Errorf("the %d GitHub messages were handled as %d distinct pull requests; handled again outside what the hand-overs had to deliver again: %v; never handled: %v",
			pullRequestMessages, len(done), twice, unhandled)
	}
	for _, over := range handOvers {
		if repeats[over.stopping] > over.window {
			t.Errorf("the hand-over from the %s had %d message(s) handled again, more than the durable's in-flight window of %d: the stop left more unsettled than it can have had in its handlers (it delivered again from sequence %d)",
				over.stopping, repeats[over.stopping], over.window, over.start)
		}
	}
	if len(abandoned) > 0 {
		t.Logf("handlings a stopping process abandoned unfinished, each handled again after: %v", abandoned)
	}
	if len(again) > 0 {
		slices.Sort(again)
		t.Logf("messages this hand-over had to deliver again, handled once more by the build that took the durable over: %v", again)
	}
}

// handOverOf is the hand-over whose half published number, and whether one did.
func handOverOf(overs []handOver, number int) (handOver, bool) {
	for _, over := range overs {
		if over.holds(number) {
			return over, true
		}
	}
	return handOver{}, false
}

// publishedEventIDs is the id of every event the outbox has published, as the stream's envelopes
// carry it (source_event_id).
func publishedEventIDs(t *testing.T, builds *twoBuilds) map[string]bool {
	t.Helper()
	rows, err := builds.database.Pool.Query(context.Background(), `select id::text from events where published_at is not null`)
	if err != nil {
		t.Fatalf("read the published events: %v", err)
	}
	defer rows.Close()
	published, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the published events: %v", err)
	}
	ids := make(map[string]bool, len(published))
	for _, id := range published {
		ids[id] = true
	}
	return ids
}

// ───── one pull request's issue, read by both builds ─────────────────────────

// The fixture of TestTwoBuildsAttributeAPullRequestFromOneSetOfFacts. Project TB has the
// repository attributionRepo, which holds pull request #81. Project ACME has backlogRepo, which
// holds the rows of steps 5 and 6. The stand-in gives each repository its own installation, so
// each has its own budget of facts queries in a pass (delivery.FactsQueriesPerPass).
const (
	attributionRepo   = "acme/widgets"
	backlogRepo       = "acme/backlog"
	attributionNumber = 81
	// backlogFirst and backlogRows are the backlog of step 5: merged rows with texts.
	backlogFirst, backlogRows = 1001, 1200
	// laterFirst and laterRows are the rows of step 6: merged rows with no facts.
	laterFirst, laterRows = 3001, 600
	// attributionCheck is the Verification document of each issue of the row: one check that a
	// result before the merge answers.
	attributionCheck = ":::check{#k1 surface=\"cli\" run=\"go test ./...\"}\nThe head of the issue's pull request passes.\n:::\n"
	// headPassLine is the message of the line that the head pass logs once in each pass
	// (delivery.Reconcile.reconcilePullRequestHeads), with its counts.
	headPassLine = `msg="dispatch delivery: pull request attribution"`
)

var (
	// firstHead and secondHead are the head of #81 before and after the move of step 4, and
	// firstUpdate and secondUpdate its update times.
	firstHead, secondHead     = strings.Repeat("c1", 20), strings.Repeat("c2", 20)
	firstUpdate, secondUpdate = time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC), time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	// backlogUpdate is the update time of each row of steps 5 and 6.
	backlogUpdate = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	// attributionInstallations is the installation of each repository that the App is installed
	// on.
	attributionInstallations = map[string]int{attributionRepo: 1, backlogRepo: 2}
	// attributionFactsAlias reads the aliases of the facts query (delivery's factsQuery) in their
	// order: a repository alias with the names of its two variables, or a pull request alias with
	// its number.
	attributionFactsAlias = regexp.MustCompile(`(r\d+): repository\(owner: \$(\w+), name: \$(\w+)\)|(p\d+): pullRequest\(number: (\d+)\)`)
	// attributionLogAttribute is one key=value of a slog text line, its value quoted or bare.
	attributionLogAttribute = regexp.MustCompile(`(\w+)=("(?:[^"\\]|\\.)*"|\S*)`)
)

// attributionFake is the stand-in GitHub of the attribution row. It holds one state. Each build
// calls a server of its own (serve), so the fake counts the reads of each build apart. It answers
// the installation and its token, GET /repos/{o}/{r}, …/commits/{sha}, …/commits/{sha}/pulls,
// …/pulls/{n}, …/pulls/{n}/commits, and the facts query (POST /graphql). It answers any other path
// with an empty list and records the path.
type attributionFake struct {
	mu         sync.Mutex
	pulls      map[string]map[int]*fakePull
	reads      map[string]map[string]int
	asked      map[string][]int
	unexpected map[string][]string
}

// fakePull is one pull request of the stand-in, as GitHub holds it now. commits is each commit
// that GitHub holds for it, oldest first: a read of the pull requests of any of them names this
// one. Each commit has one message, which names no issue.
type fakePull struct {
	number    int
	head      string
	branch    string
	title     string
	open      bool
	updatedAt time.Time
	commits   []string
}

func newAttributionFake() *attributionFake {
	return &attributionFake{
		pulls: map[string]map[int]*fakePull{}, reads: map[string]map[string]int{},
		asked: map[string][]int{}, unexpected: map[string][]string{},
	}
}

// put stores pull in repo, in place of the pull request of the same number.
func (f *attributionFake) put(repo string, pull fakePull) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pulls[repo] == nil {
		f.pulls[repo] = map[int]*fakePull{}
	}
	f.pulls[repo][pull.number] = &pull
}

// move pushes head to pull request number of repo, and gives it title and the update time at.
func (f *attributionFake) move(repo string, number int, head, title string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pull := f.pulls[repo][number]
	pull.head, pull.title, pull.updatedAt = head, title, at
	pull.commits = append(pull.commits, head)
}

// countLocked adds one read of kind by build. The caller holds f.mu.
func (f *attributionFake) countLocked(build, kind string) {
	if f.reads[build] == nil {
		f.reads[build] = map[string]int{}
	}
	f.reads[build][kind]++
}

// count adds one read of kind by build.
func (f *attributionFake) count(build, kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.countLocked(build, kind)
}

// readsOf is the reads of build by kind, and the paths no route answered.
func (f *attributionFake) readsOf(build string) (map[string]int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.reads[build]), slices.Clone(f.unexpected[build])
}

// askedOf is each pull request number that build's facts queries asked, in their order.
func (f *attributionFake) askedOf(build string) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked[build])
}

// pullsAtLocked is each pull request of repo that holds the commit sha, in number order. The
// caller holds f.mu.
func (f *attributionFake) pullsAtLocked(repo, sha string) []*fakePull {
	var pulls []*fakePull
	for _, pull := range f.pulls[repo] {
		if slices.Contains(pull.commits, sha) {
			pulls = append(pulls, pull)
		}
	}
	slices.SortFunc(pulls, func(a, b *fakePull) int { return a.number - b.number })
	return pulls
}

// pullOfLocked is the request's repository and its pull request number, or nil. The caller holds
// f.mu.
func (f *attributionFake) pullOfLocked(r *http.Request) (string, *fakePull) {
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		return repo, nil
	}
	return repo, f.pulls[repo][number]
}

// rest is the pull request as GitHub's REST API answers it.
func (p *fakePull) rest(repo string) map[string]any {
	state, merged, mergedAt := "open", false, any(nil)
	if !p.open {
		state, merged, mergedAt = "closed", true, p.updatedAt.Format(time.RFC3339)
	}
	return map[string]any{
		"number": p.number, "title": p.title, "body": "", "html_url": fmt.Sprintf("https://github.com/%s/pull/%d", repo, p.number),
		"state": state, "merged": merged, "merged_at": mergedAt, "updated_at": p.updatedAt.Format(time.RFC3339),
		"user": map[string]any{"login": "octocat"}, "labels": []any{},
		"head": map[string]any{"ref": p.branch, "sha": p.head, "repo": map[string]any{"full_name": repo}},
		"base": map[string]any{"ref": "main"},
	}
}

// node is the pull request as the facts query answers it (delivery's factsNode), with every
// commit message on its first page.
func (p *fakePull) node(repo string) map[string]any {
	state, mergedAt := "OPEN", any(nil)
	if !p.open {
		state, mergedAt = "MERGED", p.updatedAt.Format(time.RFC3339)
	}
	messages := make([]any, len(p.commits))
	for i := range p.commits {
		messages[i] = map[string]any{"commit": map[string]any{"message": fmt.Sprintf("Commit %d.", i+1)}}
	}
	return map[string]any{
		"number": p.number, "title": p.title, "body": "", "url": fmt.Sprintf("https://github.com/%s/pull/%d", repo, p.number),
		"state": state, "merged": !p.open, "mergedAt": mergedAt, "updatedAt": p.updatedAt.Format(time.RFC3339),
		"headRefName": p.branch, "headRefOid": p.head, "baseRefName": "main",
		"headRepository": map[string]any{"nameWithOwner": repo}, "mergeCommit": nil,
		"commits": map[string]any{"totalCount": len(p.commits), "nodes": messages},
	}
}

// writeGitHubJSON writes value as the body of a GitHub answer with status.
func writeGitHubJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode the answer of the stand-in GitHub: %v", err)
	}
}

// serve starts a server on the fake's state whose reads are build's, and returns its URL: the
// DISPATCH_GITHUB_API_BASE of build's processes.
func (f *attributionFake) serve(t *testing.T, build string) string {
	t.Helper()
	notFound := map[string]string{"message": "Not Found"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		f.count(build, "installation")
		id, installed := attributionInstallations[r.PathValue("owner")+"/"+r.PathValue("repo")]
		if !installed {
			writeGitHubJSON(t, w, http.StatusNotFound, notFound)
			return
		}
		writeGitHubJSON(t, w, http.StatusOK, map[string]any{
			"id": id, "app_slug": "twobuild", "permissions": map[string]string{"contents": "read", "pull_requests": "read"},
		})
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		f.count(build, "token")
		writeGitHubJSON(t, w, http.StatusCreated, map[string]string{
			"token": "ghs_twobuild_" + r.PathValue("id"), "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		f.count(build, "repository")
		writeGitHubJSON(t, w, http.StatusOK, map[string]string{
			"full_name": r.PathValue("owner") + "/" + r.PathValue("repo"), "default_branch": "main",
		})
	})
	// The commit itself: a result's record asks whether the repository holds it
	// (githubapp.Client.HasCommit), and only a 422 of this message proves that it does not.
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.countLocked(build, "commit")
		sha := r.PathValue("sha")
		if len(f.pullsAtLocked(r.PathValue("owner")+"/"+r.PathValue("repo"), sha)) == 0 {
			writeGitHubJSON(t, w, http.StatusUnprocessableEntity, map[string]string{"message": "No commit found for SHA: " + sha})
			return
		}
		_, _ = io.WriteString(w, sha)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.countLocked(build, "commit pulls")
		repo := r.PathValue("owner") + "/" + r.PathValue("repo")
		answer := []any{}
		for _, pull := range f.pullsAtLocked(repo, r.PathValue("sha")) {
			answer = append(answer, pull.rest(repo))
		}
		writeGitHubJSON(t, w, http.StatusOK, answer)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.countLocked(build, "pull")
		repo, pull := f.pullOfLocked(r)
		if pull == nil {
			writeGitHubJSON(t, w, http.StatusNotFound, notFound)
			return
		}
		writeGitHubJSON(t, w, http.StatusOK, pull.rest(repo))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.countLocked(build, "pull commits")
		_, pull := f.pullOfLocked(r)
		if pull == nil {
			writeGitHubJSON(t, w, http.StatusNotFound, notFound)
			return
		}
		answer := []any{}
		if page := r.URL.Query().Get("page"); page == "" || page == "1" {
			for i, sha := range pull.commits {
				at := map[string]string{"date": pull.updatedAt.Format(time.RFC3339)}
				answer = append(answer, map[string]any{"sha": sha, "commit": map[string]any{
					"message": fmt.Sprintf("Commit %d.", i+1), "author": at, "committer": at,
				}})
			}
		}
		writeGitHubJSON(t, w, http.StatusOK, answer)
	})
	mux.HandleFunc("POST /graphql", f.serveFacts(t, build))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.unexpected[build] = append(f.unexpected[build], r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		writeGitHubJSON(t, w, http.StatusOK, []any{})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

// serveFacts answers the facts query (delivery's readFactsBatch), whose text holds the fragment
// pullRequestFacts. A pull request the stand-in does not hold is GitHub's NOT_FOUND on its alias
// path, and a repository the App is not installed on is NOT_FOUND on its alias.
func (f *attributionFake) serveFacts(t *testing.T, build string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !strings.Contains(request.Query, "fragment pullRequestFacts") {
			f.mu.Lock()
			f.unexpected[build] = append(f.unexpected[build], "POST /graphql (not the facts query)")
			f.mu.Unlock()
			writeGitHubJSON(t, w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.countLocked(build, "graphql")
		data := map[string]any{}
		var refusals []any
		var current map[string]any
		var repoAlias, repo string
		for _, alias := range attributionFactsAlias.FindAllStringSubmatch(request.Query, -1) {
			if alias[1] != "" {
				owner, _ := request.Variables[alias[2]].(string)
				name, _ := request.Variables[alias[3]].(string)
				repoAlias, repo = alias[1], owner+"/"+name
				if _, installed := attributionInstallations[repo]; !installed {
					current, data[repoAlias] = nil, nil
					refusals = append(refusals, map[string]any{"type": "NOT_FOUND", "path": []any{repoAlias},
						"message": fmt.Sprintf("Could not resolve to a Repository with the name '%s'.", repo)})
					continue
				}
				current = map[string]any{}
				data[repoAlias] = current
				continue
			}
			if current == nil {
				continue
			}
			number, _ := strconv.Atoi(alias[5])
			f.asked[build] = append(f.asked[build], number)
			pull := f.pulls[repo][number]
			if pull == nil {
				current[alias[4]] = nil
				refusals = append(refusals, map[string]any{"type": "NOT_FOUND", "path": []any{repoAlias, alias[4]},
					"message": fmt.Sprintf("Could not resolve to a PullRequest with the number of %d.", number)})
				continue
			}
			current[alias[4]] = pull.node(repo)
		}
		answer := map[string]any{"data": data}
		if len(refusals) > 0 {
			answer["errors"] = refusals
		}
		writeGitHubJSON(t, w, http.StatusOK, answer)
	}
}

// reconcileLeaseNamespace is the first key of every loop lease: duty's leaseNamespace, which
// stays unexported, the four bytes of "LOOP" (memory_harness_test.go names it too). The second
// key is the lease's name, which Postgres hashes with hashtext.
const reconcileLeaseNamespace int32 = 0x4C4F4F50

// reconcileGate holds the delivery reconcile's lease (delivery.ReconcileLease) on a connection of
// the test. While the test holds it, each tick of either build finds the lease held and runs no
// pass (duty.Exclusive). onePass lets one pass of a build run.
type reconcileGate struct {
	pool *store.Pool
	conn *pgxpool.Conn
	held bool
}

// closedReconcileGate takes the lease before either build starts, and gives it back when the
// test ends. It fails the test when the database does not then show the lease held, because the
// gate would then take another lock than the reconcile takes.
func closedReconcileGate(t *testing.T, database *store.Store) *reconcileGate {
	t.Helper()
	ctx := context.Background()
	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("take a connection for the gate of the reconcile: %v", err)
	}
	gate := &reconcileGate{pool: database.Pool, conn: conn}
	t.Cleanup(func() {
		if gate.held {
			unlock, cancel := duty.LeaseCall()
			_, err := conn.Exec(unlock, `select pg_advisory_unlock($1, hashtext($2))`, reconcileLeaseNamespace, delivery.ReconcileLease)
			cancel()
			if err != nil {
				// Postgres gives a session's locks back when the session ends.
				closing, cancel := duty.LeaseCall()
				_ = conn.Hijack().Close(closing)
				cancel()
				return
			}
		}
		conn.Release()
	})
	gate.close(t)
	if held, err := duty.LeaseHeld(ctx, database.Pool, delivery.ReconcileLease); err != nil || !held {
		t.Fatalf("the gate took the lease %s, and the database shows it held=%v (%v): the gate takes another lock than the reconcile",
			delivery.ReconcileLease, held, err)
	}
	return gate
}

// close takes the lease, and waits behind a pass that holds it.
func (g *reconcileGate) close(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if _, err := g.conn.Exec(ctx, `select pg_advisory_lock($1, hashtext($2))`, reconcileLeaseNamespace, delivery.ReconcileLease); err != nil {
		t.Fatalf("take the lease %s: %v", delivery.ReconcileLease, err)
	}
	g.held = true
}

// open gives the lease back.
func (g *reconcileGate) open(t *testing.T) {
	t.Helper()
	ctx, cancel := duty.LeaseCall()
	defer cancel()
	var released bool
	if err := g.conn.QueryRow(ctx, `select pg_advisory_unlock($1, hashtext($2))`, reconcileLeaseNamespace, delivery.ReconcileLease).Scan(&released); err != nil || !released {
		t.Fatalf("give the lease %s back: released=%v, %v", delivery.ReconcileLease, released, err)
	}
	g.held = false
}

// onePass lets exactly one pass of p run, and answers the attributes of its head pass's line. It
// gives the lease back. Once a tick holds the lease, it queues for the lease, so the database
// gives it to the gate when that tick gives it back, before the next tick of any process.
//
// A tick of the previous build can hold the lease too: it finds its period served and runs no
// pass (holdThePreviousBuildsPasses). p then logged no pass, and the gate opens again.
func (g *reconcileGate) onePass(t *testing.T, p *dispatchProcess, step string) map[string]string {
	t.Helper()
	before := len(headPasses(p))
	passed := func() bool { return len(headPasses(p)) > before }
	for attempt := 1; ; attempt++ {
		g.open(t)
		deadline := time.Now().Add(2 * time.Minute)
		for {
			held, err := duty.LeaseHeld(context.Background(), g.pool, delivery.ReconcileLease)
			if err != nil {
				t.Fatalf("%s: %v", step, err)
			}
			if held || passed() {
				break
			}
			select {
			case <-p.Exited:
				t.Fatalf("%s: the current build exited while the gate was open:\n%s", step, p.Output.String())
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: no tick took the lease %s within 2 minutes of the gate opening:\n%s", step, delivery.ReconcileLease, p.Output.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
		g.close(t)
		// The head pass logs its line before the pass gives the lease back. The test reads the
		// process's output through a pipe, so it waits a while for the line.
		if testwait.Holds(10*time.Second, 20*time.Millisecond, passed) {
			break
		}
		if attempt == 5 {
			t.Fatalf("%s: five ticks held the lease, and the current build logged no pass:\n%s", step, p.Output.String())
		}
		t.Logf("%s: a tick that ran no pass of the current build held the lease; the gate opens again", step)
	}
	lines := headPasses(p)
	if len(lines) != before+1 {
		t.Fatalf("%s: the current build logged %d passes while the gate was open, want 1:\n%s", step, len(lines)-before, strings.Join(lines[before:], "\n"))
	}
	t.Logf("%s: %s", step, strings.TrimSpace(lines[before]))
	attributes := map[string]string{}
	for _, match := range attributionLogAttribute.FindAllStringSubmatch(lines[before], -1) {
		attributes[match[1]] = strings.Trim(match[2], `"`)
	}
	return attributes
}

// headPasses is each whole line of a head pass that p has logged, without its newline. A line
// that the pipe has not yet carried to its end is left out, so no reader parses half of its
// counts. logLines cannot tell the two apart, because it trims the newline.
func headPasses(p *dispatchProcess) []string {
	var whole []string
	for line := range strings.Lines(p.Output.String()) {
		if strings.Contains(line, headPassLine) && strings.HasSuffix(line, "\n") {
			whole = append(whole, strings.TrimSuffix(line, "\n"))
		}
	}
	return whole
}

// wantPassCounts fails the test for each count of a head pass's line that is not its want.
func wantPassCounts(t *testing.T, step string, line map[string]string, want map[string]int) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if got, err := strconv.Atoi(line[name]); err != nil || got != want[name] {
			t.Errorf("%s: the pass logged %s=%q, want %d", step, name, line[name], want[name])
		}
	}
}

// previousReconcilePeriod is the period of the previous build's delivery reconcile: its
// delivery.ReconcileInterval, as no test setting shortens it there.
const previousReconcilePeriod = 5 * time.Minute

// holdThePreviousBuildsPasses records a pass of the previous build's period that ends in the year
// 2999. Each tick of the previous build then finds its period served and runs no pass (duty's
// readPeriod). So each step reads the previous build before its next pass. The previous build
// keys its record <lease>/<period_ns>. This tree's build runs at 2 s, so the record serves none of
// its ticks. It answers the key of the record.
func holdThePreviousBuildsPasses(t *testing.T, database *store.Store) string {
	t.Helper()
	period := previousReconcilePeriod.Nanoseconds()
	key := delivery.ReconcileLease + "/" + strconv.FormatInt(period, 10)
	if _, err := database.Pool.Exec(context.Background(), `insert into duty_passes (lease, boundary, period_ns) values ($1, $2, $3)`,
		key, time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC), period); err != nil {
		t.Fatalf("record the previous build's period as served: %v", err)
	}
	return key
}

// mapRepositoryTo makes repo the default repository of project, through p.
func mapRepositoryTo(t *testing.T, p *dispatchProcess, repo, project string) {
	t.Helper()
	p.call(t, http.MethodPut, "/api/v1/settings/repo-projects/"+repo, jsonBody(t, map[string]any{"project": project, "default": true}), http.StatusOK)
}

// issueWithCheck creates an issue of project through p, gives it attributionCheck, and fails the
// test when its key is not want.
func issueWithCheck(t *testing.T, p *dispatchProcess, project, title, want string) {
	t.Helper()
	if key, _ := p.createIssueIn(t, project, title, "A spec.\n"); key != want {
		t.Fatalf("the issue %q is %s, want %s", title, key, want)
	}
	p.call(t, http.MethodPost, "/api/v1/issues/"+want+"/artifacts",
		jsonBody(t, map[string]string{"name": "verification.md", "content": attributionCheck}), http.StatusCreated)
}

// recordPass records a pass of the check k1 of issue at commit through p.
func recordPass(t *testing.T, p *dispatchProcess, issue, commit string) {
	t.Helper()
	p.call(t, http.MethodPost, "/api/v1/issues/"+issue+"/results",
		jsonBody(t, map[string]any{"check": "k1", "role": "implementer", "verdict": "pass", "commit": commit}), http.StatusCreated)
}

// basesOf is each pull request that the check k1 of issue compares its results with, as p
// answers GET /api/v1/issues/{key}/results: its number, with the head commit of the basis.
func basesOf(t *testing.T, p *dispatchProcess, issue string) map[int]string {
	t.Helper()
	var read struct {
		Checks []model.CheckBlock `json:"checks"`
	}
	getJSON(t, p, "/api/v1/issues/"+issue+"/results", &read)
	for _, check := range read.Checks {
		if check.BlockID != "k1" {
			continue
		}
		bases := map[int]string{}
		for _, basis := range check.ComparedWith {
			if basis.PullRequest != nil {
				bases[*basis.PullRequest] = basis.Commit
			}
		}
		return bases
	}
	t.Fatalf("the results of %s hold no check k1: %+v", issue, read.Checks)
	return nil
}

// wantBases fails the test where the check k1 of issue, as p answers it, compares with other pull
// requests than want.
func wantBases(t *testing.T, p *dispatchProcess, name, issue string, want []int, when string) {
	t.Helper()
	got := basesOf(t, p, issue)
	var missing, extra []int
	for _, number := range want {
		if _, found := got[number]; !found {
			missing = append(missing, number)
		}
	}
	for number := range got {
		if !slices.Contains(want, number) {
			extra = append(extra, number)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		slices.Sort(extra)
		t.Errorf("%s: the %s compares %s with %d pull requests, want %d: %s",
			when, name, issue, len(got), len(want), numbersDiff(missing, extra))
	}
}

// numbersDiff names the first few numbers of missing and of extra.
func numbersDiff(missing, extra []int) string {
	first := func(numbers []int) []int { return numbers[:min(len(numbers), 8)] }
	return fmt.Sprintf("%d missing %v, %d more %v", len(missing), first(missing), len(extra), first(extra))
}

// numbersFrom is count numbers from first, in order.
func numbersFrom(first, count int) []int {
	numbers := make([]int, count)
	for i := range numbers {
		numbers[i] = first + i
	}
	return numbers
}

// statementAnswers is the statement's answer for each stored results row, by number, computed in
// the test's own process (delivery.PullRequestIssues) over the database that both builds serve.
func statementAnswers(t *testing.T, database *store.Store) map[int]delivery.PullRequestAttribution {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `select repo, number from pull_request_heads`)
	if err != nil {
		t.Fatalf("read the stored results rows: %v", err)
	}
	keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (delivery.PullRequestKey, error) {
		var key delivery.PullRequestKey
		return key, row.Scan(&key.Repo, &key.Number)
	})
	if err != nil {
		t.Fatalf("read the stored results rows: %v", err)
	}
	answers, err := delivery.PullRequestIssues(ctx, database.Pool, keys)
	if err != nil {
		t.Fatalf("read the statement's answers: %v", err)
	}
	byNumber := make(map[int]delivery.PullRequestAttribution, len(answers))
	for key, answer := range answers {
		byNumber[key.Number] = answer
	}
	return byNumber
}

// heldBy is each number whose answer is issue, in order.
func heldBy(answers map[int]delivery.PullRequestAttribution, issue string) []int {
	var numbers []int
	for number, answer := range answers {
		if answer.IssueKey != nil && *answer.IssueKey == issue {
			numbers = append(numbers, number)
		}
	}
	slices.Sort(numbers)
	return numbers
}

// wantStatement fails the test where the statement's answer of number is not issue, has_facts
// and known.
func wantStatement(t *testing.T, answers map[int]delivery.PullRequestAttribution, number int, issue string, hasFacts bool, when string) {
	t.Helper()
	answer, found := answers[number]
	if !found || answer.IssueKey == nil || *answer.IssueKey != issue || answer.HasFacts != hasFacts || !answer.Known {
		t.Errorf("%s: the statement answers #%d %s, want %s with has_facts %v and known", when, number, describeAnswer(answer, found), issue, hasFacts)
	}
}

func describeAnswer(answer delivery.PullRequestAttribution, found bool) string {
	if !found {
		return "nothing"
	}
	issue := "nobody"
	if answer.IssueKey != nil {
		issue = *answer.IssueKey
	}
	return fmt.Sprintf("%s from %s, has_facts %v, known %v", issue, answer.Source, answer.HasFacts, answer.Known)
}

// wantStoredIsStatement fails the test for each results row whose stored issue, which the
// previous build reads, is not the statement's answer.
func wantStoredIsStatement(t *testing.T, database *store.Store, answers map[int]delivery.PullRequestAttribution, when string) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select number, issue_key from pull_request_heads`)
	if err != nil {
		t.Fatalf("read the stored issues: %v", err)
	}
	var differ []string
	stored := 0
	for rows.Next() {
		var number int
		var issue *string
		if err := rows.Scan(&number, &issue); err != nil {
			rows.Close()
			t.Fatalf("read the stored issues: %v", err)
		}
		stored++
		answer := answers[number]
		if !answer.Known || (issue == nil) != (answer.IssueKey == nil) || issue != nil && *issue != *answer.IssueKey {
			differ = append(differ, fmt.Sprintf("#%d stores %v, the statement answers %s", number, nobodyOr(issue), describeAnswer(answer, true)))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stored issues: %v", err)
	}
	if len(differ) > 0 {
		t.Errorf("%s: %d of %d results rows store another issue than the statement's answer: %v", when, len(differ), stored, differ[:min(len(differ), 8)])
	}
}

func nobodyOr(issue *string) string {
	if issue == nil {
		return "nobody"
	}
	return *issue
}

// storedPullRequest is the head and the stored issue of the results row of number of repo, and
// the head of its texts row, "" where it has none.
func storedPullRequest(t *testing.T, database *store.Store, repo string, number int) (head, issue, textsHead string) {
	t.Helper()
	var stored *string
	if err := database.Pool.QueryRow(context.Background(), `
		select h.head_sha, h.issue_key, coalesce(t.head_sha, '') from pull_request_heads h
		left join pull_request_texts t on t.repo = h.repo and t.number = h.number
		where h.repo = $1 and h.number = $2`, repo, number).Scan(&head, &stored, &textsHead); err != nil {
		t.Fatalf("read the results row of %s#%d: %v", repo, number, err)
	}
	return head, nobodyOr(stored), textsHead
}

// attributionCount runs one count statement over the builds' database.
func attributionCount(t *testing.T, database *store.Store, what, statement string, args ...any) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), statement, args...).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", what, err)
	}
	return count
}

// queuedMoves is the moves that wait for their events (pull_request_attribution_moves).
func queuedMoves(t *testing.T, database *store.Store) int {
	t.Helper()
	return attributionCount(t, database, "the queued moves", `select count(*) from pull_request_attribution_moves`)
}

// attributionEventsOn is the events of a moved attribution that issue carries.
func attributionEventsOn(t *testing.T, database *store.Store, issue string) int {
	t.Helper()
	return attributionCount(t, database, "the attribution events of "+issue,
		`select count(*) from events where issue_key = $1 and type = $2`, issue, delivery.AttributionEventType)
}

// storeBacklog stores the backlog of step 5: backlogRows merged rows of backlogRepo, each with
// current texts and its messages read. The branch of each names ACME-2 and its title names
// ACME-1. Each results row stores ACME-1 from the title, as an older rule stored it. The stand-in
// holds the same pull requests.
func storeBacklog(t *testing.T, database *store.Store, fake *attributionFake) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source, title, url, event_at, checked_at, github_read_at)
		select $1::text, n, pg_catalog.lpad(pg_catalog.to_hex(n), 40, '0'), 'legion/ACME-2', 'main', 'merged', 'ACME-1', 'title_body',
			'ACME-1 the work', 'https://github.com/' || $1::text || '/pull/' || n, $4::timestamptz, now(), now()
		from pg_catalog.generate_series($2::int, $2::int + $3::int - 1) n
	`, backlogRepo, backlogFirst, backlogRows, backlogUpdate); err != nil {
		t.Fatalf("store the backlog's results rows: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title, body, commit_messages, updated_at)
		select h.repo, h.number, h.head_sha, h.head_ref, h.repo, h.base_ref, h.title, '', array['Commit 1.'], h.event_at
		from pull_request_heads h where h.repo = $1 and h.number between $2::int and $2::int + $3::int - 1
	`, backlogRepo, backlogFirst, backlogRows); err != nil {
		t.Fatalf("store the backlog's texts: %v", err)
	}
	for _, number := range numbersFrom(backlogFirst, backlogRows) {
		head := fmt.Sprintf("%040x", number)
		fake.put(backlogRepo, fakePull{number: number, head: head, branch: "legion/ACME-2", title: "ACME-1 the work", updatedAt: backlogUpdate, commits: []string{head}})
	}
}

// storeLaterRows stores the rows of step 6: laterRows merged rows of backlogRepo with no texts, as
// the release before stores a row. Each stores ACME-1, and its title names ACME-1, so a read of
// its facts gives the issue it stores and queues no move. The stand-in holds the same pull
// requests.
func storeLaterRows(t *testing.T, database *store.Store, fake *attributionFake) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source, title, url, event_at, checked_at)
		select $1::text, n, pg_catalog.lpad(pg_catalog.to_hex(n), 40, '0'), 'work/no-key', 'main', 'merged', 'ACME-1', 'title_body',
			'ACME-1: an older change', 'https://github.com/' || $1::text || '/pull/' || n, $4::timestamptz, now()
		from pg_catalog.generate_series($2::int, $2::int + $3::int - 1) n
	`, backlogRepo, laterFirst, laterRows, backlogUpdate); err != nil {
		t.Fatalf("store the rows with no facts: %v", err)
	}
	for _, number := range numbersFrom(laterFirst, laterRows) {
		head := fmt.Sprintf("%040x", number)
		fake.put(backlogRepo, fakePull{number: number, head: head, branch: "work/no-key", title: "ACME-1: an older change", updatedAt: backlogUpdate, commits: []string{head}})
	}
}

// Two builds on one database give a pull request one issue from one set of facts, through a
// backlog and a stop of this tree's build. The previous build reads the issue that a results row
// stores. This tree's build reads the statement over the stored facts, and its pass writes that
// answer to each stored row.
//
// The test holds the delivery reconcile's lease (reconcileGate), so no pass runs until the test
// lets exactly one run. It records the previous build's period as served, so the previous build
// runs no pass in the test (holdThePreviousBuildsPasses). This tree's build runs at 2 s, and its
// first tick comes at its start (DISPATCH_TEST_RECONCILE_AT_START=1).
//
//  1. Issues TB-1 and TB-2 have a check. Open pull request #81 has the branch work/no-key, the
//     title "TB-1: the change" and the head C1. The previous build records a result of TB-1 at
//     C1. Its read-through writes #81 with no texts and with its verdict TB-1.
//  2. Before a pass, this tree's build compares TB-1 with #81: R4, the stored answer.
//  3. After one pass, #81 has texts. Both builds compare TB-1 with #81.
//  4. The stand-in moves #81 to head C2, the title "TB-2: the change" and a later update time. The
//     previous build records a result of TB-2 at C2, and writes the new head and its verdict and no
//     texts. This tree's build compares TB-2 with #81 at once, because texts of C1 are no facts of
//     C2. After one pass, #81 has texts of C2, and the answer stays TB-2.
//  5. The backlog: 1,200 rows of backlogRepo with texts, each storing an older answer. After one
//     pass, the previous build gives the statement's answer for each of them, each side of the
//     moves carries 500 events, and 700 moves stay queued.
//  6. The stop: 600 merged rows with no facts are stored after the pass of step 5. Then this tree's
//     build gets SIGTERM, and no pass runs between that pass and the stop. The previous build
//     alone then gives, for each row, the issue that this tree's build gave before the stop. The
//     queue keeps its rows. After the first pass of the restarted build, the queue holds 500
//     fewer moves, and the facts line has read 500 of the 600. Each answer is the statement's.
//
// The control of step 4 is a build of this tree whose statement reads texts of another head as
// facts (ENVOY_TEST_GO_OVERLAY). It fails the test: this tree's build then gives TB-1 from the
// texts of C1.
func TestTwoBuildsAttributeAPullRequestFromOneSetOfFacts(t *testing.T) {
	builds := newTwoBuilds(t)
	database := builds.database
	gate := closedReconcileGate(t, database)
	pin := holdThePreviousBuildsPasses(t, database)
	fake := newAttributionFake()
	fake.put(attributionRepo, fakePull{number: attributionNumber, head: firstHead, branch: "work/no-key", title: "TB-1: the change",
		open: true, updatedAt: firstUpdate, commits: []string{firstHead}})
	app := githubAppEnv(t, "")
	previousEnv, currentEnv := maps.Clone(app), maps.Clone(app)
	previousEnv["DISPATCH_GITHUB_API_BASE"] = fake.serve(t, builds.previousName())
	currentEnv["DISPATCH_GITHUB_API_BASE"] = fake.serve(t, builds.currentName())
	currentEnv["DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL"] = (2 * time.Second).String()
	currentEnv["DISPATCH_TEST_RECONCILE_AT_START"] = "1"
	previous := builds.startPrevious(t, previousEnv)
	current := builds.startCurrent(t, currentEnv)
	if err := delivery.PrepareAttribution(context.Background(), database.Pool); err != nil {
		t.Fatalf("build the statement in the test's process: %v", err)
	}
	current.createProject(t, "TB", "Two builds")
	mapRepositoryTo(t, current, attributionRepo, "TB")
	issueWithCheck(t, current, "TB", "The first issue", "TB-1")
	issueWithCheck(t, current, "TB", "The second issue", "TB-2")
	current.createProject(t, "ACME", "The backlog")
	mapRepositoryTo(t, current, backlogRepo, "ACME")
	issueWithCheck(t, current, "ACME", "The older answer", "ACME-1")
	issueWithCheck(t, current, "ACME", "The rule's answer", "ACME-2")
	previousName, currentName := builds.previousName(), builds.currentName()

	// 1. The previous build's read-through writes #81 with its verdict and no texts.
	recordPass(t, previous, "TB-1", firstHead)
	if head, issue, texts := storedPullRequest(t, database, attributionRepo, attributionNumber); head != firstHead || issue != "TB-1" || texts != "" {
		t.Fatalf("step 1: #81 stores the head %s, the issue %s and texts of %q, want %s, TB-1 and no texts", head, issue, texts, firstHead)
	}
	wantBases(t, previous, previousName, "TB-1", []int{attributionNumber}, "step 1")

	// 2. Before any pass, this tree's build gives the stored answer (R4).
	if passes := current.logLines(headPassLine); len(passes) != 0 {
		t.Fatalf("step 2: the %s ran %d passes while the test held the lease:\n%s", currentName, len(passes), strings.Join(passes, "\n"))
	}
	wantStatement(t, statementAnswers(t, database), attributionNumber, "TB-1", false, "step 2")
	wantBases(t, current, currentName, "TB-1", []int{attributionNumber}, "step 2")
	wantBases(t, current, currentName, "TB-2", nil, "step 2")

	// 3. One pass reads the facts of #81. Both builds give TB-1.
	gate.onePass(t, current, "step 3")
	if head, issue, texts := storedPullRequest(t, database, attributionRepo, attributionNumber); head != firstHead || issue != "TB-1" || texts != firstHead {
		t.Fatalf("step 3: #81 stores the head %s, the issue %s and texts of %q, want %s, TB-1 and texts of %s", head, issue, texts, firstHead, firstHead)
	}
	wantStatement(t, statementAnswers(t, database), attributionNumber, "TB-1", true, "step 3")
	for _, p := range []*dispatchProcess{current, previous} {
		wantBases(t, p, builds.buildName(p), "TB-1", []int{attributionNumber}, "step 3")
	}

	// 4. The move. The previous build writes the new head and its verdict, and no texts.
	fake.move(attributionRepo, attributionNumber, secondHead, "TB-2: the change", secondUpdate)
	recordPass(t, previous, "TB-2", secondHead)
	if head, issue, texts := storedPullRequest(t, database, attributionRepo, attributionNumber); head != secondHead || issue != "TB-2" || texts != firstHead {
		t.Fatalf("step 4: #81 stores the head %s, the issue %s and texts of %q, want %s, TB-2 and texts of %s", head, issue, texts, secondHead, firstHead)
	}
	wantStatement(t, statementAnswers(t, database), attributionNumber, "TB-2", false, "step 4, before the pass")
	// Only TB-2 is read here. A result of TB-1 is at C1, which heads no row now, so a read of
	// TB-1 reads #81 through from GitHub and stores texts of C2 before the pass.
	if bases := basesOf(t, current, "TB-2"); bases[attributionNumber] != secondHead {
		t.Fatalf("step 4, before the pass: the %s compares TB-2 with %v, want #81 at %s: it gives TB-1 from the texts of %s, which are no facts of %s (it compares TB-1 with %v)",
			currentName, bases, secondHead, firstHead, secondHead, basesOf(t, current, "TB-1"))
	}
	gate.onePass(t, current, "step 4")
	if head, issue, texts := storedPullRequest(t, database, attributionRepo, attributionNumber); head != secondHead || issue != "TB-2" || texts != secondHead {
		t.Fatalf("step 4, after the pass: #81 stores the head %s, the issue %s and texts of %q, want %s, TB-2 and texts of %s", head, issue, texts, secondHead, secondHead)
	}
	wantStatement(t, statementAnswers(t, database), attributionNumber, "TB-2", true, "step 4, after the pass")
	for _, p := range []*dispatchProcess{current, previous} {
		wantBases(t, p, builds.buildName(p), "TB-2", []int{attributionNumber}, "step 4, after the pass")
		wantBases(t, p, builds.buildName(p), "TB-1", nil, "step 4, after the pass")
	}

	// 5. The backlog. Before the pass, the previous build gives each row's older answer: the
	// reader can fail. One pass writes the statement's answer to each row.
	storeBacklog(t, database, fake)
	backlog := numbersFrom(backlogFirst, backlogRows)
	wantBases(t, previous, previousName, "ACME-1", backlog, "step 5, before the pass")
	wantBases(t, previous, previousName, "ACME-2", nil, "step 5, before the pass")
	wantPassCounts(t, "step 5", gate.onePass(t, current, "step 5"),
		map[string]int{"facts_left_before": 0, "changes": backlogRows, "events": 2 * delivery.AttributionMovesPerPass, "moves_queued": backlogRows - delivery.AttributionMovesPerPass})
	answers := statementAnswers(t, database)
	if got := heldBy(answers, "ACME-2"); !slices.Equal(got, backlog) {
		t.Fatalf("step 5: the statement gives ACME-2 to %d pull requests, want the %d of the backlog", len(got), backlogRows)
	}
	wantBases(t, previous, previousName, "ACME-2", heldBy(answers, "ACME-2"), "step 5")
	wantBases(t, previous, previousName, "ACME-1", heldBy(answers, "ACME-1"), "step 5")
	wantStoredIsStatement(t, database, answers, "step 5")
	for _, issue := range []string{"ACME-1", "ACME-2"} {
		if events := attributionEventsOn(t, database, issue); events != delivery.AttributionMovesPerPass {
			t.Errorf("step 5: %s carries %d attribution events, want %d", issue, events, delivery.AttributionMovesPerPass)
		}
	}
	if queued := queuedMoves(t, database); queued != backlogRows-delivery.AttributionMovesPerPass {
		t.Fatalf("step 5: %d moves are queued after one pass, want %d", queued, backlogRows-delivery.AttributionMovesPerPass)
	}

	// 6. The stop, while moves are queued and facts are due, and while no pass runs.
	storeLaterRows(t, database, fake)
	issues := []string{"TB-1", "TB-2", "ACME-1", "ACME-2"}
	answers = statementAnswers(t, database)
	gave := map[string]map[int]string{}
	for _, issue := range issues {
		gave[issue] = basesOf(t, current, issue)
		wantBases(t, current, currentName, issue, heldBy(answers, issue), "step 6, before the stop")
	}
	queued := queuedMoves(t, database)
	// The stop of a deploy: SIGTERM, and the graceful shutdown (stop). No pass runs, because the
	// test holds the lease. dispatchProcess.signal is in the multiprocess build alone, so the
	// harness's stop sends the signal.
	stop(t, current)
	t.Logf("step 6: the %s exited with code %d after SIGTERM", currentName, current.Cmd.ProcessState.ExitCode())
	if after := queuedMoves(t, database); after != queued || queued != backlogRows-delivery.AttributionMovesPerPass {
		t.Errorf("step 6: %d moves were queued before the stop and %d after it, want %d both times", queued, after, backlogRows-delivery.AttributionMovesPerPass)
	}
	for _, issue := range issues {
		wantBases(t, previous, previousName, issue, slices.Sorted(maps.Keys(gave[issue])), "step 6, after the stop: what the "+currentName+" gave before it")
	}
	wantStoredIsStatement(t, database, answers, "step 6, after the stop")
	current = builds.startCurrent(t, currentEnv)
	read := func() int {
		return attributionCount(t, database, "the texts of the rows with no facts",
			`select count(*) from pull_request_texts where repo = $1 and number between $2::int and $2::int + $3::int - 1`, backlogRepo, laterFirst, laterRows)
	}
	factsRead := delivery.FactsQueriesPerPass * delivery.FactsBatchSize
	askedBefore := len(fake.askedOf(currentName))
	wantPassCounts(t, "step 6, the first pass after the restart", gate.onePass(t, current, "step 6"), map[string]int{
		"facts_left_before": laterRows, "facts_left": laterRows - factsRead, "changes": 0,
		"events": 2 * delivery.AttributionMovesPerPass, "moves_queued": queued - delivery.AttributionMovesPerPass,
	})
	if texts := read(); texts != factsRead {
		t.Errorf("step 6: after the first pass of the restarted build %d of the %d rows with no facts have texts, want %d", texts, laterRows, factsRead)
	}
	asked := map[int]bool{}
	for _, number := range fake.askedOf(currentName)[askedBefore:] {
		if number >= laterFirst && number < laterFirst+laterRows {
			asked[number] = true
		}
	}
	if len(asked) != factsRead {
		t.Errorf("step 6: the first pass of the restarted build asked GitHub the facts of %d of the %d rows with no facts, want %d", len(asked), laterRows, factsRead)
	}
	if after := queuedMoves(t, database); after != queued-delivery.AttributionMovesPerPass {
		t.Errorf("step 6: after the first pass of the restarted build %d moves are queued, want %d: %d fewer than at the stop", after, queued-delivery.AttributionMovesPerPass, delivery.AttributionMovesPerPass)
	}
	answers = statementAnswers(t, database)
	for number, answer := range answers {
		if !answer.Known {
			t.Errorf("step 6: the statement answers #%d %s, want a known answer", number, describeAnswer(answer, true))
		}
	}
	for _, issue := range issues {
		for _, p := range []*dispatchProcess{current, previous} {
			wantBases(t, p, builds.buildName(p), issue, heldBy(answers, issue), "step 6, after the first pass of the restarted build")
		}
	}
	wantStoredIsStatement(t, database, answers, "step 6, after the first pass of the restarted build")
	for _, issue := range []string{"ACME-1", "ACME-2"} {
		if events := attributionEventsOn(t, database, issue); events != 2*delivery.AttributionMovesPerPass {
			t.Errorf("step 6: %s carries %d attribution events, want %d", issue, events, 2*delivery.AttributionMovesPerPass)
		}
	}

	// The previous build ran no pass: its record of its period is the test's, and it read no pull
	// request by its number, as its head pass reads an open row again.
	var boundary time.Time
	if err := database.Pool.QueryRow(context.Background(), `select boundary from duty_passes where lease = $1`, pin).Scan(&boundary); err != nil || boundary.Year() != 2999 {
		t.Errorf("the record of the previous build's period holds %v (%v), want the test's of the year 2999: the previous build ran a pass", boundary, err)
	}
	for _, name := range []string{previousName, currentName} {
		reads, unexpected := fake.readsOf(name)
		t.Logf("the GitHub reads of the %s: %v; paths no route answers: %v", name, reads, unexpected)
	}
	if reads, _ := fake.readsOf(previousName); reads["pull"] > 0 {
		t.Errorf("the %s read pull requests by their number (%v): a pass of it ran in the test", previousName, reads)
	}
	t.Logf("the pull requests that the facts queries of the %s asked: %d", currentName, len(fake.askedOf(currentName)))
}
