package repocache

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/repocache/fakegcs"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// testBucket is the bucket the tests' packs live in: the placeholder bucket the double's fixtures
// name.
const testBucket = "example-legion-packs"

// testPrefix is where testRepo's packs live: its owner and name lower-cased, then the clone's
// objects/pack/.
const testPrefix = "acme/widgets/objects/pack/"

// testRepo is the repository whose packs the tests' bucket holds, written with capitals, as a
// configuration may write it, so that the prefix's lower case shows.
var testRepo = ghrepo.MustParse("Acme/Widgets")

// bucketMode is one of the two ways a cache reaches its bucket, with package fakegcs's double in
// Google's place. setup makes the double and points the process's environment at it as the mode
// needs, and answers the opener the cache's own code is: openBucketAt with the double's endpoints
// in production, openBucket itself in emulator mode. production says which it is; a production
// signed address names Cloud Storage's own host, and an emulator's the double's.
type bucketMode struct {
	name       string
	production bool
	setup      func(t *testing.T, clock *bucketClock) bucketDouble
}

// bucketDouble is a double one mode's bucket reaches: the double, where it serves, the credential
// file GOOGLE_APPLICATION_CREDENTIALS names in production (unreadableCredentials; "" in emulator
// mode), and the opener of a bucket of the mode against it.
type bucketDouble struct {
	double      *fakegcs.Server
	running     *fakegcs.Running
	credentials string
	open        func(ctx context.Context, name string) (*bucket, error)
}

// productionMode drives the production path against the double: the metadata server's identity
// and token, through GCE_METADATA_HOST, and the Storage and IAM Credentials requests through the
// test's seam (googleEndpoints), with the double requiring the metadata server's token as the
// bearer of every Storage and IAM request, and GOOGLE_APPLICATION_CREDENTIALS naming a file no
// code may read.
var productionMode = bucketMode{name: "production", production: true, setup: func(t *testing.T, clock *bucketClock) bucketDouble {
	double, running := startDouble(t, fakegcs.Options{Now: clock.Now, RequireToken: true})
	t.Setenv(emulatorHostEnv, "")
	t.Setenv("GCE_METADATA_HOST", running.Addr())
	credentials := unreadableCredentials(t)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credentials)
	at := googleEndpoints{storage: running.URL() + "/storage/v1/", iam: running.URL() + "/"}
	return bucketDouble{double: double, running: running, credentials: credentials, open: func(ctx context.Context, name string) (*bucket, error) {
		return openBucketAt(ctx, name, testRepo, at)
	}}
}}

// emulatorMode drives emulator mode, as the real-binary harness and the burst Job run the cache:
// STORAGE_EMULATOR_HOST names the double, which answers and signs as EmulatorEmail, and
// GCE_METADATA_HOST names it too, so that a metadata request the mode must not make would be
// counted there.
var emulatorMode = bucketMode{name: "emulator", setup: func(t *testing.T, clock *bucketClock) bucketDouble {
	double, running := startDouble(t, fakegcs.Options{Email: EmulatorEmail, Now: clock.Now})
	t.Setenv(emulatorHostEnv, running.Addr())
	t.Setenv("GCE_METADATA_HOST", running.Addr())
	return bucketDouble{double: double, running: running, open: func(ctx context.Context, name string) (*bucket, error) {
		return openBucket(ctx, name, testRepo)
	}}
}}

// bucketRow is one row of TestBucketAPIAndWorkloadIdentity, run in a mode.
type bucketRow struct {
	name string
	run  func(t *testing.T, mode bucketMode)
}

// TestBucketAPIAndWorkloadIdentity holds the cache's view of its bucket (bucket) to Cloud
// Storage's JSON API and IAM Credentials' signBlob, as package fakegcs's double answers them over
// real HTTP, in both modes: production, whose identity and token come from the metadata server
// alone, and emulator mode. In each: an upload is visible only once put returned; an object that
// holds the file's bytes is taken with no upload; one that holds other bytes, there before the
// upload or created during it, is refused and left as it was; every request names the
// repository's lowercase prefix and no other repository's object is listed or deleted; a delete
// removes only the generation it names; a signed address is a V4 GET of the object for 900
// seconds, and a signer's failure is no address; and a cache told no bucket opens none and sends
// nothing. Production also holds that only the metadata server's identity reaches Storage and IAM,
// and that no credential file is read; emulator mode, that it sends no credential and takes only a
// loopback STORAGE_EMULATOR_HOST.
func TestBucketAPIAndWorkloadIdentity(t *testing.T) {
	t.Run("object names are the clone's packs and indexes under the lowercase prefix", testObjectNames)
	common := []bucketRow{
		{"an upload is visible only once put has returned", testUploadVisibleOnceClosed},
		{"an object that holds the file's bytes is taken with no upload", testMatchingObjectReused},
		{"an object that holds other bytes is refused and left as it is", testDifferingObjectRefused},
		{"an upload that another upload beat takes the object only where it holds the same bytes", testCreateConflict},
		{"every request names the repository's lowercase prefix and no other repository's object is touched", testRequestsNameThePrefix},
		{"a delete removes only the generation it names", testDeleteMatchesGeneration},
		{"a signed address is a V4 GET of the object for 900 seconds, and a signer's failure is no address", testSignedDownload},
		{"a cache told no bucket opens none and sends no request", testNoBucketNoRequest},
	}
	for _, tc := range []struct {
		mode bucketMode
		rows []bucketRow
	}{
		{productionMode, []bucketRow{
			{"only the metadata server's identity reaches Storage and IAM, and no credential file is read", testOnlyMetadataIdentity},
		}},
		{emulatorMode, []bucketRow{
			{"emulator mode sends no credential and asks no metadata server", testEmulatorSendsNoCredential},
			{"a STORAGE_EMULATOR_HOST that is not a loopback host:port is refused before any request", testEmulatorHostIsLoopback},
		}},
	} {
		t.Run(tc.mode.name, func(t *testing.T) {
			for _, row := range append(slices.Clone(common), tc.rows...) {
				t.Run(row.name, func(t *testing.T) { row.run(t, tc.mode) })
			}
		})
	}
}

