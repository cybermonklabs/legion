package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// allPublicSkips names each GET row of the table that every_get_route_answers_byte_for_byte does
// not send, with why. Every GET row has a drive (TestEveryGETRowHasADrive), so it is empty; a row
// added later with no drive fails the walk until it has one or a reason here.
var allPublicSkips = map[string]string{}

// allPublicVolatile is the members of a GET row's answer, beside its drive's own volatile members,
// that two servers' answers on one state differ in: the delivery reads stamp the moment each one
// computed its answer. The read drive sends no GET that writes twice, so its drives list none.
var allPublicVolatile = map[string][]volatileMember{
	"GET /api/v1/delivery/timeline": {{"measures.computed_at", readTime}},
	"GET /api/v1/delivery/measures": {{"computed_at", readTime}},
	"GET /api/v1/delivery/pipeline": {{"computed_at", readTime}},
}

// allPublicPair is the two servers TestAllPublicDeploymentUnchanged compares on one seeded
// database: the baseline, whose request store sends the viewer's settings as the owner and so
// meets no policy (storetest.RequestAsOwner: main's behaviour), and the server production builds,
// on the request store as dispatch_app (testDeps).
type allPublicPair struct {
	baseline, request *rlsServer
}

// newAllPublicPair builds the pair on the drive world's database, each from the drive rig's own
// dependencies with its stores replaced, so both serve the GitHub, the listener, the broker and
// the test hooks the drive's state was written with. documents, when set, gives each server a
// document service of its own on the system store.
func newAllPublicPair(t *testing.T, world *driveWorld, documents bool, change func(*Deps)) allPublicPair {
	t.Helper()
	build := func(request *store.Store) *rlsServer {
		deps := world.rig.deps
		deps.Store, deps.System = request, world.raw
		if documents {
			service := docs.New(docs.Deps{Store: world.raw, Events: deps.Events, ServerURL: testServerURL, Identity: headerIdentity(world.raw), Settle: time.Hour})
			t.Cleanup(func() {
				if err := service.Shutdown(context.Background()); err != nil {
					t.Errorf("shut the document service down: %v", err)
				}
			})
			deps.Docs = service
		}
		if change != nil {
			change(&deps)
		}
		mux := newRecordingMux()
		served := register(mux, t.Context(), deps)
		served.readWorkers = &backgroundWork{}
		t.Cleanup(func() { waitBackgroundOf(t, served) })
		return &rlsServer{handler: mux, served: served, deps: deps, mux: mux, database: world.raw}
	}
	baseline := build(storetest.RequestAsOwner(t, world.raw))
	return allPublicPair{baseline: baseline, request: build(testDeps(t, world.raw).Store)}
}

