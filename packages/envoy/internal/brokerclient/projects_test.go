// packages/envoy/internal/brokerclient/projects_test.go

package brokerclient

import (
	"context"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// TestTradeProjectsReadsAgentSecretsProjects: the projects a session names are
// AGENT_SECRETS_PROJECTS in the grammar the broker reads (oauthwire.ParseProjects): blank is
// none, and a value that is no list is an error naming the variable rather than a trade that
// quietly names fewer projects than its session asked for.
func TestTradeProjectsReadsAgentSecretsProjects(t *testing.T) {
	// The spelling a deployment sets, written out rather than read off ProjectsVar, so a renamed
	// constant fails here instead of reading a variable nobody sets.
	const variable = "AGENT_SECRETS_PROJECTS"
	read := func(value string) ([]string, error) {
		return TradeProjects(func(name string) string {
			if name == variable {
				return value
			}
			return ""
		})
	}
	for value, want := range map[string][]string{"": nil, "  ": nil, "PRIV": {"PRIV"}, "PUB PRIV": {"PRIV", "PUB"}} {
		got, err := read(value)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("TradeProjects(%s=%q) = %v, %v; want %v", variable, value, got, err, want)
		}
	}
	for _, bad := range []string{"priv", "PRIV,PUB", "PRIV PRIV"} {
		if got, err := read(bad); err == nil || got != nil || !strings.Contains(err.Error(), variable) {
			t.Errorf("TradeProjects(%s=%q) = %v, %v; want no list and an error naming the variable", variable, bad, got, err)
		}
	}
}

// TestATradeSendsTheProjectsItWasGiven: a client given projects sends them, joined as the broker
// reads them, and a client given none sends no projects parameter at all, so every trade that
// names none reads exactly as it did before the parameter existed.
func TestATradeSendsTheProjectsItWasGiven(t *testing.T) {
	for _, c := range []struct {
		projects []string
		want     []string
	}{
		{projects: []string{"PRIV", "PUB"}, want: []string{"PRIV PUB"}},
		{projects: nil, want: nil},
	} {
		var form url.Values
		broker := newStubBroker(t, func(_ int, got url.Values) (int, map[string]any) {
			form = got
			return 200, map[string]any{"access_token": "a-token", "token_type": "DPoP", "expires_in": 600}
		})
		client, err := New(Config{
			BrokerURL: brokeraddr.New(broker.URL()), Key: testKey(t), Session: "ada-box-1", Projects: c.projects,
			Subject: &scriptedSource{token: "person.token", tokenType: oauthwire.SubjectTokenTypePerson, describe: "a person's sign-in"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Token(context.Background()); err != nil {
			t.Fatalf("Token: %v", err)
		}
		if got := form[oauthwire.FormProjects]; !reflect.DeepEqual(got, c.want) {
			t.Errorf("a client given %v sent %s=%v, want %v", c.projects, oauthwire.FormProjects, got, c.want)
		}
	}
}

// TestNewRefusesProjectsThatAreNoList: a client is built only with a list the broker would take,
// so a caller that built one from somewhere other than TradeProjects cannot send it a list it
// refuses at every trade.
func TestNewRefusesProjectsThatAreNoList(t *testing.T) {
	_, err := New(Config{
		BrokerURL: brokeraddr.New("http://broker.test"), Key: testKey(t), Projects: []string{"PUB", "PRIV"},
		Subject: &scriptedSource{token: "t", tokenType: oauthwire.SubjectTokenTypePerson, describe: "a person's sign-in"},
	})
	if err == nil {
		t.Fatal("New with an unsorted list = nil error, want a refusal")
	}
}
