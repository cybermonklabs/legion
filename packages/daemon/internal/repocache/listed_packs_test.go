package repocache

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/repocache/fakegcs"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// packsStart is the time each packsRig's clock starts at, which its cache's first switch reads
// unless the row moves the clock first.
var packsStart = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

// packsRig is one cache of acme/widgets that keeps its packs in the test bucket of a double of its
// own, in emulator mode, as the real-binary harness runs it: driven refresh by refresh with the
// real git against a local remote, its switches and retirement reading a fake clock, its handler
// served over real HTTP on a loopback address of its own, and the double read apart from the
// cache's code (reader) at a clock of the test's (doubleClock), which the double checks a signed
// download's expiry by.
type packsRig struct {
	remote      string
	run         *testRunner
	opts        Options
	log         *syncLog
	double      *fakegcs.Server
	running     *fakegcs.Running
	doubleClock *bucketClock
	reader      *bucketRig
	clock       *clocktest.Fake
	c           *cache
	url         string
}

// newPacksRig is a rig whose cache has not refreshed yet.
func newPacksRig(t *testing.T) *packsRig {
	t.Helper()
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Bucket = testBucket
	log := &syncLog{}
	opts.Log = log.log
	doubleClock := &bucketClock{}
	double, running := startDouble(t, fakegcs.Options{Email: EmulatorEmail, Now: doubleClock.Now})
	t.Setenv(emulatorHostEnv, running.Addr())
	r := &packsRig{
		remote: remote, run: run, opts: opts, log: log, double: double, running: running, doubleClock: doubleClock,
		reader: &bucketRig{bucketDouble: bucketDouble{double: double, running: running}, clock: doubleClock},
	}
	r.restart(t, clocktest.New(packsStart))
	return r
}

// restart puts a new process's cache on the rig's volume in place of the one it had, its switches
// reading clk, and serves its handler.
func (r *packsRig) restart(t *testing.T, clk *clocktest.Fake) {
	t.Helper()
	c := cacheOn(t, r.opts)
	c.clock = clk
	t.Cleanup(c.closeStore)
	r.c, r.clock, r.url = c, clk, serveHandler(t, c.handler())
}

// serveHandler serves handler on a loopback address of the test's own (testnats.ListenLoopback)
// until the test ends, and answers its base address.
func serveHandler(t *testing.T, handler http.Handler) string {
	t.Helper()
	listener, err := testnats.ListenLoopback()
	if err != nil {
		t.Fatalf("listen for the cache: %v", err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return "http://" + listener.Addr().String()
}

// refresh runs one refresh of the cache, failing the test where it fails.
func (r *packsRig) refresh(t *testing.T) {
	t.Helper()
	if err := r.c.refresh(testwait.Context(t)); err != nil {
		t.Fatalf("refresh: %v; log %q", err, r.log.all())
	}
}

// repackDue makes the next refresh repack the clone, as a repackEvery since the last repack does.
func (r *packsRig) repackDue() {
	r.c.lastRepack = time.Now().Add(-repackEvery - time.Hour)
}

// get is GET path of the cache, presenting bearer where it is not empty: the status, the headers
// and the body read whole.
func (r *packsRig) get(t *testing.T, path, bearer string) (int, http.Header, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(testwait.Context(t), http.MethodGet, r.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read GET %s: %v", path, err)
	}
	return response.StatusCode, response.Header, body
}

// packs is GET /packs with the cache's token: its status, and its manifest where it is 200.
func (r *packsRig) packs(t *testing.T) (int, workspace.PackManifest) {
	t.Helper()
	status, _, body := r.get(t, "/packs", testCacheToken)
	var manifest workspace.PackManifest
	if status == http.StatusOK {
		if err := json.Unmarshal(body, &manifest); err != nil {
			t.Fatalf("/packs answered 200 with no manifest: %v", err)
		}
	}
	return status, manifest
}

// requirePacks is /packs's manifest, failing the test unless it answered 200 with a manifest every
// object of which is whole in the bucket with its pair (requireWhole).
func (r *packsRig) requirePacks(t *testing.T) workspace.PackManifest {
	t.Helper()
	status, manifest := r.packs(t)
	if status != http.StatusOK {
		t.Fatalf("/packs answered %d, not 200; log %q", status, r.log.all())
	}
	r.requireWhole(t, manifest)
	return manifest
}

// requireWhole fails the test unless every pack manifest lists has its index listed beside it and
// every index its pack, and every object it lists is in the double whole, as the double's JSON API
// reads it apart from the cache: the size and sha256 metadata the manifest states. It is the one
// invariant every answer must hold: no pack without its index, and no object that is not uploaded.
func (r *packsRig) requireWhole(t *testing.T, manifest workspace.PackManifest) {
	t.Helper()
	if manifest.Bucket != testBucket {
		t.Errorf("the manifest names bucket %q, not %s", manifest.Bucket, testBucket)
	}
	names := map[string]bool{}
	for _, object := range manifest.Objects {
		names[object.Name] = true
	}
	for _, object := range manifest.Objects {
		stem, pack := strings.CutSuffix(object.Name, ".pack")
		other := stem + ".idx"
		if !pack {
			other = strings.TrimSuffix(object.Name, ".idx") + ".pack"
		}
		if !names[other] {
			t.Errorf("the manifest lists %s without %s", object.Name, other)
		}
		status, res := r.reader.resource(t, object.Name)
		if status != http.StatusOK || res.Size != formatInt(object.Size) || res.Metadata[sha256Metadata] != object.SHA256 {
			t.Errorf("the manifest lists %s (%d bytes, SHA-256 %s), which the bucket reads %d, %s bytes, sha256 %q", object.Name, object.Size, object.SHA256, status, res.Size, res.Metadata[sha256Metadata])
		}
	}
}

// requireStill fails the test unless /packs answers want, whole, while what holds.
func (r *packsRig) requireStill(t *testing.T, want listing, what string) {
	t.Helper()
	requireListing(t, listingOf(t, r.requirePacks(t)), want, what)
}

// requireServes fails the test unless /tar streams a whole tar and /healthz answers ready.
func (r *packsRig) requireServes(t *testing.T, what string) Health {
	t.Helper()
	if status, _, body := r.get(t, "/tar", testCacheToken); status != http.StatusOK || len(body) == 0 {
		t.Errorf("/tar %s answered %d with %d bytes, not 200 and a tar", what, status, len(body))
	}
	status, _, body := r.get(t, "/healthz", "")
	var health Health
	if err := json.Unmarshal(body, &health); err != nil || status != http.StatusOK || !health.Ready {
		t.Errorf("/healthz %s answered %d %s, not 200 and ready", what, status, body)
	}
	return health
}

// stray makes the double hold an object under name, as an upload that is not this process's would
// leave it, with the sha256 metadata of its bytes, and answers its generation.
func (r *packsRig) stray(t *testing.T, name string) int64 {
	t.Helper()
	content := randomContent(t, 4<<10)
	return r.reader.seed(t, name, content, map[string]string{sha256Metadata: sha256Of(content)})
}

// lose deletes name's object from the double behind the cache's back, as a lifecycle rule of the
// bucket or another writer would, through the double's JSON API apart from the cache's code,
// and fails the test unless the double deleted it.
func (r *packsRig) lose(t *testing.T, name string) {
	t.Helper()
	target := r.running.URL() + "/storage/v1/b/" + testBucket + "/o/" + url.PathEscape(name)
	request, err := http.NewRequestWithContext(testwait.Context(t), http.MethodDelete, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+r.double.Token())
	if status, body := do(t, request); status != http.StatusNoContent {
		t.Fatalf("the double's delete of %s answered %d, not 204: %s", name, status, body)
	}
}

// firstPack is the first pack, in byte order, of the objects l lists, failing the test where it
// lists none.
func firstPack(t *testing.T, l listing) string {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(l.objects)) {
		if strings.HasSuffix(name, ".pack") {
			return name
		}
	}
	t.Fatalf("the listing %v holds no pack", slices.Sorted(maps.Keys(l.objects)))
	return ""
}

// requireHeldAsInClone fails the test unless the double holds name's object with the bytes of the
// clone's file of that name, by the size, CRC32C and MD5 the double computed of what it stored.
func (r *packsRig) requireHeldAsInClone(t *testing.T, name, what string) {
	t.Helper()
	local, err := os.ReadFile(filepath.Join(r.c.dir, "objects", "pack", strings.TrimPrefix(name, testPrefix)))
	if err != nil {
		t.Fatal(err)
	}
	status, res := r.reader.resource(t, name)
	if status != http.StatusOK {
		t.Fatalf("%s the bucket reads %s %d, not 200; log %q", what, name, status, r.log.all())
	}
	requireHolds(t, res, local)
}

// present reports whether the double holds name, as its JSON API reads it apart from the cache.
func (r *packsRig) present(t *testing.T, name string) bool {
	t.Helper()
	switch status, _ := r.reader.resource(t, name); status {
	case http.StatusOK:
		return true
	case http.StatusNotFound:
		return false
	default:
		t.Fatalf("a metadata read of %s answered %d", name, status)
		return false
	}
}

// requirePresent fails the test unless the double holds each of names as want says, while what
// holds.
func (r *packsRig) requirePresent(t *testing.T, want bool, what string, names ...string) {
	t.Helper()
	for _, name := range names {
		if got := r.present(t, name); got != want {
			t.Errorf("%s the bucket holds %s: %t, want %t; log %q", what, name, got, want, r.log.all())
		}
	}
}

// strayName is an object name of the test repository's prefix the cache writes, for a pack whose
// file no clone holds: one an interrupted upload, or another process, left in the bucket.
func strayName(t *testing.T) string {
	t.Helper()
	id := make([]byte, 20)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return testPrefix + "pack-" + hex.EncodeToString(id) + ".pack"
}

// listing is what a /packs answer lists, its addresses aside, or what a clone holds as one must
// list it: each ref by its path, with its bytes, and each pack and index by its object name, with
// its size and SHA-256 ("<size> <sha256>").
type listing struct {
	refs    map[string]string
	objects map[string]string
}

// listingOf is manifest's listing, failing the test where it names a ref path or an object twice.
func listingOf(t *testing.T, manifest workspace.PackManifest) listing {
	t.Helper()
	l := listing{refs: map[string]string{}, objects: map[string]string{}}
	for _, ref := range manifest.Refs {
		if _, ok := l.refs[ref.Path]; ok {
			t.Errorf("the manifest lists the ref %s twice", ref.Path)
		}
		l.refs[ref.Path] = string(ref.Bytes)
	}
	for _, object := range manifest.Objects {
		if _, ok := l.objects[object.Name]; ok {
			t.Errorf("the manifest lists %s twice", object.Name)
		}
		l.objects[object.Name] = formatInt(object.Size) + " " + object.SHA256
	}
	return l
}

// cloneListing is what the bare clone at dir holds as a /packs answer must list it, read from its
// files apart from the cache's code: each file the clone's tar streams (workspace.CacheEntry), a
// ref with its exact bytes, and a pack or an index under its object name with its size and the
// SHA-256 of its bytes.
func cloneListing(t *testing.T, dir string) listing {
	t.Helper()
	l := listing{refs: map[string]string{}, objects: map[string]string{}}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if !workspace.CacheEntry(name, entry.IsDir()) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if file, ok := strings.CutPrefix(name, "objects/pack/"); ok {
			l.objects[testPrefix+file] = formatInt(int64(len(body))) + " " + sha256Of(body)
		} else {
			l.refs[name] = string(body)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the clone %s: %v", dir, err)
	}
	return l
}

// requireListing fails the test unless got is want, naming what held then.
func requireListing(t *testing.T, got, want listing, what string) {
	t.Helper()
	if !maps.Equal(got.refs, want.refs) {
		t.Errorf("%s /packs lists the refs %q, not %q", what, got.refs, want.refs)
	}
	if !maps.Equal(got.objects, want.objects) {
		t.Errorf("%s /packs lists the objects %v, not %v", what, slices.Sorted(maps.Keys(got.objects)), slices.Sorted(maps.Keys(want.objects)))
	}
}

// added is the names want lists that was does not.
func added(was, want listing) []string {
	var names []string
	for _, name := range slices.Sorted(maps.Keys(want.objects)) {
		if _, ok := was.objects[name]; !ok {
			names = append(names, name)
		}
	}
	return names
}

// pair is the names of a pack and its index in byte order, the index first, as added answers
// them, stem being the object name less its extension.
func pair(stem string) []string { return []string{stem + ".idx", stem + ".pack"} }

// formatInt is n in decimal, as Cloud Storage writes a size and a generation.
func formatInt(n int64) string { return strconv.FormatInt(n, 10) }

// TestCachePublishesCompletePackSets holds the cache's publishing of its packs (R2): after the first
// clone and after every fetch it uploads each pack and index the listed set lacks and only then
// lists the clone's refs with every pack and index; a repack's new pack is listed only once it and
// its index are both whole in the bucket, /packs answering the old refs and the whole old set
// while either upload is held; an upload that fails and a bucket that cannot be opened each leave
// the old set listed, /tar and /healthz answering, and the next refresh tries again; and a pack
// without its index, or an index without its pack, is left out of the set, which lists the rest of
// the clone. Every answer of every row holds the invariant requireWhole checks.
// An object of the listed set that the bucket lost behind the cache's back, as a lifecycle rule or
// another writer deletes one, is uploaded again with the clone's bytes at the next sync, logged by
// name, whether that sync keeps the set or switches it.
func TestCachePublishesCompletePackSets(t *testing.T) {
	for _, row := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{"the first clone uploads every pack and index and lists them with the clone's refs", testFirstCloneListed},
		{"an incremental fetch uploads its pack and index and lists them with the new refs", testFetchListed},
		{"a repack held before its pack and then before its index is whole keeps the old set listed until both are", testRepackHeld},
		{"an upload that fails leaves the old set listed, /tar and /healthz answering, and the next refresh lists the new one", testUploadFailureKeepsTheSet},
		{"a pack without its index and an index without its pack are left out, and the rest of the clone is listed", testLoneFilesLeftOut},
		{"a bucket that cannot be opened leaves /tar and /healthz answering, and the next refresh opens it", testBucketOpenTriedAgain},
		{"a restarted cache whose bucket holds every pack already uploads none, and its log counts none uploaded", testRestartCountsNoUpload},
		{"a listed pack and its index the bucket lost are uploaded again at the next sync, which keeps the set, each logged by name", testLostObjectsPutBack},
		{"a listed pack the bucket lost is uploaded again at a fetch's switch, whose set lists it beside the fetch's pack and index", testLostPackPutBackAtASwitch},
	} {
		t.Run(row.name, row.run)
	}
}

