package dispatchtool

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// ReadCommands are the dispatch commands the tool runs that write nothing, as the CLI's own help
// says of each (`dispatch --help`, packages/contracts/src/dispatch-tools.ts): with them the agent
// reads any issue, its documents, comments, asks and messages, those written after it stopped
// included, and its own session's answers, limits, steps, plans, results and inbox.
var ReadCommands = []string{
	"read", "doc-read", "search", "related", "issues", "projects", "priorities", "open-asks",
	"answers", "whoami", "limits", "handoff-read", "steps", "plans", "results", "inbox",
}

// WriteCommands are the dispatch commands the tool runs that write: replies in the agent's name,
// and the resolution of a comment thread. A reply aimed where the loop posts the answer itself is
// refused (Place). Every other write — a status, a claim, an ask, a document edit, a handoff, a
// review, an issue — changes an issue, its lifecycle, its documents or its design gate, and Legion
// reads several of them as a person's move when the writer holds no claim in the tree, which a
// stopped agent's session does not.
var WriteCommands = []string{"comment", "message", "send-to-agent", "resolve-comment"}

// marked are the write commands whose body the loop's mark is added to (withMark).
var marked = []string{"comment", "message", "send-to-agent"}

// Place is where the answer loop posts the reply of the question an answer runs for (Open), as a
// write of the tool names it: the ask on whose thread the reply is posted; the comments the
// question names of the comment thread it is posted in, a reply to any of which Dispatch puts in
// that thread (or on the ask the thread hangs under); the message it replies to; and the session
// it is sent to. A write aimed there would be a second reply, and the tool refuses it (aimRefusal).
// The zero Place names none, and the tool refuses nothing for it.
type Place struct {
	Ask      string
	Comments []string
	Message  string
	Session  string
}

// commandRefusal is the reason a command outside ReadCommands and WriteCommands is refused, which
// the model reads and its reply says.
const commandRefusal = "the daemon runs only these dispatch commands for a stopped agent: the reads %s; and the writes %s. " +
	"Your session holds no claim now, so a status write, a claim, an ask or a document edit would stand as a person's move, " +
	"and the daemon writes only replies in your name. A request for such a write is work your tools cannot do now: say so in your reply."

// fileRefusal is the reason an option that hands dispatch a file of the daemon's host is refused.
const fileRefusal = "`%s` hands dispatch a file of the daemon's host, and the daemon hands it none: " +
	"give the text as the call's stdin and write `--<field>-file -`, which reads it."

// placeRefusal is the reason a write aimed where the loop posts the answer is refused: the option
// that aims it as given, the command, and that place.
const placeRefusal = "`%s` aims this `dispatch %s` where the daemon posts your reply to this question itself, %s, " +
	"so it would be a second reply there. Put what you would post there in your reply: the daemon posts it there."

// refusal is the reason argv is refused before dispatch starts, "" for none: its command, the
// first argument, is none of ReadCommands and WriteCommands, a command the CLI adds later among
// them, since the tool keeps the list of what it runs and none of what it refuses; or, for every
// command, an option names a file of the host: a `--<field>-file` or `--<field>-json-file` whose
// value is not `-`, in either of the forms the CLI reads (`--x-file v` and `--x-file=v`), or a
// picture to upload (`--image v`, `--image=v`). The CLI reads every argument that starts with `--`
// as an option, and an option's value as the rest of its word after `=` or else as the next
// argument unless that one starts with `--`, so the options are read here as it reads them.
func refusal(argv []string) string {
	if len(argv) == 0 || !slices.Contains(ReadCommands, argv[0]) && !slices.Contains(WriteCommands, argv[0]) {
		command := "dispatch"
		if len(argv) > 0 {
			command += " " + argv[0]
		}
		return fmt.Sprintf("`%s` is none of the commands this tool runs. ", command) +
			fmt.Sprintf(commandRefusal, strings.Join(ReadCommands, ", "), strings.Join(WriteCommands, ", "))
	}
	for i := 1; i < len(argv); i++ {
		name, value, ok, inline := option(argv, i)
		if name == "" {
			continue
		}
		switch {
		case name == "--image":
			return fmt.Sprintf(fileRefusal, argv[i])
		case strings.HasSuffix(name, "-file") && (!ok || value != "-"):
			given := argv[i]
			if ok && !inline {
				given += " " + value
			}
			return fmt.Sprintf(fileRefusal, given)
		}
	}
	return ""
}

