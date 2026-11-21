package natscred_test

import (
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/natscred"
)

// A service's reply prefix is one subject token naming its identity, each byte outside
// [A-Za-z0-9-] written '_' and two hex digits, so two identities never share one and nats.go
// takes it as an inbox prefix.
func TestInboxPrefixIsOneTokenOfTheIdentity(t *testing.T) {
	for identity, want := range map[string]string{
		"system:serviceaccount:legion-pub:legion-pub-daemon": "_INBOX_system_3aserviceaccount_3alegion-pub_3alegion-pub-daemon",
		"daemon@example.iam.gserviceaccount.com":             "_INBOX_daemon_40example_2eiam_2egserviceaccount_2ecom",
		"a.b*c>d e\tf":                                       "_INBOX_a_2eb_2ac_3ed_20e_09f",
		"a_b":                                                "_INBOX_a_5fb",
		"a.b":                                                "_INBOX_a_2eb",
		"Az09_-":                                             "_INBOX_Az09_5f-",
	} {
		got := natscred.InboxPrefix(identity)
		require.Equal(t, want, got, "InboxPrefix(%q)", identity)
		options := nats.GetDefaultOptions()
		require.NoError(t, nats.CustomInboxPrefix(got)(&options), "nats.go takes InboxPrefix(%q)", identity)
	}
}