// testObjectNames: an object's name is the repository's lowercase prefix then a file name the
// clone's tar streams in objects/pack, and clonePath and objectName map it to and from that file's
// path in the clone, refusing every other shape.
func testObjectNames(t *testing.T) {
	if got := workspace.PackPrefix(testRepo); got != testPrefix {
		t.Fatalf("Acme/Widgets's prefix is %q, not %q", got, testPrefix)
	}
	b := &bucket{name: testBucket, prefix: workspace.PackPrefix(testRepo)}
	sha1Hex, sha256Hex := strings.Repeat("0123456789abcdef", 2)+"01234567", strings.Repeat("abcdef0123456789", 4)
	for _, file := range []string{"pack-" + sha1Hex + ".pack", "pack-" + sha1Hex + ".idx", "pack-" + sha256Hex + ".pack", "pack-" + sha256Hex + ".idx"} {
		path, err := b.clonePath(testPrefix + file)
		if err != nil || path != "objects/pack/"+file {
			t.Errorf("clonePath(%q) = %q, %v; want objects/pack/%s", testPrefix+file, path, err, file)
		}
		name, err := b.objectName("objects/pack/" + file)
		if err != nil || name != testPrefix+file {
			t.Errorf("objectName(objects/pack/%s) = %q, %v; want %q", file, name, err, testPrefix+file)
		}
	}
	for _, name := range []string{
		"acme/gadgets/objects/pack/pack-" + sha1Hex + ".pack",     // another repository's
		"Acme/Widgets/objects/pack/pack-" + sha1Hex + ".pack",     // this one's in its configured case
		"acme/widgets-old/objects/pack/pack-" + sha1Hex + ".pack", // one whose name begins with this one's
		testPrefix + "pack-" + sha1Hex + ".rev",                   // a reverse index, which git builds itself
		testPrefix + "pack-" + sha1Hex + ".bitmap",
		testPrefix + "multi-pack-index",
		testPrefix + "tmp_pack_" + sha1Hex,
		testPrefix + "pack-" + sha1Hex + ".pack.lock",
		testPrefix + "pack-" + strings.ToUpper(sha1Hex) + ".pack",
		testPrefix + "pack-" + sha1Hex[:39] + ".pack",
		testPrefix + "nested/pack-" + sha1Hex + ".pack",
		testPrefix + "../pack-" + sha1Hex + ".pack",
		testPrefix,
		"",
	} {
		if path, err := b.clonePath(name); err == nil {
			t.Errorf("clonePath(%q) = %q, want a refusal", name, path)
		}
	}
	for _, path := range []string{"objects/pack/pack-" + sha1Hex + ".rev", "objects/pack/nested/pack-" + sha1Hex + ".pack", "objects/pack/", "packed-refs", "refs/heads/main", "HEAD", "pack-" + sha1Hex + ".pack"} {
		if name, err := b.objectName(path); err == nil {
			t.Errorf("objectName(%q) = %q, want a refusal", path, name)
		}
	}
}

// testUploadVisibleOnceClosed: an upload held in the double after its last byte arrived, before
// its object became visible, leaves put waiting and the object absent from a metadata read and
// from list; once released, put returns, and the object is there with the file's size, CRC32C
// and sha256 metadata.
func testUploadVisibleOnceClosed(t *testing.T, mode bucketMode) {
	rig := openRig(t, mode)
	ctx := testwait.Context(t)
	content := randomContent(t, 300<<10)
	local := writeLocal(t, content)
	name := packObject(content, "pack")
	hold := rig.double.HoldNext(func(bucket, object string) bool { return bucket == testBucket && object == name })
	defer hold.Release()
	done := make(chan error, 1)
	go func() { done <- rig.bucket.put(ctx, name, local) }()
	testwait.Receive(t, "the upload to stop at the double's hold", hold.Reached())
	select {
	case err := <-done:
		t.Fatalf("put returned (%v) while its upload was held, before the object was stored", err)
	default:
	}
	if status, _ := rig.resource(t, name); status != http.StatusNotFound {
		t.Fatalf("a metadata read of %s during its upload answered %d, not 404", name, status)
	}
	if listed := rig.list(t); len(listed) != 0 {
		t.Fatalf("list answered %v during the upload, not nothing", listed)
	}
	hold.Release()
	if err := testwait.Receive(t, "put to return once its upload was released", done); err != nil {
		t.Fatalf("put of %s failed: %v", name, err)
	}
	status, res := rig.resource(t, name)
	if status != http.StatusOK {
		t.Fatalf("a metadata read of %s after put answered %d", name, status)
	}
	requireHolds(t, res, content)
	if res.Metadata[sha256Metadata] != local.SHA256 {
		t.Fatalf("%s's sha256 metadata is %q, not the file's %s", name, res.Metadata[sha256Metadata], local.SHA256)
	}
	listed := rig.list(t)
	if got, ok := listed[name]; len(listed) != 1 || !ok || strconv.FormatInt(got.Generation, 10) != res.Generation || got.SHA256 != local.SHA256 || got.Size != local.Size || got.CRC32C != local.CRC32C {
		t.Fatalf("list answered %+v, not %s at generation %s with the file's size, CRC32C and SHA-256", listed, name, res.Generation)
	}
}

