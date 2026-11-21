package aws_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	envoyaws "github.com/sjawhar/envoy/infra/aws"
	"github.com/sjawhar/envoy/infra/aws/rendering"
	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/infra/render"
)

const (
	webhookListenerType = "envoy-infra:aws:WebhookListener"
	rolePolicyType      = "aws:iam/rolePolicy:RolePolicy"
	metricAlarmType     = "aws:cloudwatch/metricAlarm:MetricAlarm"

	// listenerURN is the URN of the listener every mock test constructs, named example-listener.
	listenerURN = "urn:pulumi:s::p::" + webhookListenerType + "::example-listener"
	// constructedListenerURN is its URN under Construct, in the integration server's stack and project.
	constructedListenerURN = "urn:pulumi:stack::project::" + webhookListenerType + "::example-listener"

	listenerSecretArn = "arn:aws:secretsmanager:example-region-1:000000000000:secret:example/"
	alarmTopicArn     = "arn:aws:sns:example-region-1:000000000000:example-alarms"
)

// exampleListenerArgs are placeholder inputs. The container definitions' inputs are the goldens':
// the reference's own render from the same values, with a seed, less the shared API token the
// listener no longer takes (t-e-19).
func exampleListenerArgs() *envoyaws.WebhookListenerArgs {
	return &envoyaws.WebhookListenerArgs{
		MachineID: "example-machine",
		NatsUrls:  pulumi.StringArray{pulumi.String("nats://example-bus.example-namespace.internal.example:4222")},
		Image:     "example.registry/envoy@sha256:" + strings.Repeat("0", 64),
		Region:    "example-region-1",

		SeedSecretArn:          pulumi.String(listenerSecretArn + "nats-seed"),
		Webhooks:               []string{"github", "slack"},
		GitHubWebhookSecretArn: pulumi.String(listenerSecretArn + "github-webhook"),
		SlackSigningSecretArn:  pulumi.String(listenerSecretArn + "slack-signing"),
		ReviewerAppId:          pulumi.StringRef("1234567"),

		ClusterArn:        pulumi.String("example-cluster-arn"),
		ExecutionRoleArn:  pulumi.String("example-execution-role-arn"),
		ExecutionRoleName: pulumi.String("example-execution-role"),
		TaskRoleArn:       pulumi.String("example-task-role-arn"),
		SubnetIds:         []pulumi.StringInput{pulumi.String("example-subnet-a"), pulumi.String("example-subnet-b")},
		SecurityGroupId:   pulumi.String("example-listener-sg"),
		TargetGroupArn:    pulumi.String("example-target-group-arn"),

		CloudMapNamespaceId:        pulumi.String("example-namespace-id"),
		CloudMapNamespaceName:      pulumi.String("example-namespace.internal.example"),
		CloudMapServiceName:        "example-hooks",
		CloudMapServiceLogicalName: "example-listener-cloudmap",

		Family:           "example-listener-family",
		LogGroupName:     "/ecs/envoy-listener",
		LogRetentionDays: 7,
		Cpu:              "256",
		Memory:           "512",

		ListenHost:     "0.0.0.0",
		NatsReplicas:   1,
		MentionTrigger: pulumi.StringRef("@legion"),

		AlarmTopicArn:   pulumi.String(alarmTopicArn),
		BaseTags:        map[string]string{"project": "example-project", "env": "example-env"},
		MetricNamespace: "example/EnvoyListener",
		NatsServiceArn:  pulumi.String("example-nats-service-arn"),
	}
}

// noWebhooks makes args a listener that serves no webhook: webhooks an empty list and none of a
// webhook's inputs given. With the seed kept, its container definitions are
// listener-no-webhooks-seed.golden, the reference's own render of the empty list.
func noWebhooks(a *envoyaws.WebhookListenerArgs) {
	a.Webhooks = []string{}
	a.GitHubWebhookSecretArn, a.SlackSigningSecretArn, a.ReviewerAppId, a.MentionTrigger = nil, nil, nil, nil
}

// exampleListenerOutputs are the outputs exampleListenerArgs give, under their schema names: in a
// run under mocks, and in a Construct preview whose inputs are known.
var exampleListenerOutputs = map[string]string{
	"serviceArn":        "arn:example:example-listener-service",
	"serviceName":       "example-listener-service-autonamed",
	"logGroupName":      "/ecs/envoy-listener",
	"taskDefinitionArn": "arn:example:example-listener-task",
	"internalUrl":       "http://example-hooks.example-namespace.internal.example:9020",
}

// runListener runs the listener the component refuses to register (registerWebhookListener, which
// a broker path will build on) as example-listener with args, passing it an aws provider when
// withProvider is set, and returns what the mocks saw, its outputs and the run's error.
func runListener(t *testing.T, args *envoyaws.WebhookListenerArgs, withProvider bool) (*pulumitest.Mocks, map[string]string, error) {
	t.Helper()
	m := newMocks()
	var out map[string]string
	err := m.Run(func(ctx *pulumi.Context) error {
		var opts []pulumi.ResourceOption
		if withProvider {
			prov, err := aws.NewProvider(ctx, "example-aws", &aws.ProviderArgs{Region: pulumi.String("example-region-1")})
			if err != nil {
				return err
			}
			opts = append(opts, pulumi.Providers(prov))
		}
		l, err := envoyaws.RegisterWebhookListener(ctx, "example-listener", args, opts...)
		if err != nil {
			return err
		}
		out, err = awaitOutputs(ctx, map[string]pulumi.StringOutput{
			"serviceArn": l.ServiceArn, "serviceName": l.ServiceName, "logGroupName": l.LogGroupName,
			"taskDefinitionArn": l.TaskDefinitionArn, "internalUrl": l.InternalUrl,
		})
		return err
	})
	return m, out, err
}

