package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/webhook"
	"github.com/sjawhar/envoy/listenerwire"
)

// The limits the listener reads a request body to, written here as numbers so that this file
// compiles at the commit before them: the one every /v1 route reads to (apiBodyLimit), and each
// webhook route's own (webhook's githubMaxBody, slackMaxBody and ghostWisprMaxBody).
const (
	v1BodyLimit         = 4 << 20
	githubBodyLimit     = 25 << 20
	slackBodyLimit      = 1 << 20
	ghostWisprBodyLimit = 1 << 20
)

// bodyLimitRig is a listener's /v1 routes and its webhook routes, every webhook configured, over the
// stores of one NATS account, each built as main builds it (registerV1Routes, webhookRoutes,
// openWebhooks). logs is what the /v1 routes log.
type bodyLimitRig struct {
	client       *bus.Client
	sessions     *session.SessionRegistry
	v1           *http.ServeMux
	webhooks     http.Handler
	webhookPaths []string
	logs         *cmdtest.LockedBuffer
}

func newBodyLimitRig(t *testing.T) *bodyLimitRig {
	t.Helper()
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	legionAgents, err := legionagents.Open(t.Context(), client.Conn, legionagents.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the legion agent registry: %v", err)
	}
	ciStore, err := cistore.Open(t.Context(), client.Conn, logging.New("test"), cistore.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the CI store: %v", err)
	}
	t.Cleanup(ciStore.StopWatch)
	logs := &cmdtest.LockedBuffer{}
	v1 := http.NewServeMux()
	registerV1Routes(v1, withPublishRoute(t, &listenerDeps{
		client:       client,
		registry:     registry,
		sessions:     sessions,
		legionAgents: newLegionAgentRegistry(legionAgents),
		consumers:    setupSessionConsumers(t, client),
	}), "test-machine", logging.NewWithWriter("test", logs))
	hooks := webhookRoutes(&webhook.WebhookConfig{
		GitHub:     &webhook.GitHubWebhook{Secret: webhookSecret, MentionTrigger: "@legion"},
		Slack:      &webhook.SlackWebhook{Secret: webhookSecret},
		GhostWispr: &webhook.GhostWisprWebhook{Secret: webhookSecret},
		BodyBudget: webhook.DefaultBodyBudget,
	}, nil)
	gate := newStartingGate("webhook", logging.New("test"))
	openWebhooks(gate, hooks, client, ciStore)
	var webhookPaths []string
	for _, hook := range hooks {
		webhookPaths = append(webhookPaths, hook.path)
	}
	return &bodyLimitRig{client: client, sessions: sessions, v1: v1, webhooks: gate, webhookPaths: webhookPaths, logs: logs}
}

