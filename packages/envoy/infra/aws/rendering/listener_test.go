package rendering_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/aws/rendering"
	"github.com/sjawhar/envoy/infra/core"
)

// listenerEnv is a ListenerEnv that Check passes, with both webhooks.
func listenerEnv() rendering.ListenerEnv {
	appID, trigger := "1234567", "@legion"
	return rendering.ListenerEnv{
		MachineID:      "example-machine",
		ListenHost:     "0.0.0.0",
		NatsReplicas:   1,
		Webhooks:       []string{"github", "slack"},
		ReviewerAppId:  &appID,
		MentionTrigger: &trigger,
	}
}

// withoutGitHub drops github's own inputs, as a listener that does not list github gives none.
func withoutGitHub(e *rendering.ListenerEnv) {
	e.ReviewerAppId, e.VerdictAppId, e.MentionTrigger = nil, nil, nil
}

// Check refuses each value the listener cannot run on, naming its input. This is the one table of
// these refusals: aws:WebhookListener tests that its validate runs Check, and `envoy-infra render
// webhook-listener-env` the flag each refused input maps to. core.ListenerConfig.Check's own
// refusals are core's; one row here shows that Check runs it.
func TestListenerEnvCheck(t *testing.T) {
	appID := func(v string) *string { return &v }
	intRef := func(v int) *int { return &v }
	for _, row := range []struct {
		name   string
		mutate func(*rendering.ListenerEnv)
		input  string // the input a refusal names
		err    string
	}{
		{name: "github and slack", mutate: func(*rendering.ListenerEnv) {}},
		{name: "no webhook", mutate: func(e *rendering.ListenerEnv) { withoutGitHub(e); e.Webhooks = []string{} }},
		// The webhooks lists this version takes: none, or github then slack.
		{
			name:   "github alone",
			mutate: func(e *rendering.ListenerEnv) { e.Webhooks = []string{"github"} },
			input:  "webhooks", err: `webhooks ["github"] is not supported: this version takes [] or ["github" "slack"]`,
		},
		{
			name:   "slack alone",
			mutate: func(e *rendering.ListenerEnv) { withoutGitHub(e); e.Webhooks = []string{"slack"} },
			input:  "webhooks", err: `webhooks ["slack"] is not supported: this version takes [] or ["github" "slack"]`,
		},
		{
			name:   "both in the other order",
			mutate: func(e *rendering.ListenerEnv) { e.Webhooks = []string{"slack", "github"} },
			input:  "webhooks", err: `webhooks ["slack" "github"] is not supported: this version takes [] or ["github" "slack"]`,
		},
		{
			name:   "a webhook this version does not know",
			mutate: func(e *rendering.ListenerEnv) { e.Webhooks = []string{"github", "slack", "ghostwispr"} },
			input:  "webhooks", err: `webhooks ["github" "slack" "ghostwispr"] is not supported`,
		},
		// github's inputs while github is listed.
		{
			name:   "github without reviewerAppId",
			mutate: func(e *rendering.ListenerEnv) { e.ReviewerAppId = nil },
			input:  "reviewerAppId", err: `reviewerAppId is required with webhook "github"`,
		},
		{
			name:   "github with a blank reviewerAppId",
			mutate: func(e *rendering.ListenerEnv) { e.ReviewerAppId = appID(" ") },
			input:  "reviewerAppId", err: `reviewerAppId is required with webhook "github"`,
		},
		{
			name:   "github without mentionTrigger",
			mutate: func(e *rendering.ListenerEnv) { e.MentionTrigger = nil },
			input:  "mentionTrigger", err: `mentionTrigger is required with webhook "github"`,
		},
		// The verdict App's id, which github takes and does not require.
		{name: "github with a verdictAppId", mutate: func(e *rendering.ListenerEnv) { e.VerdictAppId = appID("5341240") }},
		{
			name:   "github with a blank verdictAppId",
			mutate: func(e *rendering.ListenerEnv) { e.VerdictAppId = appID(" ") },
			input:  "verdictAppId", err: "verdictAppId is blank: give the verdict App's id, or leave it out",
		},
		{
			name:   "github with a verdictAppId that is not a number",
			mutate: func(e *rendering.ListenerEnv) { e.VerdictAppId = appID("not-a-number") },
			input:  "verdictAppId", err: `verdictAppId "not-a-number" is not a GitHub App id: a whole number above 0, with no leading zero`,
		},
		{
			name:   "github with a verdictAppId with a leading zero",
			mutate: func(e *rendering.ListenerEnv) { e.VerdictAppId = appID("05341240") },
			input:  "verdictAppId", err: `verdictAppId "05341240" is not a GitHub App id`,
		},
		{
			name: "a verdictAppId with github unlisted",
			mutate: func(e *rendering.ListenerEnv) {
				withoutGitHub(e)
				e.Webhooks, e.VerdictAppId = []string{"slack"}, appID("5341240")
			},
			input: "verdictAppId", err: `verdictAppId is given for webhook "github", which webhooks does not list`,
		},
		// github's inputs while github is not listed, which would otherwise be dropped silently.
		{
			name:   "a reviewerAppId with github unlisted",
			mutate: func(e *rendering.ListenerEnv) { e.Webhooks, e.MentionTrigger = []string{"slack"}, nil },
			input:  "reviewerAppId", err: `reviewerAppId is given for webhook "github", which webhooks does not list`,
		},
		{
			name:   "a mentionTrigger with github unlisted",
			mutate: func(e *rendering.ListenerEnv) { e.Webhooks, e.ReviewerAppId = []string{"slack"}, nil },
			input:  "mentionTrigger", err: `mentionTrigger is given for webhook "github", which webhooks does not list`,
		},
		{
			name:   "a reviewerAppId with no webhook",
			mutate: func(e *rendering.ListenerEnv) { e.Webhooks, e.MentionTrigger = []string{}, nil },
			input:  "reviewerAppId", err: `reviewerAppId is given for webhook "github", which webhooks does not list`,
		},
		{
			name:   "a mentionTrigger with no webhook",
			mutate: func(e *rendering.ListenerEnv) { e.Webhooks, e.ReviewerAppId = []string{}, nil },
			input:  "mentionTrigger", err: `mentionTrigger is given for webhook "github", which webhooks does not list`,
		},
		// The rest of the environment.
		{
			name:   "a machineId with a dot, refused by core.ListenerConfig.Check",
			mutate: func(e *rendering.ListenerEnv) { e.MachineID = "example.host" },
			input:  "machineId",
			err:    "machineId contains '.', which the listener's NATS names or the hub's grant entries cannot carry",
		},
		{
			name:   "a blank listenHost",
			mutate: func(e *rendering.ListenerEnv) { e.ListenHost = " " },
			input:  "listenHost", err: "listenHost is required: the listener binds 127.0.0.1 without it",
		},
		{
			name:   "no NATS replica",
			mutate: func(e *rendering.ListenerEnv) { e.NatsReplicas = 0 },
			input:  "natsReplicas", err: "natsReplicas 0 is below 1",
		},
		{
			name:   "more NATS replicas than NATS keeps",
			mutate: func(e *rendering.ListenerEnv) { e.NatsReplicas = 6 },
			input:  "natsReplicas", err: "natsReplicas 6 is above 5, the most copies NATS keeps",
		},
		{
			name:   "five NATS replicas",
			mutate: func(e *rendering.ListenerEnv) { e.NatsReplicas = 5 },
		},
		// The sender limit, core.CheckSenderLimit's rule, whose own refusals are core's.
		{
			name:   "a sender limit",
			mutate: func(e *rendering.ListenerEnv) { e.SenderMessagesPerMinute, e.SenderMessageBurst = intRef(4), intRef(8) },
		},
		{
			name:   "a sender burst with no rate, refused by core.CheckSenderLimit",
			mutate: func(e *rendering.ListenerEnv) { e.SenderMessageBurst = intRef(8) },
			input:  "senderMessageBurst", err: "senderMessageBurst is given without senderMessagesPerMinute",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			e := listenerEnv()
			row.mutate(&e)
			err := e.Check()
			if row.err == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, row.err)
			var refused *core.InputError
			require.True(t, errors.As(err, &refused), "%v is a core.InputError", err)
			require.Equal(t, row.input, refused.Input)
		})
	}
}

// The verdict App's id is written as ENVOY_VERDICT_APP_ID, after the other GitHub variables, only
// when it is given: a listener given none writes the environment it wrote before the input.
func TestListenerEnvWritesTheVerdictAppIdOnlyWhenGiven(t *testing.T) {
	e := listenerEnv()
	e.NatsUrls = []string{"nats://example-bus.example-namespace.internal.example:4222"}
	without, err := e.Variables()
	require.NoError(t, err)
	require.NotContains(t, strings.Join(without, "\n"), "ENVOY_VERDICT_APP_ID")

	verdictApp := "5341240"
	e.VerdictAppId = &verdictApp
	with, err := e.Variables()
	require.NoError(t, err)
	require.Equal(t, append(without, "ENVOY_VERDICT_APP_ID=5341240"), with)
}
