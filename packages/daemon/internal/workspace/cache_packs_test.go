package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/repocache/fakegcs"
)

// testPacksBucket is the bucket the stand-in cache keeps its packs in: the placeholder bucket the
// double's fixtures name.
const testPacksBucket = "example-legion-packs"

// testPacksPrefix is every object name of acme/widgets in the bucket begins with.
const testPacksPrefix = "acme/widgets/objects/pack/"

// packsCache stands in for a repository cache of acme/widgets whose packs are in a bucket: a
// fakeCache answers /healthz and /tar, a tar of the bare clone it was made from, and GET /packs
// takes the same bearer and answers manifest as JSON, naming acme/widgets, unless packs answers in
// its place. Each pack and index of the clone is in a Cloud Storage double (fakegcs), served on a
// loopback address of its own, whose clock runs skew ahead of the test's, and the manifest names
// each with a V4 address that double signed, as the cache's /packs does. As /tar is asked it records
// every file the git directories gitDirs names (a glob) hold then under objects/pack or refs, and
// their packed-refs: git init leaves none of them, so any is one the bucket's attempt left.
type packsCache struct {
	tar     *fakeCache
	clone   string
	double  *fakegcs.Server
	host    string
	skew    atomic.Int64
	gitDirs string

	mu           sync.Mutex
	manifest     PackManifest
	packs        http.HandlerFunc
	packsAsked   int
	packsBearers []string
	leftAtTar    []string
}

// newPacksCache is a stand-in cache of the bare clone at clone, its /healthz answering not ready to
// its first notReady asks, every pack and index of the clone uploaded to its double and listed in
// its manifest.
func newPacksCache(t *testing.T, clone string, notReady int) *packsCache {
	t.Helper()
	c := &packsCache{tar: &fakeCache{body: cacheTar(t, clone), notReady: notReady}, clone: clone}
	double, err := fakegcs.New(fakegcs.Options{Root: t.TempDir(), Now: func() time.Time { return time.Now().Add(time.Duration(c.skew.Load())) }})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(double)
	t.Cleanup(server.Close)
	c.double, c.host = double, server.Listener.Addr().String()
	c.manifest = c.publish(t)
	return c
}

