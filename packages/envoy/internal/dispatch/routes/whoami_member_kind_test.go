package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rawWhoami answers GET /auth/whoami's raw body for login.
func rawWhoami(t *testing.T, handler http.Handler, login string) (int, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil)
	request.Header.Set("X-Dispatch-User", login)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response.Code, strings.TrimSuffix(response.Body.String(), "\n")
}
