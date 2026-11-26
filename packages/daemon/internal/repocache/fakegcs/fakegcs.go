// Package fakegcs is a stand-in for the Google services that the repository cache and an issue
// pod's fetch reach when the cache keeps its packs in a bucket
// (runtime.kubernetes.repository_cache.bucket): Cloud Storage's JSON API, for the objects the cache
// lists, reads the metadata of, uploads and deletes; Cloud Storage's V4 signed downloads, which an
// issue pod reads each pack through; the IAM Credentials API's signBlob, which the cache signs those
// addresses with; and the GCE metadata server's paths, which the cache takes its identity and its
// token from.
//
// It exists because no identity that a pod or a builds Job here may use can sign an address: on
// 2026-11-25 each of 7 service accounts was refused iam.serviceAccounts.signBlob. So the cache's
// tests and the burst Job that runs before the merge run the real cache and the real fetch against
// this double, which signs with an RSA key it makes at its start, and a real bucket's signed
// addresses are proven after the merge, in production.
//
// No package of the production binary imports it, and the daemon passes no setting that leads to
// it: a test, or the burst Job, points the cache at it with a loopback STORAGE_EMULATOR_HOST and
// GCE_METADATA_HOST. Its files other than tests import the standard library alone, so the burst Job
// copies them, with cmd/fakegcs, into a scratch module and compiles them with the worker image's Go,
// with no go.sum of this module to read.
//
// What it answers is what Cloud Storage and IAM answer, in the fields and the JSON types a client
// reads, as the fixtures in testdata hold them (testdata/README.md says where each came from). What
// it checks, as they do:
//
//   - An object is visible to a list, a metadata read and a download only once its upload
//     completed: its bytes go to a temporary file, which is renamed into place once the last byte
//     arrived and passed its checks.
//   - ifGenerationMatch on an upload (0: create only), a metadata read and a delete: 412 where it
//     does not hold.
//   - A CRC32C or an MD5 that an upload states, against the bytes the double received: 400 where
//     they differ.
//   - A signed download, by Google's V4 scheme as the double reads Google's description of it, not
//     as the code that made the address reads it (Verifier): 403 for an address with no signature,
//     a changed path or query, a wrong scope or signed headers, an unknown signer, or one past its
//     expiry.
//   - With Options.RequireToken, the metadata server's token on every Storage and IAM request: 401
//     otherwise.
//
// It keeps each object's bytes in a file under Options.Root, never in memory, and serves a download
// with http.ServeContent from that file, so many large downloads at once cost it little. It
// implements nothing that a client of the cache does not use, and it refuses with 400 a parameter it
// does not implement rather than ignore it, so a client that comes to rely on one fails by name.
package fakegcs

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// DefaultEmail is the service account a double answers as when Options names none: a placeholder
// in the shape of a Google service account's address, the one testdata/metadata-email.txt holds.
const DefaultEmail = "legion-cache@example-project.iam.gserviceaccount.com"

// objectsDir and uploadsDir are the double's two directories under Options.Root: the files of the
// objects it holds, each named by its generation, and the temporary files of the uploads it is
// receiving, which a completed upload renames into objectsDir.
const (
	objectsDir = "objects"
	uploadsDir = "uploads"
)

// metadataPrefix is the path under which the GCE metadata server answers, the one
// cloud.google.com/go/compute/metadata asks below GCE_METADATA_HOST.
const metadataPrefix = "/computeMetadata/v1/"

// Options is one double's: the scratch directory it keeps its objects in (Root, which it creates
// and never removes, so the caller removes it), the service account the metadata server answers as
// and signBlob signs for (Email, DefaultEmail when empty), its clock (Now, time.Now when nil),
// which a signed download's expiry and each object's generation and times read, and whether every
// Storage and IAM request must carry the token its metadata server issued (RequireToken), so that a
// test can prove that only the metadata server's identity reached Storage and IAM.
type Options struct {
	Root         string
	Email        string
	Now          func() time.Time
	RequireToken bool
}

// Route is one kind of request the double serves: the key under which it counts the requests it
// received (Server.Count).
type Route string

// The routes the double counts: the Storage JSON API's list, metadata read and delete; a
// multipart upload or a resumable upload's start (RouteUpload), and a resumable upload's later
// requests, its chunks and its status queries (RouteUploadChunk); IAM's signBlob; the metadata
// server's email and token; a signed download; and any other path (RouteOther).
const (
	RouteList          Route = "list"
	RouteGet           Route = "get"
	RouteDelete        Route = "delete"
	RouteUpload        Route = "upload"
	RouteUploadChunk   Route = "upload-chunk"
	RouteSignBlob      Route = "signBlob"
	RouteMetadataEmail Route = "metadata-email"
	RouteMetadataToken Route = "metadata-token"
	RouteDownload      Route = "download"
	RouteOther         Route = "other"
)