// publish uploads each pack and index of the clone to the double, with its SHA-256 as the cache
// writes it, and answers the manifest the cache's /packs would: the bucket, every other file of the
// clone a fill takes (CacheEntry) with its exact bytes, and each pack before its index, each with
// its size, SHA-256 and an address the double signed now.
func (c *packsCache) publish(t *testing.T) PackManifest {
	t.Helper()
	manifest := PackManifest{Bucket: testPacksBucket}
	err := filepath.WalkDir(c.clone, func(path string, entry os.DirEntry, err error) error {
		if err != nil || path == c.clone {
			return err
		}
		relative, err := filepath.Rel(c.clone, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if !CacheEntry(name, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
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
		file, pack := strings.CutPrefix(name, "objects/pack/")
		if !pack {
			manifest.Refs = append(manifest.Refs, PackRef{Path: name, Bytes: body})
			return nil
		}
		manifest.Objects = append(manifest.Objects, c.put(t, testPacksPrefix+file, body))
		return nil
	})
	if err != nil {
		t.Fatalf("publish the clone %s: %v", c.clone, err)
	}
	slices.SortFunc(manifest.Objects, func(a, b PackObject) int { return strings.Compare(packFirst(a.Name), packFirst(b.Name)) })
	if len(manifest.Objects) != 2 {
		t.Fatalf("the clone %s holds the objects %v, not one pack and its index", c.clone, manifest.Objects)
	}
	return manifest
}

// packFirst orders a pack before its index: the name with its extension made to sort so.
func packFirst(name string) string {
	if stem, pack := strings.CutSuffix(name, ".pack"); pack {
		return stem + ".0"
	}
	return strings.TrimSuffix(name, ".idx") + ".1"
}

// put makes the double hold body under name, with its SHA-256 as the cache's sha256 metadata, and
// answers the manifest's line for it.
func (c *packsCache) put(t *testing.T, name string, body []byte) PackObject {
	t.Helper()
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if _, err := c.double.Put(testPacksBucket, name, bytes.NewReader(body), map[string]string{"sha256": digest}); err != nil {
		t.Fatal(err)
	}
	return PackObject{Name: name, Size: int64(len(body)), SHA256: digest, URL: c.sign(t, name)}
}

// sign is a V4 signed address of name in the test bucket on the double, signed now by the
// double's own signBlob, as the cache signs one: GOOG4-RSA-SHA256, its credential scoped to the day
// of X-Goog-Date, auto, storage, over the host header alone and an unsigned payload, for 900 seconds,
// path style. The double checks it by Google's description of the scheme, which its own test holds
// to the pinned Storage module's known answers.
func (c *packsCache) sign(t *testing.T, name string) string {
	t.Helper()
	date := time.Now().UTC().Format("20060102T150405Z")
	scope := date[:8] + "/auto/storage/goog4_request"
	query := "X-Goog-Algorithm=GOOG4-RSA-SHA256&X-Goog-Credential=" + v4Escape(fakegcs.DefaultEmail+"/"+scope) +
		"&X-Goog-Date=" + date + "&X-Goog-Expires=900&X-Goog-SignedHeaders=host"
	objectPath := "/" + testPacksBucket + "/" + name
	host, _, ok := strings.Cut(c.host, ":")
	if !ok {
		t.Fatalf("the double's address %s names no port", c.host)
	}
	canonical := strings.Join([]string{http.MethodGet, objectPath, query, "host:" + host, "", "host", "UNSIGNED-PAYLOAD"}, "\n")
	digest := sha256.Sum256([]byte(canonical))
	toSign := strings.Join([]string{"GOOG4-RSA-SHA256", date, scope, hex.EncodeToString(digest[:])}, "\n")
	return "http://" + c.host + objectPath + "?" + query + "&X-Goog-Signature=" + hex.EncodeToString(c.signBlob(t, []byte(toSign)))
}

// signBlob is the double's IAM Credentials signBlob of payload, as its own account.
func (c *packsCache) signBlob(t *testing.T, payload []byte) []byte {
	t.Helper()
	request, err := json.Marshal(map[string]string{"payload": base64.StdEncoding.EncodeToString(payload)})
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post("http://"+c.host+"/v1/projects/-/serviceAccounts/"+fakegcs.DefaultEmail+":signBlob", "application/json", bytes.NewReader(request))
	if err != nil {
		t.Fatalf("signBlob: %v", err)
	}
	defer response.Body.Close()
	var answer struct {
		SignedBlob string `json:"signedBlob"`
	}
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("signBlob answered %s (%v)", response.Status, err)
	}
	signature, err := base64.StdEncoding.DecodeString(answer.SignedBlob)
	if err != nil {
		t.Fatal(err)
	}
	return signature
}

// v4Escape percent-encodes s as a V4 canonical query value is: every byte but the unreserved
// letters, digits, '-', '.', '_' and '~', as %XX in upper-case hex.
func v4Escape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
	}
	return b.String()
}

func (c *packsCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/packs":
		c.mu.Lock()
		c.packsAsked++
		c.packsBearers = append(c.packsBearers, r.Header.Get("Authorization"))
		answer, manifest := c.packs, c.manifest
		c.mu.Unlock()
		if answer != nil {
			answer(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+testCacheToken {
			http.Error(w, "the repository cache's token is required", http.StatusUnauthorized)
			return
		}
		w.Header().Set(CacheRepositoryHeader, "acme/widgets")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manifest)
	case r.Method == http.MethodGet && r.URL.Path == "/tar":
		left := c.staged()
		c.mu.Lock()
		c.leftAtTar = append(c.leftAtTar, left...)
		c.mu.Unlock()
		c.tar.ServeHTTP(w, r)
	default:
		c.tar.ServeHTTP(w, r)
	}
}

// staged is every file the watched git directories hold under objects/pack or refs, and each
// packed-refs, by path.
func (c *packsCache) staged() []string {
	var found []string
	gitDirs, _ := filepath.Glob(c.gitDirs)
	for _, gitDir := range gitDirs {
		for _, part := range []string{"objects/pack", "refs", "packed-refs"} {
			_ = filepath.WalkDir(filepath.Join(gitDir, part), func(path string, entry os.DirEntry, err error) error {
				if err == nil && entry.Type().IsRegular() {
					found = append(found, path)
				}
				return nil
			})
		}
	}
	return found
}

