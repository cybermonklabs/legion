package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// fakeAssetStore answers from assets, or with err, and records every key it is asked for.
type fakeAssetStore struct {
	assets map[string]string
	err    error
	keys   []string
}

func (s *fakeAssetStore) GetAsset(_ context.Context, key string) ([]byte, error) {
	s.keys = append(s.keys, key)
	if s.err != nil {
		return nil, s.err
	}
	asset, ok := s.assets[key]
	if !ok {
		return nil, ErrAssetNotFound
	}
	return []byte(asset), nil
}

// A local miss under /assets/ is answered from the asset store, and nothing else is: a local file
// wins, pages and other paths never consult it, and a router with no store answers every asset
// miss with a plain 404. Only a hit carries the immutable header. A path no file can have (a NUL
// byte, a name too long, a file where a directory must be) is a miss like any other, except that
// a NUL byte or invalid UTF-8 is never asked of the store, whose Postgres key cannot carry either.
// Nothing but a store failure logs an ERROR.
func TestStaticHandlerRetainedAssets(t *testing.T) {
	const retained = "console.log('previous build')"
	for _, tc := range []struct {
		name         string
		method       string
		path         string
		store        *fakeAssetStore // nil: the router has no asset store
		status       int
		body         string
		cacheControl string
		contentType  string
		keys         []string
	}{
		{
			name: "retained hit", path: "/assets/previous-build.js",
			store:  &fakeAssetStore{assets: map[string]string{"assets/previous-build.js": retained}},
			status: http.StatusOK, body: retained, cacheControl: assetCacheControl,
			contentType: "text/javascript; charset=utf-8", keys: []string{"assets/previous-build.js"},
		},
		{
			name: "retained hit by HEAD", method: http.MethodHead, path: "/assets/previous-build.js",
			store:  &fakeAssetStore{assets: map[string]string{"assets/previous-build.js": retained}},
			status: http.StatusOK, cacheControl: assetCacheControl,
			contentType: "text/javascript; charset=utf-8", keys: []string{"assets/previous-build.js"},
		},
		{
			name: "local file wins", path: "/assets/current.js",
			store:  &fakeAssetStore{assets: map[string]string{"assets/current.js": "retained"}},
			status: http.StatusOK, body: "current", cacheControl: assetCacheControl,
		},
		{
			name: "absent object", path: "/assets/never-retained.js", store: &fakeAssetStore{},
			status: http.StatusNotFound, keys: []string{"assets/never-retained.js"},
		},
		{
			name: "NUL in an asset name", path: "/assets/foo%00.js",
			store:  &fakeAssetStore{assets: map[string]string{"assets/foo\x00.js": retained}},
			status: http.StatusNotFound,
		},
		{
			name: "NUL in an asset name, no store", path: "/assets/foo%00.js",
			status: http.StatusNotFound,
		},
		{
			// The store answers as Postgres refuses a key that is not UTF-8: asking it would be a
			// 502 and an ERROR for a name no build can hold.
			name: "invalid UTF-8 in an asset name", path: "/assets/%ff.js",
			store:  &fakeAssetStore{err: errors.New(`read dashboard asset "assets/\xff.js": ERROR: invalid byte sequence for encoding "UTF8": 0xff (SQLSTATE 22021)`)},
			status: http.StatusNotFound,
		},
		{
			name: "non-ASCII asset name", path: "/assets/caf%C3%A9.js",
			store:  &fakeAssetStore{assets: map[string]string{"assets/café.js": retained}},
			status: http.StatusOK, body: retained, cacheControl: assetCacheControl,
			contentType: "text/javascript; charset=utf-8", keys: []string{"assets/café.js"},
		},
		{
			name: "asset name too long", path: "/assets/" + strings.Repeat("a", 300) + ".js", store: &fakeAssetStore{},
			status: http.StatusNotFound, keys: []string{"assets/" + strings.Repeat("a", 300) + ".js"},
		},
		{
			name: "asset below a file", path: "/assets/current.js/more.js", store: &fakeAssetStore{},
			status: http.StatusNotFound, keys: []string{"assets/current.js/more.js"},
		},
		{
			name: "store failure", path: "/assets/previous-build.js",
			store:  &fakeAssetStore{err: errors.New("object store unavailable")},
			status: http.StatusBadGateway, keys: []string{"assets/previous-build.js"},
		},
		{
			name: "no store", path: "/assets/previous-build.js",
			status: http.StatusNotFound,
		},
		{
			name: "non-asset file", path: "/previous-build.js",
			store:  &fakeAssetStore{assets: map[string]string{"previous-build.js": retained}},
			status: http.StatusNotFound,
		},
		{
			name: "asset root itself", path: "/assets",
			store:  &fakeAssetStore{assets: map[string]string{"assets": retained}},
			status: http.StatusNotFound,
		},
		{
			name: "NUL in a non-asset file", path: "/previous-build%00.js", store: &fakeAssetStore{},
			status: http.StatusNotFound,
		},
		{
			name: "page", path: "/issues/CORE-1",
			store:  &fakeAssetStore{assets: map[string]string{"issues/CORE-1": retained}},
			status: http.StatusOK, body: "<!doctype html>", cacheControl: pageCacheControl,
		},
		{
			name: "NUL in a page", path: "/issues/CORE-1%00", store: &fakeAssetStore{},
			status: http.StatusOK, body: "<!doctype html>", cacheControl: pageCacheControl,
		},
		{
			name: "page name too long", path: "/" + strings.Repeat("a", 300), store: &fakeAssetStore{},
			status: http.StatusOK, body: "<!doctype html>", cacheControl: pageCacheControl,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logtest.SetDefault(t, slog.NewJSONHandler(&logs, nil))

			webDist := t.TempDir()
			if err := os.MkdirAll(filepath.Join(webDist, "assets"), 0o700); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{"index.html": "<!doctype html>", "assets/current.js": "current"} {
				if err := os.WriteFile(filepath.Join(webDist, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			handler, context := newTestRouter(t)
			context.WebDistDir = webDist
			if tc.store != nil {
				context.AssetStore = tc.store
			}
			server := httptest.NewServer(handler)
			defer server.Close()

			method := tc.method
			if method == "" {
				method = http.MethodGet
			}
			request, err := http.NewRequest(method, server.URL+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatalf("%s %s: %v", method, tc.path, err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read %s: %v", tc.path, err)
			}

			if response.StatusCode != tc.status || (tc.body != "" && string(body) != tc.body) {
				t.Fatalf("%s %s: status %d body %q, want %d %q", method, tc.path, response.StatusCode, body, tc.status, tc.body)
			}
			if got := response.Header.Get("Cache-Control"); got != tc.cacheControl {
				t.Errorf("Cache-Control %q, want %q", got, tc.cacheControl)
			}
			if tc.contentType != "" {
				if got := response.Header.Get("Content-Type"); got != tc.contentType {
					t.Errorf("Content-Type %q, want %q", got, tc.contentType)
				}
			}
			if method == http.MethodHead {
				if len(body) != 0 || response.Header.Get("Content-Length") != strconv.Itoa(len(retained)) {
					t.Errorf("HEAD: %d body bytes, Content-Length %q, want none and %d", len(body), response.Header.Get("Content-Length"), len(retained))
				}
			}
			var keys []string
			if tc.store != nil {
				keys = tc.store.keys
			}
			if !slices.Equal(keys, tc.keys) {
				t.Errorf("store asked for %q, want %q", keys, tc.keys)
			}
			if tc.status != http.StatusBadGateway {
				for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
					var record struct {
						Level string `json:"level"`
					}
					if json.Unmarshal([]byte(line), &record) == nil && record.Level == "ERROR" {
						t.Errorf("ERROR record for a %d: %s", tc.status, line)
					}
				}
			}
		})
	}
}

// A stat the filesystem itself fails, here in a directory the server may not search, is a 500 and
// never a miss: a build the server cannot read is not a file that is not there, so neither the
// store nor the SPA shell answers for it.
func TestStaticHandlerAnswers500WhenTheFilesystemFails(t *testing.T) {
	webDist := t.TempDir()
	locked := filepath.Join(webDist, "assets", "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if _, err := os.Stat(filepath.Join(locked, "chunk.js")); !errors.Is(err, os.ErrPermission) {
		t.Skipf("stat in a directory with no permissions answered %v, not a permission error (running as root?)", err)
	}
	store := &fakeAssetStore{}
	handler, context := newTestRouter(t)
	context.WebDistDir = webDist
	context.AssetStore = store

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/locked/chunk.js", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("unreadable build: status %d body %q, want 500", recorder.Code, recorder.Body.String())
	}
	if len(store.keys) != 0 {
		t.Fatalf("store asked for %q, want nothing", store.keys)
	}
}

// slowClient is a browser on a slow link: each write it is sent waits before it lands.
type slowClient struct {
	*httptest.ResponseRecorder
	delay time.Duration
}

func (c slowClient) Write(p []byte) (int, error) {
	time.Sleep(c.delay)
	return c.ResponseRecorder.Write(p)
}

// The fetch bound is the store's, not the browser's: a client that takes longer than the bound to
// receive a retained asset the store answered at once still receives all of it, as it would the
// same file served from the local build.
func TestStaticHandlerDeliversAWholeRetainedAssetToASlowClient(t *testing.T) {
	body := bytes.Repeat([]byte("/"), 1<<20)
	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	context.AssetStore = &countingStore{object: body}

	recorder := httptest.NewRecorder()
	// 32 KiB copies at 120 ms each take about 3.8 s for 1 MiB, past the store's 3 s bound.
	handler.ServeHTTP(slowClient{ResponseRecorder: recorder, delay: 120 * time.Millisecond},
		httptest.NewRequest(http.MethodGet, "/assets/previous-build.js", nil))

	if recorder.Code != http.StatusOK || recorder.Body.Len() != len(body) {
		t.Fatalf("slow client: status %d, received %d of %d bytes", recorder.Code, recorder.Body.Len(), len(body))
	}
}

// A store that stops answering until the shared fetch bound runs out is the same refusal the memory
// cap gives, 503, not a 502 store failure: the store was never shown to be broken, only asked too
// late to answer, and the client never gets a 200 carrying a year's immutable caching.
func TestStaticHandlerAnswers503ForARetainedAssetThatStallsPastTheFetchBound(t *testing.T) {
	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	context.AssetStore = latentStore{delay: time.Hour, object: []byte("never")}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/previous-build.js", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("stalled store: status %d with %d body bytes, want 503", recorder.Code, recorder.Body.Len())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("stalled store: Cache-Control %q, want no-store", got)
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("stalled store: Retry-After %q, want 1", got)
	}
}