// testMatchingObjectReused: an object already under the name with the file's size, CRC32C and
// sha256 metadata is the file's: put returns with no upload request, and the object keeps its
// generation.
func testMatchingObjectReused(t *testing.T, mode bucketMode) {
	rig := openRig(t, mode)
	content := randomContent(t, 32<<10)
	local := writeLocal(t, content)
	name := packObject(content, "idx")
	generation := rig.seed(t, name, content, map[string]string{sha256Metadata: local.SHA256})
	uploads := rig.double.Count(fakegcs.RouteUpload).Requests
	if err := rig.bucket.put(testwait.Context(t), name, local); err != nil {
		t.Fatalf("put of a file whose object is there already failed: %v", err)
	}
	if got := rig.double.Count(fakegcs.RouteUpload).Requests; got != uploads {
		t.Fatalf("put sent %d upload requests for an object already there", got-uploads)
	}
	if _, res := rig.resource(t, name); res.Generation != strconv.FormatInt(generation, 10) {
		t.Fatalf("the object is at generation %s after put, not the %d it was put at", res.Generation, generation)
	}
}

// testDifferingObjectRefused: an object already under the name that differs from the file in
// its size, its CRC32C or its sha256 metadata is refused by name and field, with no upload and no
// delete, and keeps its generation and its bytes.
func testDifferingObjectRefused(t *testing.T, mode bucketMode) {
	content := randomContent(t, 32<<10)
	other := slices.Clone(content)
	other[len(other)/2] ^= 0xff
	for _, tc := range []struct {
		name     string
		stored   []byte
		metadata map[string]string
		field    string
	}{
		{"other bytes of the same size", other, map[string]string{sha256Metadata: sha256Of(other)}, "CRC32C"},
		{"another size", content[:len(content)-1], map[string]string{sha256Metadata: sha256Of(content[:len(content)-1])}, "size"},
		{"the same bytes with no sha256 metadata", content, nil, "sha256 metadata"},
		{"the same bytes with another sha256 metadata", content, map[string]string{sha256Metadata: sha256Of(other)}, "sha256 metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := openRig(t, mode)
			local := writeLocal(t, content)
			name := packObject(content, "pack")
			generation := rig.seed(t, name, tc.stored, tc.metadata)
			uploads, deletes := rig.double.Count(fakegcs.RouteUpload).Requests, rig.double.Count(fakegcs.RouteDelete).Requests
			err := rig.bucket.put(testwait.Context(t), name, local)
			if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "another "+tc.field) {
				t.Fatalf("put over an object holding %s answered %v, not a refusal naming %s and its %s", tc.name, err, name, tc.field)
			}
			if rig.double.Count(fakegcs.RouteUpload).Requests != uploads || rig.double.Count(fakegcs.RouteDelete).Requests != deletes {
				t.Fatalf("put refused an object and still sent an upload or a delete")
			}
			status, res := rig.resource(t, name)
			if status != http.StatusOK || res.Generation != strconv.FormatInt(generation, 10) {
				t.Fatalf("the refused object reads %d at generation %s, not 200 at %d", status, res.Generation, generation)
			}
			requireHolds(t, res, tc.stored)
		})
	}
}

// testCreateConflict: an object created under the name while put's own upload was held, after
// put found the name free, makes the upload's create-only condition fail (412); put then reads
// the object again and takes it where it holds the file's bytes, or refuses it, by name, where it
// holds others, leaving it at the generation the other upload wrote.
func testCreateConflict(t *testing.T, mode bucketMode) {
	content := randomContent(t, 32<<10)
	other := slices.Clone(content)
	other[0] ^= 0xff
	for _, tc := range []struct {
		name    string
		stored  []byte
		refused bool
	}{
		{"the same bytes", content, false},
		{"other bytes", other, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := openRig(t, mode)
			ctx := testwait.Context(t)
			local := writeLocal(t, content)
			name := packObject(content, "pack")
			hold := rig.double.HoldNext(func(bucket, object string) bool { return bucket == testBucket && object == name })
			defer hold.Release()
			done := make(chan error, 1)
			go func() { done <- rig.bucket.put(ctx, name, local) }()
			testwait.Receive(t, "the upload to stop at the double's hold", hold.Reached())
			generation := rig.seed(t, name, tc.stored, map[string]string{sha256Metadata: sha256Of(tc.stored)})
			gets := rig.double.Count(fakegcs.RouteGet).Requests
			hold.Release()
			err := testwait.Receive(t, "put to return once its upload was released", done)
			switch {
			case tc.refused && (err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "another CRC32C")):
				t.Fatalf("put that lost the create to an object of other bytes answered %v, not a refusal naming %s and its CRC32C", err, name)
			case !tc.refused && err != nil:
				t.Fatalf("put that lost the create to an object of the same bytes failed: %v", err)
			}
			if got := rig.double.Count(fakegcs.RouteGet).Requests - gets; got != 1 {
				t.Fatalf("put read the object %d times after its create failed, not once", got)
			}
			status, res := rig.resource(t, name)
			if status != http.StatusOK || res.Generation != strconv.FormatInt(generation, 10) {
				t.Fatalf("the object reads %d at generation %s, not 200 at the %d the other upload wrote", status, res.Generation, generation)
			}
			requireHolds(t, res, tc.stored)
		})
	}
}