// testFirstCloneListed: before the first clone /packs is 503; after it, the bucket holds every pack
// and index of the clone, each uploaded once, and /packs lists exactly the clone's refs and those
// objects.
func testFirstCloneListed(t *testing.T) {
	r := newPacksRig(t)
	if status, _ := r.packs(t); status != http.StatusServiceUnavailable {
		t.Fatalf("/packs before the first clone answered %d, not 503", status)
	}
	r.refresh(t)
	want := cloneListing(t, r.c.dir)
	requireListing(t, listingOf(t, r.requirePacks(t)), want, "after the first clone")
	if len(want.objects) < 2 || len(want.refs) == 0 {
		t.Fatalf("the clone holds %v and refs %q: no pack and index to hold the row by", want.objects, want.refs)
	}
	if got := r.double.Count(fakegcs.RouteUpload).Requests; got != len(want.objects) {
		t.Errorf("the cache sent %d uploads for the clone's %d objects", got, len(want.objects))
	}
}

// testFetchListed: a fetch that brings a commit adds one pack and its index, which the next answer
// lists, with the refs at the new commit and the first clone's objects still listed.
func testFetchListed(t *testing.T) {
	r := newPacksRig(t)
	r.refresh(t)
	before := listingOf(t, r.requirePacks(t))
	moved := advance(t, r.remote, "moved")
	r.refresh(t)
	got := listingOf(t, r.requirePacks(t))
	requireListing(t, got, cloneListing(t, r.c.dir), "after the incremental fetch")
	fresh := added(before, got)
	if len(fresh) != 2 || !slices.Equal(fresh, pair(strings.TrimSuffix(fresh[0], ".idx"))) {
		t.Errorf("the fetch added %q to the listed set, not one pack and its index", fresh)
	}
	for name := range before.objects {
		if _, ok := got.objects[name]; !ok {
			t.Errorf("the fetch's set dropped %s, which the clone still holds", name)
		}
	}
	if !slices.ContainsFunc(slices.Collect(maps.Values(got.refs)), func(body string) bool { return strings.Contains(body, moved) }) {
		t.Errorf("the refs listed after the fetch name no ref at %s: %q", moved, got.refs)
	}
	if slices.ContainsFunc(slices.Collect(maps.Values(before.refs)), func(body string) bool { return strings.Contains(body, moved) }) {
		t.Errorf("the refs listed before the fetch already named %s: the row holds nothing", moved)
	}
}

// testRepackHeld: with the clone in two packs, a due repack writes one new pack; its upload is held
// in the double before the object becomes visible, then, once released and whole, its index's is
// held. Throughout, /packs answers the old refs and the whole old set, and /tar and /healthz
// answer; once the index is released, the next answer lists the repack's pack and index alone.
func testRepackHeld(t *testing.T) {
	r := newPacksRig(t)
	r.refresh(t)
	advance(t, r.remote, "second pack")
	r.refresh(t)
	old := listingOf(t, r.requirePacks(t))
	if len(old.objects) < 4 {
		t.Fatalf("the clone holds %v before the repack, not two packs and their indexes", old.objects)
	}
	type holds struct{ pack, index *fakegcs.Hold }
	armed := make(chan holds, 1)
	r.run.instead("repack", func(ctx context.Context, command workspace.Command) (workspace.Result, error) {
		fresh := func(ext string) func(bucket, name string) bool {
			return func(bucket, name string) bool {
				_, listed := old.objects[name]
				return bucket == testBucket && !listed && strings.HasSuffix(name, ext)
			}
		}
		armed <- holds{r.double.HoldNext(fresh(".pack")), r.double.HoldNext(fresh(".idx"))}
		return r.run.runner.Run(ctx, command)
	})
	r.repackDue()
	done := make(chan error, 1)
	go func() { done <- r.c.refresh(testwait.Context(t)) }()
	held := testwait.Receive(t, "the repack to start", armed)
	defer held.pack.Release()
	defer held.index.Release()

	testwait.Receive(t, "the repack's pack to stop at the double's hold", held.pack.Reached())
	r.requireStill(t, old, "while the repack's pack was held,")
	r.requireServes(t, "while the repack's pack was held")
	next := cloneListing(t, r.c.dir)
	fresh := added(old, next)
	if len(fresh) != 2 || len(next.objects) != 2 {
		t.Fatalf("the repack left the clone holding %v, not one new pack and its index", slices.Sorted(maps.Keys(next.objects)))
	}
	newPack := fresh[1]
	r.requirePresent(t, false, "while its upload was held,", newPack)

	held.pack.Release()
	testwait.Receive(t, "the repack's index to stop at the double's hold", held.index.Reached())
	r.requirePresent(t, true, "with the repack's index held,", newPack)
	r.requireStill(t, old, "with the repack's pack whole and its index held,")
	r.requireServes(t, "while the repack's index was held")

	held.index.Release()
	if err := testwait.Receive(t, "the repack's refresh to end", done); err != nil {
		t.Fatalf("the repack's refresh: %v", err)
	}
	requireListing(t, listingOf(t, r.requirePacks(t)), next, "once the repack's index was whole,")
}

