package fakegcs

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// maxResourceBytes bounds the JSON object resource an upload carries: its name, content type,
// checksums and custom metadata, a few hundred bytes from the cache.
const maxResourceBytes = 1 << 20

// uploadResource is the part of the JSON object resource an upload carries that the double keeps
// or checks: the name, the content type, the custom metadata, and the CRC32C and MD5 the client
// computed, which the double checks against the bytes it received. It ignores the rest.
type uploadResource struct {
	Name        string            `json:"name"`
	ContentType string            `json:"contentType"`
	Metadata    map[string]string `json:"metadata"`
	CRC32C      string            `json:"crc32c"`
	MD5Hash     string            `json:"md5Hash"`
}

// uploadAttrs is what one upload says of the object it makes: where, its content type and custom
// metadata, the checksums it states (Cloud Storage's base64, empty where it states none), and its
// ifGenerationMatch.
type uploadAttrs struct {
	bucket       string
	name         string
	contentType  string
	metadata     map[string]string
	statedCRC32C string
	statedMD5    string
	cond         precondition
}

// received is an upload's bytes as the double holds them before they become an object: the
// temporary file under Options.Root/uploads, its size, and the MD5 and CRC32C of its bytes.
type received struct {
	path   string
	size   int64
	md5    []byte
	crc32c uint32
}

// session is one resumable upload: what its start said (attrs), the temporary file its chunks go
// to, how many bytes that file holds (offset), and, once it completed, the object it made (done),
// or that it ended without one (ended). mu keeps one request of the session at a time.
type session struct {
	mu     sync.Mutex
	attrs  uploadAttrs
	path   string
	offset int64
	done   *object
	ended  bool
}

// readUploadAttrs reads an upload's object from its query and its JSON resource: the name from
// the query's name or else the resource's (400 where both are given and differ, or the name is none
// Cloud Storage takes), and the content type from the resource, or else from mediaType, the type
// the media part or X-Upload-Content-Type names, or else Cloud Storage's default.
func readUploadAttrs(w http.ResponseWriter, bucket string, query url.Values, res uploadResource, mediaType string, cond precondition) (uploadAttrs, bool) {
	name := query.Get("name")
	if name == "" {
		name = res.Name
	} else if res.Name != "" && res.Name != name {
		writeStorageError(w, http.StatusBadRequest, invalid(fmt.Sprintf("The name parameter %q and the resource's name %q differ", name, res.Name)))
		return uploadAttrs{}, false
	}
	if !validName(name) {
		writeStorageError(w, http.StatusBadRequest, invalid(fmt.Sprintf("Invalid object name: %q", name)))
		return uploadAttrs{}, false
	}
	contentType := res.ContentType
	if contentType == "" {
		contentType = mediaType
	}
	if contentType == "" {
		contentType = defaultContentType
	}
	return uploadAttrs{
		bucket:       bucket,
		name:         name,
		contentType:  contentType,
		metadata:     res.Metadata,
		statedCRC32C: res.CRC32C,
		statedMD5:    res.MD5Hash,
		cond:         cond,
	}, true
}

// receive copies src to a new temporary file under Options.Root/uploads, computing its MD5 and
// CRC32C on the way, so no upload is ever held in memory.
func (s *Server) receive(src io.Reader) (received, error) {
	f, err := os.CreateTemp(filepath.Join(s.root, uploadsDir), "multipart-*")
	if err != nil {
		return received{}, fmt.Errorf("fakegcs: make an upload's temporary file: %w", err)
	}
	sumMD5 := md5.New()
	sumCRC := crc32.New(crc32cTable)
	size, err := io.Copy(io.MultiWriter(f, sumMD5, sumCRC), src)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return received{}, fmt.Errorf("fakegcs: receive an upload's bytes: %w", err)
	}
	return received{path: f.Name(), size: size, md5: sumMD5.Sum(nil), crc32c: sumCRC.Sum32()}, nil
}

// hashFile reads the bytes a resumable upload's chunks wrote to path, once they are all there, for
// their size, MD5 and CRC32C.
func hashFile(path string) (received, error) {
	f, err := os.Open(path)
	if err != nil {
		return received{}, fmt.Errorf("fakegcs: open an upload's bytes: %w", err)
	}
	defer f.Close()
	sumMD5 := md5.New()
	sumCRC := crc32.New(crc32cTable)
	size, err := io.Copy(io.MultiWriter(sumMD5, sumCRC), f)
	if err != nil {
		return received{}, fmt.Errorf("fakegcs: read an upload's bytes: %w", err)
	}
	return received{path: path, size: size, md5: sumMD5.Sum(nil), crc32c: sumCRC.Sum32()}, nil
}