// testRequestsNameThePrefix: with objects of other repositories in the bucket (another
// repository of the owner, one whose name begins with this one's, and this one's names in their
// configured case, which the cache never writes) and a file under the prefix that is no pack, the
// cache's list, metadata reads, uploads and deletes all name acme/widgets/objects/pack/; list
// answers only the cache's own objects and hands the stray file back as ignored; a put, delete or
// signature of another prefix's object is refused with no request; and every other object keeps
// its generation.
func testRequestsNameThePrefix(t *testing.T, mode bucketMode) {
	rig := openRig(t, mode)
	ctx := testwait.Context(t)
	foreignContent := randomContent(t, 4<<10)
	file := strings.TrimPrefix(packObject(foreignContent, "pack"), testPrefix)
	others := map[string]int64{}
	for _, name := range []string{
		"acme/gadgets/objects/pack/" + file,
		"acme/widgets-old/objects/pack/" + file,
		"Acme/Widgets/objects/pack/" + file,
		testPrefix + "notes.txt",
	} {
		others[name] = rig.seed(t, name, foreignContent, map[string]string{sha256Metadata: sha256Of(foreignContent)})
	}
	first := len(rig.double.Requests())

	listed, ignored, err := rig.bucket.list(ctx)
	if err != nil || len(listed) != 0 || !slices.Equal(ignored, []string{testPrefix + "notes.txt"}) {
		t.Fatalf("list answered %v, ignoring %q (%v), not nothing of its own and %snotes.txt ignored", listed, ignored, err, testPrefix)
	}
	content := randomContent(t, 8<<10)
	local := writeLocal(t, content)
	name := packObject(content, "pack")
	if err := rig.bucket.put(ctx, name, local); err != nil {
		t.Fatalf("put of %s failed: %v", name, err)
	}
	listed = rig.list(t)
	if _, ok := listed[name]; len(listed) != 1 || !ok {
		t.Fatalf("list answered %v, not %s alone", listed, name)
	}
	if err := rig.bucket.delete(ctx, name, listed[name].Generation); err != nil {
		t.Fatalf("delete of %s failed: %v", name, err)
	}
	before := len(rig.double.Requests())
	for other, generation := range others {
		if err := rig.bucket.put(ctx, other, local); err == nil {
			t.Errorf("put of %s, outside the cache's objects, succeeded", other)
		}
		if err := rig.bucket.delete(ctx, other, generation); err == nil {
			t.Errorf("delete of %s, outside the cache's objects, succeeded", other)
		}
		if address, err := rig.bucket.sign(ctx, other); err == nil || address != "" {
			t.Errorf("sign of %s, outside the cache's objects, answered %q, %v", other, address, err)
		}
	}
	requests := rig.double.Requests()
	if len(requests) != before {
		t.Errorf("refusing other prefixes' objects sent %d requests", len(requests)-before)
	}

	seen := map[fakegcs.Route]int{}
	for _, request := range requests[first:] {
		named, ok := requestName(t, request)
		if !ok {
			continue
		}
		seen[request.Route]++
		if !strings.HasPrefix(named, testPrefix) || request.Route == fakegcs.RouteList && named != testPrefix {
			t.Errorf("a %s request named %q, outside %s", request.Route, named, testPrefix)
		}
	}
	for _, route := range []fakegcs.Route{fakegcs.RouteList, fakegcs.RouteGet, fakegcs.RouteUpload, fakegcs.RouteDelete} {
		if seen[route] == 0 {
			t.Errorf("the cache sent no %s request, so the row holds nothing of it (saw %v)", route, seen)
		}
	}
	for other, generation := range others {
		if status, res := rig.resource(t, other); status != http.StatusOK || res.Generation != strconv.FormatInt(generation, 10) {
			t.Errorf("%s reads %d at generation %s, not 200 at %d", other, status, res.Generation, generation)
		}
	}
}

