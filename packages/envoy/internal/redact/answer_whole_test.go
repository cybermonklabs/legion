package redact_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// digest is a byte array of a name of its own, as a hash or a key a client decodes may be.
type digest [64]byte

// TestAnAnswerRefusesAByteArrayDestinationAsTheBodyWhole: a byte array takes the body whole as
// plain text as a byte slice does: encoding/json fills it from a JSON array of numbers, which can
// spell the answer's text, and fmt prints it as text. So Decode refuses a byte array that is the
// destination itself, named or not and behind however many pointers, as it refuses every other
// destination that takes the body whole (whole), and it decodes nothing into it. Each is given a
// body that spells the refusal's words, the bearer among them, as numbers. The control is
// encoding/json, which fills such an array with the bearer. Captures
// redact.answer-whole-byte-array.
func TestAnAnswerRefusesAByteArrayDestinationAsTheBodyWhole(t *testing.T) {
	numbered := func(auth string) string {
		var numbers []string
		for _, b := range []byte("the token " + auth + " was refused") {
			numbers = append(numbers, strconv.Itoa(int(b)))
		}
		return "[" + strings.Join(numbers, ",") + "]"
	}
	var control [64]byte
	if err := json.Unmarshal([]byte(numbered("Bearer "+redacttest.Token)), &control); err != nil || !strings.Contains(string(control[:]), redacttest.Token) {
		t.Fatalf("control: encoding/json does not fill a byte array with the numbers' text (%v), so this test reads nothing", err)
	}
	answer := answerOf(t, http.StatusOK, numbered)
	var array [64]byte
	var behind *[64]byte
	var named digest
	for name, into := range map[string]any{"array": &array, "behind-pointers": &behind, "named": &named} {
		err := answer.Decode(into)
		if err == nil || !strings.Contains(err.Error(), "which would hold the body whole") {
			t.Errorf("Decode into the %s = %v, want it refused as a destination that would hold the body whole", name, err)
		}
	}
	printed := fmt.Sprintf("%s %s", array[:], named[:])
	if behind != nil {
		printed += " " + string(behind[:])
	}
	redacttest.RequireNone(t, "redact.answer-whole-byte-array", printed)
}
