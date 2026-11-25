package signedrecord_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return public, private
}

func sign(t *testing.T, key ed25519.PrivateKey, lines []string) string {
	t.Helper()
	sig, err := signedrecord.Sign(key, lines)
	if err != nil {
		t.Fatalf("sign %q: %v", lines, err)
	}
	return sig
}

// Each forgery a writer of the broker's tables could make is refused, naming the reason: a record
// with no signature, one signed by another key, a genuine record copied onto another key, a
// genuine older record read after a newer gen was accepted, another genuine record at the gen
// already accepted, a genuine record at a higher gen signed more than SignedAtSkew before the one
// accepted (a record of an earlier history of the tables), and a value changed after signing. A
// genuine record verifies, the record accepted reads again at its gen, and a higher gen signed
// within the allowance before it is accepted, so the refusals are the verifier's and not a reader
// that refuses everything. The fork row fails on a floor that keeps the gen alone, as it did at
// bb2b623923, and the backdated row on one that keeps no signing time, as at 4a4ebd8787.
func TestASignedRecordRefusesForgeriesAndReplays(t *testing.T) {
	operator, operatorKey := newKey(t)
	_, otherKey := newKey(t)
	keys := []ed25519.PublicKey{operator}
	value := []byte(`{"workflow":"cutover","ops":[],"complete":false}`)
	genuine := signedrecord.Record("hold-1", "done", "hold", 2, value)
	genuineSig := sign(t, operatorKey, genuine)
	if err := signedrecord.Verify(keys, genuineSig, genuine); err != nil {
		t.Fatalf("the genuine record does not verify: %v", err)
	}
	signedAt := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

	older := signedrecord.Record("hold-1", "done", "hold", 1, value)
	olderSig := sign(t, operatorKey, older)
	floor := &signedrecord.Floor{}
	if err := floor.Accept(2, signedAt, genuine); err != nil {
		t.Fatalf("the floor refuses the first gen it reads: %v", err)
	}
	if err := floor.Accept(2, signedAt, genuine); err != nil {
		t.Fatalf("the floor refuses the record it accepted, read again at its gen: %v", err)
	}
	skewed := &signedrecord.Floor{}
	if err := skewed.Accept(2, signedAt, genuine); err != nil {
		t.Fatal(err)
	}
	if err := skewed.Accept(3, signedAt.Add(-signedrecord.SignedAtSkew), signedrecord.Record("hold-1", "done", "hold", 3, value)); err != nil {
		t.Fatalf("the floor refuses a higher gen signed the whole allowance before the accepted record: %v", err)
	}

	for _, forgery := range []struct {
		name string
		read func() error
		want error
	}{
		{
			name: "no signature",
			read: func() error { return signedrecord.Verify(keys, "", genuine) },
			want: signedrecord.ErrNoSignature,
		},
		{
			name: "signed by another key",
			read: func() error { return signedrecord.Verify(keys, sign(t, otherKey, genuine), genuine) },
			want: signedrecord.ErrBadSignature,
		},
		{
			name: "a genuine record copied onto another key",
			read: func() error {
				return signedrecord.Verify(keys, genuineSig, signedrecord.Record("hold-1", "convert", "set", 2, value))
			},
			want: signedrecord.ErrBadSignature,
		},
		{
			name: "a genuine older record after a newer gen was accepted",
			read: func() error {
				if err := signedrecord.Verify(keys, olderSig, older); err != nil {
					return err
				}
				return floor.Accept(1, signedAt, older)
			},
			want: signedrecord.ErrReplay,
		},
		{
			name: "another genuine record at the gen already accepted",
			read: func() error {
				other := signedrecord.Record("hold-1", "done", "hold", 2, []byte(`{"workflow":"cutover","ops":[],"complete":true}`))
				if err := signedrecord.Verify(keys, sign(t, operatorKey, other), other); err != nil {
					return err
				}
				return floor.Accept(2, signedAt, other)
			},
			want: signedrecord.ErrFork,
		},
		{
			name: "a genuine record at a higher gen signed more than the allowance before the accepted one",
			read: func() error {
				earlier := signedrecord.Record("hold-1", "done", "hold", 4, value)
				if err := signedrecord.Verify(keys, sign(t, operatorKey, earlier), earlier); err != nil {
					return err
				}
				return floor.Accept(4, signedAt.Add(-signedrecord.SignedAtSkew-time.Second), earlier)
			},
			want: signedrecord.ErrBackdated,
		},
		{
			name: "a value changed after signing",
			read: func() error {
				return signedrecord.Verify(keys, genuineSig, signedrecord.Record("hold-1", "done", "hold", 2, []byte(`{"workflow":"cutover","ops":[],"complete":true}`)))
			},
			want: signedrecord.ErrBadSignature,
		},
	} {
		err := forgery.read()
		if !errors.Is(err, forgery.want) {
			t.Errorf("%s: read as %v, want it refused with %v", forgery.name, err, forgery.want)
		}
	}
}