// testDeleteMatchesGeneration: a delete of another generation than the object's is refused (412)
// and leaves it; one of its own removes it; a second, of an object now gone, counts as done; and
// one with no generation is refused before any request.
func testDeleteMatchesGeneration(t *testing.T, mode bucketMode) {
	rig := openRig(t, mode)
	ctx := testwait.Context(t)
	content := randomContent(t, 8<<10)
	name := packObject(content, "pack")
	if err := rig.bucket.put(ctx, name, writeLocal(t, content)); err != nil {
		t.Fatalf("put of %s failed: %v", name, err)
	}
	generation := rig.list(t)[name].Generation
	if err := rig.bucket.delete(ctx, name, generation+1); statusOf(err) != http.StatusPreconditionFailed {
		t.Fatalf("a delete of another generation answered %v, not a 412 refusal", err)
	}
	if status, res := rig.resource(t, name); status != http.StatusOK || res.Generation != strconv.FormatInt(generation, 10) {
		t.Fatalf("a refused delete left %s reading %d at generation %s", name, status, res.Generation)
	}
	if err := rig.bucket.delete(ctx, name, generation); err != nil {
		t.Fatalf("a delete of the object's generation failed: %v", err)
	}
	if status, _ := rig.resource(t, name); status != http.StatusNotFound {
		t.Fatalf("a deleted object reads %d, not 404", status)
	}
	if err := rig.bucket.delete(ctx, name, generation); err != nil {
		t.Fatalf("a delete of an object already gone answered %v, not done", err)
	}
	deletes := rig.double.Count(fakegcs.RouteDelete).Requests
	if err := rig.bucket.delete(ctx, name, 0); err == nil || rig.double.Count(fakegcs.RouteDelete).Requests != deletes {
		t.Fatalf("a delete with no generation answered %v and sent %d requests, not a refusal with none", err, rig.double.Count(fakegcs.RouteDelete).Requests-deletes)
	}
}

// testSignedDownload: sign's address is a V4 GET of the object, signed as the bucket's identity
// over the host alone, stating 900 seconds, on Cloud Storage's own host over HTTPS in production
// and on the emulator's over HTTP; the double's verifier takes it and serves the object's exact
// bytes to a request with no credential, at 899 seconds past its date too, and refuses it at 900.
// With IAM refusing to sign, with the request's context ended, and for a name that is not the
// cache's, sign answers an error and no address.
func testSignedDownload(t *testing.T, mode bucketMode) {
	rig := openRig(t, mode)
	ctx := testwait.Context(t)
	content := randomContent(t, 64<<10)
	name := packObject(content, "pack")
	if err := rig.bucket.put(ctx, name, writeLocal(t, content)); err != nil {
		t.Fatalf("put of %s failed: %v", name, err)
	}
	address, err := rig.bucket.sign(ctx, name)
	if err != nil {
		t.Fatalf("sign of %s failed: %v", name, err)
	}
	u, err := url.Parse(address)
	if err != nil {
		t.Fatalf("sign answered %q, no URL: %v", address, err)
	}
	scheme, host, email := "http", rig.running.Addr(), EmulatorEmail
	if mode.production {
		scheme, host, email = "https", workspace.CloudStorageHost, rig.double.Email()
	}
	query := u.Query()
	if u.Scheme != scheme || u.Host != host || u.Path != "/"+testBucket+"/"+name {
		t.Errorf("the address is %s://%s%s, not %s://%s/%s/%s", u.Scheme, u.Host, u.Path, scheme, host, testBucket, name)
	}
	if query.Get("X-Goog-Algorithm") != "GOOG4-RSA-SHA256" || query.Get("X-Goog-SignedHeaders") != "host" || !strings.HasPrefix(query.Get("X-Goog-Credential"), email+"/") {
		t.Errorf("the address's V4 parameters are %v, not GOOG4-RSA-SHA256 over the host alone, signed as %s", query, email)
	}
	if got := query.Get("X-Goog-Expires"); got != "900" {
		t.Errorf("the address states X-Goog-Expires=%s, not 900", got)
	}
	signedAt, err := time.Parse("20060102T150405Z", query.Get("X-Goog-Date"))
	if err != nil {
		t.Fatalf("the address's X-Goog-Date %q is no V4 date: %v", query.Get("X-Goog-Date"), err)
	}
	if status, body := rig.download(t, address); status != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("the signed download answered %d and %d bytes, not 200 and the object's %d", status, len(body), len(content))
	}
	rig.clock.Set(signedAt.Add(899 * time.Second))
	if status, body := rig.download(t, address); status != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("the download 899 s past the address's date answered %d and %d bytes, not 200 and the object", status, len(body))
	}
	rig.clock.Set(signedAt.Add(900 * time.Second))
	if status, _ := rig.download(t, address); status != http.StatusForbidden {
		t.Fatalf("the download 900 s past the address's date answered %d, not 403", status)
	}

	rig.double.RefuseSigning(true)
	signs := rig.double.Count(fakegcs.RouteSignBlob).Requests
	if address, err := rig.bucket.sign(ctx, name); address != "" || statusOf(err) != http.StatusForbidden {
		t.Errorf("sign with IAM refusing answered %q, %v, not IAM's 403 and no address", address, err)
	}
	if rig.double.Count(fakegcs.RouteSignBlob).Requests == signs {
		t.Errorf("sign with IAM refusing sent IAM no request")
	}
	rig.double.RefuseSigning(false)
	ended, end := context.WithCancel(ctx)
	end()
	signs = rig.double.Count(fakegcs.RouteSignBlob).Requests
	if address, err := rig.bucket.sign(ended, name); address != "" || !errors.Is(err, context.Canceled) {
		t.Errorf("sign with its request's context ended answered %q, %v, not the context's end and no address", address, err)
	}
	if address, err := rig.bucket.sign(ctx, "acme/gadgets/objects/pack/"+strings.TrimPrefix(name, testPrefix)); address != "" || err == nil {
		t.Errorf("sign of another repository's object answered %q, %v, not a refusal", address, err)
	}
	if got := rig.double.Count(fakegcs.RouteSignBlob).Requests; got != signs {
		t.Errorf("sign sent IAM %d requests it had no context or no name of the cache's for", got-signs)
	}
}

