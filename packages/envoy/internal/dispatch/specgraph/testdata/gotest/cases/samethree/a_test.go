package samethree

import "testing"

func TestSame(t *testing.T) {
	t.Parallel()
	<-Printed
	t.Log("the run of package samethree passes last")
}