// serve starts the stand-in, watching the git directories gitDirs names (a glob) at each /tar, and
// answers its address.
func (c *packsCache) serve(t *testing.T, gitDirs string) string {
	t.Helper()
	server := httptest.NewServer(c)
	t.Cleanup(server.Close)
	c.gitDirs = gitDirs
	return server.URL
}

// edit changes the manifest /packs answers.
func (c *packsCache) edit(change func(m *PackManifest)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	change(&c.manifest)
}

// answer makes /packs answer with answer, in place of the manifest.
func (c *packsCache) answer(answer http.HandlerFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.packs = answer
}

// object is the manifest's object whose name ends in suffix (".pack" or ".idx").
func (m *PackManifest) object(suffix string) *PackObject {
	for i := range m.Objects {
		if strings.HasSuffix(m.Objects[i].Name, suffix) {
			return &m.Objects[i]
		}
	}
	return nil
}

// elsewhere serves each object of the clone at its path in the test bucket, as the double would,
// whatever the address's query, as serve says: it sends the answer's headers, the object's whole
// length among them, and then what serve does with the object's bytes. It answers its host.
func (c *packsCache) elsewhere(t *testing.T, serve func(w http.ResponseWriter, r *http.Request, body []byte)) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, ok := strings.CutPrefix(r.URL.Path, "/"+testPacksBucket+"/"+testPacksPrefix)
		body, err := os.ReadFile(filepath.Join(c.clone, "objects", "pack", path.Base(file)))
		if !ok || err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		serve(w, r, body)
	}))
	t.Cleanup(server.Close)
	return server.Listener.Addr().String()
}

// moved is address with its host made host, its path and signed query kept.
func moved(t *testing.T, address, host string) string {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = host
	return u.String()
}