// testUploadFailureKeepsTheSet: the upload of a fetch's pack refused (403, which no client retries)
// is logged and leaves the first clone's set listed with its refs, while /healthz answers the
// fetch's head and /tar its clone; the refresh itself succeeds, and the next one lists the clone.
func testUploadFailureKeepsTheSet(t *testing.T) {
	r := newPacksRig(t)
	r.refresh(t)
	old := listingOf(t, r.requirePacks(t))
	moved := advance(t, r.remote, "moved")
	r.double.FailUpload(1, http.StatusForbidden)
	r.refresh(t)
	if !logged(r.log.all(), "publish the packs to bucket "+testBucket+" failed; /packs keeps the set it had") {
		t.Errorf("the failed upload was not logged: %q", r.log.all())
	}
	r.requireStill(t, old, "after the failed upload,")
	if health := r.requireServes(t, "after the failed upload"); health.Head != moved {
		t.Errorf("/healthz after the failed upload names head %s, not the fetch's %s", health.Head, moved)
	}
	r.refresh(t)
	requireListing(t, listingOf(t, r.requirePacks(t)), cloneListing(t, r.c.dir), "at the refresh after the failed upload,")
}

// testLoneFilesLeftOut: a pack without its index and an index without its pack, which git reads
// nothing of, planted in the clone beside a fetch's new pack, are left out of the set: the sync
// installs the clone's current refs with every complete pair, uploads the fetch's pack and index
// alone, and neither lists nor uploads either lone file, each logged once over two syncs. An object
// of the lone index's name already in the bucket is not deleted while the clone holds the file,
// at the second sync an hour on.
func testLoneFilesLeftOut(t *testing.T) {
	r := newPacksRig(t)
	r.refresh(t)
	before := listingOf(t, r.requirePacks(t))
	moved := advance(t, r.remote, "moved")
	pack, index := "pack-"+strings.Repeat("ab", 20)+".pack", "pack-"+strings.Repeat("cd", 20)+".idx"
	packs := filepath.Join(r.c.dir, "objects", "pack")
	for _, file := range []string{pack, index} {
		if err := os.WriteFile(filepath.Join(packs, file), randomContent(t, 4<<10), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	generation := r.stray(t, testPrefix+index)
	requests := len(r.double.Requests())
	var fetched []string
	for _, at := range []time.Duration{0, time.Hour} {
		r.clock.Set(packsStart.Add(at))
		r.refresh(t)
		want := cloneListing(t, r.c.dir)
		for _, file := range []string{pack, index} {
			if _, ok := want.objects[testPrefix+file]; !ok {
				t.Fatalf("the clone's listing holds no %s, so the row holds nothing of it", file)
			}
			delete(want.objects, testPrefix+file)
		}
		got := listingOf(t, r.requirePacks(t))
		requireListing(t, got, want, "with a lone pack and a lone index in the clone,")
		if fetched = added(before, got); len(fetched) != 2 || !slices.Equal(fetched, pair(strings.TrimSuffix(fetched[0], ".idx"))) {
			t.Errorf("the set listed beside the lone files added %q, not the fetch's pack and its index", fetched)
		}
		if !slices.ContainsFunc(slices.Collect(maps.Values(got.refs)), func(body string) bool { return strings.Contains(body, moved) }) {
			t.Errorf("the refs listed beside the lone files name no ref at the fetch's %s: %q", moved, got.refs)
		}
	}
	var uploaded []string
	for _, request := range r.double.Requests()[requests:] {
		if request.Route == fakegcs.RouteUpload {
			name, _ := requestName(t, request)
			uploaded = append(uploaded, name)
		}
	}
	slices.Sort(uploaded)
	if !slices.Equal(uploaded, fetched) {
		t.Errorf("the two syncs uploaded %q, not the fetch's pack and index alone, %q", uploaded, fetched)
	}
	r.requirePresent(t, false, "after two syncs of a clone holding it,", testPrefix+pack)
	if status, res := r.reader.resource(t, testPrefix+index); status != http.StatusOK || res.Generation != formatInt(generation) {
		t.Errorf("the bucket's object of the lone index's name reads %d at generation %s an hour on, not 200 at %d", status, res.Generation, generation)
	}
	for _, file := range []string{pack, index} {
		path, lines := filepath.Join(packs, file), 0
		for _, line := range r.log.all() {
			if strings.Contains(line, path) {
				lines++
				if !strings.Contains(line, "git reads no pack without its index, so it is left out of the listed set and not uploaded") {
					t.Errorf("the line naming %s says not why it is left out: %q", file, line)
				}
			}
		}
		if lines != 1 {
			t.Errorf("%s was named in %d log lines over two syncs, not 1: %q", file, lines, r.log.all())
		}
	}
}

// testBucketOpenTriedAgain: a bucket the cache cannot open (a STORAGE_EMULATOR_HOST it refuses) is
// logged at the sync, sends nothing, and is no failure of the refresh: /packs answers 503 and /tar
// and /healthz answer. With the variable right, the next refresh opens the bucket and lists the
// clone.
func testBucketOpenTriedAgain(t *testing.T) {
	r := newPacksRig(t)
	t.Setenv(emulatorHostEnv, "storage.googleapis.com:443")
	r.refresh(t)
	if !logged(r.log.all(), "publish the packs to bucket "+testBucket+" failed; /packs keeps the set it had, and the next refresh tries again: open bucket "+testBucket+": STORAGE_EMULATOR_HOST is") {
		t.Errorf("the bucket that could not be opened was not logged: %q", r.log.all())
	}
	if status, _ := r.packs(t); status != http.StatusServiceUnavailable {
		t.Errorf("/packs with no bucket opened answered %d, not 503", status)
	}
	r.requireServes(t, "with no bucket opened")
	if requests := r.double.Requests(); len(requests) != 0 {
		t.Errorf("a bucket that could not be opened sent %d requests", len(requests))
	}
	t.Setenv(emulatorHostEnv, r.running.Addr())
	r.refresh(t)
	requireListing(t, listingOf(t, r.requirePacks(t)), cloneListing(t, r.c.dir), "at the refresh after the bucket could not be opened,")
}

// uploadedCount is the count of objects a sync's log line says it uploaded: the number just before
// "uploaded" ("2 of them uploaded now", "0 uploaded").
var uploadedCount = regexp.MustCompile(`\b(\d+) (?:of them )?uploaded\b`)

// testRestartCountsNoUpload: a new process's cache on the same volume, whose bucket already holds
// every pack and index of the clone, lists them all at its first sync and sends Cloud Storage no
// upload (put reuses each object); the line that says what the sync listed counts as uploaded what
// Cloud Storage was sent, none, so a reader of the cache's log after a restart, a daemon roll or an
// autoscaler move is not told the cache wrote its whole clone to the bucket again.
func testRestartCountsNoUpload(t *testing.T) {
	r := newPacksRig(t)
	r.refresh(t)
	want := cloneListing(t, r.c.dir)
	uploads := r.double.Count(fakegcs.RouteUpload).Requests
	r.restart(t, clocktest.New(packsStart.Add(time.Minute)))
	before := len(r.log.all())
	r.refresh(t)
	requireListing(t, listingOf(t, r.requirePacks(t)), want, "after the restart,")
	if sent := r.double.Count(fakegcs.RouteUpload).Requests - uploads; sent != 0 {
		t.Fatalf("the restarted cache sent %d uploads for objects the bucket held already", sent)
	}
	lines, counted := r.log.all()[before:], false
	for _, line := range lines {
		m := uploadedCount.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		counted = true
		if m[1] != "0" {
			t.Errorf("the restarted cache sent Cloud Storage no upload, and logged %q", line)
		}
	}
	if !counted {
		t.Errorf("the restarted cache's first sync logged no count of what it uploaded: %q", lines)
	}
}

// testLostObjectsPutBack: once the first clone's set is listed, its pack and that pack's index,
// deleted from the bucket behind the cache's back, are uploaded again at the next sync, which
// brings nothing and keeps the same set: one upload each, the bucket holding each with the clone's
// own bytes again, a log line naming each, and /packs answering the set it answered before, whole.
func testLostObjectsPutBack(t *testing.T) {
	r := newPacksRig(t)
	r.refresh(t)
	listed := r.c.listed.Load()
	before := listingOf(t, r.requirePacks(t))
	lost := pair(strings.TrimSuffix(firstPack(t, before), ".pack"))
	for _, name := range lost {
		r.lose(t, name)
	}
	r.requirePresent(t, false, "once deleted behind the cache's back,", lost...)
	uploads := r.double.Count(fakegcs.RouteUpload).Requests
	r.refresh(t)
	if r.c.listed.Load() != listed {
		t.Errorf("the sync that put the lost pack and index back, which brought nothing, installed a set of its own")
	}
	requireListing(t, listingOf(t, r.requirePacks(t)), before, "after the sync that put the lost pack and index back,")
	if sent := r.double.Count(fakegcs.RouteUpload).Requests - uploads; sent != len(lost) {
		t.Errorf("the sync sent %d uploads, not one for each of the %d lost objects; log %q", sent, len(lost), r.log.all())
	}
	for _, name := range lost {
		r.requireHeldAsInClone(t, name, "after the sync that put it back,")
		if !logged(r.log.all(), "bucket "+testBucket+" no longer held "+name+", which the listed set names: uploaded it again") {
			t.Errorf("the upload of %s again was not logged by name: %q", name, r.log.all())
		}
	}
}

// testLostPackPutBackAtASwitch: a pack of the first clone's set, deleted from the bucket behind the
// cache's back, is uploaded again by the next sync, a fetch's, which switches to a set that lists
// it still beside the fetch's own pack and index: the switch lists the clone whole, the bucket
// holds the lost pack with the clone's bytes again, a log line names it, and the switch's line
// counts it among the three objects it uploaded.
func testLostPackPutBackAtASwitch(t *testing.T) {
	r := newPacksRig(t)
	r.refresh(t)
	before := listingOf(t, r.requirePacks(t))
	lost := firstPack(t, before)
	r.lose(t, lost)
	advance(t, r.remote, "moved")
	uploads := r.double.Count(fakegcs.RouteUpload).Requests
	r.refresh(t)
	got := listingOf(t, r.requirePacks(t))
	requireListing(t, got, cloneListing(t, r.c.dir), "after the fetch's switch,")
	if _, ok := got.objects[lost]; !ok {
		t.Fatalf("the fetch's set dropped %s, which the clone still holds", lost)
	}
	if fresh := added(before, got); len(fresh) != 2 {
		t.Fatalf("the fetch added %q to the listed set, not one pack and its index", fresh)
	}
	if sent := r.double.Count(fakegcs.RouteUpload).Requests - uploads; sent != 3 {
		t.Errorf("the switch sent %d uploads, not 3: the fetch's pack and index and the lost pack; log %q", sent, r.log.all())
	}
	r.requireHeldAsInClone(t, lost, "after the fetch's switch,")
	if !logged(r.log.all(), "bucket "+testBucket+" no longer held "+lost+", which the listed set names: uploaded it again") {
		t.Errorf("the upload of %s again was not logged by name: %q", lost, r.log.all())
	}
	if line := "listed " + strconv.Itoa(len(got.objects)) + " packs and indexes in bucket " + testBucket + ", 3 of them uploaded now, 0 reused as the bucket held them"; !logged(r.log.all(), line) {
		t.Errorf("the switch did not log %q: %q", line, r.log.all())
	}
}

// TestCacheRetiresOnlyUnlistedPacks holds the cache's retirement of objects no set names (R2): an
// object of the repository's prefix that neither the clone nor the listed set holds is deleted no
// sooner than 30 minutes after the switch that first left it out, and from then; one the clone
// alone, or the listed set alone, holds is kept; one that comes back before its deadline is kept
// and its clock starts afresh when it is left out again; two switches' objects each retire on their
// own clock; a delete that fails is tried again at the next sync; a restarted cache counts from its
// own first switch, never from an object's creation; and nothing outside the prefix, or under it
// but not of the cache's names, is deleted.
// A sync whose list of the bucket fails, at a switch or keeping the set, drops the record of each
// object the set it leaves listed or the clone holds, and keeps every other record as it was.
func TestCacheRetiresOnlyUnlistedPacks(t *testing.T) {
	if retireAfter != 30*time.Minute {
		t.Fatalf("retireAfter is %s, not 30 minutes, twice an address's 15", retireAfter)
	}
	for _, row := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{"the packs a repack's switch left out are kept at 29m59s after it and deleted at 30m, across refreshes that change nothing and keep the set", testRetiredFromThirtyMinutes},
		{"an object only the clone holds is kept", func(t *testing.T) { testHeldByOneSide(t, true) }},
		{"an object only the listed set holds is kept", func(t *testing.T) { testHeldByOneSide(t, false) }},
		{"a pack that comes back before its deadline is kept, and its clock starts afresh when it is left out again", testReappearingPackStartsAfresh},
		{"two switches' objects each retire on their own switch's clock", testTwoSwitchesTwoClocks},
		{"a delete that fails is tried again at the next sync", testFailedDeleteTriedAgain},
		{"a cache restarted within the window counts from its own first switch", testRestartCountsFromItsOwnSwitch},
		{"another repository's objects and a name the cache does not write are never deleted", testOtherPrefixesUntouched},
		{"a switch whose list of the bucket fails drops the record of each object its set or the clone holds, and keeps the rest", func(t *testing.T) { testFailedListDropsHeldRecords(t, true) }},
		{"a sync whose list of the bucket fails and that keeps the set drops the record of each object the set or the clone holds, and keeps the rest", func(t *testing.T) { testFailedListDropsHeldRecords(t, false) }},
	} {
		t.Run(row.name, row.run)
	}
}

// testRetiredFromThirtyMinutes: with the clone in two packs, a due repack's switch, at a time of
// its own, leaves both packs and their indexes out; they are in the bucket at the switch and at
// 29m59s after it, and gone at 30m, the repack's pack and index kept. The refreshes at 29m59s and
// 30m bring nothing, so each keeps the repack's set listed, the same pointer, and its retirement
// still counts from the repack's switch.
func testRetiredFromThirtyMinutes(t *testing.T) {
	r := newPacksRig(t)
	r.refresh(t)
	advance(t, r.remote, "second pack")
	r.refresh(t)
	old := listingOf(t, r.requirePacks(t))
	switched := packsStart.Add(5 * time.Minute)
	r.clock.Set(switched)
	r.repackDue()
	r.refresh(t)
	listed := r.c.listed.Load()
	next := cloneListing(t, r.c.dir)
	requireListing(t, listingOf(t, r.requirePacks(t)), next, "after the repack,")
	left := added(next, old)
	if len(left) != 4 {
		t.Fatalf("the repack's switch left %q out, not two packs and their indexes", left)
	}
	r.requirePresent(t, true, "at the repack's switch,", left...)
	r.clock.Set(switched.Add(29*time.Minute + 59*time.Second))
	r.refresh(t)
	if r.c.listed.Load() != listed {
		t.Errorf("the refresh 29m59s after the repack, which brought nothing, installed a set of its own")
	}
	r.requirePresent(t, true, "29m59s after the switch that left them out,", left...)
	r.clock.Set(switched.Add(30 * time.Minute))
	r.refresh(t)
	if r.c.listed.Load() != listed {
		t.Errorf("the refresh 30m after the repack, which brought nothing, installed a set of its own")
	}
	r.requirePresent(t, false, "30m after the switch that left them out,", left...)
	r.requirePresent(t, true, "30m after the repack's switch,", slices.Collect(maps.Keys(next.objects))...)
	for _, name := range left {
		if !logged(r.log.all(), "deleted "+name+" from bucket "+testBucket+": neither the clone nor the listed set has held it since "+switched.Format(time.RFC3339)) {
			t.Errorf("the delete of %s was not logged with its switch's time: %q", name, r.log.all())
		}
	}
}

// testHeldByOneSide: retire, run by itself at the switch and at 30m and 60m after it, keeps an
// object the clone's inventory alone holds (byClone), or the listed set alone (not byClone), and
// deletes in the same runs an object neither holds, which shows it ran.
func testHeldByOneSide(t *testing.T, byClone bool) {
	r := newPacksRig(t)
	r.refresh(t)
	set := r.c.listed.Load()
	held, neither := strayName(t), strayName(t)
	r.stray(t, held)
	r.stray(t, neither)
	inventory := map[string]bool{}
	for _, object := range set.objects {
		inventory[object.name] = true
	}
	if byClone {
		inventory[held] = true
	} else {
		listed := *set
		listed.objects = append(slices.Clone(set.objects), listedObject{name: held})
		set = &listed
	}
	ctx := testwait.Context(t)
	r.c.maint.Lock()
	for _, at := range []time.Duration{0, retireAfter, 2 * retireAfter} {
		r.c.retire(ctx, set, inventory, r.c.listBucket(ctx), packsStart.Add(at))
	}
	r.c.maint.Unlock()
	r.requirePresent(t, true, "an hour after it was held by one side alone,", held)
	r.requirePresent(t, false, "an hour after it was held by neither,", neither)
	r.requirePresent(t, true, "an hour on,", slices.Collect(maps.Keys(inventory))...)
}

// testReappearingPackStartsAfresh: a repack's switch leaves the clone's two packs out; 20 minutes
// later the first pack's files are back in the clone, and that refresh lists it again, which ends
// its clock; at 30m the second pack, never back, is deleted and the first kept; at 40m the first
// pack's files go, and that switch starts its clock afresh, so it is kept then (a clock counted
// from the repack would delete it), kept at 69m59s, and deleted at 70m.
func testReappearingPackStartsAfresh(t *testing.T) {
	r := newPacksRig(t)
	r.refresh(t)
	first := listingOf(t, r.requirePacks(t))
	advance(t, r.remote, "second pack")
	r.refresh(t)
	both := listingOf(t, r.requirePacks(t))
	comeBack := slices.Sorted(maps.Keys(first.objects))
	neverBack := added(first, both)
	packs := filepath.Join(r.c.dir, "objects", "pack")
	saved := t.TempDir()
	for _, name := range comeBack {
		copyFile(t, filepath.Join(packs, strings.TrimPrefix(name, testPrefix)), filepath.Join(saved, strings.TrimPrefix(name, testPrefix)))
	}
	switched := packsStart.Add(5 * time.Minute)
	r.clock.Set(switched)
	r.repackDue()
	r.refresh(t)

	r.clock.Set(switched.Add(20 * time.Minute))
	for _, name := range comeBack {
		copyFile(t, filepath.Join(saved, strings.TrimPrefix(name, testPrefix)), filepath.Join(packs, strings.TrimPrefix(name, testPrefix)))
	}
	r.refresh(t)
	listed := listingOf(t, r.requirePacks(t))
	requireListing(t, listed, cloneListing(t, r.c.dir), "with the first pack back in the clone,")
	if len(added(listed, first)) != 0 {
		t.Fatalf("the set listed with the first pack back is %v, which lacks it", slices.Sorted(maps.Keys(listed.objects)))
	}

	r.clock.Set(switched.Add(30 * time.Minute))
	r.refresh(t)
	r.requirePresent(t, false, "30m after the repack, never back in the clone,", neverBack...)
	r.requirePresent(t, true, "30m after the repack, back in the clone,", comeBack...)

	r.clock.Set(switched.Add(40 * time.Minute))
	for _, name := range comeBack {
		if err := os.Remove(filepath.Join(packs, strings.TrimPrefix(name, testPrefix))); err != nil {
			t.Fatal(err)
		}
	}
	r.refresh(t)
	r.requirePresent(t, true, "at the switch that left it out again, 40m after the repack,", comeBack...)
	r.clock.Set(switched.Add(69*time.Minute + 59*time.Second))
	r.refresh(t)
	r.requirePresent(t, true, "29m59s after the switch that left it out again,", comeBack...)
	r.clock.Set(switched.Add(70 * time.Minute))
	r.refresh(t)
	r.requirePresent(t, false, "30m after the switch that left it out again,", comeBack...)
}

// copyFile copies the file from to to, read-only, as git leaves a pack.
func copyFile(t *testing.T, from, to string) {
	t.Helper()
	body, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, body, 0o444); err != nil {
		t.Fatal(err)
	}
}

