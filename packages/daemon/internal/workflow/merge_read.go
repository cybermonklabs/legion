package workflow

// RequestInstructionLabel is in each text that tells an owner that a request for new work started
// the change it owns (D17): the root architect's start (ArchitectStartTask), the next-change notice
// and the start of a child's own architect. A reader tells the request's instruction by it.
const RequestInstructionLabel = "a request for new work started this change"
