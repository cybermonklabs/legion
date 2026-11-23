package leakguard

import (
	"strings"

	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/termsafe"
)

// RefusedBy is the check a refusal names. Its two values are the two constants below: a named
// type rather than a bare string, so a reader of a Refusal sees which values the field holds. An
// untyped string constant still converts to it, so the type documents the two values and does
// not enforce them.
type RefusedBy string

// The two sentences a refusal names the refusing check by. byRules is this package's own rule
// pass; bySharedCheck is the shared public leak check, whose own report the refusal carries.
const (
	byRules       RefusedBy = "this repository's own private-name rules match it"
	bySharedCheck RefusedBy = "the shared public leak check refused it"
)

// Refusal is a write refused because of the text it carries. It is one of the three answers this
// package refuses a write with; the others are *SetupFailure and *NotFound. All three refuse the
// write, and a call site tells them apart with errors.As to say which one it met.
type Refusal struct {
	// What is the write, in the entry point's words: "the push to origin", "the gh call".
	What string
	// By names the check that refused: byRules or bySharedCheck.
	By RefusedBy
	// Lines is the report. The rule pass writes one line per refusal, each naming the record and
	// the class and nothing else, with a path masked as check-private-names.sh masks one. A
	// refusal by the shared check carries that check's own report instead, which is the only
	// account of it anyone gets.
	Lines []string
}

// Error, and the Error of a *SetupFailure and a *NotFound, write each part of the answer that is
// not this file's own words as shown writes it: through the URL rule (redact.URLCredentials), so
// every URL in it keeps no userinfo and no query, and then escaped (termsafe.Escaped), so no
// control byte in it reaches a log or a terminal raw. An answer's What can hold a URL with a
// credential: git hands a pre-push hook the remote's URL as its name too for a push to a URL rather
// than to a named remote, and PushName writes that name into What. It can hold a local path, which
// the push's author chose, as can a refusal's line, which names a path. A SetupFailure's Why can
// quote a caller's argument (a gh call's --body-file, readGhFile), and a failure underneath what
// git wrote on its standard error, which can quote a URL or a path. `legion` passes every
// command's stderr through the URL rule too (cmd/legion's newRoot), but an in-process caller can
// write an answer anywhere, so each answer applies both itself. A backslash is written as it is,
// so the shared check's report lines, already escaped by its own printer, pass unchanged. A
// remote's name that is no URL ("origin") reads as written.
func (r *Refusal) Error() string {
	var b strings.Builder
	b.WriteString("LEAK CHECK: refusing ")
	b.WriteString(shown(r.What))
	b.WriteString(": ")
	b.WriteString(string(r.By))
	for _, line := range r.Lines {
		b.WriteString("\n  ")
		b.WriteString(shown(line))
	}
	return b.String()
}

// SetupFailure is a write refused because a check could not be made to run: no shared check
// resolved, the shared check exited 2 or some status it does not document, the rule engine failed,
// or a destination's visibility could not be read, other than a read GitHub answered 404, which is
// a *NotFound. It refuses the write exactly as a *Refusal does — a check that did not run has
// cleared nothing — and says so in the shared check's own words, so that a person reading a log
// can tell a broken pod from a write that names something private.
type SetupFailure struct {
	// What is the write, in the entry point's words, as a Refusal's is.
	What string
	// Why is what could not be made to run.
	Why string
	// Err is the failure underneath, where there is one.
	Err error
}

// Error is the setup failure as a log or a terminal shows it, with the write's name, the reason
// and the failure underneath each written as shown writes it (Refusal.Error). The failure
// underneath keeps its own line breaks, each of its lines shown alone, so a git's warnings and its
// `fatal:` line still read as the lines they are.
func (f *SetupFailure) Error() string {
	message := "LEAK CHECK SETUP ERROR: " + shown(f.What) + ": " + shown(f.Why)
	if f.Err != nil {
		lines := strings.Split(f.Err.Error(), "\n")
		for i, line := range lines {
			lines[i] = shown(line)
		}
		message += ": " + strings.Join(lines, "\n")
	}
	return message
}

func (f *SetupFailure) Unwrap() error {
	return f.Err
}

// NotFound is a write refused because GitHub answered the destination's visibility read with 404:
// no repository of that name exists, or the read's credential cannot see it. The guard read no
// visibility, so it refuses the write as it does for a *SetupFailure, but it says what GitHub
// answered and holds neither marker: the write named nothing private, and no check failed to run,
// so a person who read either marker would look for the wrong fault.
type NotFound struct {
	// What is the write, in the entry point's words, as a Refusal's is.
	What string
	// Repo is the destination, "owner/name", already masked as a report masks a path (maskPath).
	Repo string
}

func (f *NotFound) Error() string {
	return shown(f.What) + ": " + notFoundText(f.Repo)
}

// notFoundText is what a *NotFound and the read's own *notFoundError say of repo, a repository
// GitHub answered 404 for, repo already masked.
func notFoundText(repo string) string {
	return "GitHub answers 404 for " + repo + ", so the repository does not exist or this credential cannot read it"
}

// shown is s, a part of an answer that is not this file's own words, as an answer writes it: every
// URL in it without its userinfo and its query (redact.URLCredentials), and then each control byte
// escaped (termsafe.Escaped), uncut. The URL rule runs first, on the text as it is: it ends a URL at
// white space, which the escaping would write as letters.
func shown(s string) string {
	return termsafe.Escaped(redact.URLCredentials(s), 0)
}
