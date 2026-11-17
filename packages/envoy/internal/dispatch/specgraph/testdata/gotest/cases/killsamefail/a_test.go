package killsamefail

import "testing"

func TestSame(t *testing.T) {
	t.Parallel()
	<-ExternalStarted
	t.Error("the run of package killsamefail fails after the run of package killsamefail_test starts")
}
