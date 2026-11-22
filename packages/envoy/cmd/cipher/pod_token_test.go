// packages/envoy/cmd/cipher/pod_token_test.go
//
// The projected ServiceAccount token a pod's tests hand the client, and the file it is written to.
// They build on every platform, unlike pod_enroll_test.go (unix alone), because tests of other
// files, broker_values_line_test.go and urlcredential_test.go among them, use them too.
package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	testPodAccount = "system:serviceaccount:legion:k8s-session"
	testPodUID     = "11111111-2222-3333-4444-555555555555"
	testPodService = "k8s-session"
)

// fakePodToken is a JWT shaped like the projected ServiceAccount token a kubelet mounts for the
// agent-secrets audience. It is not signed by anything: the client reads its claims and never
// verifies them, and every broker a test hands it to is a fake.
func fakePodToken(t *testing.T, subject, podUID string) string {
	t.Helper()
	claims := map[string]any{
		"iss": "https://kubernetes.example/cluster",
		"aud": []string{"agent-secrets"},
		"sub": subject,
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	if podUID != "" {
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(payload) + ".c2ln"
}

// writePodToken writes token to a file in its own directory and answers the path, as the kubelet's
// projection mounts one.
func writePodToken(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