// aimRefusal is the reason argv, a call refusal lets run with stdin its standard input, is refused
// for aiming a write at place, "" for none: a comment replying on place's ask (`--reply-to-ask`)
// or to one of its comments (`--reply-to`), a message or an agent's message replying to its
// message (`--in-reply-to`), and an agent's message to its session (`--session`). Each option is
// read in every form the CLI reads it in: `--x v`, `--x=v`, and `--x-file -` (or `--x-file=-`),
// whose value is stdin, as the CLI reads it, whole; any other `--x-file` is refusal's. A call
// asking for help or a dry run posts nothing (asksNoWrite), and neither it, a resolution nor a
// read is refused here.
func aimRefusal(argv []string, stdin string, place Place) string {
	if asksNoWrite(argv) {
		return ""
	}
	for i := 1; i < len(argv); i++ {
		name, value, ok, inline := option(argv, i)
		if !ok {
			continue
		}
		given := argv[i]
		if !inline {
			given += " " + value
		}
		field, fromFile := strings.CutSuffix(name, "-file")
		switch {
		case fromFile && value != "-":
			continue
		case fromFile:
			value = stdin
		}
		if where := place.aimedBy(argv[0], field, value); where != "" {
			return fmt.Sprintf(placeRefusal, given, argv[0], where)
		}
	}
	return ""
}

// aimedBy is where p's reply is posted that command's option field, whose value is value, aims a
// write at, "" for none: `--reply-to-ask` of a comment naming p's ask (itemID, namesID: a whole id
// or a prefix of it); `--reply-to` of a comment naming one of p's comments, which Dispatch climbs
// to the head of their thread; `--in-reply-to` of a message or an agent's message naming p's
// message; and `--session` of an agent's message naming p's session, as written.
func (p Place) aimedBy(command, field, value string) string {
	switch {
	case command == "comment" && field == "--reply-to-ask" && namesID(itemID(value, "ask"), p.Ask, true):
		return "on the thread of ask " + p.Ask
	case command == "comment" && field == "--reply-to":
		for _, comment := range p.Comments {
			if namesID(itemID(value, "comment"), comment, false) {
				return "in the thread of comment " + comment
			}
		}
	case (command == "message" || command == "send-to-agent") && field == "--in-reply-to" && namesID(itemID(value, "message"), p.Message, false):
		return "under message " + p.Message
	case command == "send-to-agent" && field == "--session" && p.Session != "" && value == p.Session:
		return "to session " + p.Session
	}
	return ""
}

// itemID is the id value names as an id of an item of kind (ask, comment or message): the id a
// dispatch:// reference to such an item ends with, or a block reference of one
// (`dispatch://KEY/comment/<id>#b-<block id>`), which is how the CLI reads it; else value itself.
func itemID(value, kind string) string {
	ref, ok := strings.CutPrefix(value, "dispatch://")
	if !ok {
		return value
	}
	ref, _, _ = strings.Cut(ref, "#")
	at := strings.LastIndex(ref, "/"+kind+"/")
	if at < 0 {
		return value
	}
	return ref[at+len(kind)+2:]
}

// namesID says whether value names id, an item's id as Dispatch writes it: the same uuid in every
// spelling the CLI reads (envoy-client's normalizeUUID: either case, its 32 hex digits with its
// four hyphens or without them, braced where it has them, and either of those two after
// `urn:uuid:` in any case), or, where prefixes holds, as the CLI reads an ask's id, a prefix of it
// (idPrefix), which the CLI resolves against the issue's open asks. An id that is no uuid is named
// only by itself. No id is named by "" or names anything when it is "".
func namesID(value, id string, prefixes bool) bool {
	if value == "" || id == "" {
		return false
	}
	want, err := uuid.Parse(id)
	if err != nil {
		return value == id
	}
	// github.com/google/uuid's Parse, which Dispatch reads an id by, reads every spelling
	// normalizeUUID does but `urn:uuid:` before the 32 hex digits without hyphens.
	whole := value
	if len(value) == 41 && strings.EqualFold(value[:9], "urn:uuid:") {
		whole = value[9:]
	}
	if got, err := uuid.Parse(whole); err == nil {
		return got == want
	}
	return prefixes && idPrefix.MatchString(value) && strings.HasPrefix(want.String(), strings.ToLower(value))
}

// idPrefix is an id written short, as the CLI reads one (contracts' isIdPrefix): a hex digit and
// seven or more hex digits and hyphens, read without case. A whole uuid is read as itself first.
var idPrefix = regexp.MustCompile(`^[0-9a-fA-F][0-9a-fA-F-]{7,}$`)

// asksNoWrite says whether argv asks the CLI for a command's help or a dry run, which post
// nothing: the CLI prints the command's help for an argument after the command that is `--help`,
// and the arguments as JSON for `--dry-run`, wherever either stands, since it reads no argument
// that starts with `--` as an option's value.
func asksNoWrite(argv []string) bool {
	return len(argv) > 1 && slices.ContainsFunc(argv[1:], func(arg string) bool { return arg == "--help" || arg == "--dry-run" })
}

