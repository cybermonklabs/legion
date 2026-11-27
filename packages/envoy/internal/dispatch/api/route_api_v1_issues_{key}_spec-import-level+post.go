package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/spec-import-level",
	Auth:        authAny,
	Description: "Import the approved level the issue holds into the approved spec of its repository, which must be an enabled spec source of its project (409 SPEC_SOURCE_REQUIRED otherwise) {notes}: the level is the issue's spec at the version of the person's newest approval of it (409 LEVEL_NOT_APPROVED where none), and notes names the level's notes document, whose kind, about and supports count only at a version a person approved: the version that approval's request names, else the version of a person's newest approval of the notes document itself. With neither, no statement takes a field from the notes. A field that a statement's note does not write is a field that the statement lacks. The import reads a field only from the first paragraph of a list item. That list must stand at the top of the note, or in an item of another such list. A field that the note writes twice, or writes in any other paragraph, is also a field that the statement lacks. The import reads no field from a code block or a heading. A Kind with no value is also a field that the statement lacks. An About or a Supports that names no id and is not none is also a field that the statement lacks. A value of commas, dots or backticks alone names no id. An About or a Supports of none is a relation to nothing. Each component and interface of the level's Components table, and each statement whose kind, about, supports and parent resolve, is written as a node with that approval as provenance; each other statement is kept as a hold with its text, its provenance and the fields it lacks, and a statement written over an open hold clears that hold. The open holds of the level that name no repair issue get one, a child issue of the issue assigned to its assignee, whose spec lists them: the child and the name of it on each hold are written in one transaction (502 REPAIR_ISSUE_NOT_OPENED where the create fails). The import answers 409 REPAIR_ISSUE_RACED when another writer names a repair issue on one of the holds, or clears a hold, during each of three opens of the child. Then run the import again. A node the approved spec already holds or held, and a statement already held, is left as it stands, so a second run writes what the first could not. The import refuses a level that the approved spec cannot hold, and writes nothing. It answers 409 SPEC_NODE_INVALID when the approved spec cannot hold a node of the level. The causes are a row of the Components table that is no component, one id on two nodes of the level, an id that is no id, an id longer than an address of the store, and a node that breaks a rule of its kind. A row is no component when it does not hold five cells or its first cell is empty. It is also no component when its second cell is not component or interface, or its last cell names no id and is not none. It answers 409 SPEC_PARENT_MISSING when a statement is under a statement of the level that the approved spec removed or holds as another kind of node. It answers 409 SPEC_LOOP when the parent and supports relations make a loop. These three refusals carry problems, one {node, rule, detail} for each fault. Answers {issue, repo, written, held, repair_issue}.",
	Handler:     (*server).importSpecLevel,
	// The import of an approved level writes the approved spec of the issue's repository in the
	// issue's project.
	WriteClass: scopeProject,
	Shape: routeShape{
		Status: http.StatusOK, Response: model.SpecLevelImport{}, Request: importLevelInput{},
	},
})
