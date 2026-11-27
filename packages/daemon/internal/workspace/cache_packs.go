package workspace

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// PackManifest is the repository cache's answer to GET /packs, one JSON object, where its packs are
// kept in a Cloud Storage bucket (runtime.kubernetes.repository_cache.bucket): the bucket's name,
// the clone's refs, and every pack of the clone with its index, each an object of the bucket that
// the answer names with a signed address to download it by. The cache's /packs handler is its only
// writer, and an issue pod's fill its reader (StreamCache, which holds every member to check before
// it asks for any object).
//
// One answer is one snapshot of the cache's clone, the same one /tar would stream: Refs are exactly
// the files of that snapshot outside objects/pack/ (CacheEntry's rules), and Objects every pack and
// index of it, each pack with its own index and nothing else, every one of them uploaded whole
// before the cache listed it. Each SHA-256 comes from the cache, over the channel its token
// authorizes, never from the bucket, so a reader that checks every download against it takes no
// byte the bucket changed.
type PackManifest struct {
	Bucket  string       `json:"bucket"`
	Refs    []PackRef    `json:"refs"`
	Objects []PackObject `json:"objects"`
}

// PackRef is one ref file of a PackManifest: its path relative to the bare clone, slash-separated
// (HEAD, packed-refs, refs/...), and the file's exact bytes, which JSON carries in base64.
type PackRef struct {
	Path  string `json:"path"`
	Bytes []byte `json:"bytes"`
}

// PackObject is one pack or index of a PackManifest: its object name in the bucket,
// <owner>/<repo>/objects/pack/pack-<id>.pack or .idx (the owner and the repository lower-cased),
// whose last segment is the file's name in the clone's objects/pack; its size in bytes; the SHA-256
// of the cache's own file, in lower-case hex; and a V4 signed address that lets its holder GET the
// object, and nothing else, for 15 minutes from the answer. The address is a credential for its
// object for its life: a reader writes it to no log.
type PackObject struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
}

// CloudStorageHost is the one host a production signed address names: Cloud Storage's own, path
// style (https://storage.googleapis.com/<bucket>/<object>), over HTTPS. The repository cache signs
// each download for it, and a fill takes an address at no other host, but for a test's or the
// burst Job's loopback double (PackManifest.check).
const CloudStorageHost = "storage.googleapis.com"

// PackPrefix is the prefix of every object of repo's in the repository cache's bucket,
// <owner>/<repo>/objects/pack/: its owner and its name lower-cased, since GitHub takes a
// repository's names in any case and the cache must write one prefix for one repository whatever
// case its configuration wrote them in. A bucket serves the repositories of one deployment's
// project: each cache writes under its own repository's prefix alone, and a fill takes no object
// outside its own (PackManifest.check).
func PackPrefix(repo ghrepo.Repository) string {
	return strings.ToLower(repo.Owner()) + "/" + strings.ToLower(repo.Name()) + "/objects/pack/"
}

// PackFileName reports whether file, an object's name past its PackPrefix, is the name of a pack or
// an index that the clone's tar streams in objects/pack and a clone takes there: one path segment
// that CacheEntry takes in objects/pack, so that the object's name past the prefix is the file's own
// name in the clone.
func PackFileName(file string) bool {
	return file != "" && !strings.Contains(file, "/") && CacheEntry("objects/pack/"+file, false)
}

// BucketFetchTimeout bounds a fill's one attempt to fill a fresh clone from its repository cache's
// bucket, all of it: the ask of the cache's GET /packs, every download from a signed address, and
// the git verification of what those brought (StreamReport.fromBucket). It is also the life the
// cache signs each address of its answer for (repocache's signedLife is this constant), 15
// minutes, so the attempt never outlives what it was given, and the cache's retirement of an
// object no set names any more, twice this after the switch that left it out, rests on it. It is a
// bound of its own: an attempt that runs it out falls back to the cache's /tar, whose
// verification and the delta from GitHub after it each keep their whole FetchTimeout, and
// FetchBudget counts it beside them.
const BucketFetchTimeout = 15 * time.Minute

// SourceBucket and SourceTar are where a fresh clone's history came from when a repository cache
// is named (StreamReport.Source): the cache's bucket, through the signed addresses its GET /packs
// gave, or its GET /tar, which streams the clone's refs and packs from the cache itself.
const (
	SourceBucket = "bucket"
	SourceTar    = "/tar"
)

