package api

import (
	"log/slog"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/metrics"
)

// A whole-document upload over a document that already exists names the token of the version it
// replaces, and Dispatch checks that token under the write's own lock (docs.Service.ReplaceText).
// #3000 refused one that named none, 400 PRECONDITION_REQUIRED, but no deployed `dispatch` client
// can name a token on `dispatch artifact`, so from the deploy that carried #3000 every agent's
// replacement of a document was refused (INFRA-4980). Such a replacement is taken unchecked again,
// as it was before #3000, and each one stored is counted here and logged with the caller that
// sent it. The refusal comes back in a later change, once every deployed client sends the token:
// the counter reading no new replacements is how that is seen.

// unguardedReplacementsMetric is the counter of whole-document uploads that replaced a document
// naming no token. It carries no label: the caller is named in the log line, since a caller id is
// neither bounded nor public, and whoever reaches the port reads /metrics.
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
// logs it with the caller that sent it, cut as the access log cuts it (accesslog.Clip), since a
// caller whose credential proves no session names its session id in the body. A deployed
// `dispatch` command sends no version of its own, so the session that ran it is what says which
// client it was. A repair of a document outside the Proof schema names no token by design and is
// counted too; it is rare, and its line names its document.
func (s *server) noteUnguardedReplacement(artifact model.Artifact, actor model.Actor) {
	s.unguardedReplacements.Inc()
	slog.Info(unguardedReplacementMessage,
		"artifact", artifact.ID, "document", artifact.RefKey,
		"caller_kind", actor.Kind, "caller", accesslog.Clip(actor.ID))
}
