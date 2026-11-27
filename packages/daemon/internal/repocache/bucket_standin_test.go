package repocache

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The credentials the bucket rows' stand-ins hand out or write; -RDX- and the not-a-secret names
// are markers the redaction check reads in captured output.
const (
	// wordsBearer is the access token the stand-in's metadata server issues: the bearer every
	// Storage and IAM Credentials request of a production bucket carries, and the stand-in in
	// Google's place echoes.
	wordsBearer = "bucket-bearer-not-a-secret-RDX-4c1e"
	// wordsQueryToken is a credential in the query of a URL a metadata server's refusal quotes.
	wordsQueryToken = "not-a-secret-token-RDX-9a07"
	// wordsUser and wordsPassword are user information a configured address or the metadata
	// server's account name writes before an '@'.
	wordsUser, wordsPassword = "not-a-secret-user", "not-a-secret-password"
)

// googleStandIn serves, until t ends, a stand-in in the metadata server's, Cloud Storage's and IAM
// Credentials' place, and opens a production bucket against it as the cache does (openBucketAt):
// GCE_METADATA_HOST names the stand-in, STORAGE_EMULATOR_HOST is empty, and
// GOOGLE_APPLICATION_CREDENTIALS names a file no code may read (unreadableCredentials). answer
// writes the stand-in's answer to each request a row picks out by its route (googleRoute) and
// answers false for any other; the metadata server then answers its account as account and its
// token as wordsBearer, and every other request is refused 400, a status no client of the bucket
// retries, failing the test.
func googleStandIn(t *testing.T, account string, answer func(w http.ResponseWriter, r *http.Request, route string) bool) (*bucket, error) {
	t.Helper()
	base := serveHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		route := googleRoute(r)
		switch {
		case answer(w, r, route):
		case route == "metadata-email":
			_, _ = io.WriteString(w, account)
		case route == "metadata-token":
			standInJSON(w, http.StatusOK, map[string]any{"access_token": wordsBearer, "expires_in": 3599, "token_type": "Bearer"})
		default:
			t.Errorf("the bucket sent %s %s, which the row does not expect", r.Method, r.URL)
			http.Error(w, "the row does not expect this request", http.StatusBadRequest)
		}
	}))
	t.Setenv(emulatorHostEnv, "")
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(base, "http://"))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", unreadableCredentials(t))
	b, err := openBucketAt(testwait.Context(t), testBucket, testRepo, googleEndpoints{storage: base + "/storage/v1/", iam: base + "/"})
	if b != nil {
		t.Cleanup(func() { _ = b.close() })
	}
	return b, err
}

// googleRoute names the request of a production bucket r is, as the rows pick it out: the
// metadata server's account (metadata-email) and token (metadata-token), Cloud Storage's list,
// read, delete and upload of testBucket's objects, and IAM Credentials' signBlob; "" for any other.
func googleRoute(r *http.Request) string {
	path := r.URL.EscapedPath()
	objects := "/storage/v1/b/" + testBucket + "/o"
	switch {
	case path == "/computeMetadata/v1/instance/service-accounts/default/email":
		return "metadata-email"
	case path == "/computeMetadata/v1/instance/service-accounts/default/token":
		return "metadata-token"
	case path == objects && r.Method == http.MethodGet:
		return "list"
	case strings.HasPrefix(path, objects+"/") && r.Method == http.MethodGet:
		return "read"
	case strings.HasPrefix(path, objects+"/") && r.Method == http.MethodDelete:
		return "delete"
	case strings.HasPrefix(path, "/upload/storage/v1/b/"+testBucket+"/o"):
		return "upload"
	case strings.HasPrefix(path, "/v1/projects/-/serviceAccounts/"):
		return "signBlob"
	}
	return ""
}

// standInJSON answers status with v as JSON.
func standInJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// googleRefusal answers status in route's error shape, Cloud Storage's JSON API error or IAM
// Credentials' google.rpc status for signBlob, with message.
func googleRefusal(w http.ResponseWriter, route string, status int, message string) {
	body := map[string]any{"code": status, "message": message}
	if route == "signBlob" {
		body["status"] = "PERMISSION_DENIED"
	} else {
		body["errors"] = []map[string]any{{"message": message, "domain": "global", "reason": "forbidden"}}
	}
	standInJSON(w, status, map[string]any{"error": body})
}

// echoRefusal answers r 403 in route's error shape with a message that quotes the Authorization r
// carried, as whatever answers in Google's place can.
func echoRefusal(w http.ResponseWriter, r *http.Request, route string) {
	googleRefusal(w, route, http.StatusForbidden, "the gateway refused the request, which carried Authorization: "+r.Header.Get("Authorization"))
}

// refuseRoute is a stand-in's answer that refuses each request of route with echoRefusal.
func refuseRoute(route string) func(w http.ResponseWriter, r *http.Request, asked string) bool {
	return func(w http.ResponseWriter, r *http.Request, asked string) bool {
		if asked != route {
			return false
		}
		echoRefusal(w, r, asked)
		return true
	}
}

// echoedBearer is the bearer r carried, its scheme cut off.
func echoedBearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// answeredObject is Cloud Storage's JSON API resource of an object of testBucket named name, of
// size bytes with CRC32C crc, whose sha256Metadata is sha.
func answeredObject(name string, size int64, crc uint32, sha string) map[string]any {
	return map[string]any{
		"kind": "storage#object", "bucket": testBucket, "name": name, "generation": "1", "metageneration": "1",
		"size": strconv.FormatInt(size, 10), "crc32c": base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, crc)),
		"metadata": map[string]string{sha256Metadata: sha},
	}
}
