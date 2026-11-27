package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/markdown/file-round-trip",
	Auth:        authAny,
	Description: "The markdown Dispatch would store for {file}, base64, the bytes of a file an upload sends as its file part and Dispatch stores as a markdown document: the checks, the parse and the render the upload of that file makes of its bytes alone. The route writes nothing and reads no row. A body naming no file asks about an empty file. It refuses and answers as POST /api/v1/markdown/round-trip does, {new_document} and the response header included, except that the bytes are checked as a file's, in the words the upload refuses its file with: 400 NUL_CHARACTER for a file holding U+0000 and 400 INVALID_UTF8 for one holding a byte that is not UTF-8. A Dispatch without this route answers 404 or 405, which a caller reads as a Dispatch that cannot check the file.",
	Handler:     (*server).postFileRoundTrip,
	Successor:   successorIs(http.MethodPost, "/markdown-file-round-trips"),
	// The file round trip writes nothing and reads no row: it answers what a document write would
	// store for the file's bytes it is given.
	WriteClass: scopeNoWrite,
	Shape: routeShape{
		Status: http.StatusOK, Response: markdownRoundTripResponse{}, Request: fileRoundTripInput{},
	},
})