// listenerSignals are the listener's log signals as the reference writes them: each alarm and the
// filters counting into its metric, by the part of their logical names after the component's name,
// and each alarm's description for a log group named /ecs/envoy-listener.
var listenerSignals = []struct {
	alarm, metric string
	filters       [][2]string // name suffix, pattern
	description   string
}{
	{
		alarm: "publish-failure", metric: "PublishFailures",
		filters: [][2]string{
			{"publish-failure-filter", `{ $.level = "ERROR" && $.msg = "listener exception publish failed" }`},
			{"webhook-publish-failure-filter", `[date=%^\d{4}/\d{2}/\d{2}$%, time=%^\d{2}:\d{2}:\d{2}$%, source=github||source=slack||source=ghostwispr, verb=publish, result=failed*, ...]`},
		},
		description: "Envoy listener could not publish to JetStream (`listener exception publish failed` or `<webhook> publish failed`, e.g. `nats: no response from stream`) — a delivery from the Dispatch GitHub App (legion-envoy) waits for Dispatch's redelivery sweep, a Slack event is retried by Slack three times with backoff and lost only if every attempt fails, any other GitHub sender's delivery answered 503 is lost, and delivery-exception notices are lost. A GitHub delivery is counted at its first failure in each listener task; a later failure of it is logged `redelivery <delivery>: github publish failed` and not counted. Read /ecs/envoy-listener for the nats error.",
	},
	{
		alarm: "webhook-refused", metric: "WebhookRefusals",
		filters: [][2]string{
			{"webhook-publish-refused-filter", `[date=%^\d{4}/\d{2}/\d{2}$%, time=%^\d{2}:\d{2}:\d{2}$%, source=github||source=slack||source=ghostwispr, verb=publish, result=refused*, ...]`},
			{"ci-record-refused-filter", `[date=%^\d{4}/\d{2}/\d{2}$%, time=%^\d{2}:\d{2}:\d{2}$%, source=github, w1=ci, w2=record, result=refused*, ...]`},
			{"ci-head-suite-record-refused-filter", `[date=%^\d{4}/\d{2}/\d{2}$%, time=%^\d{2}:\d{2}:\d{2}$%, source=github, w1=ci, w2=head||w2=suite, w3=record, result=refused*, ...]`},
		},
		description: "Envoy listener refused a webhook delivery (`<source> publish refused`, `github ci record refused` or `github ci suite record refused`, answered 422): the envelope is past what NATS takes or the commit's CI record is past its bound, and the same input is refused on every attempt. A GitHub delivery is counted at its first refusal in each listener task: Dispatch's redelivery sweep asks for a failed delivery again for 72 hours, and each later refusal of it is logged `redelivery <delivery>: <action> refused` and not counted. A delivery refused before a fix deploys is recovered once the fix lands within those 72 hours; past them it is lost. A refused CI record leaves its commit overflowed, never settling, whatever is redelivered. Read /ecs/envoy-listener for the refusal's reason; the fix is a cap in legion's normalizer or a CI bound, not the NATS server's limits.",
	},
	{
		alarm: "reconnect-hooks-stalled", metric: "ReconnectHooksStalled",
		filters: [][2]string{
			{"reconnect-hooks-stalled-filter", `[date=%^\d{4}/\d{2}/\d{2}$%, time=%^\d{2}:\d{2}:\d{2}$%, level=ERROR, w1=envoy, w2=nats, w3=reconnect, w4=hooks, w5=have, w6=not, w7=succeeded, ...]`},
		},
		description: "An Envoy listener's KV watch rebuild has not succeeded for a minute after a NATS reconnect (`envoy nats reconnect hooks have not succeeded`): its interest, role-claim, session and CI caches no longer follow their buckets and serve what they last held, and `/healthz` reports this as 200 `degraded` naming `KV watch rebuild` — deliberately not a 503, which would replace the only task into the same failing rebuild with empty caches. The bus keeps retrying the rebuild with backoff. Read the line's `error` in /ecs/envoy-listener for the failing bucket read: typically a JetStream that refuses or does not answer consumer creation for the KV streams.",
	},
	{
		alarm: "stream-subject-dropped", metric: "StreamSubjectsDropped",
		filters: [][2]string{
			{"stream-subject-dropped-filter", `[date=%^\d{4}/\d{2}/\d{2}$%, time=%^\d{2}:\d{2}:\d{2}$%, level=WARN, w1=envoy, w2=nats, w3=stream, w4=subject, w5=replaced, w6=by, w7=an, w8=overlapping, w9=one, ...]`},
		},
		description: "An Envoy listener start dropped another deployment's `ENVOY_NOTIFICATIONS` subject because it overlaps one of its own (`envoy nats stream subject replaced by an overlapping one`) — that deployment receives nothing on the dropped subject until it starts again; read /ecs/envoy-listener for the dropped and kept subjects, then reconcile the subject lists across the deployments.",
	},
	{
		alarm: "durable-refused", metric: "DurableRefusals",
		filters: [][2]string{
			{"durable-refused-filter", `{ $.level = "ERROR" && $.msg = "subscribe refused, shutting down" }`},
		},
		description: "An Envoy listener start refused its durable consumer (`subscribe refused, shutting down`), so every replacement task exits 1 while the old task keeps serving, until the circuit breaker rolls the deployment back and the apply fails. Read the line's `error` in /ecs/envoy-listener: a durable carrying an idle heartbeat or an ack policy other than explicit, which NATS cannot change in place, is recreated by the recipe in cybermonklabs/legion packages/envoy/deploy/README.md (Recreating a listener's durable), which keeps its cursor; `bus: NATS denied the publish to \"<subject>\"` is the listener's NATS grant refusing the durable's read, create or update, which the grant must allow.",
	},
	{
		alarm: "subscribe-exhausted", metric: "SubscribesExhausted",
		filters: [][2]string{
			{"subscribe-exhausted-filter", `{ $.level = "ERROR" && $.msg = "subscribe failed after max attempts, shutting down" }`},
		},
		description: "An Envoy listener start gave up setting up its durable subscription 135 s after its first attempt (`subscribe failed after max attempts, shutting down`). ECS had already stopped the old task, so every replacement exits 1 the same way once its own bind gives up. Read the line's `error` in /ecs/envoy-listener: `consumer is already bound to a subscription` means something still holds the durable's push binding (find it with `nats consumer info ENVOY_NOTIFICATIONS listener-<machine id>`, push_bound, and stop it); any other error is JetStream failing the consumer lookup, create or update.",
	},
}