// testTwoSwitchesTwoClocks: an object first found left out at a switch 1m after the start and
// another at a switch 10m after that each retire 30m after their own switch: at the first's 30m
// the first is gone and the second kept, the second kept at its own 29m59s and gone at its 30m.
func testTwoSwitchesTwoClocks(t *testing.T) {
	r := newPacksRig(t)
	r.refresh(t)
	early, late := strayName(t), strayName(t)
	first := packsStart.Add(time.Minute)
	r.stray(t, early)
	r.clock.Set(first)
	r.refresh(t)
	second := first.Add(10 * time.Minute)
	r.stray(t, late)
	r.clock.Set(second)
	r.refresh(t)
	r.clock.Set(first.Add(30 * time.Minute))
	r.refresh(t)
	r.requirePresent(t, false, "30m after the first switch,", early)
	r.requirePresent(t, true, "30m after the first switch, 20m after the second,", late)
	r.clock.Set(second.Add(29*time.Minute + 59*time.Second))
	r.refresh(t)
	r.requirePresent(t, true, "29m59s after the second switch,", late)
	r.clock.Set(second.Add(30 * time.Minute))
	r.refresh(t)
	r.requirePresent(t, false, "30m after the second switch,", late)
}

// testFailedDeleteTriedAgain: the delete of an object due at 30m refused (403, which no client
// retries) is logged and leaves it; the next sync deletes it.
func testFailedDeleteTriedAgain(t *testing.T) {
	r := newPacksRig(t)
	name := strayName(t)
	r.stray(t, name)
	r.refresh(t)
	r.clock.Set(packsStart.Add(30 * time.Minute))
	r.double.FailDelete(1, http.StatusForbidden)
	r.refresh(t)
	r.requirePresent(t, true, "after its delete failed,", name)
	if !logged(r.log.all(), "retire "+name+" from bucket "+testBucket+" failed; tried again at the next sync: ") {
		t.Errorf("the failed delete was not logged: %q", r.log.all())
	}
	r.clock.Set(packsStart.Add(31 * time.Minute))
	r.refresh(t)
	r.requirePresent(t, false, "at the sync after its delete failed,", name)
}

