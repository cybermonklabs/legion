package rendering

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
)

const (
	// ListenerPort is the port the listener serves in the task, its default: its webhooks, /v1 and
	// /healthz, and the port the target group sends traffic to.
	ListenerPort = core.DefaultListenerPort
	// ListenerContainerName names the task's one container, which the service attaches to the
	// target group and whose log streams it prefixes.
	ListenerContainerName = "envoy-listener"
)

// Webhook is one webhook a WebhookListener serves: its name in ENVOY_WEBHOOKS, the component input
// holding its secret's ARN, and the variable ECS injects that secret as.
type Webhook struct {
	Name, SecretInput, SecretEnv string
}

// Webhooks are the webhooks this version knows, in the order the container lists their secrets.
var Webhooks = []Webhook{
	{Name: "github", SecretInput: "githubWebhookSecretArn", SecretEnv: "ENVOY_GITHUB_WEBHOOK_SECRET"},
	{Name: "slack", SecretInput: "slackSigningSecretArn", SecretEnv: "ENVOY_SLACK_SIGNING_SECRET"},
}

// supportedWebhooks are the webhooks lists this version takes, the lists whose container
// definitions the reference renders byte for byte: none, which leaves ENVOY_WEBHOOKS unset so the
// listener mounts no webhook route, and github then slack.
var supportedWebhooks = [][]string{{}, {"github", "slack"}}

// WebhookOwner is the webhook named name as the Owner of its inputs: present while webhooks lists
// it.
func WebhookOwner(webhooks []string, name string) Owner {
	return Owner{Name: fmt.Sprintf("webhook %q", name), Present: slices.Contains(webhooks, name), Absent: "which webhooks does not list"}
}

// ListenerEnv holds the inputs of a WebhookListener that its container's non-secret environment
// is made of. The component checks it and writes its Variables into the container definitions;
// `envoy-infra render webhook-listener-env` checks and prints it the same way.
type ListenerEnv struct {
	MachineID      string
	NatsUrls       []string
	ListenHost     string
	NatsReplicas   int
	Webhooks       []string
	ReviewerAppId  *string // written only with github
	MentionTrigger *string // written only with github
	// The sender limit as the component's inputs give it, nil for an input left unset; Check
	// refuses it by core.CheckSenderLimit.
	SenderMessagesPerMinute *int
	SenderMessageBurst      *int
}

// Check refuses what the component refuses of these inputs before it registers anything: every
// one but NatsUrls, which may come from another resource and which Variables refuses. A webhook's
// input is checked against Webhooks before the list itself, so each refusal is the one its input
// earns: a listed webhook's missing input, an unlisted webhook's input, then a list this version
// does not take. The sender limit comes next, by core.CheckSenderLimit. Each refusal is a
// core.InputError.
func (e ListenerEnv) Check() error {
	set := func(v *string) bool { return v != nil && strings.TrimSpace(*v) != "" }
	github := WebhookOwner(e.Webhooks, "github")
	if err := CheckOwnedInputs([]OwnedInput{
		// The listener refuses to start without it.
		{Owner: github, Field: "reviewerAppId", Given: e.ReviewerAppId != nil, Set: set(e.ReviewerAppId)},
		// The listener would fall back to a trigger of its own without it.
		{Owner: github, Field: "mentionTrigger", Given: e.MentionTrigger != nil, Set: set(e.MentionTrigger)},
	}); err != nil {
		return err
	}
	if !slices.ContainsFunc(supportedWebhooks, func(w []string) bool { return slices.Equal(e.Webhooks, w) }) {
		supported := make([]string, len(supportedWebhooks))
		for i, w := range supportedWebhooks {
			supported[i] = fmt.Sprintf("%q", w)
		}
		return core.InputErrorf("webhooks", "webhooks %q is not supported: this version takes %s", e.Webhooks, strings.Join(supported, " or "))
	}
	config, err := e.config()
	if err != nil {
		return err
	}
	if err := config.Check(); err != nil {
		return err
	}
	// The listener binds 127.0.0.1 when ENVOY_LISTEN_HOST is blank, where the target group cannot
	// reach it.
	if strings.TrimSpace(e.ListenHost) == "" {
		return core.InputErrorf("listenHost", "listenHost is required: the listener binds 127.0.0.1 without it")
	}
	if e.NatsReplicas < 1 {
		return core.InputErrorf("natsReplicas", "natsReplicas %d is below 1", e.NatsReplicas)
	}
	// The listener refuses a larger ENVOY_NATS_REPLICAS at load by the same bound
	// (bus.ParseNATSReplicas).
	if e.NatsReplicas > bus.MaxReplicas {
		return core.InputErrorf("natsReplicas", "natsReplicas %d is above %d, the most copies NATS keeps", e.NatsReplicas, bus.MaxReplicas)
	}
	return nil
}

