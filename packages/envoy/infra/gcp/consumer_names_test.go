package gcp_test

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

// consumerNamesRenderings are the consumer-name settings a component takes, each with the
// variable its creators' environments then hold: none when it is unset, where a creator runs
// fixed. The name is written out here rather than taken from core, so a rename is seen in what the
// creators read.
var consumerNamesRenderings = []struct {
	name, value string
	want        []string
}{
	{"unset", "", nil},
	{"fixed", "fixed", []string{"ENVOY_CONSUMER_NAMES=fixed"}},
	{"fresh", "fresh", []string{"ENVOY_CONSUMER_NAMES=fresh"}},
}

// consumerNamesRefusals are settings every component refuses before it registers anything, since
// a creator refuses each at its start: case and surrounding whitespace count, and a list is no
// mode.
var consumerNamesRefusals = []string{"Fresh", "fresh ", "on", "fixed,fresh"}

// consumerNamesVars is the consumer-name variable a container's environment holds, NAME=value, or
// none.
func consumerNamesVars(env map[string]any) []string {
	if value, ok := env["ENVOY_CONSUMER_NAMES"]; ok {
		return []string{"ENVOY_CONSUMER_NAMES=" + value.(string)}
	}
	return nil
}

// withoutConsumerNames is template with ENVOY_CONSUMER_NAMES taken out of every container's
// environment, so a template rendered with the setting compares with one rendered without it on
// everything else; a container with no environment is left as it is. It edits template, which
// deploymentPodTemplate builds afresh on each call.
func withoutConsumerNames(template map[string]any) map[string]any {
	for _, item := range template["spec"].(map[string]any)["containers"].([]any) {
		container := item.(map[string]any)
		env, ok := container["env"].([]any)
		if !ok {
			continue
		}
		kept := []any{}
		for _, variable := range env {
			if variable.(map[string]any)["name"] != "ENVOY_CONSUMER_NAMES" {
				kept = append(kept, variable)
			}
		}
		container["env"] = kept
	}
	return template
}

// renderedObjects is every registration of m, its inputs with every output and secret wrapper off,
// keyed by type and name, since the SDK registers from goroutines of its own and the order is not
// the render's.
func renderedObjects(m *pulumitest.Mocks) map[string]any {
	objects := map[string]any{}
	for _, r := range m.Registrations() {
		objects[r.Type+"::"+r.Name] = normalizedProperty(resource.NewObjectProperty(r.Inputs))
	}
	return objects
}

// heldDeployment is a Deployment's rendered inputs as the writer hold's update rule changes them:
// the annotation that keeps pulumi from waiting for its rollout on the Deployment's own metadata,
// and the Recreate strategy, which carries no rollingUpdate block, in place of whatever it had.
// Nothing else differs, its pod template least of all, so the change rolls no pod by itself.
func heldDeployment(t *testing.T, rendered any) map[string]any {
	t.Helper()
	held := maps.Clone(rendered.(map[string]any))
	metadata := maps.Clone(held["metadata"].(map[string]any))
	require.NotContains(t, metadata, "annotations", "the Deployment carried no annotation before the hold")
	metadata["annotations"] = map[string]any{"pulumi.com/skipAwait": "true"}
	held["metadata"] = metadata
	spec := maps.Clone(held["spec"].(map[string]any))
	spec["strategy"] = map[string]any{"type": "Recreate"}
	held["spec"] = spec
	return held
}

// requireRecreatedWithNoRollingUpdate requires the Deployment key of rendered to update by
// Recreate and to carry no rollingUpdate block at all, which the API server refuses beside
// Recreate ("may not be specified when strategy type is 'Recreate'").
func requireRecreatedWithNoRollingUpdate(t *testing.T, rendered map[string]any, key string) {
	t.Helper()
	strategy := rendered[key].(map[string]any)["spec"].(map[string]any)["strategy"].(map[string]any)
	require.Equal(t, "Recreate", strategy["type"], key)
	require.NotContains(t, strategy, "rollingUpdate", "%s renders no rollingUpdate under Recreate", key)
}

