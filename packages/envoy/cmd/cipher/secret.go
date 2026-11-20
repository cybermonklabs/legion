// packages/envoy/cmd/cipher/secret.go
//
// The secret forms: a person or an agent reads and writes agent secrets in the broker's own secret
// store - AWS Secrets Manager (secret_aws.go) or GCP Secret Manager (secret_gcp.go), chosen by the
// broker's `store` setting - under the cloud credential its environment gives it, whatever kind of
// identity that is, then asks the broker to reread each one written, so the change is served at
// once. The broker is asked only for its settings (GET /v1/settings: the namespace, key and which
// store it uses) and for the reread; every read and write is the caller's own call to the store,
// which the store's own IAM decides. The reread is signed with a broker identity this process
// already holds (heldSigner), since the broker says what it now serves only to a session; with
// none, or where that proof fails, it goes unsigned, and the broker takes it all the same.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/buildversion"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipherclient"
)

type secretForm struct {
	help command
	run  func([]string, io.Writer, io.Writer) int
}

// secretForms is the one table for dispatch, form names and help text.
var secretForms []secretForm

func init() {
	// Register after variable initialization: the handlers use commands for help.
	secretForms = []secretForm{
		{command{"secret list", "agent-secrets secret list [--json]",
			"List every agent secret, under the environment's cloud credential: its name, owner, tier, type,\nmax grant, default grant, whether it has a value, and its deletion once it is deleted,\nwith the earliest purge beside it on Secrets Manager; --json also prints each one's store,\nsecret name, github-owners, requesters, scopes and admin mark, and its created and\nlast-changed times where the store gives them. Name the store the rows are in."}, cmdSecretList},
		{command{"secret show", "agent-secrets secret show NAME [--json]",
			"Print an agent secret's name, store, secret name, owner, tier, type, github-owners,\nrequesters, scopes, admin mark, max grant, default grant, its created and\nlast-changed times, its deletion once it is deleted, with the earliest purge beside\nit on Secrets Manager, and its versions; --json prints those and whether it has a value.\nNever print its value."}, cmdSecretShow},
		{command{"secret create", "agent-secrets secret create NAME --owner me|shared|EMAIL --tier agent|human [--type " + strings.Join(policy.Types, "|") + "|none] [--github-owners LOGINS] [--requesters LIST] [--scopes LIST] [--admin true] [--max-grant DURATION] [--default-grant session]",
			"Create an agent secret under the environment's cloud credential, then ask the broker to serve it.\n--owner me is the email of the person that credential names, and is refused where it names\nnone; --owner EMAIL names the owning person by email. Read its value from standard input, or\ntype it at a prompt with echo off."}, cmdSecretCreate},
		{command{"secret set", "agent-secrets secret set NAME",
			"Set a new agent-secret value under the environment's cloud credential. Then ask the broker to\nserve it. Read the value from standard input, or type it at a prompt with echo off."}, cmdSecretSet},
		{command{"secret retag", "agent-secrets secret retag NAME [--owner me|shared|EMAIL] [--tier agent|human] [--type " + strings.Join(policy.Types, "|") + "|none] [--github-owners LOGINS|none] [--requesters LIST|none] [--scopes LIST|none] [--admin true|none] [--max-grant DURATION|none] [--default-grant session|none]",
			"Change an agent secret's owner, tier, type, GitHub owners, requesters, scopes, admin mark,\nmax grant or default grant under the environment's cloud credential. Then ask the broker\nto reread it. none removes a type, GitHub owners, requesters, scopes, the admin mark, a\nmax grant or a default grant, in a retag of its own. An administrator changes a shared secret's\nowner and tier. This CLI cannot tell whether a requesters entry naming a service is one the\ndeployment registers: it holds no list of them, so the broker's answer to the reread is what\ncatches an unregistered name, printing its refusal and its reason, and the retag exits 1 with\nthe tag written."}, cmdSecretRetag},
		{command{"secret delete", "agent-secrets secret delete NAME",
			"Schedule the agent secret NAME's deletion, restorable for 30 days, under the environment's\ncloud credential, then ask the broker to stop serving it at once."}, cmdSecretDelete},
		{command{"secret restore", "agent-secrets secret restore NAME",
			"Cancel the agent secret NAME's scheduled deletion under the environment's cloud credential,\nthen ask the broker to serve it again at once."}, cmdSecretRestore},
	}
	execForm := commands[len(commands)-1]
	commands = commands[:len(commands)-1]
	for _, form := range secretForms {
		commands = append(commands, form.help)
	}
	commands = append(commands, execForm)
}

func secretFormNames() string {
	var names strings.Builder
	for i, form := range secretForms {
		if i != 0 {
			names.WriteString(", ")
		}
		names.WriteString(strings.TrimPrefix(form.help.name, "secret "))
	}
	return names.String()
}

// recoveryWindowDays is the recovery window delete schedules every deletion with, 30 days: the
// longest Secrets Manager allows, during which restore brings the secret back.
const recoveryWindowDays = 30

// sharedRetagRefused is what a refused retag of a shared secret, or of one the person asked to make
// shared, adds to AWS's own refusal: IAM lets only an administrator set a shared secret's owner or
// tier.
const sharedRetagRefused = "a shared secret's owner and tier are an administrator's to change"

// cmdSecret dispatches the secret forms.
func cmdSecret(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "agent-secrets secret: a form is required: %s\n", secretFormNames())
		return exitUsageError
	}
	switch args[0] {
	case "-h", "-help", "--help":
		for i, form := range secretForms {
			if i > 0 {
				fmt.Fprintln(stderr)
			}
			writeCommandHelp(stderr, form.help)
		}
		return 0
	}
	// --profile was a flag of the AWS backend (cybermonklabs/legion#1833) until #4130; the
	// tool now reads the store credential from the environment, as the cloud's own tools do, so a
	// form given --profile is a usage error naming the variables to use instead, and those that
	// outrank them.
	for _, a := range args[1:] {
		flagName, _, _ := strings.Cut(a, "=")
		if strings.HasPrefix(flagName, "-") && strings.TrimLeft(flagName, "-") == "kind" {
			fmt.Fprintf(stderr, "agent-secrets secret: flag provided but not defined: -%s; use --type on secret create or retag\n", "kind")
			return exitUsageError
		}
		if a == "--profile" || strings.HasPrefix(a, "--profile=") {
			fmt.Fprintln(stderr, "agent-secrets secret: --profile is not a flag: agent-secrets reads the store credential from the environment, as the cloud's own tools do. On Google Cloud set CLOUDSDK_ACTIVE_CONFIG_NAME (a gcloud configuration) or CLOUDSDK_CORE_ACCOUNT (a gcloud account), which CLOUDSDK_AUTH_ACCESS_TOKEN and GOOGLE_APPLICATION_CREDENTIALS outrank when set; on AWS set AWS_PROFILE, which access keys in AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, and AWS_WEB_IDENTITY_TOKEN_FILE, outrank when set, as in the AWS CLI.")
			return exitUsageError
		}
	}
	for _, form := range secretForms {
		if form.help.name == "secret "+args[0] {
			return form.run(args[1:], stdout, stderr)
		}
	}
	// The argument is not printed, as cmdGroup's is not.
	fmt.Fprintf(stderr, "agent-secrets secret: unknown form (not printed); the forms are %s\n", secretFormNames())
	return exitUsageError
}

// requireTrustedBrokerURL refuses a non-https broker URL unless the broker's host is loopback
// (127.0.0.0/8, ::1 or localhost), for the two paths that act on the broker's settings: the
// secret forms, which openSession gates, and the machine source, which reads the audience its
// identity token is minted for (machineSource). The exec form's other calls and the pod paths
// keep today's behaviour, and a form's -h, which never opens a session, is unaffected. The
// settings steer a write and choose whom the machine's identity token is good for, so a
// plain-http broker a network attacker can answer is refused before the first call; an
// unparseable URL is left to the client, which names it. It reads the address (Raw) only to
// parse its scheme and host, and names it as the Address prints.
func requireTrustedBrokerURL(base cipheraddr.Address) error {
	if base.Empty() {
		return nil
	}
	u, err := url.Parse(base.Raw())
	if err != nil {
		return nil
	}
	if u.Scheme == "https" || (u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return nil
	}
	return usageErr{fmt.Errorf("AGENT_SECRETS_URL %q must be https, or http only when the broker's host is loopback (127.0.0.1, [::1] or localhost): this client acts on the broker's settings (the store a secret form writes, the audience of the identity token this machine mints), so a plain-http broker on the network is refused", base)}
}

// isLoopbackHost reports whether host is "localhost" (any case) or a loopback IP literal
// (127.0.0.0/8 or ::1).
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// usageErr is a secret form's usage error, which exits 2.
type usageErr struct{ error }