// multipartUpload takes an upload of one request (uploadType=multipart): a multipart/related body
// of the JSON object resource, then the object's bytes, which go to a temporary file. The upload
// FailUpload armed for this request answers its status before the body is read.
func (s *Server) multipartUpload(w http.ResponseWriter, r *http.Request, bucket string) {
	query := r.URL.Query()
	cond, ok := readPrecondition(w, query)
	if !ok {
		return
	}
	s.mu.Lock()
	status := nextFault(&s.completions, &s.uploadFaults)
	s.mu.Unlock()
	if status != 0 {
		writeStorageError(w, status, injected(status))
		return
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/related" || params["boundary"] == "" {
		writeStorageError(w, http.StatusBadRequest, invalid(fmt.Sprintf("A multipart upload's Content-Type must be multipart/related with a boundary, not %q", r.Header.Get("Content-Type"))))
		return
	}
	parts := multipart.NewReader(r.Body, params["boundary"])
	metadataPart, err := parts.NextRawPart()
	if err != nil {
		writeStorageError(w, http.StatusBadRequest, invalid("A multipart upload's first part, the object resource, is missing: "+err.Error()))
		return
	}
	var res uploadResource
	if err := json.NewDecoder(io.LimitReader(metadataPart, maxResourceBytes)).Decode(&res); err != nil {
		writeStorageError(w, http.StatusBadRequest, invalid("A multipart upload's object resource is not JSON: "+err.Error()))
		return
	}
	mediaPart, err := parts.NextRawPart()
	if err != nil {
		writeStorageError(w, http.StatusBadRequest, invalid("A multipart upload's second part, the object's bytes, is missing: "+err.Error()))
		return
	}
	attrs, ok := readUploadAttrs(w, bucket, query, res, mediaPart.Header.Get("Content-Type"), cond)
	if !ok {
		return
	}
	got, err := s.receive(mediaPart)
	if err != nil {
		writeStorageError(w, http.StatusBadRequest, invalid(err.Error()))
		return
	}
	if _, err := parts.NextRawPart(); !errors.Is(err, io.EOF) {
		_ = os.Remove(got.path)
		writeStorageError(w, http.StatusBadRequest, invalid("A multipart upload holds two parts, the object resource and the object's bytes, and nothing after them"))
		return
	}
	s.complete(w, r, attrs, got)
}

// startResumable starts a resumable upload (uploadType=resumable): it reads the JSON object
// resource, if any, refuses with 412 an ifGenerationMatch that does not hold already, and answers
// 200 with the session's address in Location, a Cloud Storage upload address carrying upload_id,
// which the client's chunks then go to.
func (s *Server) startResumable(w http.ResponseWriter, r *http.Request, bucket string) {
	query := r.URL.Query()
	cond, ok := readPrecondition(w, query)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxResourceBytes))
	if err != nil {
		writeStorageError(w, http.StatusBadRequest, invalid("Read the object resource: "+err.Error()))
		return
	}
	var res uploadResource
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &res); err != nil {
			writeStorageError(w, http.StatusBadRequest, invalid("A resumable upload's object resource is not JSON: "+err.Error()))
			return
		}
	}
	attrs, ok := readUploadAttrs(w, bucket, query, res, r.Header.Get("X-Upload-Content-Type"), cond)
	if !ok {
		return
	}
	f, err := os.CreateTemp(filepath.Join(s.root, uploadsDir), "resumable-*")
	if err != nil {
		writeStorageError(w, http.StatusInternalServerError, storageErrorItem{Message: "fakegcs: make the upload's temporary file: " + err.Error(), Reason: "backendError"})
		return
	}
	_ = f.Close()
	id := rand.Text()
	s.mu.Lock()
	if !cond.holds(s.objects[objectKey{bucket, attrs.name}]) {
		s.mu.Unlock()
		_ = os.Remove(f.Name())
		writeStorageError(w, http.StatusPreconditionFailed, conditionNotMet)
		return
	}
	s.sessions[id] = &session{attrs: attrs, path: f.Name()}
	s.mu.Unlock()
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	location := url.URL{
		Scheme:   scheme,
		Host:     r.Host,
		Path:     "/upload/storage/v1/b/" + bucket + "/o",
		RawQuery: url.Values{"uploadType": {"resumable"}, "name": {attrs.name}, "upload_id": {id}}.Encode(),
	}
	w.Header().Set("Location", location.String())
	w.Header().Set("X-GUploader-UploadID", id)
	w.WriteHeader(http.StatusOK)
}

// contentRange is a resumable upload request's Content-Range: the bytes it carries, first to last
// (first -1 for a request carrying none, "bytes */<total>" or "bytes */*"), and the object's total
// size, -1 where the request does not know it yet.
type contentRange struct {
	first, last, total int64
}

