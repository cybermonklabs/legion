package workspace

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

	"github.com/sjawhar/envoy/oauth2call"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// PackManifest is the repository cache's answer to GET /packs, one JSON object, where its packs are
// kept in a Cloud Storage bucket (runtime.kubernetes.repository_cache.bucket): the bucket's name,
// the clone's refs, and every pack of the clone with its index, each an object of the bucket that
// the answer names with a signed address to download it by. The cache's /packs handler is its only
// writer, and an issue pod's fetch its reader (Fetch, which holds every member to check before it
// asks for any object).
//
// One answer is one snapshot of the cache's clone, the same one /tar would stream: Refs are exactly
// the files of that snapshot outside objects/pack/ (FeedEntry's rules), and Objects every pack and
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
// each download for it, and a fetch takes an address at no other host, but for a test's or the
// burst Job's loopback double (PackManifest.check).
const CloudStorageHost = "storage.googleapis.com"

// PackPrefix is the prefix of every object of repo's in the repository cache's bucket,
// <owner>/<repo>/objects/pack/: its owner and its name lower-cased, since GitHub takes a
// repository's names in any case and the cache must write one prefix for one repository whatever
// case its configuration wrote them in. A bucket serves the repositories of one deployment's
// project: each cache writes under its own repository's prefix alone, and a fetch takes no object
// outside its own (PackManifest.check).
func PackPrefix(repo ghrepo.Repository) string {
	return strings.ToLower(repo.Owner()) + "/" + strings.ToLower(repo.Name()) + "/objects/pack/"
}

// PackFileName reports whether file, an object's name past its PackPrefix, is the name of a pack or
// an index that the clone's tar streams in objects/pack and a feed takes there: one path segment
// that FeedEntry takes in objects/pack, so that the object's name past the prefix is the file's own
// name in the clone.
func PackFileName(file string) bool {
	return file != "" && !strings.Contains(file, "/") && FeedEntry("objects/pack/"+file, false)
}

// BucketFetchTimeout bounds a fetch's one attempt to fill a fresh feed from its repository cache's
// bucket, all of it: the ask of the cache's GET /packs, every download from a signed address, and
// the git verification of what those brought (FetchReport.fromBucket). It is also the life the
// cache signs each address of its answer for (repocache's signedLife is this constant), 15
// minutes, so the attempt never outlives what it was given, and the cache's retirement of an
// object no set names any more, twice this after the switch that left it out, rests on it. It is a
// bound of its own: an attempt that runs it out falls back to the cache's /tar, whose
// verification and the delta from GitHub after it each keep their whole FetchTimeout, and
// FetchBudget counts it beside them.
const BucketFetchTimeout = 15 * time.Minute

// SourceBucket and SourceTar are where a fresh feed's history came from when a repository cache is
// named (FetchReport.Source): the cache's bucket, through the signed addresses its GET /packs gave,
// or its GET /tar, which streams the clone's refs and packs from the cache itself.
const (
	SourceBucket = "bucket"
	SourceTar    = "/tar"
)

// BucketObject is one pack or index a fetch downloaded from the repository cache's bucket and
// verified: its object name as the manifest named it, which the fetch checked is one of its own
// repository's packs or indexes; its exact size; and the SHA-256 the fetch computed of the bytes it
// wrote, which matched the manifest's.
type BucketObject struct {
	Name   string
	Size   int64
	SHA256 string
}

// The short reasons a fetch gives for filling a fresh feed from the cache's /tar rather than its
// bucket (FetchReport.TarReason), each the start of the reason's text: /packs not served at all (a
// cache with no bucket, or of a release that has no /packs, which answers the mux's plain 404
// naming no repository); an answer that is no manifest to use (unavailable: not 200, or not read); a
// manifest that does not hold (invalid: a member the fetch refuses, before any object is asked for);
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