// readPolicy is the execution-role policy that reads one secret, as the reference's json.dumps
// writes it.
func readPolicy(arn string) string {
	return `{"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Action": ["secretsmanager:GetSecretValue"], "Resource": "` + arn + `"}]}`
}

// exampleListenerChildren are the children exampleListenerArgs register, with the seed when seed is
// set: the reference's resources with their inputs, each named from the component's name as the
// reference names it from envoy-listener.
func exampleListenerChildren(t *testing.T, seed bool) map[string]child {
	if seed {
		return listenerChildren(t, "listener-github-slack-seed.golden", true)
	}
	return listenerChildren(t, "listener-github-slack-no-seed.golden", false)
}

// listenerChildren are exampleListenerChildren with the task definition's container definitions
// the golden named golden: the webhooks list changes nothing else a listener registers.
func listenerChildren(t *testing.T, golden string, seed bool) map[string]child {
	children := map[string]child{
		"example-listener-log-group": {logGroupType, map[string]any{
			"name": "/ecs/envoy-listener", "retentionInDays": float64(7), "tags": tags("example-listener"),
		}},
		"example-listener-cloudmap": {cloudMapServiceType, map[string]any{
			"name":        "example-hooks",
			"namespaceId": "example-namespace-id",
			"dnsConfig": map[string]any{
				"namespaceId":   "example-namespace-id",
				"routingPolicy": "MULTIVALUE",
				"dnsRecords":    []any{map[string]any{"type": "A", "ttl": float64(10)}},
			},
			"tags": tags("example-listener"),
		}},
		"example-listener-task": {taskDefinitionType, map[string]any{
			"family":                  "example-listener-family",
			"cpu":                     "256",
			"memory":                  "512",
			"networkMode":             "awsvpc",
			"requiresCompatibilities": []any{"FARGATE"},
			"executionRoleArn":        "example-execution-role-arn",
			"taskRoleArn":             "example-task-role-arn",
			"containerDefinitions":    readGolden(t, golden),
			"tags":                    tags("example-listener"),
		}},
		"example-listener-service": {ecsServiceType, map[string]any{
			"cluster":                         "example-cluster-arn",
			"taskDefinition":                  "arn:example:example-listener-task",
			"desiredCount":                    float64(1),
			"launchType":                      "FARGATE",
			"availabilityZoneRebalancing":     "DISABLED",
			"deploymentMaximumPercent":        float64(200),
			"deploymentMinimumHealthyPercent": float64(100),
			"deploymentCircuitBreaker":        map[string]any{"enable": true, "rollback": true},
			"waitForSteadyState":              true,
			"healthCheckGracePeriodSeconds":   float64(120),
			"enableExecuteCommand":            true,
			"networkConfiguration": map[string]any{
				"subnets":        []any{"example-subnet-a", "example-subnet-b"},
				"securityGroups": []any{"example-listener-sg"},
				"assignPublicIp": false,
			},
			"loadBalancers": []any{map[string]any{
				"targetGroupArn": "example-target-group-arn", "containerName": "envoy-listener", "containerPort": float64(9020),
			}},
			"serviceRegistries": map[string]any{"registryArn": "arn:example:example-listener-cloudmap"},
			"tags":              tags("example-listener"),
		}},
	}
	if seed {
		children["example-listener-execution-nats-nkey-seed-read"] = child{rolePolicyType, map[string]any{
			"role": "example-execution-role", "policy": readPolicy(listenerSecretArn + "nats-seed"),
		}}
	}
	for _, s := range listenerSignals {
		for _, f := range s.filters {
			children["example-listener-"+f[0]] = child{metricFilterType, map[string]any{
				"logGroupName": "/ecs/envoy-listener",
				"pattern":      f[1],
				"metricTransformation": map[string]any{
					"name": s.metric, "namespace": "example/EnvoyListener", "value": "1", "defaultValue": "0",
				},
			}}
		}
		children["example-listener-"+s.alarm] = child{metricAlarmType, map[string]any{
			"alarmDescription":   s.description,
			"namespace":          "example/EnvoyListener",
			"metricName":         s.metric,
			"statistic":          "Sum",
			"period":             float64(300),
			"evaluationPeriods":  float64(1),
			"threshold":          float64(1),
			"comparisonOperator": "GreaterThanOrEqualToThreshold",
			"treatMissingData":   "notBreaching",
			"alarmActions":       []any{alarmTopicArn},
			"okActions":          []any{alarmTopicArn},
			"tags":               tags("example-listener"),
		}}
	}
	return children
}