// A failing store is otherwise invisible from inside the task: the browser fails the chunk as it
// would a 404. Each failure leaves one ERROR record naming the key and the store's error.
func TestStaticHandlerLogsARetainedAssetStoreFailure(t *testing.T) {
	var logs bytes.Buffer
	logtest.SetDefault(t, slog.NewJSONHandler(&logs, nil))

	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	context.AssetStore = &fakeAssetStore{err: errors.New("object store unavailable")}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/previous-build.js", nil))

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("store failure: status %d, want 502", recorder.Code)
	}
	var failures int
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record struct {
			Level string `json:"level"`
			Key   string `json:"key"`
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(line), &record) == nil && record.Level == "ERROR" &&
			record.Key == "assets/previous-build.js" && strings.Contains(record.Error, "object store unavailable") {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("store failure: %d ERROR records naming the key and the error, want 1; log %q", failures, logs.String())
	}
}

// countingStore answers every key with one shared object and counts the calls.
type countingStore struct {
	object []byte
	calls  atomic.Int32
}

func (s *countingStore) GetAsset(context.Context, string) ([]byte, error) {
	s.calls.Add(1)
	return s.object, nil
}

// stalledClient is a browser that stops reading: its first write blocks until the client goes
// away, and then fails, as a write to a closed connection does.
type stalledClient struct {
	header  http.Header
	status  int
	writing chan struct{} // closed at the first write
	gone    chan struct{} // closed when the client goes away
	served  chan struct{} // closed when the handler returns
	once    sync.Once
}

