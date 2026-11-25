package fakegcs

import (
	"bytes"
	"crypto"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"

	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// testBucket is the bucket the tests' objects live in: the placeholder bucket the fixtures name.
const testBucket = "example-legion-packs"

// packPrefix is the repository prefix the tests' packs live under, the fixtures' placeholder one.
const packPrefix = "acme/widgets/objects/pack/"

// TestCloudStorageResponseFixtures drives the double through real HTTP and holds each of its
// answers to the status and the shape (keys, nesting, JSON types; no values) of the answer Google
// gave the same request, as testdata holds it (testdata/README.md says how each was captured): a
// list's first and last pages, an object's metadata, an absent object, a generation precondition
// that does not hold, the metadata server's email and token, and signBlob's answer and refusal. It
// holds the double's own behaviour beside them: a raw GET with no signature refused, uploads of
// both kinds visible only once complete and checked, a held upload, deletes, the fault switches, the
// metadata server's token required where Options.RequireToken says so, and the pinned Storage
// client's own uploads, reads, lists and deletes.
func TestCloudStorageResponseFixtures(t *testing.T) {
	double, running := startDouble(t, Options{})
	base := running.URL()
	objects := base + "/storage/v1/b/" + testBucket + "/o"
	names := []string{
		packPrefix + "pack-0000000000000000000000000000000000000001.idx",
		packPrefix + "pack-0000000000000000000000000000000000000001.pack",
		packPrefix + "pack-0000000000000000000000000000000000000002.pack",
	}
	generations := map[string]int64{}
	for _, name := range names {
		generations[name] = put(t, double, name, []byte("the bytes of "+name))
	}
	put(t, double, "acme/gadgets/objects/pack/pack-0000000000000000000000000000000000000003.pack", []byte("another repository's pack"))

	t.Run("list first page", func(t *testing.T) {
		status, _, body := call(t, http.MethodGet, objects+"?"+url.Values{"prefix": {packPrefix}, "maxResults": {"1"}}.Encode(), nil, nil)
		requireFixtureShape(t, "list-next.json", status, http.StatusOK, body, objectExtra("items", "[]")...)
		page := decode[objectList](t, body)
		if len(page.Items) != 1 || page.Items[0].Name != names[0] || page.NextPageToken == "" {
			t.Fatalf("the first page of %d names holds %+v with nextPageToken %q, not %s and a token", len(names), page.Items, page.NextPageToken, names[0])
		}
	})

	t.Run("list last page", func(t *testing.T) {
		var listed []string
		token := ""
		for pages := 1; ; pages++ {
			query := url.Values{"prefix": {packPrefix}, "maxResults": {"2"}}
			if token != "" {
				query.Set("pageToken", token)
			}
			status, _, body := call(t, http.MethodGet, objects+"?"+query.Encode(), nil, nil)
			page := decode[objectList](t, body)
			for _, item := range page.Items {
				listed = append(listed, item.Name)
			}
			if page.NextPageToken == "" {
				requireFixtureShape(t, "list-final.json", status, http.StatusOK, body, objectExtra("items", "[]")...)
				break
			}
			requireFixtureShape(t, "list-next.json", status, http.StatusOK, body, objectExtra("items", "[]")...)
			if pages > len(names) {
				t.Fatalf("a list of %d names under %s at 2 a page still has a next page after %d pages", len(names), packPrefix, pages)
			}
			token = page.NextPageToken
		}
		if !slices.Equal(listed, names) {
			t.Fatalf("the pages under %s listed %q, not %q in name order and no other repository's", packPrefix, listed, names)
		}
		status, _, body := call(t, http.MethodGet, objects+"?"+url.Values{"prefix": {packPrefix}}.Encode(), nil, nil)
		requireFixtureShape(t, "list-final.json", status, http.StatusOK, body, objectExtra("items", "[]")...)
		if got := decode[objectList](t, body).Items; len(got) != len(names) {
			t.Fatalf("a list with no maxResults holds %d objects, not the %d under %s", len(got), len(names), packPrefix)
		}
	})

	t.Run("object", func(t *testing.T) {
		name := names[1]
		status, _, body := call(t, http.MethodGet, objects+"/"+url.PathEscape(name), nil, nil)
		requireFixtureShape(t, "object.json", status, http.StatusOK, body, objectExtra()...)
		content := []byte("the bytes of " + name)
		md5Sum := md5.Sum(content)
		want := objectResource{
			Kind: "storage#object", Name: name, Bucket: testBucket, Generation: strconv.FormatInt(generations[name], 10), Metageneration: "1",
			ContentType: defaultContentType, StorageClass: "STANDARD", Size: strconv.Itoa(len(content)),
			MD5Hash: base64.StdEncoding.EncodeToString(md5Sum[:]), CRC32C: encodeCRC32C(crc32.Checksum(content, crc32cTable)),
		}
		got := decode[objectResource](t, body)
		want.ID, want.Etag, want.TimeCreated, want.Updated = got.ID, got.Etag, got.TimeCreated, got.Updated
		if !reflect.DeepEqual(got, want) || got.ID != testBucket+"/"+name+"/"+want.Generation {
			t.Fatalf("the object's resource is %+v, not %+v with id <bucket>/<name>/<generation>", got, want)
		}
	})

	t.Run("absent object", func(t *testing.T) {
		status, _, body := call(t, http.MethodGet, objects+"/"+url.PathEscape(packPrefix+"pack-absent.pack"), nil, nil)
		requireFixtureShape(t, "object-not-found.json", status, http.StatusNotFound, body)
	})

	t.Run("generation precondition", func(t *testing.T) {
		status, _, body := call(t, http.MethodGet, objects+"/"+url.PathEscape(names[1])+"?ifGenerationMatch=1", nil, nil)
		requireFixtureShape(t, "object-precondition.json", status, http.StatusPreconditionFailed, body)
		status, _, body = call(t, http.MethodGet, objects+"/"+url.PathEscape(names[1])+"?ifGenerationMatch="+strconv.FormatInt(generations[names[1]], 10), nil, nil)
		if status != http.StatusOK {
			t.Fatalf("a read whose ifGenerationMatch holds answered %d: %s", status, body)
		}
	})

	t.Run("metadata email", func(t *testing.T) {
		status, header, body := call(t, http.MethodGet, base+metadataPrefix+"instance/service-accounts/default/email", http.Header{"Metadata-Flavor": {"Google"}}, nil)
		// DefaultEmail is the fixture's placeholder address, so the whole answer is the fixture's.
		if want := fixture(t, "metadata-email.txt"); status != http.StatusOK || !bytes.Equal(body, want) || bytes.ContainsAny(want, "\r\n") {
			t.Fatalf("the metadata server's email answered %d %q, not 200 and testdata/metadata-email.txt's %q, one line with no line feed", status, body, want)
		}
		if header.Get("Metadata-Flavor") != "Google" {
			t.Fatalf("the metadata server's answer carries Metadata-Flavor %q, not Google", header.Get("Metadata-Flavor"))
		}
		if status, _, _ := call(t, http.MethodGet, base+metadataPrefix+"instance/service-accounts/default/email", nil, nil); status != http.StatusForbidden {
			t.Fatalf("the metadata server answered a request without Metadata-Flavor: Google with %d, not 403", status)
		}
	})

	t.Run("metadata token", func(t *testing.T) {
		status, _, body := call(t, http.MethodGet, base+metadataPrefix+"instance/service-accounts/default/token", http.Header{"Metadata-Flavor": {"Google"}}, nil)
		requireFixtureShape(t, "metadata-token.json", status, http.StatusOK, body)
		if got := decode[tokenAnswer](t, body); got.AccessToken != double.Token() || got.TokenType != "Bearer" || got.ExpiresIn < 1 {
			t.Fatalf("the metadata server's token is %+v, not the double's token %q, of type Bearer, for a time", got, double.Token())
		}
	})

	t.Run("signBlob", func(t *testing.T) {
		payload := []byte("GOOG4-RSA-SHA256\n20261125T000000Z\n20261125/auto/storage/goog4_request\n00")
		status, _, body := signBlobCall(t, base, double.Email(), "", payload)
		requireFixtureShape(t, "iam-sign-200.json", status, http.StatusOK, body)
		got := decode[signBlobResponse](t, body)
		signature, err := base64.StdEncoding.DecodeString(got.SignedBlob)
		digest := sha256.Sum256(payload)
		if err != nil || rsa.VerifyPKCS1v15(&double.key.PublicKey, crypto.SHA256, digest[:], signature) != nil {
			t.Fatalf("signBlob's signedBlob %q is no RSA-SHA256 PKCS #1 v1.5 signature of the payload by the double's key (%v)", got.SignedBlob, err)
		}
		if len(got.KeyID) != len(decode[signBlobResponse](t, fixture(t, "iam-sign-200.json")).KeyID) {
			t.Fatalf("signBlob's keyId %q is not 40 hexadecimal digits, as the documented answer's is", got.KeyID)
		}
	})

	t.Run("signBlob refused", func(t *testing.T) {
		double.RefuseSigning(true)
		status, _, body := signBlobCall(t, base, double.Email(), "", []byte("payload"))
		double.RefuseSigning(false)
		requireFixtureShape(t, "iam-sign-403.json", status, http.StatusForbidden, body)
		status, _, body = signBlobCall(t, base, "someone-else@example-project.iam.gserviceaccount.com", "", []byte("payload"))
		requireFixtureShape(t, "iam-sign-403.json", status, http.StatusForbidden, body)
	})

	t.Run("raw unsigned GET", func(t *testing.T) {
		before := double.Count(RouteDownload).Requests
		status, _, body := call(t, http.MethodGet, base+"/"+testBucket+"/"+names[1], nil, nil)
		if status != http.StatusForbidden || refusalCode(t, body) != "AccessDenied" || bytes.Contains(body, []byte("the bytes of")) {
			t.Fatalf("a GET of an object with no signature answered %d %s, not 403 AccessDenied", status, body)
		}
		if got := double.Count(RouteDownload).Requests - before; got != 1 {
			t.Fatalf("the double counted %d downloads for one, not 1", got)
		}
	})

	t.Run("multipart upload", func(t *testing.T) {
		name := packPrefix + "pack-multipart.pack"
		content := []byte("a multipart upload's bytes")
		resource := map[string]any{"name": name, "metadata": map[string]string{"sha256": sha256Hex(content)}, "crc32c": encodeCRC32C(crc32.Checksum(content, crc32cTable))}
		createOnly := url.Values{"uploadType": {"multipart"}, "ifGenerationMatch": {"0"}}
		status, _, body := callPayload(t, http.MethodPost, base+"/upload/storage/v1/b/"+testBucket+"/o?"+createOnly.Encode(), multipartBody(t, resource, content))
		requireFixtureShape(t, "object.json", status, http.StatusOK, body, objectExtra()...)
		requireResource(t, decode[objectResource](t, body), name, content, map[string]string{"sha256": sha256Hex(content)})
		status, _, body = callPayload(t, http.MethodPost, base+"/upload/storage/v1/b/"+testBucket+"/o?"+createOnly.Encode(), multipartBody(t, map[string]any{"name": name}, []byte("other bytes")))
		requireFixtureShape(t, "object-precondition.json", status, http.StatusPreconditionFailed, body)
		_, _, body = call(t, http.MethodGet, objects+"/"+url.PathEscape(name), nil, nil)
		requireResource(t, decode[objectResource](t, body), name, content, map[string]string{"sha256": sha256Hex(content)})

		misstated := packPrefix + "pack-misstated.pack"
		resource = map[string]any{"name": misstated, "crc32c": encodeCRC32C(crc32.Checksum([]byte("other bytes"), crc32cTable))}
		status, _, body = callPayload(t, http.MethodPost, base+"/upload/storage/v1/b/"+testBucket+"/o?uploadType=multipart", multipartBody(t, resource, content))
		if status != http.StatusBadRequest || !bytes.Contains(body, []byte("doesn't match calculated CRC32C")) {
			t.Fatalf("an upload whose stated CRC32C is another's answered %d %s, not 400", status, body)
		}
		requireAbsent(t, objects, misstated)
	})

	t.Run("resumable upload", func(t *testing.T) {
		name := packPrefix + "pack-resumable.pack"
		content := randomBytes(t, 700<<10)
		chunk := int64(256 << 10)
		location := startResumable(t, base, name, map[string]any{"metadata": map[string]string{"sha256": sha256Hex(content)}, "contentType": "application/x-git-packed-objects"})
		status, header, body := call(t, http.MethodPut, location, http.Header{"Content-Range": {fmt.Sprintf("bytes 0-%d/*", chunk-1)}}, bytes.NewReader(content[:chunk]))
		if want := fmt.Sprintf("bytes=0-%d", chunk-1); status != http.StatusPermanentRedirect || header.Get("Range") != want {
			t.Fatalf("a first chunk answered %d Range %q, not 308 with Range %s: %s", status, header.Get("Range"), want, body)
		}
		requireAbsent(t, objects, name)
		status, header, _ = call(t, http.MethodPut, location, http.Header{"Content-Range": {"bytes */*"}, "X-Guploader-No-308": {"yes"}}, nil)
		if status != http.StatusOK || header.Get("X-Http-Status-Code-Override") != "308" || header.Get("Range") != fmt.Sprintf("bytes=0-%d", chunk-1) {
			t.Fatalf("a status query with X-GUploader-No-308 answered %d, override %q, Range %q, not 200, 308 and the first chunk", status, header.Get("X-Http-Status-Code-Override"), header.Get("Range"))
		}
		// A chunk sent again, as a client sends one it heard no answer to, takes nothing twice.
		status, _, _ = call(t, http.MethodPut, location, http.Header{"Content-Range": {fmt.Sprintf("bytes 0-%d/*", 2*chunk-1)}}, bytes.NewReader(content[:2*chunk]))
		if status != http.StatusPermanentRedirect {
			t.Fatalf("a chunk overlapping the bytes held answered %d, not 308", status)
		}
		last := http.Header{
			"Content-Range": {fmt.Sprintf("bytes %d-%d/%d", 2*chunk, len(content)-1, len(content))},
			"X-Goog-Hash":   {"crc32c=" + encodeCRC32C(crc32.Checksum(content, crc32cTable))},
		}
		status, _, body = call(t, http.MethodPost, location, last, bytes.NewReader(content[2*chunk:]))
		requireFixtureShape(t, "object.json", status, http.StatusOK, body, objectExtra()...)
		got := decode[objectResource](t, body)
		requireResource(t, got, name, content, map[string]string{"sha256": sha256Hex(content)})
		if got.ContentType != "application/x-git-packed-objects" {
			t.Fatalf("the object's content type is %q, not the one its upload's resource named", got.ContentType)
		}
		status, _, body = call(t, http.MethodGet, objects+"/"+url.PathEscape(name), nil, nil)
		if status != http.StatusOK || decode[objectResource](t, body).Generation != got.Generation {
			t.Fatalf("the completed upload's object reads %d %s, not its generation %s", status, body, got.Generation)
		}
		status, _, body = call(t, http.MethodPut, location, http.Header{"Content-Range": {fmt.Sprintf("bytes */%d", len(content))}}, nil)
		if status != http.StatusOK || decode[objectResource](t, body).Generation != got.Generation {
			t.Fatalf("a completed session's status query answered %d %s, not its object", status, body)
		}

		misstated := packPrefix + "pack-resumable-misstated.pack"
		location = startResumable(t, base, misstated, nil)
		wrong := http.Header{"Content-Range": {fmt.Sprintf("bytes 0-%d/%d", len(content)-1, len(content))}, "X-Goog-Hash": {"crc32c=AAAAAA=="}}
		status, _, body = call(t, http.MethodPut, location, wrong, bytes.NewReader(content))
		if status != http.StatusBadRequest || !bytes.Contains(body, []byte("doesn't match calculated CRC32C")) {
			t.Fatalf("a last chunk whose X-Goog-Hash states another CRC32C answered %d %s, not 400", status, body)
		}
		requireAbsent(t, objects, misstated)
	})

	t.Run("held upload", func(t *testing.T) {
		name := packPrefix + "pack-held.pack"
		old := put(t, double, name, []byte("the old bytes"))
		hold := double.HoldNext(func(bucket, object string) bool { return bucket == testBucket && object == name })
		content := []byte("the new bytes")
		req := request(t, http.MethodPost, base+"/upload/storage/v1/b/"+testBucket+"/o?uploadType=multipart&ifGenerationMatch="+strconv.FormatInt(old, 10),
			multipartBody(t, map[string]any{"name": name}, content))
		answered := make(chan answer, 1)
		go func() { answered <- send(req) }()
		testwait.Receive(t, "the upload stops at its hold", hold.Reached())
		status, _, body := call(t, http.MethodGet, objects+"/"+url.PathEscape(name), nil, nil)
		if got := decode[objectResource](t, body); status != http.StatusOK || got.Generation != strconv.FormatInt(old, 10) || got.Size != strconv.Itoa(len("the old bytes")) {
			t.Fatalf("while the upload is held its object reads %d %+v, not the old generation %d", status, got, old)
		}
		_, _, body = call(t, http.MethodGet, objects+"?prefix="+url.QueryEscape(name), nil, nil)
		if items := decode[objectList](t, body).Items; len(items) != 1 || items[0].Generation != strconv.FormatInt(old, 10) {
			t.Fatalf("while the upload is held a list shows %+v, not the old generation %d alone", items, old)
		}
		hold.Release()
		got := testwait.Receive(t, "the held upload's answer", answered)
		if got.err != nil || got.status != http.StatusOK {
			t.Fatalf("the released upload answered %d %s (%v), not 200", got.status, got.body, got.err)
		}
		requireResource(t, decode[objectResource](t, got.body), name, content, nil)
	})

	t.Run("delete", func(t *testing.T) {
		name := packPrefix + "pack-deleted.pack"
		generation := put(t, double, name, []byte("bytes to delete"))
		before := double.Count(RouteDelete).Requests
		target := objects + "/" + url.PathEscape(name) + "?ifGenerationMatch="
		status, _, body := call(t, http.MethodDelete, target+strconv.FormatInt(generation+1, 10), nil, nil)
		requireFixtureShape(t, "object-precondition.json", status, http.StatusPreconditionFailed, body)
		if status, _, body := call(t, http.MethodDelete, target+strconv.FormatInt(generation, 10), nil, nil); status != http.StatusNoContent || len(body) != 0 {
			t.Fatalf("a delete whose ifGenerationMatch holds answered %d %q, not 204 with no body", status, body)
		}
		status, _, body = call(t, http.MethodDelete, target+strconv.FormatInt(generation, 10), nil, nil)
		requireFixtureShape(t, "object-not-found.json", status, http.StatusNotFound, body)
		requireAbsent(t, objects, name)
		if got := double.Count(RouteDelete).Requests - before; got != 3 {
			t.Fatalf("the double counted %d deletes for 3", got)
		}
	})

	t.Run("fault switches", func(t *testing.T) {
		double.FailUpload(2, http.StatusServiceUnavailable)
		for i, want := range []int{http.StatusOK, http.StatusServiceUnavailable, http.StatusOK} {
			name := fmt.Sprintf("%spack-fault-%d.pack", packPrefix, i)
			status, _, body := callPayload(t, http.MethodPost, base+"/upload/storage/v1/b/"+testBucket+"/o?uploadType=multipart", multipartBody(t, map[string]any{"name": name}, []byte(name)))
			if status != want {
				t.Fatalf("upload %d after FailUpload(2, 503) answered %d, not %d: %s", i+1, status, want, body)
			}
			if want != http.StatusOK {
				requireAbsent(t, objects, name)
			}
		}
		name := packPrefix + "pack-delete-fault.pack"
		put(t, double, name, []byte(name))
		double.FailDelete(1, http.StatusServiceUnavailable)
		if status, _, _ := call(t, http.MethodDelete, objects+"/"+url.PathEscape(name), nil, nil); status != http.StatusServiceUnavailable {
			t.Fatalf("the delete after FailDelete(1, 503) answered %d, not 503", status)
		}
		if status, _, _ := call(t, http.MethodGet, objects+"/"+url.PathEscape(name), nil, nil); status != http.StatusOK {
			t.Fatalf("an object whose delete failed reads %d, not 200", status)
		}
		if status, _, _ := call(t, http.MethodDelete, objects+"/"+url.PathEscape(name), nil, nil); status != http.StatusNoContent {
			t.Fatalf("the delete after the failed one answered %d, not 204", status)
		}
	})

	t.Run("metadata token required", func(t *testing.T) {
		guarded, guardedRunning := startDouble(t, Options{RequireToken: true})
		list := guardedRunning.URL() + "/storage/v1/b/" + testBucket + "/o"
		for _, header := range []http.Header{nil, {"Authorization": {"Bearer not-issued"}}} {
			if status, _, body := call(t, http.MethodGet, list, header, nil); status != http.StatusUnauthorized || decode[storageError](t, body).Error.Code != http.StatusUnauthorized {
				t.Fatalf("a list with Authorization %q answered %d %s, not 401", header.Get("Authorization"), status, body)
			}
		}
		if status, _, _ := signBlobCall(t, guardedRunning.URL(), guarded.Email(), "", []byte("payload")); status != http.StatusUnauthorized {
			t.Fatalf("a signBlob with no token answered %d, not 401", status)
		}
		token := metadataToken(t, guardedRunning.URL())
		if status, _, body := call(t, http.MethodGet, list, http.Header{"Authorization": {"Bearer " + token}}, nil); status != http.StatusOK {
			t.Fatalf("a list with the metadata server's token answered %d %s, not 200", status, body)
		}
		if status, _, body := signBlobCall(t, guardedRunning.URL(), guarded.Email(), token, []byte("payload")); status != http.StatusOK {
			t.Fatalf("a signBlob with the metadata server's token answered %d %s, not 200", status, body)
		}
		if got, want := guarded.Count(RouteList), (Count{Requests: 3, WithAuthorization: 2}); got != want {
			t.Fatalf("the double counted %+v lists, not %+v", got, want)
		}
	})

	t.Run("pinned Storage client", func(t *testing.T) {
		t.Setenv("STORAGE_EMULATOR_HOST", running.Addr())
		ctx := testwait.Context(t)
		client, err := storage.NewClient(ctx)
		if err != nil {
			t.Fatalf("make a Storage client for the double: %v", err)
		}
		defer client.Close()
		bucket := client.Bucket(testBucket)
		prefix := "acme/client/objects/pack/"
		write := func(name string, content []byte, crc *uint32, metadata map[string]string) (*storage.ObjectAttrs, error) {
			w := bucket.Object(name).If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
			w.ChunkSize = 256 << 10
			w.ContentType = defaultContentType
			w.Metadata = metadata
			if crc != nil {
				w.CRC32C, w.SendCRC32C = *crc, true
			}
			if _, err := w.Write(content); err != nil {
				_ = w.Close()
				return nil, err
			}
			if err := w.Close(); err != nil {
				return nil, err
			}
			return w.Attrs(), nil
		}
		index := []byte("a small index, sent in one multipart request")
		indexCRC := crc32.Checksum(index, crc32cTable)
		if _, err := write(prefix+"pack-a.idx", index, &indexCRC, map[string]string{"sha256": sha256Hex(index)}); err != nil {
			t.Fatalf("the client's multipart upload with a stated CRC32C failed: %v", err)
		}
		chunks := double.Count(RouteUploadChunk).Requests
		pack := randomBytes(t, 600<<10)
		attrs, err := write(prefix+"pack-b.pack", pack, nil, nil)
		if err != nil {
			t.Fatalf("the client's resumable upload, with the checksum it computes itself, failed: %v", err)
		}
		if got := double.Count(RouteUploadChunk).Requests - chunks; got < 3 || attrs.Size != int64(len(pack)) || attrs.CRC32C != crc32.Checksum(pack, crc32cTable) {
			t.Fatalf("the client's resumable upload of %d bytes in 256 KiB chunks sent %d chunk requests and made %+v", len(pack), got, attrs)
		}
		if _, err := write(prefix+"pack-a.idx", []byte("other bytes"), nil, nil); googleCode(err) != http.StatusPreconditionFailed {
			t.Fatalf("a create-only upload over an object answered %v, not 412", err)
		}
		wrongCRC := indexCRC + 1
		if _, err := write(prefix+"pack-c.pack", index, &wrongCRC, nil); googleCode(err) != http.StatusBadRequest {
			t.Fatalf("an upload whose stated CRC32C is not its bytes' answered %v, not 400", err)
		}
		if _, err := bucket.Object(prefix + "pack-c.pack").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
			t.Fatalf("an upload refused for its CRC32C left an object: %v", err)
		}
		got, err := bucket.Object(prefix + "pack-a.idx").Attrs(ctx)
		md5Sum := md5.Sum(index)
		if err != nil || got.Size != int64(len(index)) || got.CRC32C != indexCRC || !bytes.Equal(got.MD5, md5Sum[:]) || got.Metadata["sha256"] != sha256Hex(index) {
			t.Fatalf("the client read the index's attributes as %+v (%v), not its size, CRC32C, MD5 and sha256 metadata", got, err)
		}
		it := bucket.Objects(ctx, &storage.Query{Prefix: prefix})
		it.PageInfo().MaxSize = 1
		var listed []string
		for {
			o, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatalf("the client's list of %s failed: %v", prefix, err)
			}
			listed = append(listed, o.Name)
		}
		if want := []string{prefix + "pack-a.idx", prefix + "pack-b.pack"}; !slices.Equal(listed, want) {
			t.Fatalf("the client listed %q a page of one at a time, not %q", listed, want)
		}
		object := bucket.Object(prefix + "pack-b.pack")
		if err := object.If(storage.Conditions{GenerationMatch: attrs.Generation + 1}).Delete(ctx); googleCode(err) != http.StatusPreconditionFailed {
			t.Fatalf("a delete for another generation answered %v, not 412", err)
		}
		if err := object.If(storage.Conditions{GenerationMatch: attrs.Generation}).Delete(ctx); err != nil {
			t.Fatalf("a delete for the object's generation failed: %v", err)
		}
		if _, err := object.Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
			t.Fatalf("the deleted object still reads: %v", err)
		}
	})
}