// BucketObject is one pack or index a fill downloaded from the repository cache's bucket and
// verified: its object name as the manifest named it, which the fill checked is one of its own
// repository's packs or indexes; its exact size; and the SHA-256 the fill computed of the bytes it
// wrote, which matched the manifest's.
type BucketObject struct {
	Name   string
	Size   int64
	SHA256 string
}

// The short reasons a fill gives for filling a fresh clone from the cache's /tar rather than its
// bucket (StreamReport.TarReason), each the start of the reason's text: /packs not served at all
// (a cache with no bucket, or of a release that has no /packs, which answers the mux's plain 404
// naming no repository); an answer that is no manifest to use (unavailable: not 200, or not read,
// a 503 of a cache whose set is not listed or not narrowed yet among them); a manifest that does
// not hold (invalid: a member the fill refuses, before any object is asked for);
// a download that failed or stalled; an object of another size or SHA-256 than the manifest's; a
// write of what the bucket gave to the volume that failed; and a git verification of the result
// that failed, or that the attempt's bound ended.
const (
	reasonNotServed   = "/packs is not served"
	reasonUnavailable = "unavailable manifest"
	reasonInvalid     = "invalid manifest"
	reasonDownload    = "failed download"
	reasonSize        = "wrong size"
	reasonSHA256      = "wrong SHA-256"
	reasonWrite       = "failed write"
	reasonGit         = "failed git verification"
)

// bucketFailure is why an attempt to fill the clone from the repository cache's bucket ended and
// the cache's /tar fills it instead: one of the short reasons above, and what failed. Its text never
// holds a signed address or its query (downloadCause), nor a ref's contents.
type bucketFailure struct {
	reason string
	err    error
}

func (f *bucketFailure) Error() string {
	if f.err == nil {
		return f.reason
	}
	return f.reason + ": " + f.err.Error()
}

// packsAsk bounds the ask of a repository cache's GET /packs and the read of its answer, within
// BucketFetchTimeout: the cache signs one address per object on the request, so an answer takes
// moments, and one that takes a minute leaves the downloads too little of the addresses' life.
const packsAsk = time.Minute

// packManifestLimit is the most bytes of a /packs answer a fill reads: the refs of a clone, whose
// packed-refs is a few megabytes for a repository of hundreds of thousands of refs, in base64, and
// one line per pack and index. An answer past it is refused as an invalid manifest rather than
// read whole into the pod's memory.
const packManifestLimit = 64 << 20

// bucketDownloads is how many objects a fill downloads from the bucket at once: a clone holds a
// few large packs, each with a small index, so four keep the pod's link busy without opening a
// connection per object.
const bucketDownloads = 4

// downloadChunk and downloadChunks are the buffers each download reads its body into
// (downloadObject): 1 MiB each, so the file is written and hashed in large steps rather than a TLS
// record, at most 16 KiB, at a time, and at most 8 of them at once, so the body's reader can run 7
// MiB ahead of a write or a hash that pauses. A download makes a buffer only when it first needs
// one, so an index of a few kilobytes takes one, and none holds more than 8 MiB: a fill's
// bucketDownloads at once hold at most 32 MiB, however large their packs.
const (
	downloadChunk  = 1 << 20
	downloadChunks = 8
)

// downloadWindow is the window each download's low-speed watch waits on (countingReader.window):
// time.After, or a test's own.
var downloadWindow = time.After

// bucketClient downloads each pack and index from its signed address, which is the whole of its
// access: it is a client of its own, so no request it sends carries the cache's bearer token or any
// Google credential, and it reads no proxy from the container's environment. It follows no redirect,
// since a signed address names its object at the one host StreamCache checked (PackManifest.check).
var bucketClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConnsPerHost = bucketDownloads
	return &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errBucketRedirect },
	}
}()

// errBucketRedirect is a download's failure where the bucket answered a redirect.
var errBucketRedirect = errors.New("the bucket answered a redirect, which a download from a signed address never follows")

// errDownloadStalled is what a download the low-speed bound ended says of itself, naming the
// bound, which is the repository cache's stream's (CacheStreamLowSpeedBytes).
var errDownloadStalled = fmt.Errorf("the bucket delivered less than %d bytes in %s, the low-speed bound below which a download is ended", CacheStreamLowSpeedBytes, CacheStreamLowSpeedTime)

