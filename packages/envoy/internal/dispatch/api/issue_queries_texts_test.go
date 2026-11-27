package api

// The listing's own order's four texts by name, which the plan, equality, timing and speed tests
// read; listIssues picks its text from issueListTextsBySort.
var (
	listIssuesQuery           = issueListTextsBySort[issueSortListing].unpaged
	listPinnedIssuesQuery     = issueListTextsBySort[issueSortListing].pinned
	listIssuesPageQuery       = issueListTextsBySort[issueSortListing].paged
	listPinnedIssuesPageQuery = issueListTextsBySort[issueSortListing].pinnedPaged
)
