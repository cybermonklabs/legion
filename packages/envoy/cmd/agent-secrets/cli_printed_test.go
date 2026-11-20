// packages/envoy/cmd/agent-secrets/cli_printed_test.go
//
// What the forms print about a value a person typed or set: a flag's value that does not parse,
// keygen's help, AGENT_SECRETS_ENROLL_WAIT, an argument a form does not take, a settings-file key,
// an id that cannot be in a call's path, an unknown form, and the route of a failed call whose path
// holds a typed id. Each names the flag, the variable or the form, and never the value: a URL
// flag's value can carry a credential, and an argument in the wrong place is where a person who
// pasted one puts it. Each test runs a form in-process (runIn), or the client's revoke calls where
// no form reaches the line alone, with a made-up secret where a person could put one, and a
// control row that shows the clean case still works. keydir_printed_test.go holds the files that
// hold a key or a sign-in.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// signArgs is a use command that asks nothing of the network once it has a signer.
var signArgs = []string{"sign", "--method", "GET", "--url", "https://broker.example/v1/enrollments/self"}

// useBareEnvironment gives the rest of t an environment of its own: HOME, XDG_STATE_HOME and
// XDG_RUNTIME_DIR empty directories (so no settings file, sign-in, machine cache or helper socket
// of the developer's is read), AGENT_SECRETS_KEY_DIR an empty directory, no home from the password
// database (so a row that empties HOME reaches none of the developer's files either), and no
// broker, helper socket, machine identity, person or enrollment wait named.
func useBareEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("AGENT_SECRETS_KEY_DIR", t.TempDir())
	for _, name := range []string{"AGENT_SECRETS_URL", "AGENT_SECRETS_HELPER_SOCK", "AGENT_SECRETS_SIGNIN_DIR", "AGENT_SECRETS_MACHINE_IDENTITY", "AGENT_SECRETS_PERSON", "AGENT_SECRETS_ENROLL_WAIT"} {
		t.Setenv(name, "")
	}
	original := passwdHome
	passwdHome = func() string { return "" }
	t.Cleanup(func() { passwdHome = original })
}

// runIn runs `agent-secrets args...` in-process.
func runIn(args ...string) (stdout, stderr string, code int) {
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

// TestAFlagValueThatDoesNotParseIsNotPrinted: a flag whose value does not parse is refused naming
// the flag, and an argument whose flag syntax does not parse, or that is no flag of the form, is
// refused as one, never quoting the value or the argument, which Go's flag package does: an
// argument that starts with '-' can be a token pasted in the wrong place. Each prints the form's
// usage and exits 2, as before. A flag given no value is still named as Go names it, and values
// that parse still parse (the controls).
func TestAFlagValueThatDoesNotParseIsNotPrinted(t *testing.T) {
	const secret = "hunter2pw-flagvalue-5e0b32"
	for _, row := range []struct {
		name string
		args []string
		want []string
		// parses is a row whose values all parse, which no line may refuse as invalid.
		parses bool
	}{
		{"a duration given as the next argument", []string{"request", "GRANT_ME", "--ttl", secret}, []string{"invalid value for flag -ttl", "usage: agent-secrets request"}, false},
		{"a duration given after =", []string{"request", "GRANT_ME", "--ttl=" + secret}, []string{"invalid value for flag -ttl", "usage: agent-secrets request"}, false},
		{"a boolean", []string{"request", "GRANT_ME", "--json=" + secret}, []string{"invalid boolean value for flag -json", "usage: agent-secrets request"}, false},
		{"a boolean of machine list", []string{"machine", "list", "--json=" + secret}, []string{"invalid boolean value for flag -json", "usage: agent-secrets machine list"}, false},
		{"a number", []string{"register", "--wait=" + secret}, []string{"invalid value for flag -wait", "usage: agent-secrets register"}, false},
		{"a URL flag of three dashes", []string{"sign", "---url=https://tokuser9f3:" + secret + "@broker.example/v1/x"}, []string{"bad flag syntax: a flag that does not parse", "usage: agent-secrets sign"}, false},
		{"an argument that is no flag", []string{"request", "GRANT_ME", "--" + secret}, []string{"an argument that is not a flag of this form (not printed)", "usage: agent-secrets request"}, false},
		{"an argument that is no flag, with a value", []string{"secret", "list", "-" + secret + "=x"}, []string{"an argument that is not a flag of this form (not printed)", "usage: agent-secrets secret list"}, false},
		{"control: a flag given no value", []string{"request", "GRANT_ME", "--ttl"}, []string{"flag needs an argument: -ttl"}, false},
		{"control: values that parse", []string{"request", "GRANT_ME", "--ttl", "1h", "--json=true"}, []string{"AGENT_SECRETS_URL is required"}, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			useBareEnvironment(t)
			stdout, stderr, code := runIn(row.args...)
			if code != exitUsageError {
				t.Fatalf("exit %d, stderr %q; want %d", code, stderr, exitUsageError)
			}
			wantNamedNotPrinted(t, "stderr", stdout+stderr, row.want, []string{secret, "tokuser9f3"}...)
			if row.parses && strings.Contains(stderr, "invalid") {
				t.Errorf("stderr %q refuses a value that parses", stderr)
			}
		})
	}
	t.Run("the error Parse answers", func(t *testing.T) {
		var stderr bytes.Buffer
		flags := newFlagSet("request", &stderr)
		flags.Duration("ttl", 0, "a lifetime")
		err := flags.Parse([]string{"--ttl=" + secret})
		if err == nil || err.Error() != "invalid value for flag -ttl" {
			t.Fatalf("Parse = %v, want the fixed refusal naming -ttl", err)
		}
		if strings.Contains(stderr.String(), secret) {
			t.Errorf("stderr %q holds the value", stderr.String())
		}
		if err := flags.Parse([]string{"--ttl=90s"}); err != nil || flags.Lookup("ttl").Value.String() != "1m30s" {
			t.Fatalf("Parse(--ttl=90s) = %v with -ttl %q, want 1m30s", err, flags.Lookup("ttl").Value)
		}
		if _, ok := flags.Lookup("ttl").Value.(flag.Getter); !ok {
			t.Errorf("-ttl's value is %T after Parse, not the flag package's own", flags.Lookup("ttl").Value)
		}
	})
}