// testRestartCountsFromItsOwnSwitch: an object created two hours before, first found left out at a
// switch, outlives a restart 20m later: the new process's first switch starts its clock again, so
// it is kept 30m after the first process's switch, and 29m59s after the new process's first, and
// deleted 30m after that one. Neither its creation nor the first process's record moves it earlier.
func testRestartCountsFromItsOwnSwitch(t *testing.T) {
	r := newPacksRig(t)
	r.doubleClock.Set(packsStart.Add(-2 * time.Hour))
	name := strayName(t)
	r.stray(t, name)
	r.doubleClock.Set(time.Now())
	switched := packsStart.Add(time.Minute)
	r.clock.Set(switched)
	r.refresh(t)
	restarted := switched.Add(20 * time.Minute)
	r.restart(t, clocktest.New(restarted))
	r.refresh(t)
	r.clock.Set(switched.Add(30 * time.Minute))
	r.refresh(t)
	r.requirePresent(t, true, "30m after the first process's switch, 10m after the restart's,", name)
	r.clock.Set(restarted.Add(29*time.Minute + 59*time.Second))
	r.refresh(t)
	r.requirePresent(t, true, "29m59s after the restart's first switch,", name)
	r.clock.Set(restarted.Add(30 * time.Minute))
	r.refresh(t)
	r.requirePresent(t, false, "30m after the restart's first switch,", name)
}

// testOtherPrefixesUntouched: with objects of another repository of the owner, of one whose name
// begins with this one's, of this one's names in their configured case, and a file under the
// prefix that is no pack, beside a stray pack of the cache's own, syncs over two hours delete the
// stray alone, send no other delete, leave each other object at its generation, and log the file
// that is no pack once.
func testOtherPrefixesUntouched(t *testing.T) {
	r := newPacksRig(t)
	file := strings.TrimPrefix(strayName(t), testPrefix)
	others := map[string]int64{}
	for _, name := range []string{
		"acme/gadgets/objects/pack/" + file,
		"acme/widgets-old/objects/pack/" + file,
		"Acme/Widgets/objects/pack/" + file,
		testPrefix + "notes.txt",
	} {
		others[name] = r.stray(t, name)
	}
	own := testPrefix + file
	r.stray(t, own)
	for _, at := range []time.Duration{0, time.Hour, 2 * time.Hour} {
		r.clock.Set(packsStart.Add(at))
		r.refresh(t)
	}
	r.requirePresent(t, false, "two hours after the first switch,", own)
	for name, generation := range others {
		if status, res := r.reader.resource(t, name); status != http.StatusOK || res.Generation != formatInt(generation) {
			t.Errorf("%s reads %d at generation %s, not 200 at %d", name, status, res.Generation, generation)
		}
	}
	var deleted []string
	for _, request := range r.double.Requests() {
		if request.Route == fakegcs.RouteDelete {
			name, _ := requestName(t, request)
			deleted = append(deleted, name)
		}
	}
	if !slices.Equal(deleted, []string{own}) {
		t.Errorf("the syncs sent deletes of %q, not of %s alone", deleted, own)
	}
	notes := 0
	for _, line := range r.log.all() {
		if strings.Contains(line, testPrefix+"notes.txt") {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("the file under the prefix that is no pack was logged %d times, not once: %q", notes, r.log.all())
	}
}

// testFailedListDropsHeldRecords: a sync whose list of the bucket fails, at a fetch's switch
// (switches) or with nothing new, so that it keeps the set, drops the absence record of an object
// the set it leaves listed holds and of a lone pack the clone alone holds, as retire would, so a
// later set that leaves either out counts its 30 minutes from that switch; and it keeps the record
// of an object neither holds as it was. The list fails at a front of the double that answers the
// cache's list requests 403, which the Storage client does not retry, and hands every other
// request to the double.
func testFailedListDropsHeldRecords(t *testing.T, switches bool) {
	r := newPacksRig(t)
	var refuseLists atomic.Bool
	front := serveHandler(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if refuseLists.Load() && req.Method == http.MethodGet && req.URL.EscapedPath() == "/storage/v1/b/"+testBucket+"/o" {
			http.Error(w, "the test refuses this list", http.StatusForbidden)
			return
		}
		r.double.ServeHTTP(w, req)
	}))
	t.Setenv(emulatorHostEnv, strings.TrimPrefix(front, "http://"))
	r.refresh(t)
	listed := r.c.listed.Load()
	if switches {
		advance(t, r.remote, "moved")
	}
	lone := "pack-" + strings.Repeat("ab", 20) + ".pack"
	if err := os.WriteFile(filepath.Join(r.c.dir, "objects", "pack", lone), randomContent(t, 4<<10), 0o444); err != nil {
		t.Fatal(err)
	}
	inSet, inClone, neither := listed.objects[0].name, testPrefix+lone, strayName(t)
	recorded := absence{since: packsStart.Add(-time.Hour), generation: 1}
	for _, name := range []string{inSet, inClone, neither} {
		r.c.absent[name] = recorded
	}
	refuseLists.Store(true)
	r.refresh(t)
	if !logged(r.log.all(), "list the bucket's packs to put back those the listed set names and retire those no set names; tried again at the next sync: ") {
		t.Fatalf("the sync whose list the front refused logged no failed list: %q", r.log.all())
	}
	if switched := r.c.listed.Load() != listed; switched != switches {
		t.Fatalf("the sync whose list failed switched the set: %t, not %t", switched, switches)
	}
	if !r.c.listed.Load().holds(inSet) {
		t.Fatalf("the set the sync left listed does not hold %s", inSet)
	}
	for _, name := range []string{inSet, inClone} {
		if record, ok := r.c.absent[name]; ok {
			t.Errorf("the sync whose list failed kept the record %+v of %s, which the set or the clone holds", record, name)
		}
	}
	if record, ok := r.c.absent[neither]; !ok || record != recorded {
		t.Errorf("the sync whose list failed left the record of %s, which neither the set nor the clone holds, as %+v, not %+v", neither, record, recorded)
	}
}

