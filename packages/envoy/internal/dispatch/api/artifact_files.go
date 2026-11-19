package api

import (
	"cmp"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
)

// The file store's touchpoints on the artifact routes: an upload's object write before the
// transaction (putUploadedFile), whether its row keeps the bytes too (fleetServesFilesFromRows),
// and a version's download (serveFileVersion). storeArtifact and getArtifactVersion
// (artifacts.go) call them; the rows they read and write are theirs.

// putUploadedFile writes an upload's bytes to the file store under sha before storeArtifact opens
// its transaction, and answers the request itself when the upload is refused or the store refuses
// it. With no store, or for a document, it writes nothing. It reports whether the upload may go on.
//
// The object goes first: the bytes (up to maxArtifactBlobSize) cross the network to a store whose
// wait nothing here bounds, and the transaction takes a pooled connection and the owner's lock,
// which every other writer of that issue queues behind. So the refusals that transaction makes
// are made first, from the pool and locking nothing (refuseUploadBeforeItsObject; an issue that
// is closed or missing is refused before the body is read, refuseUploadToAClosedIssue): nothing
// deletes an object, and one a refused upload left would be named by no row. Deleting it on
// refusal instead is not safe, since the store is keyed by the bytes' hash and another version,
// or another upload in flight, can name the same object. The transaction's own checks still
// decide: an upload this check passes that a concurrent write then makes refusable (another
// upload taking its name with another kind, the issue closing), or whose insert fails, leaves an
// object no row names, which a later upload of the same bytes reuses. The row records the hash
// the object is keyed by, and the bytes too only while a live server process has no bucket
// (fleetServesFilesFromRows).
func (s *server) putUploadedFile(w http.ResponseWriter, r *http.Request, kind, sha string, input artifactUploadInput, target artifactTarget) bool {
	if kind == "doc" || s.deps.Files == nil {
		return true
	}
	if err := s.refuseUploadBeforeItsObject(r.Context(), target, input.name, kind); err != nil {
		s.writeHandlerError(w, err)
		return false
	}
	if err := s.deps.Files.Put(r.Context(), sha, input.contentType, input.content); err != nil {
		slog.Error("dispatch: store an uploaded file", "sha256", sha, "size", len(input.content), "error", err)
		writeError(w, "FILE_STORE_UNAVAILABLE", http.StatusBadGateway, "the file store did not accept the upload")
		return false
	}
	return true
}

