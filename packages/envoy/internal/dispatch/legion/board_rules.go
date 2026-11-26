package legion

// board_rules.go holds the column table a Legion card stands in, and the decode of the project's
// admission block. Two readers place an issue by phase: the board route, which draws its cards in
// the columns (internal/dispatch/api/legion_board.go), and the metrics, which key each issue's
// hours by the same columns (internal/dispatch/legion/metrics). A table stated in the route alone
// is a table the metrics state a second time, and one table stated twice is a card and a metric
// that disagree about the same issue. The labels the view draws are not here: they belong to the
// view, and the route holds them beside the order it draws the columns in.

import "encoding/json"

// The board's columns. Queue holds the daemon's waiting line and, after it, the issues a blocker
// link holds; Design the issues whose phase is still `admitted` that are on a card at all — the
// slot holders, and the ones no card stands above, which are on no other issue's count (the board
// route's legionBoardCompute); Retro `retro` alone; Merging `merging` and `awaiting_merge`; Waiting
// on children the parents in `awaiting_children`, whether or not the daemon's blocked list names
// them (legionBoardCompute); and Held the phase metric 2 puts on the "no role" bar beside
// `awaiting_person` and `awaiting_children` (the plan's D11).
const (
	ColumnQueue            = "queue"
	ColumnDesign           = "design"
	ColumnPlanning         = "planning"
	ColumnImplementing     = "implementing"
	ColumnTesting          = "testing"
	ColumnReviewing        = "reviewing"
	ColumnAccepting        = "accepting"
	ColumnRetro            = "retro"
	ColumnMerging          = "merging"
	ColumnAwaitingChildren = "awaiting_children"
	ColumnAwaiting         = "awaiting_person"
	ColumnHeld             = "held"
)

// PhaseColumns is the column each phase the daemon reports falls in. A phase this does not name
// (`done`, and any phase a later daemon adds) puts the issue on no card: the board shows the work
// in flight, and a phase Dispatch has never heard of is not something it can place. Which phases
// share a column is the board's own choice, made here: `retro` has a column of its own, as it has
// a word of its own in the spec's tables ("Phase words in tables"), Merging holds the two phases
// those tables name merging, `merging` and `awaiting_merge` (dispatch://INFRA-1077's b74df7a79b),
// and `awaiting_children` has a column of its own: a parent in it keeps no slot and waits for
// none, and no role works it. The metrics key an issue's hours by these columns, so an issue's
// retro hours are under `retro` there too, and on no merger figure.
var PhaseColumns = map[string]string{
	PhaseAdmitted:         ColumnDesign,
	PhasePlanning:         ColumnPlanning,
	PhaseImplementing:     ColumnImplementing,
	PhaseTesting:          ColumnTesting,
	PhaseReviewing:        ColumnReviewing,
	PhaseAccepting:        ColumnAccepting,
	PhaseRetro:            ColumnRetro,
	PhaseMerging:          ColumnMerging,
	PhaseAwaitingMerge:    ColumnMerging,
	PhaseAwaitingChildren: ColumnAwaitingChildren,
	PhaseAwaitingPerson:   ColumnAwaiting,
	PhaseHeld:             ColumnHeld,
}

// ParseAdmission is the admission block the project's `admission` observation records
// (FieldAdmission, under AdmissionKey). The second answer is whether the value was read: it is
// false for a value of another shape, because the daemon owns the wire shape and a reader of one
// names what it expects rather than trusting it. The metrics read the block through it.
func ParseAdmission(value []byte) (*Admission, bool) {
	var admission Admission
	if err := json.Unmarshal(value, &admission); err != nil {
		return nil, false
	}
	return &admission, true
}