// Admits judges a gen and signing time as Accept would, and moves nothing: the hold's routes ask it
// inside a move's transaction, before they write the row. A higher gen signed within the allowance
// is admitted and leaves the floor where it was; one signed past it is ErrBackdated, naming both
// times. And the floor judges by the latest signing time it has accepted, not the last: after gen 3
// signed 4 minutes before gen 2, gen 4 signed 6 minutes before gen 2 is refused, which a floor that
// kept gen 3's time (2 minutes before it) would accept, as at d08fb4a866.
func TestTheFloorAdmitsWithoutMovingAndKeepsTheLatestSigningTime(t *testing.T) {
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	lines := func(gen uint64) []string { return signedrecord.Record("hold-1", "done", "hold", gen, []byte(`{}`)) }
	floor := &signedrecord.Floor{}
	if err := floor.Admits(9, at.Add(-time.Hour)); err != nil {
		t.Fatalf("the zero floor does not admit a gen: %v", err)
	}
	if err := floor.Accept(2, at, lines(2)); err != nil {
		t.Fatal(err)
	}
	if err := floor.Admits(3, at.Add(-signedrecord.SignedAtSkew)); err != nil {
		t.Errorf("Admits refuses a higher gen signed the whole allowance before the accepted record: %v", err)
	}
	err := floor.Admits(3, at.Add(-signedrecord.SignedAtSkew-time.Second))
	if !errors.Is(err, signedrecord.ErrBackdated) || !strings.Contains(err.Error(), "11:54:59") || !strings.Contains(err.Error(), "12:00:00") {
		t.Errorf("Admits of a higher gen signed past the allowance answered %v, want ErrBackdated naming both times", err)
	}
	if !floor.Reached(2) || floor.Reached(3) {
		t.Errorf("Admits moved the floor: it reaches gen 3 %v, gen 2 %v; want only gen 2", floor.Reached(3), floor.Reached(2))
	}
	if err := floor.Accept(3, at.Add(-4*time.Minute), lines(3)); err != nil {
		t.Fatal(err)
	}
	err = floor.Accept(4, at.Add(-6*time.Minute), lines(4))
	if !errors.Is(err, signedrecord.ErrBackdated) {
		t.Errorf("after gen 3 signed 4 minutes before gen 2, gen 4 signed 6 minutes before gen 2 was read as %v, want ErrBackdated", err)
	} else if !strings.Contains(err.Error(), "before gen 2 signed at") || !strings.Contains(err.Error(), "12:00:00") {
		// The refusal names the gen whose time it judged by, gen 2, not the floor's gen 3, whose time
		// is 4 minutes earlier: fails while it prints gen 3 beside gen 2's time, as at 3d48321338.
		t.Errorf("the refusal %q does not name gen 2 and its time, the latest signing time accepted", err)
	}
}

