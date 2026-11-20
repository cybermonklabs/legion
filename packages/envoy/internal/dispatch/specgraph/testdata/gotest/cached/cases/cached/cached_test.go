package cached

import "testing"

func TestA(t *testing.T) {
	if Answer() != 42 {
		t.Fatal("the answer is not kept")
	}
	t.Log("the answer is kept")
}

func TestB(t *testing.T) {
	t.Skip("not here")
}