// refuseUploadBeforeItsObject answers, from the pool and locking nothing, the refusal
// storeArtifact's transaction would make of a file or image upload to target under name: a
// project that does not exist, an artifact the name or slug finds whose kind is not kind, or a new
// artifact whose name or reference key is past its bound. It reads as that transaction reads, so
// the two refuse alike whenever nothing writes the owner's artifacts between them.
func (s *server) refuseUploadBeforeItsObject(ctx context.Context, target artifactTarget, name, kind string) error {
	pool := s.deps.Store.Pool
	if target.IssueKey == nil && target.Session == "" {
		if _, err := s.loadProject(ctx, pool, target.Project); err != nil {
			return err
		}
	}
	artifact, err := lookupArtifactByNameOrSlug(ctx, pool, target, name)
	if err == nil {
		if artifact.Kind != kind {
			return artifactKindMismatch(name, artifact, kind)
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if refusal := newArtifactNameRefusal(name); refusal != nil {
		return refusal
	}
	slug, err := s.nextArtifactSlug(ctx, pool, target, name)
	if err != nil {
		return err
	}
	return newArtifactKeyRefusal(target, slug)
}

// fleetServesFilesFromRows reports whether any live server process (processes.Bucketless, read in
// the upload's transaction) has no bucket: one started without DISPATCH_FILE_STORE_BUCKET, or one
// of a build that predates the fleet columns (PredatesFleetColumns), which registers no file_store
// and so reads as having none. Such a process serves a file only from its row, and answers 503
// FILE_STORE_UNAVAILABLE for a row that holds no bytes, so while one is live an upload keeps its
// bytes in its row as well as in the bucket. backfill-files moves them out once every process has
// a bucket.
func fleetServesFilesFromRows(ctx context.Context, db processes.Querier) (bool, error) {
	bucketless, err := processes.Bucketless(ctx, db)
	if err != nil {
		return false, err
	}
	return len(bucketless) > 0, nil
}

const (
	// versionCacheControl lets a browser keep a version's bytes, which never change, for a year.
	versionCacheControl = "private, max-age=31536000, immutable"
	// downloadCacheControl makes a browser ask again, so the saved name is the current one.
	downloadCacheControl = "private, no-cache"
)

// serveFileVersion answers a file version's download from what its row holds: a row holding
// bytes is served from the row, whatever store is configured; a row the backfill cleared, or that
// an upload wrote with a store, names its object by hash and is streamed from the store.
//
// The headers wait for the object to open, so a store that fails answers 502 and never a 200 cut
// short; a body that fails after that, or does not hash to the row's hash, aborts the response
// mid-stream rather than ending it as if whole.
//
// The answer is named filename (versionFilename, attachmentDisposition), so a saved file keeps
// the artifact's name rather than the version number the address ends in. A version's bytes
// never change, so at the version address a browser keeps them for a year without asking again:
// the pictures a conversation shows inline are fetched once. The download address (download,
// from `?download=true`) answers `no-cache` instead, so a browser asks again before each use of a
// copy it holds and saves under the name of the moment: an older version's name gains its
// `-v<number>` once a newer version arrives, which the year-long cache would hide. `private`
// either way, since the route is authenticated and no shared cache may hold them. A document
// version is not served here (getArtifactVersion).
func (s *server) serveFileVersion(w http.ResponseWriter, r *http.Request, artifactID, filename string, number int, download bool, content []byte, contentType, sha *string, size *int64) {
	headers := func(length int64) {
		w.Header().Set("Content-Type", cmp.Or(deref(contentType), "application/octet-stream"))
		if sha != nil {
			w.Header().Set("ETag", *sha)
		}
		if download {
			w.Header().Set("Cache-Control", downloadCacheControl)
		} else {
			w.Header().Set("Cache-Control", versionCacheControl)
		}
		w.Header().Set("Content-Disposition", attachmentDisposition(filename))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusOK)
	}
	if content != nil {
		headers(int64(len(content)))
		_, _ = w.Write(content)
		return
	}
	if s.deps.Files == nil {
		slog.Error("dispatch: a file version holds no bytes and no file store is configured", "artifact", artifactID, "version", number, "sha256", deref(sha))
		writeError(w, "FILE_STORE_UNAVAILABLE", http.StatusServiceUnavailable, "this file's bytes are in a store this server is not configured to read")
		return
	}
	if sha == nil {
		slog.Error("dispatch: a file version holds no bytes and records no hash to read them by", "artifact", artifactID, "version", number)
		writeError(w, "FILE_MISSING", http.StatusInternalServerError, "this file's bytes cannot be found")
		return
	}
	object, err := s.deps.Files.Get(r.Context(), *sha)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) {
			// The row names an object the bucket does not hold: not an outage a retry mends, a
			// restore from the bucket's versioning.
			slog.Error("dispatch: a file version's object is missing from the file store", "artifact", artifactID, "version", number, "sha256", *sha, "error", err)
			writeError(w, "FILE_MISSING", http.StatusInternalServerError, "this file's bytes cannot be found")
			return
		}
		slog.Error("dispatch: read a file version from the file store", "artifact", artifactID, "version", number, "sha256", *sha, "error", err)
		writeError(w, "FILE_STORE_UNAVAILABLE", http.StatusBadGateway, "the file store did not return this file")
		return
	}
	defer object.Body.Close()
	// The row's recorded size is the file's length; an object of another length is not the file,
	// however it got there, and is refused before a header is written. The body's reader checks
	// the hash as the bytes go and holds the last of them back until it has, so a wrong body
	// leaves the client short of Content-Length rather than whole.
	if size != nil && object.Size != *size {
		slog.Error("dispatch: a file version's object is not the size the row records", "artifact", artifactID, "version", number, "sha256", *sha, "row_size", *size, "object_size", object.Size)
		writeError(w, "FILE_MISSING", http.StatusInternalServerError, "this file's bytes cannot be found")
		return
	}
	headers(object.Size)
	if _, err := io.Copy(w, object.Body); err != nil {
		if r.Context().Err() != nil {
			return // The client went away; nothing to tell it.
		}
		slog.Error("dispatch: stream a file version from the file store", "artifact", artifactID, "version", number, "sha256", *sha, "error", err)
		panic(http.ErrAbortHandler)
	}
}

