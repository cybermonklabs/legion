package runtime

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// SpendAttributesVariable is the OpenTelemetry resource attributes variable, the one place a
// launch's process says what the dollars it spends were spent on. The runtime composes its value
// from the operator's own and the launch (SpendResourceAttributes) and sets it on the agent's
// process after the operator's environment: internal/runtime/sandbox/manifest.go's
// roleOperatorEnv under Kubernetes, internal/runtime/tmux/spawn.go's panePairs under tmux.
//
// What the value must satisfy is not the specification but the one parser that reads it: the
// environment detector of @opentelemetry/resources inside the worker image's Oh My Pi
// (/opt/omp/bin/omp, EnvDetector._parseResourceAttributes), read out of the binary on 2026-11-11.
// It splits the value on commas, drops a member whose trimmed text is empty, and for every other
// member throws unless the member splits on `=` into exactly two parts, its key is non-empty once
// trimmed, both sides percent-decode, and neither decodes to more than 255 UTF-16 code units.
//
// Its caller wraps the whole parse in one try whose catch assigns nothing, so ONE malformed
// member costs the process every attribute of the variable — the operator's theorem.project with
// it, which is the attribute the spend report splits by today. That is why the daemon appends to
// the operator's value instead of composing one of its own, why SpendResourceAttributes writes no
// empty member and percent-encodes everything it writes, and why the configuration loader refuses
// an operator value SpendAttributesRefusal refuses rather than letting a launch carry it.
//
// Assignment inside the detector is last-wins, so the daemon's pairs are written last: they win a
// key the operator's value already holds.
const SpendAttributesVariable = "OTEL_RESOURCE_ATTRIBUTES"

// SpendIssueAttribute and SpendRoleAttribute are the two attribute keys the daemon writes into
// SpendAttributesVariable itself, so that a pod's spend splits by the issue it was spent on and
// the role that spent it. They are named here once, in one place, so that the composer that
// writes them and the refusal that keeps an operator's own value out of them cannot drift apart.
const (
	SpendIssueAttribute = "legion.issue"
	SpendRoleAttribute  = "legion.role"
)

// spendTrailing is what SpendResourceAttributes drops off the end of an operator's value before
// it appends: commas, and the spaces the detector's own trim would drop around the empty member
// they leave behind.
const spendTrailing = ", \t\n\v\f\r"

// spendHex is the upper-case hex RFC 3986 writes a percent escape in.
const spendHex = "0123456789ABCDEF"

// SpendResourceAttributes is the whole value a launch's process carries in
// SpendAttributesVariable: the operator's own value first, then legion.issue, then legion.role.
// The daemon's pairs come last because the detector's assignment is last-wins, so a key the
// operator's value already holds is the launch's by being written after it.
//
// A pair whose fact the spec does not hold is left out rather than written empty, since the
// detector keeps `legion.issue=` as an attribute with an empty value rather than dropping it, and
// an empty issue attribute on every controller pod would split the spend report by nothing. The
// project controller's launch has no issue (claim.RoleController holds none) and carries
// legion.role alone; the daemon's own boot-gate probe (internal/daemon/bootgate.go's
// gateEnvironment) has neither, since it serves no issue and runs in no role, and carries the
// operator's value alone, with no legion pair and no stray comma.
//
// The result never holds an empty member or a leading comma: trailing commas and spaces of the
// operator's value are dropped, and an operator value that is empty, or only commas and spaces,
// contributes nothing at all. The detector would drop such a member itself, but an operator who
// reads a pod's environment should find a value no stranger than the one they wrote.
//
// Every value the daemon writes is percent-encoded — RFC 3986's unreserved characters (letters,
// digits, `-`, `.`, `_`, `~`) pass through, every other byte becomes %XX — so that the detector's
// decodeURIComponent gives back exactly the issue key and the role however they are spelled, and
// so that neither can carry a comma or an `=` into a member and cost the launch the whole
// variable. The two keys are unreserved as written and encode to themselves.
func SpendResourceAttributes(operator string, spec SpawnSpec) string {
	issue := spendEncode(spec.Issue)
	role := spendEncode(string(spec.Role))
	kept := strings.TrimRight(operator, spendTrailing)

	var b strings.Builder
	b.Grow(len(kept) + len(SpendIssueAttribute) + len(issue) + len(SpendRoleAttribute) + len(role) + 4)
	b.WriteString(kept)
	if spec.Issue != "" {
		spendMember(&b, SpendIssueAttribute, issue)
	}
	if spec.Role != "" {
		spendMember(&b, SpendRoleAttribute, role)
	}
	return b.String()
}

// spendMember appends one `key=value` member, with the comma that separates it from what is
// already there and no comma when nothing is.
func spendMember(b *strings.Builder, key, value string) {
	if b.Len() > 0 {
		b.WriteByte(',')
	}
	b.WriteString(key)
	b.WriteByte('=')
	b.WriteString(value)
}