// parseContentRange reads a Content-Range of the four forms a resumable upload's requests send:
// "bytes <first>-<last>/*", "bytes <first>-<last>/<total>", "bytes */<total>" and "bytes */*".
func parseContentRange(header string) (contentRange, error) {
	spec, ok := strings.CutPrefix(header, "bytes ")
	if !ok {
		return contentRange{}, fmt.Errorf("the Content-Range %q does not start with %q", header, "bytes ")
	}
	span, total, ok := strings.Cut(spec, "/")
	if !ok {
		return contentRange{}, fmt.Errorf("the Content-Range %q names no total", header)
	}
	cr := contentRange{first: -1, last: -1, total: -1}
	if total != "*" {
		n, err := strconv.ParseInt(total, 10, 64)
		if err != nil || n < 0 {
			return contentRange{}, fmt.Errorf("the Content-Range %q names no total size", header)
		}
		cr.total = n
	}
	if span == "*" {
		return cr, nil
	}
	first, last, ok := strings.Cut(span, "-")
	if !ok {
		return contentRange{}, fmt.Errorf("the Content-Range %q names no byte range", header)
	}
	var err1, err2 error
	cr.first, err1 = strconv.ParseInt(first, 10, 64)
	cr.last, err2 = strconv.ParseInt(last, 10, 64)
	if err1 != nil || err2 != nil || cr.first < 0 || cr.last < cr.first || cr.total >= 0 && cr.last >= cr.total {
		return contentRange{}, fmt.Errorf("the Content-Range %q names no byte range of the object", header)
	}
	return cr, nil
}