// TestPacksAuthenticatesAndSignsAWholeSet holds GET /packs (R3): it takes the token /tar takes, as
// the token file holds it now; a cache with no bucket does not serve it; a cache with no listed set
// answers 503; a ready answer is the whole current snapshot, exact refs and every pack with its
// index, with names, sizes and SHA-256 of the local files, and each address a V4 GET for 900
// seconds that the double serves the object's exact bytes for, signed afresh at every answer; its
// bytes are what json.Marshal writes of the manifest it states; an address changed in its scope, a
// signed header or its path, or used at its expiry, is refused by the double; a signer's failure
// answers 503 with no manifest; a switch while an answer is signed answers 503 with none of the
// old set's addresses; and a refresh that changes nothing while an answer is signed keeps the set,
// so the answer is 200.
func TestPacksAuthenticatesAndSignsAWholeSet(t *testing.T) {
	for _, row := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{"a missing, wrong or old bearer is refused, and the rotated token is taken", testPacksBearer},
		{"a cache with no bucket answers the mux's plain 404 with no repository header", testPacksUnserved},
		{"a cache with no listed set answers 503", testPacksUnready},
		{"a ready answer is the whole current snapshot, each address a V4 GET for 900 s that the double serves", testPacksWholeSnapshot},
		{"an answer's bytes are json.Marshal's of the manifest it states, its refs none, empty or escaped alike", testPacksAnswerEncoding},
		{"an address changed in its scope, a signed header or its path, or used at its expiry, is refused", testPacksAddressRefused},
		{"a signer's failure answers 503 with no manifest", testPacksSignerFailure},
		{"a switch while the answer is signed answers 503 with none of the old set's addresses", testPacksSwitchWhileSigning},
		{"a refresh that changes nothing while the answer is signed keeps the set, and the answer is 200", testPacksUnchangedWhileSigning},
	} {
		t.Run(row.name, row.run)
	}
}

// requireNoManifest fails the test unless body, an answer that is no manifest, holds no part of
// one: no object, no ref and no address.
func requireNoManifest(t *testing.T, body []byte, what string) {
	t.Helper()
	for _, part := range []string{`"objects"`, `"refs"`, "X-Goog-", testPrefix} {
		if bytes.Contains(body, []byte(part)) {
			t.Errorf("%s the answer holds %q: %s", what, part, body)
		}
	}
}

// testPacksBearer: no bearer and another bearer are 401 with no repository header and no manifest;
// once the token file holds a new token, the old one is 401 and the new one 200, naming the
// repository.
func testPacksBearer(t *testing.T) {
	r := newPacksRig(t)
	r.refresh(t)
	for _, bearer := range []string{"", "not-the-cache-token"} {
		status, header, body := r.get(t, "/packs", bearer)
		if status != http.StatusUnauthorized || header.Get(workspace.CacheRepositoryHeader) != "" {
			t.Errorf("/packs with bearer %q answered %d naming %q, not 401 naming nothing", bearer, status, header.Get(workspace.CacheRepositoryHeader))
		}
		requireNoManifest(t, body, "/packs with bearer "+bearer+":")
	}
	rotated := "rotated-cache-token-of-acme-widgets"
	if err := os.WriteFile(r.opts.CacheTokenFile, []byte(rotated+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status, _, body := r.get(t, "/packs", testCacheToken); status != http.StatusUnauthorized {
		t.Errorf("/packs with the token before the rotation answered %d, not 401", status)
	} else {
		requireNoManifest(t, body, "/packs with the old token:")
	}
	status, header, body := r.get(t, "/packs", rotated)
	if status != http.StatusOK || header.Get(workspace.CacheRepositoryHeader) != "acme/widgets" {
		t.Fatalf("/packs with the rotated token answered %d naming %q: %s", status, header.Get(workspace.CacheRepositoryHeader), body)
	}
	var manifest workspace.PackManifest
	if err := json.Unmarshal(body, &manifest); err != nil || len(manifest.Objects) == 0 {
		t.Errorf("/packs with the rotated token answered no manifest (%v): %s", err, body)
	}
}

// testPacksUnserved: a cache told no bucket, run by Serve with STORAGE_EMULATOR_HOST naming a
// double, answers /packs with the mux's plain 404 and no repository header, with its token or none,
// as a cache of an earlier release does, serves /tar, and sends the double nothing.
func testPacksUnserved(t *testing.T) {
	remote := newRemote(t)
	opts := testOptions(t, newTestRunner(t, remote))
	double, running := startDouble(t, fakegcs.Options{Email: EmulatorEmail})
	t.Setenv(emulatorHostEnv, running.Addr())
	c := start(t, opts)
	c.ready(t)
	for _, bearer := range []string{c.bearer(t), ""} {
		response := c.get(t, "/packs", bearer)
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusNotFound || string(body) != "404 page not found\n" || response.Header.Get(workspace.CacheRepositoryHeader) != "" {
			t.Errorf("/packs of a cache with no bucket answered %d %q naming %q, not the mux's plain 404 naming nothing", response.StatusCode, body, response.Header.Get(workspace.CacheRepositoryHeader))
		}
	}
	if status, _ := c.getTar(t); status != http.StatusOK {
		t.Errorf("/tar of a cache with no bucket answered %d", status)
	}
	if requests := double.Requests(); len(requests) != 0 {
		t.Errorf("a cache with no bucket sent the double %d requests", len(requests))
	}
}

// testPacksUnready: /packs answers 503, naming the repository, before the first clone, and while
// the first sync's first upload is held in the double, when /tar and /healthz already answer; once
// released, 200.
func testPacksUnready(t *testing.T) {
	r := newPacksRig(t)
	status, header, body := r.get(t, "/packs", testCacheToken)
	if status != http.StatusServiceUnavailable || header.Get(workspace.CacheRepositoryHeader) != "acme/widgets" {
		t.Errorf("/packs before the first clone answered %d naming %q, not 503 naming acme/widgets", status, header.Get(workspace.CacheRepositoryHeader))
	}
	requireNoManifest(t, body, "/packs before the first clone:")
	hold := r.double.HoldNext(func(bucket, _ string) bool { return bucket == testBucket })
	defer hold.Release()
	done := make(chan error, 1)
	go func() { done <- r.c.refresh(testwait.Context(t)) }()
	testwait.Receive(t, "the first upload to stop at the double's hold", hold.Reached())
	status, _, body = r.get(t, "/packs", testCacheToken)
	if status != http.StatusServiceUnavailable {
		t.Errorf("/packs during the first sync answered %d, not 503", status)
	}
	requireNoManifest(t, body, "/packs during the first sync:")
	r.requireServes(t, "during the first sync")
	hold.Release()
	if err := testwait.Receive(t, "the first refresh to end", done); err != nil {
		t.Fatalf("the first refresh: %v", err)
	}
	r.requirePacks(t)
}

// testPacksWholeSnapshot: with the clone in two packs, an answer lists its exact refs and every
// pack and index, under their names, with the sizes and SHA-256 of the local files; each address
// is path style on the double, GOOG4-RSA-SHA256 over the host alone, signed as the cache's
// identity, stating 900 s, and the double serves it, with no credential, the local file's exact
// bytes. Each answer signs every address afresh, one signBlob each, and the log holds no address
// and no token.
func testPacksWholeSnapshot(t *testing.T) {
	r := newPacksRig(t)
	r.refresh(t)
	advance(t, r.remote, "second pack")
	r.refresh(t)
	for answer := range 2 {
		signs := r.double.Count(fakegcs.RouteSignBlob).Requests
		status, header, body := r.get(t, "/packs", testCacheToken)
		if status != http.StatusOK || header.Get("Content-Type") != "application/json" || header.Get(workspace.CacheRepositoryHeader) != "acme/widgets" {
			t.Fatalf("/packs answered %d, %q, naming %q: %s", status, header.Get("Content-Type"), header.Get(workspace.CacheRepositoryHeader), body)
		}
		var manifest workspace.PackManifest
		if err := json.Unmarshal(body, &manifest); err != nil {
			t.Fatalf("/packs answered no manifest: %v", err)
		}
		if got := r.double.Count(fakegcs.RouteSignBlob).Requests - signs; got != len(manifest.Objects) {
			t.Errorf("answer %d sent %d signBlob requests for its %d objects, not one each", answer, got, len(manifest.Objects))
		}
		r.requireWhole(t, manifest)
		want := cloneListing(t, r.c.dir)
		requireListing(t, listingOf(t, manifest), want, "with the clone in two packs,")
		if len(want.objects) != 4 {
			t.Fatalf("the clone holds %v, not two packs and their indexes", slices.Sorted(maps.Keys(want.objects)))
		}
		for _, object := range manifest.Objects {
			u, err := url.Parse(object.URL)
			if err != nil {
				t.Fatalf("%s's address does not parse: %v", object.Name, err)
			}
			query := u.Query()
			if u.Scheme != "http" || u.Host != r.running.Addr() || u.Path != "/"+testBucket+"/"+object.Name {
				t.Errorf("%s's address is %s://%s%s, not path style on the double", object.Name, u.Scheme, u.Host, u.Path)
			}
			if query.Get("X-Goog-Algorithm") != "GOOG4-RSA-SHA256" || query.Get("X-Goog-SignedHeaders") != "host" || query.Get("X-Goog-Expires") != "900" || !strings.HasPrefix(query.Get("X-Goog-Credential"), EmulatorEmail+"/") {
				t.Errorf("%s's address states algorithm %q, signed headers %q, expires %q, credential %q; not GOOG4-RSA-SHA256 over the host alone for 900 s as %s", object.Name,
					query.Get("X-Goog-Algorithm"), query.Get("X-Goog-SignedHeaders"), query.Get("X-Goog-Expires"), query.Get("X-Goog-Credential"), EmulatorEmail)
			}
			local, err := os.ReadFile(filepath.Join(r.c.dir, "objects", "pack", strings.TrimPrefix(object.Name, testPrefix)))
			if err != nil {
				t.Fatal(err)
			}
			if status, got := r.reader.download(t, object.URL); status != http.StatusOK || !bytes.Equal(got, local) || sha256Of(got) != object.SHA256 {
				t.Errorf("the download of %s answered %d and %d bytes, not 200 and the local file's %d bytes of SHA-256 %s", object.Name, status, len(got), len(local), object.SHA256)
			}
		}
	}
	if got := r.double.Count(fakegcs.RouteDownload).Requests; got == 0 {
		t.Errorf("the double served no download of the answers' addresses")
	}
	for _, line := range r.log.all() {
		if strings.Contains(line, "X-Goog-") || strings.Contains(line, testCacheToken) {
			t.Errorf("the cache logged an address or its token: %q", line)
		}
	}
}

// testPacksAnswerEncoding: an answer's bytes are exactly what json.Marshal writes of the
// workspace.PackManifest they decode to, its refs encoded once when the set was made; and the wire
// form servePacks encodes (packsAnswer) writes the bytes json.Marshal writes of the manifest for
// refs that are none (null), that are empty ([]), and whose path and bytes hold characters
// json.Marshal escapes.
func testPacksAnswerEncoding(t *testing.T) {
	r := newPacksRig(t)
	r.refresh(t)
	status, _, body := r.get(t, "/packs", testCacheToken)
	if status != http.StatusOK {
		t.Fatalf("/packs answered %d, not 200: %s", status, body)
	}
	var manifest workspace.PackManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("/packs answered no manifest: %v", err)
	}
	if len(manifest.Refs) == 0 || len(manifest.Objects) == 0 {
		t.Fatalf("the answer lists %d refs and %d objects: no ref and object to hold the row by", len(manifest.Refs), len(manifest.Objects))
	}
	if want, err := json.Marshal(manifest); err != nil || !bytes.Equal(body, want) {
		t.Errorf("/packs answered\n%s\nnot json.Marshal's encoding of the manifest it states (%v)\n%s", body, err, want)
	}
	for _, refs := range [][]workspace.PackRef{nil, {}, {{Path: "refs/heads/a&b<c>\u2028", Bytes: []byte("<&>\x00\xff")}}} {
		encoded, err := json.Marshal(refs)
		if err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(packsAnswer{Bucket: testBucket, Refs: encoded, Objects: manifest.Objects})
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(workspace.PackManifest{Bucket: testBucket, Refs: refs, Objects: manifest.Objects})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("the wire form of refs %q encodes\n%s\nnot json.Marshal's of the manifest\n%s", refs, got, want)
		}
	}
}