// A signing time more than the allowance ahead of the reader's clock (a row stored before the hold's
// routes judged signing times, or read after the reader's clock jumped back) is no genuine row's, so
// a later gen is judged against the reader's clock instead: a gen signed now is admitted, by the
// floor and by NotBackdated alike, and one signed 6 minutes before now is not. Fails while the floor
// judges by a far-future time as it is, as at 3d48321338, and fails too with the time capped at the
// clock plus the allowance, which refuses a gen signed even a moment before the reader reads it.
// NotBackdated names the gen it compared with, and WithinSkewOf refuses either way.
func TestAFutureSigningTimeIsJudgedByTheReadersClock(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	lines := func(gen uint64) []string { return signedrecord.Record("hold-1", "done", "hold", gen, []byte(`{}`)) }
	floor := &signedrecord.Floor{}
	if err := floor.Accept(2, now.Add(20*time.Minute), lines(2)); err != nil {
		t.Fatal(err)
	}
	if err := floor.Admits(3, now); err != nil {
		t.Errorf("after a row signed 20 minutes ahead, Admits of a gen signed now answered %v, want nil", err)
	}
	if err := floor.Accept(3, now, lines(3)); err != nil {
		t.Errorf("after a row signed 20 minutes ahead, Accept of a gen signed now answered %v, want nil", err)
	}
	if err := floor.Admits(4, now.Add(-signedrecord.SignedAtSkew-time.Minute)); !errors.Is(err, signedrecord.ErrBackdated) {
		t.Errorf("Admits of a gen signed 6 minutes before now answered %v, want ErrBackdated", err)
	}
	if err := signedrecord.NotBackdated(5, now, 4, now.Add(20*time.Minute), now); err != nil {
		t.Errorf("NotBackdated against a time 20 minutes ahead answered %v, want nil", err)
	}
	err := signedrecord.NotBackdated(5, now.Add(-6*time.Minute), 4, now, now)
	if !errors.Is(err, signedrecord.ErrBackdated) || !strings.Contains(err.Error(), "gen 5") || !strings.Contains(err.Error(), "before gen 4 signed at") {
		t.Errorf("NotBackdated of a gen 6 minutes before gen 4 answered %v, want ErrBackdated naming gens 5 and 4", err)
	}
	for _, x := range []struct {
		off  time.Duration
		want error
	}{{4 * time.Minute, nil}, {-4 * time.Minute, nil}, {6 * time.Minute, signedrecord.ErrSkewed}, {-6 * time.Minute, signedrecord.ErrSkewed}} {
		if err := signedrecord.WithinSkewOf(now.Add(x.off), now); !errors.Is(err, x.want) || (x.want == nil && err != nil) {
			t.Errorf("WithinSkewOf a time %v off the clock answered %v, want %v", x.off, err, x.want)
		}
	}
}

// A gen judged against the reader's clock, because the latest signing time is more than the
// allowance ahead of it, is refused naming that clock, not the far-future time and its gen, which
// it was not compared with. Fails while the refusal prints "before gen 4 signed at" beside the
// far-future time, as at c23c8c1b87.
func TestARefusalJudgedByTheClockNamesTheClock(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	err := signedrecord.NotBackdated(5, now.Add(-6*time.Minute), 4, now.Add(20*time.Minute), now)
	if !errors.Is(err, signedrecord.ErrBackdated) {
		t.Fatalf("NotBackdated of a gen 6 minutes before the clock, after a time 20 minutes ahead, answered %v, want ErrBackdated", err)
	}
	if text := err.Error(); !strings.Contains(text, "gen 5 signed at 2026-11-21T11:54:00Z") || !strings.Contains(text, "the reader's clock reads 2026-11-21T12:00:00Z") ||
		strings.Contains(text, "gen 4") || strings.Contains(text, "12:20:00") {
		t.Errorf("the refusal %q does not name gen 5's time and the clock alone", text)
	}
}