func (c *stalledClient) Header() http.Header    { return c.header }
func (c *stalledClient) WriteHeader(status int) { c.status = status }
func (c *stalledClient) Write([]byte) (int, error) {
	c.once.Do(func() { close(c.writing) })
	<-c.gone
	return 0, errors.New("client went away")
}

// receiver waits for one receive from ch and fails t, naming what, when it never comes:
// testwait.Receive outside a synctest bubble, and bubbleReceive inside one.
type receiver func(t testing.TB, what string, ch <-chan struct{}) struct{}

// bubbleReceive is the receiver for a synctest bubble, where the shared wait cannot run (it reads
// the binary's deadline, which a bubble's t refuses): once synctest.Wait returns, every other
// goroutine of the bubble is blocked, so a value that has not come by then never will.
func bubbleReceive(t testing.TB, what string, ch <-chan struct{}) struct{} {
	t.Helper()
	synctest.Wait()
	select {
	case value := <-ch:
		return value
	default:
		t.Fatalf("%s never came: every other goroutine of the bubble is blocked", what)
		return struct{}{}
	}
}

// stallRetainedReaders starts n clients that each get a retained asset and stop reading it,
// one at a time, and returns once all n are writing; receive is how it waits for each (receiver).
// Each goes away when the test ends.
func stallRetainedReaders(t *testing.T, handler http.Handler, n int, receive receiver) []*stalledClient {
	t.Helper()
	clients := make([]*stalledClient, n)
	for i := range clients {
		client := &stalledClient{header: http.Header{}, writing: make(chan struct{}), gone: make(chan struct{}), served: make(chan struct{})}
		clients[i] = client
		go func() {
			defer close(client.served)
			handler.ServeHTTP(client, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/assets/stalled-%d.js", i), nil))
		}()
		receive(t, "the stalled reader's handler to reach its first write", client.writing)
		if client.status != http.StatusOK {
			t.Fatalf("stalled reader %d: status %d, want 200", i, client.status)
		}
	}
	t.Cleanup(func() {
		for _, client := range clients {
			select {
			case <-client.gone:
			default:
				close(client.gone)
			}
			// A bubble's row has waited for its readers itself, before the bubble ended.
			select {
			case <-client.served:
			default:
				receive(t, "the stalled reader's handler to return once its client went away", client.served)
			}
		}
	})
	return clients
}

// A retained object stays in memory until its client has read it, and nothing bounds how long a
// client takes, so the bytes held at once are capped. A request that finds no room before its
// fetch bound runs out is refused without asking the store: 503, not kept, retried after a second.
func TestStaticHandlerCapsRetainedBytesHeldAtOnce(t *testing.T) {
	var logs bytes.Buffer
	logtest.SetDefault(t, slog.NewJSONHandler(&logs, nil))

	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	store := &countingStore{object: bytes.Repeat([]byte("/"), int(MaxRetainedAssetSize))}
	context.AssetStore = store
	stallRetainedReaders(t, handler, int(maxRetainedBytesHeld/MaxRetainedAssetSize), testwait.Receive[struct{}])

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/one-more.js", nil))

	if held := int64(store.calls.Load()) * MaxRetainedAssetSize; held > maxRetainedBytesHeld {
		t.Errorf("the store handed out %d bytes still held, past the %d-byte cap", held, maxRetainedBytesHeld)
	}
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("past the cap: status %d with %d body bytes, want 503", recorder.Code, recorder.Body.Len())
	}
	if got := store.calls.Load(); got != int32(maxRetainedBytesHeld/MaxRetainedAssetSize) {
		t.Errorf("past the cap: the store was asked %d times, want %d (no call for the refused request)", got, maxRetainedBytesHeld/MaxRetainedAssetSize)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("past the cap: Cache-Control %q, want no-store", got)
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Errorf("past the cap: Retry-After %q, want 1", got)
	}
	var warnings int
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record struct {
			Level string `json:"level"`
			Key   string `json:"key"`
		}
		if json.Unmarshal([]byte(line), &record) == nil && record.Level == "WARN" && record.Key == "assets/one-more.js" {
			warnings++
		}
	}
	if warnings != 1 {
		t.Errorf("past the cap: %d WARN records naming the key, want 1; log %q", warnings, logs.String())
	}
}