// A fresh clone comes from the cache's bucket only when every part of it holds, and from the
// cache's /tar otherwise. The control row fills the clone's .git from the bucket, its downloads
// carrying no Authorization, and asks no /tar. Each other row breaks one part, and the fill then
// asks /tar afresh with the cache's token, on a .git holding nothing the bucket's attempt wrote,
// says why in its report's TarReason, and ends with the /tar's history: a
// cache of an earlier release or with no bucket (a 404 naming no repository); a 503; an answer that
// is no manifest; a manifest listing no ref, no object, or neither, which no whole set is, a pack
// without its index, a pack and its index twice, a ref a clone does not take, an object of another
// repository, or an address at another host, each refused before any download; an object of
// another size; a pack and an index whose bytes the bucket changed; a download the bucket refuses
// for want of the object, one cut off mid-body, one whose signature the bucket refuses, one past
// its expiry, and one that stalls below the low-speed bound; a pack whose bytes the manifest's
// SHA-256 vouches for and whose own trailer git refuses; refs naming a commit the packs do not
// hold; and a history check that runs past what is left of the attempt's bound, whose reason names
// that bound and not a history that is not whole. No reason names an address or its query. An
// answer naming another repository and one that refuses the pod's token are refusals, as /tar's
// are, outside the issue's volume, and nothing asks /tar. A /tar the cache then refuses, or whose
// history does not verify, keeps the failure and its classification it has without the bucket, and
// the report still says why /tar was asked. The .git is a work tree's, as the provision's `git
// init` makes it, and either source leaves it with the cache's main and a whole history.
func TestStreamCacheFallsBackFromPacksToTar(t *testing.T) {
	notFound := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }
	unavailable := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(CacheRepositoryHeader, "acme/widgets")
		http.Error(w, "the repository cache has listed no complete set of packs in its bucket yet", http.StatusServiceUnavailable)
	}
	for _, tc := range []struct {
		name string
		// edit makes the row's fault once the stand-in is built, before the fetch.
		edit func(t *testing.T, run *recordingRunner, c *packsCache)
		// took, when set, is the clock time each step bucketFirst(cacheFetchStep) names takes on a
		// test clock (slowSteps), and the fetch runs on that clock; otherwise it runs on the local
		// runner and the wall clock.
		took map[string]time.Duration
		// source is where the report says the clone's history came from; reason is how its
		// TarReason begins, and says what else it holds.
		source, reason, says string
		// fails is what the fetch's failure holds, "" for none, and outside whether it is outside
		// the issue's volume.
		fails   string
		outside bool
	}{
		{name: "a bucket whose every part holds fills the clone", source: SourceBucket},
		{name: "a cache of an earlier release answers a bare 404", edit: func(_ *testing.T, _ *recordingRunner, c *packsCache) { c.answer(notFound) },
			source: SourceTar, reason: reasonNotServed},
		{name: "a cache with no bucket answers the mux's plain 404", edit: func(_ *testing.T, _ *recordingRunner, c *packsCache) { c.answer(http.NotFound) },
			source: SourceTar, reason: reasonNotServed},
		{name: "a cache with no listed set answers 503", edit: func(_ *testing.T, _ *recordingRunner, c *packsCache) { c.answer(unavailable) },
			source: SourceTar, reason: reasonUnavailable + ": GET ", says: "/packs answered 503 Service Unavailable"},
		{name: "an answer that is no manifest", edit: func(_ *testing.T, _ *recordingRunner, c *packsCache) {
			c.answer(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(CacheRepositoryHeader, "acme/widgets")
				_, _ = w.Write([]byte(`{"bucket":"` + testPacksBucket + `","refs":[`))
			})
		}, source: SourceTar, reason: reasonInvalid + ": GET ", says: "/packs answered no manifest"},
		{name: "a manifest that lists no ref", edit: func(_ *testing.T, _ *recordingRunner, c *packsCache) {
			c.edit(func(m *PackManifest) { m.Refs = []PackRef{} })
		}, source: SourceTar, reason: reasonInvalid, says: "it lists no ref"},
		{name: "a manifest that lists no object", edit: func(_ *testing.T, _ *recordingRunner, c *packsCache) {
			c.edit(func(m *PackManifest) { m.Objects = []PackObject{} })
		}, source: SourceTar, reason: reasonInvalid, says: "it lists no object"},
		{name: "a manifest that lists neither a ref nor an object", edit: func(_ *testing.T, _ *recordingRunner, c *packsCache) {
			c.edit(func(m *PackManifest) { m.Refs, m.Objects = []PackRef{}, []PackObject{} })
		}, source: SourceTar, reason: reasonInvalid, says: "it lists no ref"},
		{name: "a pack listed without its index", edit: func(_ *testing.T, _ *recordingRunner, c *packsCache) {
			c.edit(func(m *PackManifest) {
				m.Objects = slices.DeleteFunc(m.Objects, func(o PackObject) bool { return strings.HasSuffix(o.Name, ".idx") })
			})
		}, source: SourceTar, reason: reasonInvalid, says: ".pack without " + testPacksPrefix},
		{name: "a pack and its index listed twice", edit: func(_ *testing.T, _ *recordingRunner, c *packsCache) {
			c.edit(func(m *PackManifest) { m.Objects = append(m.Objects, m.Objects...) })
		}, source: SourceTar, reason: reasonInvalid, says: "twice"},
		{name: "a ref a clone does not take", edit: func(_ *testing.T, _ *recordingRunner, c *packsCache) {
			c.edit(func(m *PackManifest) {
				m.Refs = append(m.Refs, PackRef{Path: "config", Bytes: []byte("[core]\n\tfsmonitor = true\n")})
			})
		}, source: SourceTar, reason: reasonInvalid, says: `the ref "config", which is not one of a repository's refs`},
		{name: "an object of another repository", edit: func(_ *testing.T, _ *recordingRunner, c *packsCache) {
			c.edit(func(m *PackManifest) {
				m.object(".pack").Name = strings.Replace(m.object(".pack").Name, "acme/widgets/", "acme/gadgets/", 1)
			})
		}, source: SourceTar, reason: reasonInvalid, says: "which is no pack or index of acme/widgets"},
		{name: "an address at another host", edit: func(t *testing.T, _ *recordingRunner, c *packsCache) {
			c.edit(func(m *PackManifest) {
				u, err := url.Parse(m.object(".pack").URL)
				if err != nil {
					t.Fatal(err)
				}
				u.Scheme, u.Host = "https", "storage.example.invalid"
				m.object(".pack").URL = u.String()
			})
		}, source: SourceTar, reason: reasonInvalid, says: "an address that is neither at https://storage.googleapis.com nor at http:// on a loopback address"},
		{name: "an object of another size", edit: func(_ *testing.T, _ *recordingRunner, c *packsCache) {
			c.edit(func(m *PackManifest) { m.object(".pack").Size-- })
		}, source: SourceTar, reason: reasonSize, says: ".pack: the bucket sends "},
		{name: "a pack whose bytes the bucket changed", edit: func(t *testing.T, _ *recordingRunner, c *packsCache) {
			if err := c.double.Corrupt(testPacksBucket, c.manifest.object(".pack").Name); err != nil {
				t.Fatal(err)
			}
		}, source: SourceTar, reason: reasonSHA256, says: ".pack: its bytes have the SHA-256 "},
		{name: "an index whose bytes the bucket changed", edit: func(t *testing.T, _ *recordingRunner, c *packsCache) {
			if err := c.double.Corrupt(testPacksBucket, c.manifest.object(".idx").Name); err != nil {
				t.Fatal(err)
			}
		}, source: SourceTar, reason: reasonSHA256, says: ".idx: its bytes have the SHA-256 "},
		{name: "a download of an object the bucket does not hold", edit: func(t *testing.T, _ *recordingRunner, c *packsCache) {
			absent := testPacksPrefix + "pack-" + strings.Repeat("0", 40)
			c.edit(func(m *PackManifest) {
				for i := range m.Objects {
					m.Objects[i].Name = absent + path.Ext(m.Objects[i].Name)
					m.Objects[i].URL = c.sign(t, m.Objects[i].Name)
				}
			})
		}, source: SourceTar, reason: reasonDownload, says: "the bucket answered 404 Not Found (NoSuchKey)"},
		{name: "a download cut off mid-body", edit: func(t *testing.T, _ *recordingRunner, c *packsCache) {
			host := c.elsewhere(t, func(w http.ResponseWriter, _ *http.Request, body []byte) {
				_, _ = w.Write(body[:len(body)/2])
				w.(http.Flusher).Flush()
				panic(http.ErrAbortHandler)
			})
			c.edit(func(m *PackManifest) { m.object(".pack").URL = moved(t, m.object(".pack").URL, host) })
		}, source: SourceTar, reason: reasonDownload, says: ".pack: unexpected EOF"},
		{name: "a download whose signature the bucket refuses", edit: func(t *testing.T, _ *recordingRunner, c *packsCache) {
			c.edit(func(m *PackManifest) {
				u, err := url.Parse(m.object(".pack").URL)
				if err != nil {
					t.Fatal(err)
				}
				query := u.Query()
				signature := []byte(query.Get("X-Goog-Signature"))
				if last := len(signature) - 1; signature[last] == '0' {
					signature[last] = '1'
				} else {
					signature[last] = '0'
				}
				query.Set("X-Goog-Signature", string(signature))
				u.RawQuery = query.Encode()
				m.object(".pack").URL = u.String()
			})
		}, source: SourceTar, reason: reasonDownload, says: ".pack: the bucket answered 403 Forbidden (SignatureDoesNotMatch)"},
		{name: "a download past its address's expiry", edit: func(_ *testing.T, _ *recordingRunner, c *packsCache) {
			c.skew.Store(int64(BucketFetchTimeout + time.Minute))
		}, source: SourceTar, reason: reasonDownload, says: "the bucket answered 403 Forbidden (ExpiredToken)"},
		{name: "a download that stalls below the low-speed bound", edit: func(t *testing.T, _ *recordingRunner, c *packsCache) {
			// Both objects stall half-way. Every window of the downloads' watch passes once both
			// have reached the server, and not before, so no download is judged before it stalls.
			released := make(chan time.Time)
			var reached atomic.Int32
			host := c.elsewhere(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
				_, _ = w.Write(body[:len(body)/2])
				w.(http.Flusher).Flush()
				if reached.Add(1) == 2 {
					close(released)
				}
				<-r.Context().Done()
			})
			saved := downloadWindow
			downloadWindow = func(time.Duration) <-chan time.Time { return released }
			t.Cleanup(func() { downloadWindow = saved })
			c.edit(func(m *PackManifest) {
				for i := range m.Objects {
					m.Objects[i].URL = moved(t, m.Objects[i].URL, host)
				}
			})
		}, source: SourceTar, reason: reasonDownload, says: "the bucket delivered less than 1048576 bytes in 5m0s, the low-speed bound below which a download is ended"},
		{name: "a pack whose trailer git refuses, whatever its SHA-256 vouches for", edit: func(t *testing.T, _ *recordingRunner, c *packsCache) {
			body, err := os.ReadFile(packOf(t, c.clone))
			if err != nil {
				t.Fatal(err)
			}
			body[len(body)/2] ^= 0xff
			c.edit(func(m *PackManifest) { *m.object(".pack") = c.put(t, m.object(".pack").Name, body) })
		}, source: SourceTar, reason: reasonGit, says: ".pack, which does not verify"},
		{name: "refs that name a commit the packs do not hold", edit: func(_ *testing.T, _ *recordingRunner, c *packsCache) {
			c.edit(func(m *PackManifest) {
				m.Refs = append(m.Refs, PackRef{Path: "refs/heads/broken", Bytes: []byte(strings.Repeat("1234567890", 4) + "\n")})
			})
		}, source: SourceTar, reason: reasonGit, says: "the repository cache's history is not whole: git fsck --connectivity-only failed"},
		{name: "a history check that runs past what is left of the attempt's bound", took: map[string]time.Duration{"bucket fsck": BucketFetchTimeout + time.Second},
			source: SourceTar, reason: reasonGit + ": the bucket attempt's " + BucketFetchTimeout.String() + " bound ran out while checking the history is whole"},
		{name: "an answer naming another repository is refused", edit: func(_ *testing.T, _ *recordingRunner, c *packsCache) {
			c.answer(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(CacheRepositoryHeader, "acme/gadgets")
				_, _ = w.Write([]byte(`{}`))
			})
		}, fails: "is the repository cache of acme/gadgets, not of acme/widgets", outside: true},
		{name: "an answer refusing the pod's token is refused", edit: func(_ *testing.T, _ *recordingRunner, c *packsCache) {
			c.answer(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "the repository cache's token is required", http.StatusUnauthorized)
			})
		}, fails: "/packs answered 401 Unauthorized: it is not the cache whose token this pod holds", outside: true},
		{name: "a /tar naming another repository after the bucket failed", edit: func(_ *testing.T, _ *recordingRunner, c *packsCache) {
			c.answer(unavailable)
			c.tar.tarRepo = "acme/gadgets"
		}, source: SourceTar, reason: reasonUnavailable, fails: "is the repository cache of acme/gadgets, not of acme/widgets", outside: true},
		{name: "a /tar whose history does not verify after the bucket failed", edit: func(t *testing.T, run *recordingRunner, c *packsCache) {
			c.answer(unavailable)
			c.tar.body = cacheTar(t, corruptCacheOf(t, run.remote))
		}, source: SourceTar, reason: reasonUnavailable, fails: "which does not verify"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			pushRemoteBranch(t, run.remote, "main", "history.txt")
			c := newPacksCache(t, cacheOf(t, run.remote), 0)
			work := t.TempDir()
			runSetup(t, work, "git", "init", "--quiet", work)
			gitDir := filepath.Join(work, ".git")
			address := c.serve(t, gitDir)
			if tc.edit != nil {
				tc.edit(t, run, c)
			}
			var runner Runner = run
			var streamClock Clock = clock.Real()
			if tc.took != nil {
				test := newTestClock()
				streamClock = test
				runner = &slowSteps{recordingRunner: run, clock: test, step: bucketFirst(fillStep), took: tc.took, timeouts: map[string][]time.Duration{}}
			}
			report, err := StreamCache(context.Background(), runner, cacheStream(address, streamClock, func(string) {}), gitDir)
			t.Logf("the clone's source: %q, because %q; the fill's failure: %v", report.Source, report.TarReason, err)
			if tc.fails == "" && err != nil {
				t.Fatalf("StreamCache = %+v, %v", report, err)
			}
			if tc.fails != "" && (err == nil || !strings.Contains(err.Error(), tc.fails) || errors.Is(err, ErrOutsideVolume) != tc.outside) {
				t.Fatalf("StreamCache = %+v, %v; want it failed naming %q, outside the volume %t", report, err, tc.fails, tc.outside)
			}
			if report.Source != tc.source || !strings.HasPrefix(report.TarReason, tc.reason) || !strings.Contains(report.TarReason, tc.says) ||
				tc.reason == reasonNotServed && report.TarReason != reasonNotServed {
				t.Errorf("the report says the source %q for %q, want %q for a reason beginning %q and holding %q", report.Source, report.TarReason, tc.source, tc.reason, tc.says)
			}
			secrets := []string{"X-Goog-", testCacheToken}
			for _, object := range c.manifest.Objects {
				if u, err := url.Parse(object.URL); err == nil {
					secrets = append(secrets, u.RawQuery, u.Query().Get("X-Goog-Signature"))
				}
			}
			for _, secret := range secrets {
				if secret != "" && (strings.Contains(report.TarReason, secret) || err != nil && strings.Contains(err.Error(), secret)) {
					t.Errorf("the reason %q or the failure %v names %q", report.TarReason, err, secret)
				}
			}
			downloads := c.double.Count(fakegcs.RouteDownload)
			if downloads.WithAuthorization != 0 {
				t.Errorf("%d of the bucket's %d downloads carried an Authorization header", downloads.WithAuthorization, downloads.Requests)
			}
			if tc.reason == reasonInvalid && downloads.Requests != 0 {
				t.Errorf("the fill refused the manifest as invalid after it asked the bucket for %d downloads, not before any", downloads.Requests)
			}
			if c.packsAsked != 1 || c.packsBearers[0] != "Bearer "+testCacheToken {
				t.Errorf("/packs was asked %d times, presenting %q, want once with the cache's token", c.packsAsked, c.packsBearers)
			}
			if len(c.leftAtTar) != 0 {
				t.Errorf("/tar was asked while the clone's .git held %q, which the bucket's attempt left", c.leftAtTar)
			}
			wantTars := 0
			if tc.source == SourceTar {
				wantTars = 1
			}
			if c.tar.tarAsked != wantTars {
				t.Errorf("StreamCache asked /tar %d times, want %d", c.tar.tarAsked, wantTars)
			}
			assertNoGitHubTransfer(t, run)
			if tc.fails != "" {
				return
			}
			switch tc.source {
			case SourceBucket:
				var want []BucketObject
				var size int64
				for _, object := range c.manifest.Objects {
					want = append(want, BucketObject{Name: object.Name, Size: object.Size, SHA256: object.SHA256})
					size += object.Size
				}
				if !slices.Equal(report.Objects, want) || report.BucketBytes != size || report.Bytes != 0 || report.TarReason != "" {
					t.Errorf("StreamCache reported %+v, want the bucket's objects %+v, %d bytes and nothing from /tar", report, want, size)
				}
				if downloads := c.double.Count(fakegcs.RouteDownload).Requests; downloads != len(want) {
					t.Errorf("the double served %d downloads, want one for each of the %d objects", downloads, len(want))
				}
				for _, call := range run.Calls() {
					if slices.Contains(call.Argv, "index-pack") {
						t.Errorf("StreamCache indexed a pack the bucket gave its index with: %q", call.Argv)
					}
				}
			case SourceTar:
				if report.Bytes != int64(len(c.tar.body)) || len(report.Objects) != 0 || report.BucketBytes != 0 {
					t.Errorf("StreamCache reported %+v, want the tar's %d bytes and nothing from the bucket", report, len(c.tar.body))
				}
			}
			runSetup(t, work, "git", "--git-dir="+gitDir, "fsck", "--connectivity-only")
			if got, want := strings.TrimSpace(runSetup(t, work, "git", "--git-dir="+gitDir, "rev-parse", "refs/heads/main")), strings.TrimSpace(runSetup(t, work, "git", "--git-dir="+run.remote, "rev-parse", "main")); got != want {
				t.Errorf("the clone's main is %s, want the remote's %s", got, want)
			}
			if bare := strings.TrimSpace(runSetup(t, work, "git", "-C", work, "rev-parse", "--is-bare-repository")); bare != "false" {
				t.Errorf("the clone's .git reads as bare %q after the fill, want the work tree's .git it was", bare)
			}
		})
	}
}
