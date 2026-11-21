package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
)

// The authenticator data flags a virtual authenticator sets (WebAuthn Level 3 §6.1): the user was
// present, the user was verified, and, at an enrollment, the data carries the new credential.
const (
	flagUserPresent    = 0x01
	flagUserVerified   = 0x04
	flagAttestedCredID = 0x40
)

// virtualAuthenticator stands in for a person's device: one ES256 passkey of the relying party
// rpID, which answers the ceremonies at origin as a browser does, with its signature counter
// count, which each assertion advances by one unless a row sets it. It makes no attestation
// (format "none"), as the relying party asks.
type virtualAuthenticator struct {
	key    *ecdsa.PrivateKey
	id     []byte
	handle []byte
	count  uint32
	rpID   string
	origin string
	// verified is whether the authenticator says it verified the user; false is a passkey used
	// without user verification, which the relying party refuses.
	verified bool
}

func newVirtualAuthenticator(t *testing.T, rpID, origin string) *virtualAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return &virtualAuthenticator{key: key, id: id, rpID: rpID, origin: origin, verified: true}
}

// ceremonyOptions is what a begin answers, as much as an authenticator reads of it.
type ceremonyOptions struct {
	PublicKey struct {
		Challenge string `json:"challenge"`
		RPID      string `json:"rpId"`
		RP        struct {
			ID string `json:"id"`
		} `json:"rp"`
		User struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"user"`
		UserVerification       string `json:"userVerification"`
		Attestation            string `json:"attestation"`
		AuthenticatorSelection struct {
			ResidentKey      string `json:"residentKey"`
			UserVerification string `json:"userVerification"`
		} `json:"authenticatorSelection"`
		AllowCredentials []json.RawMessage `json:"allowCredentials"`
	} `json:"publicKey"`
}

func readCeremonyOptions(t *testing.T, body []byte) ceremonyOptions {
	t.Helper()
	var options ceremonyOptions
	if err := json.Unmarshal(body, &options); err != nil || options.PublicKey.Challenge == "" {
		t.Fatalf("the begin answered %s, want ceremony options with a challenge (%v)", body, err)
	}
	return options
}

// clientData is the collected client data of a ceremony of type kind ("webauthn.create" or
// "webauthn.get") for challenge, at the authenticator's origin.
func (a *virtualAuthenticator) clientData(t *testing.T, kind, challenge string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"type": kind, "challenge": challenge, "origin": a.origin, "crossOrigin": false})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// authData is the authenticator data of a ceremony with flags and the counter, then attested.
func (a *virtualAuthenticator) authData(flags byte, attested []byte) []byte {
	rpIDHash := sha256.Sum256([]byte(a.rpID))
	data := append([]byte{}, rpIDHash[:]...)
	if a.verified {
		flags |= flagUserVerified
	}
	data = append(data, flags)
	data = binary.BigEndian.AppendUint32(data, a.count)
	return append(data, attested...)
}

// publicKey is the passkey's public key as COSE writes an ES256 key on P-256.
func (a *virtualAuthenticator) publicKey(t *testing.T) []byte {
	t.Helper()
	data, err := webauthncbor.Marshal(map[int64]any{
		1: int64(2), 3: int64(-7), -1: int64(1),
		-2: a.key.X.FillBytes(make([]byte, 32)),
		-3: a.key.Y.FillBytes(make([]byte, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// register answers an enrollment's options, beginBody: the PublicKeyCredential
// navigator.credentials.create would answer, as JSON. The passkey keeps the user handle the
// options name.
func (a *virtualAuthenticator) register(t *testing.T, beginBody []byte) []byte {
	t.Helper()
	options := readCeremonyOptions(t, beginBody)
	handle, err := base64.RawURLEncoding.DecodeString(options.PublicKey.User.ID)
	if err != nil {
		t.Fatalf("the enrollment's user id %q is not base64url: %v", options.PublicKey.User.ID, err)
	}
	a.handle = handle
	attested := make([]byte, 16)
	attested = binary.BigEndian.AppendUint16(attested, uint16(len(a.id)))
	attested = append(attested, a.id...)
	attested = append(attested, a.publicKey(t)...)
	attestation, err := webauthncbor.Marshal(map[string]any{
		"fmt": "none", "attStmt": map[string]any{}, "authData": a.authData(flagUserPresent|flagAttestedCredID, attested),
	})
	if err != nil {
		t.Fatal(err)
	}
	id := base64.RawURLEncoding.EncodeToString(a.id)
	body, err := json.Marshal(map[string]any{
		"id": id, "rawId": id, "type": "public-key", "clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"attestationObject": base64.RawURLEncoding.EncodeToString(attestation),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(a.clientData(t, "webauthn.create", options.PublicKey.Challenge)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// assert answers a sign-in's options, beginBody, with the passkey at its next signature counter:
// the PublicKeyCredential navigator.credentials.get would answer, as JSON.
func (a *virtualAuthenticator) assert(t *testing.T, beginBody []byte) []byte {
	t.Helper()
	a.count++
	return a.assertAt(t, beginBody)
}

// assertAt is assert at the counter the authenticator holds now, unmoved.
func (a *virtualAuthenticator) assertAt(t *testing.T, beginBody []byte) []byte {
	t.Helper()
	options := readCeremonyOptions(t, beginBody)
	authData := a.authData(flagUserPresent, nil)
	clientData := a.clientData(t, "webauthn.get", options.PublicKey.Challenge)
	clientDataHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), clientDataHash[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	id := base64.RawURLEncoding.EncodeToString(a.id)
	body, err := json.Marshal(map[string]any{
		"id": id, "rawId": id, "type": "public-key", "clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"authenticatorData": base64.RawURLEncoding.EncodeToString(authData),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
			"signature":         base64.RawURLEncoding.EncodeToString(signature),
			"userHandle":        base64.RawURLEncoding.EncodeToString(a.handle),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}
