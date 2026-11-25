// packages/envoy/internal/cipher/requests/release.go
// The one release loop of a grant, which both of its releases run: the session's own (Values,
// machine_read.go) and the proxy's (AirlockValues, airlock.go). It reads or mints each granted
// name, splits the names the airlock holds back into their slots, and writes the one grant.used
// row and the one log line the release writes, with the detail of whichever caller wrote it. One
// loop rather than two, because a fix to the split or to that row must not land in one release
// and not in the other.
package requests

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// ReleasedMessage is logged, at INFO, once for each release of a grant, once its grant.used row
// is written, followed by grant=, request=, enrollment= and the fields grantUse.recorded answers,
// which the row's detail holds too: names=<the names it released>, reason=<the reason the grant's
// request gave> and then, for the session's release (Values), call_reason=<the reason the
// session's own call gave, as record.Sanitize leaves it> where that says something the grant's
// reason does not, and command=<the argument vector the session said the release starts, as
// loggedCommand records it: empty from a client from before the field>; or, for the proxy's
// (AirlockValues), airlock=true, since that release starts no command. It is the operator's line
// for what each grant was spent on and why.
//
// What the command does not hold is what loggedCommand redacts (redact.go): each value this
// release handed out of eight bytes or more, each string of eight bytes or more inside a stored
// JSON value, each token inside a minted document, each airlock placeholder, and every credential
// of a shape knownCredential matches, whatever its source. A secret that is none of these — a
// value from another store, a password typed in, a token of a shape not listed — is logged as
// the session typed it. A session passes its secrets in the command's environment, as the exec
// form does, and never on its command line.
const ReleasedMessage = "broker: released a grant"

// ReleaseCall is what a session says of one release of its grant (Values): the command the
// release starts, nil for none, and the reason its own call gave, "" for none. That reason can
// differ from the reason of the request the grant came of, since a session that asks again for
// names it holds a live grant of gets that grant back (reuseLiveGrant) and its new reason goes
// into no request row. A client from before either field sends neither.
type ReleaseCall struct {
	Command []string
	Reason  string
}

// grantUse is one release as its grant.used row and its ReleasedMessage line record it, beside
// the reason of the grant's request, which checkRelease read: the names it released and what its
// caller adds.
type grantUse struct {
	names []string
	// airlock marks the proxy's release (AirlockValues), which starts no command; command and
	// callReason are the session's release's (Values) alone.
	airlock bool
	// command is the argument vector the session's release starts, as loggedCommand records it.
	command []string
	// callReason is the reason the session's own call gave, as callReasonOf records it: "" when
	// it gave none or gave the grant's own.
	callReason string
}

// recordedField is one key and value a release records, in its grant.used row's detail and in
// its ReleasedMessage line alike.
type recordedField struct {
	key   string
	value any
}

// recorded is every field this release of a grant whose request gave reason records, in the
// order the line writes them. The row and the line both write exactly these, so a field added
// here reaches both.
func (u grantUse) recorded(reason string) []recordedField {
	fields := []recordedField{{"names", u.names}, {"reason", reason}}
	if u.airlock {
		return append(fields, recordedField{"airlock", true})
	}
	if u.callReason != "" {
		fields = append(fields, recordedField{"call_reason", u.callReason})
	}
	return append(fields, recordedField{"command", u.command})
}

// callReasonOf is the reason a session's call gave (said) as its release records it, beside
// granted, the reason of the request the grant came of: held to a request's own rules by
// record.Sanitize, since nothing refused it before the release and a field of the log must not
// stop one. It answers "" when that leaves nothing or leaves granted itself, so the row and the
// line name a call's reason only where it says something the grant's does not.
func callReasonOf(said, granted string) string {
	if cleaned := record.Sanitize(said, record.MaxReasonRunes); cleaned != granted {
		return cleaned
	}
	return ""
}

