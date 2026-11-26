package testnats

import (
	"testing"

	"github.com/nats-io/nkeys"
)

// User is a fresh nkey user's seed and public key, the public key being what StartNkeyAuthorized
// and StartNkeyGranted take.
func User(t testing.TB) (seed, public string) {
	t.Helper()
	user, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("create nkey user: %v", err)
	}
	raw, err := user.Seed()
	if err != nil {
		t.Fatalf("user seed: %v", err)
	}
	if public, err = user.PublicKey(); err != nil {
		t.Fatalf("user public key: %v", err)
	}
	return string(raw), public
}

// seedVariable is the variable a NATS connect reads its nkey seed from, bus.NkeySeedEnvVar: this
// package cannot import bus, whose own tests import it, and a test in package bus holds the two
// names equal (TestTheTestsSeedLookupAnswersTheBusSeedVariable).
const seedVariable = "NATS_NKEY_SEED"

// SeedOnly is a connect's environment (bus.WithEnvironment, bus.Dial) that holds seed as the nkey
// seed and nothing else, as a process whose environment held that one variable would: no seed
// file, no remote server allowed, no proxy. An empty seed is no seed at all, as an unset variable
// is (a variable set but empty, the bus refuses). A test that connects as an nkey user (User)
// passes it rather than set the variable with t.Setenv, which changes the whole process and so
// keeps the test from running in parallel.
func SeedOnly(seed string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if name == seedVariable && seed != "" {
			return seed, true
		}
		return "", false
	}
}