// Variables returns the container's non-secret environment, as NAME=value in the order the
// container definitions list it: core.ListenerConfig.Env's, which writes the listen host after the
// port where the reference lists it and the sender limit only when it is set, then the replicas
// and the webhooks: ENVOY_WEBHOOKS only when one is listed, and the GitHub variables only with
// github. It refuses NatsUrls as Env does, and the sender limit as Check does, since it reads the
// limit through the same rule; it expects the rest to have passed Check, which the component and
// the render command run first, and runs none of Check's other checks itself, so the container
// definitions are written for any webhook list.
func (e ListenerEnv) Variables() ([]string, error) {
	config, err := e.config()
	if err != nil {
		return nil, err
	}
	variables, err := config.Env(e.NatsUrls)
	if err != nil {
		return nil, err
	}
	variables = append(variables, "ENVOY_NATS_REPLICAS="+strconv.Itoa(e.NatsReplicas))
	if len(e.Webhooks) > 0 {
		variables = append(variables, "ENVOY_WEBHOOKS="+strings.Join(e.Webhooks, ","))
	}
	if slices.Contains(e.Webhooks, "github") {
		variables = append(variables,
			"ENVOY_GITHUB_MENTION_TRIGGER="+*e.MentionTrigger,
			"ENVOY_REVIEWER_APP_ID="+*e.ReviewerAppId,
		)
	}
	return variables, nil
}

// config is the environment as core.ListenerConfig takes it, on the listener's fixed port, with
// the sender limit core.CheckSenderLimit answers, or that rule's refusal.
func (e ListenerEnv) config() (core.ListenerConfig, error) {
	rate, burst, err := core.CheckSenderLimit("", e.SenderMessagesPerMinute, e.SenderMessageBurst)
	if err != nil {
		return core.ListenerConfig{}, err
	}
	return core.ListenerConfig{
		MachineID: e.MachineID, Port: ListenerPort, ListenHost: e.ListenHost,
		SenderMessagesPerMinute: rate, SenderMessageBurst: burst,
	}, nil
}

// SecretReadPolicy is the execution-role policy that reads the secret at arn into a container, as
// the reference's json.dumps writes it.
func SecretReadPolicy(arn string) string {
	return core.PyJSON(core.Object{
		{Key: "Version", Value: "2012-10-17"},
		{Key: "Statement", Value: []core.Object{{
			{Key: "Effect", Value: "Allow"},
			{Key: "Action", Value: []string{"secretsmanager:GetSecretValue"}},
			{Key: "Resource", Value: arn},
		}}},
	}, core.PyJSONOptions{EnsureASCII: true})
}

// ListenerContainer holds what a WebhookListener task's container takes from the component's
// inputs and children.
type ListenerContainer struct {
	Image string
	Env   ListenerEnv

	WebhookSecretArns map[string]string // each listed webhook's secret ARN, by its name
	SeedSecretArn     *string           // nil injects no seed

	LogGroup string
	Region   string
}

// ListenerContainerDefinitions returns a WebhookListener task's container definitions, written as
// json.dumps writes the reference's list: its dict order, ensure_ascii on. The environment is the
// Env's Variables, and each webhook's secret is written only with its webhook, from
// WebhookSecretArns; a listed webhook with no ARN there is refused.
func ListenerContainerDefinitions(c ListenerContainer) (string, error) {
	env, err := c.Env.Variables()
	if err != nil {
		return "", err
	}
	variables := make([]core.Object, len(env))
	for i, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		variables[i] = environment(name, value)
	}

	var secrets []core.Object
	for _, w := range Webhooks {
		if !slices.Contains(c.Env.Webhooks, w.Name) {
			continue
		}
		arn, ok := c.WebhookSecretArns[w.Name]
		if !ok {
			return "", fmt.Errorf("webhook %q is listed with no secret ARN", w.Name)
		}
		secrets = append(secrets, secret(w.SecretEnv, arn))
	}
	if c.SeedSecretArn != nil {
		secrets = append(secrets, secret(core.EnvNATSSeed, *c.SeedSecretArn))
	}

	listener := core.Object{
		{Key: "name", Value: ListenerContainerName},
		{Key: "image", Value: c.Image},
		{Key: "essential", Value: true},
		{Key: "command", Value: []string{"/usr/local/bin/envoy-listener"}},
		{Key: "portMappings", Value: []core.Object{portMapping(ListenerPort)}},
		{Key: "environment", Value: variables},
		{Key: "secrets", Value: secrets},
		// `hostname -i` is the task's ENI address in awsvpc mode, the interface the target group
		// probes, so a listener bound to loopback fails here too. It prints every address the
		// name resolves to; the first one keeps a dual-stack ENI from breaking the probe.
		{Key: "healthCheck", Value: healthCheck(
			fmt.Sprintf(`curl -sf "http://$(hostname -i | cut -d' ' -f1):%d/healthz" || exit 1`, ListenerPort), 30)},
		{Key: "logConfiguration", Value: logConfiguration(c.LogGroup, c.Region, ListenerContainerName)},
	}
	return core.PyJSON([]core.Object{listener}, core.PyJSONOptions{EnsureASCII: true}), nil
}
