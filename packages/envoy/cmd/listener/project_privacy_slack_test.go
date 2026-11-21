package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/webhook"
)

const slackTestSecret = "slack-test-secret"

// slackPublished records what the Slack intake published.
type slackPublished struct {
	mu    sync.Mutex
	items []contracts.Envelope
}

func (p *slackPublished) PublishWithin(_ context.Context, item contracts.Envelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items = append(p.items, item)
	return nil
}

func (p *slackPublished) topics() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	topics := make([]string, len(p.items))
	for i, item := range p.items {
		topics[i] = item.Topic
	}
	return topics
}

// slackEvent posts a signed Slack message event of channel to handler and answers its status.
func slackEvent(t *testing.T, handler http.Handler, channel string) int {
	t.Helper()
	return slackPayload(t, handler, channel, false)
}

// slackPayload posts a signed Slack message event of channel to handler, a reply in a thread when
// threaded, and answers its status.
func slackPayload(t *testing.T, handler http.Handler, channel string, threaded bool) int {
	t.Helper()
	thread := ""
	if threaded {
		thread = `,"thread_ts":"1700000000.000100"`
	}
	body := `{"type":"event_callback","event_id":"ev-` + channel + `","team_id":"T01234567","event":{"type":"message","channel":"` + channel + `","user":"U01234567","text":"hello","ts":"1700000001.000200"` + thread + `}}`
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(slackTestSecret))
	mac.Write([]byte("v0:" + timestamp + ":" + body))
	request := httptest.NewRequest(http.MethodPost, "/webhook/slack", strings.NewReader(body))
	request.Header.Set("X-Slack-Request-Timestamp", timestamp)
	request.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code
}

// requiredSlackIntake is the Slack intake of a required-mode listener reading source, publishing
// into published, its counters on met.
func requiredSlackIntake(t *testing.T, source *privacySource, met *metrics.Registry, published *slackPublished) http.Handler {
	t.Helper()
	logger := logging.New("test-machine")
	privacy := &listenerPrivacy{required: true, counters: newPrivacyCounters(met), logger: logger}
	privacy.k7 = newK7Reader(source.server.URL, listenerBrokerAccount(edgeAccountToken), logger)
	return webhook.SlackHandler(slackTestSecret, published, webhook.WithSlackPrivacy(privacy))
}

// TestTheSlackIntakeWaitsForAValidRead: in the required mode a Slack event whose read does not
// answer - the source failing, answering a project with no slack_channels, or a read that times
// out - is answered 503, which Slack retries, and nothing of it is published to the stream.
// Controls: the intake of an all-public listener publishes the same event on
// notifications.slack.>, and so does an intake that takes a timed-out read as "no private
// project", so each row fails for the read it names.
func TestTheSlackIntakeWaitsForAValidRead(t *testing.T) {
	source := newPrivacySource(t, privacyNone)
	for name, fail := range map[string]func() (restore func()){
		"the source failing": func() func() {
			source.failWith(http.StatusBadGateway)
			return func() { source.answer(privacyNone) }
		},
		"a project with no slack_channels": func() func() {
			source.answer(`{"projects":[{"project":"PRIV"}]}`)
			return func() { source.answer(privacyNone) }
		},
		"a read that times out": func() func() { return source.hold() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(fail())
			published := &slackPublished{}
			if status := slackEvent(t, requiredSlackIntake(t, source, metrics.New(), published), "C0PUB00001"); status != http.StatusServiceUnavailable {
				t.Fatalf("%s: the intake answered %d, want 503", name, status)
			}
			if topics := published.topics(); len(topics) != 0 {
				t.Fatalf("%s: the intake published %v, want nothing", name, topics)
			}
		})
	}
	published := &slackPublished{}
	if status := slackEvent(t, webhook.SlackHandler(slackTestSecret, published), "C0PUB00001"); status != http.StatusOK {
		t.Fatalf("control: the all-public intake answered %d", status)
	}
	if topics := published.topics(); len(topics) == 0 || !strings.HasPrefix(topics[0], "notifications.slack.") {
		t.Fatalf("control: the all-public intake published %v, want notifications.slack.", topics)
	}
	// Control: a timed-out read taken as "no private project" publishes the event to the stream.
	release := source.hold()
	defer release()
	lenient := &slackPublished{}
	privacy := &listenerPrivacy{required: true, counters: newPrivacyCounters(metrics.New()), logger: logging.New("test-machine")}
	privacy.k7 = newK7Reader(source.server.URL, listenerBrokerAccount(edgeAccountToken), privacy.logger)
	privacy.k7.timeout = 300 * time.Millisecond
	if status := slackEvent(t, webhook.SlackHandler(slackTestSecret, lenient, webhook.WithSlackPrivacy(timeoutAsNoPrivateProject{privacy})), "C0PUB00001"); status != http.StatusOK {
		t.Fatalf("control: the intake that takes a timeout as no private project answered %d", status)
	}
	if topics := lenient.topics(); len(topics) == 0 || !strings.HasPrefix(topics[0], "notifications.slack.") {
		t.Fatalf("control: the intake that takes a timeout as no private project published %v, want notifications.slack.", topics)
	}
}