// A client that goes away mid-response gives its bytes back, so the next request is served.
func TestStaticHandlerReleasesRetainedBytesWhenAClientGoesAway(t *testing.T) {
	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	store := &countingStore{object: bytes.Repeat([]byte("/"), int(MaxRetainedAssetSize))}
	context.AssetStore = store
	clients := stallRetainedReaders(t, handler, int(maxRetainedBytesHeld/MaxRetainedAssetSize), testwait.Receive[struct{}])

	full := httptest.NewRecorder()
	handler.ServeHTTP(full, httptest.NewRequest(http.MethodGet, "/assets/while-full.js", nil))
	if full.Code != http.StatusServiceUnavailable {
		t.Fatalf("at the cap: status %d, want 503", full.Code)
	}

	close(clients[0].gone)
	<-clients[0].served

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/after.js", nil))
	if recorder.Code != http.StatusOK || int64(recorder.Body.Len()) != MaxRetainedAssetSize {
		t.Fatalf("after a client went away: status %d with %d body bytes, want 200 with %d", recorder.Code, recorder.Body.Len(), MaxRetainedAssetSize)
	}
}

// A failed read gives back what it reserved: more failures than the cap holds objects, then a hit.
func TestStaticHandlerReleasesRetainedBytesWhenTheStoreFails(t *testing.T) {
	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	failing := &fakeAssetStore{err: errors.New("object store unavailable")}
	context.AssetStore = failing
	for i := range int(maxRetainedBytesHeld/MaxRetainedAssetSize) + 1 {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/assets/failing-%d.js", i), nil))
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("failure %d: status %d, want 502", i, recorder.Code)
		}
	}

	context.AssetStore = &fakeAssetStore{assets: map[string]string{"assets/after.js": "after"}}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/after.js", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "after" {
		t.Fatalf("after the store failed: status %d body %q, want 200 \"after\"", recorder.Code, recorder.Body.String())
	}
}

