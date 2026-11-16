package killsame

import "testing"

func TestSame(t *testing.T) {
	t.Parallel()
	<-ExternalStarted
	t.Log("the run of package killsame passes after the run of package killsame_test starts")
}
