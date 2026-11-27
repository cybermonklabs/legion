package fakegcs

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// maxListResults is the most objects one list page holds, Cloud Storage's own cap on maxResults
// and its default.
const maxListResults = 1000

// defaultContentType is the content type Cloud Storage gives an object whose upload named none.
const defaultContentType = "application/octet-stream"

// crc32cTable is the Castagnoli polynomial's table, the CRC32C Cloud Storage keeps of each object.
var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// errPrecondition is install's answer when the upload's ifGenerationMatch does not hold.
var errPrecondition = errors.New("fakegcs: the generation precondition does not hold")

// objectKey names one live object: its bucket and its name.
type objectKey struct {
	bucket, name string
}

// object is one live object: its bucket and name, the generation it was written as, what its
// upload said of it (content type, custom metadata) and what the double computed of its bytes
// (size, MD5, CRC32C), when it was written, and the file under Options.Root/objects that holds its
// bytes. An object never changes once installed; a later upload of its name replaces it whole.
type object struct {
	bucket      string
	name        string
	generation  int64
	contentType string
	metadata    map[string]string
	size        int64
	md5         []byte
	crc32c      uint32
	created     time.Time
	path        string
}

// objectResource is Cloud Storage's JSON object resource, in the fields and the order
// testdata/object.json holds, all of them strings as Cloud Storage writes them, generation and size
// included, and the custom metadata an upload carried (Metadata), which that fixture's object had
// none of: Cloud Storage leaves the field out where there is none, and so does the double.
type objectResource struct {
	Kind           string            `json:"kind"`
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Bucket         string            `json:"bucket"`
	Generation     string            `json:"generation"`
	Metageneration string            `json:"metageneration"`
	ContentType    string            `json:"contentType"`
	StorageClass   string            `json:"storageClass"`
	Size           string            `json:"size"`
	MD5Hash        string            `json:"md5Hash"`
	CRC32C         string            `json:"crc32c"`
	Etag           string            `json:"etag"`
	TimeCreated    string            `json:"timeCreated"`
	Updated        string            `json:"updated"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// objectList is Cloud Storage's JSON answer to a list, as testdata/list-next.json and
// list-final.json hold it: no items where none matched, and a nextPageToken only where a page
// follows.
type objectList struct {
	Kind          string           `json:"kind"`
	Items         []objectResource `json:"items,omitempty"`
	NextPageToken string           `json:"nextPageToken,omitempty"`
}

// resource is the object's JSON resource. Its metageneration is always 1, since the double changes
// no object's metadata after its upload, and its etag is Cloud Storage's encoding of generation and
// metageneration: protocol buffer fields 1 and 2 as varints, in base64.
func (o *object) resource() objectResource {
	etag := binary.AppendUvarint([]byte{0x08}, uint64(o.generation))
	etag = binary.AppendUvarint(append(etag, 0x10), 1)
	generation := strconv.FormatInt(o.generation, 10)
	created := o.created.UTC().Format("2006-01-02T15:04:05.000Z")
	return objectResource{
		Kind:           "storage#object",
		ID:             o.bucket + "/" + o.name + "/" + generation,
		Name:           o.name,
		Bucket:         o.bucket,
		Generation:     generation,
		Metageneration: "1",
		ContentType:    o.contentType,
		StorageClass:   "STANDARD",
		Size:           strconv.FormatInt(o.size, 10),
		MD5Hash:        base64.StdEncoding.EncodeToString(o.md5),
		CRC32C:         encodeCRC32C(o.crc32c),
		Etag:           base64.StdEncoding.EncodeToString(etag),
		TimeCreated:    created,
		Updated:        created,
		Metadata:       o.metadata,
	}
}

// encodeCRC32C is a CRC32C as Cloud Storage writes it: its four bytes, big-endian, in base64.
func encodeCRC32C(sum uint32) string {
	return base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, sum))
}

// validName reports whether name is an object name Cloud Storage takes: 1 to 1024 bytes of UTF-8
// holding no carriage return or line feed. The double never makes a path of a name, so it needs no
// other check.
func validName(name string) bool {
	return name != "" && len(name) <= 1024 && utf8.ValidString(name) && !strings.ContainsAny(name, "\r\n")
}

// precondition is the ifGenerationMatch a request carries: none (set false), 0 for an object that
// must not exist yet, or the generation the live object must have.
type precondition struct {
	set        bool
	generation int64
}

// holds reports whether the precondition holds for the live object o, nil where there is none.
func (p precondition) holds(o *object) bool {
	switch {
	case !p.set:
		return true
	case p.generation == 0:
		return o == nil
	}
	return o != nil && o.generation == p.generation
}

// unsupportedConditions are the preconditions Cloud Storage takes besides ifGenerationMatch, which
// the double refuses rather than ignore, since no client of the cache sends them.
var unsupportedConditions = []string{"ifGenerationNotMatch", "ifMetagenerationMatch", "ifMetagenerationNotMatch", "generation"}

// readPrecondition reads a request's ifGenerationMatch, answering 400 and false where it is no
// whole number, or where the request names a precondition the double does not implement.
func readPrecondition(w http.ResponseWriter, query url.Values) (precondition, bool) {
	if name := unsupportedParameter(query, unsupportedConditions...); name != "" {
		writeStorageError(w, http.StatusBadRequest, invalid(fmt.Sprintf("fakegcs does not implement the %s parameter", name)))
		return precondition{}, false
	}
	value := query.Get("ifGenerationMatch")
	if value == "" {
		return precondition{}, true
	}
	generation, err := strconv.ParseInt(value, 10, 64)
	if err != nil || generation < 0 {
		writeStorageError(w, http.StatusBadRequest, invalid(fmt.Sprintf("Invalid argument for ifGenerationMatch: %q", value)))
		return precondition{}, false
	}
	return precondition{set: true, generation: generation}, true
}

// unsupportedParameter answers the first of names that query sets to anything but empty or false,
// or "" where it sets none: the Go client sends a list's every parameter, unset ones as empty or
// false, so only a value says a client asked for one.
func unsupportedParameter(query url.Values, names ...string) string {
	for _, name := range names {
		if value := query.Get(name); value != "" && value != "false" {
			return name
		}
	}
	return ""
}

// list answers a list of bucket's objects: those whose names start with prefix, in the byte order
// of their names, after the page pageToken ended, at most maxResults of them (at most
// maxListResults), and a nextPageToken only where more follow. The token is the last name the page
// held, encoded; Cloud Storage's is opaque, so a client reads nothing in it.
func (s *Server) list(w http.ResponseWriter, r *http.Request, bucket string) {
	query := r.URL.Query()
	if name := unsupportedParameter(query, "delimiter", "startOffset", "endOffset", "versions", "includeTrailingDelimiter", "matchGlob", "includeFoldersAsPrefixes", "softDeleted", "filter"); name != "" {
		writeStorageError(w, http.StatusBadRequest, invalid(fmt.Sprintf("fakegcs does not implement the %s parameter", name)))
		return
	}
	limit := maxListResults
	if value := query.Get("maxResults"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			writeStorageError(w, http.StatusBadRequest, invalid(fmt.Sprintf("Invalid argument for maxResults: %q", value)))
			return
		}
		limit = min(n, maxListResults)
	}
	after := ""
	if token := query.Get("pageToken"); token != "" {
		name, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			writeStorageError(w, http.StatusBadRequest, invalid(fmt.Sprintf("Invalid page token: %q", token)))
			return
		}
		after = string(name)
	}
	prefix := query.Get("prefix")
	s.mu.Lock()
	var matched []*object
	for key, o := range s.objects {
		if key.bucket == bucket && strings.HasPrefix(key.name, prefix) && key.name > after {
			matched = append(matched, o)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(matched, func(a, b *object) int { return strings.Compare(a.name, b.name) })
	answer := objectList{Kind: "storage#objects"}
	for _, o := range matched[:min(limit, len(matched))] {
		answer.Items = append(answer.Items, o.resource())
	}
	if len(matched) > limit {
		answer.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(matched[limit-1].name))
	}
	writeJSON(w, http.StatusOK, answer)
}

// get answers bucket/name's object resource: 404 where there is no such object, and 412 where the
// request's ifGenerationMatch does not hold for it. The double serves an object's bytes through a
// signed download alone, so it refuses alt=media.
func (s *Server) get(w http.ResponseWriter, r *http.Request, bucket, name string) {
	query := r.URL.Query()
	if alt := query.Get("alt"); alt != "" && alt != "json" {
		writeStorageError(w, http.StatusBadRequest, invalid(fmt.Sprintf("fakegcs serves an object's bytes through a V4 signed download alone, not alt=%s", alt)))
		return
	}
	cond, ok := readPrecondition(w, query)
	if !ok {
		return
	}
	s.mu.Lock()
	o := s.objects[objectKey{bucket, name}]
	s.mu.Unlock()
	switch {
	case o == nil:
		writeStorageError(w, http.StatusNotFound, noSuchObject(bucket, name))
	case !cond.holds(o):
		writeStorageError(w, http.StatusPreconditionFailed, conditionNotMet)
	default:
		writeJSON(w, http.StatusOK, o.resource())
	}
}

// delete removes bucket/name's object and answers 204 with no body, as Cloud Storage does: 404
// where there is no such object, 412 where the request's ifGenerationMatch does not hold for it, and
// the status FailDelete armed for this request, deleting nothing.
func (s *Server) delete(w http.ResponseWriter, r *http.Request, bucket, name string) {
	cond, ok := readPrecondition(w, r.URL.Query())
	if !ok {
		return
	}
	key := objectKey{bucket, name}
	s.mu.Lock()
	if status := nextFault(&s.deletions, &s.deleteFaults); status != 0 {
		s.mu.Unlock()
		writeStorageError(w, status, injected(status))
		return
	}
	o := s.objects[key]
	switch {
	case o == nil:
		s.mu.Unlock()
		writeStorageError(w, http.StatusNotFound, noSuchObject(bucket, name))
		return
	case !cond.holds(o):
		s.mu.Unlock()
		writeStorageError(w, http.StatusPreconditionFailed, conditionNotMet)
		return
	}
	delete(s.objects, key)
	s.mu.Unlock()
	_ = os.Remove(o.path)
	w.WriteHeader(http.StatusNoContent)
}

// Put makes bucket/name hold content's bytes, with custom metadata and the default content type,
// as a completed upload with no precondition would, and answers its generation: for a test that
// needs objects in place before the code under test runs. It takes no Hold and no FailUpload.
func (s *Server) Put(bucket, name string, content io.Reader, metadata map[string]string) (int64, error) {
	if !validBucket(bucket) || !validName(name) {
		return 0, fmt.Errorf("fakegcs: %q/%q is no bucket and object name Cloud Storage takes", bucket, name)
	}
	got, err := s.receive(content)
	if err != nil {
		return 0, err
	}
	o, err := s.install(uploadAttrs{bucket: bucket, name: name, contentType: defaultContentType, metadata: metadata}, got)
	if err != nil {
		_ = os.Remove(got.path)
		return 0, err
	}
	return o.generation, nil
}

// Corrupt inverts the first byte of bucket/name's stored bytes and leaves its metadata as it was, so
// its size and checksums no longer describe what a download serves: what a bucket that changed a
// pack looks like to a reader that checks what it read.
func (s *Server) Corrupt(bucket, name string) error {
	s.mu.Lock()
	o := s.objects[objectKey{bucket, name}]
	s.mu.Unlock()
	if o == nil {
		return fmt.Errorf("fakegcs: no object %s/%s to corrupt", bucket, name)
	}
	if o.size == 0 {
		return fmt.Errorf("fakegcs: %s/%s is empty, so it has no byte to corrupt", bucket, name)
	}
	f, err := os.OpenFile(o.path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("fakegcs: open %s/%s's bytes: %w", bucket, name, err)
	}
	defer f.Close()
	var first [1]byte
	if _, err := f.ReadAt(first[:], 0); err != nil {
		return fmt.Errorf("fakegcs: read %s/%s's first byte: %w", bucket, name, err)
	}
	first[0] ^= 0xff
	if _, err := f.WriteAt(first[:], 0); err != nil {
		return fmt.Errorf("fakegcs: write %s/%s's first byte: %w", bucket, name, err)
	}
	return f.Close()
}

// install makes the received bytes the live object bucket/name, where its precondition holds for
// the object live now (errPrecondition otherwise): it renames the temporary file into objectsDir
// under the object's new generation and puts the object in the index in one step under s.mu, so a
// list, a metadata read or a download sees the object whole or not at all. The object it replaced
// loses its file; a download already reading it keeps reading the file it opened.
func (s *Server) install(attrs uploadAttrs, got received) (*object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := objectKey{attrs.bucket, attrs.name}
	old := s.objects[key]
	if !attrs.cond.holds(old) {
		return nil, errPrecondition
	}
	now := s.now()
	generation := max(now.UnixMicro(), s.lastGeneration+1)
	s.lastGeneration = generation
	path := filepath.Join(s.root, objectsDir, strconv.FormatInt(generation, 10))
	if err := os.Rename(got.path, path); err != nil {
		return nil, fmt.Errorf("fakegcs: move the upload of %s/%s into place: %w", attrs.bucket, attrs.name, err)
	}
	o := &object{
		bucket:      attrs.bucket,
		name:        attrs.name,
		generation:  generation,
		contentType: attrs.contentType,
		metadata:    attrs.metadata,
		size:        got.size,
		md5:         got.md5,
		crc32c:      got.crc32c,
		created:     now,
		path:        path,
	}
	s.objects[key] = o
	if old != nil {
		_ = os.Remove(old.path)
	}
	return o, nil
}

// storageError is Cloud Storage's JSON error answer, as testdata/object-not-found.json and
// object-precondition.json hold it.
type storageError struct {
	Error storageErrorBody `json:"error"`
}

// storageErrorBody is a storageError's one field: the status, the message, and the one error item
// Cloud Storage lists.
type storageErrorBody struct {
	Code    int                `json:"code"`
	Message string             `json:"message"`
	Errors  []storageErrorItem `json:"errors"`
}

// storageErrorItem is one error item of a storageError: its message, its domain (always
// "global"), its reason, and, for a refused header, where (LocationType "header", Location its
// name).
type storageErrorItem struct {
	Message      string `json:"message"`
	Domain       string `json:"domain"`
	Reason       string `json:"reason"`
	LocationType string `json:"locationType,omitempty"`
	Location     string `json:"location,omitempty"`
}

// conditionNotMet is Cloud Storage's refusal of a generation precondition that does not hold,
// word for word as testdata/object-precondition.json holds it.
var conditionNotMet = storageErrorItem{
	Message:      "At least one of the pre-conditions you specified did not hold.",
	Reason:       "conditionNotMet",
	LocationType: "header",
	Location:     "If-Match",
}

// noSuchObject is Cloud Storage's answer for an object that does not exist, as
// testdata/object-not-found.json holds it.
func noSuchObject(bucket, name string) storageErrorItem {
	return storageErrorItem{Message: "No such object: " + bucket + "/" + name, Reason: "notFound"}
}

// invalid is Cloud Storage's refusal of a request it cannot take, with message saying why.
func invalid(message string) storageErrorItem {
	return storageErrorItem{Message: message, Reason: "invalid"}
}

// injected is the error item of a request FailUpload or FailDelete made fail with status.
func injected(status int) storageErrorItem {
	return storageErrorItem{Message: fmt.Sprintf("fakegcs: a test made this request fail with %d %s", status, http.StatusText(status)), Reason: "injected"}
}

// writeStorageError answers status with Cloud Storage's JSON error holding item, whose message is
// the error's message too, and whose domain is "global".
func writeStorageError(w http.ResponseWriter, status int, item storageErrorItem) {
	item.Domain = "global"
	writeJSON(w, status, storageError{Error: storageErrorBody{Code: status, Message: item.Message, Errors: []storageErrorItem{item}}})
}

// storageNotFound answers a Storage path of no shape the double serves.
func storageNotFound(w http.ResponseWriter, _ *http.Request) {
	writeStorageError(w, http.StatusNotFound, storageErrorItem{Message: "Not Found", Reason: "notFound"})
}

// storageMethodNotAllowed answers a Storage path the double serves, asked with a method it does not
// serve there.
func storageMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	writeStorageError(w, http.StatusMethodNotAllowed, storageErrorItem{Message: "fakegcs does not serve " + r.Method + " " + r.URL.Path, Reason: "methodNotAllowed"})
}
