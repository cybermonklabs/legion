package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/projects/{key}/architecture-syncs", Auth: authAny, Versions: onlyV2,
		Description: "Import a project's architecture sources now {repos?}: repos names the sources to import, each " +
			"owner/name (400 SOURCE_INPUT for one that is not), and absent or [] imports every source of the project. " +
			"Each source is imported whatever another's import did, and each failure - an access failure, a " +
			"credential or branch problem, included - is on its own row's last_error, the previous projection kept " +
			"up. 404 SOURCE_NOT_FOUND answers three cases: a named source the project has no source from, naming it, " +
			"before any is imported; a named source removed while the sync runs, naming it, once the sources before " +
			"it are imported; and absent or [] on a project with none. An import stores the source's sync " +
			"bookkeeping and, on a change, its " +
			"components, and appends architecture.synced or architecture.sync_failed only when the source's state " +
			"changes. Answers 200 {items, total, limit, offset}, every source imported in the order they were added: " +
			architectureSourceText + ". 404 PROJECT_NOT_FOUND for a project that does not exist.",
		Handler:  s.syncArchitectureV2,
		Resource: "architecture-syncs", Kind: kindCollection,
		Body:  requestBody{Kind: bodyJSON, Type: architectureSyncV2Input{}, Optional: true},
		Shape: routeShape{Status: http.StatusOK, Response: page[model.ArchitectureSource]{}},
		Scope: scopeProject, ProjectOf: projectPathProject,
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, projectMissingRefusal, sourceInputRefusal,
			architectureSourceMissingRefusal, projectHasNoSourceRefusal},
	}
})