// latentStore answers as a store across a network does: each object arrives after a delay, or the
// read fails with its context's error when that ends first. The delay is a stand-in's own, the
// round trip to the object store the handler reads through (Context.AssetStore), which the
// handler bounds with retainedAssetFetchTimeout; it is not a bound of the test's.
type latentStore struct {
	delay  time.Duration
	object []byte
}

func (s latentStore) GetAsset(ctx context.Context, _ string) ([]byte, error) {
	select {
	case <-time.After(s.delay):
		return s.object, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// A stale tab opening a route asks for every chunk the route imports at once (the dashboard's
// issue page imports eight), and another tab can be doing the same. Small chunks hold almost
// nothing of the memory cap, so a burst of them is served rather than refused.
func TestStaticHandlerServesABurstOfSmallRetainedAssets(t *testing.T) {
	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	context.AssetStore = latentStore{delay: 50 * time.Millisecond, object: []byte("console.log('previous build')")}

	const burst = 16
	codes := make([]int, burst)
	var wg sync.WaitGroup
	for i := range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/assets/chunk-%d.js", i), nil))
			codes[i] = recorder.Code
		}()
	}
	testwait.Returns(t, fmt.Sprintf("the %d requests of the burst to be answered", burst), wg.Wait)

	refused := 0
	for _, code := range codes {
		if code != http.StatusOK {
			refused++
		}
	}
	if refused != 0 {
		t.Fatalf("a burst of %d small retained assets: %d not served (statuses %v), want all 200", burst, refused, codes)
	}
}

// A request keeps only its object's size once it is read, so stalled readers of real chunk sizes
// share the cap rather than each holding the most one object can be: 32 stalled readers of the
// dashboard's largest chunk, 1,747,955 bytes, fit under the cap, and every one is served.
func TestStaticHandlerHoldsOnlyEachRetainedObjectsSize(t *testing.T) {
	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	const size, readers = 1747955, 32
	if size*readers > maxRetainedBytesHeld {
		t.Fatalf("%d readers of %d bytes exceed the %d-byte cap; the test needs them to fit", readers, size, maxRetainedBytesHeld)
	}
	context.AssetStore = &countingStore{object: bytes.Repeat([]byte("/"), size)}

	stallRetainedReaders(t, handler, readers, testwait.Receive[struct{}])
}

