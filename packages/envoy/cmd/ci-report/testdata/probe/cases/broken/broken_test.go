package broken

import "testing"

func TestTheBuildReadsTheClaim(t *testing.T) {
	_ = claimOf("the first session")
}