// secretFail prints a secret form's failure and answers its exit code: 2 for a usage error, 1 for
// anything else.
func secretFail(stderr io.Writer, form string, err error) int {
	fmt.Fprintf(stderr, "agent-secrets %s: %v\n", form, err)
	var usage usageErr
	if errors.As(err, &usage) {
		return exitUsageError
	}
	return 1
}

// secretFlagValues are the secret forms' flags that take a value.
var secretFlagValues = map[string]bool{"owner": true, "tier": true, "type": true, "github-owners": true, "requesters": true, "scopes": true, "admin": true, "max-grant": true, "default-grant": true}

// secretFlags is form's flag set. The store credential comes from the environment, never a flag
// (cmdSecret refuses --profile), so no form registers one for it.
func secretFlags(form string, stderr io.Writer) *flagSet {
	return newFlagSet(form, stderr)
}

// secretName is the one NAME a form takes, and its slug: the secret's name under the prefix. A
// NAME that is no secret name is refused with the rule it broke and not printed, as a tag flag's
// value is not (valueNotPrinted): a NAME is where a person who pasted the value in place of the
// name puts it.
func secretName(positional []string) (name, slug string, err error) {
	if len(positional) != 1 {
		return "", "", usageErr{errors.New("exactly one secret NAME is required")}
	}
	slug, err = policy.NameToSlug(positional[0])
	if err != nil {
		return "", "", usageErr{fmt.Errorf("NAME (not printed): %w", err)}
	}
	return positional[0], slug, nil
}

// secretStdin is where create and set read a secret's value from; tests replace it.
var secretStdin io.Reader = os.Stdin

// stdinTerminal answers r's file descriptor when r is a terminal, where create and set prompt for
// the value instead of reading r to its end; tests replace it.
var stdinTerminal = func(r io.Reader) (fd int, ok bool) {
	f, isFile := r.(*os.File)
	if !isFile || !term.IsTerminal(int(f.Fd())) {
		return 0, false
	}
	return int(f.Fd()), true
}

// Unix core protection stays in place for the process lifetime, including after
// a prompt or piped value reaches the AWS client.
var disableCoreDumps = sync.OnceValue(preventCoreDumps)

// readHidden reads one hidden line from terminal fd, less its line ending; tests
// replace it. It calls prompt to show the label once it holds the terminal with echo
// off, so a prompt started in the background shows nothing over the shell's own line.
// The reader drains bracketed pastes through their end, and input following an
// unbracketed line through a bounded quiet window. deadline, when it is not the zero
// time, is when the read gives up with errValueTimedOut; the zero time sets no bound.
var readHidden = readHiddenAtTerminal

// onSignalDeath, while set, runs once just before a hang-up, an interrupt, a quit or a termination
// ends the form by that signal (endBySignal). Create sets it from before it asks the store to make
// the secret until that secret holds its value or the discard has run (pendingCreate.watch,
// pendingCreate.interrupted), so a signal in that stretch deletes the empty secret rather than
// leaving it, and says what the create left; nil otherwise.
var onSignalDeath func()

// errMoreThanOneLine is readHidden's answer when more input followed the first line at the prompt,
// as a paste of a value of more than one line does: the prompt reads one line, and the rest would
// reach the shell once the form exits.
var errMoreThanOneLine = errors.New("more than one line was entered at the prompt")

// errPasteCutShort is readHidden's answer when the terminal hung up inside a bracketed paste,
// before its closing mark, or when the paste fell quiet for maxPasteDrain without its closing mark
// coming: what was read is not the whole value, so none of it is.
var errPasteCutShort = errors.New("the paste was cut short before its closing mark")

// errValueCutShort is readHidden's answer when the terminal hung up outside a paste, before the
// person ended the value: what was typed is not necessarily all they meant to enter.
var errValueCutShort = errors.New("the terminal hung up before the value ended")

// A stop discards what the terminal holds unread (the watcher's flush) without a count of
// how much. The entire entry must be refused, even when the reader had already received a prefix.
var errPromptStopped = errors.New("the terminal stopped while reading the value")

// errValueTimedOut is readSecretValue's answer, from either arm, when no whole value came before
// its deadline.
var errValueTimedOut = errors.New("no value came in time")

// valueDeadlineNotice is the reason the value prompt hands onDiscard when the value deadline passes
// with an entry under way (it is errValueTimedOut): whether a bracketed paste was open then, inside
// which a signal key is pasted text, and the byte of the interrupt key that ends the process by
// SIGINT, 0 where no key does (none set, SIGINT inherited as ignored, or a terminal that is not
// this process's controlling one). The notice names only what ends the entry from there.
type valueDeadlineNotice struct {
	inPaste   bool
	interrupt byte
}

func (valueDeadlineNotice) Error() string { return errValueTimedOut.Error() }

func (valueDeadlineNotice) Is(target error) bool { return target == errValueTimedOut }

// ends says how the entry under way ends, and so when a create deletes the empty secret it made:
// a paste must end before Enter can end the entry; the interrupt key, where it ends the process,
// deletes it at once; and where it does not, ending the process does.
func (n valueDeadlineNotice) ends() string {
	switch {
	case n.inPaste:
		return "this entry ends: finish the paste, then press Enter, and the create deletes it then"
	case n.interrupt != 0:
		return "you press Enter to end this entry, and the create deletes it then; " + keyName(n.interrupt) + " deletes it now"
	}
	return "you press Enter to end this entry, or end this process (close this terminal, or send it SIGTERM), and the create deletes it then"
}

// keyName is what a person calls the key that sends c: a control byte by its own name or its
// Ctrl- chord (promptControlByteError.name, "Ctrl-C" for 0x03), a printable character as that key
// ("the Z key", the space bar), and any other byte by its number.
func keyName(c byte) string {
	switch {
	case c < 0x20 || c == 0x7f:
		return promptControlByteError{c: c}.name()
	case c == ' ':
		return "the space bar"
	case c < 0x7f:
		return fmt.Sprintf("the %c key", c)
	}
	return fmt.Sprintf("the key that sends byte 0x%02x", c)
}

// valueReadWindow is how long `secret create` reads the value of the secret it made before it
// gives up and discards that secret: policy.PendingValueWindow, the time the broker holds such a
// secret as waiting for its value (pending-value), so the create ends its read when the broker
// stops waiting. The create counts it from before the store made the secret, so its read never
// outlasts the broker's window, and hands readSecretValue the moment it ends. Tests shorten it.
var valueReadWindow = policy.PendingValueWindow

// errNoForeground is readHidden's answer when its process group is orphaned while it waits in the
// background (the shell that started it has gone): no shell can ever hand it the terminal, so it
// refuses before showing anything rather than waiting forever.
var errNoForeground = errors.New("no shell can bring the value prompt to the foreground of this terminal")

// errNoValuePrompt is readHidden's answer on a platform the value prompt does not run on
// (secret_prompt_other.go): the value must be piped in.
var errNoValuePrompt = errors.New("no value prompt is available on this platform")

// promptControlByteError is an unhandled control byte typed or pasted at the prompt: c, the first
// one the line held, and whether the value held text before it (textBefore) and after it
// (textAfter), which say where it came. Its parts let the form turn the reader error into a usage
// error that names the invisible input and where it is, so a person can tell a Tab a paste added
// at an end from one inside the value.
type promptControlByteError struct {
	c                     byte
	textBefore, textAfter bool
}

// Error names the byte by its number and its name, and where it came, with the note, for a Tab at
// one end, that a paste often adds one there.
func (e promptControlByteError) Error() string {
	name := e.name()
	article := "a"
	if strings.ContainsRune("AEIOU", rune(name[0])) {
		article = "an"
	}
	where := "inside the value"
	switch {
	case !e.textBefore && !e.textAfter:
		where = "as the whole value"
	case !e.textAfter:
		where = "at the end of the value"
	case !e.textBefore:
		where = "at the start of the value"
	}
	note := ""
	if e.c == '\t' && e.textBefore != e.textAfter {
		note = ", where a paste often adds one"
	}
	return fmt.Sprintf("the control byte 0x%02x cannot be typed at the prompt: %s %s %s%s", e.c, article, name, where, note)
}

// name is what a person calls the byte: the key that sends it where it has a name of its own, and
// the Ctrl- chord that types it otherwise.
func (e promptControlByteError) name() string {
	switch e.c {
	case 0x00:
		return "NUL"
	case '\b':
		return "Backspace"
	case '\t':
		return "Tab"
	case '\v':
		return "vertical tab"
	case '\f':
		return "form feed"
	case 0x1b:
		return "Escape"
	case 0x7f:
		return "Delete"
	}
	return "Ctrl-" + string(rune('@'+e.c))
}

// shellUnsafe is any character outside the set a POSIX shell reads literally in a bare word.
var shellUnsafe = regexp.MustCompile(`[^A-Za-z0-9_./:@-]`)