// A fetch that runs out of the shared three-second bound before the store answers — most often
// because most of it was already spent waiting for memory-cap room, leaving the store call only
// milliseconds to finish — is the same 503 the cap's own refusal gives, not a 502 store failure:
// the store was never actually unhealthy, only asked too late to answer in time. The request's own
// context carries a deadline shorter than retainedAssetFetchTimeout, standing in for a bound that
// ran out mostly waiting on the memory cap, so the fetch's shared ctx expires in milliseconds
// rather than the full three seconds.
//
// It runs in a synctest bubble: the request's deadline and the store's answer are the bubble's
// fake time, which moves only once every goroutine waits, so the 20 ms deadline ends the fetch
// before the store's second whatever the machine's load (the handler and the store do no network
// I/O).
func TestStaticHandlerAnswers503WhenTheFetchRunsOutOfTheSharedBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		shortBound, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		var logs bytes.Buffer
		logtest.SetDefault(t, slog.NewJSONHandler(&logs, nil))

		handler, appContext, stop := bubbleTestRouter(t)
		defer stop()
		appContext.WebDistDir = t.TempDir()
		appContext.AssetStore = latentStore{delay: time.Second, object: []byte("late")}

		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/assets/late-joiner.js", nil).WithContext(shortBound)
		handler.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("fetch past the shared bound: status %d with %d body bytes, want 503", recorder.Code, recorder.Body.Len())
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("fetch past the shared bound: Cache-Control %q, want no-store", got)
		}
		if got := recorder.Header().Get("Retry-After"); got != "1" {
			t.Errorf("fetch past the shared bound: Retry-After %q, want 1", got)
		}
		var warnings, errorRecords int
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			var record struct {
				Level string `json:"level"`
				Key   string `json:"key"`
			}
			if json.Unmarshal([]byte(line), &record) != nil || record.Key != "assets/late-joiner.js" {
				continue
			}
			switch record.Level {
			case "WARN":
				warnings++
			case "ERROR":
				errorRecords++
			}
		}
		if warnings != 1 {
			t.Errorf("fetch past the shared bound: %d WARN records naming the key, want 1; log %q", warnings, logs.String())
		}
		if errorRecords != 0 {
			t.Errorf("fetch past the shared bound: %d ERROR records naming the key, want 0; log %q", errorRecords, logs.String())
		}
	})
}

// neverWrittenResponseWriter fails the test the moment anything is written to it: a canceled
// request has nobody left to answer, so the handler must never call WriteHeader or Write.
type neverWrittenResponseWriter struct {
	t      *testing.T
	header http.Header
}

func (w *neverWrittenResponseWriter) Header() http.Header { return w.header }

func (w *neverWrittenResponseWriter) WriteHeader(status int) {
	w.t.Fatalf("client gone while queued: WriteHeader(%d) called, want no response", status)
}

func (w *neverWrittenResponseWriter) Write(p []byte) (int, error) {
	w.t.Fatalf("client gone while queued: Write(%q) called, want no response", p)
	return 0, nil
}

// A client that disconnects while its request is still queued for memory-cap room is not a cap
// refusal: the cap can be entirely empty, there is nobody left to answer, and the handler writes
// nothing and logs no WARN (the cap-refusal line would otherwise double as a false capacity alarm
// on every ordinary aborted page load).
func TestStaticHandlerWritesNoResponseWhenTheClientCancelsWhileQueuedForTheMemoryCap(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	var logs bytes.Buffer
	logtest.SetDefault(t, slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	handler, appContext := newTestRouter(t)
	appContext.WebDistDir = t.TempDir()
	appContext.AssetStore = &fakeAssetStore{}

	req := httptest.NewRequest(http.MethodGet, "/assets/aborted.js", nil).WithContext(canceled)
	handler.ServeHTTP(&neverWrittenResponseWriter{t: t, header: http.Header{}}, req)

	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record struct {
			Level string `json:"level"`
		}
		if json.Unmarshal([]byte(line), &record) == nil && record.Level == "WARN" {
			t.Fatalf("client gone while queued: a WARN was logged, want none; log %q", logs.String())
		}
	}
}

