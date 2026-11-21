package gcp_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
)

// routeAuthorization is a routeAuthorization at stage, from the Secret envoy-nats-route's key
// route.
func routeAuthorization(stage string) *gcp.ClusteredHubRouteAuthorizationArgs {
	return &gcp.ClusteredHubRouteAuthorizationArgs{SecretName: "envoy-nats-route", SecretKey: "route", Stage: stage}
}

// wantRouteEnv is the nats container's env with the route credential of the Secret
// envoy-nats-route: the password by reference, and the three pods' route URLs carrying it.
func wantRouteEnv() map[string]any {
	return map[string]any{
		"GOMEMLIMIT": "1638MiB",
		"NATS_ROUTE_PASSWORD": map[string]any{"valueFrom": map[string]any{"secretKeyRef": map[string]any{
			"name": "envoy-nats-route", "key": "route",
		}}},
		"NATS_ROUTE_URLS": "[nats://route:$(NATS_ROUTE_PASSWORD)@envoy-nats-0.envoy-nats-headless:6222, " +
			"nats://route:$(NATS_ROUTE_PASSWORD)@envoy-nats-1.envoy-nats-headless:6222, " +
			"nats://route:$(NATS_ROUTE_PASSWORD)@envoy-nats-2.envoy-nats-headless:6222]",
	}
}

// Each stage renders what the one before it did, and more, and nothing else of the chart's values
// changes: env adds the route variables to the nats container and leaves nats.conf's cluster block
// as it was; routes names $NATS_ROUTE_URLS as the cluster's routes; authorization names the route
// user and $NATS_ROUTE_PASSWORD as its authorization. No value holds a credential: the password is
// a secretKeyRef, and nats.conf names only the variables. The control is the hub without
// routeAuthorization, whose values are TestClusteredNatsHubChartValues' whole.
func TestClusteredNatsHubRouteStagesRenderInOrder(t *testing.T) {
	s, err := gcp.ResolveClusteredNatsHub(clusteredArgs())
	require.NoError(t, err)
	require.Equal(t, wantChartValues(s.AuthConf), chartValues(t, runClusteredHub(t, clusteredArgs())),
		"control: without routeAuthorization the values are as before")
	require.Equal(t, []string{"env", "routes", "authorization"}, gcp.ClusteredHubRouteStages)

	merges := map[string]map[string]any{
		"env":    nil,
		"routes": {"routes": "<< $NATS_ROUTE_URLS >>"},
		"authorization": {
			"routes":        "<< $NATS_ROUTE_URLS >>",
			"authorization": map[string]any{"user": "route", "password": "<< $NATS_ROUTE_PASSWORD >>"},
		},
	}
	for _, stage := range gcp.ClusteredHubRouteStages {
		t.Run(stage, func(t *testing.T) {
			args := clusteredArgs()
			args.RouteAuthorization = routeAuthorization(stage)
			resolved, err := gcp.ResolveClusteredNatsHub(args)
			require.NoError(t, err)
			require.Equal(t, []string{"envoy-nats-route", "route", stage},
				[]string{resolved.RouteSecretName, resolved.RouteSecretKey, resolved.RouteStage})
			require.Equal(t, s.AuthConf, resolved.AuthConf, "the route credential is no user of auth.conf")

			want := wantChartValues(s.AuthConf)
			want["container"].(map[string]any)["env"] = wantRouteEnv()
			if merge := merges[stage]; merge != nil {
				want["config"].(map[string]any)["cluster"].(map[string]any)["merge"] = merge
			}
			require.Equal(t, want, chartValues(t, runClusteredHub(t, args)))
		})
	}
}

// Beside a callout, the cluster block's merge holds the routes' ping pin and the stage's route
// entries together, so neither replaces the other.
func TestClusteredNatsHubRouteAuthorizationKeepsTheCalloutsRoutePing(t *testing.T) {
	args := withoutTailnet(clusteredArgs())
	args.Users = append(args.Users, authServiceUser())
	args.AuthCallout = authCallout()
	args.RouteAuthorization = routeAuthorization("authorization")
	cluster := chartValues(t, runClusteredHub(t, args))["config"].(map[string]any)["cluster"].(map[string]any)
	require.Equal(t, map[string]any{
		"ping_interval": "30s", "ping_max": 2.0,
		"routes":        "<< $NATS_ROUTE_URLS >>",
		"authorization": map[string]any{"user": "route", "password": "<< $NATS_ROUTE_PASSWORD >>"},
	}, cluster["merge"])
}

// The route URLs name the pods of the StatefulSet, <name>-<i>.<name>-headless, the chart's
// fullnameOverride, and not the client Service, serviceName. The fixture gives both the same
// value, so this row gives the hub a client Service of another name: a URL built from serviceName
// names pods that do not exist, and every route would fail to resolve.
func TestClusteredNatsHubRouteURLsNameTheStatefulSetsPods(t *testing.T) {
	args := clusteredArgs()
	args.ServiceName = "envoy-hub-client"
	args.RouteAuthorization = routeAuthorization("env")
	env := chartValues(t, runClusteredHub(t, args))["container"].(map[string]any)["env"].(map[string]any)
	require.Equal(t, wantRouteEnv()["NATS_ROUTE_URLS"], env["NATS_ROUTE_URLS"])
	require.NotContains(t, env["NATS_ROUTE_URLS"], "envoy-hub-client", "the URLs name no pod of the client Service's name")
}

// Kubernetes expands $(NATS_ROUTE_PASSWORD) in NATS_ROUTE_URLS only when the password's variable
// comes first in the container's env, and the chart writes container.env in its keys' order
// (templates/_helpers.tpl, nats.env, ranges over the map, which a Go template does in key order).
// So in the env the component renders, the variable that holds the password by its secretKeyRef
// sorts before the one that holds the route URLs, and the URLs reference the password's variable
// by its name, once each, and no other variable. TestClusteredNatsHubRouteAuthorizationRendersAndRuns
// reads the order in the rendered StatefulSet.
func TestClusteredNatsHubRouteEnvKeepsThePasswordFirst(t *testing.T) {
	args := clusteredArgs()
	args.RouteAuthorization = routeAuthorization("env")
	env := chartValues(t, runClusteredHub(t, args))["container"].(map[string]any)["env"].(map[string]any)
	var password, urls string
	for name, value := range env {
		switch value := value.(type) {
		case map[string]any:
			if from, ok := value["valueFrom"].(map[string]any); ok && from["secretKeyRef"] != nil {
				require.Empty(t, password, "one variable holds the password")
				password = name
			}
		case string:
			if strings.Contains(value, "nats://") {
				require.Empty(t, urls, "one variable holds the route URLs")
				urls = name
			}
		}
	}
	require.NotEmpty(t, password, "a variable holds the password: %v", env)
	require.NotEmpty(t, urls, "a variable holds the route URLs: %v", env)
	names := slices.Sorted(maps.Keys(env))
	require.Less(t, slices.Index(names, password), slices.Index(names, urls), "the password's variable %s sorts before the URLs' %s: %v", password, urls, names)
	value := env[urls].(string)
	require.Equal(t, 3, strings.Count(value, "$("+password+")"), "each route URL references the password's variable: %s", value)
	require.Equal(t, 3, strings.Count(value, "$("), "the URLs reference no other variable: %s", value)
}
