// packages/envoy/internal/broker/requests/redact.go
// What a release records of the command a session says it starts (Values, ReleaseCall): the
// first record.MaxCommandArguments arguments, each with every secret this file can name in it
// replaced by redactedArgument, then cut to record.MaxCommandArgumentBytes bytes. It names two
// kinds of secret. The first is the values this release handed out (handedOut): each value
// whole, each string of a stored JSON value, each token of a minted document and each airlock
// placeholder. The second is a credential of a well-known shape (knownCredential), whatever its
// source, which covers a token an earlier release minted. A secret of neither kind is logged as
// the session typed it; ReleasedMessage says so to its reader.
package requests

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/broker/record"
)

// redactedArgument is what a release records in place of each run of an argument that holds a
// secret.
const redactedArgument = "[redacted]"

// minRedactedBytes is the shortest handed-out value, and the shortest string of a stored JSON
// value, that a release redacts. A shorter one is left as written: marking every place a few
// bytes occur would garble the command and show the value by where the marks land, and it is no
// credential a log would leak.
const minRedactedBytes = 8

// knownCredential matches each credential shape a release redacts wherever it came from, as the
// common secret scanners name them: GitHub's personal, OAuth, user-to-server, installation and
// refresh tokens and its fine-grained personal tokens; AWS access key ids, long-term (AKIA) and
// temporary (ASIA); Google OAuth access tokens; Slack tokens; and a PEM private key block, to
// its END line or, where a cut ended the argument first, to the argument's end.
var knownCredential = regexp.MustCompile(strings.Join([]string{
	`gh[pousr]_[A-Za-z0-9]{36,}`,
	`github_pat_[A-Za-z0-9_]{22,}`,
	`(?:AKIA|ASIA)[A-Z0-9]{16}`,
	`ya29\.[A-Za-z0-9_.\-]+`,
	`xox[abprs]-[A-Za-z0-9-]+`,
	`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----(?s:.*?)(?:-----END (?:[A-Z0-9]+ )*PRIVATE KEY-----|\z)`,
}, "|"))

// knownCredentialAtCut matches the start of one of knownCredential's token shapes running to an
// argument's end, which is what a client's cut leaves of a token that crossed it: too short for
// knownCredential's own lengths, and still a credential's first bytes.
var knownCredentialAtCut = regexp.MustCompile(`(?:gh[pousr]_|github_pat_|AKIA|ASIA|ya29\.|xox[abprs]-)[A-Za-z0-9_.\-]*\z`)

// reachesTheCut reports whether arg is as long as the agent-secrets client sends an argument it
// cut (record.ClipArgument), so that its end may be where a secret was cut through.
func reachesTheCut(arg string) bool {
	return len(arg) > record.MaxCommandArgumentBytes-utf8.UTFMax
}

// loggedCommand is command as a release records it: its first record.MaxCommandArguments
// arguments, in each every secret redactArgument names replaced by redactedArgument and a NUL
// byte by U+FFFD, then cut by record.ClipArgument. It redacts before it cuts, so a secret that
// crosses the cut leaves no start of itself behind. A NUL byte is in no argument a process runs
// with, and the JSON a grant.used row holds refuses one, so a session that sends one has it
// replaced rather than failing its own release over a line of the log. It answers an empty
// vector, never nil, for a session that named no command, as a client from before the field
// does.
func loggedCommand(command, handedOut []string) []string {
	values := slices.DeleteFunc(slices.Clone(handedOut), func(v string) bool { return len(v) < minRedactedBytes })
	logged := make([]string, 0, min(len(command), record.MaxCommandArguments))
	for _, arg := range command[:min(len(command), record.MaxCommandArguments)] {
		arg = strings.ReplaceAll(redactArgument(arg, values), "\x00", "\uFFFD")
		logged = append(logged, record.ClipArgument(arg))
	}
	return logged
}

// redactArgument is arg with each run that holds a secret replaced by one redactedArgument: every
// place a value of values occurs, overlapping ones included; every knownCredential match; and,
// in an argument that reachesTheCut, an end that is the start of a value or of a token shape,
// which is what the client's cut leaves of a secret it ran through. It marks every such byte
// before it writes anything, so two secrets that overlap are one run with no tail of either left
// over, and a marker the replacement writes is never matched again.
func redactArgument(arg string, values []string) string {
	var covered []bool
	mark := func(from, to int) {
		if covered == nil {
			covered = make([]bool, len(arg))
		}
		for i := from; i < to; i++ {
			covered[i] = true
		}
	}
	for _, value := range values {
		for at := 0; at < len(arg); {
			found := strings.Index(arg[at:], value)
			if found < 0 {
				break
			}
			mark(at+found, at+found+len(value))
			at += found + 1
		}
	}
	for _, match := range knownCredential.FindAllStringIndex(arg, -1) {
		mark(match[0], match[1])
	}
	if reachesTheCut(arg) {
		for _, value := range values {
			for k := min(len(value)-1, len(arg)); k > 0; k-- {
				if strings.HasSuffix(arg, value[:k]) {
					mark(len(arg)-k, len(arg))
					break
				}
			}
		}
		if match := knownCredentialAtCut.FindStringIndex(arg); match != nil {
			mark(match[0], match[1])
		}
	}
	if covered == nil {
		return arg
	}
	var out strings.Builder
	for i := 0; i < len(arg); {
		if !covered[i] {
			out.WriteByte(arg[i])
			i++
			continue
		}
		out.WriteString(redactedArgument)
		for i < len(arg) && covered[i] {
			i++
		}
	}
	return out.String()
}

// handedOut is every value a session's release handed out or read, for loggedCommand to redact:
// each released name's secret parts (secretParts), and, for the names the airlock holds back,
// what the session holds in their place — each name's filled value and each slot's placeholder,
// the proxy password among them — since a placeholder is what opens the proxy.
func handedOut(read []releasedName, held *AirlockRelease) []string {
	var values []string
	for _, released := range read {
		values = append(values, released.secretParts()...)
	}
	if held != nil {
		for _, name := range held.Names {
			values = append(values, name.Value)
			for _, placeholder := range name.Slots {
				values = append(values, placeholder)
			}
		}
	}
	return values
}

// secretParts are the strings of this name's value that a command holding any of them holds a
// secret by: the whole value; what each slot of it holds as the airlock takes the value apart
// (airlockSplitOf), which for a minted document is each token inside it rather than the fields
// around it; and, for a stored value, every string inside it when it is a JSON document
// (jsonStrings), since a command takes a stored login or key pair apart and holds its fields. A
// name the airlock holds back has its split already; any other is taken apart as its kind says.
// A value its kind cannot take apart is its whole self and its JSON strings alone.
func (r releasedName) secretParts() []string {
	parts := []string{r.value}
	split := r.split
	if !r.behindAirlock() {
		split, _ = airlockSplitOf("", r.kind, r.value)
	}
	for _, value := range split.values {
		parts = append(parts, value)
	}
	if r.kind == "" {
		parts = append(parts, jsonStrings(r.value)...)
	}
	return parts
}

// jsonStrings is every string at any depth of value read as JSON, keys left out: nil for a value
// that is not JSON.
func jsonStrings(value string) []string {
	var doc any
	if json.Unmarshal([]byte(value), &doc) != nil {
		return nil
	}
	var strs []string
	var walk func(any)
	walk = func(node any) {
		switch node := node.(type) {
		case string:
			strs = append(strs, node)
		case []any:
			for _, item := range node {
				walk(item)
			}
		case map[string]any:
			for _, item := range node {
				walk(item)
			}
		}
	}
	walk(doc)
	return strs
}
