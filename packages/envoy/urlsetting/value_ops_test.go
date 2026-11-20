package urlsetting_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/urlsetting"
)

// TestEveryReadFormKeepsTheValueGuarded: a flag (flag.FlagSet.Var), a JSON field and a YAML field
// each read the value as a Value, which keeps it as configured (Raw) and prints it cleaned. The
// control is the plain string field beside it, which prints the credential.
func TestEveryReadFormKeepsTheValueGuarded(t *testing.T) {
	var flagged urlsetting.Value
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Var(&flagged, "url", "a URL")
	if err := fs.Parse([]string{"-url", guardedRaw}); err != nil {
		t.Fatal(err)
	}
	var fromJSON struct {
		URL   urlsetting.Value `json:"url"`
		Plain string           `json:"plain"`
	}
	if err := json.Unmarshal([]byte(`{"url":"`+guardedRaw+`","plain":"`+guardedRaw+`"}`), &fromJSON); err != nil {
		t.Fatal(err)
	}
	var fromYAML struct {
		URL   urlsetting.Value `yaml:"url"`
		Plain string           `yaml:"plain"`
	}
	if err := yaml.Unmarshal([]byte("url: "+guardedRaw+"\nplain: "+guardedRaw+"\n"), &fromYAML); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]urlsetting.Value{"flag": flagged, "json": fromJSON.URL, "yaml": fromYAML.URL} {
		if value.Raw() != guardedRaw {
			t.Errorf("%s: Raw = %v, want the value as configured", name, value)
		}
		if marker := containsMarker(fmt.Sprintf("%v %+v %s %q", value, value, value, value)); marker != "" {
			t.Errorf("%s: the value prints %q", name, marker)
		}
	}
	if containsMarker(fromJSON.Plain) == "" || containsMarker(fromYAML.Plain) == "" {
		t.Error("control: the plain string fields print no credential, so the check cannot see one")
	}
	var usage strings.Builder
	fs.SetOutput(&usage)
	fs.PrintDefaults()
	if marker := containsMarker(usage.String()); marker != "" {
		t.Errorf("the flag's usage prints %q", marker)
	}
	if got := flagged.Type(); got != "url" {
		t.Errorf("Type = %q, want url", got)
	}
}

// TestTheTextOperationsKeepAValue: the operations a reader applies to the text it read keep it a
// Value, as strings would do them, and Equal compares two Values by their text.
func TestTheTextOperationsKeepAValue(t *testing.T) {
	if got := urlsetting.New("  " + guardedRaw + "\n").TrimSpace(); !got.Equal(urlsetting.New(guardedRaw)) {
		t.Errorf("TrimSpace = %v, want the value without its white space", got)
	}
	if got := urlsetting.New("https://example.test///").TrimRight("/"); got.Raw() != "https://example.test" {
		t.Errorf("TrimRight = %v, want no trailing slash", got)
	}
	if got := urlsetting.New(" ").TrimSpace(); !got.Empty() {
		t.Errorf("TrimSpace of white space = %v, want the empty value", got)
	}
	pieces := urlsetting.New("nats://a:1,nats://user:pw@b:2").Split(",")
	if len(pieces) != 2 || pieces[0].Raw() != "nats://a:1" || pieces[1].Raw() != "nats://user:pw@b:2" {
		t.Fatalf("Split = %v, want the two URLs", pieces)
	}
	if got := urlsetting.Join(pieces, ","); got.Raw() != "nats://a:1,nats://user:pw@b:2" {
		t.Errorf("Join = %v, want the list as written", got)
	}
	if strings.Contains(fmt.Sprint(pieces), "pw") {
		t.Errorf("a list of Values prints %v, which holds a password", pieces)
	}
	if got := (urlsetting.Value{}).Split(","); got != nil {
		t.Errorf("Split of the empty value = %v, want none", got)
	}
	if urlsetting.New(guardedRaw).Equal(urlsetting.New(guardedRaw + "x")) {
		t.Error("Equal holds for two different texts")
	}
}

// TestTheAnswerWordsAndRefusalFollowTheValue: Words prints an answer's text beside a clean value
// and WordsNotPrinted beside one that carries a credential; Refuse names a value net/url reads
// apart by its length; Clean says which.
func TestTheAnswerWordsAndRefusalFollowTheValue(t *testing.T) {
	clean := urlsetting.New("https://broker.example")
	if !clean.Clean() || clean.Words("the words") != "the words" {
		t.Errorf("a clean value: Clean = %v, Words = %q", clean.Clean(), clean.Words("the words"))
	}
	credentialed := urlsetting.New(guardedRaw)
	if credentialed.Clean() || credentialed.Words("the words") != urlsetting.WordsNotPrinted {
		t.Errorf("a credentialed value: Clean = %v, Words = %q", credentialed.Clean(), credentialed.Words("the words"))
	}
	misread := urlsetting.New("https://tok/en@host.example")
	err := misread.Refuse("SOME_URL")
	if err == nil || !strings.Contains(err.Error(), "SOME_URL") || strings.Contains(err.Error(), "tok/en") {
		t.Errorf("Refuse of a value net/url reads apart = %v, want a refusal by length", err)
	}
	if err := clean.Refuse("SOME_URL"); err != nil {
		t.Errorf("control: Refuse of a clean value = %v, want nil", err)
	}
}