// shellWord is s as one shell word: bare when every character is literal, else single-quoted with
// each `'` closed, escaped and reopened.
func shellWord(s string) string {
	if s != "" && !shellUnsafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// pipeCommand is the command line a form names for piping a value in: prefix (the backend's
// signInPrefix), agent-secrets, then words, each shell-quoted, so running it signs in the same way.
func pipeCommand(prefix string, words ...string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = shellWord(w)
	}
	return prefix + "agent-secrets " + strings.Join(quoted, " ")
}

// readSecretValue reads the value of the secret name for a form, giving up with
// errValueTimedOut at deadline, or never where deadline is the zero time; a value read whole only
// after deadline is refused the same way. At a terminal it prompts on stderr, once it holds the
// terminal with echo off, naming deadline where there is one, and reads one line (readHidden),
// as `gh secret set` does, so the value never shows on the screen; a value of more than one line
// is refused there, naming pipeTo, the form's own command line (pipeCommand) with the value piped
// in. Otherwise it reads all of secretStdin.
//
// Both arms then meet one tail: it refuses a value that is not valid UTF-8, whichever arm read
// it, and then reads the value once, by policy.TrimLineEnding, the broker's own rule, so a value
// stored through this client and a value stored by any other writer are spent alike — the broker
// reads what it releases by that same rule, and a reader of its own here would drift from it.
// Where the rule changed nothing — a value the rule leaves alone, a PEM one among them — one
// final line feed goes, which is what
// this reader has always removed, so `echo` and a file ending in a newline give the value without
// one. A line typed at the prompt carries no ending at all, so the terminal arm meets the
// fallback and nothing else. An empty value is a usage error either way, in its own arm's words.
func readSecretValue(name, pipeTo string, deadline time.Time, stderr io.Writer) (string, error) {
	if err := disableCoreDumps(); err != nil {
		return "", err
	}
	// value is what the arm read, and empty the refusal of a value that is empty once read.
	var value, empty string
	if fd, ok := stdinTerminal(secretStdin); ok {
		prompted := false
		line, err := readHidden(fd, deadline, func() {
			prompted = true
			if !deadline.IsZero() {
				fmt.Fprintf(stderr, "Enter the value by %s, %s after the create, or nothing is stored.\n", deadline.Format("15:04:05 MST"), valueWindowText())
			}
			fmt.Fprintf(stderr, "Value for %s: ", name)
		}, func(reason error) {
			// Only a create's read has a deadline, and only where it made the secret first (GCP):
			// the prompt reads on until this entry ends, and the create deletes that empty secret
			// only then, or at once on a signal that ends the process (pendingCreate.interrupted).
			var timedOut valueDeadlineNotice
			if errors.As(reason, &timedOut) {
				fmt.Fprintf(stderr, "\nThe value deadline passed with an entry under way: no value came within %s of the create, so none is stored. The empty secret %s stays in the store until %s. Then run %s again and type the whole value.\n", valueWindowText(), name, timedOut.ends(), pipeTo)
				return
			}
			fmt.Fprintf(stderr, "\nNothing was stored. Press Enter to finish discarding this entry, then run %s again and type the whole value.\n", pipeTo)
		})
		if prompted {
			// Echo is off, so the line ending the person typed never reached the screen.
			fmt.Fprintln(stderr)
		}
		if errors.Is(err, errMoreThanOneLine) {
			return "", usageErr{fmt.Errorf("a value of more than one line must be piped in: %s < FILE", pipeTo)}
		}
		if errors.Is(err, errPromptStopped) {
			return "", usageErr{fmt.Errorf("nothing was stored: %w; run %s again and type the whole value", err, pipeTo)}
		}
		if errors.Is(err, errNoForeground) || errors.Is(err, errNoValuePrompt) {
			return "", usageErr{fmt.Errorf("%w; pipe the value in: %s < FILE", err, pipeTo)}
		}
		var control promptControlByteError
		if errors.As(err, &control) {
			return "", usageErr{fmt.Errorf("%w. Nothing was stored, and the value was not trimmed: enter it again without the %s, or, if the %s is part of it, pipe the value in: %s < FILE", control, control.name(), control.name(), pipeTo)}
		}
		if errors.Is(err, errValueTimedOut) {
			return "", valueTimedOut(pipeTo)
		}
		if err != nil {
			return "", fmt.Errorf("read the value at the terminal: %w", err)
		}
		value, empty = string(line), "no value was entered at the prompt"
	} else {
		data, err := readAllBy(secretStdin, deadline)
		if errors.Is(err, errValueTimedOut) {
			return "", valueTimedOut(pipeTo)
		}
		if err != nil {
			return "", fmt.Errorf("read the value from standard input: %w", err)
		}
		value, empty = string(data), "the value is read from standard input, which was empty"
	}
	// The prompt reads on through a paste, and through what follows the line, however long that
	// takes, so a value can come whole only after the deadline: it did not come in time, and a
	// create stores no value once the broker has stopped holding its secret as waiting for one.
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return "", valueTimedOut(pipeTo)
	}
	if !utf8.ValidString(value) {
		return "", usageErr{errors.New("the value must be valid UTF-8")}
	}
	stored, changed := policy.TrimLineEnding(value)
	if !changed {
		stored = strings.TrimSuffix(stored, "\n")
	}
	if stored == "" {
		return "", usageErr{errors.New(empty)}
	}
	return stored, nil
}

// valueTimedOut is the error a create's read of its value ends with at its deadline: nothing was
// stored, how long the read lasted, and the command to run again. It is no usage error: the
// command was right, and the person or the pipe was too slow.
func valueTimedOut(pipeTo string) error {
	return fmt.Errorf("nothing was stored: %w, within %s of the create, as long as the broker holds a new secret waiting for its value, so the create gave up; run %s again", errValueTimedOut, valueWindowText(), pipeTo)
}

// valueWindowText is valueReadWindow as a line says it: whole minutes as such ("10 minutes"), any
// other length as Go writes it.
func valueWindowText() string {
	if valueReadWindow >= time.Minute && valueReadWindow%time.Minute == 0 {
		return fmt.Sprintf("%d minutes", valueReadWindow/time.Minute)
	}
	return valueReadWindow.String()
}

// readAllBy reads all of r, or answers errValueTimedOut at deadline when r has not ended by then;
// the zero deadline reads with no bound. A read it gives up on stays blocked, which costs nothing:
// the form ends at once.
func readAllBy(r io.Reader, deadline time.Time) ([]byte, error) {
	if deadline.IsZero() {
		return io.ReadAll(r)
	}
	type read struct {
		data []byte
		err  error
	}
	done := make(chan read, 1)
	go func() {
		data, err := io.ReadAll(r)
		done <- read{data, err}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case got := <-done:
		return got.data, got.err
	case <-timer.C:
		return nil, errValueTimedOut
	}
}

// noteEdgeSpaces prints, once a form has stored value as name's value, one line on stderr when
// value starts or ends with a space: a space can be part of a secret, so the form stores the value
// as given, neither trimmed nor refused, but one at an end is more often a paste's or a typing
// slip than the secret's own, and nothing else shows it. The line names the secret and the end,
// never the value.
func noteEdgeSpaces(stderr io.Writer, form, name, value string) {
	starts, ends := strings.HasPrefix(value, " "), strings.HasSuffix(value, " ")
	var where string
	switch {
	case starts && ends:
		where = "starts and ends"
	case starts:
		where = "starts"
	case ends:
		where = "ends"
	default:
		return
	}
	fmt.Fprintf(stderr, "agent-secrets %s: the value of %s %s with a space; it was stored as given, the space included\n", form, name, where)
}

// checkValueFor refuses value, which a secret of type (a policy.Types entry) holds or is about to
// hold, when the broker's minter for type would refuse it: it decodes
// the value by the broker's own rule (policy.DecodeValue: the value its release hands that
// minter, decoded with secrets.ParseValue, the decoder every minter calls), so the refusal names
// what is wrong in the minter's own words and never quotes the value. Create and set call it
// before they write, and retag on the value it read (checkRetagValue); a refusal is a usage
// error, as readSecretValue's refusals of a value are.
func checkValueFor(secretType, value string) error {
	if err := policy.DecodeValue(secretType, value); err != nil {
		return usageErr{fmt.Errorf("the broker refuses this value as %s secret's, so nothing is written: %w", withArticle(secretType), err)}
	}
	return nil
}

// withArticle is type, a type tag's value, after the article it takes, by its first letter: "a
// github-app", "an azure-login". A message that names a type it holds in a variable writes it
// through this, since the types do not all take "a".
func withArticle(secretType string) string {
	if secretType != "" && strings.ContainsRune("aeiou", rune(secretType[0])) {
		return "an " + secretType
	}
	return "a " + secretType
}

// minRecoveryWindowDays is the shortest recovery window Secrets Manager allows a deletion, 7 days.
const minRecoveryWindowDays = 7