// creatorDeployments renders the Deployments of m named names, each without its pod template,
// whose goldens requirePodGolden holds, as one indented JSON object keyed by name: what the writer
// hold's update rule may change.
func creatorDeployments(t *testing.T, m *pulumitest.Mocks, names ...string) string {
	t.Helper()
	deployments := map[string]any{}
	for _, name := range names {
		deployment := normalizedProperty(resource.NewObjectProperty(m.Named(t, deploymentType, name).Inputs)).(map[string]any)
		spec := maps.Clone(deployment["spec"].(map[string]any))
		delete(spec, "template")
		deployment["spec"] = spec
		deployments[name] = deployment
	}
	b, err := json.MarshalIndent(deployments, "", "  ")
	require.NoError(t, err)
	return string(b) + "\n"
}

// clusteredListenerPlaces are the two places gcp:ClusteredNatsHub's session listener is declared
// at, each with the path its refusals name: inside the tailnet entry, and as the hub's own input,
// which requires a credential of every caller (ownListener sets one).
var clusteredListenerPlaces = []struct {
	path string
	at   func(*gcp.ClusteredNatsHubArgs) *gcp.ClusteredHubListenerArgs
}{
	{"tailnet.listener", listener},
	{"listener", ownListener},
}

// gcp:ClusteredNatsHub renders its session listener's consumer-name setting into the listener
// container's environment exactly as it is given, wherever the listener is declared, and changes
// nothing else of the pod; unset, it renders no variable, so the pod is what it was before the
// input existed (clustered-hub-listener-pod.golden, and testdata/production-render.golden byte for
// byte). A setting the creators refuse stops the constructor with the InputError naming the
// listener's input, quoting the value, before anything registers.
func TestClusteredNatsHubListenerConsumerNames(t *testing.T) {
	for _, place := range clusteredListenerPlaces {
		args := clusteredArgs()
		place.at(args)
		base := deploymentPodTemplate(t, runClusteredHub(t, args), clusteredListenerName)
		for _, row := range consumerNamesRenderings {
			t.Run(place.path+" renders "+row.name, func(t *testing.T) {
				args := clusteredArgs()
				place.at(args).ConsumerNames = row.value
				template := deploymentPodTemplate(t, runClusteredHub(t, args), clusteredListenerName)
				require.Equal(t, row.want, consumerNamesVars(containerEnv(t, podContainer(t, template, "listener"))))
				require.Equal(t, base, withoutConsumerNames(template), "the setting changes nothing else of the pod")
			})
		}
		for _, value := range consumerNamesRefusals {
			t.Run(place.path+" refuses "+value, func(t *testing.T) {
				args := clusteredArgs()
				place.at(args).ConsumerNames = value
				_, err := gcp.ResolveClusteredNatsHub(args)
				var refused *core.InputError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, place.path+".consumerNames", refused.Input)
				require.ErrorContains(t, err, place.path+`.consumerNames "`+value+`" must be "fixed" or "fresh"`)

				m, err := runClustered(t, "example-clustered-hub", args, nil)
				require.ErrorContains(t, err, "envoy-infra:gcp:ClusteredNatsHub example-clustered-hub: "+place.path+".consumerNames ")
				require.Empty(t, m.Types(), "nothing registered")
			})
		}
	}
}

// gcp:Dispatch renders its consumer-name setting into both creators' environments, the app's
// envoy-dispatch and the listener's envoy-listener, exactly as it is given, and changes nothing
// else of either pod or of the front door's, which creates no consumer; unset, it renders no
// variable (dispatch-pod.golden, dispatch-listener-pod.golden, and
// testdata/production-render.golden byte for byte). A setting the creators refuse stops the
// constructor with the InputError naming the input, quoting the value, before anything but the
// test's providers registers.
func TestDispatchConsumerNames(t *testing.T) {
	base, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	for _, row := range consumerNamesRenderings {
		t.Run("renders "+row.name, func(t *testing.T) {
			args := dispatchArgs()
			args.ConsumerNames = row.value
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)
			require.Equal(t, row.want, consumerNamesVars(appEnv(t, m)), "the app")
			require.Equal(t, row.want, consumerNamesVars(listenerEnv(t, m)), "the listener")
			for _, deployment := range []string{"example-dispatch", "example-dispatch-listener", "example-dispatch-tailnet"} {
				require.Equal(t, deploymentPodTemplate(t, base, deployment), withoutConsumerNames(deploymentPodTemplate(t, m, deployment)),
					"the setting changes nothing else of %s's pod", deployment)
			}
		})
	}
	for _, value := range consumerNamesRefusals {
		t.Run("refuses "+value, func(t *testing.T) {
			args := dispatchArgs()
			args.ConsumerNames = value
			_, err := gcp.ResolveDispatch(args)
			var refused *core.InputError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, "consumerNames", refused.Input)
			require.ErrorContains(t, err, `consumerNames "`+value+`" must be "fixed" or "fresh"`)

			m, err := runDispatch(t, args, true)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: consumerNames ")
			require.ElementsMatch(t, []string{gcpProviderType, randomProviderType, tailscaleProviderType}, m.Types(),
				"nothing registered beyond the test's providers")
		})
	}
}

