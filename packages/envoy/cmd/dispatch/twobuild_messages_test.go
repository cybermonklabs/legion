//go:build twobuild

// The bus half of the two-build suite: the messages and subscription settings the two builds pass
// and reconcile while both serve one NATS (twobuild_harness_test.go holds the harness both halves
// start their builds with).

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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testnats"
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
// did it. The first commit is the handler's last call to GitHub before it writes the pull
// request's rows and acknowledges the message, and the fake takes delay to answer it, which spreads
// the messages' handling over a stop. A handling counts as done when that answer is sent, so a
// handling counted done can still be unacknowledged; one whose process went away while it waited
// (the request's context ended) is counted as abandoned instead, since a message a process stopped
// handling unfinished is one NATS must deliver again. Anything else is answered with an empty list.
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
		_, _ = fmt.Fprintf(w, `{"number":%s,"title":"A pull request","user":{"login":"octocat"},"labels":[],"merged_at":"2026-10-27T00:00:00Z","additions":1,"deletions":1}`, r.PathValue("number"))
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
		f.mu.Lock()
		f.done[number] = append(f.done[number], handling{build: build, at: time.Now()})
		f.mu.Unlock()
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
func publishMergedPullRequest(t *testing.T, js natsgo.JetStreamContext, number int, delivery string) uint64 {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"kind": "pr", "action": "closed", "merged": "true",
		"repo": "acme/deploy", "number": strconv.Itoa(number), "author": "octocat",
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
// take the durable over and for every one of numbers to be handled. It returns that hand-over: its
// time is after holder's process exited, so every handling holder did is before it.
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
	standbyBound := boundLines(standby, bus.DispatchDeliveryDurable)
	before := deliverSubjectOf(t, js, bus.DispatchDeliveryDurable)
	stop(t, holder)
	over := handOver{
		stopping: builds.buildName(holder), taking: builds.buildName(standby), at: time.Now(),
		sequences: sequences,
		window:    readBusSettings(t, js).Consumers[bus.DispatchDeliveryDurable].MaxAckPending,
	}
	t.Logf("stopped the %s with %d of %d pull requests handled", over.stopping, handledOf(), len(numbers))
	waitForBound(t, standby, over.taking, bus.DispatchDeliveryDurable, standbyBound, 60*time.Second)
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
// build, which holds no hold, delivers from the durable, a restarted process of this tree
// stands by rather than take the durable from it.
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

	// This tree, stopped above, comes back while the previous build delivers from the durable
	// under no hold, as every build before holds does: it stands by and never takes the binding
	// from it.
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
	// handled again is counted. A handling a stopping process abandoned is not one: NATS
	// delivers that message again, which is what at-least-once delivery is for.
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
