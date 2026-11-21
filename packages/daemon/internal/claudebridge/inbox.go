package claudebridge

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// InboxVariable names the inbox socket in Claude Code's environment, which its MCP servers inherit:
// packages/claude-envoy's channel server writes each Envoy delivery there instead of a channel
// notification, which Claude Code's print mode never delivers, each of the claim's task values
// and finished numbers, and the word that the claim's ready has answered.
const InboxVariable = "LEGION_CLAUDE_INBOX"

// InboxTask, InboxReady, InboxFinished and InboxTakePending are the Kinds of a task line, of the
// ready line, of a finished line and of a take-pending query (InboxDelivery).
const (
	InboxTask        = "task"
	InboxReady       = "ready"
	InboxFinished    = "finished"
	InboxTakePending = "take-pending"
)

// InboxDelivery is one line a client writes to the inbox: an Envoy delivery, already rendered, that
// becomes a user turn; a task line, the claim's task value; the ready line, claude-envoy's word
// that the claim's `claims/ready` has answered, and the read of claims/task after it too, whose
// reading line comes ahead of it; a finished line, a reading of the daemon's that held no value;
// or a take-pending query, claude-envoy's hooks asking whether the claim's take holds a task.
// InboxAnswer is the line the bridge writes back: for a delivery, once Claude Code has taken it,
// re-emitting it as the user message it reads (--replay-user-messages), or with an error once it
// never will — the delivery is in the transcript, which is what claude-envoy's `done` says; for
// the claim's other lines, once the bridge's loop has taken it (take.go), which answers a
// take-pending query at once, whatever Claude Code is doing.
type InboxDelivery struct {
	ID string `json:"id"`
	// Kind is "" for an Envoy delivery, InboxTask for a task line, InboxReady for the ready line,
	// InboxFinished for a finished line and InboxTakePending for a take-pending query. A task line
	// is a value of the claim's key in the task bucket, which claude-envoy hands the bridge as it
	// is handed it — relayed, read at its registration, or read from claims/task
	// (packages/claude-envoy, src/legion-inbox.ts) — and carries Number, Epoch, Revision and
	// Finished and no Content. The ready line and a take-pending query carry their ID alone, and a
	// finished line its ID and Finished.
	Kind    string `json:"kind,omitempty"`
	Content string `json:"content"`
	// Blocks is the delivery as Claude Code's own content blocks, which a line carries where the
	// delivery is more than its text: a person's own message and the pictures it embeds
	// (packages/claude-envoy, src/legion-inbox.ts). The bridge reads none of it — each block goes
	// to Claude Code as it came — and Content stays the delivery's text, so a line without the
	// field is the line it always was and a bridge of any age delivers the text.
	Blocks []json.RawMessage `json:"blocks,omitempty"`
	// Number, Epoch and Revision are a task line's value (claim.TaskValue): the number of the task
	// the claim is owed, 0 where the value was deleted at this revision and the claim has no task
	// from it on; the bucket's epoch; and the key's revision in it.
	Number   uint64 `json:"number,omitempty"`
	Epoch    string `json:"epoch,omitempty"`
	Revision uint64 `json:"revision,omitempty"`
	// Finished is the number of the last task the daemon's reading says the claim finished
	// (claim.TaskResponse's Finished, the registration's): on a task line the reading the value
	// came with, 0 for a value that came with no reading, a relayed one; on a finished line a
	// reading that held no value. Every task line carries it, 0 included: a task line without it
	// is one a claude-envoy older than this bridge wrote, which held the finished number itself,
	// and is refused (olderTaskLine).
	Finished *uint64 `json:"finished,omitempty"`
}

type InboxAnswer struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// TakePending is a take-pending query's answer, and nil on the answer of every other line,
	// which is therefore the line it always was.
	TakePending *TakePendingAnswer `json:"takePending,omitempty"`
}