// A signature names the key that made it: Sign writes <key id>.<signature>, the key id KeyID's
// fingerprint of the public half (the unpadded base64url of the first 8 bytes of its SHA-256),
// which holds no key material. Verify picks the listed key by that id: a list holding the key, a
// retired key beside it, verifies; a list without it answers *UnlistedKeyError carrying the id,
// which is ErrBadSignature too; a listed key that did not sign these lines is ErrBadSignature
// naming its id; and a key id that is not one is a signature that does not decode, so no text a
// writer puts there reaches an error. Fails while Sign writes no key id, as at c23c8c1b87, or
// Verify answers an unlisted key with a bare ErrBadSignature.
func TestASignatureNamesItsKeyAndVerifyPicksTheListedKeyByIt(t *testing.T) {
	operator, operatorKey := newKey(t)
	retired, _ := newKey(t)
	lines := signedrecord.HoldRow(3, "held", "hold-1", time.Unix(0, 0))
	digest := sha256.Sum256(operator)
	kid := signedrecord.KeyID(operator)
	if want := base64.RawURLEncoding.EncodeToString(digest[:8]); kid != want {
		t.Fatalf("KeyID = %q, want %q, the first 8 bytes of the key's SHA-256", kid, want)
	}
	sig := sign(t, operatorKey, lines)
	if !strings.HasPrefix(sig, kid+".") {
		t.Fatalf("Sign wrote %q, which does not open with its key's id %q and a dot", sig, kid)
	}
	if err := signedrecord.Verify([]ed25519.PublicKey{retired, operator}, sig, lines); err != nil {
		t.Errorf("Verify with the key listed after a retired one answered %v, want nil", err)
	}
	err := signedrecord.Verify([]ed25519.PublicKey{retired}, sig, lines)
	var unlisted *signedrecord.UnlistedKeyError
	if !errors.As(err, &unlisted) || unlisted.KeyID != kid || !errors.Is(err, signedrecord.ErrBadSignature) || !strings.Contains(err.Error(), kid) {
		t.Errorf("Verify with the key not listed answered %v, want an UnlistedKeyError carrying %s that is ErrBadSignature", err, kid)
	}
	if err != nil && strings.Contains(err.Error(), base64.RawURLEncoding.EncodeToString(operator)) {
		t.Errorf("the refusal %q holds the key itself", err)
	}
	err = signedrecord.Verify([]ed25519.PublicKey{operator}, sig, signedrecord.HoldRow(4, "held", "hold-1", time.Unix(0, 0)))
	if !errors.Is(err, signedrecord.ErrBadSignature) || errors.As(err, &unlisted) || !strings.Contains(err.Error(), kid) {
		t.Errorf("Verify of other lines under the listed key answered %v, want ErrBadSignature naming %s and no UnlistedKeyError", err, kid)
	}
	_, signature, _ := strings.Cut(sig, ".")
	err = signedrecord.Verify([]ed25519.PublicKey{operator}, "<b>x</b>."+signature, lines)
	if !errors.Is(err, signedrecord.ErrBadSignature) || errors.As(err, &unlisted) || strings.Contains(err.Error(), "<b>") {
		t.Errorf("Verify of a key id that is not one answered %v, want ErrBadSignature naming no part of it", err)
	}
}

// A signature with no key id, as Sign wrote before signatures named their key (the tests' fixtures
// and the model's stored rows), still verifies by trying each listed key; when none verifies it,
// the refusal says "signed with no key id", so the reader knows why it names none. The first half
// fails with a Verify that takes only signatures naming a key, the second with a refusal that does
// not say so, as at c23c8c1b87.
func TestASignatureWithNoKeyIDVerifiesByTryingEachListedKey(t *testing.T) {
	operator, operatorKey := newKey(t)
	other, _ := newKey(t)
	lines := signedrecord.HoldRow(3, "held", "hold-1", time.Unix(0, 0))
	bare := base64.RawURLEncoding.EncodeToString(ed25519.Sign(operatorKey, []byte(strings.Join(lines, "\n"))))
	if err := signedrecord.Verify([]ed25519.PublicKey{other, operator}, bare, lines); err != nil {
		t.Errorf("Verify of a signature with no key id, under a listed key, answered %v, want nil", err)
	}
	err := signedrecord.Verify([]ed25519.PublicKey{other}, bare, lines)
	var unlisted *signedrecord.UnlistedKeyError
	if !errors.Is(err, signedrecord.ErrBadSignature) || errors.As(err, &unlisted) || !strings.Contains(err.Error(), "signed with no key id") {
		t.Errorf("Verify of a signature with no key id that no listed key verifies answered %v, want ErrBadSignature saying it was signed with no key id", err)
	}
}

