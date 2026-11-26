package gcp_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
)

// legionStatusScope true renders DISPATCH_LEGION_STATUS_SCOPE=on on the app's container, right
// after DISPATCH_SERVER_URL, in each mode that runs Dispatch, and unset renders the pod it always
// had (requireSwitchInputRendersOn). With parentStatusRule set too, it comes right after
// DISPATCH_PARENT_STATUS_RULE, in each mode.
func TestTheLegionStatusScopeInputRendersTheSetting(t *testing.T) {
	requireSwitchInputRendersOn(t, "DISPATCH_LEGION_STATUS_SCOPE", func(a *gcp.DispatchArgs) { a.LegionStatusScope = true })
	for _, mode := range dispatchSwitchModes {
		t.Run(mode.name+" beside parentStatusRule", func(t *testing.T) {
			both := mode.args()
			both.ParentStatusRule, both.LegionStatusScope = true, true
			m, err := runDispatch(t, both, true)
			require.NoError(t, err)
			requireRightAfter(t, m, "DISPATCH_PARENT_STATUS_RULE", "DISPATCH_LEGION_STATUS_SCOPE")
		})
	}
}