// TakePendingAnswer is what the bridge answers a take-pending query with (take.go): Pending,
// whether the claim's take holds a task, which claude-envoy's Stop hook reads to block no stop
// while it does; and Waiting, the number of the task the claim is owed that the turn running did
// not take, 0 when no turn runs or none waits, which its PostToolUse hook names in its note.
type TakePendingAnswer struct {
	Pending bool   `json:"pending"`
	Waiting uint64 `json:"waiting"`
}

// inbox is the bridge's listener: a unix socket in a directory of its own, 0700, so only this user
// reaches it, made under the system's temporary directory because a socket's path is bounded (108
// bytes on Linux) and a claim's directory under a state directory need not fit.
type inbox struct {
	dir      string
	path     string
	listener net.Listener
	wg       sync.WaitGroup
}

// inboxDirPrefix names the inbox's directory, which os.MkdirTemp makes under the process TMPDIR,
// and inboxSocketName the socket the bridge binds in it.
const (
	inboxDirPrefix  = "legion-claude-"
	inboxSocketName = "inbox.sock"
)

// InboxLongestPath is the longest path the bridge makes under the process TMPDIR, written
// relative to it: the inbox directory and the socket in it. The ten digits are os.MkdirTemp's own
// suffix at its widest, a random number below 1e10. A test that runs the bridge in-process hands
// this to testtmp.Dir as the room the TMPDIR it places needs, so that the socket's path stays
// inside the 107 bytes Linux holds.
const InboxLongestPath = inboxDirPrefix + "0123456789" + "/" + inboxSocketName

// listenInbox opens the inbox and serves it until close, handing each delivery to the loop.
func listenInbox(events chan<- event, stop <-chan struct{}, logger *log.Logger) (*inbox, error) {
	dir, err := os.MkdirTemp("", inboxDirPrefix)
	if err != nil {
		return nil, fmt.Errorf("make the inbox directory: %w", err)
	}
	path := filepath.Join(dir, inboxSocketName)
	listener, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("listen on the inbox: %w", err)
	}
	in := &inbox{dir: dir, path: path, listener: listener}
	in.wg.Add(1)
	go func() {
		defer in.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					logger.Printf("the inbox stopped accepting: %v", err)
				}
				return
			}
			in.wg.Add(1)
			go func() {
				defer in.wg.Done()
				serveInbox(conn, events, stop, logger)
			}()
		}
	}()
	return in, nil
}

const (
	// inboxPending bounds the lines one client has sent and the bridge has not yet answered; past
	// it the inbox reads that client's next line once the oldest is answered.
	inboxPending = 64
	// inboxFlushTimeout bounds writing what a client is owed once the inbox closes: the answers
	// the loop gave before closing it, each one short line a reading client takes at once.
	inboxFlushTimeout = 100 * time.Millisecond
	malformedDelivery = "an inbox delivery is one JSON object with an id and content"
	malformedTask     = "an inbox task line is one JSON object with an id, a number, an epoch, a revision and the claim's finished number"
	malformedFinished = "an inbox finished line is one JSON object with an id and the claim's finished number"
	// olderTaskLine answers a task line of the shape claude-envoy wrote before the bridge held the
	// claim's finished number, the ready gate and the one newest-value rule (take.go): such a
	// claude-envoy is not the one this bridge ships with, so the bridge takes no task from it
	// rather than weigh a value with half of what it needs.
	olderTaskLine = malformedTask + ": this one names no finished number, as a claude-envoy older than this bridge writes it, so no task is taken from it"
)

// owed is one answer a client is owed, in the order it sent its deliveries.
type owed struct {
	id     string
	answer <-chan inboxReply
}

// inboxReply is the loop's answer to one inbox line, which serveInbox writes as that line's
// InboxAnswer: err is why the bridge refused the line or will never take it, nil once it has taken
// it; takePending is a take-pending query's answer, nil for every other line.
type inboxReply struct {
	err         error
	takePending *TakePendingAnswer
}

