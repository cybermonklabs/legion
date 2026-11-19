package main

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
)

// The model services' endpoints, DISPATCH_AZURE_EMBED_ENDPOINT, DISPATCH_AZURE_CHAT_ENDPOINT and
// DISPATCH_CHAT_ENDPOINT, are each refused at boot, named by its length and the cause, when an @
// is left once net/url takes out its user part: a token written with a raw / as the whole user part
// (https://<a>/<b>@host) is otherwise read with <a> as the host, which the client then asks for and
// the boot's one line of the model names in its identity (<place>:<host>/<model>), and an @ in the
// path is refused with the same cause, since net/url leaves it outside the user part too. The
// control is the same value with that character percent-encoded, which boots, and so does the
// endpoint with no user part. The token's halves are made for this test.
func TestBootRefusesAModelEndpointWithAnAtLeftOverByItsLength(t *testing.T) {
	const (
		first  = "not-a-secret-tok5c8e"
		second = "not-a-secret-tok9d1a"
	)
	for _, endpoint := range []struct {
		setting, host string
		environment   func(map[string]string) map[string]string
	}{
		{"DISPATCH_AZURE_EMBED_ENDPOINT", "embed.example", azureEmbedEnvironment},
		{"DISPATCH_AZURE_CHAT_ENDPOINT", "chat.example", azureChatEnvironment},
		{"DISPATCH_CHAT_ENDPOINT", "chat.example", endpointChatEnvironment},
	} {
		environment := func(value string) settingValues {
			return envGetter(endpoint.environment(map[string]string{endpoint.setting: value}))
		}
		if _, err := resolveBootConfig(environment("https://" + endpoint.host + "/v1")); err != nil {
			t.Fatalf("the control %s with no user part: %v, want it admitted", endpoint.setting, err)
		}
		for _, form := range []struct{ name, value, encoded string }{
			{"a / in its user part", "https://" + first + "/" + second + "@" + endpoint.host + "/v1", "https://" + first + "%2F" + second + "@" + endpoint.host + "/v1"},
			{"an @ in its path and no user part", "https://" + endpoint.host + "/v1/" + first + "@" + second, "https://" + endpoint.host + "/v1/" + first + "%40" + second},
		} {
			t.Run(endpoint.setting+" with "+form.name, func(t *testing.T) {
				_, err := resolveBootConfig(environment(form.value))
				if err == nil {
					parsed, _ := url.Parse(form.value)
					t.Fatalf("admitted, and the boot's line of the model names the host net/url read from it, %q", parsed.Host)
				}
				want := fmt.Sprintf("%s, a value of %d characters, %s", endpoint.setting, len(form.value), redact.AtOutsideUserPart)
				if err.Error() != want {
					t.Errorf("err = %v, want %q", err, want)
				}
				if strings.Contains(err.Error(), first) || strings.Contains(err.Error(), second) {
					t.Errorf("the refusal holds a part of the token: %q", err)
				}
				if _, err := resolveBootConfig(environment(form.encoded)); err != nil {
					t.Errorf("the control %s: %v, want it admitted", form.encoded, err)
				}
			})
		}
	}
}