// With replaceUnderHold, gcp:ClusteredNatsHub's session listener Deployment, which runs one pod
// and otherwise surges the new pod in before the old one stops, is recreated, so the old pod stops
// before the new one starts, and carries pulumi.com/skipAwait, so the apply does not wait for a
// pod whose startup probe cannot pass while the hold refuses its NATS connection. Nothing else of
// it changes, nor any other object the hub renders, the tailnet hosts' Deployment among them.
// replaceUnderHold false renders every object as it renders with the input unset.
func TestClusteredNatsHubListenerReplaceUnderHold(t *testing.T) {
	listenerKey := deploymentType + "::" + clusteredListenerName
	for _, place := range clusteredListenerPlaces {
		args := clusteredArgs()
		place.at(args)
		base := renderedObjects(runClusteredHub(t, args))
		require.Contains(t, base, listenerKey)
		require.Equal(t, map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxUnavailable": 0.0, "maxSurge": 1.0}},
			base[listenerKey].(map[string]any)["spec"].(map[string]any)["strategy"], "the listener rolls outside the hold")
		t.Run(place.path+" held", func(t *testing.T) {
			args := clusteredArgs()
			place.at(args).ReplaceUnderHold = true
			held := renderedObjects(runClusteredHub(t, args))
			want := maps.Clone(base)
			want[listenerKey] = heldDeployment(t, base[listenerKey])
			require.Equal(t, want, held)
			requireRecreatedWithNoRollingUpdate(t, held, listenerKey)
		})
		t.Run(place.path+" not held", func(t *testing.T) {
			args := clusteredArgs()
			place.at(args).ReplaceUnderHold = false
			require.Equal(t, base, renderedObjects(runClusteredHub(t, args)))
		})
	}
}

// With replaceUnderHold, gcp:Dispatch's two consumer creators, the app and the listener beside
// it, are recreated and carry pulumi.com/skipAwait: the old pod stops before the new one starts,
// and the apply does not wait for a pod whose readiness the hold's refusal of its NATS connection
// can hold back. At one replica each, as both run by default, each is recreated already, so only
// the annotation changes; above one, where each would otherwise roll and keep old pods serving
// beside the new, each is recreated too. The front door, which creates no consumer, keeps its own
// strategy and waits as before, and nothing else of any object changes. replaceUnderHold false
// renders every object as it renders with the input unset.
func TestDispatchReplaceUnderHold(t *testing.T) {
	creators := []string{deploymentType + "::example-dispatch", deploymentType + "::example-dispatch-listener"}
	for _, row := range []struct {
		name     string
		replicas int
	}{{"at one replica", 1}, {"at two replicas", 2}} {
		scaled := func() *gcp.DispatchArgs {
			args := dispatchArgs()
			args.Replicas, args.ListenerReplicas = pulumi.IntRef(row.replicas), pulumi.IntRef(row.replicas)
			return args
		}
		baseMocks, err := runDispatch(t, scaled(), true)
		require.NoError(t, err)
		base := renderedObjects(baseMocks)
		t.Run(row.name+" held", func(t *testing.T) {
			args := scaled()
			args.ReplaceUnderHold = true
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)
			want := maps.Clone(base)
			for _, key := range creators {
				require.Contains(t, base, key)
				want[key] = heldDeployment(t, base[key])
			}
			held := renderedObjects(m)
			require.Equal(t, want, held)
			for _, key := range creators {
				requireRecreatedWithNoRollingUpdate(t, held, key)
			}
		})
		t.Run(row.name+" not held", func(t *testing.T) {
			args := scaled()
			args.ReplaceUnderHold = false
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)
			require.Equal(t, base, renderedObjects(m))
		})
	}
}