// timeoutAsNoPrivateProject is the control's privacy: privacy's answer, with a read that fails
// taken as no private project listing the channel.
type timeoutAsNoPrivateProject struct{ *listenerPrivacy }

func (p timeoutAsNoPrivateProject) SlackChannelProjects(ctx context.Context, channel string) ([]string, error) {
	projects, err := p.listenerPrivacy.SlackChannelProjects(ctx, channel)
	if err != nil {
		return nil, nil
	}
	return projects, nil
}

// everyChannelPrivate is the no-private-project row's control: an intake that routes every
// channel's event to PRIV's lane, as private-lane routing applied with no project private would.
type everyChannelPrivate struct{ *listenerPrivacy }

func (everyChannelPrivate) SlackChannelProjects(context.Context, string) ([]string, error) {
	return []string{"PRIV"}, nil
}

// TestTheSlackIntakeIsTodaysWithNoPrivateProject: with no project private (the privacy read
// answers {"projects":[]}), the required-mode Slack intake publishes every channel's events -
// the channel event and a thread reply's copy - on exactly the subjects, under exactly the
// dedupe keys, that the intake of main publishes today. Control: the private-lane routing applied
// with no private project changes the subject, and the comparison fails naming it.
func TestTheSlackIntakeIsTodaysWithNoPrivateProject(t *testing.T) {
	source := newPrivacySource(t, privacyNone)
	keys := func(handler http.Handler, published *slackPublished, channel string, threaded bool) []string {
		t.Helper()
		if status := slackPayload(t, handler, channel, threaded); status != http.StatusOK {
			t.Fatalf("the intake answered %d for %s", status, channel)
		}
		published.mu.Lock()
		defer published.mu.Unlock()
		out := make([]string, 0, len(published.items))
		for _, item := range published.items {
			out = append(out, item.Topic+" "+item.DedupeKey)
		}
		published.items = nil
		return out
	}
	for _, channel := range []string{"C0PUB00001", "C0PRIV00001", "D0DIRECT01"} {
		for _, threaded := range []bool{false, true} {
			today, required := &slackPublished{}, &slackPublished{}
			want := keys(webhook.SlackHandler(slackTestSecret, today), today, channel, threaded)
			if threaded && !slices.ContainsFunc(want, func(key string) bool { return strings.Contains(key, ".thread.") }) {
				t.Fatalf("channel %s: today's intake published no thread copy of a reply: %v", channel, want)
			}
			got := keys(requiredSlackIntake(t, source, metrics.New(), required), required, channel, threaded)
			if len(want) == 0 || !slices.Equal(got, want) {
				t.Errorf("channel %s (thread %v): the required-mode intake published %v, want today's %v", channel, threaded, got, want)
			}
		}
	}
	routed := &slackPublished{}
	privacy := &listenerPrivacy{required: true, counters: newPrivacyCounters(metrics.New())}
	today := &slackPublished{}
	want := keys(webhook.SlackHandler(slackTestSecret, today), today, "C0PUB00001", false)
	got := keys(webhook.SlackHandler(slackTestSecret, routed, webhook.WithSlackPrivacy(everyChannelPrivate{privacy})), routed, "C0PUB00001", false)
	if slices.Equal(got, want) {
		t.Fatalf("control: private-lane routing applied with no private project published %v, today's subjects: the comparison cannot see a changed subject", got)
	}
}