func TestWebhookListenerRegistersTheReferenceChildren(t *testing.T) {
	for _, row := range []struct {
		name, golden string
		seed         bool
		mutate       func(*envoyaws.WebhookListenerArgs)
	}{
		{"seed true", "listener-github-slack-seed.golden", true, func(*envoyaws.WebhookListenerArgs) {}},
		{"seed false", "listener-github-slack-no-seed.golden", false, func(a *envoyaws.WebhookListenerArgs) { a.SeedSecretArn = nil }},
		{"no webhooks", "listener-no-webhooks-seed.golden", true, noWebhooks},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := exampleListenerArgs()
			row.mutate(args)
			m, out, err := runListener(t, args, true)
			require.NoError(t, err)

			require.Equal(t, "example-listener", m.One(t, webhookListenerType).Name)
			children := childrenOf(t, m)
			requireChildren(t, listenerChildren(t, row.golden, row.seed), children)
			for name, c := range children {
				require.Equal(t, listenerURN, c.RPC.GetParent(), "%s's parent is the component", name)
				require.Contains(t, c.RPC.GetProvider(), providerURN+"::", "%s uses the aws provider the component was given", name)
				require.Empty(t, c.RPC.GetAliases(), "%s is adopted from nothing", name)
			}
			require.Equal(t, exampleListenerOutputs, out)
		})
	}
}

// The service tag is derived from the component's name, as the reference derives it from
// envoy-listener; the role policies and the metric filters carry no tags.
func TestWebhookListenerTagsEachChildWithItsService(t *testing.T) {
	m, _, err := runListener(t, exampleListenerArgs(), true)
	require.NoError(t, err)

	tagged := 0
	for name, c := range childrenOf(t, m) {
		switch c.Type {
		case rolePolicyType, metricFilterType:
			require.NotContains(t, c.Inputs, resource.PropertyKey("tags"), "%s carries no tags", name)
		default:
			require.Equal(t, tags("example-listener"), c.Inputs["tags"].Mappable(), "%s's tags", name)
			tagged++
		}
	}
	require.Equal(t, 10, tagged, "the log group, Cloud Map service, task definition, service and six alarms")
}

// The task definition comes after the policy its execution role reads the seed with, and the
// service's create and update wait up to fifteen minutes for its rollout.
func TestWebhookListenerOrdersTheTaskDefinitionAfterItsPolicies(t *testing.T) {
	m, _, err := runListener(t, exampleListenerArgs(), true)
	require.NoError(t, err)
	require.Contains(t, m.One(t, taskDefinitionType).RPC.GetDependencies(),
		"urn:pulumi:s::p::"+webhookListenerType+"$"+rolePolicyType+"::example-listener-execution-nats-nkey-seed-read")
}

func TestWebhookListenerServiceWaitsFifteenMinutes(t *testing.T) {
	m, _, err := runListener(t, exampleListenerArgs(), true)
	require.NoError(t, err)
	timeouts := m.One(t, ecsServiceType).RPC.GetCustomTimeouts()
	require.Equal(t, "15m", timeouts.GetCreate())
	require.Equal(t, "15m", timeouts.GetUpdate())
	require.Empty(t, timeouts.GetDelete())
}

// With adopt, each child takes over the root-level resource of its own name.
func TestWebhookListenerAdoptAliasesEachChildToItsRootLevelName(t *testing.T) {
	args := exampleListenerArgs()
	args.Adopt = true
	m, _, err := runListener(t, args, true)
	require.NoError(t, err)
	requireAdopted(t, exampleListenerChildren(t, true), childrenOf(t, m))
}

// A listener that serves no webhook carries none of a webhook's settings or secrets: no
// ENVOY_WEBHOOKS, which the listener reads as no route to mount, no ENVOY_GITHUB_MENTION_TRIGGER
// or ENVOY_REVIEWER_APP_ID, and neither signing secret. Its seed stays.
func TestWebhookListenerWithNoWebhooksWritesNoWebhookSetting(t *testing.T) {
	args := exampleListenerArgs()
	noWebhooks(args)
	m, _, err := runListener(t, args, true)
	require.NoError(t, err)

	type entry struct {
		Name string `json:"name"`
	}
	var containers []struct {
		Environment []entry `json:"environment"`
		Secrets     []entry `json:"secrets"`
	}
	require.NoError(t, json.Unmarshal([]byte(m.One(t, taskDefinitionType).Inputs["containerDefinitions"].StringValue()), &containers))
	require.Len(t, containers, 1)
	names := func(entries []entry) []string {
		var out []string
		for _, e := range entries {
			out = append(out, e.Name)
		}
		return out
	}
	require.Equal(t, []string{
		"PORT", "ENVOY_LISTEN_HOST", "ENVOY_MACHINE_ID", "NATS_URLS", "ENVOY_ALLOW_REMOTE_NATS", "ENVOY_NATS_REPLICAS",
	}, names(containers[0].Environment))
	require.Equal(t, []string{"NATS_NKEY_SEED"}, names(containers[0].Secrets))
}

// The alarms' descriptions send the reader to the log group the filters read, whatever its name.
func TestWebhookListenerAlarmsNameItsLogGroup(t *testing.T) {
	args := exampleListenerArgs()
	args.LogGroupName = "/ecs/example-hooks"
	m, _, err := runListener(t, args, true)
	require.NoError(t, err)

	alarms := 0
	for name, c := range childrenOf(t, m) {
		switch c.Type {
		case metricAlarmType:
			description := c.Inputs["alarmDescription"].StringValue()
			require.Contains(t, description, "/ecs/example-hooks", "%s's description", name)
			require.NotContains(t, description, "/ecs/envoy-listener", "%s's description", name)
			alarms++
		case metricFilterType:
			require.Equal(t, "/ecs/example-hooks", c.Inputs["logGroupName"].StringValue(), "%s's log group", name)
		}
	}
	require.Equal(t, 6, alarms)
}

