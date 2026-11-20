// revoke_refusal_test.go reads writeRevokeRefusal directly: a revoke that keeps losing its grant
// to adoptions cannot be driven through the route without the requests package's own test hook,
// so the row holds the answer each of RevokeByApprover's refusals gets.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/requests"
)

// TestARevokeRefusalAnswersItsOwnCode: each refusal RevokeByApprover answers, wrapped as it wraps
// it, gets its own status and code. A grant that moved to another box each time the revoke
// read it (requests.ErrGrantMoving) is 409 GRANT_MOVING with the sentence that says to run the
// revoke again, not the 500 INTERNAL every unnamed error gets; that one is the control.
func TestARevokeRefusalAnswersItsOwnCode(t *testing.T) {
	for _, c := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"no such grant", pgx.ErrNoRows, http.StatusNotFound, "NOT_FOUND"},
		{"Dispatch down", fmt.Errorf("ask: %w", requests.ErrProjectAccessUnavailable), http.StatusServiceUnavailable, "PROJECT_ACCESS_UNAVAILABLE"},
		{"not named", requests.ErrNotApprover, http.StatusForbidden, "NOT_APPROVER"},
		{"kept moving", fmt.Errorf("revoke grant g: %w: it moved 3 times in a row", requests.ErrGrantMoving), http.StatusConflict, "GRANT_MOVING"},
		{"anything else (the control)", errors.New("the database went away"), http.StatusInternalServerError, "INTERNAL"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeRevokeRefusal(rec, c.err)
			var body struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("the answer %q is not a JSON error: %v", rec.Body.String(), err)
			}
			if rec.Code != c.status || body.Code != c.code {
				t.Errorf("writeRevokeRefusal(%v) = %d %s; want %d %s", c.err, rec.Code, body.Code, c.status, c.code)
			}
			if c.code == "GRANT_MOVING" && body.Error != grantMoving {
				t.Errorf("409 GRANT_MOVING says %q; want %q", body.Error, grantMoving)
			}
		})
	}
}
