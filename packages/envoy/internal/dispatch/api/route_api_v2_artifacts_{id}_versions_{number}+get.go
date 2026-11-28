package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/artifacts/{id}/versions/{number}", Auth: authAny, Versions: onlyV2,
		Description: "One version of a document, a file or an image, of any owner. A document version answers as JSON " +
			"{markdown, number, named, summary, authors, created_at, size, mime?, sha256}; a file or an image version answers " +
			"its bytes under its own content type, with Content-Disposition: attachment naming the artifact's name, " +
			"-v<number> before the extension for an older version (report-v2.pdf), and Cache-Control: private, " +
			"max-age=31536000, immutable. download=true answers every kind with Content-Disposition: attachment under that " +
			"name and Cache-Control: private, no-cache, and a document as its Markdown, text/markdown, its name ending in " +
			".md when the stored name has no Markdown extension. A version's author carries proven: true only when the " +
			"credential of every write that author made to the version proved it. include=blocks adds to a document " +
			"version's JSON that version's typed blocks, each under the id its opening line writes, with byte ranges into " +
			"the markdown of the same answer: the version as this build renders it. 404 VERSION_NOT_FOUND for a number " +
			"that names no version of it, 404 ARTIFACT_NOT_FOUND.",
		Handler:  s.getArtifactVersionV2,
		Resource: "versions", Kind: kindBytes, Key: "{number}",
		Query: artifactVersionV2Query{},
		Shape: routeShape{
			Status: http.StatusOK, Response: artifactVersionText{},
			Opaque: "a file version answers that file's bytes under its own content type, named by Content-Disposition (serveFileVersion); with download=true a document version answers its Markdown as text/markdown; only a document version read without that parameter carries the JSON above",
			// A file's own content type is any type: `*/*`, the media range OpenAPI keys such
			// an answer by.
			OpaqueContentTypes: []string{"*/*", "text/markdown"},
		},
		IDs: v2DocumentIDs(),
		// include=blocks parses the version (docs.VersionBlocks), and writeHandlerError names
		// its refusal: an element a newer build wrote, or another tree outside the schema. The
		// route never reaches the document service, so it declares neither of the other two
		// refusals of documentRefusals.
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, documentMissingRefusal, versionMissingRefusal, fileStoreDownRefusal,
			documentNewerContentRefusal, documentOutsideSchemaRefusal,
			{Condition: apicode.FileBytesMissing, Code: apicode.FileMissing},
			{Condition: apicode.FileStoreNotConfigured, Code: apicode.FileStoreUnconfigured},
		},
	}
})