// natsUrls may be known only in the apply, so core.ListenerConfig.Env refuses a bad one there and
// the refusal reaches the consumer as the run's error.
func TestWebhookListenerRefusesWhatTheListenerEnvironmentRefuses(t *testing.T) {
	args := exampleListenerArgs()
	args.NatsUrls = pulumi.StringArray{pulumi.String("nats://a.internal.example:4222,nats://b.internal.example:4222")}
	_, _, err := runListener(t, args, true)
	require.ErrorContains(t, err, "natsUrls[0] contains a comma")
}

// An optional ARN the consumer gives but that resolves to nothing is refused, naming its field,
// rather than leaving the container without it or naming an empty secret.
func TestWebhookListenerRefusesAGivenArnThatResolvesToNoValue(t *testing.T) {
	for _, row := range []struct {
		name  string
		value func(*string) *string
	}{
		{"nil", func(*string) *string { return nil }},
		{"empty", func(*string) *string { return pulumi.StringRef("") }},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := exampleListenerArgs()
			args.SeedSecretArn = pulumi.String(listenerSecretArn + "nats-seed").ToStringPtrOutput().
				ApplyT(row.value).(pulumi.StringPtrOutput)
			_, _, err := runListener(t, args, true)
			require.ErrorContains(t, err, "envoy-infra:aws:WebhookListener example-listener: seedSecretArn resolved to no value")
		})
	}
}

func TestWebhookListenerRefusesWithoutAnAWSProviderBeforeRegistering(t *testing.T) {
	m, _, err := runListener(t, exampleListenerArgs(), false)
	require.ErrorContains(t, err, `envoy-infra:aws:WebhookListener needs a "aws" provider in its providers option`)
	require.Empty(t, m.Types())
}

// The listener binds off loopback behind its target group and relays agent sessions, and a session
// presents a broker token, which the listener verifies only by introspecting it at the secrets
// broker as a platform identity the broker knows. An ECS task has none, and the shared API token
// that admitted every caller is gone (t-e-19), so the component refuses every configuration at
// preview, before it registers anything, naming that limit: the old full configuration, a minimal
// one, and one validate would refuse, so the limit is the refusal whatever else is wrong. Both of
// the plugin's paths are held: a Go program's own call under preview, and Construct, under preview
// and in an update, with every input from another resource and, under preview, with every input
// unknown, as a program in another language sends them. Its args carry no apiTokenSecretArn and no
// apiTokenSecretVersionId, so no schema built from them offers either. Negative control, run by
// hand: NewWebhookListener calling registerWebhookListener in place of its refusal; the old full
// configuration then previews, and its rows fail naming the missing refusal.
func TestTheAWSWebhookListenerRefusesWithoutABrokerVerifier(t *testing.T) {
	const refusal = "envoy-infra:aws:WebhookListener example-listener: " +
		"aws:WebhookListener cannot verify a session's broker token; deploy a listener with broker settings"
	minimal := func() *envoyaws.WebhookListenerArgs {
		a := exampleListenerArgs()
		noWebhooks(a)
		a.SeedSecretArn = nil
		return a
	}
	invalid := func() *envoyaws.WebhookListenerArgs {
		a := exampleListenerArgs()
		a.SubnetIds = nil
		return a
	}
	for _, row := range []struct {
		name string
		args func() *envoyaws.WebhookListenerArgs
	}{
		{"the old full configuration", exampleListenerArgs},
		{"a minimal configuration", minimal},
		{"a configuration validate refuses", invalid},
	} {
		t.Run(row.name+" in a program's preview", func(t *testing.T) {
			m := newMocks()
			m.Preview = true
			err := m.Run(func(ctx *pulumi.Context) error {
				prov, err := aws.NewProvider(ctx, "example-aws", &aws.ProviderArgs{Region: pulumi.String("example-region-1")})
				if err != nil {
					return err
				}
				_, err = envoyaws.NewWebhookListener(ctx, "example-listener", row.args(), pulumi.Providers(prov))
				return err
			})
			require.ErrorContains(t, err, refusal, "the preview is not refused naming the limit")
			require.Equal(t, []string{awsProviderType}, m.Types(), "nothing but the provider is registered")
		})
		for _, construction := range []struct {
			name   string
			input  func(string) property.Value
			dryRun bool
		}{
			{"through Construct's preview", fromUpstream, true},
			{"through Construct's update", fromUpstream, false},
			{"through Construct's preview with every input unknown", unknownFromUpstream, true},
		} {
			t.Run(row.name+" "+construction.name, func(t *testing.T) {
				in := listenerInputsOf(t, row.args(), construction.input)
				m, _, err := construct(t, webhookListenerType, "example-listener", in, construction.dryRun)
				require.ErrorContains(t, err, refusal, "the configuration is not refused naming the limit")
				require.Empty(t, m.Types())
			})
		}
	}
	args := reflect.TypeFor[envoyaws.WebhookListenerArgs]()
	for i := range args.NumField() {
		name, _, _ := strings.Cut(args.Field(i).Tag.Get("pulumi"), ",")
		require.NotContains(t, []string{"apiTokenSecretArn", "apiTokenSecretVersionId"}, name, "the args still carry the shared token's input")
	}
}