// A key id and a signature are read only in the one text Sign writes for them: the id 11
// characters, the signature 86, unpadded base64url with zero spare bits and no line break, which
// Go's decoder skips even in Strict mode. Anything else is a signature that does not decode, and
// no part of it reaches the error or an UnlistedKeyError, so no refusal suggests listing a key that
// no one has; an id in that form that no listed key has is named, saying no listed key has it.
// The id rows fail while isKeyID takes any text that decodes to 8 bytes, as at e481b26500, where a
// line break inside the operator's own id reaches UnlistedKeyError.KeyID; the signature rows while
// the signature part is decoded leniently, as at e481b26500, where a line break inside it, or a
// spare bit set, still verifies.
func TestAKeyIDAndASignatureAreReadOnlyInSignsForm(t *testing.T) {
	operator, operatorKey := newKey(t)
	stranger, _ := newKey(t)
	keys := []ed25519.PublicKey{operator}
	lines := signedrecord.HoldRow(3, "held", "hold-1", time.Unix(0, 0))
	kid, signature, _ := strings.Cut(sign(t, operatorKey, lines), ".")
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, signature[len(signature)-1])
	spareBit := signature[:len(signature)-1] + string(alphabet[last^1])
	for name, sig := range map[string]string{
		"a line break inside the key id":         kid[:5] + "\n" + kid[5:] + "." + signature,
		"a CR and LF inside the key id":          kid[:5] + "\r\n" + kid[5:] + "." + signature,
		"a key id of 12 characters":              kid + "A." + signature,
		"a line break inside the signature part": kid + "." + signature[:10] + "\n" + signature[10:],
		"a spare bit set in the signature part":  kid + "." + spareBit,
		"a line break in a signature with no id": signature[:10] + "\n" + signature[10:],
	} {
		err := signedrecord.Verify(keys, sig, lines)
		var unlisted *signedrecord.UnlistedKeyError
		if !errors.Is(err, signedrecord.ErrBadSignature) || errors.As(err, &unlisted) || !strings.Contains(fmt.Sprint(err), "does not decode") || strings.ContainsAny(fmt.Sprint(err), "\r\n") {
			t.Errorf("Verify of %s answered %q, want a signature that does not decode, naming no part of it", name, err)
		}
	}
	err := signedrecord.Verify([]ed25519.PublicKey{stranger}, kid+"."+signature, lines)
	if text := fmt.Sprint(err); !strings.Contains(text, "key id "+kid) || !strings.Contains(text, "no listed key has") {
		t.Errorf("Verify of an id no listed key has answered %q, want it to name the id and say no listed key has it", text)
	}
}

// A value is signed by its meaning, not its bytes: Postgres stores a record's value as jsonb, which
// reorders keys and drops whitespace, so the value read back verifies against the signature made
// over the value written.
func TestARecordValueVerifiesAfterJSONBRewritesIt(t *testing.T) {
	operator, operatorKey := newKey(t)
	written := signedrecord.Record("h", "cohort", "cohort", 1, []byte(`{ "b": [1, 2], "a": "x<y" }`))
	read := signedrecord.Record("h", "cohort", "cohort", 1, []byte(`{"a": "x<y", "b": [1, 2]}`))
	if err := signedrecord.Verify([]ed25519.PublicKey{operator}, sign(t, operatorKey, written), read); err != nil {
		t.Fatalf("the value as jsonb gives it back does not verify: %v", err)
	}
}

// A field holding a line break is refused when signed and when verified, so no two records can
// write the same lines.
func TestAFieldHoldingALineBreakIsRefused(t *testing.T) {
	operator, operatorKey := newKey(t)
	lines := signedrecord.HoldRow(3, "held", "hold\nreleased", time.Unix(0, 0))
	if _, err := signedrecord.Sign(operatorKey, lines); !errors.Is(err, signedrecord.ErrBadField) {
		t.Fatalf("Sign of a line break = %v, want ErrBadField", err)
	}
	if err := signedrecord.Verify([]ed25519.PublicKey{operator}, "c2ln", lines); !errors.Is(err, signedrecord.ErrBadField) {
		t.Fatalf("Verify of a line break = %v, want ErrBadField", err)
	}
}