// earliestPurge is the earliest moment Secrets Manager can purge a secret scheduled for deletion,
// read back from the DeletedDate it answers for it (DescribeSecret's and ListSecrets'); nil while
// it is not scheduled. DeletedDate is the moment the delete ran (measured against Secrets
// Manager), and neither call answers the recovery window the delete used, which can be anything
// from 7 to 30 days, so the earliest the secret can be gone is DeletedDate plus the 7-day minimum:
// a reader told this date never waits past the purge to restore it. It is the one place that reads
// DeletedDate. delete itself prints DeleteSecret's own DeletionDate, the exact end of the window it
// scheduled.
func earliestPurge(deletedDate *time.Time) *time.Time {
	if deletedDate == nil {
		return nil
	}
	t := deletedDate.AddDate(0, 0, minRecoveryWindowDays)
	return &t
}

// formatTime is t in UTC as RFC 3339, or "-" when Secrets Manager answered none.
func formatTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

// orDash is s, or "-" when it is empty.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// secretBackend is the per-store backend the secret forms call: AWS Secrets Manager
// (secrets.StoreAWS, or "" from a broker that predates the `store` setting) or GCP Secret
// Manager (secrets.StoreGCP), chosen from the broker's GET /v1/settings `store` field. Every
// method takes the secret's name under the prefix (secretName's slug); the full store-side name
// (each backend's own id) is each backend's own concern.
type secretBackend interface {
	// requireAccount refuses, before any read or write, a sign-in whose account does not hold the
	// agent secrets: a read there would list another account's secrets as if they were the agent
	// secrets. AWS only: GCP needs no equivalent (gcpBackend.requireAccount says why). AWS keeps
	// the sign-in it read for personEmail and identity.
	requireAccount(ctx context.Context) error
	// personEmail answers, for --owner me alone, the lowercased email of the person the sign-in
	// names: AWS reads an Identity Center sign-in's session name, GCP asks Google's userinfo
	// endpoint. It refuses a sign-in that names no person - a machine's role, a service account, a
	// token without an email scope - which reads and writes all the same: the store's IAM, not
	// this CLI, decides what any sign-in may do.
	personEmail(ctx context.Context) (string, error)
	// identity names the sign-in, as `<source>: <principal>`, for the line every form prints
	// before its first store call (openSession) and a refusal of the store's
	// (secretSession.refusedAs): the source the backend took the credential from (a
	// variable, a file, gcloud, the machine) and who it acts as - AWS's ARN, a Google account's
	// email - or that the principal is unknown.
	identity() string
	// chooseAnother is how a refusal of the store's tells the caller to sign in with another
	// credential, as one clause that starts "choose another": built from the source that signed
	// in, it names the variables that can choose another from there - never one that source
	// outranks without saying so - and on Google Cloud the gcloud configurations
	// CLOUDSDK_ACTIVE_CONFIG_NAME can name.
	chooseAnother() string
	// signInPrefix is the shell assignments a command line the form prints for running again
	// (pipeCommand) needs to sign in as this one did when the sign-in came from a variable that
	// may have been set for this command alone: GCP's GOOGLE_APPLICATION_CREDENTIALS when that
	// chose the source, and CLOUDSDK_ACTIVE_CONFIG_NAME and CLOUDSDK_CORE_ACCOUNT when they chose
	// the gcloud login; AWS's AWS_PROFILE or AWS_DEFAULT_PROFILE when that profile signed in, and
	// AWS_WEB_IDENTITY_TOKEN_FILE and AWS_ROLE_ARN for a web identity; else "", and never a key or
	// a token. It ends in a space when it is not empty.
	signInPrefix() string

	list(ctx context.Context) ([]secretView, error)
	show(ctx context.Context, slug string) (secretView, error)
	// prepareCreate builds the create's state without writing. begin makes the value-free store
	// call, where the backend has one, after the form has installed its signal cleanup.
	prepareCreate(slug string, tags map[string]string) newSecret
	set(ctx context.Context, slug, value string) error
	// currentTags answers the secret's tags as one read holds them (AWS tags, or Secret Manager
	// annotations with that read's etag), from which retag builds the tags the secret will hold once
	// its write lands, and against which it makes that write.
	currentTags(ctx context.Context, slug string) (heldTags, error)
	// tagReadPermission names the store permission currentTags needs, for the refusal a set
	// prints when the store refuses that read (secretSession.tagReadRefusal).
	tagReadPermission() string
	// readValue answers the secret's current value, as the broker's own store reads it: a
	// non-empty string, and on Secret Manager valid UTF-8. Only retag reads it, to decode it under
	// the type it is about to write (checkRetagValue), and nothing prints it.
	readValue(ctx context.Context, slug string) (string, error)
	// valueReadPermission names the store permissions readValue needs, for the line retag prints
	// when the store refuses that read (checkRetagValue).
	valueReadPermission() string
	// setTags writes tags onto the secret in one call and leaves every other tag as it is.
	// removeTags removes keys from it in one call and leaves every other tag as it is. Each writes
	// against held, and answers errTagsChanged when another write raced it: GCP sends held's
	// annotations, changed, with held's etag, which a raced write fails; AWS's tag calls take no
	// condition, so its write reads the tags back and restores held's (awsBackend.raceCheck).
	setTags(ctx context.Context, slug string, held heldTags, tags map[string]string) error
	removeTags(ctx context.Context, slug string, held heldTags, keys []string) error
	// delete schedules the secret's deletion and answers the moment it happens (AWS:
	// DeleteSecret's own recovery window end; GCP: the expire_time it sets), nil only on error.
	delete(ctx context.Context, slug string) (deletion *time.Time, err error)
	restore(ctx context.Context, slug string) error

	// isAccessDenied reports whether err is the store's own access-denied refusal and only that
	// (AWS's AccessDeniedException, Secret Manager's PERMISSION_DENIED), used by retag's
	// shared-secret message, by its value read (checkRetagValue) and by every refusal's identity
	// (secretSession.refusedAs).
	isAccessDenied(err error) bool

	// close releases the backend's own connection (GCP's gRPC client; AWS's HTTP-based SDK
	// clients need no release). A short CLI process would reclaim it on exit regardless, but a
	// test building many backends in one process leaks goroutines without it.
	close() error
}

// newSecret is a prepared secret create. begin makes any value-free store call before the value
// is read; on failure it answers what its recovery left, so the form can reread after a deletion.
// store writes the first value. discard, called when no value will be stored, checks creation
// identity and versions before deleting an empty GCP secret; AWS makes nothing until store and
// discards nothing. Its error names what stays and the command that clears or settles it, and is
// nil when nothing stays. Each store call of begin, store and discard runs under
// storeCallDeadline. unsettled names, for a form that ends without waiting on the call in flight,
// what the store may be left holding and the command that clears or settles it; a second signal
// calls it while begin, store or discard may be running, so it reads only what is safe then.
// madeBeforeValue reports whether begin makes a secret that waits in the store for its value (GCP),
// which the broker holds as pending for policy.PendingValueWindow, so the form's read of the value
// ends with that window; AWS makes nothing before the value, and its read has no bound.
type newSecret interface {
	begin(ctx context.Context) (discardOutcome, error)
	store(ctx context.Context, value string) error
	discard(ctx context.Context) (discardOutcome, error)
	unsettled() string
	madeBeforeValue() bool
}

// secretSession is one secret form's connection: the broker and the backend (requireAccount)
// opened for it; src, the store those reads and writes go to, which every answer and every
// refusal of the form names (source.go); and readsTypeTag, whether the broker's settings say it
// reads a secret's type from the type tag (Settings.SecretTypeTag, requireTypeTag).
type secretSession struct {
	ctx          context.Context
	broker       *client
	backend      secretBackend
	src          source
	readsTypeTag bool
	// settings are the broker's GET /v1/settings, whose judge_keys `secret show` reads.
	settings Settings
	// signerOnce finds, at the first reread, the broker identity every reread is signed with,
	// signer, or why this process holds none, noSigner (rereadSigner).
	signerOnce sync.Once
	signer     proofSigner
	noSigner   error
}

// rereadSigner is the signer this session's rereads are sent under: a broker identity this process
// already holds (heldSigner, not buildSigner, which the session forms use and which may wait on a
// launcher, trade or register), looked up the first time a reread needs it, so a form that rereads
// nothing looks for none, and one with no identity waits on nothing and trades nothing. It
// answers nil and why where the process holds none; askReread then sends the reread unsigned.
func (s *secretSession) rereadSigner() (proofSigner, error) {
	s.signerOnce.Do(func() { s.signer, s.noSigner = heldSigner() })
	return s.signer, s.noSigner
}

var errSignedRereadMissingProof = errors.New("the broker received no Proof header on the signed reread")