// testNoBucketNoRequest: the constructor the cache calls, given no bucket name, answers no bucket
// and no error, and no request of any kind reaches the double its environment names.
func testNoBucketNoRequest(t *testing.T, mode bucketMode) {
	d := mode.setup(t, &bucketClock{})
	b, err := d.open(testwait.Context(t), "")
	if b != nil || err != nil {
		t.Fatalf("opening no bucket answered %v, %v, not none", b, err)
	}
	if requests := d.double.Requests(); len(requests) != 0 {
		t.Fatalf("opening no bucket sent %d requests: %v", len(requests), requests)
	}
}

// testOnlyMetadataIdentity: in production every Storage and IAM request carries a bearer, and
// with the double refusing any but the token its metadata server issued (RequireToken), list, put,
// sign and the signed download all succeed, as the account the metadata server named, the only
// one the double signs for; the credential file GOOGLE_APPLICATION_CREDENTIALS names, a fifo no one
// writes, is never opened, so no credential discovery ran. The double's refusal is real: the same
// requests sent with no credential, by a bucket in emulator mode at the same double, are refused.
func testOnlyMetadataIdentity(t *testing.T, mode bucketMode) {
	rig := openRig(t, mode)
	ctx := testwait.Context(t)
	content := randomContent(t, 16<<10)
	name := packObject(content, "pack")
	if err := rig.bucket.put(ctx, name, writeLocal(t, content)); err != nil {
		t.Fatalf("put with the metadata server's token failed: %v", err)
	}
	if listed := rig.list(t); len(listed) != 1 {
		t.Fatalf("list with the metadata server's token answered %v", listed)
	}
	address, err := rig.bucket.sign(ctx, name)
	if err != nil {
		t.Fatalf("sign with the metadata server's token failed: %v", err)
	}
	if status, body := rig.download(t, address); status != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("the signed download answered %d and %d bytes", status, len(body))
	}
	if rig.bucket.email != rig.double.Email() {
		t.Errorf("the bucket signs as %s, not the metadata server's account %s", rig.bucket.email, rig.double.Email())
	}
	for _, route := range []fakegcs.Route{fakegcs.RouteList, fakegcs.RouteGet, fakegcs.RouteUpload, fakegcs.RouteSignBlob, fakegcs.RouteMetadataEmail, fakegcs.RouteMetadataToken} {
		got := rig.double.Count(route)
		if got.Requests == 0 {
			t.Errorf("the production path sent no %s request", route)
		}
		if metadataRoute := route == fakegcs.RouteMetadataEmail || route == fakegcs.RouteMetadataToken; !metadataRoute && got.WithAuthorization != got.Requests {
			t.Errorf("%d of %d %s requests carried no bearer", got.Requests-got.WithAuthorization, got.Requests, route)
		}
	}
	if got := rig.double.Count(fakegcs.RouteOther).Requests; got != 0 {
		t.Errorf("the production path sent %d requests the double serves no route for", got)
	}
	requireUnread(t, rig.credentials)

	t.Setenv(emulatorHostEnv, rig.running.Addr())
	anonymous, err := openBucket(ctx, testBucket, testRepo)
	if err != nil {
		t.Fatalf("open a bucket that sends no credential: %v", err)
	}
	defer anonymous.close()
	if _, _, err := anonymous.list(ctx); statusOf(err) != http.StatusUnauthorized {
		t.Fatalf("a list with no credential answered %v, not the double's 401", err)
	}
}

// testEmulatorSendsNoCredential: in emulator mode no request carries an Authorization header, no
// metadata request is sent, and the addresses are signed as EmulatorEmail.
func testEmulatorSendsNoCredential(t *testing.T, mode bucketMode) {
	rig := openRig(t, mode)
	ctx := testwait.Context(t)
	content := randomContent(t, 16<<10)
	name := packObject(content, "pack")
	if err := rig.bucket.put(ctx, name, writeLocal(t, content)); err != nil {
		t.Fatalf("put in emulator mode failed: %v", err)
	}
	address, err := rig.bucket.sign(ctx, name)
	if err != nil {
		t.Fatalf("sign in emulator mode failed: %v", err)
	}
	if status, _ := rig.download(t, address); status != http.StatusOK {
		t.Fatalf("the signed download answered %d", status)
	}
	if err := rig.bucket.delete(ctx, name, rig.list(t)[name].Generation); err != nil {
		t.Fatalf("delete in emulator mode failed: %v", err)
	}
	for _, request := range rig.double.Requests() {
		if request.Route == fakegcs.RouteMetadataEmail || request.Route == fakegcs.RouteMetadataToken {
			t.Errorf("emulator mode asked the metadata server %s", request.URL.Path)
		}
	}
	for _, route := range []fakegcs.Route{fakegcs.RouteList, fakegcs.RouteGet, fakegcs.RouteUpload, fakegcs.RouteDelete, fakegcs.RouteSignBlob, fakegcs.RouteDownload} {
		if got := rig.double.Count(route); got.Requests == 0 || got.WithAuthorization != 0 {
			t.Errorf("emulator mode sent %d %s requests, %d with an Authorization header; want some, none with one", got.Requests, route, got.WithAuthorization)
		}
	}
}

