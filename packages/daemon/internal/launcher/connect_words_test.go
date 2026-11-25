package launcher

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// TestAConnectTheLauncherRefusesOrDialsPrintsNoCredential: a --connect carrying a made-up user
// name and password in its user information, or a token in its query, prints no run of any of
// them, in Run's error or in the launcher's own lines (captures launcher.connect/<case>). The
// command prints Run's error through redact.URLCredentials, and the launcher's lines go through
// redact.Writer, and both find a URL only by its "://":
//
//   - no-scheme, the value written without tcp://, user information and query and all, was
//     refused quoting it whole, with no "://".
//   - user-information, a tcp:// value with user information, was refused quoting it and wrapping
//     net.SplitHostPort's error, which repeats it less its scheme. It holds no query, so only the
//     refusal of user information can refuse it.
//   - query, a tcp:// value with a token in its query, was taken: its address was the value less
//     its scheme, and every reconnect logged "dial <that address>: <net's error, which repeats
//     it>". It holds no user information, which would have had it refused as user-information is,
//     and leaves only the refusal of a query to refuse it.
//   - dial, a tcp:// value the launcher takes, path "/" and all, logs its reconnect line naming
//     the host and port alone. A value the launcher takes can hold no credential, so this case
//     holds none; it shows the dial line names host:port and nothing more of the value, where the
//     line named the value less its scheme ("127.0.0.1:1/").
//
// Each refusal names the flag, and a value refused never reaches a dial. The twin shows each
// value holds the credentials its case says it carries, so the row can see one printed.
func TestAConnectTheLauncherRefusesOrDialsPrintsNoCredential(t *testing.T) {
	const user, password, token = "not-a-secret-user", "not-a-secret-password", oauth2calltest.Token
	query := "?access" + "_token=" + token
	cases := []struct {
		name, connect string
		credentials   []string
		// names is what the printed text must hold: the flag a refusal names, or the dial line.
		names string
	}{
		{"no-scheme", user + ":" + password + "@127.0.0.1:1/" + query, []string{user, password, token}, "--connect"},
		{"user-information", "tcp://" + user + ":" + password + "@127.0.0.1:1/", []string{user, password}, "--connect"},
		{"query", "tcp://127.0.0.1:1/" + query, []string{token}, "--connect"},
		{"dial", "tcp://127.0.0.1:1/", nil, "launcher: daemon connection: dial 127.0.0.1:1: "},
	}
	t.Run("twin", func(t *testing.T) {
		for _, tc := range cases {
			for _, credential := range tc.credentials {
				if oauth2calltest.Piece(tc.connect, credential) == "" {
					t.Errorf("%s: --connect %q holds no run of %q, so the row could see none printed", tc.name, tc.connect, credential)
				}
			}
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			printed := launchPrinting(t, tc.connect)
			oauth2calltest.RequireNoPiece(t, "launcher.connect/"+tc.name, printed, user, password, token)
			if !strings.Contains(printed, tc.names) {
				t.Errorf("the launcher printed %q, want it to hold %q", printed, tc.names)
			}
			if tc.credentials != nil && strings.Contains(printed, "launcher: daemon connection: ") {
				t.Errorf("the launcher dialed --connect %q, which it must refuse: %q", tc.connect, printed)
			}
		})
	}
}

// launchPrinting runs a single-role launcher given connect and answers what it printed: Run's
// error, and every line it wrote to its stderr. A launcher that refuses connect returns at once;
// one that takes it is stopped once it has logged the end of its first daemon connection, the line
// naming the address it dialed.
func launchPrinting(t *testing.T, connect string) string {
	t.Helper()
	stderr := &lockedBuffer{}
	cfg := Config{
		Connect: connect, Token: "launcher-token", Sandbox: "legion-legion-legion-208", Role: "tester", PodUID: "pod-1",
		PrivateDir: t.TempDir(), LeakCheckDir: t.TempDir(), Stderr: stderr, StopGrace: time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() { returned <- Run(ctx, cfg) }()
	var err error
	ended := false
	testwait.Eventually(t, "the launcher's return or its first daemon connection's line", func() bool {
		select {
		case err = <-returned:
			ended = true
			return true
		default:
			return strings.Contains(stderr.String(), "launcher: daemon connection: ")
		}
	})
	if !ended {
		cancel()
		err = testwait.Receive(t, "the stopped launcher's return", returned)
	}
	printed := stderr.String()
	if err != nil {
		printed = err.Error() + "\n" + printed
	}
	return printed
}
