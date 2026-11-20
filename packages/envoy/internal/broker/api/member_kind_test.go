package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/projectaccess"
)

// TestAnUncheckableMemberKindIsAnsweredUnderItsOneCode: the API port answers a refusal the broker
// made because it could not learn a person's member kind 503 under projectaccess.UncheckableCode,
// the code the airlock proxy answers the same refusal with, in the refusal's fixed words and
// never the failure's, which can name Dispatch's address. An error that is no such refusal is left
// to the caller's next case.
func TestAnUncheckableMemberKindIsAnsweredUnderItsOneCode(t *testing.T) {
	failure := errors.New("dial tcp 10.20.0.9:8766: connect: connection refused")
	uncheckable := &projectaccess.MemberKindUncheckable{Person: "uma@example.com", Err: failure}
	w := httptest.NewRecorder()
	if !writeMemberKindUncheckable(w, "the release", fmt.Errorf("read grant: %w", uncheckable)) {
		t.Fatal("writeMemberKindUncheckable did not answer a wrapped MemberKindUncheckable")
	}
	var body struct{ Code, Error string }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer %q is no refusal body: %v", w.Body.String(), err)
	}
	if w.Code != http.StatusServiceUnavailable || body.Code != projectaccess.UncheckableCode || body.Error != uncheckable.Error() {
		t.Fatalf("the answer is %d %+v; want 503 %s %q", w.Code, body, projectaccess.UncheckableCode, uncheckable.Error())
	}
	if strings.Contains(w.Body.String(), "10.20.0.9") {
		t.Fatalf("the answer %q names the failure", w.Body.String())
	}
	if writeMemberKindUncheckable(httptest.NewRecorder(), "the release", failure) {
		t.Fatal("writeMemberKindUncheckable answered an error that is no MemberKindUncheckable")
	}
}
