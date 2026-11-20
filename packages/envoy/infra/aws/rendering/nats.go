package rendering

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/infra/core"
)

// The ports nats-server serves in the task: clients, cluster routes and HTTP monitoring.
const (
	NatsClientPort  = 4222
	natsClusterPort = 6222
	natsMonitorPort = 8222
)

// tailnetLocalAddrPort is where the sidecar's containerboot serves its health check.
const tailnetLocalAddrPort = "127.0.0.1:41234"

// NatsContainers holds what a NatsService task's two containers take from the component's inputs
// and children.
type NatsContainers struct {
	NatsImage        string
	Launch           Launch // NatsLaunch's
	TailscaleImage   string
	TailnetHostname  string
	TailnetTags      []string
	TailnetEphemeral bool
	TailnetSecretArn string // the sidecar's OAuth client secret, injected as TS_OAUTH_SECRET
	LogGroup         string
	Region           string
}

// NatsContainerDefinitions returns a NatsService task's container definitions, the NATS container
// and the Tailscale sidecar, written as json.dumps writes the reference's list: its dict order,
// ensure_ascii on.
func NatsContainerDefinitions(c NatsContainers) string {
	nats := core.Object{
		{Key: "name", Value: "nats"},
		{Key: "image", Value: c.NatsImage},
		{Key: "essential", Value: true},
	}
	nats = append(nats, c.Launch.fields...)
	nats = append(nats,
		core.Field{Key: "portMappings", Value: []core.Object{
			portMapping(NatsClientPort), portMapping(natsClusterPort), portMapping(natsMonitorPort),
		}},
		core.Field{Key: "mountPoints", Value: []core.Object{mountPoint("nats-data", "/data")}},
		core.Field{Key: "healthCheck", Value: healthCheck(
			fmt.Sprintf("wget -qO- http://%s:%d/healthz || exit 1", c.Launch.monitorHost(), natsMonitorPort), 30)},
		core.Field{Key: "logConfiguration", Value: logConfiguration(c.LogGroup, c.Region, "nats")},
	)

	// The sidecar serves the task's NATS client port on the tailnet. Its auth key is the OAuth
	// client secret with the key's attributes appended, which containerboot reads from TS_AUTHKEY.
	script := strings.Join([]string{
		"set -eu;",
		"mkdir -p /tmp/ts;",
		fmt.Sprintf(`echo '{"TCP":{"%d":{"TCPForward":"127.0.0.1:%d"}}}' > /tmp/ts/serve.json;`, NatsClientPort, NatsClientPort),
		`export TS_AUTHKEY="${TS_OAUTH_SECRET}?ephemeral=` + strconv.FormatBool(c.TailnetEphemeral) + `&preauthorized=true";`,
		"unset TS_OAUTH_SECRET;",
		"exec /usr/local/bin/containerboot",
	}, " ")
	sidecar := core.Object{
		{Key: "name", Value: "tailscale-sidecar"},
		{Key: "image", Value: c.TailscaleImage},
		{Key: "essential", Value: true},
		{Key: "entryPoint", Value: []string{"/bin/sh", "-c"}},
		{Key: "command", Value: []string{script}},
		{Key: "environment", Value: []core.Object{
			environment("TS_HOSTNAME", c.TailnetHostname),
			environment("TS_USERSPACE", "true"),
			environment("TS_STATE_DIR", "/var/lib/tailscale"),
			environment("TS_SERVE_CONFIG", "/tmp/ts/serve.json"),
			environment("TS_EXTRA_ARGS", "--advertise-tags="+strings.Join(c.TailnetTags, ",")),
			environment("TS_AUTH_ONCE", "true"),
			environment("TS_ENABLE_HEALTH_CHECK", "true"),
			environment("TS_LOCAL_ADDR_PORT", tailnetLocalAddrPort),
		}},
		{Key: "secrets", Value: []core.Object{secret("TS_OAUTH_SECRET", c.TailnetSecretArn)}},
		{Key: "mountPoints", Value: []core.Object{mountPoint("tsnet-state", "/var/lib/tailscale")}},
		{Key: "healthCheck", Value: healthCheck("wget -qO- http://"+tailnetLocalAddrPort+"/healthz || exit 1", 60)},
		{Key: "logConfiguration", Value: logConfiguration(c.LogGroup, c.Region, "tailscale-sidecar")},
	}
	return core.PyJSON([]core.Object{nats, sidecar}, core.PyJSONOptions{EnsureASCII: true})
}

// Launch is how the NATS container starts: its launch fields, and the host its health check reads
// monitoring on, which the start script decides.
type Launch struct {
	fields []core.Field
	// monitorOnLoopback is set where the start script binds monitoring to core.PrivateMonitorHost.
	monitorOnLoopback bool
}

// monitorHost is the host the container's health check reads /healthz on: localhost, as before
// private projects, or core.PrivateMonitorHost where the start script binds monitoring there. On
// the hub's image (nats:2.15.1-alpine) busybox wget takes ::1 for localhost whenever the
// container's loopback has IPv6, the Linux and Docker default, and nothing listens on [::1] then,
// so the check names the address the server binds (core/nats_monitor_healthcheck_live_test.go).
func (l Launch) monitorHost() string {
	if l.monitorOnLoopback {
		return core.PrivateMonitorHost
	}
	return "localhost"
}

// NatsLaunch returns the NATS container's launch. Without authorization the image's own entrypoint
// runs nats-server with serverArgs. With it, the task has no file of its own to mount, so
// `/bin/sh -c` runs core's StartScript, which `envoy-infra render nats-start-script` prints, and
// which binds monitoring to core.PrivateMonitorHost beside privateProjects; its refusals, each a
// core.InputError naming its input, pass through as they are.
func NatsLaunch(authorization *core.Authorization, header, serverArgs []string, configPath string) (Launch, error) {
	if authorization == nil {
		return Launch{fields: []core.Field{{Key: "command", Value: append([]string{"nats-server"}, serverArgs...)}}}, nil
	}
	script, err := authorization.StartScript(header, serverArgs, configPath)
	if err != nil {
		return Launch{}, err
	}
	return Launch{
		fields: []core.Field{
			{Key: "entryPoint", Value: []string{"/bin/sh", "-c"}},
			{Key: "command", Value: []string{script}},
		},
		monitorOnLoopback: authorization.PrivateProjects,
	}, nil
}
