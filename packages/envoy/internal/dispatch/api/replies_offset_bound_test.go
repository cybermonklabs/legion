package api

import (
	"net/http"
	"strconv"
	"testing"
)

// A page's offset is the caller's input, and GET /api/v1/me/replies takes any offset up to the
// largest int, then reads offset+limit rows of each kind (listMyReplies' reach). Near that bound
// the sum wraps to a negative number, which Postgres refuses as a LIMIT, and the route answers 500
// for what is the caller's input: the class dispatch://INFRA-175 closed for the integers that reach
// an integer column (fitsIntegerColumn). The route must answer such an offset as the other paged
// reads do, an empty page past the end (GET /api/v1/me/answers, /api/v1/issues and /api/v1/search
// each answer 200 for it), or refuse it as a client error, and never 500. The control is an offset
// of 0, and an offset of 3000000000, whose sum with the limit does not wrap.
func TestAnOffsetNearTheLargestIntIsNoFaultOfTheRepliesRoute(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	for _, offset := range []string{strconv.Itoa(int(^uint(0) >> 1)), "9223372036854775800"} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/replies?limit=10&offset="+offset, nil, "alice")
		if response.Code >= 500 {
			t.Errorf("GET /api/v1/me/replies?limit=10&offset=%s: status=%d body=%s, want an empty page or a 4xx refusal", offset, response.Code, response.Body.String())
		}
	}
	for _, offset := range []string{"0", "3000000000"} {
		if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/replies?limit=10&offset="+offset, nil, "alice"); response.Code != http.StatusOK {
			t.Errorf("GET /api/v1/me/replies?limit=10&offset=%s (the control): status=%d body=%s, want 200", offset, response.Code, response.Body.String())
		}
	}
}
