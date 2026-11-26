package api

import (
	"strings"
	"testing"
)

// TestAnAssigneeReadFailureNamesNoPerson: a failed read of whether a login may be assigned names no
// person, since the error reaches the log line of the 500 it answers. The rule is the access
// package's (TestNoAccessErrorNamesAPerson); this read is the API's.
//
// Control: the login back in the error's text; the test fails naming it.
func TestAnAssigneeReadFailureNamesNoPerson(t *testing.T) {
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	database.Pool.Close()
	const login = "dana@example.com"
	_, err := served.allowedLogin(t.Context(), login)
	if err == nil {
		t.Fatal("a read on a closed pool answered no error")
	}
	if strings.Contains(err.Error(), login) || strings.Contains(err.Error(), "dana") {
		t.Errorf("the assignee read's error = %q, which names the person", err)
	}
}