// askReread asks the broker to reread name on ctx, signed by the identity this process holds
// (rereadSigner), which the broker answers with what it now serves; a person's sign-in whose
// lease lapsed registers again once on the way (doProofBuilt). Where the process holds none, or
// the signed call failed for its proof alone (proofFailed), the call goes again with no proof,
// which the broker takes whatever it can check and rereads after answering. A signed reread that
// ran and failed, or that the broker refused for another reason, is the answer as it stands: the
// write is served only from the broker's next reload, and a second call would fail the same way
// and spend a second token of both reread buckets. unsigned is why the call that answered carried
// no proof, nil for a signed one the broker answered in full. A signed call the broker answered
// 202, as it answers a call with no proof, is the broker's answer as it stands, and unsigned then
// says so: a front between this command and the broker dropped the Proof header on the way.
func (s *secretSession) askReread(ctx context.Context, name string) (r Reread, unsigned, err error) {
	signer, unsigned := s.rereadSigner()
	if signer != nil {
		r, err = s.broker.RereadSecret(ctx, signer, name)
		switch {
		case err == nil && !r.Told:
			return r, errSignedRereadMissingProof, nil
		case !proofFailed(err):
			return r, nil, err
		}
		unsigned = fmt.Errorf("the signed reread failed: %w", err)
	}
	r, err = s.broker.RereadSecret(ctx, nil, name)
	return r, unsigned, err
}

// proofFailed reports whether a signed reread failed for its proof and not for the reread: the
// proof could not be signed (signError), the broker did not accept it (401, a lapsed enrollment
// the sign-in could not register again among them), or the broker could not check it (503
// DATABASE_UNAVAILABLE, which it answers before it reads anything). A reread that ran and failed
// (500, 503 REQUEST_ENDED), a name no secret can carry (400), a spent reread limit (429) and a
// broker that could not be reached are not: an unsigned call would meet each of them too.
func proofFailed(err error) bool {
	var unsigned *signError
	if errors.As(err, &unsigned) {
		return true
	}
	var refused *apiError
	if !errors.As(err, &refused) {
		return false
	}
	return refused.Status == http.StatusUnauthorized ||
		refused.Status == http.StatusServiceUnavailable && refused.Code == "DATABASE_UNAVAILABLE"
}

// whoDecides is secret show's decides and guidance lines: who decides a request for the key, and
// the guidance the model judge decides it on, so an agent can meet the guidance on its first
// request. It reads the key's own tags and the broker's judge_keys (GET /v1/settings), the rules'
// answer, which holds while the rules and the tags are unchanged:
//
//   - an administrator's key is a person's, on the broker page, whatever the rules say, since a
//     person approves every release of one;
//   - a key the rules give a judge entry is the model judge's, up to its seconds, against its
//     guidance, and its person's only if the judge cannot decide;
//   - any other key is a person's, on the broker page.
//
// An agent-tier key is granted at once to a session its tags allow, so for one of those the line
// says nobody decides, and names who decides any other session's request.
func whoDecides(view secretView, judged map[string]JudgeKey) (decides, guidance string) {
	guidance = "-"
	entry, hasJudge := judged[view.Name]
	switch {
	case view.Admin == "true":
		decides = "a person, on the broker page: an administrator's key, which the judge never decides"
	case hasJudge:
		decides = fmt.Sprintf("the model judge, up to %s, against its guidance; the key's person only if the judge cannot decide", time.Duration(entry.Seconds)*time.Second)
		if entry.Guidance != "" {
			guidance = entry.Guidance
		}
	default:
		decides = "a person, on the broker page"
	}
	if view.Tier == policy.TierAgent {
		decides = "nobody, for a session its tags allow; otherwise " + decides
	}
	return decides, guidance
}

// Close releases the session's backend connection.
func (s *secretSession) Close() error { return s.backend.close() }

// storeName is the store this session reads and writes, for a line that names where an answer
// came from: the secret forms call the store directly under the environment's cloud credential,
// so the store, not the broker, is the source of what they print.
func (s *secretSession) storeName() string { return s.src.storeName() }

// refusedAs is err, and where err is the store's own access-denied refusal, the sign-in the form
// called the store as (backend.identity) beside it, so a refusal of IAM's names whom IAM refused,
// and how to choose another (backend.chooseAnother).
func (s *secretSession) refusedAs(err error) error {
	if !s.backend.isAccessDenied(err) {
		return err
	}
	return fmt.Errorf("%w; the store refused %s; %s", err, s.backend.identity(), s.backend.chooseAnother())
}

// tagReadRefusal is err, the failure of a form's read of the secret name's tags, said as the
// form must say it: a store that refused the read names the permission that read needs
// (backend.tagReadPermission), since whoever may store a value may read that secret's tags, and
// a person whose grant predates that rule is refused here with nothing written. Any other
// failure is said as it is.
func (s *secretSession) tagReadRefusal(name string, err error) error {
	if !s.backend.isAccessDenied(err) {
		return fmt.Errorf("read %s's tags for its type: %w", name, err)
	}
	return fmt.Errorf("read %s's tags for its type, which needs %s: %w", name, s.backend.tagReadPermission(), err)
}

// requireTypeTag refuses a write after which the secret name holds the type secretType ("" for
// none) unless the broker's settings said it reads the type tag (Settings.SecretTypeTag). A
// broker from before the tag sends no such setting and reads no type tag. Every broker from
// legion-envoy-v16.0.0 to the release before the tag reads only the old kind tag and ignores the
// rollback guard (policy.TagRollbackGuard), so it serves a secret that carries type alone as a
// stored secret and every grant of it would release the key or login it stores; only the brokers
// from legion-envoy-v13.19.0 to the release before v16.0.0 read the guard, and refuse the secret.
// No broker before the tag names a type in its reread that this CLI can compare (rereadRetag,
// rereadValue). The first release that reads the type tag is therefore the broker's rollback
// floor. Create, set and retag ask requireTypeTag before they read a value or write anything. A
// write that leaves no type is safe on every broker, and is never refused. The refusal names the
// release this binary was built as (buildversion), whose broker reads the tag, since the CLI and
// the broker ship in one legion-envoy release.
func (s *secretSession) requireTypeTag(name, secretType string) error {
	if secretType == "" || s.readsTypeTag {
		return nil
	}
	return fmt.Errorf("%s would hold the type %s, and %s does not say it reads the type tag: its GET /v1/settings sends no secret_type_tag, as no broker from before that tag does. Such a broker serves a secret that carries type alone as a stored secret, releasing what it stores to every grant, and its reread names no type to check, so nothing is written. Move the broker to the release this agent-secrets is from (%s) or a later one, or run this with the agent-secrets of the broker's own release",
		name, secretType, s.src.brokerAt(), buildversion.String())
}

// ownerTag is the owner tag --owner names, as checkOwnerTier answered it: shared, a person's
// email, or, for me, the email of the person the sign-in names (backend.personEmail), which only
// --owner me asks for. A sign-in that names no person, or names one by an email the broker does
// not accept as an owner, is refused naming --owner <email>, which names the owner instead.
func (s *secretSession) ownerTag(owner string) (string, error) {
	if owner != ownerMe {
		return owner, nil
	}
	email, err := s.backend.personEmail(s.ctx)
	if err == nil && !policy.ValidPersonOwner(email) {
		err = fmt.Errorf("the sign-in's email %q is not an email the broker accepts as an owner", email)
	}
	if err != nil {
		return "", usageErr{fmt.Errorf("--owner me: %w: name the owner with --owner <email>", err)}
	}
	return email, nil
}

// secretFailIn is secretFail for a failure met once the session is open: it names the store the
// form looked in, so no refusal of this CLI's reads as another secrets CLI's answer about the
// name, and the sign-in the store refused (refusedAs). It is a function rather than a method on
// secretSession because broker-refgen reads every exit code this package returns, and it follows a
// plain call, never a method's.
func secretFailIn(stderr io.Writer, form string, s *secretSession, err error) int {
	return secretFail(stderr, form, fmt.Errorf("%w; looked in %s", s.refusedAs(err), s.storeName()))
}

// openSession reads the broker's settings from AGENT_SECRETS_URL, opens the backend its `store`
// field names (openSecretBackend) and, on AWS, refuses a sign-in in another account
// (backend.requireAccount), whose secrets are not the agent secrets; GCP needs no equivalent. It
// refuses no sign-in for the kind of identity it is, a read's or a write's: the store's IAM
// decides what it may do. Once the backend is open, and before any store call, it prints on
// stderr the one line that names the sign-in form's calls run under (backend.identity).
func openSession(ctx context.Context, stderr io.Writer, form string) (*secretSession, error) {
	base := brokerBaseURL()
	if base.Empty() {
		return nil, usageErr{fmt.Errorf("%s is required: it names the secrets broker whose store these forms read and write", brokerURLVar)}
	}
	if err := requireTrustedBrokerURL(base); err != nil {
		return nil, err
	}
	broker := newClient(base)
	settings, err := broker.Settings(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the settings of %s: %w", brokerSource(base), err)
	}
	backend, err := openSecretBackend(ctx, settings)
	if err != nil {
		return nil, err
	}
	s := &secretSession{ctx: ctx, broker: broker, backend: backend, src: brokerSource(base).withSettings(settings), readsTypeTag: settings.SecretTypeTag, settings: settings}
	if err := backend.requireAccount(ctx); err != nil {
		s.Close()
		return nil, err
	}
	fmt.Fprintf(stderr, "agent-secrets %s: using %s\n", form, backend.identity())
	return s, nil
}

