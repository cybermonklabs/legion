package api

import (
	"log/slog"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/metrics"
)

// A whole-document upload over a document that already exists names the token of the version it
// replaces, and Dispatch checks that token under the write's own lock (docs.Service.ReplaceText).
// #3000 refused one that named none, 400 PRECONDITION_REQUIRED, but no deployed `dispatch` client
// can name a token on `dispatch artifact`, so from the deploy that carried #3000 every agent's
// replacement of a document was refused (INFRA-4980). Such a replacement is taken unchecked again,
// as it was before #3000, and each one stored is counted here and logged with the document it
// replaced. The refusal comes back in a later change, once every deployed client sends the token:
// the counter reading no new replacements is how that is seen.

// unguardedReplacementsMetric is the counter of whole-document uploads that replaced a document
// naming no token. It carries no label, since whoever reaches the port reads /metrics.
const unguardedReplacementsMetric = "envoy_dispatch_document_replacements_unguarded_total"

// unguardedReplacementMessage is the INFO line each such replacement logs once it has committed.
const unguardedReplacementMessage = "dispatch: an upload replaced a document naming no token for the version it replaces; taken unchecked until every client sends one (INFRA-4980)"

// registerUnguardedReplacements adds the counter to registry and answers it. A nil registry, as
// this package's tests run, adds none and answers nil, which counts nothing (metrics.Counter.Add).
func registerUnguardedReplacements(registry *metrics.Registry) *metrics.Counter {
	if registry == nil {
		return nil
	}
	return registry.NewCounter(unguardedReplacementsMetric,
		"Whole-document uploads that replaced an existing document naming no token for the version they replace, taken unchecked")
}

// noteUnguardedReplacement counts a committed upload that replaced artifact naming no token and
// logs the document it replaced. The line names no caller and no request header: the access log's
// line for the same POST names the caller, its kind and the User-Agent at the same moment, so the
// client that sent it is read by joining the two on the upload's path and time. A repair of a
// document outside the Proof schema names no token by design and is counted too; it is rare, and
// its line names its document.
func (s *server) noteUnguardedReplacement(artifact model.Artifact) {
	s.unguardedReplacements.Inc()
	slog.Info(unguardedReplacementMessage, "artifact", artifact.ID, "document", artifact.RefKey)
}

// A file's or a picture's upload over one that already exists names the token of the version it
// replaces in the same way, and Dispatch checks it under the owner's lock (refuseFileBase). One
// naming none is taken unchecked too, since no deployed client but this build's `dispatch` names
// one (the dashboard's upload does not), and each one stored is counted here, apart from the
// documents', so the same later refusal can cover files once every client sends a token: this
// counter reading no new replacements is how that is seen.

// unguardedFileReplacementsMetric is the counter of file and picture uploads that replaced one
// naming no token. It carries no label, for the reason the documents' counter carries none.
const unguardedFileReplacementsMetric = "envoy_dispatch_file_replacements_unguarded_total"

// unguardedFileReplacementMessage is the INFO line each such replacement logs once it has
// committed.
const unguardedFileReplacementMessage = "dispatch: an upload replaced a file naming no token for the version it replaces; taken unchecked until every client sends one"

// registerUnguardedFileReplacements adds the file counter to registry and answers it, nil for a
// nil registry, as registerUnguardedReplacements does.
func registerUnguardedFileReplacements(registry *metrics.Registry) *metrics.Counter {
	if registry == nil {
		return nil
	}
	return registry.NewCounter(unguardedFileReplacementsMetric,
		"File and picture uploads that replaced an existing file or picture naming no token for the version they replace, taken unchecked")
}

// noteUnguardedFileReplacement counts a committed upload that replaced the file or picture
// artifact naming no token and logs the artifact it replaced, naming no caller, as
// noteUnguardedReplacement does.
func (s *server) noteUnguardedFileReplacement(artifact model.Artifact) {
	s.unguardedFileReplacements.Inc()
	slog.Info(unguardedFileReplacementMessage, "artifact", artifact.ID, "file", artifact.RefKey)
}