// SpendAttributesRefusal is why an operator's own value of SpendAttributesVariable is one the
// detector would throw on, or "" when nothing here refuses it. The configuration loader asks it
// of the value the operator sets, because the throw is not that member's to pay: the detector's
// caller catches it with an empty hand, so the launch loses every attribute of the variable,
// including the operator's own theorem.project.
//
// It applies the measured rules in the detector's own order — an empty member is skipped as the
// detector skips it, then the split on `=` into exactly two parts, the non-empty key, the
// percent-decode of each side (a malformed escape and a decoded byte sequence that is not valid
// UTF-8 both throw in decodeURIComponent), then the 255-code-unit bound on each decoded side,
// which JavaScript counts in UTF-16 code units. It refuses one thing of its own beyond them: a
// member whose decoded key is SpendIssueAttribute or SpendRoleAttribute, which the runtime writes
// into every launch itself and would override without a word.
//
// The reason names the member it refuses and what is wrong with it, never the rest of the value:
// an operator reading it should see the one member to fix, and the attributes of the others are
// nobody's business to repeat.
func SpendAttributesRefusal(value string) string {
	for _, member := range strings.Split(value, ",") {
		trimmed := strings.TrimSpace(member)
		if trimmed == "" {
			continue
		}
		parts := strings.Split(member, "=")
		if len(parts) != 2 {
			return fmt.Sprintf("its member %q does not split on = into exactly one key and one value", trimmed)
		}
		key := strings.TrimSpace(parts[0])
		if key == "" {
			return fmt.Sprintf("its member %q has an empty key", trimmed)
		}
		decodedKey, problem := spendDecode(key)
		if problem != "" {
			return fmt.Sprintf("its member %q has a key with %s", trimmed, problem)
		}
		decodedValue, problem := spendDecode(strings.TrimSpace(parts[1]))
		if problem != "" {
			return fmt.Sprintf("its member %q has a value with %s", trimmed, problem)
		}
		if n := spendUTF16Len(decodedKey); n > 255 {
			return fmt.Sprintf("its member %q has a key of %d characters once percent-decoded, over the detector's 255", trimmed, n)
		}
		if n := spendUTF16Len(decodedValue); n > 255 {
			return fmt.Sprintf("its member %q has a value of %d characters once percent-decoded, over the detector's 255", trimmed, n)
		}
		if decodedKey == SpendIssueAttribute || decodedKey == SpendRoleAttribute {
			return fmt.Sprintf("its member %q names %s, which the runtime writes into every launch itself", trimmed, decodedKey)
		}
	}
	return ""
}

// spendEncode is the percent-encoding of one side of a member the daemon writes: RFC 3986's
// unreserved characters pass through and every other byte becomes %XX in upper-case hex, which is
// what decodeURIComponent undoes exactly. It counts first and allocates second, so the real
// values — every issue key and every role, which are unreserved throughout — cost no allocation.
func spendEncode(s string) string {
	escapes := 0
	for i := range len(s) {
		if !spendUnreserved(s[i]) {
			escapes++
		}
	}
	if escapes == 0 {
		return s
	}
	out := make([]byte, 0, len(s)+2*escapes)
	for i := range len(s) {
		if c := s[i]; spendUnreserved(c) {
			out = append(out, c)
		} else {
			out = append(out, '%', spendHex[c>>4], spendHex[c&0x0f])
		}
	}
	return string(out)
}

// spendUnreserved is RFC 3986's unreserved set, the bytes decodeURIComponent gives back as they
// stand: letters, digits, `-`, `.`, `_`, `~`.
func spendUnreserved(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	}
	return c == '-' || c == '.' || c == '_' || c == '~'
}

// spendDecode is decodeURIComponent as the detector runs it: each `%XX` is one byte, every other
// character stands, and a run of escaped bytes must be valid UTF-8 together. It answers the
// decoded text, or "" and the problem to name in a refusal — a malformed escape (`%` without two
// hex digits after it) or escapes that do not decode as UTF-8 (`%FF`, an overlong form, a
// surrogate half), each of which throws in the detector and costs the launch every attribute.
func spendDecode(s string) (string, string) {
	if !strings.ContainsRune(s, '%') {
		return s, ""
	}
	var b strings.Builder
	b.Grow(len(s))
	var escaped []byte
	flush := func() bool {
		if len(escaped) == 0 {
			return true
		}
		if !utf8.Valid(escaped) {
			return false
		}
		b.Write(escaped)
		escaped = escaped[:0]
		return true
	}
	for i := 0; i < len(s); {
		if s[i] != '%' {
			if !flush() {
				return "", "percent escapes that are not valid UTF-8"
			}
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+2 >= len(s) {
			return "", "a malformed percent escape"
		}
		hi, hiOK := spendUnhex(s[i+1])
		lo, loOK := spendUnhex(s[i+2])
		if !hiOK || !loOK {
			return "", "a malformed percent escape"
		}
		escaped = append(escaped, hi<<4|lo)
		i += 3
	}
	if !flush() {
		return "", "percent escapes that are not valid UTF-8"
	}
	return b.String(), ""
}

// spendUnhex is one hex digit of a percent escape, in either case, and whether it was one.
func spendUnhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// spendUTF16Len is the length the detector's `.length` reads: JavaScript counts UTF-16 code
// units, so every rune above the basic plane counts twice.
func spendUTF16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}