// TestCloudStorageV4Conformance holds the double's check of a signed download (Verifier) to Google's
// own published answers, so that the double cannot agree with a producer that has the scheme wrong:
// each row of the pinned Storage module's V4 conformance file that signs a path-style GET of an
// object over the host header alone, the "Simple GET" row first, with the inactive dummy key that
// module publishes beside it, read from the module's own directory (no copy of the key is kept
// here). For each, the canonical request and the string to sign the verifier builds are the row's,
// and a verifier at the row's time takes the row's address. Then it refuses the Simple GET address
// with one query value changed, a parameter added, the path or the host changed, other signed
// headers, a wrong scope, another signer, no signature, and at a clock past its expiry; it takes an
// address the pinned client signs with the dummy key now. Last, the double's own signer: an address
// storage.SignedURL makes through the double's signBlob downloads the object's exact bytes with no
// Authorization header, and altered, expired and foreign addresses are refused.
func TestCloudStorageV4Conformance(t *testing.T) {
	module := storageModuleDir(t)
	rows := readVectors(t, filepath.Join(module, "internal", "test", "conformance", "v4_signatures.json"))
	dummy := readDummyAccount(t, filepath.Join(module, "internal", "test", "conformance", "service-account"))
	known := Verifier{Signers: map[string]*rsa.PublicKey{dummy.email: &dummy.key.PublicKey}}
	simple, ok := rows["Simple GET"]
	if !ok {
		t.Fatal("the pinned Storage module's v4_signatures.json has no Simple GET row")
	}

	t.Run("known answer Simple GET", func(t *testing.T) { requireKnownAnswer(t, known, simple) })
	for _, description := range slices.Sorted(maps.Keys(rows)) {
		row := rows[description]
		if description == simple.Description || row.Method != http.MethodGet || len(row.Headers) > 0 || row.URLStyle != "" || row.Object == "" {
			continue
		}
		t.Run("known answer "+description, func(t *testing.T) { requireKnownAnswer(t, known, row) })
	}

	signedAt := mustTime(t, simple.Timestamp)
	at := func(when time.Time) Verifier {
		v := known
		v.Now = func() time.Time { return when }
		return v
	}
	for _, tc := range []struct {
		name     string
		verifier Verifier
		change   func(u *url.URL)
		code     string
	}{
		{name: "a changed query value is refused", verifier: at(signedAt), change: setQuery("X-Goog-Expires", "11"), code: "SignatureDoesNotMatch"},
		{name: "an added query parameter is refused", verifier: at(signedAt), change: setQuery("prefix", "other"), code: "SignatureDoesNotMatch"},
		{name: "a changed path is refused", verifier: at(signedAt), change: func(u *url.URL) { u.Path = "/test-bucket/other-object" }, code: "SignatureDoesNotMatch"},
		{name: "a changed host is refused", verifier: at(signedAt), change: func(u *url.URL) { u.Host = "storage.example.com" }, code: "SignatureDoesNotMatch"},
		{name: "other signed headers are refused", verifier: at(signedAt), change: setQuery("X-Goog-SignedHeaders", "host;x-goog-date"), code: "AccessDenied"},
		{name: "a scope of another location is refused", verifier: at(signedAt), change: setQuery("X-Goog-Credential", dummy.email+"/20190201/us-central1/storage/goog4_request"), code: "AccessDenied"},
		{name: "a scope of another day is refused", verifier: at(signedAt), change: setQuery("X-Goog-Credential", dummy.email+"/20190202/auto/storage/goog4_request"), code: "AccessDenied"},
		{name: "another algorithm is refused", verifier: at(signedAt), change: setQuery("X-Goog-Algorithm", "GOOG4-HMAC-SHA256"), code: "AccessDenied"},
		{name: "a life past seven days is refused", verifier: at(signedAt), change: setQuery("X-Goog-Expires", "604801"), code: "AccessDenied"},
		{name: "no signature is refused", verifier: at(signedAt), change: func(u *url.URL) { u.RawQuery = "" }, code: "AccessDenied"},
		{name: "an address at its expiry is refused", verifier: at(signedAt.Add(time.Duration(simple.Expiration) * time.Second)), code: "ExpiredToken"},
		{name: "an address past its expiry is refused", verifier: at(signedAt.Add(time.Duration(simple.Expiration+1) * time.Second)), code: "ExpiredToken"},
		{name: "an unknown signer is refused", verifier: Verifier{Signers: map[string]*rsa.PublicKey{"other@example-project.iam.gserviceaccount.com": &dummy.key.PublicKey}, Now: at(signedAt).Now}, code: "AccessDenied"},
		{name: "another key of the same account is refused", verifier: Verifier{Signers: map[string]*rsa.PublicKey{dummy.email: &newKey(t).PublicKey}, Now: at(signedAt).Now}, code: "SignatureDoesNotMatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := mustURL(t, simple.ExpectedURL)
			if tc.change != nil {
				tc.change(u)
			}
			requireRefused(t, tc.verifier, u, tc.code)
		})
	}

	t.Run("the pinned client's address with the dummy key is taken", func(t *testing.T) {
		signed, err := storage.SignedURL("test-bucket", "test/object name", &storage.SignedURLOptions{
			GoogleAccessID: dummy.email,
			PrivateKey:     dummy.pem,
			Method:         http.MethodGet,
			Expires:        time.Now().Add(15 * time.Minute),
			Scheme:         storage.SigningSchemeV4,
		})
		if err != nil {
			t.Fatalf("sign an address with the dummy key: %v", err)
		}
		u := mustURL(t, signed)
		bucket, object, err := known.Verify(http.MethodGet, u.Host, u)
		if err != nil || bucket != "test-bucket" || object != "test/object name" {
			t.Fatalf("the verifier answered %q %q (%v) for the pinned client's address, not test-bucket and its object", bucket, object, err)
		}
	})

	t.Run("the double's own signer", func(t *testing.T) {
		clock := &testClock{}
		clock.Set(time.Now())
		double, running := startDouble(t, Options{Now: clock.Now, RequireToken: true})
		token := metadataToken(t, running.URL())
		name := packPrefix + "pack-signed.pack"
		content := randomBytes(t, 3<<20+5)
		put(t, double, name, content)
		put(t, double, packPrefix+"pack-other.pack", []byte("another pack"))
		sign := func(t *testing.T) string {
			t.Helper()
			signed, err := storage.SignedURL(testBucket, name, &storage.SignedURLOptions{
				GoogleAccessID: double.Email(),
				SignBytes:      signBlobVia(running.URL(), double.Email(), token),
				Method:         http.MethodGet,
				Expires:        time.Now().Add(15 * time.Minute),
				Scheme:         storage.SigningSchemeV4,
				Hostname:       running.Addr(),
				Insecure:       true,
			})
			if err != nil {
				t.Fatalf("sign an address through the double's signBlob: %v", err)
			}
			return signed
		}
		signed := sign(t)

		t.Run("downloads the object's exact bytes", func(t *testing.T) {
			status, _, body := call(t, http.MethodGet, signed, nil, nil)
			if status != http.StatusOK || !bytes.Equal(body, content) {
				t.Fatalf("the signed download answered %d and %d bytes, not 200 and the object's %d bytes", status, len(body), len(content))
			}
			if got := double.Count(RouteDownload); got.Requests == 0 || got.WithAuthorization != 0 {
				t.Fatalf("the double counted %+v downloads: one or more, and none with an Authorization header", got)
			}
			if got := double.Count(RouteSignBlob); got.Requests == 0 || got.WithAuthorization != got.Requests {
				t.Fatalf("the double counted %+v signBlob requests: one or more, each with the metadata server's token", got)
			}
		})
		for _, tc := range []struct {
			name   string
			change func(u *url.URL)
			code   string
		}{
			{name: "a changed query value is refused", change: setQuery("X-Goog-Expires", "901"), code: "SignatureDoesNotMatch"},
			{name: "an added query parameter is refused", change: setQuery("generation", "1"), code: "SignatureDoesNotMatch"},
			{name: "a changed path is refused", change: func(u *url.URL) { u.Path = "/" + testBucket + "/" + packPrefix + "pack-other.pack" }, code: "SignatureDoesNotMatch"},
			{name: "no signature is refused", change: func(u *url.URL) { u.RawQuery = "" }, code: "AccessDenied"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				u := mustURL(t, signed)
				tc.change(u)
				requireDownloadRefused(t, u.String(), tc.code)
			})
		}
		t.Run("an expired address is refused", func(t *testing.T) {
			clock.Set(time.Now().Add(16 * time.Minute))
			requireDownloadRefused(t, signed, "ExpiredToken")
			clock.Set(time.Now())
			if status, _, _ := call(t, http.MethodGet, signed, nil, nil); status != http.StatusOK {
				t.Fatalf("the address answered %d once the clock was back within its life, not 200", status)
			}
		})
		t.Run("an address of another signer is refused", func(t *testing.T) {
			foreign, err := storage.SignedURL(testBucket, name, &storage.SignedURLOptions{
				GoogleAccessID: dummy.email,
				PrivateKey:     dummy.pem,
				Method:         http.MethodGet,
				Expires:        time.Now().Add(15 * time.Minute),
				Scheme:         storage.SigningSchemeV4,
				Hostname:       running.Addr(),
				Insecure:       true,
			})
			if err != nil {
				t.Fatalf("sign an address with the dummy key: %v", err)
			}
			requireDownloadRefused(t, foreign, "AccessDenied")
		})
		t.Run("a refused signBlob signs nothing", func(t *testing.T) {
			double.RefuseSigning(true)
			defer double.RefuseSigning(false)
			_, err := storage.SignedURL(testBucket, name, &storage.SignedURLOptions{
				GoogleAccessID: double.Email(),
				SignBytes:      signBlobVia(running.URL(), double.Email(), token),
				Method:         http.MethodGet,
				Expires:        time.Now().Add(15 * time.Minute),
				Scheme:         storage.SigningSchemeV4,
				Hostname:       running.Addr(),
				Insecure:       true,
			})
			if err == nil || !strings.Contains(err.Error(), "403") {
				t.Fatalf("an address signed while signBlob refuses answered %v, not signBlob's 403", err)
			}
		})
		t.Run("a corrupted object is served as stored", func(t *testing.T) {
			if err := double.Corrupt(testBucket, name); err != nil {
				t.Fatal(err)
			}
			status, _, body := call(t, http.MethodGet, sign(t), nil, nil)
			want := slices.Clone(content)
			want[0] ^= 0xff
			if status != http.StatusOK || !bytes.Equal(body, want) {
				t.Fatalf("the corrupted object downloaded %d and %d bytes, not 200 and its bytes with the first one inverted", status, len(body))
			}
		})
	})
}

