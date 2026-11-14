// packages/envoy/internal/broker/helper/protocol_test.go

package helper

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSocketIsThereWhenSetOrPresent: a helper socket is there whenever SocketVar names one, even
// a path nothing is at, since a client then asks that socket and fails rather than signing
// another way, and the named socket is the one asked even with the default present beside it;
// with SocketVar unset, the socket is the default under XDG_RUNTIME_DIR, there only when that
// path exists. agent-secrets takes the helper by this rule, and `legion controller start` drops
// the machine session kept in the controller's key directory by it (R6), so the two never
// disagree about whether the helper signs the controller's reads.
func TestSocketIsThereWhenSetOrPresent(t *testing.T) {
	for _, tc := range []struct {
		name string
		// set names a socket in SocketVar, at a path nothing is at.
		set bool
		// present puts a file at the default socket.
		present   bool
		wantThere bool
	}{
		{name: "set, with nothing at the path it names", set: true, wantThere: true},
		{name: "set, with the default present too", set: true, present: true, wantThere: true},
		{name: "unset, with the default present", present: true, wantThere: true},
		{name: "unset, with nothing at the default", wantThere: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtimeDir := t.TempDir()
			env := map[string]string{"XDG_RUNTIME_DIR": runtimeDir}
			getenv := func(name string) string { return env[name] }
			want := filepath.Join(runtimeDir, "agent-secrets", "helper.sock")
			if tc.present {
				if err := os.MkdirAll(filepath.Dir(want), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(want, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.set {
				want = filepath.Join(t.TempDir(), "absent", "helper.sock")
				env[SocketVar] = want
			}

			sock, there := Socket(getenv)
			if sock != want || there != tc.wantThere {
				t.Errorf("Socket = %q, %v; want %q, %v", sock, there, want, tc.wantThere)
			}
		})
	}
}