// testEmulatorHostIsLoopback: a STORAGE_EMULATOR_HOST that is not a loopback address or localhost
// with a port is refused, naming the value, and no request reaches the double; a loopback
// host:port of each form opens a bucket, also with no request.
func testEmulatorHostIsLoopback(t *testing.T, mode bucketMode) {
	d := mode.setup(t, &bucketClock{})
	ctx := testwait.Context(t)
	for _, value := range []string{
		"storage.googleapis.com:443",
		"localhost.example.com:4443",
		"10.0.0.8:4443",
		"0.0.0.0:4443",
		"[fe80::1]:4443",
		"[::1%lo]:4443",
		"http://127.0.0.1:4443",
		"127.0.0.1",
		"localhost",
		"127.0.0.1:0",
		"127.0.0.1:65536",
	} {
		t.Setenv(emulatorHostEnv, value)
		if b, err := openBucket(ctx, testBucket, testRepo); b != nil || err == nil || !strings.Contains(err.Error(), value) {
			t.Errorf("STORAGE_EMULATOR_HOST=%s opened %v, %v, not a refusal naming it", value, b, err)
		}
	}
	_, port, _ := strings.Cut(d.running.Addr(), ":")
	for _, value := range []string{d.running.Addr(), "localhost:" + port, "127.0.0.1:" + port, "[::1]:" + port} {
		t.Setenv(emulatorHostEnv, value)
		b, err := openBucket(ctx, testBucket, testRepo)
		if err != nil {
			t.Errorf("STORAGE_EMULATOR_HOST=%s was refused: %v", value, err)
			continue
		}
		_ = b.close()
	}
	if requests := d.double.Requests(); len(requests) != 0 {
		t.Fatalf("opening buckets in emulator mode sent %d requests: %v", len(requests), requests)
	}
}

// bucketRig is one mode's bucket open against its own double, with the double's clock.
type bucketRig struct {
	bucketDouble
	bucket *bucket
	clock  *bucketClock
}

// opened is what a bucket's opener answered.
type opened struct {
	bucket *bucket
	err    error
}

// openRig makes mode's double and opens the test bucket against it, as the cache would, closing
// it when the test ends. The open runs apart from the test, which waits for it with
// testwait.Receive, so that an open stuck reading the fifo GOOGLE_APPLICATION_CREDENTIALS names in
// production fails by name.
func openRig(t *testing.T, mode bucketMode) *bucketRig {
	t.Helper()
	clock := &bucketClock{}
	d := mode.setup(t, clock)
	answer := make(chan opened, 1)
	go func() {
		b, err := d.open(testwait.Context(t), testBucket)
		answer <- opened{b, err}
	}()
	got := testwait.Receive(t, "the bucket to open", answer)
	if got.err != nil {
		t.Fatalf("open bucket %s in %s mode: %v", testBucket, mode.name, got.err)
	}
	t.Cleanup(func() { _ = got.bucket.close() })
	return &bucketRig{bucketDouble: d, bucket: got.bucket, clock: clock}
}

// startDouble makes a double over a directory of the test's, serves it on a loopback address of
// its own (testnats.ListenLoopback, the module's listener for a fake a client dials), and stops it
// when the test ends.
func startDouble(t *testing.T, opts fakegcs.Options) (*fakegcs.Server, *fakegcs.Running) {
	t.Helper()
	opts.Root = t.TempDir()
	double, err := fakegcs.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := testnats.ListenLoopback()
	if err != nil {
		t.Fatalf("listen for the double: %v", err)
	}
	running := double.Serve(listener)
	t.Cleanup(func() { _ = running.Close() })
	return double, running
}

// unreadableCredentials is a credential file no code may read, for GOOGLE_APPLICATION_CREDENTIALS
// to name: a fifo no process writes. A plain open of it for reading, the open os.ReadFile and every
// credential discovery makes, waits for a writer for ever, so a bucket that looked for
// application default credentials would never open; and once the test ends, requireUnread finds
// nothing of the process holding it or waiting on it.
func unreadableCredentials(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application-default-credentials.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("make the fifo GOOGLE_APPLICATION_CREDENTIALS names: %v", err)
	}
	t.Cleanup(func() { requireUnread(t, path) })
	return path
}

// requireUnread fails the test where any code of the process holds the fifo at path open for
// reading, or waits in an open of it: an open of a fifo for writing that does not wait fails with
// ENXIO only while it has no reader, and a plain open for reading counts as one while it waits.
func requireUnread(t *testing.T, path string) {
	t.Helper()
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err == nil {
		_ = syscall.Close(fd)
		t.Errorf("code of the process opened %s, the credential file GOOGLE_APPLICATION_CREDENTIALS names, for reading", path)
		return
	}
	if !errors.Is(err, syscall.ENXIO) {
		t.Errorf("look for a reader of %s: %v", path, err)
	}
}

// bucketClock is the double's clock: the real time until a test sets one, which the double's check
// of a signed download's expiry then reads.
type bucketClock struct{ at atomic.Pointer[time.Time] }

// Now is the time last set, or the real time before any.
func (c *bucketClock) Now() time.Time {
	if at := c.at.Load(); at != nil {
		return *at
	}
	return time.Now()
}

