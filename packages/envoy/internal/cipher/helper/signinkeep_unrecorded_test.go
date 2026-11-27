// packages/envoy/internal/cipher/helper/signinkeep_unrecorded_test.go
//go:build linux

// A helper that finds no record of the boxes it enrolled under a person's sign-in cannot tell
// which boxes stand under the sign-in on this box: the release before kept no such record, and a
// lost one keeps none. A box a launcher enrolled there renews with its own key and tells the
// helper nothing, and the broker refuses it its renewal an hour after the sign-in's last trade.
// So the helper holds the sign-in on the box, and trades it until that sign-in leaves the box.

package helper

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// recordedHolds is what file holds: the boxes, each with the thumbprint of the sign-in it stands
// under, and the sign-ins held for want of a record.
func recordedHolds(t *testing.T, file string) (boxes map[string]string, signIns []string) {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read the record of the boxes: %v", err)
	}
	var kept struct {
		Boxes   map[string]string `json:"boxes"`
		SignIns []string          `json:"sign_ins_with_unrecorded_boxes"`
	}
	if err := json.Unmarshal(data, &kept); err != nil {
		t.Fatalf("the record of the boxes is %q, no record: %v", data, err)
	}
	return kept.Boxes, kept.SignIns
}

// warnings are logged's WARN records about the record of the boxes (their file attribute names
// it), each as its message and its person attribute where it has one.
func warnings(logged *logtest.Recorder) (messages, people []string) {
	for _, record := range logged.Records() {
		if record.Level != slog.LevelWarn {
			continue
		}
		var file, person string
		record.Attrs(func(attr slog.Attr) bool {
			switch attr.Key {
			case "file":
				file = attr.Value.String()
			case "person":
				person = attr.Value.String()
			}
			return true
		})
		if file != SignInBoxesNamed {
			continue
		}
		messages = append(messages, record.Message)
		if person != "" {
			people = append(people, person)
		}
	}
	return messages, people
}

// TestAHelperWithNoRecordOfItsBoxesKeepsTheSignInOnTheBoxTraded. Rows: the first start after a
// release that kept no record, with a person signed in, asks for their sign-in with nothing
// enrolled, says so at WARN naming them, and saves the hold; it stops once the person signs in
// again, which makes a key no box enrolled before stands under, and the record drops the hold. A
// restart keeps the hold with no second WARN. A start with nobody signed in holds nothing and
// starts an empty record, so a later sign-in is not held. A record that cannot be read is held
// the same way, said at WARN, and written again.
func TestAHelperWithNoRecordOfItsBoxesKeepsTheSignInOnTheBoxTraded(t *testing.T) {
	const ada = "ada@example.com"
	t.Run("the first start after a release that kept no record", func(t *testing.T) {
		f := newEnrolBroker(t)
		file := filepath.Join(t.TempDir(), "sessions.json.signin-boxes")
		b, _, _ := machineIdentityBroker(t, f)
		b.SignInBoxesFile = file
		logged := &logtest.Recorder{}
		b.Log = slog.New(logged)
		signIns := &stubSignIns{authority: signedIn(t, f, ada, "credential-of-adas-sign-in")}
		asks, _ := startKeeping(t, b, signIns)
		testwait.WithinPolling(t, 5*time.Second, 5*time.Millisecond, "three asks for the sign-in on the box, with no record of its boxes", func() bool { return asks() >= 3 })
		if messages, people := warnings(logged); len(people) != 1 || people[0] != ada {
			t.Fatalf("the helper's WARN lines %q name %q; want one naming %s, the person whose sign-in it holds", messages, people, ada)
		}
		if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("the record the helper started: %v, %v; want a file only its owner reads", info, err)
		}
		if boxes, held := recordedHolds(t, file); len(boxes) != 0 || len(held) != 1 {
			t.Fatalf("the record holds boxes %q and sign-ins %q; want no box and the one sign-in on the box", boxes, held)
		}

		signIns.mu.Lock()
		signIns.authority = signedIn(t, f, ada, "credential-of-adas-next-sign-in")
		signIns.mu.Unlock()
		asksStop(t, asks, "once the sign-in held for want of a record has left the box")
		if _, held := recordedHolds(t, file); len(held) != 0 {
			t.Fatalf("the record still holds sign-ins %q after the one held left the box", held)
		}
	})
	t.Run("a restart keeps the hold", func(t *testing.T) {
		f := newEnrolBroker(t)
		file := filepath.Join(t.TempDir(), "sessions.json.signin-boxes")
		person := signedIn(t, f, ada, "credential-of-adas-sign-in")
		first, _, _ := machineIdentityBroker(t, f)
		first.SignInBoxesFile = file
		_, stopFirst := startKeeping(t, first, &stubSignIns{authority: person})
		stopFirst()

		again, _, _ := machineIdentityBroker(t, f)
		again.SignInBoxesFile = file
		logged := &logtest.Recorder{}
		again.Log = slog.New(logged)
		asks, _ := startKeeping(t, again, &stubSignIns{authority: person})
		testwait.WithinPolling(t, 5*time.Second, 5*time.Millisecond, "three asks for the sign-in the helper before held", func() bool { return asks() >= 3 })
		if messages, _ := warnings(logged); len(messages) != 0 {
			t.Fatalf("a helper that read the record logged WARN lines %q; want none", messages)
		}
	})
	t.Run("nobody signed in", func(t *testing.T) {
		f := newEnrolBroker(t)
		file := filepath.Join(t.TempDir(), "sessions.json.signin-boxes")
		b, _, _ := machineIdentityBroker(t, f)
		b.SignInBoxesFile = file
		signIns := &stubSignIns{}
		asks, _ := startKeeping(t, b, signIns)
		if boxes, held := recordedHolds(t, file); len(boxes) != 0 || len(held) != 0 {
			t.Fatalf("the record the helper started holds boxes %q and sign-ins %q; want an empty record", boxes, held)
		}
		signIns.mu.Lock()
		signIns.authority = signedIn(t, f, ada, "credential-of-adas-sign-in")
		signIns.mu.Unlock()
		asksStop(t, asks, "for a sign-in made after a start with nobody signed in")
		if n := asks(); n != 0 {
			t.Fatalf("the helper asked for a sign-in made after its start %d times with nothing enrolled; want none", n)
		}
	})
	t.Run("an unreadable record", func(t *testing.T) {
		f := newEnrolBroker(t)
		file := filepath.Join(t.TempDir(), "sessions.json.signin-boxes")
		if err := os.WriteFile(file, []byte("not a record"), 0o600); err != nil {
			t.Fatal(err)
		}
		b, _, _ := machineIdentityBroker(t, f)
		b.SignInBoxesFile = file
		logged := &logtest.Recorder{}
		b.Log = slog.New(logged)
		asks, _ := startKeeping(t, b, &stubSignIns{authority: signedIn(t, f, ada, "credential-of-adas-sign-in")})
		testwait.WithinPolling(t, 5*time.Second, 5*time.Millisecond, "three asks for the sign-in on the box, with a record that cannot be read", func() bool { return asks() >= 3 })
		if messages, people := warnings(logged); len(people) != 1 || people[0] != ada {
			t.Fatalf("the helper's WARN lines %q name %q; want one naming %s", messages, people, ada)
		}
		if _, held := recordedHolds(t, file); len(held) != 1 {
			t.Fatalf("the record written again holds sign-ins %q; want the one on the box", held)
		}
	})
}
