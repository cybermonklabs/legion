package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
)

func TestARereadStartedAfterDetachedWaitBeganIsNotCounted(t *testing.T) {
	local := awstest.NewLocal(policytest.Secret("WORKER_TOKEN", policy.OwnerShared, policy.TierAgent, "v1"))
	detached := &Detached{}
	if err := detached.Wait(context.Background()); err != nil {
		t.Fatalf("wait before a reread: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, Deps{
		Policy:        policytest.Current(t, local),
		Detached:      detached,
		SecretsStore:  aws.Name,
		SecretsPrefix: policytest.Prefix,
	})
	logged := policytest.CaptureLog(t)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/secrets/WORKER_TOKEN/reread", nil))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("a reread after detached wait began = %d %s; want 202", recorder.Code, recorder.Body.String())
	}
	if text := logged.String(); !strings.Contains(text, "broker: reread after its answer was not run because the pod is stopping") || !strings.Contains(text, "WORKER_TOKEN") {
		t.Fatalf("a reread after detached wait began logged %q; want the not-run reread line naming WORKER_TOKEN", text)
	}
}