// releaseRead is what one caller wants of the shared read. The session's release asks for every
// granted name; the proxy's asks for the names behind the airlock alone, since the session was
// handed the rest at its own release.
type releaseRead struct {
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
	// expires is when a minted value stops working, zero for a stored one, and type the policy
	// type that says what the slots are. The proxy holds both beside the values: the first is
	// what it mints again before, the second what says how to spend them.
	expires    time.Time
	secretType string
	// gitHubValue is the github.value of the GitHub profile the name is served as
	// (githubname.ValueInstallations or githubname.ValueToken), "" for any other name: with the
	// type, what says how the value takes apart into slots (airlockSplitOf), and what the value
	// is to a give-back (GiveBack).
	gitHubValue string
	// leftOut is, for a github-app profile, each entry the cut of its permissions to what an
	// installation holds left out, by account (secrets.Minted.LeftOut); nil for any other name
	// and where the cut left nothing out.
	leftOut map[string][]string
}

// behindAirlock reports whether the airlock's rules hold this name back, which is what says
// whether its split was taken.
func (r releasedName) behindAirlock() bool { return r.split.values != nil }

// readRelease reads every name checked found granted — or, for the proxy, the names behind the
// airlock alone — in the grant's own name order, and takes the split of each name the airlock
// holds back. Once every value is read, each stored one is checked against the policy as it
// stands then (recheckStoredSources), so a retag that lands during the reads never releases
// what the secret stores once it is a type. A read that fails after it minted a value for an
// earlier name, or for this one, hands that value to nobody, so it starts each such value's
// give-back (giveBack) before it answers the failure; the give-back runs in the background, and a
// broker that stops first cuts it short and leaves the token to lapse.
func (m *Machine) readRelease(ctx context.Context, grantID string, checked grantRelease, want releaseRead) (_ []releasedName, err error) {
	read := make([]releasedName, 0, len(checked.granted))
	defer func() {
		if err != nil {
			m.giveBack(ctx, read)
		}
	}()
	for _, g := range checked.granted {
		// The name's proxy block, nil for a name no profile puts behind the proxy. Every entry
		// binds at least one host (proxyrules.Validate), so an entry is a name the airlock holds back.
		entry := m.Airlock.Lookup(g.name)
		// A name under an entry of kind dpop is a CI run's results capability, which the broker
		// mints and holds, and never a stored secret's release: it is refused before anything of
		// it is read.
		if entry != nil && entry.Kind == proxyrules.KindDPoP {
			return nil, fmt.Errorf("release %s: %w", g.name, ErrAirlockMintedOnly)
		}
		if entry == nil && want.airlockOnly {
			continue
		}
		served := checked.set.Secrets[g.name]
		minted, err := m.readValue(ctx, grantID, checked.enrollmentID, g, served)
		if err != nil {
			return nil, err
		}
		// Held for the give-back as soon as it is read, so a split that refuses this value gives
		// it back as well as every value before it.
		read = append(read, releasedName{name: g.name, value: minted.Value, expires: minted.Expires, secretType: served.Type(),
			gitHubValue: gitHubValueOf(served), leftOut: minted.LeftOut})
		if entry != nil {
			released := &read[len(read)-1]
			if released.split, err = airlockSplitOf(entry.Kind, released.secretType, released.gitHubValue, minted.Value); err != nil {
				return nil, fmt.Errorf("release %s behind the airlock: %w", g.name, err)
			}
		}
	}
	if err := m.recheckStoredSources(ctx, grantID, checked.granted); err != nil {
		return nil, err
	}
	return read, nil
}

// gitHubValueOf is the github.value of the GitHub profile served is, "" for a name that is no
// GitHub profile.
func gitHubValueOf(served policy.Secret) string {
	if served.Profile == nil || served.Profile.GitHub == nil {
		return ""
	}
	return served.Profile.GitHub.Value
}