// Set makes Now answer at.
func (c *bucketClock) Set(at time.Time) { c.at.Store(&at) }

// seed makes the double hold content under name with metadata, as an upload that is not the
// cache's would, and answers its generation.
func (r *bucketRig) seed(t *testing.T, name string, content []byte, metadata map[string]string) int64 {
	t.Helper()
	generation, err := r.double.Put(testBucket, name, bytes.NewReader(content), metadata)
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

// list is the bucket's list, failing the test where it fails.
func (r *bucketRig) list(t *testing.T) map[string]storedObject {
	t.Helper()
	listed, _, err := r.bucket.list(testwait.Context(t))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return listed
}

// bucketResource is what the double's JSON API answers of an object, in the fields the tests
// compare: Cloud Storage writes the generation and the size as strings, and the checksums in
// base64.
type bucketResource struct {
	Name       string            `json:"name"`
	Generation string            `json:"generation"`
	Size       string            `json:"size"`
	CRC32C     string            `json:"crc32c"`
	MD5Hash    string            `json:"md5Hash"`
	Metadata   map[string]string `json:"metadata"`
}

// resource reads name's object from the double's JSON API, apart from the code under test, with
// the metadata server's token, and answers the status and, where it is 200, the resource.
func (r *bucketRig) resource(t *testing.T, name string) (int, bucketResource) {
	t.Helper()
	target := r.running.URL() + "/storage/v1/b/" + testBucket + "/o/" + url.PathEscape(name)
	request, err := http.NewRequestWithContext(testwait.Context(t), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+r.double.Token())
	status, body := do(t, request)
	var res bucketResource
	if status == http.StatusOK {
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("the double's resource of %s does not parse: %v", name, err)
		}
	}
	return status, res
}

// download GETs a signed address with no credential, as an issue pod's fetch does, from the
// double. A production address names Cloud Storage's own host over HTTPS, so its request goes to
// the double with that Host, the host the double checks its signature over.
func (r *bucketRig) download(t *testing.T, address string) (int, []byte) {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatalf("parse the signed address: %v", err)
	}
	host := u.Host
	u.Scheme, u.Host = "http", r.running.Addr()
	request, err := http.NewRequestWithContext(testwait.Context(t), http.MethodGet, u.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = host
	return do(t, request)
}

// do sends request and reads its whole answer, failing the test where no answer came.
func do(t *testing.T, request *http.Request) (int, []byte) {
	t.Helper()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", request.Method, request.URL.Path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer to %s %s: %v", request.Method, request.URL.Path, err)
	}
	return response.StatusCode, body
}

// requestName is the object name a Storage request named, or for a list the prefix it asked for,
// and false for a request that names neither: a list's prefix parameter, a metadata read's or a
// delete's object path segment, an upload's name parameter.
func requestName(t *testing.T, request fakegcs.Request) (string, bool) {
	t.Helper()
	switch request.Route {
	case fakegcs.RouteList:
		return request.URL.Query().Get("prefix"), true
	case fakegcs.RouteUpload:
		return request.URL.Query().Get("name"), true
	case fakegcs.RouteGet, fakegcs.RouteDelete:
		_, escaped, ok := strings.Cut(request.URL.EscapedPath(), "/o/")
		name, err := url.PathUnescape(escaped)
		if !ok || err != nil {
			t.Fatalf("a %s request's path %q names no object", request.Route, request.URL.EscapedPath())
		}
		return name, true
	}
	return "", false
}

// requireHolds fails the test unless res is the resource of an object holding content: its size,
// CRC32C and MD5.
func requireHolds(t *testing.T, res bucketResource, content []byte) {
	t.Helper()
	md5Sum := md5.Sum(content)
	crc := base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, crc32.Checksum(content, castagnoli)))
	if res.Size != strconv.Itoa(len(content)) || res.CRC32C != crc || res.MD5Hash != base64.StdEncoding.EncodeToString(md5Sum[:]) {
		t.Fatalf("%s holds %s bytes, CRC32C %s and MD5 %s, not the %d bytes expected", res.Name, res.Size, res.CRC32C, res.MD5Hash, len(content))
	}
}

// writeLocal writes content to a file of the test's and answers it as put takes it
// (localObjectOf), failing the test unless localObjectOf read content's own size, SHA-256 and
// CRC32C.
func writeLocal(t *testing.T, content []byte) localObject {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pack")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	local, err := localObjectOf(path)
	if err != nil {
		t.Fatal(err)
	}
	if local.Size != int64(len(content)) || local.SHA256 != sha256Of(content) || local.CRC32C != crc32.Checksum(content, castagnoli) {
		t.Fatalf("localObjectOf read %+v, not the file's size, SHA-256 and CRC32C", local)
	}
	return local
}

// packObject is the test repository's object name for content as a pack or an index (ext): a
// SHA-1-shaped name drawn from content, which a pack's name is.
func packObject(content []byte, ext string) string {
	sum := sha1.Sum(content)
	return testPrefix + "pack-" + hex.EncodeToString(sum[:]) + "." + ext
}

// sha256Of is content's SHA-256 in hex, the sha256 metadata the cache sets on each object.
func sha256Of(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// randomContent is n random bytes, the content of a pack whose bytes a test compares.
func randomContent(t *testing.T, n int) []byte {
	t.Helper()
	content := make([]byte, n)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	return content
}
