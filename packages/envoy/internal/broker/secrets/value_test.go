package secrets_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// unsignableAppKey is a 512-bit RSA key in PEM, made at run time from two fresh 256-bit primes:
// x509 parses it, and crypto/rsa refuses to sign with it, since Go refuses every RSA key shorter
// than 1024 bits (crypto/rsa's minimum key size). It is the stored key a GitHub App value can
// hold that parses and still cannot sign. No fixture writes the key out: rsa.GenerateKey refuses
// that size, and a key written into the source is a key the push guard refuses.
func unsignableAppKey(t *testing.T) string {
	t.Helper()
	one := big.NewInt(1)
	for {
		p, err := rand.Prime(rand.Reader, 256)
		if err != nil {
			t.Fatal(err)
		}
		q, err := rand.Prime(rand.Reader, 256)
		if err != nil {
			t.Fatal(err)
		}
		phi := new(big.Int).Mul(new(big.Int).Sub(p, one), new(big.Int).Sub(q, one))
		d := new(big.Int).ModInverse(big.NewInt(65537), phi)
		if p.Cmp(q) == 0 || d == nil {
			continue
		}
		key := &rsa.PrivateKey{PublicKey: rsa.PublicKey{N: new(big.Int).Mul(p, q), E: 65537}, D: d, Primes: []*big.Int{p, q}}
		key.Precompute()
		return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	}
}

// leak is a word every refused value below carries where it can, beside the key material: no
// refusal may quote it.
const leak = "s3cr3t"

// refusedValue is one value a kind's minter refuses, and what the refusal must say.
type refusedValue struct {
	kind, name, value, says string
}

// refusedValues are, for each kind, the values its minter refuses: for github-app a value that is
// not a JSON object, no app_id, an app_id that is not digits, no private_key, a key that does not
// parse and one that cannot sign.
func refusedValues(t *testing.T) []refusedValue {
	t.Helper()
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	app := func(fields map[string]string) string {
		encoded, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	return []refusedValue{
		{"github-app", "not a JSON object", leak + " is no JSON", "decode GitHub App secret: not a JSON object of its fields"},
		{"github-app", "no app_id", app(map[string]string{"private_key": pemKey, "note": leak}), "GitHub App secret needs an app_id and private_key"},
		{"github-app", "an app_id that is not digits", app(map[string]string{"app_id": "12" + leak, "private_key": pemKey}), "GitHub App secret needs an app_id and private_key"},
		{"github-app", "no private_key", app(map[string]string{"app_id": "123456", "note": leak}), "GitHub App secret needs an app_id and private_key"},
		{"github-app", "a key that does not parse", app(map[string]string{"app_id": "123456", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("no key")}))}), "parse GitHub App private key: not a PKCS1 or PKCS8 private key"},
		{"github-app", "a key with no PEM block", app(map[string]string{"app_id": "123456", "private_key": leak}), "parse GitHub App private key: no PEM block found"},
		{"github-app", "a key that cannot sign", app(map[string]string{"app_id": "123456", "private_key": unsignableAppKey(t)}), "sign GitHub App JWT: "},
		{"google-login", "not a JSON object", leak, "decode Google login secret: not a JSON object of its fields"},
		{"google-login", "not an authorized_user credential", `{"type":"service_account","client_id":"client-id","client_secret":"` + leak + `","refresh_token":"refresh-not-a-secret"}`, "Google login secret must be an authorized_user credential"},
		{"google-login", "no refresh_token", `{"type":"authorized_user","client_id":"client-id","client_secret":"` + leak + `"}`, "Google login secret must be an authorized_user credential"},
		{"google-service-account", "not a JSON object", leak, "decode Google service account secret: not a JSON object of its fields"},
		{"google-service-account", "a person's address", `{"service_account":"` + leak + `@theorem.dev"}`, "Google service account secret must name one <id>@<project>.iam.gserviceaccount.com account in service_account"},
		{"azure-login", "not a JSON object", leak, "Azure login secret must name tenant_id and client_id"},
		{"azure-login", "no client_id", `{"tenant_id":"` + leak + `"}`, "Azure login secret must name tenant_id and client_id"},
		{"azure-login", "a tenant_id of white space", `{"tenant_id":" ","client_id":"` + leak + `"}`, "Azure login secret must name tenant_id and client_id"},
	}
}

// mint is one minter's Mint.
type mint func(context.Context, secrets.MintRequest) (secrets.Minted, error)

// minterOf is the minter of kind as the broker holds it, set up so that the stored value is the
// first thing it reads: the Azure login minter is configured for the secret the request names, at
// human tier, since that gate comes before the value.
func minterOf(t *testing.T, kind string) (mint, secrets.MintRequest) {
	t.Helper()
	switch kind {
	case "github-app":
		return secrets.GitHubApp{}.Mint, secrets.MintRequest{Name: "APP_KEY", GitHubOwners: []string{"acme"}}
	case "google-login":
		return secrets.GoogleLogin{}.Mint, secrets.MintRequest{Name: "LOGIN_KEY"}
	case "google-service-account":
		return (&secrets.GoogleServiceAccount{}).Mint, secrets.MintRequest{Name: "ACCOUNT_KEY"}
	case "azure-login":
		return secrets.AzureLogin{SecretName: "AZURE_LOGIN"}.Mint, secrets.MintRequest{Name: "AZURE_LOGIN", HumanTier: true}
	}
	t.Fatalf("no minter for kind %q", kind)
	return nil, secrets.MintRequest{}
}

