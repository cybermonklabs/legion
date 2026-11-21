package natsgrant_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/natsgrant"
)

var updateSessionFixture = flag.Bool("update", false, "rewrite packages/contracts/fixtures/session-grant.json from natsgrant.Session")

// sessionFixturePath is the session grant as the auth service answers it, for the placeholder id
// sessionFixtureID, read by relative path as the other shared fixtures of this repository are.
// Its reader is packages/envoy-client/test/nats-server.ts (sessionGrantConfig), which renders it
// with each Bun row's own session id for the users of the NATS servers those rows start: the
// consumer-delivery row of envoy-client and the replay rows of pi-envoy and claude-envoy. The
// file is the encoder's output byte for byte (renderSessionFixture), so biome.json's formatter
// excludes it, as it does the other generated JSON files.
const sessionFixturePath = "../../contracts/fixtures/session-grant.json"

// sessionFixtureID is the id the fixture writes the grant for. A reader swaps it for its own
// session's id token by token, so it is one subject token and appears in no other token.
const sessionFixtureID = "ses_fixture_placeholder"

// sessionFixture is the fixture's shape: the id it was written for, then the grant.
type sessionFixture struct {
	SessionID string   `json:"session_id"`
	Publish   []string `json:"publish"`
	Subscribe []string `json:"subscribe"`
}

// renderSessionFixture is the fixture's bytes: Go's encoder indenting by two spaces, with no HTML
// escaping, so `>` stays as the grant writes it rather than \u003e.
func renderSessionFixture(t *testing.T, fixture sessionFixture) []byte {
	t.Helper()
	var written bytes.Buffer
	encoder := json.NewEncoder(&written)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	require.NoError(t, encoder.Encode(fixture))
	return written.Bytes()
}

// The committed fixture is natsgrant.Session of its placeholder id, element for element, so the Bun
// rows that start a server from it hold a session to the grant the auth service gives one, and it
// is the bytes renderSessionFixture writes. A change to the grant, or a hand edit of the file, is
// red here until the fixture is written again: go test -run
// TestTheSessionGrantFixtureIsNatsgrantSession ./natsgrant/ -update.
func TestTheSessionGrantFixtureIsNatsgrantSession(t *testing.T) {
	grant, err := natsgrant.Session(sessionFixtureID)
	require.NoError(t, err)
	want := sessionFixture{SessionID: sessionFixtureID, Publish: grant.Publish, Subscribe: grant.Subscribe}
	rendered := renderSessionFixture(t, want)
	if *updateSessionFixture {
		require.NoError(t, os.WriteFile(sessionFixturePath, rendered, 0o644))
	}
	var committed sessionFixture
	readJSON(t, sessionFixturePath, &committed)
	require.NotEmpty(t, committed.Publish, "%s holds no publish list, so it says nothing about the grant", sessionFixturePath)
	const rewrite = "write it again with `go test -run TestTheSessionGrantFixtureIsNatsgrantSession ./natsgrant/ -update`"
	require.Equal(t, want, committed,
		"%s is not natsgrant.Session(%q): "+rewrite+", and run the Bun rows that read it (packages/envoy-client/test/nats-server.ts, sessionGrantConfig)",
		sessionFixturePath, sessionFixtureID)
	raw, err := os.ReadFile(sessionFixturePath)
	require.NoError(t, err)
	require.Equal(t, string(rendered), string(raw),
		"%s holds the grant but not the bytes the -update run writes: "+rewrite, sessionFixturePath)
}