// authenticated reports whether a request of the route is a Storage or IAM request, one that
// Options.RequireToken makes present the metadata server's token. The metadata server's own paths
// take no token, and a signed download is authorized by its address alone.
func (r Route) authenticated() bool {
	switch r {
	case RouteList, RouteGet, RouteDelete, RouteUpload, RouteUploadChunk, RouteSignBlob:
		return true
	}
	return false
}

// Count is what the double counted of one route's requests: how many it received, refused ones
// included, and how many of those carried an Authorization header of any kind, so that a test can
// show that a signed download carried none.
type Count struct {
	Requests          int
	WithAuthorization int
}

// Request is one request the double received: its route, its method and its URL as the client
// sent it, query included, so that a test reads which object or which list prefix each request
// named (a Storage JSON API path's object, a list's prefix parameter, an upload's name
// parameter), and not only how many of a route there were.
type Request struct {
	Route  Route
	Method string
	URL    url.URL
}

// Server is the double, an http.Handler. New makes one, and Serve serves it on a listener the
// caller holds: a test passes a loopback listener from the module's own helper
// (testnats.ListenLoopback), and cmd/fakegcs one it binds itself.
//
// Its fault switches are for tests of the cache's and the fetch's own refusals: FailUpload and
// FailDelete make a later request answer an error, HoldNext stops an upload before its object
// becomes visible until the test releases it, Corrupt changes an object's stored bytes behind its
// metadata, and RefuseSigning makes signBlob answer IAM's refusal. Count and Requests read what it
// received.
type Server struct {
	root         string
	email        string
	now          func() time.Time
	requireToken bool
	token        string
	key          *rsa.PrivateKey
	keyID        string
	verifier     Verifier

	// mu guards every field below it. An object, once installed, never changes: a later upload of
	// its name installs a new one in its place.
	mu             sync.Mutex
	objects        map[objectKey]*object
	lastGeneration int64
	sessions       map[string]*session
	counts         map[Route]Count
	requests       []Request
	completions    int
	deletions      int
	uploadFaults   []fault
	deleteFaults   []fault
	holds          []*Hold
	refuseSigning  bool
}

// fault is one armed failure: the request of its kind numbered at (counted from the double's
// start) answers status.
type fault struct {
	at     int
	status int
}

