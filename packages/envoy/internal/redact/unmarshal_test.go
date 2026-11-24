package redact_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/answertest"
)

// TestUnmarshalReadsAnAnswerAsItsData: a caller whose destination takes the answer's text as it
// came — a json.RawMessage it hands back to whoever knows what it means — reads the answer through
// Unmarshal, which fills it; Decode refuses that destination, which is why the function exists. A
// body that does not decode is named in DecodeError's fixed text, naming the destination's type
// and never a value of the body.
func TestUnmarshalReadsAnAnswerAsItsData(t *testing.T) {
	t.Parallel()
	const secret = "not-a-secret-token"
	answer := answertest.Of(200, `{"answers":{"risk":{"legend":{"3":"`+secret+`"}}}}`)
	var into struct {
		Answers map[string]struct {
			Legend json.RawMessage `json:"legend"`
		} `json:"answers"`
	}
	if err := redact.Unmarshal(answer, &into); err != nil {
		t.Fatalf("Unmarshal = %v; want the answer's data read", err)
	}
	if got := string(into.Answers["risk"].Legend); got != `{"3":"`+secret+`"}` {
		t.Errorf("the legend read = %q; want the answer's own bytes", got)
	}
	if err := answer.Decode(&into); err == nil {
		t.Error("Decode took a destination holding the answer's text as it came; Unmarshal is for that destination alone")
	}
}

// TestUnmarshalNamesABodyThatDoesNotDecodeWithoutQuotingIt: a body that is not the JSON the
// destination holds is DecodeError's text — the destination's type and the member at fault, and no
// value of the body, whose every byte is another party's.
func TestUnmarshalNamesABodyThatDoesNotDecodeWithoutQuotingIt(t *testing.T) {
	t.Parallel()
	const value = "not-a-secret-token"
	var into struct {
		Answers map[string]json.RawMessage `json:"answers"`
	}
	err := redact.Unmarshal(answertest.Of(200, `{"answers":"`+value+`"}`), &into)
	if err == nil {
		t.Fatal("Unmarshal read an answer whose answers is a string")
	}
	if strings.Contains(err.Error(), value) {
		t.Errorf("Unmarshal = %q; want no value of the body", err)
	}
	if !strings.Contains(err.Error(), "answers") {
		t.Errorf("Unmarshal = %q; want the member at fault named", err)
	}
}
