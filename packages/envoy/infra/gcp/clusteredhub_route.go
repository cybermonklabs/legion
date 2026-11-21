package gcp

import (
	"fmt"
	"slices"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
)

// clusteredHubRouteStage is how far a hub's routeAuthorization has gone. Each stage renders all
// that the one before it rendered, and more.
type clusteredHubRouteStage int

const (
	// routeStageEnv gives the nats container the route password, from the Secret, and the route
	// URLs built from it, as NATS_ROUTE_PASSWORD and NATS_ROUTE_URLS. nats.conf is unchanged, so
	// the apply rolls the pods and changes no route.
	routeStageEnv clusteredHubRouteStage = iota
	// routeStageRoutes also names $NATS_ROUTE_URLS as nats.conf's cluster.routes, so each server
	// presents the credential on the routes it opens. The reloader applies it; no pod restarts.
	routeStageRoutes
	// routeStageAuthorization also names the route user and $NATS_ROUTE_PASSWORD as nats.conf's
	// cluster.authorization, so each server refuses a route that does not present the credential.
	// The reloader applies it; no pod restarts.
	routeStageAuthorization
)

// clusteredHubRouteStageNames are the names routeAuthorization.stage takes, each bound to its
// stage, in the stages' order.
//
// The order holds because nats-server reads a variable of nats.conf from its own environment at
// each load: a nats.conf that names $NATS_ROUTE_URLS before the container holds it fails every
// reload ("variable reference for 'NATS_ROUTE_URLS' on line … can not be found", v2.15.1
// conf/parse.go), every later one of a user change included, and a pod that restarts on it exits
// 1. So env is finished on every pod, which the apply's wait for the StatefulSet's roll shows,
// before routes; and every server opens its routes with the credential, which routes' reload
// gives, before authorization refuses a route without one. Each stage is its own apply.
//
// A reload accepts a change to the cluster block but its host or port (reload.go:1710-1737), and
// it closes only the routes the server accepted that now fail the credential check (reload.go:
// 2367-2376), and an accepted route that duplicates one the server's own routes name is marked as
// one it opened itself, which the check skips (route.go:2484-2530). So a process that joined as a
// route during env or routes keeps the routes the hub's servers opened to it, and once
// authorization applies it can read the password: one INFO naming another server with
// auth_required makes a server dial that address as an implicit route with the route user and
// password (route.go:702-713, :1092-1094). Authorization fences a process that tries to join after
// it and not one that joined before, so once authorization is on every pod the rollout runs
// `envoy-infra routes check` (package routes): each pod's /routez against the pods' IPs and server
// ids, each pod's log from the reload that loaded authorization against the pods' IPs, its route
// port's INFO for auth_required, and no failed reload (docs/nats-hub.md, Route authorization).
var clusteredHubRouteStageNames = [...]string{
	routeStageEnv:           "env",
	routeStageRoutes:        "routes",
	routeStageAuthorization: "authorization",
}

// String is the stage's name, as routeAuthorization.stage takes it.
func (s clusteredHubRouteStage) String() string { return clusteredHubRouteStageNames[s] }

// The route credential's names: the user every route presents, and the nats container's two
// variables. Kubernetes expands $(NATS_ROUTE_PASSWORD) in NATS_ROUTE_URLS only when the password's
// variable comes first in the container's env, and the chart writes container.env in key order,
// which these names keep (TestClusteredNatsHubRouteEnvKeepsThePasswordFirst).
const (
	clusteredHubRouteUser        = "route"
	clusteredHubRoutePasswordEnv = "NATS_ROUTE_PASSWORD"
	clusteredHubRouteURLsEnv     = "NATS_ROUTE_URLS"
)