// A navigation cancels the chunks the page was still loading. A request whose client goes away
// while the store reads its object is answered nothing and logs no ERROR: the store did not fail,
// and a store failure line on every navigation would be a false alarm. The read it started still
// finishes, for the next request of the same chunk.
func TestStaticHandlerWritesNoResponseWhenTheClientCancelsWhileTheStoreReads(t *testing.T) {
	var logs bytes.Buffer
	logtest.SetDefault(t, slog.NewJSONHandler(&logs, nil))

	handler, appContext := newTestRouter(t)
	appContext.WebDistDir = t.TempDir()
	store := gatedStore{object: []byte("console.log('lib')"), reading: make(chan struct{}, 2), release: make(chan struct{})}
	appContext.AssetStore = store
	var releaseOnce sync.Once
	releaseStore := func() { releaseOnce.Do(func() { close(store.release) }) }
	t.Cleanup(releaseStore)
	navigatedAway, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The handler runs off the test goroutine, so a handler that waits for the read its client
	// left fails the row by name below rather than hanging it; what it wrote is read once it has
	// returned.
	answered := &answerRecorder{header: http.Header{}}
	served := make(chan struct{})
	go func() {
		defer close(served)
		handler.ServeHTTP(answered, httptest.NewRequest(http.MethodGet, "/assets/lib.js", nil).WithContext(navigatedAway))
	}()
	// The client goes away once the store is reading the object, and the read is held until the
	// request has returned, so the cancel lands inside the read on every run.
	testwait.Receive(t, "the store's read of the chunk to begin", store.reading)
	cancel()
	testwait.Receive(t, "the request whose client went away to return while the store still reads", served)
	if answered.status != 0 || answered.body.Len() != 0 {
		t.Fatalf("client gone while the store read: status %d and body %q written, want no response", answered.status, answered.body.String())
	}

	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record struct {
			Level string `json:"level"`
		}
		if json.Unmarshal([]byte(line), &record) == nil && (record.Level == "ERROR" || record.Level == "WARN") {
			t.Fatalf("client gone while the store read: a %s was logged, want none; log %q", record.Level, logs.String())
		}
	}
	releaseStore()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/lib.js", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "console.log('lib')" {
		t.Fatalf("the next request: status %d body %q, want 200 with the object", recorder.Code, recorder.Body.String())
	}
}

// gatedStore answers as latentStore does, with a gate for its delay: each read reports on reading
// as it begins and holds until release is closed or its context ends, so a row acts once the store
// is reading rather than after a span that only stands for it.
type gatedStore struct {
	object  []byte
	reading chan struct{}
	release chan struct{}
}