// versionFilename is the name a version saves under, before safeFilename makes it safe for the
// header: the artifact's stored name, with `.md` added to a document's name that ends in no
// Markdown extension, since a document downloads as its Markdown, and with `-v<number>` before
// the extension of a version older than latest, so it does not take the latest version's name in
// a folder. The extension starts at the last `.` after the first byte, so `.env` and `Makefile`
// take the suffix at their end.
func versionFilename(name, kind string, number, latest int) string {
	hasExtension := func(extension string) bool {
		return len(name) >= len(extension) && strings.EqualFold(name[len(name)-len(extension):], extension)
	}
	if kind == "doc" && !hasExtension(".md") && !hasExtension(".markdown") {
		name += ".md"
	}
	if number >= latest {
		return name
	}
	suffix := "-v" + strconv.Itoa(number)
	if dot := strings.LastIndexByte(name, '.'); dot > 0 {
		return name[:dot] + suffix + name[dot:]
	}
	return name + suffix
}

// attachmentDisposition is the Content-Disposition of an answer saved as filename. The name goes
// in twice, as RFC 6266 says: `filename*`, RFC 8187's encoding of the whole UTF-8 name, which a
// browser prefers, and `filename`, an ASCII form for a client that reads only that parameter.
// curl -OJ is one: given `filename*` alone it saves under the address's last segment, which is
// the version number. Written by hand because mime.FormatMediaType writes `filename*` alone for
// a name that needs encoding.
func attachmentDisposition(filename string) string {
	safe := safeFilename(filename)
	return `attachment; filename="` + plainFilename(safe) + `"; filename*=UTF-8''` + percentEncodeFilename(safe)
}

// safeFilename is name with each path separator (`/`, `\`), control character (Cc) and
// bidirectional control (Bidi_Control) written as `_`: a separator would name a folder, and a
// bidirectional control can draw the name's extension in a false order, so `photo` U+202E
// `gpj.exe` shows as `photoexe.jpg`, a program that reads as a picture. Every other code point
// stays, the zero-width joiner inside an emoji included.
func safeFilename(name string) string {
	return strings.Map(func(char rune) rune {
		if char == '/' || char == '\\' || unicode.Is(unicode.Cc, char) || unicode.Is(unicode.Bidi_Control, char) {
			return '_'
		}
		return char
	}, name)
}

// plainFilename is name as the text inside the quotes of a `filename` parameter: each code point
// outside printable ASCII, each `"` and each `\` written as one `_`. A browser reads `filename*`
// and never sees this form. The quote and the backslash are not escaped with a backslash, as
// RFC 9110's quoted-string allows, because curl -OJ saves an answer whose `filename` holds `\"`
// under the address's last segment rather than under the name.
func plainFilename(name string) string {
	return strings.Map(func(char rune) rune {
		if char < 0x20 || char > 0x7e || char == '"' || char == '\\' {
			return '_'
		}
		return char
	}, name)
}

// percentEncodeFilename is name as a `filename*` parameter's value, the text after its charset
// and language: every byte outside RFC 8187's attr-char set percent-encoded. Hand-rolled because
// the standard library's escapes are for URLs, not for this: url.PathEscape leaves a colon, an
// equals sign and an at sign through, which that set does not hold and RFC 8187 allows only
// percent-encoded, and url.QueryEscape writes a space as a plus sign, which a client reads as a
// plus sign.
func percentEncodeFilename(name string) string {
	const hexDigits = "0123456789ABCDEF"
	var encoded strings.Builder
	encoded.Grow(len(name))
	for _, char := range []byte(name) {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9',
			strings.IndexByte("!#$&+-.^_`|~", char) >= 0:
			encoded.WriteByte(char)
		default:
			encoded.WriteByte('%')
			encoded.WriteByte(hexDigits[char>>4])
			encoded.WriteByte(hexDigits[char&0xf])
		}
	}
	return encoded.String()
}
