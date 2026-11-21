package streams_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/streams"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/streamset"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// serviceHub starts a lone server whose user service, with token as its password, holds grant, as
// the auth service answers the identity token presents, and an admin every credential-less
// connection is.
func serviceHub(t *testing.T, token string, grant natsgrant.Permissions) (string, natsgo.JetStreamContext) {
	t.Helper()
	quote := func(subjects []string) string {
		quoted := make([]string, len(subjects))
		for index, subject := range subjects {
			quoted[index] = fmt.Sprintf("%q", subject)
		}
		return strings.Join(quoted, ", ")
	}
	config := fmt.Sprintf(`jetstream {}
authorization {
  users = [
    { user: "admin", password: "admin" },
    { user: %q, password: %q, permissions: { publish: { allow: [%s] }, subscribe: { allow: [%s] } } }
  ]
}
no_auth_user: "admin"
`, natscred.ServiceUser, token, quote(grant.Publish), quote(grant.Subscribe))
	_, url := testnats.StartConfig(t, config, []string{"-js"}, true)
	admin := testnats.Connect(t, url)
	t.Cleanup(admin.Close)
	js, err := admin.JetStream()
	require.NoError(t, err)
	return url, js
}

// holdBroker is a broker's /v1/nats-hold read route answering one hold row of phase, signed with
// key.
func holdBroker(t *testing.T, key ed25519.PrivateKey, phase natsgrant.OperatorPhase) string {
	t.Helper()
	signedAt := time.Now().UTC()
	rowSig, err := signedrecord.Sign(key, signedrecord.HoldRow(7, string(phase), "hold-a", signedAt))
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/nats-hold":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"gen": 7, "phase": string(phase), "hold_id": "hold-a", "reason": "test",
				"updated_at": signedAt.Format(time.RFC3339Nano), "updated_by": "operator",
				"signed_at": signedAt.Format(time.RFC3339Nano), "sig": rowSig,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := streams.Command(t.Context(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// The command's words: a usage it cannot run is exit 2 naming what is missing, and update and
// census, the writer hold's held commands, are words this command does not take; check, as the
// measurement job's identity, fails naming a missing stream and is clean once every declared stream
// is made; and ensure, as the operator, reads the hold row through the broker and makes
// ENVOY_MEASURE while it is released, and refuses, exit 1 naming the phase, while it is held.
func TestTheStreamsCommandWords(t *testing.T) {
	for name, args := range map[string][]string{
		"no word":                    {},
		"an unknown word":            {"remove", "--nats-url", "nats://127.0.0.1:1", "--token-file", "x"},
		"no NATS URL":                {"check", "--token-file", "x"},
		"no credential":              {"check", "--nats-url", "nats://127.0.0.1:1"},
		"an operator with a seed":    {"ensure", "--nats-url", "nats://127.0.0.1:1", "--nkey-seed-file", "x"},
		"an operator with no broker": {"ensure", "--nats-url", "nats://127.0.0.1:1", "--token-file", "x"},
		"a replica count past five":  {"check", "--nats-url", "nats://127.0.0.1:1", "--token-file", "x", "--replicas", "6"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := run(t, args...)
			require.Equal(t, 2, code, "%s", stderr)
			require.Contains(t, stderr, "usage:")
		})
	}
	for _, word := range []string{"update", "census"} {
		code, _, stderr := run(t, word, "--nats-url", "nats://127.0.0.1:1", "--token-file", "x", "--broker-url", "http://127.0.0.1:1", "--hold-verify-keys", "k")
		require.Equal(t, 2, code, "streams %s: %s", word, stderr)
		require.Contains(t, stderr, "streams update and streams census are the writer hold's held commands", "streams %s", word)
	}

	t.Run("check", func(t *testing.T) {
		url, js := serviceHub(t, "measure-token", natsgrant.ForCallout(natsgrant.Measure()))
		token := writeFile(t, "token", "measure-token\n")
		code, stdout, stderr := run(t, "check", "--nats-url", url, "--token-file", token)
		require.Equal(t, 1, code, "%s%s", stdout, stderr)
		require.Contains(t, stdout, "FAIL "+bus.Stream+": missing")
		admin, err := natsgo.Connect(url)
		require.NoError(t, err)
		defer admin.Close()
		require.NoError(t, streamset.EnsureStreams(t.Context(), admin, streamset.Settings{}))
		measure, _ := streamset.Find(streamset.Declared(streamset.Settings{}), streamset.MeasureStream)
		_, err = js.AddStream(&measure.Config)
		require.NoError(t, err)
		code, stdout, stderr = run(t, "check", "--nats-url", url, "--token-file", token)
		require.Equal(t, 0, code, "%s%s", stdout, stderr)
		require.Equal(t, "CLEAN\n", stdout)
	})

	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	verify := base64.RawURLEncoding.EncodeToString(public)

	t.Run("ensure", func(t *testing.T) {
		url, js := serviceHub(t, "operator-token", natsgrant.Operator(natsgrant.OperatorPhaseReleased))
		token := writeFile(t, "token", "operator-token")
		held := holdBroker(t, key, natsgrant.OperatorPhaseHeld)
		code, stdout, stderr := run(t, "ensure", "--nats-url", url, "--token-file", token, "--broker-url", held, "--hold-verify-keys", verify)
		require.Equal(t, 1, code, "%s%s", stdout, stderr)
		require.Contains(t, stderr, "the row is held")
		_, err := js.StreamInfo(streamset.MeasureStream)
		require.ErrorIs(t, err, natsgo.ErrStreamNotFound, "ensure under a held row made the stream")

		released := holdBroker(t, key, natsgrant.OperatorPhaseReleased)
		code, stdout, stderr = run(t, "ensure", "--nats-url", url, "--token-file", token, "--broker-url", released, "--hold-verify-keys", verify)
		require.Equal(t, 0, code, "%s%s", stdout, stderr)
		require.Contains(t, stdout, "CHANGED "+streamset.MeasureStream+": made")
		_, err = js.StreamInfo(streamset.MeasureStream)
		require.NoError(t, err)
	})
}