// TestParseValueRefusesWhatEachMinterRefusesInItsWords: ParseValue refuses each value its kind's
// minter refuses, wrapping ErrMintStoredValue, naming what is wrong and quoting no byte of the
// value; the minter, driven with the same value, answers the same words, because the decoder is
// the only reader of a stored value.
func TestParseValueRefusesWhatEachMinterRefusesInItsWords(t *testing.T) {
	for _, row := range refusedValues(t) {
		t.Run(row.kind+"/"+row.name, func(t *testing.T) {
			_, err := secrets.ParseValue(row.kind, row.value)
			if !errors.Is(err, secrets.ErrMintStoredValue) || !strings.Contains(err.Error(), row.says) {
				t.Fatalf("ParseValue(%s) = %v; want ErrMintStoredValue saying %q", row.kind, err, row.says)
			}
			if strings.Contains(err.Error(), leak) || strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), "AAAA") {
				t.Fatalf("ParseValue(%s) = %q quotes the value", row.kind, err)
			}
			minter, req := minterOf(t, row.kind)
			req.Value = row.value
			minted, mintErr := minter(t.Context(), req)
			if mintErr == nil || minted.Value != "" {
				t.Fatalf("%s minter minted %q from a value ParseValue refuses", row.kind, minted.Value)
			}
			if mintErr.Error() != err.Error() || secrets.MintErrorClass(mintErr) != "stored-value" {
				t.Fatalf("%s minter = %q (class %s); want ParseValue's own %q, class stored-value", row.kind, mintErr, secrets.MintErrorClass(mintErr), err)
			}
		})
	}
}

// TestParseValueDecodesEachKindsFields: a value each kind's minter mints from decodes into that
// kind's fields, and only them.
func TestParseValueDecodesEachKindsFields(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		kind, value string
		want        func(secrets.Value) bool
	}{
		{"github-app", githubAppValue(t, "123456", key), func(v secrets.Value) bool {
			return v.GitHubApp.AppID == "123456" && v.GitHubApp.Key != nil && v.GitHubApp.Key.Equal(key) && v.GoogleLogin == (secrets.GoogleLoginValue{})
		}},
		{"google-login", `{"type":"authorized_user","client_id":"client-id","client_secret":"client-not-a-secret","refresh_token":"refresh-not-a-secret"}`, func(v secrets.Value) bool {
			return v.GoogleLogin == secrets.GoogleLoginValue{ClientID: "client-id", ClientSecret: "client-not-a-secret", RefreshToken: "refresh-not-a-secret"} && v.GitHubApp.Key == nil
		}},
		{"google-service-account", `{"service_account":"robot-1@acme-agent-infra.iam.gserviceaccount.com"}`, func(v secrets.Value) bool {
			return v.GoogleServiceAccount.ServiceAccount == "robot-1@acme-agent-infra.iam.gserviceaccount.com" && v.AzureLogin == (secrets.AzureLoginValue{})
		}},
		{"azure-login", azureLoginValue, func(v secrets.Value) bool {
			return v.AzureLogin == secrets.AzureLoginValue{TenantID: "tenant-id", ClientID: "client-id"} && v.GoogleServiceAccount == (secrets.GoogleServiceAccountValue{})
		}},
	} {
		got, err := secrets.ParseValue(row.kind, row.value)
		if err != nil || !row.want(got) {
			t.Fatalf("ParseValue(%s) = %+v, %v; want that kind's fields alone", row.kind, got, err)
		}
	}
}

// TestParseValueRefusesAKindNoMinterReads: the decoder knows the four kinds and no other, so a
// kind tag naming any other, or none, is refused as a value no minter mints from.
func TestParseValueRefusesAKindNoMinterReads(t *testing.T) {
	for _, kind := range []string{"", "other", "GitHub-App"} {
		if _, err := secrets.ParseValue(kind, azureLoginValue); !errors.Is(err, secrets.ErrMintStoredValue) || !strings.Contains(err.Error(), "is no kind a minter reads") {
			t.Fatalf("ParseValue(%q) = %v; want a refusal naming no kind a minter reads", kind, err)
		}
	}
}

// TestParseValueLeavesTheOtherChecksInTheirPlace: the decoder replaces only the minters' reading
// of the value. The Azure minter's gate on the configured human-tier secret still comes first,
// so a release it refuses is refused whatever the value holds; the GitHub minter's narrowing
// check now comes after the value, so a bad value under a bad narrowing is the stored value's.
func TestParseValueLeavesTheOtherChecksInTheirPlace(t *testing.T) {
	_, err := (secrets.AzureLogin{SecretName: "AZURE_LOGIN"}).Mint(t.Context(), secrets.MintRequest{Name: "OTHER", HumanTier: true, Value: leak})
	if got := secrets.MintErrorClass(err); got != "refused" {
		t.Fatalf("Azure mint of another secret with a bad value = %v (class %s); want refused", err, got)
	}
	narrowed := secrets.MintRequest{Value: `{"app_id":"123456"}`, GitHubOwners: []string{"acme"}, GitHubNarrowing: &secrets.GitHubNarrowing{Owner: "outsider"}}
	_, err = (secrets.GitHubApp{}).Mint(t.Context(), narrowed)
	if got := secrets.MintErrorClass(err); got != "stored-value" {
		t.Fatalf("GitHub mint of a bad value under an unlisted owner = %v (class %s); want stored-value", err, got)
	}
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	narrowed.Value = githubAppValue(t, "123456", key)
	if _, err := (secrets.GitHubApp{}).Mint(t.Context(), narrowed); !errors.Is(err, secrets.ErrMintNarrowingRefused) {
		t.Fatalf("GitHub mint of a good value under an unlisted owner = %v; want ErrMintNarrowingRefused", err)
	}
}
