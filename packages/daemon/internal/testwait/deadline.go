package testwait

import (
	"flag"
	"time"
)

// BinaryDeadline is the deadline go test's -timeout gives this test binary, reckoned from now,
// and false under -timeout 0, which gives none. It is for a TestMain, which has no test to ask
// (testing.T.Deadline): the TestMains that give every bound registered with programbound this
// deadline (programbound.SetDeadline) read it here. A TestMain calls it before m.Run, which
// starts go test's own alarm a moment later, so this deadline is never the later of the two. It
// parses the command line first if nothing has yet.
func BinaryDeadline() (time.Time, bool) {
	if !flag.Parsed() {
		flag.Parse()
	}
	timeout := flag.Lookup("test.timeout").Value.(flag.Getter).Get().(time.Duration)
	if timeout <= 0 {
		return time.Time{}, false
	}
	return time.Now().Add(timeout), true
}