// testPacksAddressRefused: one address of an answer, which the double serves as it is, is refused
// (403) with its credential's scope changed, with another signed header named, and with another
// listed object's path; it is served 899 s past its X-Goog-Date and refused at 900 s.
func testPacksAddressRefused(t *testing.T) {
	r := newPacksRig(t)
	r.refresh(t)
	manifest := r.requirePacks(t)
	if len(manifest.Objects) < 2 {
		t.Fatalf("the answer lists %d objects, not a pack and its index", len(manifest.Objects))
	}
	target, other := manifest.Objects[0], manifest.Objects[1]
	if status, _ := r.reader.download(t, target.URL); status != http.StatusOK {
		t.Fatalf("the address as signed answered %d, not 200", status)
	}
	changed := func(change func(u *url.URL, query url.Values)) string {
		u, err := url.Parse(target.URL)
		if err != nil {
			t.Fatal(err)
		}
		query := u.Query()
		change(u, query)
		u.RawQuery = query.Encode()
		return u.String()
	}
	for _, tc := range []struct {
		name    string
		address string
	}{
		{"another credential scope", changed(func(_ *url.URL, q url.Values) {
			q.Set("X-Goog-Credential", strings.Replace(q.Get("X-Goog-Credential"), "/auto/storage/", "/us-east1/storage/", 1))
		})},
		{"another signed header", changed(func(_ *url.URL, q url.Values) { q.Set("X-Goog-SignedHeaders", "host;range") })},
		{"another object's path", changed(func(u *url.URL, _ url.Values) { u.Path = "/" + testBucket + "/" + other.Name })},
	} {
		if tc.address == target.URL {
			t.Fatalf("the address with %s is the address as signed", tc.name)
		}
		if status, _ := r.reader.download(t, tc.address); status != http.StatusForbidden {
			t.Errorf("the address with %s answered %d, not 403", tc.name, status)
		}
	}
	u, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	signedAt, err := time.Parse("20060102T150405Z", u.Query().Get("X-Goog-Date"))
	if err != nil {
		t.Fatalf("the address's X-Goog-Date does not parse: %v", err)
	}
	r.doubleClock.Set(signedAt.Add(899 * time.Second))
	if status, _ := r.reader.download(t, target.URL); status != http.StatusOK {
		t.Errorf("the address 899 s past its date answered %d, not 200", status)
	}
	r.doubleClock.Set(signedAt.Add(900 * time.Second))
	if status, _ := r.reader.download(t, target.URL); status != http.StatusForbidden {
		t.Errorf("the address 900 s past its date answered %d, not 403", status)
	}
}

// testPacksSignerFailure: with IAM refusing to sign, /packs answers 503 with no part of a manifest,
// and logs the failure; once IAM signs again, 200.
func testPacksSignerFailure(t *testing.T) {
	r := newPacksRig(t)
	r.refresh(t)
	r.double.RefuseSigning(true)
	status, header, body := r.get(t, "/packs", testCacheToken)
	if status != http.StatusServiceUnavailable || header.Get("Content-Type") == "application/json" {
		t.Errorf("/packs with IAM refusing answered %d, %q, not 503 and no manifest", status, header.Get("Content-Type"))
	}
	requireNoManifest(t, body, "/packs with IAM refusing:")
	if !logged(r.log.all(), "sign the packs' addresses for ") {
		t.Errorf("the refused signature was not logged: %q", r.log.all())
	}
	r.double.RefuseSigning(false)
	r.requirePacks(t)
}

// hookSigner is a signer that runs hook before each signature it hands on to inner.
type hookSigner struct {
	inner signer
	hook  func()
}

func (s hookSigner) Sign(ctx context.Context, payload []byte) ([]byte, error) {
	s.hook()
	return s.inner.Sign(ctx, payload)
}

// testPacksSwitchWhileSigning: the first signature of an answer runs a refresh that brings a commit
// and switches the listed set to it; the answer is 503 with none of the old set's addresses, the
// answer counted no /tar stream meanwhile, and the next answer lists the new set.
func testPacksSwitchWhileSigning(t *testing.T) {
	r := newPacksRig(t)
	r.refresh(t)
	old := r.c.listed.Load()
	advance(t, r.remote, "moved")
	type landed struct {
		err      error
		inFlight int64
	}
	switched := make(chan landed, 1)
	ctx := testwait.Context(t)
	var once sync.Once
	r.c.store.signer = hookSigner{inner: r.c.store.signer, hook: func() {
		once.Do(func() {
			inFlight := r.c.inFlight.Load()
			switched <- landed{r.c.refresh(ctx), inFlight}
		})
	}}
	status, _, body := r.get(t, "/packs", testCacheToken)
	got := testwait.Receive(t, "the refresh the signer ran", switched)
	if got.err != nil {
		t.Fatalf("the refresh the signer ran: %v", got.err)
	}
	if r.c.listed.Load() == old {
		t.Fatalf("the refresh the signer ran listed no new set: the row holds nothing")
	}
	if status != http.StatusServiceUnavailable {
		t.Errorf("/packs with a switch while it signed answered %d, not 503", status)
	}
	requireNoManifest(t, body, "/packs with a switch while it signed:")
	if got.inFlight != 0 {
		t.Errorf("the cache counted %d /tar streams while /packs signed, not 0", got.inFlight)
	}
	requireListing(t, listingOf(t, r.requirePacks(t)), cloneListing(t, r.c.dir), "at the answer after the switch,")
}

// testPacksUnchangedWhileSigning: the first signature of an answer runs a refresh that brings
// nothing; that refresh keeps the listed set, the same pointer, so the answer is 200, whole, and
// lists the clone.
func testPacksUnchangedWhileSigning(t *testing.T) {
	r := newPacksRig(t)
	r.refresh(t)
	listed := r.c.listed.Load()
	refreshed := make(chan error, 1)
	ctx := testwait.Context(t)
	var once sync.Once
	r.c.store.signer = hookSigner{inner: r.c.store.signer, hook: func() {
		once.Do(func() { refreshed <- r.c.refresh(ctx) })
	}}
	status, manifest := r.packs(t)
	if err := testwait.Receive(t, "the refresh the signer ran", refreshed); err != nil {
		t.Fatalf("the refresh the signer ran: %v", err)
	}
	if r.c.listed.Load() != listed {
		t.Errorf("the refresh the signer ran, which brought nothing, installed a set of its own")
	}
	if status != http.StatusOK {
		t.Fatalf("/packs with a refresh that brought nothing while it signed answered %d, not 200; log %q", status, r.log.all())
	}
	r.requireWhole(t, manifest)
	requireListing(t, listingOf(t, manifest), cloneListing(t, r.c.dir), "with a refresh that brought nothing while it signed,")
}