// openSecretBackend builds the backend settings.Store names: AWS Secrets Manager, pinned to the
// key ARN's own account and region (checkSettingsKeyARN), for "" or secrets.StoreAWS - a
// broker that predates `store` names neither - or GCP Secret Manager, pinned to the project
// secrets_prefix and kms_key_name's own shape agree on (checkSettingsKMSKeyName), for
// secrets.StoreGCP. The store credential comes from the environment, not a flag. Any other store
// is refused before any cloud call.
func openSecretBackend(ctx context.Context, settings Settings) (secretBackend, error) {
	switch settings.Store {
	case "", secrets.StoreAWS:
		if err := checkSettingsKeyARN(settings); err != nil {
			return nil, err
		}
		sm, st, sources, err := awsClients(ctx, settings.AWSRegion)
		if err != nil {
			return nil, err
		}
		return &awsBackend{settings: settings, sm: awsStore{sm}, st: st, source: awsSourceOf(sources)}, nil
	case secrets.StoreGCP:
		if err := checkSettingsKMSKeyName(settings); err != nil {
			return nil, err
		}
		return newGCPBackend(ctx, settings)
	default:
		return nil, fmt.Errorf("the broker's settings name an unknown secret store %s", settings.words(strconv.Quote(settings.Store)))
	}
}

// displayName is the NAME a session asks for the secret named id, or id itself when its name
// under the prefix maps to none (the broker refuses such a secret as name-malformed). Both
// backends share it (awsBackend.displayName, gcpBackend.displayName): their store-side name
// differs, but it maps back to NAME the same way, from settings.SecretsPrefix alone.
func displayName(prefix, id string) string {
	slug := strings.TrimPrefix(id, prefix)
	name := policy.SlugToName(slug)
	if back, err := policy.NameToSlug(name); err != nil || back != slug {
		return id
	}
	return name
}

// rereadAfter asks the broker to reread name after a form's write that leaves no type to check,
// and prints its answer: exit 0 when the broker now does what the write meant (serves the secret,
// or, after a delete, does not), or took a reread it says no more of (reread); 1 when its answer
// contradicts the write or it could not be asked. restorable is when a deleted secret stops being
// restorable, which only delete names.
func rereadAfter(s *secretSession, stdout, stderr io.Writer, form, name string, wantServed bool, restorable *time.Time) int {
	if _, ok := reread(s, stdout, stderr, form, name, "", wantServed, restorable); !ok {
		return 1
	}
	return 0
}

// rereadValue is rereadAfter for a write of name's value, create's or set's, which also exits 1
// when the secret does not hold secretType, the type it holds once the write lands ("" for a
// stored secret), as rereadRetag does for a retag: a broker that serves a type secret as stored
// releases the key or login it stores to every grant of it, and one that serves a stored secret
// as a type mints from a value nobody checked as that type's. The store's own type tag is checked
// whatever the broker answers (reread); the broker's view is checked where it answered in full.
// The value stands, and the line names the retag that writes the secret's type as this form read
// it and asks the broker to reread it.
func rereadValue(s *secretSession, stdout, stderr io.Writer, form, name, secretType string) int {
	r, ok := reread(s, stdout, stderr, form, name, secretType, true, nil)
	if !ok {
		return 1
	}
	if !r.Told || r.Type == secretType {
		return 0
	}
	retag, releases := secretType, ""
	if retag == "" {
		retag = tagNone
	}
	if r.Type == "" {
		releases = ", so every grant of it releases what it stores as it is"
	}
	fmt.Fprintf(stderr, "agent-secrets %s: the value of %s in %s stands, but %s serves %s as %s, not %s%s. Run `%s` to write the type again and ask it to reread %s, until that exits 0\n",
		form, name, s.storeName(), s.src.brokerAt(), name, servedTypeName(s, r.Type), typeName(secretType), releases,
		pipeCommand(s.backend.signInPrefix(), "secret", "retag", name, "--type", retag), name)
	return 1
}

// reread is every form's reread after its write: it asks the broker (askReread), prints the
// broker's answer, checks it against the write (wantServed: served, or after a delete not), and,
// for a write that leaves name holding secretType, reads the type tag back from the store under
// the sign-in that wrote it (typeTagHolds), whatever the broker answered, a reread it refused or
// could not be asked for among them. ok is false where the form exits 1, which it printed: the
// broker could not be asked, its answer contradicts the write, or the store holds another type. A
// reread the broker took and said no more of, all a call with no proof is answered (r.Told false),
// contradicts nothing, so the form exits on the type tag alone; it prints the one line that says
// the broker's view was not checked, why, and the command that checks it (sayUnchecked).
// Otherwise r is the broker's own answer to a session's reread, whose type the caller holds to
// secretType.
func reread(s *secretSession, stdout, stderr io.Writer, form, name, secretType string, wantServed bool, restorable *time.Time) (Reread, bool) {
	r, unsigned, err := s.askReread(s.ctx, name)
	ok := true
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "agent-secrets %s: the write to %s stands, but %s could not be asked to reread %s, so it serves the change only from its next reload: %v\n", form, s.storeName(), s.src.brokerAt(), name, err)
		ok = false
	case !r.Told:
		fmt.Fprintf(stdout, "broker: rereading %s from %s\n", name, s.storeName())
	default:
		switch {
		case r.Served && r.Type != "":
			fmt.Fprintf(stdout, "broker: serving %s from %s (type %s)\n", name, s.storeName(), s.settings.words(r.Type))
		case r.Served:
			fmt.Fprintf(stdout, "broker: serving %s from %s\n", name, s.storeName())
		case !wantServed && r.Reason == policy.ReasonAbsent:
			fmt.Fprintf(stdout, "broker: %s is deleted in %s (restorable until %s)\n", name, s.storeName(), formatTime(restorable))
		default:
			fmt.Fprintf(stdout, "broker: refusing %s from %s (reason=%s)\n", name, s.storeName(), s.settings.words(r.Reason))
		}
		switch {
		case wantServed && !r.Served:
			fmt.Fprintf(stderr, "agent-secrets %s: %s refuses %s after the write to %s (reason=%s)\n", form, s.src.brokerAt(), name, s.storeName(), s.settings.words(r.Reason))
			ok = false
		case !wantServed && r.Served:
			fmt.Fprintf(stderr, "agent-secrets %s: %s still serves %s after its delete in %s\n", form, s.src.brokerAt(), name, s.storeName())
			ok = false
		}
	}
	if secretType != "" && !typeTagHolds(s, stderr, form, name, secretType) {
		return r, false
	}
	if ok && !r.Told {
		sayUnchecked(s, stderr, form, name, secretType, unsigned)
	}
	return r, ok
}

// typeTagHolds reads name's type tag back from the store, under the sign-in the form wrote with,
// and reports whether it is secretType, the type the form just wrote. When it is not, or the read
// fails, it prints why, naming the secret, the type the form asked for and the tag it found
// ("none" for no tag): a type secret whose tag the store did not keep is served as stored by any
// broker that reads it, releasing what it stores as it is.
func typeTagHolds(s *secretSession, stderr io.Writer, form, name, secretType string) bool {
	slug, err := policy.NameToSlug(name)
	var held heldTags
	if err == nil {
		held, err = s.backend.currentTags(s.ctx, slug)
	}
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets %s: the write to %s stands, but reading %s's type tag back failed, so whether the store kept the type %s is unknown: %v\n",
			form, s.storeName(), name, secretType, s.refusedAs(err))
		return false
	}
	// The type the store holds is the type tag, else the old kind tag a secret from before it may
	// still carry, as the broker reads it (policy.TypeFromTags).
	found := policy.TypeFromTags(held.tags)
	if found == secretType {
		return true
	}
	if found == "" {
		found = tagNone
	}
	fmt.Fprintf(stderr, "agent-secrets %s: the write to %s stands, but %s's type tag there is %s, not %s. Run `%s` to write the type again, until that exits 0\n",
		form, s.storeName(), name, found, secretType, pipeCommand(s.backend.signInPrefix(), "secret", "retag", name, "--type", secretType))
	return false
}

