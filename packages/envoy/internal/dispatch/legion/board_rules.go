package legion

// board_rules.go holds the column table a Legion card stands in. Two readers place an issue by
// phase: the board route, which draws its cards in the columns
// (internal/dispatch/api/legion_board.go), and the metrics, which key each issue's hours by the
// same columns (internal/dispatch/legion/metrics). Both read the table here (Columns, and
// PhaseColumns, which is read off it): a table stated in the route alone is a table the metrics
// state a second time, and one table stated twice is a card and a metric that disagree about the
// same issue. The labels the view draws are not here: they belong to the view, and the route
// holds them beside the column each one names.

import "fmt"

// The board's columns. Queue holds no phase: the board route fills it (legionBoardCompute);
// Design the `admitted` issues on a card that Queue does not hold — the slot
// holders, and the ones no card stands above, which are on no other issue's count (the board
// route's legionBoardCompute); Step the issues at a step of a role the deployment defines,
// whatever its type; Retro `retro` alone; Merging `merging` and `awaiting_merge`; Waiting
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
	ColumnStep             = "step"
	ColumnAccepting        = "accepting"
	ColumnRetro            = "retro"
	ColumnMerging          = "merging"
	ColumnAwaitingChildren = "awaiting_children"
	ColumnAwaiting         = "awaiting_person"
	ColumnHeld             = "held"
)

// Column is one column of the board: its id, and the phases the daemon reports that the board
// places in it, in the order an issue passes through them.
type Column struct {
	ID     string
	Phases []string
}

// Columns is every column of the board with the phases each holds. The order is the board's
// left-to-right order; the metrics read each column's phases alone (partsOf in legion/metrics).
// Queue holds no phase of its own: the board route fills it (legionBoardCompute). An issue the
// queue does not hold, in a phase
// no column holds (`done`, and any phase a later daemon adds), is on no card: the board shows the
// work in flight, and a phase Dispatch has never heard of is not something it can place. Which
// phases share a column is the board's own choice, made here: `retro` has a column of its own, as
// it has a word of its own in the spec's tables ("Phase words in tables"), Merging holds the two
// phases those tables name merging, `merging` and `awaiting_merge` (dispatch://INFRA-1077's
// b74df7a79b), `step` has a column of its own, since a step of a role the deployment defines is
// of any type its settings name, which neither Testing nor Reviewing would say, and stands after
// Reviewing, where a task of the default steps would put a step of its own, though a plan can
// name one at any place, and
// `awaiting_children` has a column of its own: a parent in it keeps no slot and waits for none,
// and no role works it. The metrics key an issue's hours by these columns, so an issue's retro
// hours are under `retro` there too, and on no merger figure.
var Columns = []Column{
	{ColumnQueue, nil},
	{ColumnDesign, []string{PhaseAdmitted}},
	{ColumnPlanning, []string{PhasePlanning}},
	{ColumnImplementing, []string{PhaseImplementing}},
	{ColumnTesting, []string{PhaseTesting}},
	{ColumnReviewing, []string{PhaseReviewing}},
	{ColumnStep, []string{PhaseStep}},
	{ColumnAccepting, []string{PhaseAccepting}},
	{ColumnRetro, []string{PhaseRetro}},
	{ColumnMerging, []string{PhaseMerging, PhaseAwaitingMerge}},
	{ColumnAwaitingChildren, []string{PhaseAwaitingChildren}},
	{ColumnAwaiting, []string{PhaseAwaitingPerson}},
	{ColumnHeld, []string{PhaseHeld}},
}

// PhaseColumns is the column each phase the daemon reports falls in, read off Columns, and no
// entry for a phase no column holds.
var PhaseColumns = phaseColumns(Columns)

// phaseColumns indexes columns by phase: each phase a column holds, against that column's id. A
// phase is in one column: it panics on a phase it has already placed, naming the phase and both
// columns, since the metrics give a phase's hours to the first column that lists it (partsOf,
// issueParts) and the board puts its card in the column this index names.
func phaseColumns(columns []Column) map[string]string {
	byPhase := map[string]string{}
	for _, column := range columns {
		for _, phase := range column.Phases {
			if placed, held := byPhase[phase]; held {
				panic(fmt.Sprintf("legion: phase %q is in column %q and in column %q; a phase is in one column", phase, placed, column.ID))
			}
			byPhase[phase] = column.ID
		}
	}
	return byPhase
}
