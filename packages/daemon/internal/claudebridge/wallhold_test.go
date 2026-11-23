package claudebridge

import (
	"regexp"
	"runtime"
	"strings"
)

// heldSamples is how many reads of the goroutines in a row must find the same hold before a row
// names it (heldOnTheWallClock): a wait the bridge leaves at once, such as a read a deadline of
// time.Now() is about to end, is gone by the next read, and a hold stays.
const heldSamples = 5

// bridgeFrame is a frame of one of this package's non-test files in a goroutine dump.
var bridgeFrame = regexp.MustCompile(`/internal/claudebridge/[^/\s]+\.go:\d+`)

// allStacks is every goroutine's stack, as runtime.Stack writes them.
func allStacks() string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// firstLine is text up to its first newline.
func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// heldOnTheWallClock is the goroutine of the bridge's that stacks, a dump of every goroutine
// (allStacks), shows blocked in a wait on the wall clock, which no Config.Clock moves, and whether
// there is one. Such a wait is a goroutine with a frame in one of this package's non-test files
// that is asleep in time.Sleep, in a sleep or timed wait of syscall's or golang.org/x/sys/unix's,
// or in a read or write of a network connection once the inbox's close has begun (a goroutine in
// inbox.close): from then on every connection of the inbox has a deadline of time.Now(), so a read
// or write still waiting waits on a deadline someone set on the wall clock. It is answered only
// when no goroutine of the bridge's is runnable or running: a machine too loaded to run the
// bridge leaves its goroutines runnable, not blocked, so it is never read as a hold. A bridge
// waiting on a process or a pipe is blocked too, and neither is a wall-clock wait.
func heldOnTheWallClock(stacks string) (string, bool) {
	var bridge []string
	closing := false
	for _, g := range strings.Split(stacks, "\n\n") {
		if !hasBridgeFrame(g) {
			continue
		}
		bridge = append(bridge, g)
		if strings.Contains(g, ".(*inbox).close(") {
			closing = true
		}
	}
	held := ""
	for _, g := range bridge {
		state := goroutineState(g)
		switch {
		case state == "runnable" || state == "running":
			return "", false
		case held != "":
		case state == "sleep" && strings.Contains(g, "time.Sleep("):
			held = g
		case state == "syscall" && syscallSleep.MatchString(g):
			held = g
		case state == "IO wait" && closing && networkReadOrWrite.MatchString(g):
			held = g
		}
	}
	return held, held != ""
}

var (
	syscallSleep       = regexp.MustCompile(`\b(syscall|unix)\.(Nanosleep|ClockNanosleep|Select|Pselect|Poll|Ppoll|EpollWait|Pause)\(`)
	networkReadOrWrite = regexp.MustCompile(`net\.\(\*(conn|netFD)\)\.(Read|Write)\(`)
)

// hasBridgeFrame is whether goroutine g of a dump runs a frame of this package's non-test files.
func hasBridgeFrame(g string) bool {
	for _, frame := range bridgeFrame.FindAllString(g, -1) {
		if !strings.Contains(frame, "_test.go:") {
			return true
		}
	}
	return false
}

// goroutineState is the state its dump's header gives goroutine g, "IO wait" for
// "goroutine 7 [IO wait, 2 minutes]:".
func goroutineState(g string) string {
	header := firstLine(strings.TrimLeft(g, "\n"))
	_, state, ok := strings.Cut(header, "[")
	if !ok {
		return ""
	}
	state, _, _ = strings.Cut(state, "]")
	state, _, _ = strings.Cut(state, ",")
	return state
}