// bucketFailure is why an attempt to fill the feed from the repository cache's bucket ended and
// the cache's /tar fills it instead: one of the short reasons above, and what failed. Its text never
// holds a signed address or its query (downloadCause), nor a ref's contents, and it holds the
// cache's and the bucket's words only as the redaction rule reads them: a status by its code and
// standard phrase (oauth2call.StatusLine), a member of the cache's manifest as manifestWords
// prints it, a bucket's error code as refusalCode does, and a failed ask or read in the rule's own
// words (oauth2call.Unanswered, oauth2call.ReadAnswer, oauth2call.StreamAnswer).
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

// packManifestLimit is the most bytes of a /packs answer a fetch reads: the refs of a clone, whose
// packed-refs is a few megabytes for a repository of hundreds of thousands of refs, in base64, and
// one line per pack and index. An answer past it is refused as an invalid manifest rather than
// read whole into the pod's memory.
const packManifestLimit = 64 << 20

// bucketDownloads is how many objects a fetch downloads from the bucket at once: a clone holds a
// few large packs, each with a small index, so four keep the pod's link busy without opening a
// connection per object.
const bucketDownloads = 4

// downloadChunk and downloadChunks are the buffers each download reads its body into
// (downloadObject): 1 MiB each, so the file is written and hashed in large steps rather than a TLS
// record, at most 16 KiB, at a time, and at most 8 of them at once, so the body's reader can run 7
// MiB ahead of a write or a hash that pauses. A download makes a buffer only when it first needs
// one, so an index of a few kilobytes takes one, and none holds more than 8 MiB: a fetch's
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
// since a signed address names its object at the one host Fetch checked (PackManifest.check).
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

