package replay

// Branch is one value of each knob of a replay that the record does not give, so that a result
// that rests on one is run under each of its values and read side by side. A knob's values are
// words, each named below with the assumption of the spec it stands for. The slowdown factor f,
// the seventh knob, travels beside the branch as RunConfig.Slowdown, the one place a run reads
// it.
type Branch struct {
	// Name is "main" or "main+<knob>=<value>", the main branch with one knob changed.
	Name string `json:"name"`
	// Timing is when a link removal, a child moved out, a gate closing and an outside blocker's
	// close happen in the replay: "wall", at their recorded time, or "offset", at their recorded
	// offset from the start of the wait they end (assumption 4).
	Timing string `json:"timing"`
	// Answer is when a person answers: "wall" or "offset", as Timing (assumption 5).
	Answer string `json:"answer"`
	// AfterSendBack is how long work after a send-back takes: "recorded", its recorded time, or
	// "less-slot-wait", that time less its recorded wait for a slot (assumption 13).
	AfterSendBack string `json:"afterSendBack"`
	// FirstStarts is the order of first starts: "recorded", the order the record started issues
	// in, or "rank", the board's rank.
	FirstStarts string `json:"firstStarts"`
	// NeverStarted is the work of an issue that never started in the window: "none", it never
	// finishes; "drawn", drawn from the window's started issues; "drawn-done", drawn from its
	// finished issues (assumption 8).
	NeverStarted string `json:"neverStarted"`
	// Merges is the merges term of the conflict draw: "fitted", the rate fitted to the window's
	// base branches, or "zero" (assumption 3).
	Merges string `json:"merges"`
}

// MainBranch is the branch a replay runs unless it is asked for another: events and answers at
// their recorded offsets, work after a send-back and first starts as recorded, no work for an
// issue that never started, and the fitted merges term.
var MainBranch = Branch{
	Name:          "main",
	Timing:        "offset",
	Answer:        "offset",
	AfterSendBack: "recorded",
	FirstStarts:   "recorded",
	NeverStarted:  "none",
	Merges:        "fitted",
}