// sayUnchecked is the one line a form prints for a reread the broker took and said no more of: a
// call that carried no proof, and why (unsigned). It says what the form's exit 0 then proves, and
// names the command to run where this command has a broker identity to check the broker's own
// view: for a write that leaves a type, the retag that writes it again and rereads it, before
// which no key or login is to be stored in the secret, since a broker that still read the secret
// as stored would release it as it is; for any other write, a signed reread.
func sayUnchecked(s *secretSession, stderr io.Writer, form, name, secretType string, unsigned error) {
	if secretType != "" {
		fmt.Fprintf(stderr, "agent-secrets %s: %s carries the type tag %s in %s, but whether %s serves it as %s was not checked (%v), so this exit 0 says only that the store holds the tag. Store no key or login in %s until `%s`, run where this command has a broker identity (`%s`), prints `broker: serving %s from … (type %s)`\n",
			form, name, secretType, s.storeName(), s.src.brokerAt(), secretType, unsigned, name,
			pipeCommand(s.backend.signInPrefix(), "secret", "retag", name, "--type", secretType), cipherclient.SignInCommand, name, secretType)
		return
	}
	route := s.broker.baseURL.Call(s.broker.route("/v1/secrets/" + name + "/reread"))
	fmt.Fprintf(stderr, "agent-secrets %s: %s took the reread of %s, but how it serves %s was not checked (%v), so this exit 0 says only that the write stands. To check it, run `curl -s -X POST -H \"Proof: $(agent-secrets sign --method POST --url %s)\" %s` where this command has a broker identity (`%s`)\n",
		form, s.src.brokerAt(), name, name, unsigned, route, route, cipherclient.SignInCommand)
}

// secretView is what list and show print of one secret: never its value.
type secretView struct {
	// Name is the NAME a session asks for it as (or its name in the secret store, when it maps to none).
	Name string `json:"name"`
	// Store is the secret store this row was read from, as the broker's settings describe it
	// (source.storeName): a machine can hold another secrets CLI answering the same names from
	// another store, and a reader of these rows must never take one for the other.
	Store string `json:"store"`
	// SecretName is its whole name in the secret store.
	SecretName string `json:"secret_name"`
	Owner      string `json:"owner"`
	Tier       string `json:"tier"`
	Type       string `json:"type"`
	// GitHubOwners is a github-app secret's github-owners tag, the accounts the broker mints for.
	GitHubOwners string `json:"github_owners"`
	// Requesters is the requesters tag: the people and services that may ask for the secret at
	// all, or "" where any session its owner and tier allow may.
	Requesters string `json:"requesters"`
	// Scopes is a google-service-account secret's scopes tag: the OAuth scope names the token it
	// mints carries, each the part of a Google scope after https://www.googleapis.com/auth/, or
	// "" for a secret that names none, whose token carries the cloud-platform scope alone
	// (policy.TagScopes).
	Scopes string `json:"scopes"`
	// Admin is the admin tag as the store holds it, "true" for a secret whose grant is an
	// administrator's and "" for one that is no administrator's (policy.TagAdmin). It is the raw
	// value rather than a bool so that show prints a tag written by hand as anything else, which
	// the broker refuses the secret for (policy.ReasonAdminMalformed), instead of hiding it as
	// not-admin.
	Admin        string `json:"admin"`
	MaxGrant     string `json:"max_grant"`
	DefaultGrant string `json:"default_grant"`
	// HasValue is whether it holds a current, servable version (AWS: a version carries AWSCURRENT;
	// GCP: versions/latest is ENABLED), so the broker can release a value.
	HasValue bool `json:"has_value"`
	// Created and LastChanged are absent when the secret store answers none.
	Created     *time.Time `json:"created,omitempty"`
	LastChanged *time.Time `json:"last_changed,omitempty"`
	// Deleted is set while the secret is scheduled for deletion: AWS, the moment the delete ran;
	// GCP, the expire_time it set, the future moment GCP deletes it for good (GCP has no separate
	// scheduling moment to report, so this is not "when its deletion was scheduled" there).
	Deleted *time.Time `json:"deleted,omitempty"`
	// EarliestPurge is the earliest Secrets Manager can purge it, set while it is scheduled for
	// deletion (earliestPurge): restorable at least until then, perhaps longer.
	EarliestPurge *time.Time `json:"earliest_purge,omitempty"`
	// Versions maps each version id to its AWS staging labels, or each GCP version number to its
	// one-element state (ENABLED, DISABLED, DESTROYED). Only show answers it.
	Versions map[string][]string `json:"versions,omitempty"`
}

// withTags is v with the fields a secret's tags (AWS tags, or Secret Manager annotations) give.
func (v secretView) withTags(tags map[string]string) secretView {
	v.Owner, v.Tier = tags[policy.TagOwner], tags[policy.TagTier]
	v.Type, v.GitHubOwners, v.MaxGrant, v.DefaultGrant = policy.TypeFromTags(tags), tags[policy.TagGitHubOwners], tags[policy.TagMaxGrant], tags[policy.TagDefaultGrant]
	v.Requesters, v.Scopes, v.Admin = tags[policy.TagRequesters], tags[policy.TagScopes], tags[policy.TagAdmin]
	return v
}

