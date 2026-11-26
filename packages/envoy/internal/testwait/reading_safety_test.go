package testwait

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// liveSocket is a reading a test can hand Until while its producer keeps mutating the socket's
// connection. describe must report the socket value the caller handed it without reading through
// the pointer into that live connection.
type liveSocket struct {
	connection *liveConnection
}

type liveConnection struct {
	sequence int
	payload  [32]byte
}

// TestADescribeDoesNotRaceWithTheObjectsBehindPointers re-executes this test under -race. The child
// gives up on a reading of []*liveSocket while a goroutine writes each socket's live connection. A
// race report makes the parent fail, and is the control at the old describe, which walked through
// the pointer into connection.
func TestADescribeDoesNotRaceWithTheObjectsBehindPointers(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestDescribeLiveSocketsChild$", "-test.timeout="+childTimeout(t).String(), "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	err := child.Run()
	t.Logf("the live-socket child's output (%v):\n%s\nits stderr:\n%s", err, &stdout, &stderr)
	if err == nil {
		t.Fatal("the child was meant to give up; it passed")
	}
	output := stdout.String() + stderr.String()
	if strings.Contains(output, "WARNING: DATA RACE") {
		t.Fatalf("describe raced the socket writer:\n%s", output)
	}
	if !strings.Contains(output, "gave up waiting for live sockets to close") {
		t.Fatalf("the child did not give up by name:\n%s", output)
	}
}

// TestDescribeLiveSocketsChild is the child of TestADescribeDoesNotRaceWithTheObjectsBehindPointers.
// It runs only when its parent re-executes this binary.
func TestDescribeLiveSocketsChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestADescribeDoesNotRaceWithTheObjectsBehindPointers")
	}
	socket := &liveSocket{connection: &liveConnection{}}
	started, stop, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		close(started)
		for {
			select {
			case <-stop:
				return
			default:
			}
			socket.connection.sequence++
			socket.connection.payload[0]++
		}
	}()
	<-started
	t.Log("plant fired: a live socket writer mutates the pointers the reading hands Until")
	t.Cleanup(func() { close(stop); <-stopped })
	end := childGiveUpAt(t)
	stepped := useStepClock(t, end.Add(-time.Hour))
	reads := 0
	Until(t, "live sockets to close", func() ([]*liveSocket, bool) {
		if reads++; reads == 2 {
			stepped.now = end
		}
		return []*liveSocket{socket}, false
	})
}

// sharedNode has two fields that point at the same next node. Without a shared-pointer guard, the
// output doubles at every level; 30 levels are not a useful failure message.
type sharedNode struct {
	left, right *sharedNode
}

// firstPointer holds an interface as its first field. A pointer to this struct and a pointer to the
// field have the same address but different types, so only the later repetition of the latter is a
// cycle.
type firstPointer struct {
	first any
}

// sharedDepthEnv lets the base control plant 18 levels: that is enough to show the old
// exponential growth without exhausting the scope. The fixed head's row uses 30.
const sharedDepthEnv = "TESTWAIT_SHARED_POINTER_DEPTH"

func sharedDepth(t *testing.T) int {
	t.Helper()
	if value := os.Getenv(sharedDepthEnv); value != "" {
		depth, err := strconv.Atoi(value)
		if err != nil || depth < 1 {
			t.Fatalf("%s = %q, want a positive integer: %v", sharedDepthEnv, value, err)
		}
		return depth
	}
	return 30
}

// A reading with 30 shared-pointer levels is bounded, and pointers with the same address but
// different types do not become a false cycle. The base control uses 18 levels through
// sharedDepthEnv, enough to make its old output megabytes without exhausting the test's scope.
func TestDescribeBoundsSharedPointersAndKeysCyclesByType(t *testing.T) {
	depth := sharedDepth(t)
	t.Logf("plant fired: shared pointer depth %d", depth)
	leaf := &sharedNode{}
	for range depth {
		leaf = &sharedNode{left: leaf, right: leaf}
	}
	if got := describe(leaf); len(got) > 4<<10 {
		t.Errorf("the %d-level shared reading is %d bytes, more than the 4096-byte cap", depth, len(got))
	}
	value := &firstPointer{}
	value.first = &value.first
	got := describe(value)
	if strings.HasPrefix(got, "&{first:<cycle>") {
		t.Errorf("a pointer to a struct and a pointer to its first field, which share an address but not a type, printed a false cycle: %s", got)
	}
	if len(got) > 4<<10 {
		t.Errorf("the cyclic reading is %d bytes, more than the 4096-byte cap: %s", len(got), got)
	}
}