// serveInbox reads one client's lines, handing each delivery and task value to the loop as it
// arrives, and answers them in the order they came, each once the loop has answered it: a line
// waits for the answers of the lines ahead of it on its connection, so claude-envoy writes its
// take-pending query down a connection of its own. The loop answers every line it took before the
// inbox closes, so those answers are written then too, within inboxFlushTimeout. The watcher of
// the inbox's close ends with the connection too: claude-envoy dials a connection for every
// take-pending query, so a watcher left parked on stop would hold one goroutine and one closed
// connection for each of them until the bridge exits.
func serveInbox(conn net.Conn, events chan<- event, stop <-chan struct{}, logger *log.Logger) {
	defer conn.Close()
	served := make(chan struct{})
	defer close(served)
	go func() {
		select {
		case <-stop:
			_ = conn.SetReadDeadline(time.Now())
			_ = conn.SetWriteDeadline(time.Now().Add(inboxFlushTimeout))
		case <-served:
		}
	}()
	pending := make(chan owed, inboxPending)
	go func() {
		defer close(pending)
		reader := bufio.NewReaderSize(conn, 64<<10)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			answer := make(chan inboxReply, 1)
			var d InboxDelivery
			if err := json.Unmarshal(line, &d); err != nil {
				d = InboxDelivery{}
			}
			if ev, refusal := inboxEvent(d, answer); refusal != nil {
				answer <- inboxReply{err: refusal}
			} else if !send(events, stop, ev) {
				return
			}
			select {
			case pending <- owed{id: d.ID, answer: answer}:
			case <-stop:
				return
			}
		}
	}()
	encoder := json.NewEncoder(conn)
	for o := range pending {
		var reply inboxReply
		select {
		case reply = <-o.answer:
		case <-stop:
			select {
			case reply = <-o.answer:
			default:
				return
			}
		}
		answer := InboxAnswer{ID: o.id, OK: true, TakePending: reply.takePending}
		if reply.err != nil {
			answer = InboxAnswer{ID: o.id, Error: reply.err.Error()}
		}
		if err := encoder.Encode(answer); err != nil {
			logger.Printf("could not answer inbox line %s: %v", o.id, err)
			return
		}
	}
}

// inboxEvent is the event the inbox line d hands the loop, answered on answer, or why d is no line
// the bridge takes. A line that is not JSON reaches it as the zero line, which names no id.
func inboxEvent(d InboxDelivery, answer chan<- inboxReply) (event, error) {
	if d.ID == "" {
		return event{}, errors.New(malformedDelivery)
	}
	switch d.Kind {
	case "":
		if d.Content == "" {
			return event{}, errors.New(malformedDelivery)
		}
		return event{delivery: &delivery{id: d.ID, content: d.Content, blocks: d.Blocks, answer: answer}}, nil
	case InboxTask:
		switch {
		case d.Epoch == "" || d.Revision == 0:
			return event{}, errors.New(malformedTask)
		case d.Finished == nil:
			return event{}, errors.New(olderTaskLine)
		}
		return event{task: &taskLine{value: taskValue{number: d.Number, epoch: d.Epoch, revision: d.Revision},
			finished: *d.Finished, answer: answer}}, nil
	case InboxReady:
		return event{ready: answer}, nil
	case InboxTakePending:
		return event{takePending: answer}, nil
	case InboxFinished:
		if d.Finished == nil {
			return event{}, errors.New(malformedFinished)
		}
		return event{finished: &finishedLine{number: *d.Finished, answer: answer}}, nil
	default:
		return event{}, fmt.Errorf("an inbox line of kind %q is none the bridge takes", d.Kind)
	}
}

// close stops accepting, waits for every client, which closing stop ends, and removes the socket's
// directory.
func (in *inbox) close() {
	_ = in.listener.Close()
	in.wg.Wait()
	_ = os.RemoveAll(in.dir)
}
