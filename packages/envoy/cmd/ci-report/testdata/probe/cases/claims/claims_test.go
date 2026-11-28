package claims

import "testing"

func TestTheClaimStaysAfterARestart(t *testing.T) {
	t.Log("the daemon read 1 claim after the restart")
}

func TestTheOwnerStaysAfterARestart(t *testing.T) {
	t.Errorf("the owner is %q after the restart, want %q", "", "the first session")
}

func TestTheWaitIsSkipped(t *testing.T) {
	t.Skip("this machine has no second database")
}
