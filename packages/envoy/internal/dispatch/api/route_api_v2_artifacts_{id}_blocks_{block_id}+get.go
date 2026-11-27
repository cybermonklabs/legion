package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/artifacts/{id}/blocks/{block_id}", Auth: authAny, Versions: onlyV2,
		Description: "Where one block stands in the live document: its path from the top-level block down (type, id, " +
			"child index), and for a table block, row or cell the table's id, the row index (0 is the header), the cell's " +
			"column index, the text of the header cell drawn above it and the row's cells. block_id is the block's whole id " +
			"or a unique prefix of 8 characters or more. 404 BLOCK_NOT_FOUND for a block the live document does not hold, " +
			"400 NOT_DOCUMENT for a file or an image, 404 ARTIFACT_NOT_FOUND.",
		Handler:  s.getArtifactBlockPathV2,
		Resource: "blocks", Kind: kindItem, Key: "{block_id}",
		Shape: routeShape{Status: http.StatusOK, Response: model.BlockPath{}},
		IDs: &idRoute{
			Kind: "artifact", Blocks: true, NotFound: apicode.ArtifactNotFound, BlockNotFound: apicode.BlockNotFound,
		},
		Refusals: documentRefusals(undeclaredKeyRefusal, documentMissingRefusal, blockMissingRefusal, artifactNotDocumentRefusal),
	}
})
