package intake

import "github.com/sjawhar/legion/daemon/internal/record"

// A control of the merged-tree vet gate of INFRA-5392 (dispatch://INFRA-5392), never merged.
// PlanNotTaken exists at INFRA-5392-base (27131260cfa0, from #4940's 8bcb8e595330) and not at
// this commit's parent ec5eb85c5a2b. So go vet of this commit alone fails with
// "undefined: PlanNotTaken", and go vet of this commit merged into INFRA-5392-base passes.
var _ func(string, record.Assignment) *Refusal = PlanNotTaken