// register writes a live session entry for id, as a registration would.
func (rig *bodyLimitRig) register(t *testing.T, id string) {
	t.Helper()
	if err := rig.sessions.Put(id, session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine", Dir: "/test/" + id, Title: id}); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

// serve answers request as the listener does: a webhook path on the webhook routes, any other on
// the /v1 routes.
func (rig *bodyLimitRig) serve(request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	if slices.Contains(rig.webhookPaths, request.URL.Path) {
		rig.webhooks.ServeHTTP(recorder, request)
	} else {
		rig.v1.ServeHTTP(recorder, request)
	}
	return recorder
}

// storedState is every stream of the rig's account, KV buckets included, by its last sequence, and
// the notification stream's consumer count. A store write, a publish the stream keeps and the
// creation of a session consumer each change it.
func (rig *bodyLimitRig) storedState(t *testing.T) map[string]uint64 {
	t.Helper()
	state := map[string]uint64{}
	for name := range rig.client.JS().StreamNames() {
		info, err := rig.client.JS().StreamInfo(name)
		if err != nil {
			t.Fatalf("read stream %s: %v", name, err)
		}
		state[name] = info.State.LastSeq
		if name == bus.Stream {
			state[name+" consumers"] = uint64(info.State.Consumers)
		}
	}
	if len(state) == 0 {
		t.Fatal("the rig's account holds no stream")
	}
	return state
}

// padded is the JSON object object with a member "pad" of x's added, so that the whole is exactly
// size bytes. No route reads a member it does not know.
func padded(t *testing.T, object string, size int) string {
	t.Helper()
	open, closed := strings.CutSuffix(object, "}")
	fill := size - len(open) - len(`,"pad":""}`)
	if !closed || fill < 0 {
		t.Fatalf("cannot pad %.200s to %d bytes", object, size)
	}
	return open + `,"pad":"` + strings.Repeat("x", fill) + `"}`
}

// v1Request is a request to a /v1 route, as a client sends it.
func v1Request(method, target, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

// marshalled is value as JSON.
func marshalled(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %v: %v", value, err)
	}
	return string(encoded)
}

// bodyLimitRow is one route that reads a request body. build returns the route's request with a
// body of exactly size bytes that the route serves today, naming a session, role, event or
// delivery no earlier request named, and does first whatever the request needs, such as the
// registration an unsubscribe removes. real returns the largest real request a caller sends the
// route, as its comment measures it.
type bodyLimitRow struct {
	name, path string
	limit      int
	// webhook is set for a webhook route, which answers an error as text, where a /v1 route
	// answers {"error": …}.
	webhook bool
	// record names the line the route writes for each request (requestRecord), when it writes one.
	record string
	build  func(t *testing.T, size int) *http.Request
	real   func(t *testing.T) *http.Request
}

// Every listener route that reads a request body reads it to a limit, set above the largest real
// request of that route, and answers a body past it 413 with an error that names the limit, in bytes
// and in MiB, having done nothing else: no store write, no publish, no consumer. A body at the limit,
// and the largest real request, are served as any request under the limit is. The /v1 routes share
// one limit, 4 MiB: the largest real request is a send or a publish whose message and payload each
// come near NATS's 1 MiB max payload, about 2 MiB, since the envelope carries the payload and only
// a one-line summary of the message. Each webhook route has its own. The rows are every route that
// reads a body: /v1/messages/send and publish, /v1/interests/subscribe and unsubscribe,
// /v1/roles/set, the two Legion agent PUTs, and the GitHub, Slack and Ghost Wispr webhooks. The
// listener's other routes read no body. A /v1 body is read to the limit whole, past its first JSON
// value too: a small send followed by more than the limit of spaces is refused as a body past it.
// A body past the limit is refused whatever it holds: one that is not JSON, sent with its
// Content-Length and without one (chunked), is answered 413 as well, and not 400.
func TestARequestBodyOverTheLimitIsRefused(t *testing.T) {
	t.Parallel()
	rig := newBodyLimitRig(t)
	const (
		sendTarget = "ses_body_limit_send_target"
		teamTopic  = "notifications.team.body-limit"
		started    = "2026-11-16T03:15:00Z"
	)
	rig.register(t, sendTarget)
	count := 0
	next := func(prefix string) string {
		count++
		return fmt.Sprintf("%s-%d", prefix, count)
	}

	// The largest registration the live listener held on 2026-11-16, read from GET /v1/sessions and
	// GET /v1/interests/ there: 802 topics, their list 34,611 bytes of JSON, none longer than 90
	// bytes. No session's title was longer than 66 bytes, no directory longer than 86, and no
	// capability list longer than the three delivery modes.
	realTopics := make([]string, 802)
	for i := range realTopics {
		realTopics[i] = fmt.Sprintf("notifications.team.real-registration-%04d", i)
	}
	realTitle := strings.Repeat("t", 66)
	realDir := "/" + strings.Repeat("d", 85)
	registration := func(id string, topics []string, title, dir string) string {
		return marshalled(t, map[string]any{
			"session_id":         id,
			"dir":                dir,
			"title":              title,
			"topics":             topics,
			"port":               0,
			"capabilities":       []string{"aside", "btw", "steer"},
			"self_subscribed":    true,
			"delivery":           map[string]any{"consumer": true, "epoch": "epoch-" + id},
			"client_instance_id": id + "-instance",
			"host":               "body-limit-host",
			"plugin":             "@legion/pi-envoy",
			"plugin_version":     "0.0.0",
		})
	}
	subscribed := func(t *testing.T, id string, topics []string) {
		t.Helper()
		if answer := rig.serve(v1Request(http.MethodPost, "/v1/interests/subscribe", registration(id, topics, "Body limit", "/test/"+id))); answer.Code != http.StatusOK {
			t.Fatalf("register %s: status %d, body %.300s", id, answer.Code, answer.Body.String())
		}
	}
	// A message and a payload of 1,000,000 bytes each: the envelope holds the payload whole and
	// fits the 1 MiB max payload, so the body, about 2 MB, is served.
	realMessage := map[string]any{"source": "agent", "message": strings.Repeat("m", 1_000_000), "payload": strings.Repeat("p", 1_000_000)}
	// The largest Legion agent entry the registry takes: a 128-byte session id and a 1,024-byte
	// title (legionagents' maxSessionIDBytes and maxTitleBytes).
	largestEntry := func(id string) string {
		return marshalled(t, map[string]string{
			"session_id": id, "legion_project": "infra", "issue": "INFRA-3589", "role": "tester",
			"role_token": "legion-infra-infra-3589-tester", "title": strings.Repeat("T", 1024),
			"harness": "omp", "started_at": started,
		})
	}
	longID := func(prefix string) string {
		id := next(prefix)
		return id + strings.Repeat("x", 128-len(id))
	}
	comment := func(id string) string {
		return `{"action":"created","repository":{"name":"widgets","owner":{"login":"acme"},"full_name":"acme/widgets"},` +
			`"issue":{"number":7,"title":"t","html_url":"https://github.com/acme/widgets/issues/7"},` +
			`"comment":{"id":` + id + `,"body":"no mention here","html_url":"https://github.com/acme/widgets/issues/7#issuecomment-` + id + `","user":{"login":"someone","type":"User"}},` +
			`"sender":{"login":"someone","type":"User"}}`
	}
	slackEvent := func(eventID, text string) string {
		return `{"type":"event_callback","event_id":"` + eventID + `","team_id":"T01234567",` +
			`"event":{"type":"message","channel":"C01234567","user":"U01234567","text":"` + text + `",` +
			`"blocks":[{"type":"rich_text","block_id":"b","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"` + text + `"}]}]}],` +
			`"ts":"1700000000.000100"}}`
	}
	summary := func(session string) string {
		return `{"event_type":"summary_ready","created_at":"2026-11-16T04:16:29Z","payload":{"session_id":"` + session + `","type":"summary_ready",` +
			`"started_at":"2026-11-16T03:00:00Z","ended_at":"2026-11-16T04:00:00Z","duration":3600,` +
			`"drive_folder_url":"https://drive.google.com/drive/folders/1AbCdEfGhIjKlMnOpQrStUvWxYz012345",` +
			`"session_url":"https://ghostwispr.example.com/sessions/` + session + `","status":"completed",` +
			`"summary_preset":"meeting","summary_model":"model-1","timestamp":"2026-11-16T04:16:29Z","version":"1","payload_type":"summary_ready"}}`
	}

	rows := []bodyLimitRow{
		{
			name: "send", path: "/v1/messages/send", limit: v1BodyLimit, record: "listener send",
			build: func(t *testing.T, size int) *http.Request {
				return v1Request(http.MethodPost, "/v1/messages/send", padded(t, marshalled(t, map[string]any{
					"target_session": sendTarget, "source": "agent", "message": "a send of its size",
				}), size))
			},
			real: func(t *testing.T) *http.Request {
				body := maps.Clone(realMessage)
				body["target_session"] = sendTarget
				return v1Request(http.MethodPost, "/v1/messages/send", marshalled(t, body))
			},
		},
		{
			name: "publish", path: "/v1/messages/publish", limit: v1BodyLimit, record: "listener publish",
			build: func(t *testing.T, size int) *http.Request {
				return v1Request(http.MethodPost, "/v1/messages/publish", padded(t, marshalled(t, map[string]any{
					"topic": teamTopic, "source": "agent", "message": "a publish of its size",
				}), size))
			},
			real: func(t *testing.T) *http.Request {
				body := maps.Clone(realMessage)
				body["topic"] = teamTopic
				return v1Request(http.MethodPost, "/v1/messages/publish", marshalled(t, body))
			},
		},
		{
			name: "subscribe", path: "/v1/interests/subscribe", limit: v1BodyLimit,
			build: func(t *testing.T, size int) *http.Request {
				id := next("ses_body_limit_subscribe")
				return v1Request(http.MethodPost, "/v1/interests/subscribe", padded(t, registration(id, []string{teamTopic}, "Body limit", "/test/"+id), size))
			},
			real: func(t *testing.T) *http.Request {
				return v1Request(http.MethodPost, "/v1/interests/subscribe", registration(next("ses_body_limit_subscribe_real"), realTopics, realTitle, realDir))
			},
		},
		{
			name: "unsubscribe", path: "/v1/interests/unsubscribe", limit: v1BodyLimit,
			build: func(t *testing.T, size int) *http.Request {
				id := next("ses_body_limit_unsubscribe")
				subscribed(t, id, []string{teamTopic})
				return v1Request(http.MethodPost, "/v1/interests/unsubscribe", padded(t, marshalled(t, map[string]any{
					"session_id": id, "topics": []string{teamTopic}, "client_instance_id": id + "-instance",
				}), size))
			},
			// An unsubscribe of every topic of the largest registration.
			real: func(t *testing.T) *http.Request {
				id := next("ses_body_limit_unsubscribe_real")
				subscribed(t, id, realTopics)
				return v1Request(http.MethodPost, "/v1/interests/unsubscribe", marshalled(t, map[string]any{
					"session_id": id, "topics": realTopics, "client_instance_id": id + "-instance",
				}))
			},
		},
		{
			name: "role claim", path: "/v1/roles/set", limit: v1BodyLimit,
			build: func(t *testing.T, size int) *http.Request {
				id := next("ses_body_limit_claimant")
				rig.register(t, id)
				return v1Request(http.MethodPost, "/v1/roles/set", padded(t, marshalled(t, map[string]any{
					"session_id": id, "role": next("body-limit-role"),
				}), size))
			},
			// A soft claim naming every field envoy-client's setRole sends, its ids as long as a
			// session id is.
			real: func(t *testing.T) *http.Request {
				id := longID("ses_body_limit_real_claimant")[:36]
				rig.register(t, id)
				return v1Request(http.MethodPost, "/v1/roles/set", marshalled(t, map[string]any{
					"session_id": id, "role": fmt.Sprintf("legion-infra-infra-%d-implementer", 3589+count), "soft": true,
					"previous_session_id": strings.Repeat("p", 36), "client_instance_id": strings.Repeat("c", 36),
				}))
			},
		},
		{
			name: "legion agent batch", path: "/v1/legion/agents", limit: v1BodyLimit,
			build: func(t *testing.T, size int) *http.Request {
				return v1Request(http.MethodPut, "/v1/legion/agents", padded(t, `{"entries":[`+legionEntry(next("ses-body-limit-batch"), "INFRA-3589", "tester", started)+`]}`, size))
			},
			// The largest batch the registry takes: legionagents.MaxBatch entries, each the largest
			// entry.
			real: func(t *testing.T) *http.Request {
				entries := make([]string, legionagents.MaxBatch)
				for i := range entries {
					entries[i] = largestEntry(longID("ses-body-limit-real-batch"))
				}
				return v1Request(http.MethodPut, "/v1/legion/agents", `{"entries":[`+strings.Join(entries, ",")+`]}`)
			},
		},
		{
			name: "legion agent entry", path: "/v1/legion/agents/sessions/", limit: v1BodyLimit,
			build: func(t *testing.T, size int) *http.Request {
				id := next("ses-body-limit-entry")
				return v1Request(http.MethodPut, "/v1/legion/agents/sessions/"+id, padded(t, legionEntry(id, "INFRA-3589", "tester", started), size))
			},
			real: func(t *testing.T) *http.Request {
				id := longID("ses-body-limit-real-entry")
				return v1Request(http.MethodPut, "/v1/legion/agents/sessions/"+id, largestEntry(id))
			},
		},
		{
			name: "github webhook", path: "/webhook/github", limit: githubBodyLimit, webhook: true,
			build: func(t *testing.T, size int) *http.Request {
				id := strconv.Itoa(1000 + count)
				return githubDelivery("issue_comment", next("delivery-body-limit"), padded(t, comment(id), size))
			},
			// GitHub delivers a payload of up to 25 MB ("Payload cap",
			// docs.github.com/en/webhooks/webhook-events-and-payloads).
			real: func(t *testing.T) *http.Request {
				id := strconv.Itoa(1000 + count)
				return githubDelivery("issue_comment", next("delivery-body-limit-real"), padded(t, comment(id), 25_000_000))
			},
		},
		{
			name: "slack webhook", path: "/webhook/slack", limit: slackBodyLimit, webhook: true,
			build: func(t *testing.T, size int) *http.Request {
				return slackDelivery(padded(t, slackEvent(next("Ev_body_limit"), "a message of its size"), size))
			},
			// A message at the 40,000 characters Slack cuts a message's text to ("Truncating
			// content", docs.slack.dev/reference/methods/chat.postMessage), each a six-byte \u
			// escape, in its text and again in its blocks.
			real: func(t *testing.T) *http.Request {
				return slackDelivery(slackEvent(next("Ev_body_limit_real"), strings.Repeat(`\u00e9`, 40_000)))
			},
		},
		{
			name: "ghostwispr webhook", path: "/webhook/ghostwispr", limit: ghostWisprBodyLimit, webhook: true,
			build: func(t *testing.T, size int) *http.Request {
				return ghostWisprDelivery(next("gw-body-limit"), padded(t, summary(next("2026111603")), size))
			},
			// A summary_ready event carrying every field the envelope copies (ghostWisprPayload).
			real: func(t *testing.T) *http.Request {
				return ghostWisprDelivery(next("gw-body-limit-real"), summary(next("2026111604")))
			},
		},
	}

	// refused serves request, whose body is one byte past row's limit, and requires the route to
	// answer 413 naming the limit, having written, published and logged nothing else.
	refused := func(t *testing.T, row bodyLimitRow, request *http.Request) {
		t.Helper()
		before := rig.storedState(t)
		logged := rig.logs.Len()
		answer := rig.serve(request)
		body := answer.Body.String()
		if answer.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s with a body of %d bytes: status %d, body %.300s; want 413", row.path, row.limit+1, answer.Code, body)
		}
		refusal := body
		if !row.webhook {
			var answered listenerwire.APIError
			if err := json.Unmarshal(answer.Body.Bytes(), &answered); err != nil {
				t.Fatalf("%s: the refusal %.300s is not the route's JSON error: %v", row.path, body, err)
			}
			refusal = answered.Error
		}
		if names := fmt.Sprintf("%d bytes (%d MiB)", row.limit, row.limit>>20); !strings.Contains(refusal, names) {
			t.Fatalf("%s: the refusal reads %q, want it to name the limit, %s", row.path, refusal, names)
		}
		if after := rig.storedState(t); !maps.Equal(before, after) {
			t.Fatalf("%s refused the body and still wrote, published or made a consumer: the account's streams went from %v to %v", row.path, before, after)
		}
		if row.record != "" {
			wantRecord(t, rig.logs.String()[logged:], row.record, map[string]any{
				"outcome": "too_large", "status": float64(http.StatusRequestEntityTooLarge),
			})
		}
		t.Logf("%s refused %d bytes: 413 %s", row.path, row.limit+1, strings.TrimSpace(refusal))
	}
	// notJSON is row's request one byte past its limit, with its method, path and headers, and a
	// body of that size that is not JSON: it starts with x. chunked sends it with no
	// Content-Length, so the route learns the body's size only by reading it.
	notJSON := func(t *testing.T, row bodyLimitRow, chunked bool) *http.Request {
		t.Helper()
		request := row.build(t, row.limit+1)
		body := strings.Repeat("x", row.limit+1)
		request.Body = io.NopCloser(strings.NewReader(body))
		request.ContentLength = int64(len(body))
		if chunked {
			request.ContentLength = -1
			request.TransferEncoding = []string{"chunked"}
		}
		return request
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Run("one byte over the limit", func(t *testing.T) {
				refused(t, row, row.build(t, row.limit+1))
			})
			t.Run("one byte over the limit, not JSON, with its Content-Length", func(t *testing.T) {
				refused(t, row, notJSON(t, row, false))
			})
			t.Run("one byte over the limit, not JSON, chunked", func(t *testing.T) {
				refused(t, row, notJSON(t, row, true))
			})
			t.Run("at the limit", func(t *testing.T) {
				wantServed(t, rig, row, row.build(t, row.limit))
			})
			t.Run("the largest real request", func(t *testing.T) {
				wantServed(t, rig, row, row.real(t))
			})
		})
	}

	t.Run("send: a small value followed by more than the limit", func(t *testing.T) {
		small := marshalled(t, map[string]any{"target_session": sendTarget, "source": "agent", "message": "a send followed by spaces"})
		request := v1Request(http.MethodPost, "/v1/messages/send", small+strings.Repeat(" ", v1BodyLimit))
		// With no Content-Length the route reads the body to find its size, so the refusal is
		// the read's past the first JSON value, not the refusal of a Content-Length past the limit.
		request.ContentLength = -1
		request.TransferEncoding = []string{"chunked"}
		before := rig.storedState(t)
		logged := rig.logs.Len()
		answer := rig.serve(request)
		if answer.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("a send of %d bytes followed by %d spaces: status %d, body %.300s; want 413", len(small), v1BodyLimit, answer.Code, answer.Body.String())
		}
		if after := rig.storedState(t); !maps.Equal(before, after) {
			t.Fatalf("the send refused its body and still wrote or published: the account's streams went from %v to %v", before, after)
		}
		wantRecord(t, rig.logs.String()[logged:], "listener send", map[string]any{
			"outcome": "too_large", "status": float64(http.StatusRequestEntityTooLarge),
		})
	})
}

