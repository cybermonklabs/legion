package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/markdown-file-round-trips", Auth: authAny, Versions: onlyV2,
		Description: fmt.Sprintf("The markdown Dispatch would store for {file}, base64, the bytes of a file an upload "+
			"sends as its file part and Dispatch stores as a markdown document: the checks, the parse and the render "+
			"the upload of that file makes of its bytes alone, answered 200 {markdown}. The route writes nothing and "+
			"reads no row. A body naming no file asks about an empty file. It refuses and answers as POST "+
			"/api/v2/markdown-round-trips does, new_document and the response header %s included, except that the "+
			"bytes are checked as a file's, in the words the upload refuses its file with: 400 NUL_CHARACTER for a "+
			"file holding U+0000 and 400 INVALID_UTF8 for one holding a byte that is not UTF-8. The body may hold %d "+
			"bytes, the largest file one document holds as base64; past that it is 413 REQUEST_TOO_LARGE, which "+
			"carries no such header.",
			contracts.TextRefusalHeader, fileRoundTripLimit),
		Handler:  s.postFileRoundTripV2,
		Resource: "markdown-file-round-trips", Kind: kindComputation,
		Body:      requestBody{Kind: bodyJSON, Type: fileRoundTripInput{}, TextChecked: true},
		Shape:     routeShape{Status: http.StatusOK, Response: markdownRoundTripResponse{}},
		Scope:     scopeNoWrite,
		BodyLimit: fileRoundTripBodyLimit,
		Refusals:  markdownWriteRefusals(undeclaredKeyRefusal),
	}
})
