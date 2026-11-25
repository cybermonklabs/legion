package graphcensus

// approvalProjections holds no node or tuple projections. The census checks carry rows
// against their review reasons in censusOfReviewCarries.
var approvalProjections []projection

// approvalBackfills includes the pass over carried reviews (backfillPasses).
var approvalBackfills = []backfillPass{oneKind(backfillReviewCarries)}