// option reads argv[i] as the CLI reads an option: its name, "" for an argument that is none, and
// its value, the rest of its word after `=`, else the next argument unless that one is an option
// too, with whether it has one and whether that value is the rest of its word (inline).
func option(argv []string, i int) (name, value string, ok, inline bool) {
	if !strings.HasPrefix(argv[i], "--") {
		return "", "", false, false
	}
	if name, value, ok := strings.Cut(argv[i], "="); ok {
		return name, value, true, true
	}
	if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
		return argv[i], argv[i+1], true, false
	}
	return argv[i], "", false, false
}

// Mark is what the tool adds to the body of each comment, message and agent's message a call
// posts: the loop's marker (intake.AnswerMarker) naming the call, <question>.<seq>, the answer key
// a gh call's Legion footer names, after a blank line, so the loop answers no message its own tool
// posted (answerloop's carriesLoopMark), and so its own reply's marker, <question> and no more,
// which the loop reads a thread for before posting there again, is never this one.
func Mark(question string, seq int) string {
	return fmt.Sprintf("\n\n%s%s.%d -->", intake.AnswerMarker, question, seq)
}

// withMark is argv and stdin as dispatch runs them: for a comment, a message or an agent's message,
// mark added to its body wherever the body comes from, the value of `--body` (or `--body=`), or
// stdin where the body's option is `--body-file -` (or `--body-file=-`); every other call as given.
func withMark(argv []string, stdin, mark string) ([]string, string) {
	if !slices.Contains(marked, argv[0]) {
		return argv, stdin
	}
	run := slices.Clone(argv)
	for i := 1; i < len(run); i++ {
		name, value, ok, inline := option(run, i)
		switch {
		case !ok:
		case name == "--body" && inline:
			run[i] += mark
		case name == "--body":
			run[i+1] += mark
			i++
		case name == "--body-file" && value == "-":
			stdin += mark
		}
	}
	return run, stdin
}

// UnknownOutcome is the result a restart records for a dispatch call the daemon stopped while it
// ran: Dispatch is not read for it, since the loop's mark is no key Dispatch can be searched by,
// so the model is told to read before it writes again.
const UnknownOutcome = "The daemon stopped while this call ran; what it did is unknown. Read Dispatch before you repeat a write."

// Write is one dispatch call that writes which the answer loop ran for a question (Writes): its
// seq among the question's calls, its command, and dispatch's exit status, nil where dispatch
// answered none, since it could not start or the daemon stopped while it ran.
type Write struct {
	Seq     int
	Command string
	Exit    *int
}

// Writes are the dispatch calls of calls, as answer_tool_calls records them, that write and that
// dispatch ran, or may have, in their order: every command but ReadCommands, the CLI's own reads,
// so a command the tool does not know counts as a write, and a call the daemon stopped during
// counts too. A call asking for help or a dry run, which posts nothing (asksNoWrite), is none; so
// is a call the tool refused before dispatch started, which it records with its refusal and no
// exit, and a call of another tool.
func Writes(calls []store.AnswerToolCall) []Write {
	var written []Write
	for _, call := range calls {
		if call.Tool != store.ToolDispatch || call.RefusedBeforeRun() || asksNoWrite(call.Args) {
			continue
		}
		command := ""
		if len(call.Args) > 0 {
			command = call.Args[0]
		}
		if slices.Contains(ReadCommands, command) {
			continue
		}
		written = append(written, Write{Seq: call.Seq, Command: command, Exit: call.Exit})
	}
	return written
}

// CallWrites says whether a call the model asks for, by args, its arguments as Call carries them
// (a JSON array of strings), writes (writes): it runs one of WriteCommands, the four writes the
// tool runs, and asks for neither help nor a dry run. Every other call writes nothing: a read, a
// write's help or dry run, and a command the tool does not run or arguments that are no such
// array, which Run refuses before dispatch starts. The answer refuses each call that writes while
// the project is paused (internal/answer's toolSet), and runs the rest.
func CallWrites(args string) bool {
	argv, err := ghtool.DecodeArgs(args, "dispatch")
	return err == nil && writes(argv)
}

// writes says whether argv, a call's arguments, writes: its command is one of WriteCommands and
// it asks the CLI for neither help nor a dry run (asksNoWrite). CallWrites reads a call the model
// asks for by it, and each call's `answers: dispatch` line says by it whether a call that ran
// wrote (Answer.log).
func writes(argv []string) bool {
	return len(argv) > 0 && slices.Contains(WriteCommands, argv[0]) && !asksNoWrite(argv)
}
