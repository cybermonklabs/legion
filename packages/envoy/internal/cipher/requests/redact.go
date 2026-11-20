// packages/envoy/internal/cipher/requests/redact.go
// What a release records of the command a session says it starts (Values, ReleaseCall): the
// first record.MaxCommandArguments arguments, each cut to record.MaxCommandArgumentBytes bytes
// and then with every secret this file can name in it replaced by redactedArgument. It names two
// kinds of secret. The first is the values this release handed out (handedOut): each value
// whole, each string of a stored JSON value, each token of a minted document and each airlock
// placeholder. The second is a credential of a well-known shape (credentialTokens and
// pemPrivateKey), whatever its source, which covers a token an earlier release minted. A secret
// of neither kind is logged as the session typed it; ReleasedMessage says so to its reader.
package requests

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/record"
)

// redactedArgument is what a release records in place of each run of an argument that holds a
// secret.
const redactedArgument = "[redacted]"

// minRedactedBytes is the shortest handed-out value, and the shortest string of a stored JSON
// value, that a release redacts wherever it occurs. A shorter one is left as written: marking
// every place a few bytes occur would replace every short common word, a user name or `true`,
// and show the value by where the marks land, and a credential that short is not one
// (dispatch://INFRA-3801/artifact/decisions-md, "What a release log redacts"). The rule at the cut
// has no minimum: an argument the cut ended loses any end, down to one byte, that starts a value
// of this length or more.
const minRedactedBytes = 8

// credentialToken is one token shape a release redacts wherever it came from: the prefix that
// names it, the characters of its body, and the fewest of them a whole token holds. Both
// patterns are built from this one table, so a shape added here is matched whole and at the cut
// alike.
type credentialToken struct {
	prefix, body string
	least        int
}

// credentialTokens are the token shapes the common secret scanners name.
var credentialTokens = []credentialToken{
	{`gh[pousr]_`, `[A-Za-z0-9]`, 36},   // GitHub personal, OAuth, user-to-server, installation and refresh tokens
	{`github_pat_`, `[A-Za-z0-9_]`, 22}, // GitHub fine-grained personal tokens
	{`(?:AKIA|ASIA)`, `[A-Z0-9]`, 16},   // AWS access key ids, long-term and temporary
	{`ya29\.`, `[A-Za-z0-9_.\-]`, 1},    // Google OAuth access tokens
	{`xoxd-`, `[A-Za-z0-9%/+=-]`, 1},    // Slack d cookies, whose base64 body a -b d=… argument often carries URL-encoded
	{`xox[a-z]-`, `[A-Za-z0-9-]`, 1},    // Slack tokens, every xox prefix: xoxa, xoxb, xoxc, xoxd, xoxe, xoxp, xoxr, xoxs
}

// pemPrivateKey matches a PEM private key block to its END line or, where a cut ended the
// argument first, to the argument's end, so it needs no form of its own at the cut.
const pemPrivateKey = `-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----(?s:.*?)(?:-----END (?:[A-Z0-9]+ )*PRIVATE KEY-----|\z)`

// knownCredential matches each whole credential of a known shape. knownCredentialAtCut matches
// the start of a token shape running to an argument's end, which is what a cut leaves of a token
// that crossed it: too short for the token's own length, and still its first bytes.
var knownCredential, knownCredentialAtCut = credentialPatterns()

func credentialPatterns() (whole, atCut *regexp.Regexp) {
	wholes := []string{pemPrivateKey}
	starts := make([]string, 0, len(credentialTokens))
	for _, token := range credentialTokens {
		wholes = append(wholes, token.prefix+token.body+"{"+strconv.Itoa(token.least)+",}")
		starts = append(starts, token.prefix+token.body+"*")
	}
	return regexp.MustCompile(strings.Join(wholes, "|")), regexp.MustCompile(`(?:` + strings.Join(starts, "|") + `)\z`)
}

// loggedCommand is command as a release records it: its first record.MaxCommandArguments
// arguments, each cut by record.ClipArgument, then with every secret redactArgument names
// replaced by redactedArgument and a NUL byte by U+FFFD, then cut again, since a marker can be
// longer than what it replaced. It cuts first so the redaction reads no more of a session's body
// than a release records, however large the body; the rule at the cut catches a secret the cut
// ran through. A NUL byte is in no argument a process runs with, and the JSON a grant.used row
// holds refuses one, so a session that sends one has it replaced rather than failing its own
// release over a line of the log. It answers an empty vector, never nil, for a session that named
// no command, as a client from before the field does.
func loggedCommand(command, handedOut []string) []string {
	return redactCommand(command, handedOut, redactArgument)
}

// redactCommand is loggedCommand with the redaction of one argument passed in, which is how a
// test counts the bytes the redaction is handed.
func redactCommand(command, handedOut []string, redact func(arg string, values []string) string) []string {
	values := slices.DeleteFunc(slices.Clone(handedOut), func(v string) bool { return len(v) < minRedactedBytes })
	logged := make([]string, 0, min(len(command), record.MaxCommandArguments))
	for _, arg := range command[:min(len(command), record.MaxCommandArguments)] {
		arg = strings.ReplaceAll(redact(record.ClipArgument(arg), values), "\x00", "\uFFFD")
		logged = append(logged, record.ClipArgument(arg))
	}
	return logged
}

// redactArgument is arg with each run that holds a secret replaced by one redactedArgument: every
// place a value of values occurs, overlapping ones included; every knownCredential match; and,
// in an argument that record.ReachesTheCut, an end that is the start of a value or of a token
// shape, which is what a cut leaves of a secret it ran through. It marks every such byte before
// it writes anything, so two secrets that overlap are one run with no tail of either left over,
// and a marker the replacement writes is never matched again.
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
	if record.ReachesTheCut(arg) {
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
// name the airlock holds back has its split already; any other is taken apart as its type says.
// A value its type cannot take apart is its whole self and its JSON strings alone.
func (r releasedName) secretParts() []string {
	parts := []string{r.value}
	split := r.split
	if !r.behindAirlock() {
		split, _ = airlockSplitOf("", r.secretType, r.value)
	}
	for _, value := range split.values {
		parts = append(parts, value)
	}
	if r.secretType == "" {
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
