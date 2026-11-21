// Package servicecallout starts, for a test, a NATS server whose real auth service
// (internal/cipher/natscallout) admits services by their platform tokens, which it reads unsigned:
// so a client under test connects as a service does in production, with the grant the auth service
// answers for its kind, and asks under its own reply prefix. It is a package of its own because
// natscallout's own tests import testnats, so testnats cannot import natscallout.
package servicecallout

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/testnats"
)

// Start starts a server with JetStream whose auth service admits each entry of services
// (natscallout.ParseServices' form), with static beside it as users of the server's own, which the
// auth service is not asked about, and returns its client URL once the service answers. Both stop
// when the test ends.
func Start(t testing.TB, services string, static ...testnats.StaticUser) string {
	t.Helper()
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys, static...)
	dir := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	entries, err := natscallout.ParseServices(services)
	if err != nil {
		t.Fatal(err)
	}
	service, err := natscallout.Start(context.Background(), natscallout.Settings{
		URLs: []string{url}, SeedFile: write("service.seed", keys.ServiceSeed),
		IssuerSeedFile: write("issuer.seed", keys.IssuerSeed), XKeySeedFile: write("xkey.seed", keys.XKeySeed), Services: entries,
	}, natscallout.Deps{ServiceToken: unsigned, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ready, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := service.Ready(ready); err != nil {
		t.Fatalf("the auth service never answered: %v", err)
	}
	return url
}

// Token is an unsigned platform token naming account, issued now and lapsing after life, which
// Start's auth service takes as verified.
func Token(account string, life time.Duration) string {
	encode := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	now := time.Now()
	return encode(map[string]string{"alg": "none"}) + "." +
		encode(map[string]any{"sub": account, "iat": now.Unix(), "exp": now.Add(life).Unix()}) + ".unsigned"
}

// TokenFile writes Token(account, life) to a file of the test's own and returns its path, the
// value a client's NATS_SERVICE_TOKEN_FILE names.
func TokenFile(t testing.TB, account string, life time.Duration) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "service-token")
	if err := os.WriteFile(path, []byte(Token(account, life)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// unsigned is a natscallout.Deps.ServiceToken that reads a token's claims without verifying it.
func unsigned(_ context.Context, raw string) (string, bool, time.Time, time.Time, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", false, time.Time{}, time.Time{}, errors.New("not a token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false, time.Time{}, time.Time{}, err
	}
	var claims struct {
		Sub      string
		Iat, Exp int64
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false, time.Time{}, time.Time{}, err
	}
	return claims.Sub, false, time.Unix(claims.Iat, 0), time.Unix(claims.Exp, 0), nil
}
