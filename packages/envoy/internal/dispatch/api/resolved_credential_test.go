package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestARefusedCallerIsNeitherReadOnlyNorApproveOnly holds the one read of the caller the two token
// gates share (resolvedCredential) to a caller that resolved. A decision that carries a refusal
// (resolvedCaller.err) gives neither gate a credential, though the actor beside it holds one; the
// same decision with no refusal gives each gate its credential; and a request no layer decided
// gives neither. Every layer that carries a refusal today carries the zero actor beside it
// (brokerTokenActor, optionalActor), so no request through the server can tell the error check
// from its absence, and this reads the decision on the context directly.
func TestARefusedCallerIsNeitherReadOnlyNorApproveOnly(t *testing.T) {
	readOnly := &model.Credential{ReadOnly: true}
	approveOnly := &model.Credential{ApproveOnly: true}
	refusal := errorf(http.StatusUnauthorized, "BROKER_TOKEN_NOT_ACTIVE", "the secrets broker does not hold this access token as live")
	carrying := func(caller resolvedCaller) *http.Request {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
		return request.WithContext(context.WithValue(request.Context(), resolvedCallerKey{}, caller))
	}

	if got := readOnlyCredential(carrying(resolvedCaller{actor: model.Actor{Credential: readOnly}, err: refusal}).Context()); got != nil {
		t.Errorf("readOnlyCredential of a refused caller holding a read-only credential = %+v; want nil", got)
	}
	if approveOnlyRequest(carrying(resolvedCaller{actor: model.Actor{Credential: approveOnly}, err: refusal})) {
		t.Error("approveOnlyRequest of a refused caller holding an approve-only credential = true; want false")
	}

	if got := readOnlyCredential(carrying(resolvedCaller{actor: model.Actor{Credential: readOnly}}).Context()); got != readOnly {
		t.Errorf("readOnlyCredential of a resolved read-only caller = %+v; want its credential", got)
	}
	if !approveOnlyRequest(carrying(resolvedCaller{actor: model.Actor{Credential: approveOnly}})) {
		t.Error("approveOnlyRequest of a resolved approve-only caller = false; want true")
	}

	undecided := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	if got := readOnlyCredential(undecided.Context()); got != nil {
		t.Errorf("readOnlyCredential of a request no layer decided = %+v; want nil", got)
	}
	if approveOnlyRequest(undecided) {
		t.Error("approveOnlyRequest of a request no layer decided = true; want false")
	}
}
