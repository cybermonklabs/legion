package main

// loadgen oidc-mint: the token issuer of a cluster copy (cluster/cluster.sh), made at `up`.
//
// The copy's listener takes a send's role mark from Dispatch's verified principal alone
// (ENVOY_DISPATCH_PRINCIPAL), and verifies every identity token against an issuer it discovers
// (ENVOY_OIDC_ISSUER, internal/oidc). A listener that cannot read its issuer's discovery document
// takes no mark from Dispatch: an older one exits, and a newer one starts and answers that
// issuer's tokens 503 until it can read it. The copy's namespace lets its pods reach each other
// and DNS and nothing else, the cluster's own token issuer among the rest, so the copy carries an
// issuer of its own, inside the namespace. This command makes it: it generates one P-256 key,
// signs one token with it for each -sub (ES256, one of the two algorithms the listener's verifier
// takes), writes each token to the file its -sub names, and prints the key's public half.
// cluster.sh serves the public half inside the namespace (manifests/oidc-issuer.yaml) and gives
// each pod its token as a file, so the listener's verification is production's, against keys that
// never leave the namespace. The private key lives in this process alone: once it exits, the
// tokens it wrote are the only tokens that key ever verifies, so every subject a run needs is
// named in one call.

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// oidcMintKey is what oidc-mint prints, one JSON object: the signing key's id, its RFC 7638
// SHA-256 thumbprint, and its public point, under the members a JSON Web Key names them by, each
// 32 bytes in unpadded base64url. Nothing in it is a credential.
type oidcMintKey struct {
	KeyID string `json:"kid"`
	X     string `json:"x"`
	Y     string `json:"y"`
}

// oidcMintSubject is one -sub: the token's subject and the file the token is written to.
type oidcMintSubject struct {
	subject, file string
}

// oidcMintSubjects is the repeated -sub flag, <subject>=<file>, in the order given.
type oidcMintSubjects []oidcMintSubject

func (s *oidcMintSubjects) String() string { return "" }

// Set parses <subject>=<file>. A subject holds no "=", so the file is everything after the first
// one. The subject is a service account's, system:serviceaccount:<namespace>:<name>, the only shape
// the listener's ENVOY_OIDC_ACCOUNTS and ENVOY_DISPATCH_PRINCIPAL admit; a subject or a file given
// twice is refused, since the second token would replace the first.
func (s *oidcMintSubjects) Set(value string) error {
	subject, file, found := strings.Cut(value, "=")
	if !found || file == "" {
		return fmt.Errorf("%q is not <subject>=<file>", value)
	}
	rest, prefixed := strings.CutPrefix(subject, "system:serviceaccount:")
	namespace, name, cut := strings.Cut(rest, ":")
	if !prefixed || !cut || namespace == "" || name == "" || strings.Contains(name, ":") || strings.ContainsFunc(subject, func(r rune) bool { return r <= ' ' }) {
		return fmt.Errorf("subject %q is not system:serviceaccount:<namespace>:<name>", subject)
	}
	for _, given := range *s {
		if given.subject == subject {
			return fmt.Errorf("subject %s is given twice", subject)
		}
		if given.file == file {
			return fmt.Errorf("file %s is given twice", file)
		}
	}
	*s = append(*s, oidcMintSubject{subject: subject, file: file})
	return nil
}

// oidcMintOptions are the tokens' shared claims (iss and aud), their life from now, and one sub
// and file for each token.
type oidcMintOptions struct {
	issuer, audience string
	lifetime         time.Duration
	subjects         oidcMintSubjects
}

func oidcMintArgs(args []string) (oidcMintOptions, error) {
	fs := flag.NewFlagSet("oidc-mint", flag.ContinueOnError)
	issuer := fs.String("issuer", "", "the issuer URL every token names (iss), the listener's ENVOY_OIDC_ISSUER; its discovery document is served at <issuer>/.well-known/openid-configuration (required)")
	audience := fs.String("audience", "", "the audience every token names (aud), the listener's ENVOY_OIDC_AUDIENCE (required)")
	lifetime := fs.Duration("lifetime", 0, "how long each token lives from now (exp - iat) (required)")
	var subjects oidcMintSubjects
	fs.Var(&subjects, "sub", "system:serviceaccount:<namespace>:<name>=<file>: one token for that subject, written to that file (mode 600); repeatable, at least one")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: loadgen oidc-mint -issuer <url> -audience <aud> -lifetime <duration> -sub <subject>=<file> [-sub <subject>=<file>]...")
		fmt.Fprintln(fs.Output(), "Generates one P-256 key, writes one ES256 token of it for each -sub to that -sub's file, and prints the key's")
		fmt.Fprintln(fs.Output(), "public half, {\"kid\", \"x\", \"y\"}, which the issuer serves. The private key is never written.")
		fs.PrintDefaults()
	}
	if err := parseArgs(fs, args); err != nil {
		return oidcMintOptions{}, err
	}
	switch {
	case fs.NArg() > 0:
		return oidcMintOptions{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case *issuer == "":
		return oidcMintOptions{}, errors.New("-issuer is required: the listener's ENVOY_OIDC_ISSUER")
	case *audience == "":
		return oidcMintOptions{}, errors.New("-audience is required: the listener's ENVOY_OIDC_AUDIENCE")
	case *lifetime <= 0:
		return oidcMintOptions{}, fmt.Errorf("-lifetime must be a positive duration, not %s", *lifetime)
	case len(subjects) == 0:
		return oidcMintOptions{}, errors.New("-sub is required: <subject>=<file> for each token, at least one")
	}
	// The listener's verifier compares iss with ENVOY_OIDC_ISSUER byte for byte and reads the
	// discovery document under it, so an issuer that is no absolute http(s) URL of a host, or that
	// carries a query, a fragment or a trailing slash, is refused here rather than minted into a
	// token nothing verifies.
	parsed, err := url.Parse(*issuer)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil || strings.HasSuffix(parsed.Path, "/") {
		return oidcMintOptions{}, fmt.Errorf("-issuer %q is not an absolute http or https URL of a host with no query, fragment or trailing slash", *issuer)
	}
	return oidcMintOptions{issuer: *issuer, audience: *audience, lifetime: *lifetime, subjects: subjects}, nil
}