// fromBucket tries to fill gitDir, the git directory of the clone being made, from the repository
// cache's bucket, all within BucketFetchTimeout of its start, by the stream's clock: it asks the
// cache's GET /packs with its token (askPacks), holds the whole answer to check before it asks for
// any object (PackManifest.check), downloads every pack and index from its signed address into
// gitDir's objects/pack (downloadObjects), writes each ref's exact bytes, and verifies the result
// as verifyCache does a tar that carried every pack's index: each pack and its index to the
// checksums git wrote into them, and every object the refs reach with `git fsck
// --connectivity-only`. So it leaves gitDir as unpackCacheInto does, the cache's refs in place. No
// pack is indexed here: each index comes from the bucket, vouched for by the SHA-256 the cache
// sent, and a pack without one is an error. On success it records the bucket as the clone's source,
// the objects it verified, their bytes and the times.
//
// It answers a *bucketFailure when the cache's /tar must fill the clone instead (fromCache removes
// what it wrote first), and any other error for a failure that ends the fill: an answer that
// refuses the pod's token or names another repository (askCache), which /tar would meet too.
func (r *StreamReport) fromBucket(ctx context.Context, run Runner, stream CacheStream, gitDir string) error {
	started := stream.Clock.Now()
	deadline := started.Add(BucketFetchTimeout)
	bounded, cancel := context.WithTimeout(ctx, BucketFetchTimeout)
	defer cancel()
	manifest, err := askPacks(bounded, stream)
	if err != nil {
		return err
	}
	if err := manifest.check(stream); err != nil {
		return &bucketFailure{reasonInvalid, err}
	}
	objects, size, err := downloadObjects(bounded, manifest, gitDir)
	if err != nil {
		if bounded.Err() != nil && ctx.Err() == nil {
			return &bucketFailure{reasonDownload, fmt.Errorf("the bucket attempt's %s bound ran out", BucketFetchTimeout)}
		}
		return err
	}
	if err := writeRefs(gitDir, manifest.Refs); err != nil {
		return err
	}
	verifying := stream.Clock.Now()
	if err := verifyCache(ctx, run, stream.Clock, gitDir, deadline, "bucket attempt's", BucketFetchTimeout, false); err != nil {
		switch {
		// git's own words for a history that is not whole name the objects the refs reach, which
		// are the refs' contents: the reason names the check alone.
		case errors.Is(err, errHistoryNotWhole):
			err = fmt.Errorf("%w: git fsck --connectivity-only failed", errHistoryNotWhole)
		// A check the attempt's bound ended has shown nothing missing: the reason names the bound.
		case errors.Is(err, errHistoryCheckRanOut):
			err = fmt.Errorf("the bucket attempt's %s bound ran out while checking the history is whole", BucketFetchTimeout)
		}
		return &bucketFailure{reasonGit, err}
	}
	r.Source, r.Objects, r.BucketBytes = SourceBucket, objects, size
	r.BucketDuration, r.VerifyDuration = verifying.Sub(started), stream.Clock.Now().Sub(verifying)
	return nil
}

// askPacks is the repository cache's GET /packs, with its token (askCache), within packsAsk, read
// whole up to packManifestLimit: the manifest of a 200 answer. A 404 naming no repository is a cache
// that does not serve /packs at all; an answer askCache refuses (a 401, or one naming another
// repository) is that refusal, outside the issue's volume, as it is for /tar; any other answer, an
// ask the cache did not answer, and a body that is no manifest are bucket failures.
func askPacks(ctx context.Context, request CacheStream) (PackManifest, error) {
	asking, cancel := context.WithTimeout(ctx, packsAsk)
	defer cancel()
	response, err := askCache(asking, request, "/packs", true)
	if response != nil {
		defer response.Body.Close()
	}
	cache := strings.TrimSuffix(request.URL, "/")
	switch {
	case response == nil:
		return PackManifest{}, &bucketFailure{reasonUnavailable, err}
	case response.StatusCode == http.StatusNotFound && len(response.Header.Values(CacheRepositoryHeader)) == 0:
		return PackManifest{}, &bucketFailure{reason: reasonNotServed}
	case err != nil:
		return PackManifest{}, OutsideVolume(err)
	case response.StatusCode != http.StatusOK:
		return PackManifest{}, &bucketFailure{reasonUnavailable, fmt.Errorf("GET %s/packs answered %s", cache, response.Status)}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, packManifestLimit+1))
	if err != nil {
		return PackManifest{}, &bucketFailure{reasonUnavailable, fmt.Errorf("read GET %s/packs: %w", cache, err)}
	}
	if len(body) > packManifestLimit {
		return PackManifest{}, &bucketFailure{reasonInvalid, fmt.Errorf("GET %s/packs answered more than %d bytes", cache, packManifestLimit)}
	}
	var manifest PackManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return PackManifest{}, &bucketFailure{reasonInvalid, fmt.Errorf("GET %s/packs answered no manifest: %w", cache, err)}
	}
	return manifest, nil
}