// TestAllPublicDeploymentUnchanged (plan R5): in a deployment with no private project and no
// limited member, the server on the request store answers what main answers. The state is the read
// drive's seeded store (newDriveWorld), every kind of state a GET row reads; each row compares the
// server production builds with a baseline server on the same database whose request store meets
// no policy (storetest.RequestAsOwner). Each subtest builds its own pair on that database.
func TestAllPublicDeploymentUnchanged(t *testing.T) {
	t.Parallel()
	world := newDriveWorld(t)
	writer := world.brokerSend("the drive's writing token", world.writer)

	t.Run("every_get_route_answers_byte_for_byte", func(t *testing.T) {
		pair := newAllPublicPair(t, world, false, nil)
		drives := maps.Clone(readRowDrives)
		maps.Copy(drives, getWriteDrives)
		var names []string
		for _, route := range pair.request.served.routes() {
			if route.Method != http.MethodGet {
				continue
			}
			name := route.Method + " " + route.Pattern
			if _, driven := drives[name]; !driven {
				if reason, skipped := allPublicSkips[name]; skipped {
					t.Logf("%s is not sent: %s", name, reason)
				} else {
					t.Errorf("%s has no drive (readRowDrives, getWriteDrives) and no reason in allPublicSkips", name)
				}
				continue
			}
			names = append(names, name)
		}
		for _, name := range sendOrder(t, names, drives) {
			drive := drives[name]
			target := world.fill(t, drive.target)
			baseline := pair.baseline.serve(t, writer.request(t, target))
			pair.baseline.awaitWork(t)
			request := pair.request.serve(t, writer.request(t, target))
			pair.request.awaitWork(t)
			if want := max(drive.status, http.StatusOK); baseline.Code != want {
				t.Errorf("GET %s: the baseline answered %d %s, and the drive's state answers %d, so the comparison proves nothing",
					target, baseline.Code, cutBody(baseline.Body.String()), want)
			}
			if difference := allPublicDifference(request, baseline, append(slices.Clone(drive.volatile), allPublicVolatile[name]...)); difference != "" {
				t.Errorf("GET %s: %s", target, difference)
			}
		}
		t.Logf("compared %d GET rows", len(names))
	})
	t.Run("writes_answer_alike", func(t *testing.T) {
		// Two databases, one each, each written the same writes from the same state: the
		// baseline's and the request store's answers must carry one status and one code.
		baselineDatabase, requestDatabase := storetest.Open(t), storetest.Open(t)
		for _, database := range []*store.Store{baselineDatabase, requestDatabase} {
			seedPeople(t, database, rlsOwner)
		}
		baseline := newRLSServer(t, baselineDatabase, rlsServerConfig{asOwner: true})
		request := newRLSServer(t, requestDatabase, rlsServerConfig{})
		writes := []struct {
			what, method, target string
			body                 any
		}{
			{"a project's create", http.MethodPost, "/api/v1/projects", map[string]any{"key": "ALIKE", "name": "Alike"}},
			{"a refused project create", http.MethodPost, "/api/v1/projects", map[string]any{"key": "not a key", "name": "Refused"}},
			{"an issue's create", http.MethodPost, "/api/v1/issues", map[string]any{"project": "ALIKE", "title": "The first issue", "spec": "# Spec\n\nThe first.\n", "force": true}},
			{"a second issue's create", http.MethodPost, "/api/v1/issues", map[string]any{"project": "ALIKE", "title": "The second issue", "force": true}},
			{"a comment", http.MethodPost, "/api/v1/issues/ALIKE-1/comments", map[string]any{"body": "A remark."}},
			{"a status change", http.MethodPost, "/api/v1/issues/ALIKE-1/status-change", map[string]any{"status": "todo"}},
			{"a link", http.MethodPatch, "/api/v1/issues/ALIKE-2", map[string]any{"blocked_by": []string{"ALIKE-1"}}},
			{"a refused link", http.MethodPatch, "/api/v1/issues/ALIKE-1", map[string]any{"blocked_by": []string{"ALIKE-2"}}},
		}
		for _, write := range writes {
			want := baseline.as(t, rlsOwner, write.method, write.target, write.body)
			got := request.as(t, rlsOwner, write.method, write.target, write.body)
			wantCode, _ := answerCode(want)
			gotCode, _ := answerCode(got)
			if got.Code != want.Code || gotCode != wantCode {
				t.Errorf("%s: the request store answers %d %q (%s), and the baseline %d %q (%s)",
					write.what, got.Code, gotCode, cutBody(got.Body.String()), want.Code, wantCode, cutBody(want.Body.String()))
			}
		}
	})
	t.Run("keyword_and_meaning_search_answer_alike", func(t *testing.T) {
		embedder := &fakeEmbedder{vectors: map[string][]float32{rlsMeaningQuery: defaultVector()}}
		processAllPending(t, world.raw, embedder)
		pair := newAllPublicPair(t, world, false, func(deps *Deps) { deps.Embedder = embedder })
		volatile := []volatileMember{{"took_ms", searchTook}}
		for _, search := range []struct{ what, target string }{
			{"the keyword search", "/api/v1/search?q=drive"},
			{"the meaning search", "/api/v1/search?q=" + strings.ReplaceAll(rlsMeaningQuery, " ", "+")},
		} {
			baseline := pair.baseline.serve(t, writer.request(t, search.target))
			request := pair.request.serve(t, writer.request(t, search.target))
			var found struct {
				Results  []json.RawMessage `json:"results"`
				Degraded string            `json:"degraded"`
			}
			if err := json.Unmarshal(baseline.Body.Bytes(), &found); err != nil || baseline.Code != http.StatusOK || len(found.Results) == 0 || found.Degraded != "" {
				t.Errorf("%s: the baseline answered %d %s, which holds no result of a search that ran whole, so the comparison proves nothing",
					search.what, baseline.Code, cutBody(baseline.Body.String()))
			}
			if difference := allPublicDifference(request, baseline, volatile); difference != "" {
				t.Errorf("%s: %s", search.what, difference)
			}
		}
	})
	t.Run("live_events_and_document_socket_frames_alike", func(t *testing.T) {
		pair := newAllPublicPair(t, world, true, nil)
		head := eventHead(t, world.raw)
		issue := world.values["issue"]
		var feeds []*socketClient
		for _, s := range []*rlsServer{pair.baseline, pair.request} {
			listener := httptest.NewServer(s.handler)
			t.Cleanup(listener.Close)
			client := openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(listener.URL, "http")+liveSocketPattern, asUser(rlsOwner)).servedBy(world.raw).read()
			subscribeEvents(t, client, "e1", &head)
			feeds = append(feeds, client)
		}
		renameIssue(t, world.rig.handler, issue, "The drive reads every row, renamed")
		var frames [][]byte
		for _, client := range feeds {
			frame, _ := eventOfType(t, client, "e1", "issue.updated")
			encoded, err := json.Marshal(frame)
			if err != nil {
				t.Fatalf("encode the event frame: %v", err)
			}
			frames = append(frames, encoded)
		}
		if !bytes.Equal(frames[0], frames[1]) {
			t.Errorf("the live event frame differs: the request store's server sent\n%s\nand the baseline's\n%s", frames[1], frames[0])
		}
		spec := strings.TrimSpace(world.values["spec"])
		var answers [][]byte
		for _, s := range []*rlsServer{pair.baseline, pair.request} {
			listener := httptest.NewServer(s.handler)
			t.Cleanup(listener.Close)
			url := "ws" + strings.TrimPrefix(listener.URL, "http") + "/ws/doc/" + spec + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
			peer := docstest.Dial(t, url, asUser(rlsOwner), spec, crdt.New())
			answers = append(answers, testwait.Receive(t, "the room's answer to the peer's sync step 1", peer.Answers))
		}
		if !bytes.Equal(answers[0], answers[1]) {
			t.Errorf("the document socket's sync step 2 differs: the request store's server sent %d bytes, the baseline's %d", len(answers[1]), len(answers[0]))
		}
	})
}