// giveBack hands each value in read that a type's minter made back to that type's upstream
// (Machine.GiveBacks), with the value form of the GitHub profile it was minted for
// (gitHubValue), for a release that minted it and hands it to nobody: one that failed, or a
// name the airlock holds back, whose session gets placeholders (heldBack). A stored value, and a
// value of a type with no GiveBack, is left as it is. It runs in the background, detached from
// ctx, and the release answers at once: nothing the release answers depends on it, and a
// give-back that waited on GitHub, up to its bound for each installation of each value, could
// hold a session's release past the time its client gives the call. Each GiveBack bounds its own
// revokes; one a stopping broker cuts short leaves its token to lapse, as before the give-back.
// A GiveBack that panics leaves its value to lapse too, logged (recoverGiveBack), and the
// give-back goes on to the next value: net/http recovers a panic only on a handler's own
// goroutine, so one here would end the broker.
func (m *Machine) giveBack(ctx context.Context, read []releasedName) {
	type minted struct {
		giveBack          GiveBack
		name, value, form string
	}
	var back []minted
	for _, released := range read {
		if giveBack := m.GiveBacks[released.secretType]; giveBack != nil && released.value != "" {
			back = append(back, minted{giveBack, released.name, released.value, released.gitHubValue})
		}
	}
	if len(back) == 0 {
		return
	}
	detached := context.WithoutCancel(ctx)
	m.givingBack.Add(1)
	go func() {
		defer m.givingBack.Done()
		for _, each := range back {
			func() {
				defer recoverGiveBack(each.name)
				each.giveBack(detached, each.name, each.value, each.form)
			}()
		}
	}()
}

// giveBackPanickedMessage is logged, at ERROR, each time a GiveBack panics (recoverGiveBack),
// followed by name=<the secret's name>, error=<what panicText says of the panic> and stack=<the
// goroutine's stack>, which hold no part of the value it was giving back. That value is left to
// lapse.
const giveBackPanickedMessage = "broker: a give-back of a minted value panicked; the value is left to lapse"

// recoverGiveBack, deferred directly around one value's GiveBack (giveBack), logs a panic there
// (giveBackPanickedMessage) and lets the give-back go on to the next value.
func recoverGiveBack(name string) {
	if p := recover(); p != nil {
		slog.Error(giveBackPanickedMessage, "name", name, "error", panicText(p), "stack", string(debug.Stack()))
	}
}

// panicText is what a log line or an error says of the panic p, from code that may hold a secret
// value or a token: a runtime error's message, which names types and indexes and no data, or else
// the panic value's type alone, since a value a panic was given can be anything that code held.
func panicText(p any) string {
	if err, ok := p.(runtime.Error); ok {
		return err.Error()
	}
	return fmt.Sprintf("a panic of type %T", p)
}

// heldBack is the names in read the airlock holds back. A session's release reads or mints each
// one's value only to learn the slots it hands placeholders for, and the proxy reads or mints a
// value of its own when it first carries one (AirlockValues), so nobody holds the value read here.
func heldBack(read []releasedName) []releasedName {
	var held []releasedName
	for _, released := range read {
		if released.behindAirlock() {
			held = append(held, released)
		}
	}
	return held
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
// line, both holding the fields use records beside the reason of the grant's request
// (grantUse.recorded). The row says the values did leave the broker, so each caller writes it
// last, once everything that release hands over is minted and recorded; the line follows the
// row, so a release whose row failed logs nothing.
func (m *Machine) auditGrantUsed(ctx context.Context, grantID string, checked grantRelease, use grantUse) error {
	fields := use.recorded(checked.reason)
	detail := make(auditDetail, len(fields))
	attrs := make([]any, 0, 6+2*len(fields))
	attrs = append(attrs, "grant", grantID, "request", checked.requestID, "enrollment", checked.enrollmentID)
	for _, field := range fields {
		detail[field.key] = field.value
		attrs = append(attrs, field.key, field.value)
	}
	if _, err := m.Store.Pool.Exec(ctx, `insert into audit (kind, enrollment_id, request_id, grant_id, actor, detail) values ('grant.used',$1,$2,$3,$4,$5)`,
		checked.enrollmentID, checked.requestID, grantID, "session:"+checked.enrollmentID, detail); err != nil {
		return err
	}
	slog.Info(ReleasedMessage, attrs...)
	return nil
}