// TestSignedPackFetchMatchesTar holds the two ends of /packs together (R3): an issue pod's
// workspace.StreamCache against the real cache, which keeps its packs in the double and narrowed
// away a branch the remote holds beside main, fills one fresh clone's .git from the bucket, its
// downloads carrying no Authorization, and, once /packs cannot sign (503), another from /tar of the
// same cache, saying why. The two .git directories hold the same ref files, byte for byte, the
// cache's main and no other branch, and the same packs and indexes, the cache's own, each of the
// same size and SHA-256, so of the same checksums git wrote into them. After the step that filled
// each from the cache, the fill ran the same git commands on it, as the runner recorded them: the
// object format the pack and index checks read and the connectivity check; and neither ran `git
// index-pack`. What the provision fetches from GitHub after either is the fill's, which
// workspace's and workspace-init's own tests hold.
func TestSignedPackFetchMatchesTar(t *testing.T) {
	r := newPacksRig(t)
	git(t, r.remote, "--git-dir="+r.remote, "branch", "other/branch", "main")
	r.refresh(t)
	listed := cloneListing(t, r.c.dir)
	requireListing(t, listingOf(t, r.requirePacks(t)), listed, "before the fills,")
	cached := git(t, r.c.dir, "--git-dir="+r.c.dir, "rev-parse", "refs/heads/main")
	type filled struct {
		report workspace.StreamReport
		gitDir string
		steps  []string
	}
	fill := func(what string) filled {
		work := filepath.Join(t.TempDir(), "widgets")
		git(t, filepath.Dir(work), "init", "--quiet", work)
		gitDir := filepath.Join(work, ".git")
		r.run.mu.Lock()
		before := len(r.run.argv)
		r.run.mu.Unlock()
		report, err := workspace.StreamCache(testwait.Context(t), r.run, workspace.CacheStream{
			Repo: ghrepo.MustParse("acme/widgets"), URL: r.url, Token: testCacheToken,
			Log: func(line string) { t.Logf("the fill %s logged: %s", what, line) }, Clock: clock.Real(),
		}, gitDir)
		if err != nil {
			t.Fatalf("the fill %s: %v", what, err)
		}
		r.run.mu.Lock()
		argv := slices.Clone(r.run.argv[before:])
		r.run.mu.Unlock()
		return filled{report, gitDir, fillSteps(argv, work)}
	}
	bucket := fill("through the bucket")
	if bucket.report.Source != workspace.SourceBucket || bucket.report.Bytes != 0 || len(bucket.report.Objects) != len(listed.objects) {
		t.Fatalf("the first fill reported %+v, want the bucket's %d objects and no tar", bucket.report, len(listed.objects))
	}
	if got := r.double.Count(fakegcs.RouteDownload); got.Requests != len(listed.objects) || got.WithAuthorization != 0 {
		t.Errorf("the double served %d downloads, %d of them with an Authorization header, want %d with none", got.Requests, got.WithAuthorization, len(listed.objects))
	}
	r.double.RefuseSigning(true)
	tar := fill("through /tar")
	if tar.report.Source != workspace.SourceTar || !strings.HasPrefix(tar.report.TarReason, "unavailable manifest: ") || tar.report.Bytes <= 0 || len(tar.report.Objects) != 0 {
		t.Fatalf("the second fill reported %+v, want /tar's, the manifest unavailable", tar.report)
	}
	fromBucket, fromTar := cloneListing(t, bucket.gitDir), cloneListing(t, tar.gitDir)
	if !maps.Equal(fromBucket.refs, fromTar.refs) {
		t.Errorf("the .git filled from the bucket holds the ref files %q, the one from /tar %q", fromBucket.refs, fromTar.refs)
	}
	if !maps.Equal(fromBucket.objects, fromTar.objects) || !maps.Equal(fromBucket.objects, listed.objects) {
		t.Errorf("the .git filled from the bucket holds the packs and indexes %v, the one from /tar %v, the cache %v", fromBucket.objects, fromTar.objects, listed.objects)
	}
	for _, f := range []filled{bucket, tar} {
		if got := git(t, f.gitDir, "--git-dir="+f.gitDir, "rev-parse", "refs/heads/main"); got != cached {
			t.Errorf("the .git %s's main is %s, want the cache's %s", f.gitDir, got, cached)
		}
		if refs := git(t, f.gitDir, "--git-dir="+f.gitDir, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
			t.Errorf("the .git %s holds the refs %q, want the cache's narrowed main alone", f.gitDir, refs)
		}
	}
	t.Logf("after the cache, the fill through the bucket ran %q", bucket.steps)
	if !slices.Equal(bucket.steps, tar.steps) {
		t.Errorf("after the cache, the fill through the bucket ran %q, the fill through /tar %q", bucket.steps, tar.steps)
	}
	for _, want := range []string{"rev-parse --show-object-format", "fsck --connectivity-only"} {
		if !slices.Contains(bucket.steps, want) {
			t.Errorf("the fill through the bucket ran %q, nothing of it %q", bucket.steps, want)
		}
	}
	if slices.Contains(bucket.steps, "index-pack") || slices.Contains(tar.steps, "index-pack") {
		t.Errorf("a fill indexed a pack the cache gave its index with: through the bucket %q, through /tar %q", bucket.steps, tar.steps)
	}
}

// fillSteps is each git command of argvs that names a path under root, the clone a fill fills, as
// its subcommand and the first flag after it: git's `-c` pairs and --git-dir set aside, so the
// steps of two fills into two clones compare.
func fillSteps(argvs [][]string, root string) []string {
	var steps []string
	for _, argv := range argvs {
		if len(argv) == 0 || argv[0] != "git" || !slices.ContainsFunc(argv, func(arg string) bool { return strings.Contains(arg, root) }) {
			continue
		}
		args := argv[1:]
		for len(args) > 1 && args[0] == "-c" {
			args = args[2:]
		}
		args = slices.DeleteFunc(slices.Clone(args), func(arg string) bool { return strings.HasPrefix(arg, "--git-dir=") })
		if len(args) == 0 {
			continue
		}
		step := args[0]
		if len(args) > 1 && strings.HasPrefix(args[1], "--") && args[1] != "--bare" {
			step += " " + args[1]
		}
		steps = append(steps, step)
	}
	return steps
}

// /packs serves only the narrowed clone, as /tar does. A cache whose health answers
// unready, as it does while a narrowing repacks or after a fetch that left its clone unnarrowed,
// answers 503 on /packs, whatever set it listed. A force-push of the default branch makes the next
// refresh narrow again: while that narrowing repacks, /packs answers 503 with no manifest; once the
// narrowing has ended and the cache answers ready and serves /tar again, /packs still answers 503,
// with no manifest, while the sync after it uploads the narrowed packs, so no answer carries the
// set listed before the narrowing. Once that sync is whole, /packs lists the narrowed clone exactly,
// every object of its packs one the rewritten main reaches, and none of the objects listed before.
func TestPacksServesOnlyTheNarrowedClone(t *testing.T) {
	r := newPacksRig(t)
	advance(t, r.remote, "old")
	r.refresh(t)
	before := listingOf(t, r.requirePacks(t))

	served := r.c.health.Load()
	r.c.health.Store(&Health{Repo: r.opts.Repo.String()})
	r.requireNoPacks(t, "while the cache answers unready with a set listed,")
	r.c.health.Store(served)
	r.requirePacks(t)

	repacking, releaseRepack := holdRepack(t, r.run)
	defer releaseRepack()
	upload := r.double.HoldNext(func(bucket, name string) bool {
		_, listed := before.objects[name]
		return bucket == testBucket && !listed
	})
	defer upload.Release()
	rewritten := forcePushMain(t, r.remote)
	done := make(chan error, 1)
	go func() { done <- r.c.refresh(testwait.Context(t)) }()
	testwait.Receive(t, "the narrowing's repack of the rewritten default branch", repacking)
	r.requireNoPacks(t, "while the narrowing repacks,")
	releaseRepack()
	testwait.Receive(t, "the first upload of the narrowed packs to stop at the double's hold", upload.Reached())
	if health := r.requireServes(t, "while the narrowed packs upload"); health.Head != rewritten {
		t.Errorf("/healthz while the narrowed packs upload answers the head %s, want the rewritten %s", health.Head, rewritten)
	}
	r.requireNoPacks(t, "while the narrowed packs upload,")
	upload.Release()
	if err := testwait.Receive(t, "the refresh of the rewritten default branch", done); err != nil {
		t.Fatalf("the refresh of the rewritten default branch: %v; log %q", err, r.log.all())
	}

	after := listingOf(t, r.requirePacks(t))
	requireListing(t, after, cloneListing(t, r.c.dir), "once the narrowed packs were whole,")
	var indexes []string
	for name := range after.objects {
		if strings.HasSuffix(name, ".idx") {
			indexes = append(indexes, filepath.Join(r.c.dir, "objects", "pack", strings.TrimPrefix(name, testPrefix)))
		}
		if _, listed := before.objects[name]; listed {
			t.Errorf("/packs after the narrowing lists %s, which the set before it listed", name)
		}
	}
	if len(indexes) == 0 {
		t.Fatalf("/packs after the narrowing lists %v, no index", after.objects)
	}
	if got := git(t, r.c.dir, "--git-dir="+r.c.dir, "rev-parse", "refs/heads/main"); got != rewritten {
		t.Fatalf("the cache's main is %s after the narrowing, want the rewritten %s", got, rewritten)
	}
	requireIndexedOnMain(t, r.c.dir, indexes, "the listed index")
}

// requireNoPacks fails the test unless /packs answers 503 with no manifest, naming what held then.
func (r *packsRig) requireNoPacks(t *testing.T, what string) {
	t.Helper()
	status, _, body := r.get(t, "/packs", testCacheToken)
	if status != http.StatusServiceUnavailable {
		t.Errorf("%s /packs answered %d, not 503", what, status)
	}
	requireNoManifest(t, body, what)
}
