// packages/envoy/internal/cipherclient/environment_test.go
//
// Which key a machine trade proves: a pod role's own agent-secrets key, and a fresh one of this
// process's own everywhere else.

package cipherclient

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// TestMachineKeyIsTheRolesKey: a role in a pod, where AGENT_SECRETS_MACHINE_IDENTITY is kubernetes
// and AGENT_SECRETS_KEY_DIR names its agent-secrets directory, trades with the key.pem `agent-secrets
// keygen` wrote there, so the trade's DPoP proof embeds that key, whose registration the broker
// holds the pod session's exchange to. A key.pem that cannot be read, or holds no key, refuses the
// trade, naming the directory by AGENT_SECRETS_KEY_DIR and never its path, which can be a token a
// person pasted there, and no fresh key stands in for it. Every other machine proves a fresh key of
// its own.
func TestMachineKeyIsTheRolesKey(t *testing.T) {
	dir := t.TempDir()
	roleKey := testKey(t)
	writeRoleKey(t, dir, roleKey)
	pod := map[string]string{IdentityVar: IdentityKubernetes, KeyDirVar: dir}

	t.Run("a pod role trades with its own agent-secrets key", func(t *testing.T) {
		key, err := MachineKey(getenvOf(pod))
		if err != nil {
			t.Fatalf("MachineKey for a pod role with %s: %v", filepath.Join(dir, RoleKeyFile), err)
		}
		if got, want := tradeThumbprint(t, key), keyThumbprint(t, roleKey); got != want {
			t.Fatalf("the trade's DPoP proof embeds the key of thumbprint %s, want the role's own key.pem's %s", got, want)
		}
	})

	t.Run("a role key that cannot be read or holds no key refuses the trade, naming the variable and not the path", func(t *testing.T) {
		const pasted = "madeUpKeyDirToken0123456789abcdef"
		root := t.TempDir()
		notPEM := filepath.Join(root, "not-pem-"+pasted)
		if err := os.Mkdir(notPEM, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(notPEM, RoleKeyFile), []byte("no key here\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, row := range []struct{ name, dir, want string }{
			{"a directory that does not exist, a token pasted in its place", filepath.Join(root, pasted),
				"read this role's agent-secrets key in the directory AGENT_SECRETS_KEY_DIR names: open: no such file or directory"},
			{"a key.pem that holds no PEM block", notPEM,
				"this role's agent-secrets key in the directory AGENT_SECRETS_KEY_DIR names holds no PEM block"},
		} {
			key, err := MachineKey(getenvOf(map[string]string{IdentityVar: IdentityKubernetes, KeyDirVar: row.dir}))
			if err == nil || err.Error() != row.want {
				t.Fatalf("%s: MachineKey = %v, want exactly %q", row.name, err, row.want)
			}
			for i := 0; i+10 <= len(pasted); i++ {
				if strings.Contains(err.Error(), pasted[i:i+10]) {
					t.Fatalf("%s: MachineKey = %q, which holds %q of the directory's path", row.name, err, pasted[i:i+10])
				}
			}
			if key != nil {
				t.Fatalf("%s: MachineKey answered a key beside its refusal: a fresh key stands in for the role's", row.name)
			}
		}
	})

	// Every other case is as it was: a key of this process's own, never the one in the directory.
	for name, env := range map[string]map[string]string{
		"a kubernetes identity with no key directory": {IdentityVar: IdentityKubernetes},
		"another identity beside a key directory":     {IdentityVar: IdentityMetadata, KeyDirVar: dir},
		"no identity beside a key directory":          {KeyDirVar: dir},
	} {
		key, err := MachineKey(getenvOf(env))
		if err != nil {
			t.Fatalf("MachineKey for %s: %v", name, err)
		}
		if keyThumbprint(t, key) == keyThumbprint(t, roleKey) {
			t.Errorf("MachineKey for %s answered the key in %s, want a fresh key of this process's own", name, dir)
		}
	}
}

// writeRoleKey writes key into dir as `agent-secrets keygen` writes key.pem: PKCS#8 PEM, 0600.
func writeRoleKey(t *testing.T, dir string, key *ecdsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, RoleKeyFile), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// getenvOf is a getenv over env alone.
func getenvOf(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

// tradeThumbprint trades once at a stub broker proving key and answers the RFC 7638 thumbprint of
// the key the trade's DPoP proof embeds.
func tradeThumbprint(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": "role-token-not-a-secret", "token_type": "DPoP", "expires_in": 600}
	})
	client, err := New(Config{
		BrokerURL: broker.Address(), Key: key,
		Subject: &scriptedSource{token: "projected.role.token", tokenType: oauthwire.SubjectTokenTypeKubernetes, describe: "the role's projected token"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	requests := broker.Requests()
	if len(requests) != 1 {
		t.Fatalf("the broker saw %d trades, want one", len(requests))
	}
	raw, err := json.Marshal(requests[0].ProofHeader["jwk"])
	if err != nil {
		t.Fatal(err)
	}
	var embedded jose.JSONWebKey
	if err := embedded.UnmarshalJSON(raw); err != nil {
		t.Fatalf("the trade's DPoP proof embeds no key that parses (%s): %v", raw, err)
	}
	return jwkThumbprint(t, embedded)
}

// keyThumbprint is key's RFC 7638 thumbprint.
func keyThumbprint(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	return jwkThumbprint(t, jose.JSONWebKey{Key: key.Public()})
}

// jwkThumbprint is jwk's RFC 7638 thumbprint, base64url with no padding as the broker records it.
func jwkThumbprint(t *testing.T, jwk jose.JSONWebKey) string {
	t.Helper()
	sum, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(sum)
}