// bucketName is the characters and ends of a Cloud Storage bucket's name, which ValidBucketName
// holds with its lengths.
var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*[a-z0-9]$`)

// ValidBucketName reports whether name has a Cloud Storage bucket's shape: 3 to 222 lower-case
// letters, digits, dashes, underscores and dots, starting and ending with a letter or a digit, with
// 1 to 63 of them between two dots, so a name with no dot is 3 to 63 long, and it stands in an
// address's path as it is (https://cloud.google.com/storage/docs/buckets#naming). It is the one
// rule of a bucket's name: the loader holds runtime.kubernetes.repository_cache.bucket to it, and a
// fill holds a /packs manifest's bucket to it (PackManifest.check), so the loader accepts no
// bucket that every fill then refuses, nor the reverse. It holds the shape alone, which is what
// keeps an address (`gs://…`), a capital or a path out of the configuration; the rest of Cloud
// Storage's rules (no `goog` prefix, no name that is an IP address) are its own, which it applies
// where the bucket is made, before any cache reads it.
func ValidBucketName(name string) bool {
	if len(name) < 3 || len(name) > 222 || !bucketName.MatchString(name) {
		return false
	}
	for part := range strings.SplitSeq(name, ".") {
		if part == "" || len(part) > 63 {
			return false
		}
	}
	return true
}

// sha256Hex is a SHA-256 as the manifest states one: 64 lower-case hex digits.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// check is why the manifest may not fill request's clone, or nil when every member of it is one the
// fill takes. It holds the whole answer before the fill asks for any object:
//   - the bucket is a bucket's name;
//   - it lists at least one ref and at least one object: a whole set of the cache's clone lists at
//     least one ref and one pack with its index, so an answer that lists no ref or no object is no
//     whole set, and taken, it would leave the whole of the clone's history to the fetch from
//     GitHub that follows while the report named the bucket as its source;
//   - each ref is a file a clone takes from its cache (CacheEntry), outside objects/pack/, its path
//     slash-separated, clean and inside the repository, named once, and no other ref's directory;
//   - each object is a pack or an index of request's repository, its PackPrefix,
//     <owner>/<repo>/objects/pack/ (the two lower-cased), then one file name a clone takes there
//     (PackFileName), named once, of a size of 0 or more and a SHA-256 of 64 lower-case hex digits,
//     each pack listed with its index and each index with its pack;
//   - each object's address is a GET of that object in that bucket with a query, at Cloud Storage
//     over HTTPS, or, only where the cache itself is on a loopback address (a test, or the burst
//     Job, which run the cache and a double of Cloud Storage beside each other), over HTTP at a
//     loopback address.
//
// The refusal names the member, and never the address: that is a credential for its object.
func (m PackManifest) check(request CacheStream) error {
	if !ValidBucketName(m.Bucket) {
		return fmt.Errorf("it names the bucket %q, which is no Cloud Storage bucket's name", m.Bucket)
	}
	if len(m.Refs) == 0 {
		return errors.New("it lists no ref, so it is no whole set of the cache's clone, which lists at least one")
	}
	if len(m.Objects) == 0 {
		return errors.New("it lists no object, so it is no whole set of the cache's clone, which lists at least one pack with its index")
	}
	refs := map[string]bool{}
	for _, ref := range m.Refs {
		if why := refPathRefusal(ref.Path); why != "" {
			return fmt.Errorf("it lists the ref %q, which %s", ref.Path, why)
		}
		if refs[ref.Path] {
			return fmt.Errorf("it lists the ref %q twice", ref.Path)
		}
		refs[ref.Path] = true
	}
	for ref := range refs {
		for dir := path.Dir(ref); dir != "."; dir = path.Dir(dir) {
			if refs[dir] {
				return fmt.Errorf("it lists the ref %q, which is the directory of the ref %q", dir, ref)
			}
		}
	}
	prefix := PackPrefix(request.Repo)
	loopback := onLoopback(request.URL)
	objects := map[string]bool{}
	for _, object := range m.Objects {
		file, ours := strings.CutPrefix(object.Name, prefix)
		if !ours || !PackFileName(file) {
			return fmt.Errorf("it lists the object %q, which is no pack or index of %s (%spack-<id>.pack or .idx)", object.Name, request.Repo, prefix)
		}
		if objects[object.Name] {
			return fmt.Errorf("it lists the object %s twice", object.Name)
		}
		objects[object.Name] = true
		if object.Size < 0 {
			return fmt.Errorf("it lists %s with a size of %d bytes", object.Name, object.Size)
		}
		if !sha256Hex.MatchString(object.SHA256) {
			return fmt.Errorf("it lists %s with the SHA-256 %q, which is not 64 lower-case hex digits", object.Name, object.SHA256)
		}
		if why := addressRefusal(object.URL, m.Bucket, object.Name, loopback); why != "" {
			return fmt.Errorf("it gives %s an address that %s", object.Name, why)
		}
	}
	for name := range objects {
		stem, pack := strings.CutSuffix(name, ".pack")
		other := stem + ".idx"
		if !pack {
			other = strings.TrimSuffix(name, ".idx") + ".pack"
		}
		if !objects[other] {
			return fmt.Errorf("it lists %s without %s", name, other)
		}
	}
	return nil
}