// The writer hold's update rule, pinned in goldens: the creators' Deployments, without their pod
// templates. Without replaceUnderHold (unset or false, one Go value) they render as main rendered
// them before the input existed, which dispatch-creators-at-2-replicas.golden and
// clustered-hub-listener-deployment.golden hold, each written from main's render: Dispatch's app
// and listener at two replicas each roll, as #2404's standby listener pod would run them, and the
// hub's listener surges its one pod. With it each is recreated, with no rollingUpdate block, and
// carries pulumi.com/skipAwait, whatever its replica count. The two goldens of a component differ,
// so a render that ignored the input fails one row.
func TestTheWriterHoldsUpdateRuleMatchesItsGoldens(t *testing.T) {
	dispatchAtTwo := func(held bool) string {
		args := dispatchArgs()
		args.Replicas, args.ListenerReplicas = pulumi.IntRef(2), pulumi.IntRef(2)
		args.ReplaceUnderHold = held
		m, err := runDispatch(t, args, true)
		require.NoError(t, err)
		return creatorDeployments(t, m, "example-dispatch", "example-dispatch-listener")
	}
	hubListener := func(held bool) string {
		args := clusteredArgs()
		listener(args).ReplaceUnderHold = held
		return creatorDeployments(t, runClusteredHub(t, args), clusteredListenerName)
	}
	for _, row := range []struct {
		name, today, held string
		render            func(bool) string
	}{
		{"gcp:Dispatch at two replicas", "dispatch-creators-at-2-replicas.golden", "dispatch-creators-at-2-replicas-held.golden", dispatchAtTwo},
		{"gcp:ClusteredNatsHub's listener", "clustered-hub-listener-deployment.golden", "clustered-hub-listener-deployment-held.golden", hubListener},
	} {
		t.Run(row.name, func(t *testing.T) {
			require.NotEqual(t, readGolden(t, row.today), readGolden(t, row.held))
			require.Equal(t, readGolden(t, row.today), row.render(false), "not held: today's render")
			require.Equal(t, readGolden(t, row.held), row.render(true), "held")
		})
	}
}

// The two inputs reach the clustered hub's listener and Dispatch through the plugin's own
// Construct, the path a consumer's program takes: production's shapes with replaceUnderHold false
// and an empty consumerNames render production's document byte for byte, and with
// replaceUnderHold true and consumerNames fresh they change it, so the comparison can see them.
func TestProductionRenderTakesTheHoldInputsThroughConstruct(t *testing.T) {
	base := prodRenderDocument(t, prodRenderShapes())
	with := func(hold bool, mode string) []prodRenderShape {
		shapes := prodRenderShapes()
		for _, shape := range shapes {
			switch {
			case shape.typ == "envoy-infra:gcp:Dispatch":
				shape.inputs["replaceUnderHold"] = property.New(hold)
				shape.inputs["consumerNames"] = prodRenderString(mode)
			case shape.typ == "envoy-infra:gcp:ClusteredNatsHub" && shape.inputs["tailnet"].IsMap():
				tailnet := shape.inputs["tailnet"].AsMap().AsMap()
				listener := tailnet["listener"].AsMap().AsMap()
				listener["replaceUnderHold"] = property.New(hold)
				listener["consumerNames"] = prodRenderString(mode)
				tailnet["listener"] = prodRenderObject(listener)
				shape.inputs["tailnet"] = prodRenderObject(tailnet)
			}
		}
		return shapes
	}
	require.Equal(t, string(base), string(prodRenderDocument(t, with(false, ""))), "the inputs at their defaults change nothing")
	changed := string(prodRenderDocument(t, with(true, "fresh")))
	require.NotEqual(t, string(base), changed)
	for _, want := range []string{`"ENVOY_CONSUMER_NAMES"`, `"pulumi.com/skipAwait"`, `"Recreate"`} {
		require.Less(t, strings.Count(string(base), want), strings.Count(changed, want), "%s is rendered", want)
	}
}
