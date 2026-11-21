package writerhold

import (
	"context"
	"time"
)

// DeclaredServer is one declared server as step 1 read it: a pod of the StatefulSet, and its own
// /varz answer. Its server_id is the id step 4 and the report compare a restarted server's against,
// and its server_name is a declared name: the name a route to it must carry. The declared names are
// what the declared pods answer, never their pod names.
type DeclaredServer struct {
	Pod  string
	Varz Varz
}

// Declared is step 1's record of every declared server, in the StatefulSet's pod order.
type Declared []DeclaredServer

// defaultAuthTimeout is nats-server's authorization timeout when the configuration sets none, 2 s
// without TLS (server/const.go AUTH_TIMEOUT), which /varz then answers.
const defaultAuthTimeout = 2 * time.Second

// AuthTimeout is the longest auth_timeout the declared servers answered at step 1, or nats-server's
// default when none answered one: no connection a server holds for its authorization outlives it.
func (d Declared) AuthTimeout() time.Duration {
	longest := time.Duration(0)
	for _, server := range d {
		if timeout := time.Duration(server.Varz.AuthTimeout * float64(time.Second)); timeout > longest {
			longest = timeout
		}
	}
	if longest == 0 {
		return defaultAuthTimeout
	}
	return longest
}

// pods are the declared servers' pods, in order.
func (d Declared) pods() []string {
	pods := make([]string, len(d))
	for i, server := range d {
		pods[i] = server.Pod
	}
	return pods
}

// names are the declared names, each a declared pod's server_name at step 1, by pod.
func (d Declared) names() map[string]string {
	names := make(map[string]string, len(d))
	for _, server := range d {
		names[server.Pod] = server.Varz.ServerName
	}
	return names
}

// Discover is step 1's reading of every declared server, under the prepare grant, mutating
// nothing. The declared servers are the StatefulSet's pods (KubeHub.Pods), each read through its
// own monitoring port:
//
//   - /varz: server_id, server_name and auth_timeout, recorded. A declared server that does not
//     answer is UNKNOWN, and so is one that answers no id or no name, or the id or the name of
//     another declared pod.
//   - /routez: every route's remote_id is the id of another declared server, and its remote_name
//     that server's declared name. A route to any other server stops the hold, naming it.
//   - /leafz shows no leafnode, and /gatewayz no name and no outbound or inbound gateway: a client
//     behind either does not show in a server's /connz, so either stops the hold.
func Discover(ctx context.Context, hub KubeHub) (Declared, error) {
	pods, err := hub.Pods(ctx)
	if err != nil {
		return nil, err
	}
	reading, err := readServers(ctx, StepPreflight, hub.Monitor, pods)
	if err != nil {
		return nil, err
	}
	defer reading.close()
	declared := make(Declared, len(pods))
	names := make(map[string]string, len(pods))
	for i, pod := range pods {
		varz := reading.varz[pod]
		if varz.ServerName == "" {
			return nil, unknownf(StepPreflight, nil, "pod %s's /varz answers no server_name, so its routes cannot be checked by name", pod)
		}
		for other, name := range names {
			if name == varz.ServerName {
				return nil, unknownf(StepPreflight, nil, "pods %s and %s both answer server_name %q", other, pod, name)
			}
		}
		names[pod] = varz.ServerName
		declared[i] = DeclaredServer{Pod: pod, Varz: varz}
	}
	if err := reading.checkTopology(ctx, StepPreflight, names, nil); err != nil {
		return nil, err
	}
	return declared, nil
}

// opener opens one pod's monitoring port, which stop closes: KubeHub.Monitor, or a row's own.
type opener func(ctx context.Context, pod string) (m Monitor, stop func(), err error)

// serverReading is one reading of every declared server: each pod's monitoring port, open for the
// reading's every request, and each pod's /varz answer, read first.
type serverReading struct {
	pods     []string
	monitors map[string]Monitor
	stops    []func()
	varz     map[string]Varz
	// pod is each server's pod, by the server_id its /varz answered.
	pod map[string]string
}

func (r *serverReading) close() {
	for _, stop := range r.stops {
		stop()
	}
}