// refPathRefusal is why name, a ref path of a manifest, is no ref file a clone takes, or "" when it
// is one: a clean path inside the repository that CacheEntry takes as a file (HEAD, packed-refs,
// refs/...), and nothing under objects/pack/, which the bucket's objects alone fill.
func refPathRefusal(name string) string {
	switch {
	case !filepath.IsLocal(filepath.FromSlash(name)) || path.Clean(name) != name:
		return "is not a path inside the clone's git directory"
	case strings.HasPrefix(name, "objects/pack/") || !CacheEntry(name, false):
		return "is not one of a repository's refs"
	}
	return ""
}

// addressRefusal is why address is not a signed download of bucket/name that a fill may ask, or
// "" when it is one (PackManifest.check). It never quotes the address.
func addressRefusal(address, bucket, name string, loopback bool) string {
	u, err := url.Parse(address)
	if err != nil {
		return "does not parse"
	}
	want := "/" + bucket + "/" + name
	if u.Opaque != "" || u.User != nil || u.Fragment != "" || u.RawQuery == "" || u.Path != want || u.EscapedPath() != want {
		return "is not a signed GET of " + bucket + "/" + name
	}
	switch {
	case u.Scheme == "https" && u.Host == CloudStorageHost:
		return ""
	case loopback && u.Scheme == "http" && loopbackHostPort(u.Host):
		return ""
	case loopback:
		return "is neither at https://" + CloudStorageHost + " nor at http:// on a loopback address"
	}
	return "is not at https://" + CloudStorageHost
}

// onLoopback reports whether the repository cache's address names a loopback host.
func onLoopback(cacheURL string) bool {
	u, err := url.Parse(cacheURL)
	return err == nil && loopbackHost(u.Hostname())
}

// loopbackHostPort reports whether hostport is a loopback host with a port.
func loopbackHostPort(hostport string) bool {
	host, port, err := net.SplitHostPort(hostport)
	return err == nil && port != "" && loopbackHost(host)
}

// loopbackHost reports whether host is localhost or a loopback address.
func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// downloadObjects downloads every object of manifest into gitDir's objects/pack, at most
// bucketDownloads at once (downloadObject), and answers each as it verified it, in the manifest's
// order, with their bytes together. The packs start first, largest first, then the indexes, largest
// first, so a large pack is never left waiting behind many small indexes until its address expires.
// The first failure ends the rest, and is the answer.
func downloadObjects(ctx context.Context, manifest PackManifest, gitDir string) ([]BucketObject, int64, error) {
	order := make([]int, len(manifest.Objects))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		first, second := manifest.Objects[a], manifest.Objects[b]
		if packA, packB := strings.HasSuffix(first.Name, ".pack"), strings.HasSuffix(second.Name, ".pack"); packA != packB {
			if packA {
				return -1
			}
			return 1
		}
		return cmp.Compare(second.Size, first.Size)
	})
	verified := make([]BucketObject, len(manifest.Objects))
	group, downloading := errgroup.WithContext(ctx)
	group.SetLimit(bucketDownloads)
	for _, i := range order {
		object := manifest.Objects[i]
		group.Go(func() error {
			got, err := downloadObject(downloading, object, filepath.Join(gitDir, "objects", "pack", path.Base(object.Name)))
			verified[i] = got
			return err
		})
	}
	if err := group.Wait(); err != nil {
		return nil, 0, err
	}
	var size int64
	for _, object := range verified {
		size += object.Size
	}
	return verified, size, nil
}

