package supervise

import (
	"go/ast"
	"go/types"
	"slices"
	"testing"
)

// R4 of dispatch://INFRA-3136 says that one policy decides whether a claim's failure charges a
// budget (Charges): every charge site asks it, and no charge site decides anything itself. A
// site that charges a failure does so through a charge function, which asks the policy. A direct
// call of the machine's launch beside that call, a launch that no charge preceded (launchAfter
// with charged false), is a second way out of the same end that asks nothing: the site then
// decides by a test of its own whether this end charges, and the end it waives never reaches the
// policy, so it has no row there to read or change. launchAfter is not such a call: it carries
// whether a charge preceded it, and the retries of one launch call it to try that launch again
// from the top (launchFromTop, startWaitsForStore). The compiler cannot hold this, since launch
// is a method the package calls from many sites, and a site that may call a charge function may
// call it too. So this test reads the package's source: in each function that calls a charge
// function, its closures included, it names every direct call of launch. Delete it when a
// relaunch after a failure can be had only from a charge function.
//
// The check c-one-charge-policy of dispatch://INFRA-3136's verification.md asks for a test that
// "reads the package's own source and fails when a charge site decides for itself".
func TestAFailureSiteRelaunchesOnlyThroughTheChargeFunctions(t *testing.T) {
	src := checkOutcomeSources(t)[supervisePath]
	for fn, decl := range src.funcs() {
		if decl.Body == nil || slices.Contains(chargeFunctions, fn.Name()) {
			continue
		}
		var charges int
		var direct []*ast.CallExpr
		ast.Inspect(decl.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee := src.callee(call)
			if callee == nil || callee.Pkg() != src.pkg {
				return true
			}
			switch {
			case slices.Contains(chargeFunctions, callee.Name()):
				charges++
			case callee.Name() == "launch" && callee.Type().(*types.Signature).Recv() != nil:
				direct = append(direct, call)
			}
			return true
		})
		if charges == 0 {
			continue
		}
		for _, call := range direct {
			t.Errorf("%s (in %s) launches beside a charge of the same function's failure: that path decides for itself that the end charges nothing, which is the policy's to decide (Charges); give the end an outcome of its own with a row in Charges and relaunch through a charge function",
				src.pos(call), fn.Name())
		}
	}
}