// A record's digest line must be a SHA-256 digest in lowercase hex, which Record writes for a JSON
// value: lines whose digest is anything else, built by hand, are refused by Sign and Verify
// (ErrBadField), as a value that is not JSON is. Fails if the check reads a sentinel string at the
// digest's position: a hand-built digest of "abc" signs.
func TestARecordWhoseDigestIsNoSHA256IsRefused(t *testing.T) {
	operator, operatorKey := newKey(t)
	good := signedrecord.Record("h", "done", "hold", 1, []byte(`{}`))
	sig := sign(t, operatorKey, good)
	for name, digest := range map[string]string{
		"three letters":  "abc",
		"uppercase hex":  strings.ToUpper(good[5]),
		"one digit more": good[5] + "0",
		"empty":          "",
	} {
		lines := append([]string(nil), good...)
		lines[5] = digest
		if _, err := signedrecord.Sign(operatorKey, lines); !errors.Is(err, signedrecord.ErrBadField) {
			t.Errorf("Sign of a record whose digest is %s answered %v, want ErrBadField", name, err)
		}
		if err := signedrecord.Verify([]ed25519.PublicKey{operator}, sig, lines); !errors.Is(err, signedrecord.ErrBadField) {
			t.Errorf("Verify of a record whose digest is %s answered %v, want ErrBadField", name, err)
		}
	}
	if _, err := signedrecord.Sign(operatorKey, signedrecord.Record("h", "done", "hold", 1, []byte(`not json`))); !errors.Is(err, signedrecord.ErrBadField) {
		t.Errorf("Sign of a record whose value is not JSON answered %v, want ErrBadField", err)
	}
}

// Each kind's lines open with its own tag, so a signature over one kind's lines never verifies as
// another kind's.
func TestEachKindOpensWithItsTag(t *testing.T) {
	for want, lines := range map[string][]string{
		"nats-hold-v1":                 signedrecord.HoldRow(1, "released", "h", time.Unix(0, 0)),
		"nats-hold-record-v1":          signedrecord.Record("h", "done", "hold", 1, []byte(`{}`)),
		"nats-hold-record-delete-v1":   signedrecord.RecordDelete("h", "cohort", "cohort", 1),
		"nats-cutover-v1":              signedrecord.Cutover("session", "done", "h", 1),
		"envoy-kv-records-manifest-v1": signedrecord.RecordsManifest([]byte("{}\n")),
	} {
		if lines[0] != want {
			t.Errorf("lines open with %q, want %q", lines[0], want)
		}
	}
}

// A record's delete is signed over the record's hold id, kind, key and the gen it deletes, under
// its own tag: a delete signed over one gen does not verify over another, so an old signature
// cannot delete a newer record, and a record write's signature does not verify as a delete.
func TestARecordDeleteIsSignedOverTheGenItDeletes(t *testing.T) {
	operator, operatorKey := newKey(t)
	keys := []ed25519.PublicKey{operator}
	sig := sign(t, operatorKey, signedrecord.RecordDelete("h", "cohort", "cohort", 2))
	if err := signedrecord.Verify(keys, sig, signedrecord.RecordDelete("h", "cohort", "cohort", 2)); err != nil {
		t.Fatalf("the delete's own lines: %v", err)
	}
	for name, lines := range map[string][]string{
		"another gen":      signedrecord.RecordDelete("h", "cohort", "cohort", 3),
		"another key":      signedrecord.RecordDelete("h", "cohort", "other", 2),
		"another hold id":  signedrecord.RecordDelete("h2", "cohort", "cohort", 2),
		"a record's write": signedrecord.Record("h", "cohort", "cohort", 2, []byte(`{}`)),
	} {
		if err := signedrecord.Verify(keys, sig, lines); !errors.Is(err, signedrecord.ErrBadSignature) {
			t.Errorf("the delete's signature over %s answered %v, want ErrBadSignature", name, err)
		}
	}
	write := sign(t, operatorKey, signedrecord.Record("h", "cohort", "cohort", 2, []byte(`{}`)))
	if err := signedrecord.Verify(keys, write, signedrecord.RecordDelete("h", "cohort", "cohort", 2)); !errors.Is(err, signedrecord.ErrBadSignature) {
		t.Errorf("a record write's signature verified as a delete: %v", err)
	}
}