// downloadObject downloads object from its signed address with bucketClient into the new file
// target, checking its size and SHA-256 as it streams: a body longer than the manifest's size is
// stopped at its first byte past it, a shorter one, or one of another SHA-256, fails, and so does an
// answer other than 200 or one that states another length. It holds the repository cache's
// low-speed bound (countingReader.watchStall, downloadWindow): a bucket that answers nothing, or
// whose body falls below the bound, is ended, naming it. No failure names the address or its query.
//
// Three goroutines pass downloadChunks buffers of downloadChunk bytes round, each in order: one
// does nothing but read the body into them (readChunks), this one writes each to the file, and one
// hashes each the file took and hands it back to the reader. So the socket keeps draining while the
// disk and the hash work, and the hash, the slowest of the three on a CPU without SHA instructions
// (200 to 240 MB/s on an issue pod's), never waits on a write. Where one goroutine read, hashed and
// wrote in turn, nothing read the socket while it hashed and wrote, its receive window filled and
// the sender stalled: a 1 GiB pack arrived at 55 MB/s where a plain read of its address into a file
// ran at 310 to 380 MB/s, and each of 20 fetches at once in one pod ran out of its
// BucketFetchTimeout. The first failure, a write's, a read's or a byte past the size, is the answer:
// no buffer after it reaches the file, the request's context is cancelled so the reader ends, and
// both other goroutines have ended before this returns.
func downloadObject(ctx context.Context, object PackObject, target string) (BucketObject, error) {
	fetching, stop := context.WithCancel(ctx)
	defer stop()
	stream := &countingReader{window: downloadWindow}
	stream.watchStall(fetching, stop)
	failed := func(reason string, err error) (BucketObject, error) {
		return BucketObject{}, &bucketFailure{reason, fmt.Errorf("%s: %w", object.Name, err)}
	}
	ask, err := http.NewRequestWithContext(fetching, http.MethodGet, object.URL, nil)
	if err != nil {
		return failed(reasonDownload, errors.New("its address cannot be asked"))
	}
	response, err := bucketClient.Do(ask)
	if err != nil {
		return failed(reasonDownload, downloadCause(err, object.URL, stream))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return failed(reasonDownload, fmt.Errorf("the bucket answered %s%s", response.Status, refusalCode(response.Body)))
	}
	if response.ContentLength >= 0 && response.ContentLength != object.Size {
		return failed(reasonSize, fmt.Errorf("the bucket sends %d bytes, not the %d the manifest states", response.ContentLength, object.Size))
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return failed(reasonWrite, err)
	}
	stream.reader = response.Body
	free := make(chan []byte, downloadChunks)
	for range downloadChunks {
		free <- nil
	}
	full, written := make(chan []byte, downloadChunks), make(chan []byte, downloadChunks)
	var readFailure error
	go func() {
		defer close(full)
		readFailure = readChunks(fetching, io.LimitReader(stream, object.Size+1), free, full)
	}()
	digest, hashed := sha256.New(), make(chan struct{})
	go func() {
		defer close(hashed)
		for chunk := range written {
			digest.Write(chunk)
			free <- chunk[:cap(chunk)]
		}
	}()
	var (
		n       int64
		reason  string
		failure error
	)
	// Each buffer the file took goes on to the hash, and every other straight back to free, until
	// the reader has ended and closed full: after a failure, nothing more reaches the file.
	for chunk := range full {
		switch {
		case failure != nil:
		case n+int64(len(chunk)) > object.Size:
			reason, failure = reasonSize, fmt.Errorf("the bucket sent more than the %d bytes the manifest states", object.Size)
			stop()
		default:
			_, err := file.Write(chunk)
			if err == nil {
				n += int64(len(chunk))
				written <- chunk
				continue
			}
			reason, failure = reasonWrite, err
			stop()
		}
		free <- chunk[:cap(chunk)]
	}
	close(written)
	<-hashed
	closed := file.Close()
	switch {
	case failure != nil:
		return failed(reason, failure)
	case readFailure != nil:
		return failed(reasonDownload, downloadCause(readFailure, object.URL, stream))
	case closed != nil:
		return failed(reasonWrite, closed)
	case n < object.Size:
		return failed(reasonSize, fmt.Errorf("the bucket sent %d bytes, not the %d the manifest states", n, object.Size))
	}
	sum := hex.EncodeToString(digest.Sum(nil))
	if sum != object.SHA256 {
		return failed(reasonSHA256, fmt.Errorf("its bytes have the SHA-256 %s, not the %s the manifest states", sum, object.SHA256))
	}
	return BucketObject{Name: object.Name, Size: n, SHA256: sum}, nil
}

