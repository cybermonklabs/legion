package config

import "testing"

// TestINFRA1115NegativeControl fails on purpose. It exists only on the throwaway
// branch legion/INFRA-1115-negative-control, which is the negative control of
// checks k1 and j4: the same build-farm Job run at this commit must print FAIL
// and end the Job Failed. The branch is deleted in the turn that reads the
// result, and nothing of it reaches legion/INFRA-1115.
func TestINFRA1115NegativeControl(t *testing.T) {
	t.Fatal("INFRA-1115 negative control: this test fails on purpose")
}
