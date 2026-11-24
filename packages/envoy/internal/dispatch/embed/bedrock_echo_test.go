package embed

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestABedrockCallPrintsNoEchoOfItsCredential: whatever answers at Bedrock's address answers the
// embeddings call, which carries the session token in X-Amz-Security-Token, with an echo of it: a
// redirect whose Location does not parse (unparsed), which net/http's error quotes whatever the
// client's redirect policy, since Go parses a Location before CheckRedirect is asked, and a 403
// whose request id header and message quote it (answer). The call's error, which the embedding
// queue logs, holds no run of the token and no word of the Location, and the answer's error still
// names its status. The client is the SDK's own, with its own HTTP client and retries, as
// NewBedrock builds it. The twin shows the SDK's own error holds the echo. Captures
// embed.bedrock-echo/<case>.
func TestABedrockCallPrintsNoEchoOfItsCredential(t *testing.T) {
	const token = "ws3hn-not-a-secret-RDX-6d0f"
	replies := map[string]func(w http.ResponseWriter, credential string){
		"unparsed": func(w http.ResponseWriter, credential string) {
			w.Header().Set("Location", redirectstest.Unparsed(credential))
			w.WriteHeader(http.StatusFound)
		},
		"answer": func(w http.ResponseWriter, credential string) {
			w.Header().Set("X-Amzn-Requestid", credential)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `{"__type":"AccessDeniedException","message":"denied to %s"}`, credential)
		},
	}
	serve := func(t *testing.T, reply func(http.ResponseWriter, string)) aws.Config {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reply(w, r.Header.Get("X-Amz-Security-Token"))
		}))
		t.Cleanup(server.Close)
		return aws.Config{
			Region:       "us-east-1",
			Credentials:  credentials.NewStaticCredentialsProvider("test-access-key", "test-secret-key", token),
			BaseEndpoint: aws.String(server.URL),
		}
	}
	t.Run("twin", func(t *testing.T) {
		_, err := bedrockruntime.NewFromConfig(serve(t, replies["unparsed"])).InvokeModel(context.Background(), &bedrockruntime.InvokeModelInput{
			ModelId: aws.String(BedrockModel), Body: []byte(`{}`), ContentType: aws.String("application/json"),
		})
		if err == nil || !strings.Contains(err.Error(), token) {
			t.Fatalf("the SDK's own error holds no echo of the session token, so the rows cannot see a leak: %v", err)
		}
	})
	for _, name := range []string{"unparsed", "answer"} {
		t.Run(name, func(t *testing.T) {
			_, err := NewBedrockWithConfig(serve(t, replies[name])).Embed(context.Background(), []string{"x"}, InputDocument)
			if err == nil {
				t.Fatal("the call answered no error")
			}
			redacttest.RequireNoPiece(t, "embed.bedrock-echo/"+name, err.Error(), token)
			if strings.Contains(err.Error(), "moved") {
				t.Errorf("the call's error names the Location: %q", err)
			}
			if name == "answer" && !strings.Contains(err.Error(), "403") {
				t.Errorf("the call's error names no status: %q", err)
			}
		})
	}
}