// TestHelpNamesTheKeyDirVariableNotItsValue: keygen's --out defaults to AGENT_SECRETS_KEY_DIR, the
// directory of key.pem, and a help text is pasted wherever a person asks for help, so -h and the
// usage a parse failure prints name the variable beside the flag, never its value. A default
// that is the program's own constant still prints as it is (the control).
func TestHelpNamesTheKeyDirVariableNotItsValue(t *testing.T) {
	const secret = "hunter2pw-helpdir-80c4f1"
	for _, row := range []struct {
		args []string
		code int
		want []string
	}{
		{[]string{"keygen", "-h"}, 0, []string{`-out string`, "AGENT_SECRETS_KEY_DIR when not given"}},
		{[]string{"keygen", "--no-such-flag"}, exitUsageError, []string{"an argument that is not a flag of this form (not printed)", "AGENT_SECRETS_KEY_DIR when not given"}},
		{[]string{"enroll", "-h"}, 0, []string{`(default "` + defaultPodTokenFile + `")`}},
	} {
		t.Run(strings.Join(row.args, " "), func(t *testing.T) {
			useBareEnvironment(t)
			t.Setenv("AGENT_SECRETS_KEY_DIR", filepath.Join(t.TempDir(), secret))
			stdout, stderr, code := runIn(row.args...)
			if code != row.code {
				t.Fatalf("exit %d, stderr %q; want %d", code, stderr, row.code)
			}
			wantNamedNotPrinted(t, "the help", stdout+stderr, row.want, []string{secret}...)
		})
	}
}

