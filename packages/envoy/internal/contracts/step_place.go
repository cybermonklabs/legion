package contracts

// StepPlaceHolds reports whether place is a place a run of kind can stand at: from 1 for a step a
// plan names, 0 for a retro or a merge, which no plan names. A kind that is none of the three
// holds no place. It is the one statement of the rule: Dispatch's step store refuses a write
// outside it, and the Legion daemon, through github.com/sjawhar/envoy/steprecord, refuses to
// queue one.
func StepPlaceHolds(kind string, place int) bool {
	switch kind {
	case StepKindStep:
		return place >= 1
	case StepKindRetro, StepKindMerge:
		return place == 0
	}
	return false
}
