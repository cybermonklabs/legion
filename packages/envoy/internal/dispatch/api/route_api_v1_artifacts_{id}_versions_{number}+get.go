package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/versions/{number}",
	Auth:        authAny,
	Description: "One version of a document, a file or an image. A named or settled document version answers as JSON; a file or an image version answers its bytes under its own content type, with Content-Disposition: attachment naming the artifact's name, -v<number> before the extension for an older version (report-v2.pdf), and Cache-Control: private, max-age=31536000, immutable. With ?download=true every kind answers Content-Disposition: attachment with that name and Cache-Control: private, no-cache, and a document answers its Markdown as text/markdown, its name ending in .md when the stored name has no Markdown extension. {number} is a version from 1 to 2147483647 (the largest the store holds), and any other is 400 INVALID_VERSION. One renderer answers a document's version, JSON and download alike, and names what its bytes are in the Dispatch-Render-Source header: tree for a version the server rendered from the issue's tree, rendered again from the tree as of that version's time; stored for every other version, its stored markdown byte for byte; none for a version that stores no markdown, whose answer holds no bytes and is no document. A version's size and sha256 are those of the bytes answered. A document's version also answers digest, what an approval of it covers and a review's content_sha256 pins (artifact_version_text_sha256): the digest of its query's record where the pass that stated the version recorded one, and otherwise the sha256 of its stored markdown, which a version of a document no query covers always answers; so an approval is compared with digest, not with sha256. A version's author carries proven: true only when the credential of every write that author made to the version proved it. With ?blocks=true a document version's JSON carries that version's typed blocks, each under the id its opening line writes, with byte ranges into the markdown of the same answer: the version as this build renders it.",
	Handler:     (*server).getArtifactVersion,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: artifactVersionText{}, Params: artifactVersionQuery{},
		Opaque:             "a file version answers that file's bytes under its own content type, named by Content-Disposition (serveFileVersion); with download=true a document version answers its Markdown as text/markdown; only a document version read without that parameter carries the JSON above",
		OpaqueContentTypes: []string{"application/octet-stream", "text/markdown"},
	},
})