// TestEnrollWaitRefusalNamesTheRuleNotTheValue: an AGENT_SECRETS_ENROLL_WAIT that is no duration
// is refused naming the variable and the rule it broke, never its value, by a use command and by
// `identity --source` alike, exit 2; a duration still bounds the wait (the control).
func TestEnrollWaitRefusalNamesTheRuleNotTheValue(t *testing.T) {
	const secret = "hunter2pw-enrollwait-6a2d90"
	for _, row := range []struct {
		name, wait string
		args       []string
		code       int
	}{
		{"a use command", secret, signArgs, exitUsageError},
		{"identity --source", secret, []string{"identity", "--source"}, exitUsageError},
		{"control: a duration", "3s", signArgs, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			useBareEnvironment(t)
			t.Setenv("AGENT_SECRETS_KEY_DIR", newKeyDir(t))
			t.Setenv("AGENT_SECRETS_ENROLL_WAIT", row.wait)
			_, stderr, code := runIn(row.args...)
			if code != row.code {
				t.Fatalf("exit %d, stderr %q; want %d", code, stderr, row.code)
			}
			if row.code == 0 {
				if strings.Contains(stderr, "AGENT_SECRETS_ENROLL_WAIT") {
					t.Errorf("stderr %q refuses a duration", stderr)
				}
				return
			}
			wantNamedNotPrinted(t, "stderr", stderr, []string{"AGENT_SECRETS_ENROLL_WAIT", "Go duration"}, []string{secret}...)
		})
	}
}

// wantUnexpectedArgument is the refusal of an argument a form does not take, as every form prints
// it, written out by hand.
const wantUnexpectedArgument = "unexpected argument (not printed: an argument this form does not take may be a credential pasted in the wrong place)"

// TestAnUnexpectedArgumentIsNotPrinted: a form that takes no such argument refuses it, exit 2,
// without quoting it: an argument a form does not take is where a credential pasted in the wrong
// place lands. Each form run without it goes on past the check (the control).
func TestAnUnexpectedArgumentIsNotPrinted(t *testing.T) {
	const secret = "hunter2pw-positional-4e7a19"
	for _, form := range []struct {
		args []string
		// waits is a form that, with nothing after it, waits on the helper, so it has no control
		// row: its refusal row above is the check.
		waits bool
	}{
		{args: []string{"grant", "list"}}, {args: []string{"identity"}}, {args: []string{"login"}}, {args: []string{"keygen"}}, {args: []string{"self"}},
		{args: []string{"whoami"}}, {args: []string{"register"}, waits: true}, {args: []string{"secret", "list"}}, {args: []string{"sign"}},
	} {
		name := strings.Join(form.args, " ")
		t.Run(name, func(t *testing.T) {
			useBareEnvironment(t)
			t.Setenv("AGENT_SECRETS_HELPER_SOCK", filepath.Join(t.TempDir(), "none.sock"))
			stdout, stderr, code := runIn(append(append([]string{}, form.args...), secret)...)
			if code != exitUsageError {
				t.Fatalf("exit %d, stderr %q; want %d", code, stderr, exitUsageError)
			}
			wantNamedNotPrinted(t, "the output", stdout+stderr, []string{"agent-secrets " + name + ": " + wantUnexpectedArgument}, []string{secret}...)
		})
		if form.waits {
			continue
		}
		t.Run("control: "+name+" with no such argument", func(t *testing.T) {
			useBareEnvironment(t)
			t.Setenv("AGENT_SECRETS_HELPER_SOCK", filepath.Join(t.TempDir(), "none.sock"))
			stdout, stderr, _ := runIn(form.args...)
			if strings.Contains(stdout+stderr, "unexpected argument") {
				t.Errorf("the output %q refuses an argument it was not given", stdout+stderr)
			}
		})
	}
}

// TestASettingsFileKeyItDoesNotTakeIsNotPrinted: a line of the settings file whose key the file
// does not take is refused naming the file and the line, never the key: a token pasted into the
// file as a line of its own reads as a key up to its first '='. A line whose key the file takes is
// still read (the control).
func TestASettingsFileKeyItDoesNotTakeIsNotPrinted(t *testing.T) {
	const secret = "eyJhbGciOiJSUzI1NiJ9.hunter2pw-settingsline-0b6c33"
	for _, line := range []string{secret + "==", secret + "=x"} {
		t.Run(line, func(t *testing.T) {
			path := writeClientConfig(t, "# settings\n\n"+line+"\n")
			stdout, stderr, code := runIn("identity")
			if code != exitUsageError {
				t.Fatalf("exit %d, stderr %q; want %d", code, stderr, exitUsageError)
			}
			wantNamedNotPrinted(t, "the output", stdout+stderr, []string{path + " line 3 sets a key this file does not take (not printed)"}, []string{"hunter2pw-settingsline"}...)
		})
	}
	t.Run("control: a key the file takes", func(t *testing.T) {
		unsetenv(t, "AGENT_SECRETS_URL")
		writeClientConfig(t, "AGENT_SECRETS_URL=https://secrets.example\n")
		if _, stderr, code := runIn("identity"); code == exitUsageError {
			t.Fatalf("exit %d, stderr %q: a key the file takes was refused", code, stderr)
		}
		if got := clientGetenv("AGENT_SECRETS_URL"); got != "https://secrets.example" {
			t.Fatalf("the file's AGENT_SECRETS_URL read as %q", got)
		}
	})
}