// readChunks is downloadObject's reader: it takes a buffer from free, fills it from body, and hands
// it to full, each buffer whole but the body's last, until body ends: nil at its end, and otherwise
// the read's failure, or ctx's where ctx ended while it waited for a buffer. A nil from free is a
// buffer not made yet, which it makes. It never waits on full, which has room for every buffer
// there is, so it waits only on a read and on free, and a cancelled ctx ends both.
func readChunks(ctx context.Context, body io.Reader, free <-chan []byte, full chan<- []byte) error {
	for {
		var buffer []byte
		select {
		case buffer = <-free:
		case <-ctx.Done():
			return ctx.Err()
		}
		if buffer == nil {
			buffer = make([]byte, downloadChunk)
		}
		var n int
		var err error
		for n < len(buffer) && err == nil {
			var read int
			read, err = body.Read(buffer[n:])
			n += read
		}
		if n > 0 {
			full <- buffer[:n]
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// downloadCause is what err, a failure of the download from address, says, without the address:
// errDownloadStalled once the low-speed watch ended it, and otherwise err's own cause. A *url.Error
// carries the whole address, query and signature included, so only the error it wraps is kept, and
// any text of the address still in that is cut out.
func downloadCause(err error, address string, stream *countingReader) error {
	if stream.stalled.Load() {
		return errDownloadStalled
	}
	var asked *url.Error
	if errors.As(err, &asked) {
		err = asked.Err
	}
	if errors.Is(err, errBucketRedirect) {
		return errBucketRedirect
	}
	text := err.Error()
	if u, parseErr := url.Parse(address); parseErr == nil && u.RawQuery != "" {
		text = strings.ReplaceAll(text, address, "<signed address>")
		text = strings.ReplaceAll(text, u.RawQuery, "<query>")
	}
	return errors.New(text)
}

// refusalCode is the code Cloud Storage's XML error answer names, " (Code)", read from at most 64
// KiB of body, or "" where the answer holds none: AccessDenied, SignatureDoesNotMatch or
// ExpiredToken tell an address refused from an object missing (NoSuchKey). Nothing else of the
// answer is kept: a refusal of a signature carries the request it signed, query included.
func refusalCode(body io.Reader) string {
	text, err := io.ReadAll(io.LimitReader(body, 64<<10))
	if err != nil {
		return ""
	}
	var answer struct {
		Code string `xml:"Code"`
	}
	if xml.Unmarshal(text, &answer) != nil || !errorCode.MatchString(answer.Code) {
		return ""
	}
	return " (" + answer.Code + ")"
}

// errorCode is the shape of a Cloud Storage XML error's code: a word of letters.
var errorCode = regexp.MustCompile(`^[A-Za-z]{1,64}$`)

// writeRefs writes each ref of a manifest at its path in gitDir, its exact bytes, making its
// directory; a ref git init wrote already (HEAD) is written over. Each path is one check took
// (refPathRefusal).
func writeRefs(gitDir string, refs []PackRef) error {
	for _, ref := range refs {
		target := filepath.Join(gitDir, filepath.FromSlash(ref.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return &bucketFailure{reasonWrite, fmt.Errorf("the ref %s: %w", ref.Path, err)}
		}
		if err := os.WriteFile(target, ref.Bytes, 0o644); err != nil {
			return &bucketFailure{reasonWrite, fmt.Errorf("the ref %s: %w", ref.Path, err)}
		}
	}
	return nil
}
