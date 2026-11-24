package cipher

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// echoInTime is a stand-in for the broker that accepts every call, and answers it with a body of
// the contract's shape whose time is the credentials the call carried: the Proof header, the
// Authorization header's bearer and every string of the call's JSON body, joined. A decoder's
// error quotes the time it could not read whole ("parsing time \"...\""), so a client that prints
// that error prints the credentials.
func echoInTime(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		carried := []string{r.Header.Get("Proof"), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		for _, value := range body {
			if s, ok := value.(string); ok {
				carried = append(carried, s)
			}
		}
		echo := strings.Join(carried, " ")
		answer := map[string]any{
			"credential_id": "cred-echo", "expires_at": echo,
			"enrollment_id": "enr-echo", "lease_expires_at": echo,
			"pod_uid": podEnrollment.PodUID, "thumbprint": podEnrollment.Thumbprint, "generation": 1,
			"carried": []map[string]any{{"session_id": podEnrollment.Session, "enrollment_id": "enr-echo", "state": "live", "ends_at": echo}},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestAnAcceptedBrokerAnswerThatDoesNotDecodePrintsNoCredentialItEchoes: the broker's 200, or a
// stand-in's, echoes the call's credentials into a time the client decodes - the login's
// ServiceAccount token, the registration's bearer and pod token, the enrollment's and the carried
// list's launcher proof - and each call's error names the answer as not the contract's shape and
// holds none of them: the answer decodes through oauth2call.Answer.Decode, whose error quotes no
// value.
func TestAnAcceptedBrokerAnswerThatDoesNotDecodePrintsNoCredentialItEchoes(t *testing.T) {
	const serviceToken = "daemon-service-token-RDX-7c41e2"
	server := echoInTime(t)
	newClient := func() *Client {
		return &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, serviceToken)}
	}
	for name, call := range map[string]func(c *Client) error{
		"the login": func(c *Client) error {
			_, err := c.Login(context.Background())
			return err
		},
		"the pod session's registration": func(c *Client) error {
			_, err := c.RegisterPodSession(context.Background(), podEnrollment.PodUID, podEnrollment.PodToken, podEnrollment.Session, podEnrollment.Thumbprint, 1)
			return err
		},
		"the enrollment": func(c *Client) error {
			withCredential(t, c, "cred-0")
			_, err := c.Enroll(context.Background(), podEnrollment)
			return err
		},
		"the carried list": func(c *Client) error {
			withCredential(t, c, "cred-0")
			_, err := c.Carried(context.Background(), []string{podEnrollment.Session})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call(newClient())
			if err == nil || !strings.Contains(err.Error(), "not the contract's shape") {
				t.Fatalf("%s through an answer that does not decode = %v, want it named as not the contract's shape", name, err)
			}
			for _, credential := range []string{serviceToken, podEnrollment.PodToken, "eyJ"} {
				if strings.Contains(err.Error(), credential) {
					t.Errorf("%s's error holds the credential %q the call carried:\n%s", name, credential, err)
				}
			}
		})
	}
}