// fromBucket tries to fill staging, the feed's temporary sibling, from the repository cache's
// bucket, all within BucketFetchTimeout of its start, by the request's clock: it asks the cache's
// GET /packs with its token (askPacks), holds the whole answer to check before it asks for any
// object (PackManifest.check), makes staging a bare repository (`git init --bare`), downloads every
// pack and index from its signed address into objects/pack (downloadObjects), writes each ref's
// exact bytes, and verifies the result as verifyCache does a tar that carried every pack's index:
// each pack and its index to the checksums git wrote into them, and every object the refs reach
// with `git fsck --connectivity-only`. No pack is indexed here: each index comes from the bucket,
// vouched for by the SHA-256 the cache sent, and a pack without one is an error. On success it
// records the bucket as the feed's source, the objects it verified, their bytes and the times.
//
// It answers a *bucketFailure when the cache's /tar must fill the feed instead (Fetch removes what
// it wrote first), and any other error for a failure that ends the fetch: an answer that refuses the
// pod's token or names another repository (askCache), which /tar would meet too, and a staging
// repository git could not make.
func (r *FetchReport) fromBucket(ctx context.Context, run Runner, request FetchRequest, staging string, held *fetchCredential) error {
	started := request.Clock.Now()
	deadline := started.Add(BucketFetchTimeout)
	bounded, cancel := context.WithTimeout(ctx, BucketFetchTimeout)
	defer cancel()
	manifest, words, err := askPacks(bounded, request)
	if err != nil {
		return err
	}
	if err := manifest.check(request, words); err != nil {
		return &bucketFailure{reasonInvalid, err}
	}
	// The init runs beside the repository it makes rather than in it, as unpackCache's does.
	if _, err := RunChecked(ctx, run, slices.Concat(feedGit, []string{"init", "--bare", "--quiet", staging}), isolatedGitConfig, filepath.Dir(staging)); err != nil {
		return err
	}
	objects, size, err := downloadObjects(bounded, manifest, staging)
	if err != nil {
		if bounded.Err() != nil && ctx.Err() == nil {
			return &bucketFailure{reasonDownload, fmt.Errorf("the bucket attempt's %s bound ran out", BucketFetchTimeout)}
		}
		return err
	}
	if err := writeRefs(staging, manifest.Refs, words); err != nil {
		return err
	}
	verifying := request.Clock.Now()
	if err := verifyCache(ctx, run, request, staging, held, deadline, BucketFetchTimeout, false); err != nil {
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
	r.BucketDuration, r.VerifyDuration = verifying.Sub(started), request.Clock.Now().Sub(verifying)
	return nil
}

// askPacks is the repository cache's GET /packs, with its token (askCache), within packsAsk, read
// up to packManifestLimit (oauth2call.ReadAnswer) and decoded (Answer.Decode): the manifest of a 200
// answer, and the words its members print as (manifestWords). A 404 naming no repository is a cache
// that does not serve /packs at all; an answer askCache refuses (a 401, or one naming another
// repository) is that refusal, outside the issue's volume, as it is for /tar; any other answer, an
// ask the cache did not answer, and a body that is no manifest are bucket failures. The answer is
// the cache's words, or those of whatever answers at its address, which can echo the bearer the ask
// carried: its status prints by its code and standard phrase (oauth2call.StatusLine), and a read or
// a decode that failed in the rule's own words, never the answer's.
func askPacks(ctx context.Context, request FetchRequest) (PackManifest, manifestWords, error) {
	asking, cancel := context.WithTimeout(ctx, packsAsk)
	defer cancel()
	response, err := askCache(asking, request, "/packs", true)
	if response != nil {
		defer response.Body.Close()
	}
	cache := strings.TrimSuffix(request.CacheURL, "/")
	switch {
	case response == nil:
		return PackManifest{}, manifestWords{}, &bucketFailure{reasonUnavailable, err}
	case response.StatusCode == http.StatusNotFound && len(response.Header.Values(CacheRepositoryHeader)) == 0:
		return PackManifest{}, manifestWords{}, &bucketFailure{reason: reasonNotServed}
	case err != nil:
		return PackManifest{}, manifestWords{}, OutsideVolume(err)
	case response.StatusCode != http.StatusOK:
		return PackManifest{}, manifestWords{}, &bucketFailure{reasonUnavailable, fmt.Errorf("GET %s/packs answered %s", cache, oauth2call.StatusLine(response.StatusCode))}
	}
	answer, err := oauth2call.ReadAnswer(response, packManifestLimit)
	switch {
	case errors.Is(err, oauth2call.ErrAnswerTooLarge):
		return PackManifest{}, manifestWords{}, &bucketFailure{reasonInvalid, fmt.Errorf("GET %s/packs answered more than %d bytes", cache, packManifestLimit)}
	case err != nil:
		return PackManifest{}, manifestWords{}, &bucketFailure{reasonUnavailable, fmt.Errorf("read GET %s/packs: %w", cache, err)}
	}
	var decoded packsAnswer
	if err := answer.Decode(&decoded); err != nil {
		return PackManifest{}, manifestWords{}, &bucketFailure{reasonInvalid, fmt.Errorf("GET %s/packs answered no manifest: %w", cache, err)}
	}
	manifest := PackManifest{Bucket: decoded.Bucket, Refs: make([]PackRef, len(decoded.Refs)), Objects: decoded.Objects}
	for i, ref := range decoded.Refs {
		contents, err := base64.StdEncoding.DecodeString(ref.Bytes)
		if err != nil {
			return PackManifest{}, manifestWords{}, &bucketFailure{reasonInvalid, fmt.Errorf("GET %s/packs answered no manifest: the bytes of its ref %d are not base64", cache, i+1)}
		}
		manifest.Refs[i] = PackRef{Path: ref.Path, Bytes: contents}
	}
	return manifest, newManifestWords(answer, manifest.Objects), nil
}

// packsAnswer is a /packs answer as askPacks decodes it (Answer.Decode): PackManifest's members,
// of the same names, with each ref's bytes as the base64 text JSON carries them in, which askPacks
// decodes as encoding/json does a byte slice. Decode refuses a byte slice, whose text would leave
// the answer as plain text; a ref's bytes are data, which nothing prints.
type packsAnswer struct {
	Bucket string `json:"bucket"`
	Refs   []struct {
		Path  string `json:"path"`
		Bytes string `json:"bytes"`
	} `json:"refs"`
	Objects []PackObject `json:"objects"`
}

// manifestWords is how a fetch prints the members of a /packs answer: each is the cache's words,
// or those of whatever answers at its address, which can echo the bearer the ask carried, or a
// signed address the answer gives. A member prints as another party's words (said): the rule's
// reading, with every credential the ask carried (the answer's record: the cache's bearer, and the
// query values and user information of the cache's URL) and every query value of every object's
// address in the answer redacted first. A member that leaves the fetch as it came - the bucket's
// name, an object's name or SHA-256, which a refusal and FetchReport.Objects name as they are - is
// held first to no run of a credential the ask carried (oauth2call.Echoes, PackManifest.check):
// their shapes, hex digits and a bucket name's characters, hold the cache's bearer, 64 hex digits,
// whole.
type manifestWords struct {
	answer oauth2call.Answer
	signed []string
}

// newManifestWords is the words of answer, whose objects give the addresses objects name.
func newManifestWords(answer oauth2call.Answer, objects []PackObject) manifestWords {
	words := manifestWords{answer: answer}
	for _, object := range objects {
		if _, query, ok := strings.Cut(object.URL, "?"); ok {
			values, _ := url.ParseQuery(query)
			for _, value := range values {
				words.signed = append(words.signed, value...)
			}
		}
	}
	return words
}

// said is member, a member of the answer, as a message prints it.
func (w manifestWords) said(member string) oauth2call.RemoteText {
	return w.answer.Said(member).WithoutTokens(w.signed...)
}

// bucketName is the characters and ends of a Cloud Storage bucket's name, which ValidBucketName
// holds with its lengths.
var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*[a-z0-9]$`)

// ValidBucketName reports whether name has a Cloud Storage bucket's shape: 3 to 222 lower-case
// letters, digits, dashes, underscores and dots, starting and ending with a letter or a digit, with
// 1 to 63 of them between two dots, so a name with no dot is 3 to 63 long, and it stands in an
// address's path as it is (https://cloud.google.com/storage/docs/buckets#naming). It is the one
// rule of a bucket's name: the loader holds runtime.kubernetes.repository_cache.bucket to it, and a
// fetch holds a /packs manifest's bucket to it (PackManifest.check), so the loader accepts no
// bucket that every fetch then refuses, nor the reverse. It holds the shape alone, which is what
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

// check is why the manifest may not fill request's feed, or nil when every member of it is one the
// fetch takes. It holds the whole answer before the fetch asks for any object:
//   - the bucket is a bucket's name, holding no run of a credential the ask carried;
//   - it lists at least one ref and at least one object: a whole set of the cache's clone lists at
//     least one ref and one pack with its index, so an answer that lists no ref or no object is no
//     whole set, and taken, it would leave the whole of the feed's history to the fetch from GitHub
//     that follows while the report named the bucket as its source;
//   - each ref is a file a feed takes from its cache (FeedEntry), outside objects/pack/, its path
//     slash-separated, clean and inside the repository, named once, and no other ref's directory;
//   - each object is a pack or an index of request's repository, its PackPrefix,
//     <owner>/<repo>/objects/pack/ (the two lower-cased), then one file name a feed takes there
//     (PackFileName), named once, of a size of 0 or more and a SHA-256 of 64 lower-case hex digits,
//     its name and its SHA-256 holding no run of a credential the ask carried, each pack listed
//     with its index and each index with its pack;
//   - each object's address is a GET of that object in that bucket with a query, at Cloud Storage
//     over HTTPS, or, only where the cache itself is on a loopback address (a test, or the burst
//     Job, which run the cache and a double of Cloud Storage beside each other), over HTTP at a
//     loopback address.
//
// The refusal names the member, and never the address: that is a credential for its object. A
// member it refuses for its shape is the cache's words as they came, and prints as words prints it
// (manifestWords.said); one it refuses for a run of a credential the ask carried is named by its
// place. A member it takes prints as it is from here on: the bucket's name, each object's name and
// SHA-256 (manifestWords).
func (m PackManifest) check(request FetchRequest, words manifestWords) error {
	const carried = "a run of a credential the ask of /packs carried"
	if !ValidBucketName(m.Bucket) {
		return fmt.Errorf("it names the bucket %q, which is no Cloud Storage bucket's name", words.said(m.Bucket))
	}
	if oauth2call.Echoes(words.answer, m.Bucket) {
		return errors.New("it names a bucket holding " + carried)
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
			return fmt.Errorf("it lists the ref %q, which %s", words.said(ref.Path), why)
		}
		if refs[ref.Path] {
			return fmt.Errorf("it lists the ref %q twice", words.said(ref.Path))
		}
		refs[ref.Path] = true
	}
	for ref := range refs {
		for dir := path.Dir(ref); dir != "."; dir = path.Dir(dir) {
			if refs[dir] {
				return fmt.Errorf("it lists the ref %q, which is the directory of the ref %q", words.said(dir), words.said(ref))
			}
		}
	}
	prefix := PackPrefix(request.Repo)
	loopback := onLoopback(request.CacheURL)
	objects := map[string]bool{}
	for i, object := range m.Objects {
		file, ours := strings.CutPrefix(object.Name, prefix)
		if !ours || !PackFileName(file) {
			return fmt.Errorf("it lists the object %q, which is no pack or index of %s (%spack-<id>.pack or .idx)", words.said(object.Name), request.Repo, prefix)
		}
		if oauth2call.Echoes(words.answer, object.Name) {
			return fmt.Errorf("it lists object %d with a name holding %s", i+1, carried)
		}
		if objects[object.Name] {
			return fmt.Errorf("it lists the object %s twice", object.Name)
		}
		objects[object.Name] = true
		if object.Size < 0 {
			return fmt.Errorf("it lists %s with a size of %d bytes", object.Name, object.Size)
		}
		if !sha256Hex.MatchString(object.SHA256) {
			return fmt.Errorf("it lists %s with the SHA-256 %q, which is not 64 lower-case hex digits", object.Name, words.said(object.SHA256))
		}
		if oauth2call.Echoes(words.answer, object.SHA256) {
			return fmt.Errorf("it lists %s with a SHA-256 holding %s", object.Name, carried)
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

// refPathRefusal is why name, a ref path of a manifest, is no ref file a feed takes, or "" when it
// is one: a clean path inside the repository that FeedEntry takes as a file (HEAD, packed-refs,
// refs/...), and nothing under objects/pack/, which the bucket's objects alone fill.
func refPathRefusal(name string) string {
	switch {
	case !filepath.IsLocal(filepath.FromSlash(name)) || path.Clean(name) != name:
		return "is not a path inside the feed"
	case strings.HasPrefix(name, "objects/pack/") || !FeedEntry(name, false):
		return "is not one of a repository's refs"
	}
	return ""
}

// addressRefusal is why address is not a signed download of bucket/name that a fetch may ask, or
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

// downloadObjects downloads every object of manifest into staging's objects/pack, at most
// bucketDownloads at once (downloadObject), and answers each as it verified it, in the manifest's
// order, with their bytes together. The packs start first, largest first, then the indexes, largest
// first, so a large pack is never left waiting behind many small indexes until its address expires.
// The first failure ends the rest, and is the answer.
func downloadObjects(ctx context.Context, manifest PackManifest, staging string) ([]BucketObject, int64, error) {
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
			got, err := downloadObject(downloading, object, filepath.Join(staging, "objects", "pack", path.Base(object.Name)))
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
// whose body falls below the bound, is ended, naming it. No failure names the address or its query,
// and none prints what the bucket wrote, or whatever answers at the address, which can echo the
// signed query the request carried, but as the rule reads it: a status by its code and standard
// phrase (oauth2call.StatusLine), an error's code as refusalCode prints it, an ask that got no
// answer as downloadCause names it, and a read of the body that failed as oauth2call.StreamAnswer
// says it.
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
		return failed(reasonDownload, downloadCause(err, ask, stream))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return failed(reasonDownload, fmt.Errorf("the bucket answered %s%s", oauth2call.StatusLine(response.StatusCode), refusalCode(response)))
	}
	if response.ContentLength >= 0 && response.ContentLength != object.Size {
		return failed(reasonSize, fmt.Errorf("the bucket sends %d bytes, not the %d the manifest states", response.ContentLength, object.Size))
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return failed(reasonWrite, err)
	}
	// The body is data, streamed into the file as it arrives (oauth2call.StreamAnswer), and read to
	// one byte past the manifest's size at most, so a longer one is stopped at its first byte past it.
	stream.reader = oauth2call.StreamAnswer(response, object.Size+1)
	free := make(chan []byte, downloadChunks)
	for range downloadChunks {
		free <- nil
	}
	full, written := make(chan []byte, downloadChunks), make(chan []byte, downloadChunks)
	var readFailure error
	go func() {
		defer close(full)
		readFailure = readChunks(fetching, stream, free, full)
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
	case readFailure != nil && stream.stalled.Load():
		return failed(reasonDownload, errDownloadStalled)
	case readFailure != nil:
		return failed(reasonDownload, readFailure)
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

// downloadCause is what err, the failure of ask, a download that got no answer, says without the
// address: errDownloadStalled once the low-speed watch ended it, errBucketRedirect where the bucket
// answered a redirect, and otherwise net/http's operation and the failure by its transport text or
// its class (oauth2call.Unanswered), the address named "<signed address>". A *url.Error carries the
// whole address, query and signature included, and net/http's own text of an answer it could not
// read quotes what the answering party wrote, which can echo that query.
func downloadCause(err error, ask *http.Request, stream *countingReader) error {
	switch {
	case stream.stalled.Load():
		return errDownloadStalled
	case errors.Is(err, errBucketRedirect):
		return errBucketRedirect
	}
	return oauth2call.Unanswered(err, ask, "<signed address>")
}

// refusalCode is the code Cloud Storage's XML error answer names, " (Code)", read from at most 64
// KiB of response's body (oauth2call.ReadAnswer), or "" where the answer holds none:
// AccessDenied, SignatureDoesNotMatch or ExpiredToken tell an address refused from an object
// missing (NoSuchKey). Nothing else of the answer is kept: a refusal of a signature carries the
// request it signed, query included. The code is the bucket's words, or those of whatever answers
// at the address, held to a word of letters and printed as the rule reads it (Answer.Said), with
// every credential the request carried, each value of the signed query, redacted.
func refusalCode(response *http.Response) string {
	answer, err := oauth2call.ReadAnswer(response, 64<<10)
	if err != nil {
		return ""
	}
	var body bytes.Buffer
	if _, err := answer.StreamTo(&body); err != nil {
		return ""
	}
	var refusal struct {
		Code string `xml:"Code"`
	}
	if xml.Unmarshal(body.Bytes(), &refusal) != nil || !errorCode.MatchString(refusal.Code) {
		return ""
	}
	return " (" + answer.Said(refusal.Code).String() + ")"
}

// errorCode is the shape of a Cloud Storage XML error's code: a word of letters.
var errorCode = regexp.MustCompile(`^[A-Za-z]{1,64}$`)

// writeRefs writes each ref of a manifest at its path in staging, its exact bytes, making its
// directory; a ref git init wrote already (HEAD) is written over. Each path is one check took
// (refPathRefusal), and the cache's words: a write that failed names the ref as words prints it
// (manifestWords.said), and of a failure on a path the operation and the system's reason alone,
// since the path holds the ref's name as it came, as entryFailure does of a tar's entry.
func writeRefs(staging string, refs []PackRef, words manifestWords) error {
	for _, ref := range refs {
		target := filepath.Join(staging, filepath.FromSlash(ref.Path))
		err := os.MkdirAll(filepath.Dir(target), 0o755)
		if err == nil {
			err = os.WriteFile(target, ref.Bytes, 0o644)
		}
		if err == nil {
			continue
		}
		if onPath := (*fs.PathError)(nil); errors.As(err, &onPath) {
			err = fmt.Errorf("%s: %w", onPath.Op, onPath.Err)
		}
		return &bucketFailure{reasonWrite, fmt.Errorf("the ref %s: %w", words.said(ref.Path), err)}
	}
	return nil
}