// answerCode is the code a refusal's body carries, and "" for an answer that carries none.
func answerCode(response *httptest.ResponseRecorder) (string, bool) {
	var refusal struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &refusal); err != nil {
		return "", false
	}
	return refusal.Code, refusal.Code != ""
}

// allPublicDifference says how got, the request store's answer, differs from want, the baseline's,
// and is empty when the two have one status and the same bytes, or, where the bytes differ, the
// same JSON once the members volatile lists are left out of both.
func allPublicDifference(got, want *httptest.ResponseRecorder, volatile []volatileMember) string {
	if got.Code != want.Code {
		return fmt.Sprintf("the request store answers %d %s, and the baseline %d %s", got.Code, cutBody(got.Body.String()), want.Code, cutBody(want.Body.String()))
	}
	if bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) {
		return ""
	}
	var gotBody, wantBody any
	if json.Unmarshal(got.Body.Bytes(), &gotBody) != nil || json.Unmarshal(want.Body.Bytes(), &wantBody) != nil {
		return fmt.Sprintf("the request store's body is\n%s\nand the baseline's\n%s", cutBody(got.Body.String()), cutBody(want.Body.String()))
	}
	left := make([]string, 0, len(volatile))
	for _, member := range volatile {
		dropMember(gotBody, strings.Split(member.path, "."))
		dropMember(wantBody, strings.Split(member.path, "."))
		left = append(left, member.path+" ("+member.reason+")")
	}
	differences := memberDifferences("", gotBody, wantBody, nil)
	switch {
	case len(differences) > 0:
		return fmt.Sprintf("the request store's answer, but for [%s], differs from the baseline's in %d members (the request store's value first):\n  %s",
			strings.Join(left, "; "), len(differences), strings.Join(differences, "\n  "))
	case len(volatile) == 0:
		return fmt.Sprintf("the two bodies hold the same JSON in other bytes:\n%s\nand\n%s", cutBody(got.Body.String()), cutBody(want.Body.String()))
	}
	return ""
}