func runOIDCMint(args []string) error {
	return oidcMintTo(args, os.Stdout)
}

// oidcMintTo is oidc-mint with the key's public half printed to stdout.
func oidcMintTo(args []string, stdout io.Writer) error {
	opts, err := oidcMintArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	key, tokens, err := mintOIDC(opts, time.Now())
	if err != nil {
		return err
	}
	for i, s := range opts.subjects {
		if err := writeTokenFile(s.file, tokens[i]); err != nil {
			return err
		}
	}
	return json.NewEncoder(stdout).Encode(key)
}

// writeTokenFile replaces file with token in one rename, from a file of mode 600 beside it, so a
// reader sees no part of a token and the token is never readable by another user.
func writeTokenFile(file, token string) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+".*")
	if err != nil {
		return fmt.Errorf("writing the token for %s: %w", file, err)
	}
	_, err = tmp.WriteString(token)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), file)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing the token for %s: %w", file, err)
	}
	return nil
}

// mintOIDC generates the key and signs one token for each subject with it (mintOIDCWith).
func mintOIDC(opts oidcMintOptions, now time.Time) (oidcMintKey, []string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return oidcMintKey{}, nil, fmt.Errorf("generating the P-256 key: %w", err)
	}
	return mintOIDCWith(key, opts, now)
}

// mintOIDCWith signs one token with key for each subject, in opts.subjects' order, each issued at
// now, and answers key's public half. The command runs it once, on a key nothing else holds; a
// test holds the key, so it can sign one more token under the key an issuer it starts serves.
func mintOIDCWith(key *ecdsa.PrivateKey, opts oidcMintOptions, now time.Time) (oidcMintKey, []string, error) {
	public := jose.JSONWebKey{Key: &key.PublicKey, Algorithm: string(jose.ES256), Use: "sig"}
	thumbprint, err := public.Thumbprint(crypto.SHA256)
	if err != nil {
		return oidcMintKey{}, nil, fmt.Errorf("the key's thumbprint: %w", err)
	}
	var minted oidcMintKey
	// The point as go-jose writes it into a key set, which is what the listener's verifier parses
	// it back from (internal/oidc keys.go).
	encoded, err := json.Marshal(public)
	if err != nil {
		return oidcMintKey{}, nil, fmt.Errorf("encoding the public key: %w", err)
	}
	if err := json.Unmarshal(encoded, &minted); err != nil {
		return oidcMintKey{}, nil, fmt.Errorf("reading the public key's point: %w", err)
	}
	minted.KeyID = base64.RawURLEncoding.EncodeToString(thumbprint)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: key, KeyID: minted.KeyID}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return oidcMintKey{}, nil, fmt.Errorf("the tokens' signer: %w", err)
	}
	tokens := make([]string, 0, len(opts.subjects))
	for _, s := range opts.subjects {
		payload, err := json.Marshal(map[string]any{
			"iss": opts.issuer,
			"sub": s.subject,
			"aud": opts.audience,
			"iat": now.Unix(),
			"exp": now.Add(opts.lifetime).Unix(),
		})
		if err != nil {
			return oidcMintKey{}, nil, fmt.Errorf("encoding the claims of %s's token: %w", s.subject, err)
		}
		signed, err := signer.Sign(payload)
		if err != nil {
			return oidcMintKey{}, nil, fmt.Errorf("signing %s's token: %w", s.subject, err)
		}
		token, err := signed.CompactSerialize()
		if err != nil {
			return oidcMintKey{}, nil, fmt.Errorf("serializing %s's token: %w", s.subject, err)
		}
		tokens = append(tokens, token)
	}
	return minted, tokens, nil
}