// failsAfter is a request body that reads as data and then fails, as a client that resets its
// connection partway through its body does.
type failsAfter struct {
	data string
	read bool
}

func (f *failsAfter) Read(p []byte) (int, error) {
	if f.read {
		return 0, errors.New("connection reset by peer")
	}
	f.read = true
	return copy(p, f.data), nil
}

// A /v1 body is judged by what its decode met and what the read of its rest met, one of each:
// either one past the limit is a 413 naming it, any other failure of either is a 400
// "invalid json", and a body both read cleanly is served. Each case is sent with no
// Content-Length, so the route learns the body's size only by reading it.
func TestAV1BodyIsJudgedByItsDecodeAndTheReadOfItsRest(t *testing.T) {
	t.Parallel()
	overTheLimit := strings.Repeat(" ", v1BodyLimit)
	for _, row := range []struct {
		name    string
		body    io.Reader
		status  int
		message string
	}{
		{name: "the decode and the rest read cleanly", body: strings.NewReader(`{"a":1}`), status: 0, message: ""},
		{name: "the rest runs past the limit", body: strings.NewReader(`{"a":1}` + overTheLimit), status: http.StatusRequestEntityTooLarge, message: apiBodyTooLarge},
		{name: "the rest fails to read", body: &failsAfter{data: `{"a":1}`}, status: http.StatusBadRequest, message: "invalid json"},
		{name: "a syntax error, and the rest reads cleanly", body: strings.NewReader(`{"a":x} and more`), status: http.StatusBadRequest, message: "invalid json"},
		{name: "a syntax error, and the rest runs past the limit", body: strings.NewReader(`x` + overTheLimit), status: http.StatusRequestEntityTooLarge, message: apiBodyTooLarge},
		{name: "the decode runs past the limit", body: strings.NewReader(`"` + strings.Repeat("x", v1BodyLimit) + `"`), status: http.StatusRequestEntityTooLarge, message: apiBodyTooLarge},
		{name: "an empty body", body: strings.NewReader(""), status: http.StatusBadRequest, message: "invalid json"},
	} {
		t.Run(row.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/messages/send", row.body)
			request.ContentLength = -1
			var into any
			status, message := readAPIBody(httptest.NewRecorder(), request, &into)
			if status != row.status || message != row.message {
				t.Fatalf("readAPIBody = %d %q, want %d %q", status, message, row.status, row.message)
			}
		})
	}
}