// uploadChunk takes one later request of a resumable upload: a chunk of bytes, a chunk that names
// the object's total size (the last), or a query of how far the upload got. A chunk that starts
// past the bytes the session holds is refused; one that starts before them, a retry of a chunk the
// double kept, has its bytes already held skipped. While bytes are missing it answers that the
// upload is incomplete, with the bytes it holds in Range: 308, or, to a client that sent
// X-GUploader-No-308: yes, as the Go client does, 200 with X-Http-Status-Code-Override: 308. Once
// the total is there it completes the upload and answers the object; a completed session answers
// its object again.
func (s *Server) uploadChunk(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	sess := s.sessions[r.URL.Query().Get("upload_id")]
	s.mu.Unlock()
	if sess == nil {
		writeStorageError(w, http.StatusNotFound, storageErrorItem{Message: "No such upload session", Reason: "notFound"})
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	switch {
	case sess.done != nil:
		writeJSON(w, http.StatusOK, sess.done.resource())
		return
	case sess.ended:
		writeStorageError(w, http.StatusGone, storageErrorItem{Message: "The upload session ended without an object", Reason: "gone"})
		return
	}
	cr, err := parseContentRange(r.Header.Get("Content-Range"))
	if err != nil {
		writeStorageError(w, http.StatusBadRequest, invalid(err.Error()))
		return
	}
	if cr.total >= 0 {
		s.mu.Lock()
		status := nextFault(&s.completions, &s.uploadFaults)
		s.mu.Unlock()
		if status != 0 {
			writeStorageError(w, status, injected(status))
			return
		}
	}
	if cr.first >= 0 {
		if err := sess.write(r, cr); err != nil {
			writeStorageError(w, http.StatusBadRequest, invalid(err.Error()))
			return
		}
	}
	switch {
	case cr.total >= 0 && sess.offset > cr.total:
		writeStorageError(w, http.StatusBadRequest, invalid(fmt.Sprintf("the upload holds %d bytes, more than the total %d its request names", sess.offset, cr.total)))
		return
	case cr.total >= 0 && sess.offset == cr.total:
		got, err := hashFile(sess.path)
		if err != nil {
			writeStorageError(w, http.StatusInternalServerError, storageErrorItem{Message: err.Error(), Reason: "backendError"})
			return
		}
		// The start's resource and the last request's X-Goog-Hash may each state checksums: the
		// resource's are checked here, where the header replaces them, and the header's in complete.
		attrs := sess.attrs
		if crc, md5Sum := statedHashes(r.Header.Values("X-Goog-Hash")); crc != "" || md5Sum != "" {
			if !hashesMatch(w, sess.attrs, got) {
				sess.end()
				return
			}
			attrs.statedCRC32C, attrs.statedMD5 = crc, md5Sum
		}
		if sess.done = s.complete(w, r, attrs, got); sess.done == nil {
			sess.end()
		}
		return
	}
	if sess.offset > 0 {
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", sess.offset-1))
	}
	if r.Header.Get("X-GUploader-No-308") == "yes" {
		w.Header().Set("X-Http-Status-Code-Override", "308")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusPermanentRedirect)
}

// write appends a chunk's bytes to the session's file at the offset it holds, skipping the chunk's
// bytes before that offset, and refuses a chunk that starts past it or whose body is not as long as
// its Content-Range. On any failure it cuts the file back to the bytes it held before, so the
// session holds exactly offset bytes after every request.
func (sess *session) write(r *http.Request, cr contentRange) error {
	if cr.first > sess.offset {
		return fmt.Errorf("the chunk starts at byte %d, past the %d bytes the upload holds", cr.first, sess.offset)
	}
	length := cr.last - cr.first + 1
	if r.ContentLength >= 0 && r.ContentLength != length {
		return fmt.Errorf("the chunk's Content-Length %d is not the %d bytes its Content-Range names", r.ContentLength, length)
	}
	f, err := os.OpenFile(sess.path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("fakegcs: open the upload's bytes: %w", err)
	}
	held := min(sess.offset-cr.first, length)
	_, err = io.CopyN(io.Discard, r.Body, held)
	if err == nil {
		_, err = f.Seek(sess.offset, io.SeekStart)
	}
	if err == nil {
		_, err = io.CopyN(f, r.Body, length-held)
	}
	if err == nil {
		var past [1]byte
		if n, _ := io.ReadFull(r.Body, past[:]); n > 0 {
			err = errors.New("the chunk's body is longer than its Content-Range")
		}
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Truncate(sess.path, sess.offset)
		return fmt.Errorf("the chunk was not received whole: %w", err)
	}
	sess.offset = max(sess.offset, cr.last+1)
	return nil
}

// end marks the session over without an object, after its completion was refused or its request
// ended at a Hold, and removes its file: a later request of it answers 410. The caller holds
// sess.mu.
func (sess *session) end() {
	sess.ended = true
	_ = os.Remove(sess.path)
}

// statedHashes reads the CRC32C and the MD5 a request's X-Goog-Hash headers state, Cloud
// Storage's "crc32c=<base64>" and "md5=<base64>", in one header separated by commas or in several.
func statedHashes(headers []string) (crc, md5Sum string) {
	for _, header := range headers {
		for _, item := range strings.Split(header, ",") {
			name, value, _ := strings.Cut(strings.TrimSpace(item), "=")
			switch name {
			case "crc32c":
				crc = value
			case "md5":
				md5Sum = value
			}
		}
	}
	return crc, md5Sum
}

// hashesMatch checks the checksums the upload states against those of the bytes the double
// received, answering 400 and false, with Cloud Storage's words, where one differs.
func hashesMatch(w http.ResponseWriter, attrs uploadAttrs, got received) bool {
	if attrs.statedCRC32C != "" {
		if computed := encodeCRC32C(got.crc32c); attrs.statedCRC32C != computed {
			writeStorageError(w, http.StatusBadRequest, invalid(fmt.Sprintf("Provided CRC32C %q doesn't match calculated CRC32C %q.", attrs.statedCRC32C, computed)))
			return false
		}
	}
	if attrs.statedMD5 != "" {
		if computed := base64.StdEncoding.EncodeToString(got.md5); attrs.statedMD5 != computed {
			writeStorageError(w, http.StatusBadRequest, invalid(fmt.Sprintf("Provided MD5 hash %q doesn't match calculated MD5 hash %q.", attrs.statedMD5, computed)))
			return false
		}
	}
	return true
}

// complete makes an upload whose bytes all arrived an object, or refuses it, removing its bytes:
// 400 where a checksum it states differs from its bytes', then, after any Hold that matches it, 412
// where its ifGenerationMatch does not hold for the object live then. It answers the object and
// writes its resource, or answers nil, having written the refusal, or nothing where the request
// ended at a Hold.
func (s *Server) complete(w http.ResponseWriter, r *http.Request, attrs uploadAttrs, got received) *object {
	if !hashesMatch(w, attrs, got) {
		_ = os.Remove(got.path)
		return nil
	}
	if !s.waitAtHold(r.Context(), attrs.bucket, attrs.name) {
		_ = os.Remove(got.path)
		return nil
	}
	o, err := s.install(attrs, got)
	switch {
	case errors.Is(err, errPrecondition):
		_ = os.Remove(got.path)
		writeStorageError(w, http.StatusPreconditionFailed, conditionNotMet)
		return nil
	case err != nil:
		_ = os.Remove(got.path)
		writeStorageError(w, http.StatusInternalServerError, storageErrorItem{Message: err.Error(), Reason: "backendError"})
		return nil
	}
	writeJSON(w, http.StatusOK, o.resource())
	return o
}
