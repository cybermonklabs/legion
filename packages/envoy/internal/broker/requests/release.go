// packages/envoy/internal/broker/requests/release.go
// The one release loop of a grant, which both of its releases run: the session's own (Values,
// machine_read.go) and the proxy's (AirlockValues, airlock.go). It reads or mints each granted
// name, splits the names the airlock holds back into their slots, and writes the one grant.used
// row and the one log line the release writes, with the detail of whichever caller wrote it. One
// loop rather than two, because a fix to the split, to that row, or to what a narrowing may reach
// must not land in one release and not in the other.
package requests

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// ReleasedMessage is logged, at INFO, once for each release of a grant, once its grant.used row
// is written, followed by grant=, request=, enrollment=, names=<the names it released>,
// reason=<the reason the grant's request gave> and then, for the session's release (Values),
// command=<the argument vector the session said the release starts, as loggedCommand records
// it: empty from a client from before the field>, or, for the proxy's (AirlockValues),
// airlock=true, since that release starts no command. It is the operator's line for what each
// grant was spent on, and never holds a value the release handed out.
const ReleasedMessage = "broker: released a grant"

// The bounds of the command a release records (loggedCommand). The session names its own
// command, so without them one release could write a row and a line of any length.
const (
	maxLoggedArguments     = 32
	maxLoggedArgumentBytes = 256
)

// redactedArgument is what a release records in place of each value it handed out that the
// command holds.
const redactedArgument = "[redacted]"

// grantUse is one release as its grant.used row and its ReleasedMessage line record it, beside
// the reason of the grant's request, which checkRelease read: the names it released and what its
// caller adds.
type grantUse struct {
	names []string
	// airlock marks the proxy's release (AirlockValues), which starts no command and narrows
	// nothing; command and narrowing are the session's release's (Values) alone.
	airlock bool
	// command is the argument vector the session's release starts, as loggedCommand records it.
	command []string
	// narrowing is the GitHub App narrowing the session's release applied, nil for none.
	narrowing *secrets.GitHubNarrowing
}

// detail is the grant.used row's detail of this release of a grant whose request gave reason.
func (u grantUse) detail(reason string) auditDetail {
	detail := auditDetail{"names": u.names, "reason": reason}
	if u.airlock {
		detail["airlock"] = true
		return detail
	}
	detail["command"] = u.command
	if u.narrowing != nil {
		detail["github_app"] = auditDetail{"owner": u.narrowing.Owner, "repositories": u.narrowing.Repositories, "read_only": u.narrowing.ReadOnly}
	}
	return detail
}

