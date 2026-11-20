package launcher

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/sqlstate"
)

// openFailed is what the pinned Oh My Pi's report of a failed first open of its session database
// says, between the URL file it names and the failure: one line on its standard error, written just
// before it exits 1 (resolveSessionStorage in its src/session/session-storage-config.ts, and its
// src/main.ts):
//
//	Error: OMP_SESSION_SQL_DSN_FILE names <file>, but the session database could not be opened: <message> (<code>)
//
// <code> is its database driver's (Bun's SQL client) own name for the failure. A server's own
// answer is serverAnswered, which an Oh My Pi that prints the server's SQLSTATE follows with it,
// after a space: (ERR_POSTGRES_SERVER_ERROR 57P03).
const openFailed = "but the session database could not be opened: "

// connectionFailed begins every code Bun's SQL client gives a connection that failed before the
// server answered it: refused (ERR_POSTGRES_CONNECTION_REFUSED), ended while it was being opened
// (ERR_POSTGRES_CONNECTION_FAILED, ERR_POSTGRES_CONNECTION_CLOSED) or not answered in time
// (ERR_POSTGRES_CONNECTION_TIMEOUT).
const connectionFailed = "ERR_POSTGRES_CONNECTION_"

// serverAnswered is the code Bun's SQL client gives an answer of the server's own, whatever its
// SQLSTATE: a server starting up (57P03) and a password it refuses (28P01) alike.
const serverAnswered = "ERR_POSTGRES_SERVER_ERROR"

// reportLine bounds the line a childReport holds while it waits for the line's end; a longer line
// is no report and is dropped up to its end.
const reportLine = 4096

// ansiColor is the colour a report can carry when the child's output is a terminal's.
var ansiColor = regexp.MustCompile("\x1b\\[[0-9;]*m")

// childReport reads a child's standard error, as it passes to the launcher's own, for the child's
// own report that its first open of its session database failed (openFailed). A failure the
// driver names as the connection's own (connectionFailed) is a store fault the child reported, told
// by where it came from, as the launcher's own connection's is (ompsessions.ConnectURL). So is a
// server's own answer (serverAnswered) whose report carries a SQLSTATE of the server's own state
// (sqlstate.NotJudged, the one rule the daemon's stores read a server's answer by): 57P03 and 53300
// are, 28P01 is not. The pinned Oh My Pi prints no SQLSTATE: its report carries the server's
// message and Bun's code alone, the SQLSTATE being only the error's errno, which it keeps as the
// cause of its refusal and never prints. So with it a store starting up and a password the server
// refuses read the same, decide nothing, and the store witness decides as it does for a child that
// reports nothing (docs/kubernetes.md). The driver names a URL file that is wrong (a host that does
// not resolve, a port where some other server listens) as it names a refused dial, so a report
// marks the exit only once the launcher's own connect to the same URL file meets no fault of the
// configuration's (childExitFault). Writes never fail, so the child's output goes on whatever its
// lines hold.
type childReport struct {
	mu    sync.Mutex
	line  []byte
	long  bool
	fault error
}

func (r *childReport) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for rest := b; len(rest) > 0; {
		end := bytes.IndexByte(rest, '\n')
		part := rest
		if end >= 0 {
			part = rest[:end]
		}
		if !r.long && len(r.line)+len(part) <= reportLine {
			r.line = append(r.line, part...)
		} else {
			r.long, r.line = true, r.line[:0]
		}
		if end < 0 {
			break
		}
		if !r.long {
			r.read(string(r.line))
		}
		r.line, r.long, rest = r.line[:0], false, rest[end+1:]
	}
	return len(b), nil
}

// read takes one whole line of the child's standard error.
func (r *childReport) read(line string) {
	_, detail, ok := strings.Cut(ansiColor.ReplaceAllString(line, ""), openFailed)
	if !ok {
		return
	}
	detail = strings.TrimSpace(detail)
	open := strings.LastIndexByte(detail, '(')
	if open < 0 || !strings.HasSuffix(detail, ")") {
		return
	}
	name, state, _ := strings.Cut(detail[open+1:len(detail)-1], " ")
	if strings.HasPrefix(name, connectionFailed) || name == serverAnswered && isSQLSTATE(state) && sqlstate.NotJudged(state) {
		r.fault = fmt.Errorf("the child reported its session database could not be opened: %s", detail)
	}
}

// isSQLSTATE is whether code has a SQLSTATE's form: five digits or upper-case letters.
func isSQLSTATE(code string) bool {
	if len(code) != 5 {
		return false
	}
	for _, c := range code {
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// storeFault is the store's fault the child reported, nil for none or for a nil report.
func (r *childReport) storeFault() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fault
}

// alongside is w with the report reading what is written to it too; the report alone when w is
// nil, which exec.Cmd would have made the null device, and w itself for a nil report, which reads
// nothing.
func (r *childReport) alongside(w io.Writer) io.Writer {
	switch {
	case r == nil:
		return w
	case w == nil:
		return r
	}
	return io.MultiWriter(w, r)
}
