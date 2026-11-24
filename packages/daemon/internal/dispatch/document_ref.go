package dispatch

// DocumentRef is the Dispatch reference of a document of issue, as a reference to one of its blocks
// names it before its `#b-` (FormatBlockRef in packages/envoy/internal/dispatch/text):
// `dispatch://<issue>/spec` for the issue's primary document, and `dispatch://<issue>/artifact/<slug>`
// for any other, slug being the name Dispatch filed it under. It is the one spelling of that
// reference: the plan a register_plan reads (api's registerPlan) and the document a
// design-gate step reviews (workflow's stepRef) are both named by it.
func DocumentRef(issue, slug string, primary bool) string {
	if primary {
		return "dispatch://" + issue + "/spec"
	}
	return "dispatch://" + issue + "/artifact/" + slug
}
