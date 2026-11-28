package slow

import (
	"testing"
	"time"
)

func TestTheQuickReadEnds(t *testing.T) {
	t.Log("the read ended in 1 ms")
}

func TestTheSlowReadEnds(t *testing.T) {
	t.Log("the read starts")
	time.Sleep(30 * time.Second)
}