// New makes a double over the scratch directory opts.Root, with a fresh 2048-bit RSA key of its own
// that signBlob signs with and its signed downloads are checked against, and a fresh token that its
// metadata server issues. It refuses an empty Root.
func New(opts Options) (*Server, error) {
	if opts.Root == "" {
		return nil, errors.New("fakegcs: no scratch directory (Options.Root) to keep objects in")
	}
	email := opts.Email
	if email == "" {
		email = DefaultEmail
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	for _, dir := range []string{objectsDir, uploadsDir} {
		if err := os.MkdirAll(filepath.Join(opts.Root, dir), 0o700); err != nil {
			return nil, fmt.Errorf("fakegcs: make the %s directory under %s: %w", dir, opts.Root, err)
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("fakegcs: make the signing key: %w", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("fakegcs: encode the signing key's public half: %w", err)
	}
	keyID := sha1.Sum(der)
	return &Server{
		root:         opts.Root,
		email:        email,
		now:          now,
		requireToken: opts.RequireToken,
		token:        "fakegcs-" + rand.Text(),
		key:          key,
		keyID:        hex.EncodeToString(keyID[:]),
		verifier:     Verifier{Signers: map[string]*rsa.PublicKey{email: &key.PublicKey}, Now: now},
		objects:      map[objectKey]*object{},
		sessions:     map[string]*session{},
		counts:       map[Route]Count{},
	}, nil
}

// Email is the service account the double's metadata server answers as and its signBlob signs for.
func (s *Server) Email() string { return s.email }

// Token is the access token the double's metadata server issues, the one Options.RequireToken
// makes every Storage and IAM request present.
func (s *Server) Token() string { return s.token }

// Count is what the double counted of route's requests so far.
func (s *Server) Count(route Route) Count {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[route]
}

// Requests is every request the double received so far, refused ones included, in the order it
// received them.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// FailUpload makes the n-th upload request from now (n 1 or more) that would complete an object
// answer status with Cloud Storage's JSON error shape, before the double reads its body, so that
// nothing of it is kept: a resumable upload stays where it was and may send its last chunk again.
// The requests that would complete an object are a multipart upload and a resumable upload's
// request that names its total size, and each attempt counts, a client's retry included.
func (s *Server) FailUpload(n, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploadFaults = append(s.uploadFaults, fault{at: s.completions + n, status: status})
}

// FailDelete makes the n-th delete request from now (n 1 or more) answer status with Cloud
// Storage's JSON error shape, deleting nothing; each attempt counts, a client's retry included.
func (s *Server) FailDelete(n, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteFaults = append(s.deleteFaults, fault{at: s.deletions + n, status: status})
}

// nextFault counts one more request of a kind (*seen) and answers the status of the fault armed for
// it, removing that fault, or 0 where none is. The caller holds s.mu.
func nextFault(seen *int, faults *[]fault) int {
	*seen++
	for i, f := range *faults {
		if f.at == *seen {
			*faults = append((*faults)[:i], (*faults)[i+1:]...)
			return f.status
		}
	}
	return 0
}

// RefuseSigning makes signBlob answer IAM's refusal (testdata/iam-sign-403.json's shape) while on
// is true, as IAM answers a caller that may not sign as the account.
func (s *Server) RefuseSigning(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuseSigning = on
}

// Hold is one stop HoldNext set: the next upload of an object it matches stops once the double
// received all of its bytes and checked them, before its generation precondition is read and before
// the object becomes visible, and waits there until Release, so that a test sees what a client sees
// while an upload is unfinished. A Hold stops one upload; a later one passes.
type Hold struct {
	match   func(bucket, name string) bool
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

// HoldNext sets a Hold on the next upload whose bucket and object name match answers true for.
func (s *Server) HoldNext(match func(bucket, name string) bool) *Hold {
	h := &Hold{match: match, reached: make(chan struct{}), release: make(chan struct{})}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holds = append(s.holds, h)
	return h
}

// Reached is closed once an upload stopped at the hold.
func (h *Hold) Reached() <-chan struct{} { return h.reached }

// Release lets the upload stopped at the hold go on, or the next one that reaches it pass. A second
// Release does nothing.
func (h *Hold) Release() { h.once.Do(func() { close(h.release) }) }

// waitAtHold stops an upload of bucket/name at the first Hold that matches it, taking that Hold,
// until the Hold is released (true) or the request ends (false: its client gave up, or the server
// stops). With no Hold that matches, it answers true at once.
func (s *Server) waitAtHold(ctx context.Context, bucket, name string) bool {
	s.mu.Lock()
	var hold *Hold
	for i, h := range s.holds {
		if h.match(bucket, name) {
			hold = h
			s.holds = append(s.holds[:i], s.holds[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	if hold == nil {
		return true
	}
	close(hold.reached)
	select {
	case <-hold.release:
		return true
	case <-ctx.Done():
		return false
	}
}

// ServeHTTP counts and records the request under its route, refuses a Storage or IAM request
// without the metadata server's token where Options.RequireToken says so, and serves the rest.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route, serve := s.route(r)
	s.mu.Lock()
	count := s.counts[route]
	count.Requests++
	if r.Header.Get("Authorization") != "" {
		count.WithAuthorization++
	}
	s.counts[route] = count
	s.requests = append(s.requests, Request{Route: route, Method: r.Method, URL: *r.URL})
	s.mu.Unlock()
	if s.requireToken && route.authenticated() && r.Header.Get("Authorization") != "Bearer "+s.token {
		if route == RouteSignBlob {
			writeRPCError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "Request had invalid authentication credentials. Expected OAuth 2 access token, login cookie or other valid authentication credential. See https://developers.google.com/identity/sign-in/web/devconsole-project.", nil)
			return
		}
		writeStorageError(w, http.StatusUnauthorized, storageErrorItem{Message: "Invalid Credentials", Reason: "authError", LocationType: "header", Location: "Authorization"})
		return
	}
	serve(w, r)
}

// route names the request's route and the handler that serves it. A Storage JSON API path is read
// in its escaped form, since an object's name is one escaped path segment there, and a signed
// download's path in its decoded form, since there an object's name is the rest of the path.
func (s *Server) route(r *http.Request) (Route, http.HandlerFunc) {
	path := r.URL.EscapedPath()
	switch {
	case strings.HasPrefix(path, metadataPrefix):
		switch strings.TrimPrefix(path, metadataPrefix) {
		case "instance/service-accounts/default/email":
			return RouteMetadataEmail, s.metadataEmail
		case "instance/service-accounts/default/token":
			return RouteMetadataToken, s.metadataToken
		}
		return RouteOther, http.NotFound
	case strings.HasPrefix(path, "/storage/v1/b/"):
		bucket, name, hasName, ok := objectPath(strings.TrimPrefix(path, "/storage/v1/b/"))
		switch {
		case !ok:
			return RouteOther, storageNotFound
		case !hasName && r.Method == http.MethodGet:
			return RouteList, func(w http.ResponseWriter, r *http.Request) { s.list(w, r, bucket) }
		case hasName && r.Method == http.MethodGet:
			return RouteGet, func(w http.ResponseWriter, r *http.Request) { s.get(w, r, bucket, name) }
		case hasName && r.Method == http.MethodDelete:
			return RouteDelete, func(w http.ResponseWriter, r *http.Request) { s.delete(w, r, bucket, name) }
		}
		return RouteOther, storageMethodNotAllowed
	case strings.HasPrefix(path, "/upload/storage/v1/b/"):
		bucket, _, hasName, ok := objectPath(strings.TrimPrefix(path, "/upload/storage/v1/b/"))
		if !ok || hasName {
			return RouteOther, storageNotFound
		}
		query := r.URL.Query()
		if query.Get("upload_id") != "" {
			if r.Method != http.MethodPut && r.Method != http.MethodPost {
				return RouteUploadChunk, storageMethodNotAllowed
			}
			return RouteUploadChunk, s.uploadChunk
		}
		if r.Method != http.MethodPost {
			return RouteUpload, storageMethodNotAllowed
		}
		switch query.Get("uploadType") {
		case "multipart":
			return RouteUpload, func(w http.ResponseWriter, r *http.Request) { s.multipartUpload(w, r, bucket) }
		case "resumable":
			return RouteUpload, func(w http.ResponseWriter, r *http.Request) { s.startResumable(w, r, bucket) }
		}
		return RouteUpload, func(w http.ResponseWriter, r *http.Request) {
			writeStorageError(w, http.StatusBadRequest, invalid(fmt.Sprintf("fakegcs serves uploadType multipart and resumable, not %q", r.URL.Query().Get("uploadType"))))
		}
	case strings.HasPrefix(path, "/v1/projects/"):
		return RouteSignBlob, s.signBlob
	}
	return RouteDownload, s.download
}

// objectPath reads the part of a Storage JSON API path after /b/: a bucket, then "o", then, for one
// object, its name as one escaped segment. It answers ok false for any other shape, a name holding
// an unescaped slash among them, which Cloud Storage reads as no object either.
func objectPath(rest string) (bucket, name string, hasName, ok bool) {
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[1] != "o" {
		return "", "", false, false
	}
	bucket, err := url.PathUnescape(parts[0])
	if err != nil || !validBucket(bucket) {
		return "", "", false, false
	}
	if len(parts) == 2 {
		return bucket, "", false, true
	}
	name, err = url.PathUnescape(parts[2])
	if err != nil || name == "" {
		return "", "", false, false
	}
	return bucket, name, true, true
}

// validBucket reports whether name is a bucket name of Cloud Storage's form: 3 to 63 lower-case
// letters, digits, dashes, underscores and dots, starting and ending with a letter or a digit. The
// double holds any bucket so named, making it on its first upload.
func validBucket(name string) bool {
	if len(name) < 3 || len(name) > 63 {
		return false
	}
	for i := range len(name) {
		c := name[i]
		alnum := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if !alnum && (i == 0 || i == len(name)-1 || c != '-' && c != '_' && c != '.') {
			return false
		}
	}
	return true
}

// Running is a double serving one listener: Serve's answer.
type Running struct {
	http    *http.Server
	addr    string
	cancel  context.CancelFunc
	stopped chan struct{}
	err     error
}

// Serve serves the double on listener until Close, in a goroutine of its own. Each request's
// context ends at Close, so that an upload stopped at a Hold no test released ends with it.
func (s *Server) Serve(listener net.Listener) *Running {
	base, cancel := context.WithCancel(context.Background())
	running := &Running{
		http:    &http.Server{Handler: s, BaseContext: func(net.Listener) context.Context { return base }},
		addr:    listener.Addr().String(),
		cancel:  cancel,
		stopped: make(chan struct{}),
	}
	go func() {
		defer close(running.stopped)
		if err := running.http.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			running.err = err
		}
	}()
	return running
}

// Addr is the host:port the double serves on: what a client's STORAGE_EMULATOR_HOST and
// GCE_METADATA_HOST name, and the Hostname a test's storage.SignedURL signs for.
func (r *Running) Addr() string { return r.addr }

// URL is the double's base address, http://<Addr>.
func (r *Running) URL() string { return "http://" + r.addr }

// Wait waits until the double stops serving and answers why: nil after Close, or the listener's
// error.
func (r *Running) Wait() error {
	<-r.stopped
	return r.err
}

// Close stops the double: it ends every request's context, then stops the server with Shutdown, on
// a context already ended so that it waits for no request, before Close, so that no handler starts
// on a connection the stop closed after reading its request's headers (docs/testing.md, "A fake HTTP
// server whose client gives up on requests"), and waits for the serving goroutine to return.
func (r *Running) Close() error {
	r.cancel()
	ended, end := context.WithCancel(context.Background())
	end()
	_ = r.http.Shutdown(ended)
	err := r.http.Close()
	<-r.stopped
	return err
}

// writeJSON answers status with v as JSON, as Cloud Storage and IAM answer.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "fakegcs: encode the answer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