func (s gatedStore) GetAsset(ctx context.Context, _ string) ([]byte, error) {
	s.reading <- struct{}{}
	select {
	case <-s.release:
		return s.object, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// answerRecorder records what a handler wrote, for a row that runs the handler off the test
// goroutine and reads the record on it once the handler has returned.
type answerRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *answerRecorder) Header() http.Header { return w.header }

func (w *answerRecorder) WriteHeader(status int) { w.status = status }

func (w *answerRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

// blockingStore holds every read until release is closed, then answers a key under
// assets/missing- with ErrAssetNotFound and any other with object. It reports each read's key on
// arrived as the read starts, and records the most reads it held at once.
type blockingStore struct {
	object  []byte
	arrived chan string
	release chan struct{}

	mu          sync.Mutex
	inFlight    int
	maxInFlight int
}

func (s *blockingStore) GetAsset(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	s.inFlight++
	s.maxInFlight = max(s.maxInFlight, s.inFlight)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()
	s.arrived <- key
	<-s.release
	if strings.HasPrefix(key, "assets/missing-") {
		return nil, ErrAssetNotFound
	}
	return s.object, nil
}

// A navigation, or a client that means harm, abandons each request the moment it reaches the
// store. The read it started runs on (readRetainedAsset), and its reservation stays until that read
// ends, so abandoned requests for distinct missing chunks never hold more store reads at once than
// the memory cap has room for. Once those reads end, every reservation comes back: the cap's whole
// room is served again.
//
// It runs in a synctest bubble, since the handler and the store do no network I/O: once
// synctest.Wait returns, every request either holds a read in the store or waits for a
// reservation, so a read that has not arrived by then never will.
func TestStaticHandlerHoldsTheReservationOfAnAbandonedRequestUntilItsReadEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		handler, appContext, stop := bubbleTestRouter(t)
		defer stop()
		appContext.WebDistDir = t.TempDir()
		const requests = 20
		store := &blockingStore{
			object:  bytes.Repeat([]byte("/"), int(MaxRetainedAssetSize)),
			arrived: make(chan string, requests+int(maxRetainedBytesHeld/MaxRetainedAssetSize)),
			release: make(chan struct{}),
		}
		var releaseOnce sync.Once
		releaseStore := func() { releaseOnce.Do(func() { close(store.release) }) }
		defer releaseStore()
		appContext.AssetStore = store

		cancels := make(map[string]context.CancelFunc, requests)
		var served sync.WaitGroup
		for i := range requests {
			ctx, cancel := context.WithCancel(context.Background())
			key := fmt.Sprintf("assets/missing-%d.js", i)
			cancels[key] = cancel
			served.Add(1)
			go func() {
				defer served.Done()
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/"+key, nil).WithContext(ctx))
			}()
		}
		// Abandon each request as its read reaches the store, until every request is blocked
		// with no read left to arrive: past the cap's room, the rest wait for a reservation and
		// reach the store no more. synctest.Wait returns before the bubble's clock moves, which it
		// does only once every goroutine is blocked, so once no read arrives the clock moves on to
		// just short of the requests' fetch bound (retainedAssetFetchTimeout, from their start at
		// the bubble's first moment), and what arrived meanwhile is abandoned in turn: a
		// reservation that a timer of its own gives back before its read ends lets a waiting
		// request's read reach the store inside that span. An hour, which costs nothing elsewhere,
		// would end the waiting requests first, at their bound, with a 503 that reads nothing.
		moved := false
		for {
			synctest.Wait()
			select {
			case key := <-store.arrived:
				cancels[key]()
				continue
			default:
			}
			if moved {
				break
			}
			time.Sleep(retainedAssetFetchTimeout - time.Nanosecond)
			moved = true
		}
		for _, cancel := range cancels {
			cancel()
		}
		served.Wait()

		store.mu.Lock()
		most := store.maxInFlight
		store.mu.Unlock()
		if room := int(maxRetainedBytesHeld / MaxRetainedAssetSize); most > room {
			t.Errorf("%d abandoned requests held %d store reads at once, want at most the cap's room of %d", requests, most, room)
		}

		releaseStore()
		// Each stalled reader holds the most one object can, so all of them together need every
		// byte of the cap back; one still held is a 503 once its fetch bound runs out.
		clients := stallRetainedReaders(t, handler, int(maxRetainedBytesHeld/MaxRetainedAssetSize), bubbleReceive)
		// The bubble ends only once every goroutine in it has, which is before its cleanups run,
		// so the stalled clients go here.
		for _, client := range clients {
			close(client.gone)
			<-client.served
		}
	})
}

// bubbleTestRouter is newTestRouter for a synctest bubble, with a document service of the
// bubble's own. A bubble ends only once every goroutine in it has, and that check comes before
// its cleanups run, so the row defers stop, which waits out the reads a request left running
// (retainedAssetFetchTimeout of the bubble's clock) and shuts the document service down.
func bubbleTestRouter(t *testing.T) (http.Handler, *AppContext, func()) {
	t.Helper()
	people := newMemoryPeople()
	documents := docs.New(docs.Deps{Events: events.NewBroker()})
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey: testSigningKey,
		People:     people,
		Identity:   identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		Lifetime:   t.Context(),
		Docs:       documents,
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	standInEventLog(ctx, 0)
	handler, _ := New(ctx)
	return handler, ctx, func() {
		time.Sleep(retainedAssetFetchTimeout)
		_ = documents.Shutdown(context.Background())
	}
}