// validate refuses each input NewWebhookListener cannot build from. The table runs against validate
// directly and holds the component's own refusals: the webhooks list itself, each webhook's secret
// ARN, and the service's. rendering.ListenerEnv.Check's refusals are tested in rendering; one row
// shows that validate runs it. TestWebhookListenerRefusesAnInvalidInputBeforeRegistering shows that
// the constructor runs validate first and prefixes the component's name.
func TestWebhookListenerValidate(t *testing.T) {
	for _, row := range []struct {
		name   string
		mutate func(*envoyaws.WebhookListenerArgs)
		err    string
	}{
		// webhooks itself is required: a listener with no webhook gives [], and an omitted list is
		// never read as one.
		{
			name: "webhooks omitted",
			mutate: func(a *envoyaws.WebhookListenerArgs) {
				noWebhooks(a)
				a.Webhooks = nil
			},
			err: `webhooks is required: give [] for a listener with no webhook`,
		},
		// A listed webhook's secret.
		{
			name:   "github without its secret",
			mutate: func(a *envoyaws.WebhookListenerArgs) { a.GitHubWebhookSecretArn = nil },
			err:    `githubWebhookSecretArn is required with webhook "github"`,
		},
		{
			name:   "slack without its secret",
			mutate: func(a *envoyaws.WebhookListenerArgs) { a.SlackSigningSecretArn = nil },
			err:    `slackSigningSecretArn is required with webhook "slack"`,
		},
		// A secret for a webhook that is not listed, which would otherwise be dropped silently.
		{
			name: "a github secret with github unlisted",
			mutate: func(a *envoyaws.WebhookListenerArgs) {
				a.Webhooks, a.ReviewerAppId, a.MentionTrigger = []string{"slack"}, nil, nil
			},
			err: `githubWebhookSecretArn is given for webhook "github", which webhooks does not list`,
		},
		{
			name:   "a slack secret with slack unlisted",
			mutate: func(a *envoyaws.WebhookListenerArgs) { a.Webhooks = []string{"github"} },
			err:    `slackSigningSecretArn is given for webhook "slack", which webhooks does not list`,
		},
		{
			name: "a github secret with no webhook",
			mutate: func(a *envoyaws.WebhookListenerArgs) {
				noWebhooks(a)
				a.GitHubWebhookSecretArn = pulumi.String(listenerSecretArn + "github-webhook")
			},
			err: `githubWebhookSecretArn is given for webhook "github", which webhooks does not list`,
		},
		{
			name: "a slack secret with no webhook",
			mutate: func(a *envoyaws.WebhookListenerArgs) {
				noWebhooks(a)
				a.SlackSigningSecretArn = pulumi.String(listenerSecretArn + "slack-signing")
			},
			err: `slackSigningSecretArn is given for webhook "slack", which webhooks does not list`,
		},
		// The environment, through rendering.ListenerEnv.Check.
		{
			name: "a webhooks list this version does not take",
			mutate: func(a *envoyaws.WebhookListenerArgs) {
				a.Webhooks, a.SlackSigningSecretArn = []string{"github"}, nil
			},
			err: `webhooks ["github"] is not supported: this version takes [] or ["github" "slack"]`,
		},
		// The service.
		{
			name:   "no subnet",
			mutate: func(a *envoyaws.WebhookListenerArgs) { a.SubnetIds = nil },
			err:    "subnetIds names no subnet",
		},
		{
			name:   "base tags holding service",
			mutate: func(a *envoyaws.WebhookListenerArgs) { a.BaseTags["service"] = "example" },
			err:    `baseTags holds "service"`,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := exampleListenerArgs()
			row.mutate(args)
			require.ErrorContains(t, args.Validate(), row.err)
		})
	}
	require.NoError(t, exampleListenerArgs().Validate())
}

func TestWebhookListenerRefusesAnInvalidInputBeforeRegistering(t *testing.T) {
	args := exampleListenerArgs()
	args.SubnetIds = nil
	m, _, err := runListener(t, args, true)
	require.ErrorContains(t, err, "envoy-infra:aws:WebhookListener example-listener: subnetIds names no subnet")
	require.Equal(t, []string{awsProviderType}, m.Types(), "nothing but the provider is registered")
}

