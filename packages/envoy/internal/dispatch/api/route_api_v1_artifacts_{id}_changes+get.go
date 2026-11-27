package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/changes",
	Auth:        authAny,
	Description: "What changed between two stored versions of one document, ?from=<n>&to=<m>: {artifact_id, from, to, decision_only, unchanged, sections, blocks}, both versions read from their stored markdown with every ask block cut out, so the block index's deferred pass is never waited for. Exactly one of three holds: decision_only, true when the two stored texts differ and cut to the same text; unchanged, true when the two differ in nothing any section or block holds (the stored texts are the same, or the cut texts name nothing and are not decision-only); or sections and blocks name at least one item. sections and blocks are empty when decision_only or unchanged is true. sections is each top-level unit whose text differs, that moved, or that one version alone holds: a `##` section by heading and occurrence (the heading's text as a heading anchor reads it, without markdown's syntax and with a line break in it as a line feed; 0 for the first section of that heading, so a `##` heading with no words counts from occurrence 1), and a run of the text outside every `##` section (the title and what stands above the first `##`, or what follows a `#` heading up to the next `##`) under heading \"\" and occurrence 0 with its place: place before_first_section, or place after_section with after_heading and after_occurrence naming the `##` section directly before the run, in the later version for a changed, added or moved run and in the earlier for a removed one. A `#` or `##` heading that holds a line break, which is stored in setext form with no `#`, cuts the document as any heading of its level does. blocks is each typed block but an ask whose text or any attribute differs, by id and type. Each unit and block is marked changed, added or removed, and a unit whose text is the same is marked moved when its place among the document's top-level units is another: it stands outside one longest common subsequence of the two versions' orders of the units both hold, and of several that long the one kept holds the later version's earliest units, so of two units that swapped places the later version's second is moved. Both versions hold a run when both hold its text at the same place, and then, among the runs left in each version, the n-th run of one text in each is one run in both, so adding or removing one copy of a repeated run moves no other copy's match; a run the other version does not hold pairs with that version's unpaired run at the same place and is marked changed, and a run left unpaired is added or removed. A block is never marked moved. Each list holds the later version's items in its document order, a moved unit where the later version holds it, then what only the earlier holds, in its order. from and to must be positive, at most 2147483647 (the largest version number the store holds), and from below to (400 INVALID_VERSION); a version the document does not have is 404 NOT_FOUND, and a file or an image 400 NOT_DOCUMENT.",
	Handler:     (*server).getArtifactChanges,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape: routeShape{
		Status: http.StatusOK, Response: artifactChanges{}, Params: artifactChangesQuery{},
	},
})
