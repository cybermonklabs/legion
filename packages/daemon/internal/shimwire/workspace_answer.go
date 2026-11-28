package shimwire

import (
	"fmt"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/redact"
)

// MaxAnswerErrorBytes is the most of a refusal's reason that a one-line answer to the daemon
// carries: the answer to `workspace-work` (AnswerWorkspaceWork) and the shim's answers to
// `adopt-working-copy` and to `start-at-branch`. A failed command's reason holds all of jj's
// standard error (workspace.RunCommand), which has no bound of its own. Such an answer goes out as
// one line, and the writer refuses a line over MaxFrameBytes: the outbox then closes the
// connection and sends that line first again at each reconnect, so the role's stream stays down
// (ASVS v5.0.0-15.2.2). A cut reason keeps its start, which names the command, its exit and jj's
// first words, and its end, where workspace.RunCommand puts the note it adds to a failure
// (workspace.StaleGitLockNote, which tells the role what to do about a git lock). The daemon names
// the reason on its log lines, and for the read in the message on the issue too, which a short
// reason keeps readable.
const MaxAnswerErrorBytes = 4 << 10

// CutAnswerError is reason as a one-line answer carries it: with any credential a URL in it names
// taken out (redact.URLCredentials), all of it when it is at most MaxAnswerErrorBytes, and otherwise
// its first and its last half of that bound, each cut where a UTF-8 character starts, with a line
// between them that says how many bytes were cut there and how long the reason was.
func CutAnswerError(reason string) string {
	reason = redact.URLCredentials(reason)
	if len(reason) <= MaxAnswerErrorBytes {
		return reason
	}
	head := MaxAnswerErrorBytes / 2
	for head > 0 && !utf8.RuneStart(reason[head]) {
		head--
	}
	tail := len(reason) - MaxAnswerErrorBytes/2
	for tail < len(reason) && !utf8.RuneStart(reason[tail]) {
		tail++
	}
	return fmt.Sprintf("%s\n[%d bytes of this error were cut here. The error had %d bytes.]\n%s",
		reason[:head], tail-head, len(reason), reason[tail:])
}

// AnswerWorkspaceWork is the one answer to a `workspace-work` request, whichever process makes the
// read (workspace.ReadWork): a role's shim (internal/shim's workspaceWork), or, where no role
// process runs in the pod, the pod's launcher (internal/launcher's readWorkspace, R6 of
// #5285). A request that fails validation is answered with the reason where it
// names an id, and where it names none it is no answer at all: refused is that reason, which the
// caller logs before it drops the frame. A read that fails is answered with its error, cut to one
// line (CutAnswerError). A read that succeeds lists the first MaxWorkspaceWorkCommits work commits
// and counts them all.
func AnswerWorkspaceWork(request WorkspaceWork, read func() (head string, changed bool, work []string, err error)) (result WorkspaceWorkResult, refused error) {
	result = WorkspaceWorkResult{ID: request.ID, OK: true}
	if err := request.Validate(); err != nil {
		if request.ID == "" {
			return WorkspaceWorkResult{}, err
		}
		result.OK, result.Error = false, CutAnswerError(err.Error())
		return result, nil
	}
	head, changed, work, err := read()
	if err != nil {
		result.OK, result.Error = false, CutAnswerError(err.Error())
		return result, nil
	}
	result.Head, result.Changed, result.Count = head, changed, len(work)
	result.Commits = work[:min(len(work), MaxWorkspaceWorkCommits)]
	return result, nil
}