// The sender limit reaches the listener container's environment in the task definition exactly as
// it is given, each variable there when its input is set and absent when it is not. A limit
// core.CheckSenderLimit refuses stops the constructor with the InputError naming the input, which
// validate returns through rendering.ListenerEnv.Check, before anything but the provider
// registers.
func TestWebhookListenerSenderMessageLimit(t *testing.T) {
	for _, row := range []struct {
		name        string
		rate, burst *int
		want        []string
	}{
		{"no limit", nil, nil, nil},
		{"a rate alone", pulumi.IntRef(4), nil, []string{"ENVOY_SENDER_MESSAGES_PER_MINUTE=4"}},
		{"a rate and a burst", pulumi.IntRef(4), pulumi.IntRef(8), []string{"ENVOY_SENDER_MESSAGES_PER_MINUTE=4", "ENVOY_SENDER_MESSAGE_BURST=8"}},
	} {
		t.Run("aws:WebhookListener renders "+row.name, func(t *testing.T) {
			args := exampleListenerArgs()
			args.SenderMessagesPerMinute, args.SenderMessageBurst = row.rate, row.burst
			m, _, err := runListener(t, args, true)
			require.NoError(t, err)
			var containers []struct {
				Environment []struct{ Name, Value string } `json:"environment"`
			}
			require.NoError(t, json.Unmarshal([]byte(m.One(t, taskDefinitionType).Inputs["containerDefinitions"].StringValue()), &containers))
			require.Len(t, containers, 1)
			var got []string
			for _, e := range containers[0].Environment {
				if strings.HasPrefix(e.Name, "ENVOY_SENDER_") {
					got = append(got, e.Name+"="+e.Value)
				}
			}
			require.Equal(t, row.want, got)
		})
	}
	for _, row := range []struct {
		name        string
		rate, burst *int
		input       string
	}{
		{"a rate of 0", pulumi.IntRef(0), nil, "senderMessagesPerMinute"},
		{"a rate of 60001", pulumi.IntRef(60001), nil, "senderMessagesPerMinute"},
		{"a burst of 0", pulumi.IntRef(4), pulumi.IntRef(0), "senderMessageBurst"},
		{"a burst with no rate", nil, pulumi.IntRef(8), "senderMessageBurst"},
	} {
		t.Run("aws:WebhookListener refuses "+row.name, func(t *testing.T) {
			args := exampleListenerArgs()
			args.SenderMessagesPerMinute, args.SenderMessageBurst = row.rate, row.burst
			var refused *core.InputError
			require.ErrorAs(t, args.Validate(), &refused)
			require.Equal(t, row.input, refused.Input)

			m, _, err := runListener(t, args, true)
			require.ErrorContains(t, err, "envoy-infra:aws:WebhookListener example-listener: "+row.input+" ")
			require.Equal(t, []string{awsProviderType}, m.Types(), "nothing but the provider is registered")
		})
	}
}

// without returns text with each part removed, requiring each to occur in it exactly once.
func without(t *testing.T, text string, parts ...string) string {
	t.Helper()
	for _, part := range parts {
		require.Equal(t, 1, strings.Count(text, part), "%q occurs once", part)
		text = strings.Replace(text, part, "", 1)
	}
	return text
}

// ENVOY_WEBHOOKS is written only when a webhook is listed, the GitHub variables only with github,
// and each webhook's secret only with its webhook. The component takes no webhook or github and
// slack, which the goldens pin through it; github or slack alone reaches only the container
// definitions, so this test calls them directly, on listener-github-slack-no-seed.golden's inputs.
// Every row gives both webhooks' ARNs, so what writes a secret is its webhook being listed.
func TestListenerContainerDefinitionsWriteEachWebhookOnlyWhenListed(t *testing.T) {
	golden := readGolden(t, "listener-github-slack-no-seed.golden")
	const (
		webhooks      = `{"name": "ENVOY_WEBHOOKS", "value": "github,slack"}`
		gitHubEnv     = `, {"name": "ENVOY_GITHUB_MENTION_TRIGGER", "value": "@legion"}, {"name": "ENVOY_REVIEWER_APP_ID", "value": "1234567"}`
		gitHubSecret  = `{"name": "ENVOY_GITHUB_WEBHOOK_SECRET", "valueFrom": "` + listenerSecretArn + `github-webhook"}`
		slackSecret   = `{"name": "ENVOY_SLACK_SIGNING_SECRET", "valueFrom": "` + listenerSecretArn + `slack-signing"}`
		gitHubOnlyVar = `{"name": "ENVOY_WEBHOOKS", "value": "github"}`
		slackOnlyVar  = `{"name": "ENVOY_WEBHOOKS", "value": "slack"}`
	)
	container := func(webhooks []string, arns map[string]string) rendering.ListenerContainer {
		appID, trigger := "1234567", "@legion"
		return rendering.ListenerContainer{
			Image: "example.registry/envoy@sha256:" + strings.Repeat("0", 64),
			Env: rendering.ListenerEnv{
				MachineID:      "example-machine",
				NatsUrls:       []string{"nats://example-bus.example-namespace.internal.example:4222"},
				ListenHost:     "0.0.0.0",
				NatsReplicas:   1,
				Webhooks:       webhooks,
				ReviewerAppId:  &appID,
				MentionTrigger: &trigger,
			},
			WebhookSecretArns: arns,
			LogGroup:          "/ecs/envoy-listener",
			Region:            "example-region-1",
		}
	}
	both := map[string]string{"github": listenerSecretArn + "github-webhook", "slack": listenerSecretArn + "slack-signing"}
	for _, row := range []struct {
		name     string
		webhooks []string
		want     string
	}{
		{"github", []string{"github"}, strings.Replace(without(t, golden, ", "+slackSecret), webhooks, gitHubOnlyVar, 1)},
		{"slack", []string{"slack"}, strings.Replace(without(t, golden, gitHubEnv, gitHubSecret+", "), webhooks, slackOnlyVar, 1)},
		{"none", []string{}, without(t, golden, ", "+webhooks, gitHubEnv, gitHubSecret+", "+slackSecret)},
	} {
		t.Run(row.name, func(t *testing.T) {
			definitions, err := rendering.ListenerContainerDefinitions(container(row.webhooks, both))
			require.NoError(t, err)
			require.Equal(t, row.want, definitions)
		})
	}
	t.Run("a listed webhook with no secret ARN", func(t *testing.T) {
		_, err := rendering.ListenerContainerDefinitions(container([]string{"github", "slack"}, map[string]string{"github": both["github"]}))
		require.EqualError(t, err, `webhook "slack" is listed with no secret ARN`)
	})
}