// TestASlackEventOfAPrivateChannelGoesOnItsProjectsLane: an event of a channel PRIV's
// slack_channels lists is published on PRIV's own Slack lane, notifications.legion.priv.slack.…,
// and nothing of it on notifications.slack.>. Control: an event of a channel no private project
// lists is published on notifications.slack.> as before.
func TestASlackEventOfAPrivateChannelGoesOnItsProjectsLane(t *testing.T) {
	source := newPrivacySource(t, privacyListingPRIV)
	published := &slackPublished{}
	intake := requiredSlackIntake(t, source, metrics.New(), published)
	if status := slackEvent(t, intake, "C0PRIV00001"); status != http.StatusOK {
		t.Fatalf("the intake answered %d for PRIV's channel", status)
	}
	topics := published.topics()
	if len(topics) == 0 {
		t.Fatal("the intake published nothing for PRIV's channel")
	}
	for _, topic := range topics {
		if !strings.HasPrefix(topic, "notifications.legion.priv.slack.T01234567.C0PRIV00001.") {
			t.Fatalf("an event of PRIV's channel went on %q, want PRIV's own Slack lane", topic)
		}
		if project, _ := contracts.SubjectProject(topic); project != "PRIV" {
			t.Fatalf("%q reads as project %q, want PRIV", topic, project)
		}
	}
	control := &slackPublished{}
	if status := slackEvent(t, requiredSlackIntake(t, source, metrics.New(), control), "C0PUB00001"); status != http.StatusOK {
		t.Fatalf("control: the intake answered %d for a public channel", status)
	}
	if topics := control.topics(); len(topics) == 0 || !strings.HasPrefix(topics[0], "notifications.slack.T01234567.C0PUB00001.") {
		t.Fatalf("control: a public channel's event went on %v, want notifications.slack.", topics)
	}
}

// TestASlackChannelTwoProjectsListGetsNoLane: an event of a channel two private projects list is
// acknowledged, published on no lane, and counted in envoy_listener_slack_withheld_total. Control:
// with one project listing it, the event is published on that project's lane.
func TestASlackChannelTwoProjectsListGetsNoLane(t *testing.T) {
	source := newPrivacySource(t, `{"projects":[{"project":"PRIV","slack_channels":["C0SHARED01"],"slack_channels_changed_at":null},{"project":"SEC","slack_channels":["c0shared01 "],"slack_channels_changed_at":null}]}`)
	met := metrics.New()
	published := &slackPublished{}
	if status := slackEvent(t, requiredSlackIntake(t, source, met, published), "C0SHARED01"); status != http.StatusOK {
		t.Fatalf("the intake answered %d for a channel two projects list, want 200", status)
	}
	if topics := published.topics(); len(topics) != 0 {
		t.Fatalf("the intake published %v for a channel two projects list, want nothing", topics)
	}
	recorder := httptest.NewRecorder()
	met.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if withheld := counterValue(t, recorder.Body.String(), `envoy_listener_slack_withheld_total{reason="shared_channel"}`); withheld != 1 {
		t.Fatalf("envoy_listener_slack_withheld_total is %d, want 1", withheld)
	}
	source.answer(`{"projects":[{"project":"PRIV","slack_channels":["C0SHARED01"],"slack_channels_changed_at":null}]}`)
	control := &slackPublished{}
	if status := slackEvent(t, requiredSlackIntake(t, source, metrics.New(), control), "C0SHARED01"); status != http.StatusOK {
		t.Fatalf("control: the intake answered %d", status)
	}
	if topics := control.topics(); len(topics) == 0 || !strings.HasPrefix(topics[0], "notifications.legion.priv.slack.") {
		t.Fatalf("control: one project's channel went on %v, want PRIV's lane", topics)
	}
}