// wantServed serves request and asserts the route served it as it serves any request: 200, with
// the store write or publish the route makes.
func wantServed(t *testing.T, rig *bodyLimitRig, row bodyLimitRow, request *http.Request) {
	t.Helper()
	size := request.ContentLength
	before := rig.storedState(t)
	answer := rig.serve(request)
	if answer.Code != http.StatusOK {
		t.Fatalf("%s with a body of %d bytes: status %d, body %.300s; want 200", row.path, size, answer.Code, answer.Body.String())
	}
	if after := rig.storedState(t); maps.Equal(before, after) {
		t.Fatalf("%s answered 200 to a body of %d bytes and wrote nothing: the account's streams stayed %v", row.path, size, before)
	}
	t.Logf("%s served %d bytes: 200", row.path, size)
}

// wantRecord asserts that logs hold exactly one line named msg, carrying fields.
func wantRecord(t *testing.T, logs, msg string, fields map[string]any) {
	t.Helper()
	var found []map[string]any
	for line := range strings.Lines(logs) {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == msg {
			found = append(found, record)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the request logged %d %q lines, want one:\n%s", len(found), msg, logs)
	}
	for field, want := range fields {
		if found[0][field] != want {
			t.Fatalf("the %q line's %s = %#v, want %#v: %v", msg, field, found[0][field], want, found[0])
		}
	}
}