// startDouble makes a double over a directory of the test's, serves it on a loopback address of
// its own (testnats.ListenLoopback, the module's listener for a fake a client dials), and stops it
// when the test ends.
func startDouble(t *testing.T, opts Options) (*Server, *Running) {
	t.Helper()
	if opts.Root == "" {
		opts.Root = t.TempDir()
	}
	double, err := New(opts)
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

// put makes an object of the double hold content, failing the test where it cannot, and answers
// its generation.
func put(t *testing.T, double *Server, name string, content []byte) int64 {
	t.Helper()
	generation, err := double.Put(testBucket, name, bytes.NewReader(content), nil)
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

// answer is what one request a test sent was answered: its status and body, or the error that
// kept it from an answer.
type answer struct {
	status int
	header http.Header
	body   []byte
	err    error
}

// request builds a request bound to the test's context, its body a payload built by the caller.
func request(t *testing.T, method, target string, payload payload) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(testwait.Context(t), method, target, bytes.NewReader(payload.body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, target, err)
	}
	maps.Copy(req.Header, payload.header)
	return req
}

// send sends req with a client that carries no credential and reads its whole answer; it fails no
// test, so a goroutine may run it.
func send(req *http.Request) answer {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return answer{err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return answer{status: resp.StatusCode, header: resp.Header, body: body, err: err}
}

// call sends one request with header and body (either may be nil) and answers its status, headers
// and body, failing the test where no answer came.
func call(t *testing.T, method, target string, header http.Header, body io.Reader) (int, http.Header, []byte) {
	t.Helper()
	p := payload{header: header}
	if body != nil {
		read, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		p.body = read
	}
	return callPayload(t, method, target, p)
}

// callPayload is call with a payload's headers and body.
func callPayload(t *testing.T, method, target string, p payload) (int, http.Header, []byte) {
	t.Helper()
	got := send(request(t, method, target, p))
	if got.err != nil {
		t.Fatalf("%s %s: %v", method, target, got.err)
	}
	return got.status, got.header, got.body
}

// payload is a request's headers and body, as multipartBody builds them.
type payload struct {
	header http.Header
	body   []byte
}

// multipartBody is a multipart upload's body, as the Go client builds one: a multipart/related of
// the JSON object resource and then the bytes.
func multipartBody(t *testing.T, resource map[string]any, content []byte) payload {
	t.Helper()
	var body bytes.Buffer
	parts := multipart.NewWriter(&body)
	metadata, err := parts.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	if err == nil {
		err = json.NewEncoder(metadata).Encode(resource)
	}
	var media io.Writer
	if err == nil {
		media, err = parts.CreatePart(textproto.MIMEHeader{"Content-Type": {defaultContentType}})
	}
	if err == nil {
		_, err = media.Write(content)
	}
	if err == nil {
		err = parts.Close()
	}
	if err != nil {
		t.Fatalf("build a multipart upload's body: %v", err)
	}
	return payload{header: http.Header{"Content-Type": {"multipart/related; boundary=" + parts.Boundary()}}, body: body.Bytes()}
}

// startResumable starts a create-only resumable upload of name with resource as its JSON body, and
// answers the session's address from Location, failing the test unless it is one of the double's.
func startResumable(t *testing.T, base, name string, resource map[string]any) string {
	t.Helper()
	var body io.Reader
	if resource != nil {
		encoded, err := json.Marshal(resource)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	query := url.Values{"uploadType": {"resumable"}, "name": {name}, "ifGenerationMatch": {"0"}}
	status, header, answer := call(t, http.MethodPost, base+"/upload/storage/v1/b/"+testBucket+"/o?"+query.Encode(), http.Header{"Content-Type": {"application/json; charset=UTF-8"}}, body)
	location := header.Get("Location")
	if status != http.StatusOK || !strings.HasPrefix(location, base+"/upload/storage/v1/b/"+testBucket+"/o?") || !strings.Contains(location, "upload_id=") {
		t.Fatalf("a resumable upload's start answered %d with Location %q, not 200 and a session address of the double: %s", status, location, answer)
	}
	return location
}

// signBlobCall asks the double's signBlob to sign payload as email, with token as its bearer where
// token is not empty.
func signBlobCall(t *testing.T, base, email, token string, payload []byte) (int, http.Header, []byte) {
	t.Helper()
	header := http.Header{"Content-Type": {"application/json"}}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	body, err := json.Marshal(signBlobRequest{Payload: base64.StdEncoding.EncodeToString(payload)})
	if err != nil {
		t.Fatal(err)
	}
	return call(t, http.MethodPost, base+"/v1/projects/-/serviceAccounts/"+email+":signBlob", header, bytes.NewReader(body))
}

// signBlobVia is a storage.SignedURLOptions.SignBytes that signs through the double's signBlob over
// HTTP with token as its bearer, as the cache's IAM request does; it fails no test, since the client
// calls it.
func signBlobVia(base, email, token string) func([]byte) ([]byte, error) {
	return func(toSign []byte) ([]byte, error) {
		body, err := json.Marshal(signBlobRequest{Payload: base64.StdEncoding.EncodeToString(toSign)})
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequest(http.MethodPost, base+"/v1/projects/-/serviceAccounts/"+email+":signBlob", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		got := send(req)
		if got.err != nil {
			return nil, got.err
		}
		if got.status != http.StatusOK {
			return nil, fmt.Errorf("signBlob answered %d: %s", got.status, got.body)
		}
		var signed signBlobResponse
		if err := json.Unmarshal(got.body, &signed); err != nil {
			return nil, err
		}
		return base64.StdEncoding.DecodeString(signed.SignedBlob)
	}
}

// metadataToken reads the token the double's metadata server issues, as a client on GCE does.
func metadataToken(t *testing.T, base string) string {
	t.Helper()
	status, _, body := call(t, http.MethodGet, base+metadataPrefix+"instance/service-accounts/default/token", http.Header{"Metadata-Flavor": {"Google"}}, nil)
	if status != http.StatusOK {
		t.Fatalf("the metadata server's token answered %d: %s", status, body)
	}
	return decode[tokenAnswer](t, body).AccessToken
}

// requireAbsent fails the test unless the double answers a read of name with 404.
func requireAbsent(t *testing.T, objects, name string) {
	t.Helper()
	if status, _, body := call(t, http.MethodGet, objects+"/"+url.PathEscape(name), nil, nil); status != http.StatusNotFound {
		t.Fatalf("%s reads %d %s, not 404: an object that is not whole is visible", name, status, body)
	}
}

// requireResource fails the test unless res is name's resource holding content, with metadata as
// its custom metadata.
func requireResource(t *testing.T, res objectResource, name string, content []byte, metadata map[string]string) {
	t.Helper()
	md5Sum := md5.Sum(content)
	if res.Name != name || res.Bucket != testBucket || res.Size != strconv.Itoa(len(content)) || res.CRC32C != encodeCRC32C(crc32.Checksum(content, crc32cTable)) ||
		res.MD5Hash != base64.StdEncoding.EncodeToString(md5Sum[:]) || !maps.Equal(res.Metadata, metadata) || res.Generation == "" {
		t.Fatalf("the upload made %+v, not %s of %d bytes with their CRC32C and MD5 and metadata %v", res, name, len(content), metadata)
	}
}

// requireDownloadRefused fails the test unless the double answers a GET of target with 403 and the
// XML error code.
func requireDownloadRefused(t *testing.T, target, code string) {
	t.Helper()
	status, _, body := call(t, http.MethodGet, target, nil, nil)
	if status != http.StatusForbidden || refusalCode(t, body) != code {
		t.Fatalf("the double answered %d with %d bytes beginning %.200q, not 403 %s", status, len(body), body, code)
	}
}

// refusalCode is the Code of Cloud Storage's XML error in body.
func refusalCode(t *testing.T, body []byte) string {
	t.Helper()
	var refusal xmlError
	if err := xml.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("the answer %q is no XML error: %v", body, err)
	}
	return refusal.Code
}

// googleCode is the HTTP status of the Google API error err holds, or 0.
func googleCode(err error) int {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return 0
}

// fixture reads testdata/name.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// decode decodes body's JSON as a T, failing the test where it is not one.
func decode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode %q as %T: %v", body, v, err)
	}
	return v
}

// objectExtra is the path, below the keys and "[]" (an array's elements) of within, of the one key an
// object resource of the double may hold beyond a fixture's: metadata, the custom metadata the
// cache sets on each pack it uploads and reads back before it reuses one. Cloud Storage writes it
// only for an object that has custom metadata, and no object of the fixtures holds the key.
func objectExtra(within ...string) []string {
	return []string{strings.Join(append(within, "metadata"), ".")}
}

// requireFixtureShape fails the test unless status is want and body has the shape of
// testdata/name: the same keys at every level, the same nesting and the same JSON types, with no
// value compared. Each of extra is a dotted path (keys, and "[]" for an array's elements) of a key
// body may hold beyond the fixture's.
func requireFixtureShape(t *testing.T, name string, status, want int, body []byte, extra ...string) {
	t.Helper()
	if status != want {
		t.Fatalf("the double answered %d, not %d as Google did (testdata/%s): %s", status, want, name, body)
	}
	got := decode[any](t, body)
	for _, path := range extra {
		dropKey(got, strings.Split(path, "."))
	}
	wantShape, gotShape := jsonShape(decode[any](t, fixture(t, name))), jsonShape(got)
	if !reflect.DeepEqual(gotShape, wantShape) {
		gotJSON, _ := json.Marshal(gotShape)
		wantJSON, _ := json.Marshal(wantShape)
		t.Fatalf("the double's answer has the shape\n%s\nnot testdata/%s's\n%s", gotJSON, name, wantJSON)
	}
}

// dropKey removes the key path names from the decoded JSON value v, in each element of an array
// where the path says "[]".
func dropKey(v any, path []string) {
	switch v := v.(type) {
	case map[string]any:
		if len(path) == 1 {
			delete(v, path[0])
			return
		}
		dropKey(v[path[0]], path[1:])
	case []any:
		if path[0] == "[]" {
			for _, element := range v {
				dropKey(element, path[1:])
			}
		}
	}
}

// jsonShape is the shape of a decoded JSON value: an object's keys, each with its value's shape; an
// array's distinct element shapes; and otherwise the JSON type's name.
func jsonShape(v any) any {
	switch v := v.(type) {
	case map[string]any:
		shape := make(map[string]any, len(v))
		for key, value := range v {
			shape[key] = jsonShape(value)
		}
		return shape
	case []any:
		shapes := []any{}
		for _, element := range v {
			shape := jsonShape(element)
			if !slices.ContainsFunc(shapes, func(seen any) bool { return reflect.DeepEqual(seen, shape) }) {
				shapes = append(shapes, shape)
			}
		}
		return shapes
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}

// vector is one signingV4Tests row of the pinned Storage module's v4_signatures.json, in the
// fields this test reads.
type vector struct {
	Description              string            `json:"description"`
	Bucket                   string            `json:"bucket"`
	Object                   string            `json:"object"`
	Method                   string            `json:"method"`
	Expiration               int64             `json:"expiration"`
	Timestamp                string            `json:"timestamp"`
	ExpectedURL              string            `json:"expectedUrl"`
	Headers                  map[string]string `json:"headers"`
	URLStyle                 string            `json:"urlStyle"`
	ExpectedCanonicalRequest string            `json:"expectedCanonicalRequest"`
	ExpectedStringToSign     string            `json:"expectedStringToSign"`
}

// readVectors reads the conformance file's signingV4Tests rows, by description.
func readVectors(t *testing.T, path string) map[string]vector {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the pinned Storage module's V4 conformance file: %v", err)
	}
	var file struct {
		SigningV4Tests []vector `json:"signingV4Tests"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	rows := map[string]vector{}
	for _, row := range file.SigningV4Tests {
		rows[row.Description] = row
	}
	return rows
}

// dummyAccount is the conformance file's signer: the inactive dummy service account the pinned
// Storage module publishes, which the module states is fine to be public, as its JSON key file
// holds it.
type dummyAccount struct {
	email string
	pem   []byte
	key   *rsa.PrivateKey
}

// readDummyAccount reads the dummy service account's key file from the module's directory.
func readDummyAccount(t *testing.T, path string) dummyAccount {
	t.Helper()
	var file struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
	}
	if err := json.Unmarshal(fixtureAt(t, path), &file); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	block, _ := pem.Decode([]byte(file.PrivateKey))
	if block == nil {
		t.Fatalf("%s holds no PEM private key", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(*rsa.PrivateKey)
	if err != nil || !ok {
		t.Fatalf("%s's private key is no RSA key (%v)", path, err)
	}
	return dummyAccount{email: file.ClientEmail, pem: []byte(file.PrivateKey), key: key}
}

// fixtureAt reads a file outside testdata, failing the test where it cannot.
func fixtureAt(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// storageModuleDir is the directory of the cloud.google.com/go/storage module this module pins, as
// go list reads it from the module cache, run in this package's directory.
func storageModuleDir(t *testing.T) string {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("find the go command, which reads where the pinned Storage module is: %v", err)
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	list := exec.Command(goTool, "list", "-m", "-f", "{{.Dir}}", "cloud.google.com/go/storage")
	list.Dir = dir
	out, err := list.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		t.Fatalf("go list the pinned Storage module: %v: %s", err, exit.Stderr)
	}
	module := strings.TrimSpace(string(out))
	if err != nil || module == "" {
		t.Fatalf("go list names no directory of the pinned Storage module (%v): is it in the module cache?", err)
	}
	return module
}

// requireKnownAnswer fails the test unless the canonical request and the string to sign the
// verifier builds for row's address are the row's, and a verifier at the row's time takes the
// address as the row's bucket and object.
func requireKnownAnswer(t *testing.T, known Verifier, row vector) {
	t.Helper()
	u := mustURL(t, row.ExpectedURL)
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		t.Fatal(err)
	}
	canonical, toSign := v4Canonical(row.Method, u.Host, u.Path, query)
	if canonical != row.ExpectedCanonicalRequest {
		t.Errorf("the verifier's canonical request is\n%q\nnot the row's\n%q", canonical, row.ExpectedCanonicalRequest)
	}
	if toSign != row.ExpectedStringToSign {
		t.Errorf("the verifier's string to sign is\n%q\nnot the row's\n%q", toSign, row.ExpectedStringToSign)
	}
	at := mustTime(t, row.Timestamp)
	known.Now = func() time.Time { return at }
	bucket, object, err := known.Verify(row.Method, u.Host, u)
	if err != nil || bucket != row.Bucket || object != row.Object {
		t.Fatalf("the verifier answered %q %q (%v) for the row's address, not %q %q", bucket, object, err, row.Bucket, row.Object)
	}
}

// requireRefused fails the test unless v refuses a GET of u with code.
func requireRefused(t *testing.T, v Verifier, u *url.URL, code string) {
	t.Helper()
	_, _, err := v.Verify(http.MethodGet, u.Host, u)
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Code != code {
		t.Fatalf("the verifier answered %v for %s, not a refusal %s", err, u, code)
	}
}

// setQuery is a change of an address that sets its query parameter name to value.
func setQuery(name, value string) func(u *url.URL) {
	return func(u *url.URL) {
		query := u.Query()
		query.Set(name, value)
		u.RawQuery = query.Encode()
	}
}

// mustURL parses raw, failing the test where it is no URL.
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// mustTime parses an RFC 3339 time, failing the test where it is none.
func mustTime(t *testing.T, raw string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// newKey is a fresh RSA key no signer of the tests holds.
func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// randomBytes is n random bytes, the content of a pack whose bytes a test compares.
func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	content := make([]byte, n)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	return content
}

// sha256Hex is content's SHA-256 in hex, the custom metadata the cache sets on each pack.
func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// testClock is a clock a test sets, which Options.Now reads.
type testClock struct{ at atomic.Int64 }

// Now is the time last set.
func (c *testClock) Now() time.Time { return time.Unix(0, c.at.Load()) }

// Set makes Now answer at.
func (c *testClock) Set(at time.Time) { c.at.Store(at.UnixNano()) }
