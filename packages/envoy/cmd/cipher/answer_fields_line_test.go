// answer_fields_line_test.go drives the values this CLI prints from an answer it accepts - a
// machine login's user_code and verification_uri, and a pending request's approve_url - with a
// control character in them, each of which must print on one line.
package main

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/helper"
	"github.com/sjawhar/envoy/internal/redact"
)

// forgedValues are answer values that hold a line feed, a carriage return and a line feed, and an
// escape sequence: printed as they came, each would start a line of its own or rewrite the
// terminal's line.
var forgedValues = []string{
	"KQ7M-X4PZ\nagent-secrets machine login: FORGED approved",
	"KQ7M-X4PZ\r\nagent-secrets: FORGED",
	"KQ7M-X4PZ\x1b[2Kagent-secrets: FORGED",
}

// noControl reports a line of text that holds a control character, which none of these may.
func noControl(t *testing.T, what, text string) {
	t.Helper()
	if strings.ContainsAny(strings.TrimSuffix(text, "\n"), "\r\n\x1b") {
		t.Errorf("%s = %q, which holds a control character inside it", what, text)
	}
}

// TestMachineLoginPrintsTheBrokersCodeAndPageOnOneLine: machine login prints the user_code the
// broker answered as it came where it holds no control character, and Go-quoted where it holds
// one, so it stays on its own one line; a verification_uri with a control character in it is no
// plain URL, so it prints as redact.NotPrinted, on one line too.
func TestMachineLoginPrintsTheBrokersCodeAndPageOnOneLine(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at answer_fields_line_test.go:40).
	t.Setenv("AGENT_SECRETS_APPROVE_URL", "")
	const page = "https://dispatch.example/credentials/machine"
	rows := []struct{ code, uri, want string }{
		{"KQ7M-X4PZ", page, "machine login code: KQ7M-X4PZ\nenter it at " + page + " — approve only if the code matches this terminal\n"},
	}
	for _, forged := range forgedValues {
		rows = append(rows,
			struct{ code, uri, want string }{forged, page,
				"machine login code: " + strconv.Quote(forged) + "\nenter it at " + page + " — approve only if the code matches this terminal\n"},
			struct{ code, uri, want string }{"KQ7M-X4PZ", page + "\n" + forged,
				"machine login code: KQ7M-X4PZ\nenter it at " + redact.NotPrinted + " — approve only if the code matches this terminal\n"})
	}
	for _, row := range rows {
		t.Run(strconv.Quote(row.code)+" "+strconv.Quote(row.uri), func(t *testing.T) {
			t.Setenv("AGENT_SECRETS_HELPER_SOCK", fakeLoginHelperAt(t, row.code, row.uri, []string{"issued"}))
			var stdout, stderr bytes.Buffer
			if exit := cmdMachineLogin(nil, &stdout, &stderr); exit != 0 {
				t.Fatalf("machine login = %d (stderr %q), want 0", exit, stderr.String())
			}
			if stdout.String() != row.want {
				t.Errorf("stdout = %q, want %q", stdout.String(), row.want)
			}
			for _, line := range strings.SplitAfter(stdout.String(), "\n") {
				noControl(t, "a line of stdout", line)
			}
		})
	}
}

// TestMachineLoginStatusPrintsTheBrokersCodeOnOneLine: every line login-status names the most
// recent login's user_code in - a login waiting beside a held credential, a denied or expired one
// beside it, and a login waiting with none held - prints the code as it came where it holds no
// control character, and Go-quoted where it holds one.
func TestMachineLoginStatusPrintsTheBrokersCodeOnOneLine(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at answer_fields_line_test.go:93).
	for _, code := range append([]string{"KQ7M-X4PZ"}, forgedValues...) {
		printed := code
		if code != "KQ7M-X4PZ" {
			printed = strconv.Quote(code)
		}
		for _, row := range []struct {
			name string
			resp helper.Response
			line string
		}{
			{"pending beside a held credential", helper.Response{LoginState: "pending", CredentialHeld: true}, "a machine login is waiting for approval (code " + printed + ")"},
			{"denied beside a held credential", helper.Response{LoginState: "denied", CredentialHeld: true}, "the most recent machine login (code " + printed + ") was denied"},
			{"expired beside a held credential", helper.Response{LoginState: "expired", CredentialHeld: true}, "the most recent machine login (code " + printed + ") expired before anyone approved it"},
			{"pending with none held", helper.Response{LoginState: "pending"}, "(code " + printed + ")\n"},
		} {
			t.Run(row.name+" "+strconv.Quote(code), func(t *testing.T) {
				row.resp.OK, row.resp.Code = true, code
				sock, _ := fakeHelper(t, row.resp)
				t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
				var stdout, stderr bytes.Buffer
				cmdMachineLoginStatus(nil, &stdout, &stderr)
				if !strings.Contains(stderr.String(), row.line) {
					t.Errorf("stderr = %q, want it to hold %q", stderr.String(), row.line)
				}
				for _, line := range strings.SplitAfter(stderr.String(), "\n") {
					noControl(t, "a line of stderr", line)
				}
			})
		}
	}
}

// TestAPendingRequestNamesTheBrokersPageOnOneLine: the line that says whom a pending request
// waits on names the approve_url the broker answered as it came where it holds no control
// character, and Go-quoted where it holds one.
func TestAPendingRequestNamesTheBrokersPageOnOneLine(t *testing.T) {
	t.Parallel()
	const page = "https://broker.example/approve/req-1"
	approver := "ada@example.com"
	if got, want := waitingOn(cipheraddr.New("https://broker.example"), &approver, page, false), "waiting for ada@example.com to approve it on the secrets broker: "+page; got != want {
		t.Errorf("waitingOn = %q, want %q", got, want)
	}
	for _, forged := range forgedValues {
		brokerPage := page + "\n" + forged
		for _, who := range []*string{&approver, nil} {
			got := waitingOn(cipheraddr.New("https://broker.example"), who, brokerPage, false)
			if !strings.Contains(got, ": "+strconv.Quote(brokerPage)) {
				t.Errorf("waitingOn(%v, %q) = %q, want it to name the page Go-quoted", who, brokerPage, got)
			}
			noControl(t, "waitingOn", got)
		}
	}
}