// `envoy-infra render webhook-listener-env` prints the environment the listener behind the
// component's refusal (registerWebhookListener) registers in its container, in its order, and
// `render metric-filters --component webhook-listener` the log metric filters it registers, for the
// same inputs. The render package holds each output against hand-derived values; this holds it
// against the registration, which only this package reaches, since the component refuses every
// configuration (t-e-19).
func TestRenderPrintsWhatTheListenerBehindTheRefusalRegisters(t *testing.T) {
	for _, row := range []struct {
		name        string
		natsUrls    []string
		listenHost  string
		replicas    int
		webhooks    []string
		rate, burst *int
	}{
		{"github and slack", []string{"nats://example-bus.example-namespace.internal.example:4222"}, "0.0.0.0", 1, []string{"github", "slack"}, nil, nil},
		{"no webhook and two loopback NATS servers", []string{"nats://127.0.0.1:4222", "nats://localhost:4223"}, "::", 3, []string{}, nil, nil},
		{"a sender limit", []string{"nats://example-bus.example-namespace.internal.example:4222"}, "0.0.0.0", 1, []string{"github", "slack"}, pulumi.IntRef(4), pulumi.IntRef(8)},
	} {
		t.Run("webhook-listener-env with "+row.name, func(t *testing.T) {
			args := exampleListenerArgs()
			if len(row.webhooks) == 0 {
				noWebhooks(args)
			}
			args.NatsUrls = pulumi.ToStringArray(row.natsUrls)
			args.ListenHost, args.NatsReplicas = row.listenHost, row.replicas
			args.SenderMessagesPerMinute, args.SenderMessageBurst = row.rate, row.burst
			m, _, err := runListener(t, args, true)
			require.NoError(t, err)
			var containers []struct {
				Environment []struct{ Name, Value string } `json:"environment"`
			}
			require.NoError(t, json.Unmarshal([]byte(m.One(t, taskDefinitionType).Inputs["containerDefinitions"].StringValue()), &containers))
			require.Len(t, containers, 1)
			var registered strings.Builder
			for _, e := range containers[0].Environment {
				registered.WriteString(e.Name + "=" + e.Value + "\n")
			}

			flags := []string{"webhook-listener-env", "--machine-id", args.MachineID}
			for _, u := range row.natsUrls {
				flags = append(flags, "--nats-url", u)
			}
			for _, w := range row.webhooks {
				flags = append(flags, "--webhook", w)
			}
			if len(row.webhooks) > 0 {
				flags = append(flags, "--reviewer-app-id", *args.ReviewerAppId, "--mention-trigger", *args.MentionTrigger)
			}
			flags = append(flags, "--listen-host", row.listenHost, "--nats-replicas", strconv.Itoa(row.replicas))
			if row.rate != nil {
				flags = append(flags, "--sender-messages-per-minute", strconv.Itoa(*row.rate))
			}
			if row.burst != nil {
				flags = append(flags, "--sender-message-burst", strconv.Itoa(*row.burst))
			}
			var stdout, stderr bytes.Buffer
			require.Equal(t, 0, render.Command(flags, &stdout, &stderr), stderr.String())
			require.Equal(t, registered.String(), stdout.String())
		})
	}
	t.Run("metric-filters for webhook-listener", func(t *testing.T) {
		m, _, err := runListener(t, exampleListenerArgs(), true)
		require.NoError(t, err)
		type filter struct {
			LogicalName     string `json:"logicalName"`
			Pattern         string `json:"pattern"`
			MetricName      string `json:"metricName"`
			MetricNamespace string `json:"metricNamespace"`
		}
		var registered []filter
		for name, c := range childrenOf(t, m) {
			if c.Type != metricFilterType {
				continue
			}
			transformation := c.Inputs["metricTransformation"].ObjectValue()
			registered = append(registered, filter{
				LogicalName: name, Pattern: c.Inputs["pattern"].StringValue(),
				MetricName: transformation["name"].StringValue(), MetricNamespace: transformation["namespace"].StringValue(),
			})
		}
		require.Len(t, registered, 9)
		var stdout, stderr bytes.Buffer
		require.Equal(t, 0, render.Command([]string{"metric-filters", "--component", "webhook-listener",
			"--metric-namespace", "example/EnvoyListener", "--logical-name-prefix", "example-listener", "--json"}, &stdout, &stderr), stderr.String())
		var rendered []filter
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &rendered))
		require.ElementsMatch(t, registered, rendered)
	})
}

// The ordering input comes from a resource of its own, so a test can tell its dependencies from
// the other Input fields'.
const natsService = resource.URN("urn:pulumi:s::p::envoy-infra:aws:NatsService$aws:ecs/service:Service::nats-service")

// listenerInputs are exampleListenerArgs as a consumer sends them to Construct (listenerInputsOf).
func listenerInputs(t *testing.T, input func(string) property.Value) map[string]property.Value {
	t.Helper()
	return listenerInputsOf(t, exampleListenerArgs(), input)
}

// listenerInputsOf are args as a consumer sends them to Construct, each Input field made a value by
// input, the ordering input natsServiceArn carrying its own resource's dependencies.
func listenerInputsOf(t *testing.T, args *envoyaws.WebhookListenerArgs, input func(string) property.Value) map[string]property.Value {
	t.Helper()
	in := inputs(t, args, input)
	in["natsServiceArn"] = property.New(literal(t, args.NatsServiceArn)).WithDependencies([]resource.URN{natsService})
	return in
}

// A plain field is for a literal: Construct refuses one whose input carries a dependency, before
// the component registers anything.
func TestWebhookListenerConstructRefusesAPlainFieldFromAnotherResource(t *testing.T) {
	in := listenerInputs(t, fromUpstream)
	in["image"] = fromUpstream(exampleListenerArgs().Image)
	m, _, err := construct(t, webhookListenerType, "example-listener", in, false)
	require.ErrorContains(t, err, "image")
	require.Empty(t, m.Types())
}