// loggedCommand is command as a release records it: its first maxLoggedArguments arguments, in
// each every value in handedOut replaced by redactedArgument and a NUL byte by U+FFFD, then cut
// to its first maxLoggedArgumentBytes bytes on a character's boundary. It redacts before it
// cuts, so a value that crosses the cut leaves no prefix of itself behind, and it replaces the
// longest values first, so a value that holds another, such as a minted document and the token
// inside it, is replaced whole. A NUL byte is in no argument a process runs with, and the JSON a
// grant.used row holds refuses one, so a session that sends one has it replaced rather than
// failing its own release over a line of the log. It answers an empty vector, never nil, for a
// session that named no command, as a client from before the field does.
func loggedCommand(command, handedOut []string) []string {
	values := slices.DeleteFunc(slices.Clone(handedOut), func(v string) bool { return v == "" })
	slices.SortFunc(values, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	logged := make([]string, 0, min(len(command), maxLoggedArguments))
	for _, arg := range command[:min(len(command), maxLoggedArguments)] {
		for _, value := range values {
			arg = strings.ReplaceAll(arg, value, redactedArgument)
		}
		arg = strings.ReplaceAll(arg, "\x00", "\uFFFD")
		if len(arg) > maxLoggedArgumentBytes {
			cut := maxLoggedArgumentBytes
			for cut > 0 && !utf8.RuneStart(arg[cut]) {
				cut--
			}
			arg = arg[:cut]
		}
		logged = append(logged, arg)
	}
	return logged
}

// handedOut is every value a session's release handed out or read, for loggedCommand to redact:
// each released name's secret parts (secretParts), and, for the names the airlock holds back,
// what the session holds in their place — each name's filled value, each slot's placeholder and
// the proxy password, which is one of them — since a placeholder is what opens the proxy.
func handedOut(read []releasedName, held *AirlockRelease) []string {
	var values []string
	for _, released := range read {
		values = append(values, released.secretParts()...)
	}
	if held != nil {
		values = append(values, held.ProxyPassword)
		for _, name := range held.Names {
			values = append(values, name.Value)
			for _, placeholder := range name.Slots {
				values = append(values, placeholder)
			}
		}
	}
	return values
}

// releaseRead is what one caller wants of the shared read. The session's release asks for every
// granted name and may narrow; the proxy's asks for the names behind the airlock alone, since
// the session was handed the rest at its own release.
type releaseRead struct {
	// narrowing is this release's GitHub App narrowing, nil for a release that asks for none.
	// It reaches the github-app minter of every github-app name the grant holds.
	narrowing *secrets.GitHubNarrowing
	// airlockOnly leaves every name the airlock does not hold back out of the read: no value of
	// it is read, and none is minted.
	airlockOnly bool
}

// releasedName is one granted name as a release read it.
type releasedName struct {
	name string
	// value is what the store or the minter answered. It is what the session is handed for a
	// name the airlock does not hold back, and goes no further than the loop for one it does,
	// which the session holds placeholders for instead.
	value string
	// split is how that value takes apart into slots, for a name the airlock holds back: the
	// real value behind each slot, which the proxy swaps into a request, and how the shape the
	// session holds is filled with those slots' placeholders. airlockSplitOf always answers a
	// map, so a nil one is what says the airlock does not hold this name back (behindAirlock).
	split airlockSplit
	// expires is when a minted value stops working, zero for a stored one, and kind the policy
	// kind that says what the slots are. The proxy holds both beside the values: the first is
	// what it mints again before, the second what says how to spend them.
	expires time.Time
	kind    string
}

// behindAirlock reports whether the airlock's rules hold this name back, which is what says
// whether its split was taken.
func (r releasedName) behindAirlock() bool { return r.split.values != nil }

// secretParts are the strings of this name's value that a command holding any of them holds a
// secret by: the whole value, and what each slot of it holds as the airlock takes the value apart
// (airlockSplitOf), which for a minted document is each token inside it rather than the fields
// around it. A name the airlock holds back has its split already; any other is taken apart as
// its kind says, with no entry, so a stored value is its whole self alone. A value its kind cannot
// take apart is its whole self alone too.
func (r releasedName) secretParts() []string {
	parts := []string{r.value}
	split := r.split
	if !r.behindAirlock() {
		var err error
		if split, err = airlockSplitOf(&airlock.SecretRule{}, r.kind, r.value); err != nil {
			return parts
		}
	}
	for _, value := range split.values {
		parts = append(parts, value)
	}
	return parts
}

// readRelease reads every name checked found granted — or, for the proxy, the names behind the
// airlock alone — in the grant's own name order, and takes the split of each name the airlock
// holds back. Once every value is read, each stored one is checked against the policy as it
// stands then (recheckStoredSources), so a retag that lands during the reads never releases
// what the secret stores once it is a kind.
func (m *Machine) readRelease(ctx context.Context, grantID string, checked grantRelease, want releaseRead) ([]releasedName, error) {
	read := make([]releasedName, 0, len(checked.granted))
	for _, g := range checked.granted {
		entry := m.Airlock.Lookup(g.name)
		if entry == nil && want.airlockOnly {
			continue
		}
		served := checked.set.Secrets[g.name]
		minted, err := m.readValue(ctx, grantID, checked.enrollmentID, g, served, want.narrowing)
		if err != nil {
			return nil, err
		}
		released := releasedName{name: g.name, value: minted.Value, expires: minted.Expires, kind: served.Kind()}
		if entry != nil {
			if released.split, err = airlockSplitOf(entry, released.kind, minted.Value); err != nil {
				return nil, fmt.Errorf("release %s behind the airlock: %w", g.name, err)
			}
		}
		read = append(read, released)
	}
	if err := m.recheckStoredSources(ctx, grantID, checked.granted); err != nil {
		return nil, err
	}
	return read, nil
}

// releasedNames is every name a read answered, in the grant's own name order: what the
// grant.used row records as having left the broker.
func releasedNames(read []releasedName) []string {
	names := make([]string, len(read))
	for i, released := range read {
		names[i] = released.name
	}
	return names
}

// auditGrantUsed writes the one grant.used row a release writes and then its ReleasedMessage
// line, both naming the reason of the grant's request beside use, the caller's own: the names it
// released and, for the session's release, the command it starts and the narrowing it applied;
// for the proxy's, that the values went behind the airlock. The row says the values did leave
// the broker, so each caller writes it last, once everything that release hands over is minted
// and recorded; the line follows the row, so a release whose row failed logs nothing.
func (m *Machine) auditGrantUsed(ctx context.Context, grantID string, checked grantRelease, use grantUse) error {
	if _, err := m.Store.Pool.Exec(ctx, `insert into audit (kind, enrollment_id, request_id, grant_id, actor, detail) values ('grant.used',$1,$2,$3,$4,$5)`,
		checked.enrollmentID, checked.requestID, grantID, "session:"+checked.enrollmentID, use.detail(checked.reason)); err != nil {
		return err
	}
	attrs := []any{"grant", grantID, "request", checked.requestID, "enrollment", checked.enrollmentID, "names", use.names, "reason", checked.reason}
	if use.airlock {
		attrs = append(attrs, "airlock", true)
	} else {
		attrs = append(attrs, "command", use.command)
	}
	slog.Info(ReleasedMessage, attrs...)
	return nil
}