// readServers opens every pod's monitoring port and reads its /varz, in step. A pod whose port
// does not open or whose /varz does not answer is UNKNOWN, and so is an answer with no server_id,
// or two pods answering as one server, which a reading that reaches the wrong server would show.
func readServers(ctx context.Context, step string, open opener, pods []string) (*serverReading, error) {
	reading := &serverReading{pods: pods, monitors: map[string]Monitor{}, varz: map[string]Varz{}, pod: map[string]string{}}
	for _, pod := range pods {
		monitor, stop, err := open(ctx, pod)
		if err != nil {
			reading.close()
			return nil, unknownf(step, err, "declared server %s's monitoring port", pod)
		}
		reading.stops = append(reading.stops, stop)
		reading.monitors[pod] = monitor
		var varz Varz
		if err := monitor.Get(ctx, "/varz", &varz); err != nil {
			reading.close()
			return nil, unknownf(step, err, "declared server %s's /varz", pod)
		}
		if varz.ServerID == "" {
			reading.close()
			return nil, unknownf(step, nil, "declared server %s's /varz answers no server_id", pod)
		}
		if other, ok := reading.pod[varz.ServerID]; ok {
			reading.close()
			return nil, unknownf(step, nil, "pods %s and %s both answer as server %s", other, pod, varz.ServerID)
		}
		reading.varz[pod] = varz
		reading.pod[varz.ServerID] = pod
	}
	return reading, nil
}

// checkTopology is the route, leafnode and gateway check of step 1, which step 4 and the report run
// again, on the servers the reading read. names are the declared names, by pod: a server must
// still answer its pod's name, and a route's remote_name must be the declared name of the server
// its remote_id names. before maps the step-1 id of each server restarted since to its pod: a
// route still listed to one is a server's old process not yet gone, UNKNOWN, not a stranger.
func (r *serverReading) checkTopology(ctx context.Context, step string, names map[string]string, before map[string]string) error {
	for _, pod := range r.pods {
		if name := r.varz[pod].ServerName; name != names[pod] {
			return refusef(step, "pod %s's server answers server_name %q, and step 1 read %q", pod, name, names[pod])
		}
	}
	for _, pod := range r.pods {
		monitor, varz := r.monitors[pod], r.varz[pod]
		server := describeServer(pod, varz)
		var routez Routez
		if err := monitor.Get(ctx, "/routez", &routez); err != nil {
			return unknownf(step, err, "%s's /routez", server)
		}
		if routez.ServerID != varz.ServerID {
			return unknownf(step, nil, "%s's /routez answered as server %s: the server changed during the reading", server, routez.ServerID)
		}
		for _, route := range routez.Routes {
			target, declared := r.pod[route.RemoteID]
			switch {
			case route.RemoteID == "":
				return unknownf(step, nil, "%s lists route rid %d with no remote id yet", server, route.RID)
			case declared && target != pod:
				if route.RemoteName != names[target] {
					return refusef(step, "%s has a route to server %s named %q, not its declared name %q", server, route.RemoteID, route.RemoteName, names[target])
				}
			case !declared && before[route.RemoteID] != "":
				return unknownf(step, nil, "%s still lists a route to %s, the server pod %s ran before its restart", server, route.RemoteID, before[route.RemoteID])
			default:
				return refusef(step, "%s has a route to server %s (%s), which is no other declared server", server, route.RemoteName, route.RemoteID)
			}
		}
		var leafz Leafz
		if err := monitor.Get(ctx, "/leafz", &leafz); err != nil {
			return unknownf(step, err, "%s's /leafz", server)
		}
		if leafz.Leafnodes != 0 {
			return refusef(step, "%s has %d leafnode connection(s), and a client behind one does not show in its /connz", server, leafz.Leafnodes)
		}
		var gatewayz Gatewayz
		if err := monitor.Get(ctx, "/gatewayz", &gatewayz); err != nil {
			return unknownf(step, err, "%s's /gatewayz", server)
		}
		if gatewayz.Name != "" || len(gatewayz.Outbound) > 0 || len(gatewayz.Inbound) > 0 {
			return refusef(step, "%s has a gateway: name %q, %d outbound and %d inbound, and a client behind one does not show in its /connz",
				server, gatewayz.Name, len(gatewayz.Outbound), len(gatewayz.Inbound))
		}
	}
	return nil
}

// describeServer names a declared server in a finding: its name, its pod and its id.
func describeServer(pod string, varz Varz) string {
	return "server " + varz.ServerName + " (pod " + pod + ", id " + varz.ServerID + ")"
}
