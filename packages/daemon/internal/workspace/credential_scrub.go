package workspace

import (
	"encoding/base64"
	"strings"

	"github.com/sjawhar/envoy/oauth2call"
)

// shown is text a run under the credential wrote, as an error made of it prints it once it leaves
// the process and nothing decides on it: the shared clone's steps' (cloneSteps.runIn) and its
// fetch's (ensureFetchConfiguration), which Provision returns to the daemon's outbox
// (outbox.provisionIn) and to workspace-init's caller. It is scrub's text read once more by the
// fail-closed rule (oauth2call.Field), which prints it as WordsNotReadable where it cannot read a
// URL in it as clean, and on one line (Go-quoted where git wrote more than one). The rule reads the
// text after the URL rule, by design: git's refusal of a redirect names the asked-for URL with its
// query and more text after it, which the rule cannot read as clean, so read before the URL rule
// every such refusal would print as WordsNotReadable. What that order gives up is a query value
// whitespace parts from its URL (`?access_token= <value>`): the URL rule takes its `?…=` away, and
// the fail-closed rule then reads the value as a word.
func (r remote) shown(text string) string {
	return oauth2call.Field(strings.TrimSpace(r.scrub(text)))
}

// scrubTokens is text with each of tokens, the provisioning tokens a run of git held, taken out
// wherever git, or whatever answered in GitHub's place, wrote it back: each token whole replaced by
// ***, and then every run of oauth2call.TokenPiece characters of a token, or of the Basic
// credential git sends with it (Authorization: Basic base64(provisioningUser:token)), redacted by
// the rule envoy prints a request's echo by (oauth2call.Tokens), in the text as git wrote it and in
// its percent-decoded reading (redactDecoded). A redirect's Location can carry a token cut short
// or percent-encoded, and an ERR packet or a sideband line the Authorization header git sent; none
// of those is the token as written. A token shorter than the piece is redacted only whole, as the
// rule redacts one. Fetch's classifiers decide on what this answers (transientCloneFailure,
// outsideVolumeFailure), and it replaces only runs of a credential's characters: a GitHub token
// (ghs_ and letters and digits) and a base64 value hold no space, and every phrase the classifiers
// match holds one but SSL_ERROR_SYSCALL, a run of which only a token beginning ghs_SYSCALL holds.
func scrubTokens(text string, tokens ...string) string {
	var credentials []string
	for _, token := range tokens {
		if token == "" {
			continue
		}
		text = strings.ReplaceAll(text, token, "***")
		credentials = append(credentials, token, base64.StdEncoding.EncodeToString([]byte(provisioningUser+":"+token)))
	}
	if len(credentials) == 0 {
		return text
	}
	return oauth2call.Tokens(redactDecoded(text, credentials), -1, credentials...)
}

// redactDecoded is text with each span of it that a run of credentials in its percent-decoded
// reading (percentDecoded) came from replaced by ***: a run some or all of whose characters text
// writes as %XX escapes, in either case, which the rule, reading the text as it is, finds no run
// of. A window of oauth2call.TokenPiece characters of the reading (fewer at its end) holds a run
// where the rule changes it (oauth2call.Tokens), and the escapes and bytes it was read from are its
// span, so the spans are the rule's own reading of the decoded text, whichever of a run's bytes
// were encoded. The windows are read only where the reading holds a run at all: a text with no
// escape, or none of a credential in its reading, costs one reading.
func redactDecoded(text string, credentials []string) string {
	decoded, from := percentDecoded(text)
	if decoded == text || oauth2call.Tokens(decoded, -1, credentials...) == decoded {
		return text
	}
	starts := make([]int, 0, len(decoded)+1)
	for at := range decoded {
		starts = append(starts, at)
	}
	starts = append(starts, len(decoded))
	covered := make([]bool, len(text))
	for at := range len(starts) - 1 {
		end := min(at+oauth2call.TokenPiece, len(starts)-1)
		window := decoded[starts[at]:starts[end]]
		if oauth2call.Tokens(window, -1, credentials...) != window {
			for b := from[starts[at]]; b < from[starts[end]]; b++ {
				covered[b] = true
			}
		}
	}
	var out strings.Builder
	for at := 0; at < len(text); {
		kept := at
		for at < len(text) && !covered[at] {
			at++
		}
		out.WriteString(text[kept:at])
		if at == len(text) {
			break
		}
		for at < len(text) && covered[at] {
			at++
		}
		out.WriteString("***")
	}
	return out.String()
}

// percentDecoded is text with each %XX escape (X a hexadecimal digit, in either case) read as the
// byte it names, a '%' with no two such digits after it read as itself, and from: from[i] is the
// offset in text of what the reading's byte i was read from, its escape's '%' or the byte itself,
// and from[len(decoded)] is len(text). A text with no '%' is its own reading, with no from.
func percentDecoded(text string) (decoded string, from []int) {
	if !strings.Contains(text, "%") {
		return text, nil
	}
	read := make([]byte, 0, len(text))
	from = make([]int, 0, len(text)+1)
	for at := 0; at < len(text); {
		from = append(from, at)
		if text[at] == '%' && at+2 < len(text) {
			high, highOK := hexDigit(text[at+1])
			low, lowOK := hexDigit(text[at+2])
			if highOK && lowOK {
				read = append(read, high<<4|low)
				at += 3
				continue
			}
		}
		read = append(read, text[at])
		at++
	}
	return string(read), append(from, len(text))
}

// hexDigit is the value of c as a hexadecimal digit, in either case, and whether it is one.
func hexDigit(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
