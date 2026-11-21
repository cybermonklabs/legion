package signedrecord_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
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
// genuine older record read after a newer gen was accepted, and a value changed after signing.
// A genuine record verifies, so the refusals are the verifier's and not a reader that refuses
// everything.
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

	older := signedrecord.Record("hold-1", "done", "hold", 1, value)
	olderSig := sign(t, operatorKey, older)
	floor := &signedrecord.Floor{}
	if err := floor.Accept(2); err != nil {
		t.Fatalf("the floor refuses the first gen it reads: %v", err)
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
				return floor.Accept(1)
			},
			want: signedrecord.ErrReplay,
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

// Each kind's lines open with its own tag, so a signature over one kind's lines never verifies as
// another kind's.
func TestEachKindOpensWithItsTag(t *testing.T) {
	for want, lines := range map[string][]string{
		"nats-hold-v1":               signedrecord.HoldRow(1, "released", "h", time.Unix(0, 0)),
		"nats-hold-record-v1":        signedrecord.Record("h", "done", "hold", 1, []byte(`{}`)),
		"nats-hold-record-delete-v1": signedrecord.RecordDelete("h", "cohort", "cohort", 1),
		"nats-cutover-v1":            signedrecord.Cutover("session", "done", "h", 1),
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