// A key list is read from its setting's text: entries parted by whitespace or commas, each the
// unpadded base64url of a 32-byte Ed25519 public key. An entry that is not is refused naming the
// setting and its place in the list and never its text, since text pasted there in error may be a
// private half.
func TestParsePublicKeysReadsTheSettingsList(t *testing.T) {
	first, _ := newKey(t)
	second, _ := newKey(t)
	encode := func(key ed25519.PublicKey) string { return base64.RawURLEncoding.EncodeToString(key) }
	keys, err := signedrecord.ParsePublicKeys("EXAMPLE_KEYS", " "+encode(first)+",\n"+encode(second)+" ")
	if err != nil || len(keys) != 2 || !keys[0].Equal(first) || !keys[1].Equal(second) {
		t.Fatalf("ParsePublicKeys of two keys = %v, %v", keys, err)
	}
	if keys, err := signedrecord.ParsePublicKeys("EXAMPLE_KEYS", "  "); err != nil || len(keys) != 0 {
		t.Fatalf("ParsePublicKeys of nothing = %v, %v; want no key and no error", keys, err)
	}
	secret := base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.PrivateKeySize))
	for raw, place := range map[string]string{
		encode(first) + " " + secret:                  "entry 2",
		base64.StdEncoding.EncodeToString(first):      "entry 1",
		encode(first) + "," + encode(second) + ",!!!": "entry 3",
	} {
		_, err := signedrecord.ParsePublicKeys("EXAMPLE_KEYS", raw)
		if err == nil || !strings.Contains(err.Error(), "EXAMPLE_KEYS "+place) || strings.Contains(err.Error(), secret) {
			t.Errorf("ParsePublicKeys(%q) = %v, want a refusal naming EXAMPLE_KEYS %s and not the entry", raw, err, place)
		}
	}
}

// A key listed twice is refused at load, naming both places in the list and the key's id, never
// the key's text: a list that holds a key twice was written in error, and its rotation steps read
// as if two keys were listed. Two different keys stay accepted whatever their ids, since Verify
// tries every listed key that has a signature's id. Fails while ParsePublicKeys keeps a duplicate,
// as at 886868daac.
func TestParsePublicKeysRefusesAKeyListedTwice(t *testing.T) {
	first, _ := newKey(t)
	second, _ := newKey(t)
	encode := func(key ed25519.PublicKey) string { return base64.RawURLEncoding.EncodeToString(key) }
	_, err := signedrecord.ParsePublicKeys("EXAMPLE_KEYS", encode(first)+","+encode(second)+" "+encode(first))
	if err == nil || !strings.Contains(err.Error(), "EXAMPLE_KEYS entries 1 and 3") || !strings.Contains(err.Error(), signedrecord.KeyID(first)) {
		t.Fatalf("ParsePublicKeys of a key listed twice answered %v, want a refusal naming entries 1 and 3 and key id %s", err, signedrecord.KeyID(first))
	}
	if strings.Contains(err.Error(), encode(first)) {
		t.Errorf("the refusal %q holds the key itself", err)
	}
	if keys, err := signedrecord.ParsePublicKeys("EXAMPLE_KEYS", encode(first)+" "+encode(second)); err != nil || len(keys) != 2 {
		t.Errorf("ParsePublicKeys of two different keys answered %v, %v; want both", keys, err)
	}
}

// A record value that is not JSON is refused ErrBadField, naming the byte where it stops being
// JSON and no character of it: the value is a record's, and json's message for a syntax error
// quotes the character it stopped at. Run by hand, the control is Canonical at bb2b623923, whose
// error ends `invalid character 'Q' looking for beginning of value`.
func TestAValueThatIsNotJSONIsRefusedNamingNoPartOfIt(t *testing.T) {
	_, err := signedrecord.Canonical([]byte(`{"a":Q7canary}`))
	if !errors.Is(err, signedrecord.ErrBadField) {
		t.Fatalf("Canonical of a value that is not JSON = %v, want ErrBadField", err)
	}
	if text := err.Error(); strings.Contains(text, "Q") || strings.Contains(text, "canary") || !strings.Contains(text, "byte 6") {
		t.Errorf("Canonical's error is %q: it holds part of the value, or names no byte", text)
	}
}