// newClusteredHubRoute resolves routeAuthorization, nil without one. The Secret is one the hub's
// namespace already holds, named by reference; the component never reads, creates or waits on it,
// so its consumer has it synced before the env stage's apply, or the rolled pods do not start.
func newClusteredHubRoute(args *ClusteredHubRouteAuthorizationArgs, namespace string) (*clusteredHubRoute, error) {
	if args == nil {
		return nil, nil
	}
	if !dnsSubdomain(args.SecretName) {
		return nil, core.InputErrorf("routeAuthorization.secretName", "routeAuthorization.secretName %q must be a DNS subdomain, the name of a Secret in namespace %s", args.SecretName, namespace)
	}
	if !secretKeyRule.MatchString(args.SecretKey) {
		return nil, core.InputErrorf("routeAuthorization.secretKey", "routeAuthorization.secretKey %q must match %s", args.SecretKey, secretKeyRule)
	}
	stage := slices.Index(clusteredHubRouteStageNames[:], args.Stage)
	if stage < 0 {
		return nil, core.InputErrorf("routeAuthorization.stage", "routeAuthorization.stage %q must be one of %s, in that order, one apply each: "+
			"env gives the pods the credential, routes presents it, and authorization requires it", args.Stage, strings.Join(clusteredHubRouteStageNames[:], ", "))
	}
	return &clusteredHubRoute{secretName: args.SecretName, secretKey: args.SecretKey, stage: clusteredHubRouteStage(stage)}, nil
}

// env is the nats container's route variables, as chart container.env entries: the password from
// the Secret, and the URLs of the three servers' routes with it as each URL's password, which
// Kubernetes expands in the pod. The URLs name the pods of the chart's headless Service,
// <name>-<i>.<name>-headless, on the route port, by the absolute name in the hub's namespace with
// the trailing dot. A relative name that fails while a pod is re-created goes down the resolver's
// search list, whose next name is <name>-<i>.<name>-headless.svc.cluster.local: Service <name>-<i>
// of a namespace named <name>-headless, which would be sent the password, since a route sends its
// CONNECT, credential and all, before it reads its peer's INFO (nats-server 2.15.1
// server/route.go:2087-2094).
func (r *clusteredHubRoute) env(release, namespace string) pulumi.Map {
	urls := make([]string, clusteredHubServers)
	for i := range urls {
		pod := clusterLocal(fmt.Sprintf("%s-%d.%s-headless", release, i, release), namespace) + "."
		urls[i] = fmt.Sprintf("nats://%s:$(%s)@%s:%d", clusteredHubRouteUser, clusteredHubRoutePasswordEnv, pod, clusteredHubClusterPort)
	}
	return pulumi.Map{
		clusteredHubRoutePasswordEnv: pulumi.Map{"valueFrom": pulumi.Map{"secretKeyRef": pulumi.Map{
			"name": pulumi.String(r.secretName), "key": pulumi.String(r.secretKey),
		}}},
		clusteredHubRouteURLsEnv: pulumi.String("[" + strings.Join(urls, ", ") + "]"),
	}
}

// merge adds what the stage renders into nats.conf's cluster block to merge, the chart's
// config.cluster.merge: nothing at env, the routes at routes, and the routes and the authorization
// at authorization. Each value names a variable and never the credential: the chart writes a
// string of the form << X >> as X unquoted (templates/_helpers.tpl, nats.formatConfig), so the
// ConfigMap holds $NATS_ROUTE_URLS and $NATS_ROUTE_PASSWORD, which nats-server reads from its
// environment. The chart's own config.cluster.routeURLs.user and password are not used: the chart
// writes them into every route URL of nats.conf as literals (files/config/cluster.yaml).
func (r *clusteredHubRoute) merge(merge pulumi.Map) {
	if r.stage >= routeStageRoutes {
		merge["routes"] = pulumi.String("<< $" + clusteredHubRouteURLsEnv + " >>")
	}
	if r.stage >= routeStageAuthorization {
		merge["authorization"] = pulumi.Map{
			"user":     pulumi.String(clusteredHubRouteUser),
			"password": pulumi.String("<< $" + clusteredHubRoutePasswordEnv + " >>"),
		}
	}
}