// writeIndentedJSON writes v as indented JSON.
func writeIndentedJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func cmdSecretList(args []string, stdout, stderr io.Writer) int {
	const form = "secret list"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags := secretFlags(form, stderr)
	asJSON := flags.Bool("json", false, "print the secrets as JSON")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) > 0 {
		return secretFail(stderr, form, usageErr{errors.New(unexpectedArgument)})
	}
	s, err := openSession(context.Background(), stderr, form)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	defer s.Close()
	views, err := s.backend.list(s.ctx)
	if err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	// sort.Slice's less answers a bool: an int-returning comparator would be read by broker-refgen
	// as an exit code, which every int-returning function in this package is.
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	for i := range views {
		views[i].Store = s.storeName()
	}
	if *asJSON {
		if err := writeIndentedJSON(stdout, views); err != nil {
			return secretFailIn(stderr, form, s, err)
		}
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tOWNER\tTIER\tTYPE\tMAX_GRANT\tDEFAULT_GRANT\tVALUE\tDELETED\tEARLIEST_PURGE")
	for _, v := range views {
		value := "no"
		if v.HasValue {
			value = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", v.Name, orDash(v.Owner), orDash(v.Tier), orDash(v.Type), orDash(v.MaxGrant), orDash(v.DefaultGrant), value, formatTime(v.Deleted), formatTime(v.EarliestPurge))
	}
	if err := tw.Flush(); err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	// The source line and the legend go to stderr, so the table on stdout stays one row per
	// secret for a script. The source names the one store every row was read from, since a
	// machine can hold another secrets CLI listing other secrets under the same names.
	fmt.Fprintf(stderr, "Every row is a secret in %s.\n", s.storeName())
	// The legend names Secrets Manager's own recovery window, so it is printed only for a row
	// that has one (EarliestPurge, AWS-only: GCP's expire_time is the exact purge moment, with
	// no separate recovery window to explain).
	if slices.ContainsFunc(views, func(v secretView) bool { return v.EarliestPurge != nil }) {
		fmt.Fprintf(stderr, "EARLIEST_PURGE is the deletion plus Secrets Manager's %d-day minimum recovery window: restore works at least until then; the window the delete chose may be longer.\n", minRecoveryWindowDays)
	}
	return 0
}

// ---------------------------------------------------------------------------
// show
// ---------------------------------------------------------------------------

func cmdSecretShow(args []string, stdout, stderr io.Writer) int {
	const form = "secret show"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags := secretFlags(form, stderr)
	asJSON := flags.Bool("json", false, "print the secret as JSON")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	_, slug, err := secretName(positional)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	s, err := openSession(context.Background(), stderr, form)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	defer s.Close()
	view, err := s.backend.show(s.ctx, slug)
	if err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	view.Store = s.storeName()

	if *asJSON {
		if err := writeIndentedJSON(stdout, view); err != nil {
			return secretFailIn(stderr, form, s, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "name: %s\nstore: %s\nsecret_name: %s\nowner: %s\ntier: %s\ntype: %s\ngithub_owners: %s\nrequesters: %s\nscopes: %s\nadmin: %s\nmax_grant: %s\ndefault_grant: %s\ncreated: %s\nlast_changed: %s\n",
		view.Name, view.Store, view.SecretName, orDash(view.Owner), orDash(view.Tier), orDash(view.Type), orDash(view.GitHubOwners), orDash(view.Requesters), orDash(view.Scopes), orDash(view.Admin), orDash(view.MaxGrant), orDash(view.DefaultGrant), formatTime(view.Created), formatTime(view.LastChanged))
	decides, guidance := whoDecides(view, s.settings.JudgeKeys)
	fmt.Fprintf(stdout, "decides: %s\nguidance: %s\n", decides, s.settings.words(guidance))
	switch {
	case view.EarliestPurge != nil:
		fmt.Fprintf(stdout, "deleted: %s\nearliest_purge: %s (the deletion plus Secrets Manager's %d-day minimum recovery window: restorable at least until then; the window the delete chose may be longer)\n",
			formatTime(view.Deleted), formatTime(view.EarliestPurge), minRecoveryWindowDays)
	case view.Deleted != nil:
		fmt.Fprintf(stdout, "deleted: %s\n", formatTime(view.Deleted))
	}
	versions := make([]string, 0, len(view.Versions))
	for version := range view.Versions {
		versions = append(versions, version)
	}
	slices.Sort(versions)
	if len(versions) == 0 {
		fmt.Fprintln(stdout, "versions: none")
	}
	for _, version := range versions {
		fmt.Fprintf(stdout, "version: %s %s\n", version, strings.Join(view.Versions[version], ","))
	}
	return 0
}

// ---------------------------------------------------------------------------
// create
// ---------------------------------------------------------------------------

func cmdSecretCreate(args []string, stdout, stderr io.Writer) int {
	const form = "secret create"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags := secretFlags(form, stderr)
	owner := flags.String("owner", "", "me (the email of the person your AWS or Google sign-in names), shared, or a person's email")
	tier := flags.String("tier", "", "agent (its owner's agents use it without asking) or human (a person approves each use)")
	secretType := flags.String("type", "", strings.Join(policy.Types, ", ")+" or none (the default)")
	githubOwners := flags.String("github-owners", "", `the GitHub accounts a github-app secret mints for, separated by spaces ("<login> <login>")`)
	requesters := flags.String("requesters", "", `the people (each by their lowercase sign-in email) and registered services that may ask for this secret at all, separated by spaces ("<email> <service>"); an admin secret and a google-service-account one each need a --max-grant of an hour or less, and a secret that only lists who may ask keeps its own cap, or none`)
	scopes := flags.String("scopes", "", `the OAuth scopes the token a google-service-account secret mints carries, each the part of a Google scope after https://www.googleapis.com/auth/, separated by spaces ("cloud-platform bigquery"); read on that type alone, and a secret without it mints cloud-platform`)
	admin := flags.String("admin", "", "true marks a secret whose grant is an administrator's: such a secret is human tier, so a person approves each release, names its requesters, and bounds its grants to an hour with --max-grant")
	maxGrant := flags.String("max-grant", "", "the longest grant duration this secret allows")
	defaultGrant := flags.String("default-grant", "", "session: a request with no --ttl lasts until its session ends, not 12 hours")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	name, slug, err := secretName(positional)
	if err == nil && (*owner == "" || *tier == "") {
		err = usageErr{errors.New("--owner and --tier are required")}
	}
	if err == nil {
		*owner, err = checkOwnerTier(*owner, *tier)
	}
	if err == nil {
		err = checkType(*secretType)
	}
	if err == nil {
		*githubOwners, err = checkGitHubOwners(*githubOwners, false)
	}
	if err == nil {
		err = checkGitHubOwnersFor(*secretType, *githubOwners, *githubOwners != "")
	}
	if err == nil {
		*requesters, err = checkRequesters(*requesters, false)
	}
	if err == nil {
		*scopes, err = checkScopes(*scopes, false)
	}
	if err == nil {
		err = checkAdmin(*admin, false)
	}
	if err == nil {
		err = checkMaxGrant(*maxGrant, false)
	}
	if err == nil {
		err = checkDefaultGrant(*defaultGrant, false)
	}
	if err != nil {
		return secretFail(stderr, form, err)
	}
	tags := optionalTags(tagFlags{
		Type:         *secretType,
		GitHubOwners: *githubOwners,
		Requesters:   *requesters,
		Scopes:       *scopes,
		Admin:        *admin,
		MaxGrant:     *maxGrant,
		DefaultGrant: *defaultGrant,
	})
	s, err := openSession(context.Background(), stderr, form)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	defer s.Close()
	if err := s.requireTypeTag(name, policy.TypeFromTags(tags)); err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	ownerValue, err := s.ownerTag(*owner)
	if err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	tags[policy.TagOwner], tags[policy.TagTier] = ownerValue, *tier
	// The cross-tag rules weigh the owner tag, which me resolves to only once the sign-in is
	// read, so they are checked here rather than beside the flags — still before the value is
	// read and before anything is written. The owner and tier go into the map above, so the
	// tags the check weighs are the tags this create sends and the broker then reads.
	if err := checkTagsFor(tags); err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	command := []string{"secret", "create", name, "--owner", *owner, "--tier", *tier}
	if *secretType != "" {
		command = append(command, "--type", *secretType)
	}
	if *githubOwners != "" {
		command = append(command, "--github-owners", *githubOwners)
	}
	if *requesters != "" {
		command = append(command, "--requesters", *requesters)
	}
	if *scopes != "" {
		command = append(command, "--scopes", *scopes)
	}
	if *admin != "" {
		command = append(command, "--admin", *admin)
	}
	if *maxGrant != "" {
		command = append(command, "--max-grant", *maxGrant)
	}
	if *defaultGrant != "" {
		command = append(command, "--default-grant", *defaultGrant)
	}
	// Install cleanup before the first store call: a signal can arrive after the store has
	// created the empty secret but before its answer reaches the form.
	pending := &pendingCreate{s: s, form: form, name: name, secretType: policy.TypeFromTags(tags), owner: ownerValue, tier: *tier, stdout: stdout, stderr: stderr, created: s.backend.prepareCreate(slug, tags)}
	return runCreate(pending, func(deadline time.Time) (string, error) {
		value, err := readSecretValue(name, pipeCommand(s.backend.signInPrefix(), command...), deadline, stderr)
		if err == nil && policy.ValidType(*secretType) {
			err = checkValueFor(*secretType, value)
		}
		return value, err
	})
}

// ---------------------------------------------------------------------------
// set
// ---------------------------------------------------------------------------

func cmdSecretSet(args []string, stdout, stderr io.Writer) int {
	const form = "secret set"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags := secretFlags(form, stderr)
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	name, slug, err := secretName(positional)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	s, err := openSession(context.Background(), stderr, form)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	defer s.Close()
	// The value is checked under the type the secret carries, as the broker's minter for that
	// type will read it; a type tag naming no type the broker mints is refused before the value
	// is read, since the broker serves no value under it, and so is a type secret's value where
	// the broker does not say it reads the type tag (requireTypeTag). Whoever stores a value may
	// therefore read that secret's tags, and a store that refuses the read says which permission
	// it needs (tagReadRefusal).
	held, err := s.backend.currentTags(s.ctx, slug)
	if err != nil {
		return secretFailIn(stderr, form, s, s.tagReadRefusal(name, err))
	}
	secretType := policy.TypeFromTags(held.tags)
	if secretType != "" && !policy.ValidType(secretType) {
		return secretFailIn(stderr, form, s, usageErr{fmt.Errorf("%s's type tag %q names no type the broker mints, and it refuses the secret as %s: retag it --type %s or %s before setting its value", name, secretType, policy.ReasonTypeUnknown, strings.Join(policy.Types, ", "), tagNone)})
	}
	if err := s.requireTypeTag(name, secretType); err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	value, err := readSecretValue(name, pipeCommand(s.backend.signInPrefix(), "secret", "set", name), time.Time{}, stderr)
	if err == nil && secretType != "" {
		err = checkValueFor(secretType, value)
	}
	if err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	if err := s.backend.set(s.ctx, slug, value); err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	fmt.Fprintf(stdout, "set a new value of %s in %s\n", name, s.storeName())
	noteEdgeSpaces(stderr, form, name, value)
	return rereadValue(s, stdout, stderr, form, name, secretType)
}

// ---------------------------------------------------------------------------
// delete, restore
// ---------------------------------------------------------------------------

// cmdSecretDelete schedules a secret's deletion with the longest recovery window (AWS) or at
// recoveryWindowDays from now (GCP's expire_time), never forcing it, so restore can bring the
// secret back until the window ends.
func cmdSecretDelete(args []string, stdout, stderr io.Writer) int {
	const form = "secret delete"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags := secretFlags(form, stderr)
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	name, slug, err := secretName(positional)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	s, err := openSession(context.Background(), stderr, form)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	defer s.Close()
	deletion, err := s.backend.delete(s.ctx, slug)
	if err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	fmt.Fprintf(stdout, "deleted %s in %s\n", name, s.storeName())
	return rereadAfter(s, stdout, stderr, form, name, false, deletion)
}

func cmdSecretRestore(args []string, stdout, stderr io.Writer) int {
	const form = "secret restore"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags := secretFlags(form, stderr)
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	name, slug, err := secretName(positional)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	s, err := openSession(context.Background(), stderr, form)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	defer s.Close()
	if err := s.backend.restore(s.ctx, slug); err != nil {
		return secretFailIn(stderr, form, s, err)
	}
	fmt.Fprintf(stdout, "restored %s in %s\n", name, s.storeName())
	return rereadAfter(s, stdout, stderr, form, name, true, nil)
}