// TestAnIdThatCannotBeInAPathIsNotPrinted: an id a form would put in a call's path, which a URL
// path would change, is refused before any call naming the kind of id and never the argument, which
// is where a credential pasted in the wrong place lands. An id that can be in a path goes on to the
// call (the control).
func TestAnIdThatCannotBeInAPathIsNotPrinted(t *testing.T) {
	const secret = "hunter2pw/pathid-91de07"
	for _, row := range []struct {
		args []string
		kind string
	}{
		{[]string{"status", secret}, "a request id"},
		{[]string{"cancel", secret}, "a request id"},
		{[]string{"revoke", secret}, "a grant id"},
		{[]string{"machine", "revoke", secret}, "a machine id"},
		{[]string{"grant", "revoke", secret}, "a grant id"},
	} {
		for _, id := range []struct {
			value   string
			refused bool
		}{{secret, true}, {"plain-id-0001", false}} {
			args := append(append([]string{}, row.args[:len(row.args)-1]...), id.value)
			name := strings.Join(args, " ")
			if !id.refused {
				name = "control: " + name
			}
			t.Run(name, func(t *testing.T) {
				useBareEnvironment(t)
				t.Setenv("AGENT_SECRETS_URL", "http://127.0.0.1:1")
				t.Setenv("AGENT_SECRETS_KEY_DIR", newKeyDir(t))
				sock := filepath.Join(t.TempDir(), "helper.sock")
				if err := os.WriteFile(sock, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
				stdout, stderr, code := runIn(args...)
				if code == 0 {
					t.Fatalf("exit 0, stdout %q; want a failure", stdout)
				}
				refusal := "not " + row.kind + " (the argument is not printed: it holds a character a call's path would change)"
				if !id.refused {
					if strings.Contains(stderr, "not "+row.kind) {
						t.Errorf("stderr %q refuses an id that can be in a path", stderr)
					}
					return
				}
				wantNamedNotPrinted(t, "the output", stdout+stderr, []string{refusal}, []string{"hunter2pw"}...)
			})
		}
	}
}

// TestAnUnknownFormIsNotPrinted: a group of forms - machine, grant, secret - refuses a form it does
// not have naming the forms it has, never the argument, which is where a credential pasted in the
// wrong place lands. A group given no form still says one is required (the control).
func TestAnUnknownFormIsNotPrinted(t *testing.T) {
	const secret = "hunter2pw-form-2a5c70"
	for _, row := range []struct{ group, forms string }{
		{"machine", "login, login-status, list, revoke"},
		{"grant", "list, revoke"},
		{"secret", "list, show, create, set, retag, delete, restore"},
	} {
		t.Run(row.group, func(t *testing.T) {
			useBareEnvironment(t)
			stdout, stderr, code := runIn(row.group, secret)
			if code != exitUsageError {
				t.Fatalf("exit %d, stderr %q; want %d", code, stderr, exitUsageError)
			}
			wantNamedNotPrinted(t, "the output", stdout+stderr, []string{"agent-secrets " + row.group + ": unknown form (not printed); the forms are " + row.forms}, []string{secret}...)
		})
		t.Run("control: "+row.group+" with no form", func(t *testing.T) {
			useBareEnvironment(t)
			_, stderr, code := runIn(row.group)
			if code != exitUsageError || !strings.Contains(stderr, "agent-secrets "+row.group+": a form is required: "+row.forms) {
				t.Fatalf("exit %d, stderr %q; want %d naming the forms", code, stderr, exitUsageError)
			}
		})
	}
}

// useUnreachableBroker leaves the rest of t with a bare environment whose broker is url, a key dir
// that holds a session to sign with, and a helper that signs every call this machine's login makes.
func useUnreachableBroker(t *testing.T, url string) {
	t.Helper()
	useBareEnvironment(t)
	t.Setenv("AGENT_SECRETS_URL", url)
	t.Setenv("AGENT_SECRETS_KEY_DIR", newKeyDir(t))
	sock, _ := fakeHelper(t, helper.Response{OK: true, Proof: "eyJ.fake.proof"})
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
}

// TestAnIdAPersonTypedIsLeftOutOfAFailedCallsRoute: a form that puts an id the person typed in a
// call's path, and whose call fails before the broker answers it - nothing answers, or the answer
// is cut off - names the route with the id left out, as `(the machine id, not printed)`: the
// broker never accepted the id, and an argument that is no id is where a credential pasted in the
// wrong place lands. A call whose path holds no typed id still prints its URL whole (the control),
// and the failure still classifies as it did (errors.Is).
func TestAnIdAPersonTypedIsLeftOutOfAFailedCallsRoute(t *testing.T) {
	const secret = "hunter2pw-typedid-60b1e4"
	forms := []struct {
		args  []string
		route string
	}{
		{[]string{"status", secret}, "GET %s/v1/requests/(the request id, not printed)"},
		{[]string{"cancel", secret}, "POST %s/v1/requests/(the request id, not printed)/cancel"},
		{[]string{"revoke", secret}, "POST %s/v1/grants/(the grant id, not printed)/revoke"},
		{[]string{"machine", "revoke", secret}, "POST %s/v1/operator/machines/(the machine id, not printed)/revoke"},
		// In a session (the key dir here holds one), grant revoke is the session's own revoke.
		{[]string{"grant", "revoke", secret}, "POST %s/v1/grants/(the grant id, not printed)/revoke"},
	}
	cutOff := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"partial":`))
	}))
	t.Cleanup(cutOff.Close)
	for _, broker := range []struct{ name, url, says string }{
		{"nothing answers", "http://127.0.0.1:1", "the connection was refused"},
		{"the answer is cut off", cutOff.URL, "read response"},
	} {
		for _, form := range forms {
			t.Run(broker.name+", "+strings.Join(form.args[:len(form.args)-1], " "), func(t *testing.T) {
				useUnreachableBroker(t, broker.url)
				stdout, stderr, code := runIn(form.args...)
				if code == 0 {
					t.Fatalf("exit 0, stdout %q; want a failure", stdout)
				}
				wantNamedNotPrinted(t, "the output", stdout+stderr, []string{fmt.Sprintf(form.route, broker.url), broker.says}, []string{secret}...)
			})
		}
	}
	t.Run("control: a call whose path holds no typed id", func(t *testing.T) {
		useUnreachableBroker(t, "http://127.0.0.1:1")
		_, stderr, code := runIn("grant", "list")
		if code == 0 || !strings.Contains(stderr, "GET http://127.0.0.1:1/v1/operator/grants") || strings.Contains(stderr, "not printed)") {
			t.Fatalf("exit %d, stderr %q; want a failure naming the call's URL whole", code, stderr)
		}
	})
	t.Run("the failure still classifies", func(t *testing.T) {
		key, err := proof.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		c := newClient(brokeraddr.New("http://127.0.0.1:1"))
		signer := &fileSigner{key: key, enrollmentID: "enr-test"}
		for route, err := range map[string]error{
			"POST http://127.0.0.1:1/v1/operator/machines/(the machine id, not printed)/revoke": c.RevokeOperatorMachine(context.Background(), signer, secret),
			"POST http://127.0.0.1:1/v1/operator/grants/(the grant id, not printed)/revoke":     c.RevokeOperatorGrant(context.Background(), signer, secret),
		} {
			var unreachable *unreachableError
			if !errors.Is(err, syscall.ECONNREFUSED) || !errors.As(err, &unreachable) {
				t.Fatalf("%s = %v; want an unreachableError that is ECONNREFUSED", route, err)
			}
			wantNamedNotPrinted(t, "the error", err.Error(), []string{route}, []string{secret}...)
		}
	})
}
