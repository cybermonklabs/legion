package projectaccess_test

import (
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
)

// TestTheAnswerTellsAnEmptyChannelListFromAMissingOne: Dispatch's answer carries slack_channels
// always, empty for a project whose owner named none (dispatch://INFRA-3862's #t-a-31), and a
// Dispatch from before that task carries no such key at all. The two read differently, so the
// broker can fail closed on the second rather than take it as "no limit".
func TestTheAnswerTellsAnEmptyChannelListFromAMissingOne(t *testing.T) {
	server := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		"NAMED": {Visibility: "private", Owner: "carol@example.com", SlackChannels: []string{"C0123456789", "G9876543210"}},
		"EMPTY": {Visibility: "private", Owner: "carol@example.com"},
		"OLD":   {Visibility: "private", Owner: "carol@example.com", OmitSlackChannels: true},
	}, nil)
	client := projectaccess.New(server.URL, server.Account, nil)
	for _, c := range []struct {
		project  string
		channels []string
		present  bool
	}{
		{"NAMED", []string{"C0123456789", "G9876543210"}, true},
		{"EMPTY", []string{}, true},
		{"OLD", nil, false},
	} {
		answer, err := client.Access(t.Context(), c.project, "carol@example.com")
		if err != nil {
			t.Fatalf("Access(%s): %v", c.project, err)
		}
		if !reflect.DeepEqual(answer.SlackChannels, c.channels) || answer.SlackChannelsPresent != c.present {
			t.Errorf("Access(%s) carries channels %#v present=%v, want %#v present=%v",
				c.project, answer.SlackChannels, answer.SlackChannelsPresent, c.channels, c.present)
		}
	}
}
