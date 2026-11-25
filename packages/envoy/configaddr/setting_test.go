package configaddr_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/configaddr"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// probe is a configured address that carries a credential in each place one can sit: its user
// information, a query value and a fragment value. The parameter's name is in two parts so that no
// line of this file reads as a credential to a scanner.
const probe = "https://" + redacttest.User + ":" + redacttest.Password + "@listener.example/path?access" + "_token=" +
	redacttest.Token + "#access" + "_token=" + redacttest.Token

// settings are the settings an Address is read from: the secrets broker's, the flag that names it
// too, and the three addresses Dispatch and the broker call.
var settings = []string{"AGENT_SECRETS_URL", "--broker", "ENVOY_URL", "BROKER_ENVOY_URL", "DISPATCH_AGENT_SECRETS_URL"}

// TestAnAddressNamesTheSettingItWasReadFrom: Setting is the name New was given, and an empty value
// keeps it, as an address that is Empty and prints empty. WordsNotPrinted, which Words and WordsOf
// print beside an address that is not clean, names that setting and not the broker, so a message
// about an Envoy listener's answer does not send its reader to the broker's address; it holds no
// part of the address. The zero Address names no setting and says so in no setting's name.
func TestAnAddressNamesTheSettingItWasReadFrom(t *testing.T) {
	for _, setting := range settings {
		t.Run(setting, func(t *testing.T) {
			addr := configaddr.New(setting, probe)
			if got := addr.Setting(); got != setting {
				t.Errorf("New(%q, the probe).Setting() = %q, want %q", setting, got, setting)
			}
			empty := configaddr.New(setting, "")
			if !empty.Empty() || empty.Raw() != "" || empty.String() != "" || empty.Setting() != setting {
				t.Errorf("New(%q, \"\") reports Empty() %v, Raw() %q, String() %q and Setting() %q; want an empty address that names %s",
					setting, empty.Empty(), empty.Raw(), empty.String(), empty.Setting(), setting)
			}
			want := "(not printed: the address " + setting + " names is not clean)"
			for name, got := range map[string]string{
				"WordsNotPrinted": addr.WordsNotPrinted(),
				"Words":           addr.Words(redacttest.Words),
				"WordsOf":         addr.WordsOf(redact.NewRemoteText(redacttest.Words)),
				"empty.Words":     empty.Words(redacttest.Words),
			} {
				redacttest.RequireNone(t, "configaddr.setting/"+name, got)
				if got != want {
					t.Errorf("%s beside an address %s names that is not clean = %q, want %q", name, setting, got, want)
				}
				if strings.Contains(strings.ReplaceAll(got, setting, ""), "broker") {
					t.Errorf("%s beside an address %s names = %q, which names the broker beside the setting", name, setting, got)
				}
			}
			if addr.WordsNotPrinted() == redact.WordsNotReadable {
				t.Errorf("WordsNotPrinted reads as redact.WordsNotReadable, %q", redact.WordsNotReadable)
			}
		})
	}
	var zero configaddr.Address
	if got := zero.Setting(); got != "" {
		t.Errorf("the zero Address's Setting() = %q, want none", got)
	}
	if got, want := zero.WordsNotPrinted(), "(not printed: the address is not clean)"; got != want {
		t.Errorf("the zero Address's WordsNotPrinted() = %q, want %q", got, want)
	}
}
