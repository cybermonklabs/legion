package busprojects

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
)

// ListenerTarget is one listener workload of DISPATCH_PRIVATE_LISTENERS, in the JSON that
// variable holds: one entry per workload that serves a hub of DISPATCH_PRIVATE_HUBS.
type ListenerTarget struct {
	// Name is the workload's name, which the report and every error name it by.
	Name string `json:"name"`
	// Hub is the URL of the hub of DISPATCH_PRIVATE_HUBS the workload serves. The proof reads
	// every workload whichever hub it serves, and never reads Hub: that every hub has a workload
	// is for the code that builds the inventory to check, cmd/dispatch's settings, which do not
	// check it at this head, so an inventory that leaves a hub out passes the proof with none of
	// that hub's listeners read.
	Hub string `json:"hub"`
	// Source is the privacy source every replica must read from: the secrets broker's base URL.
	Source string `json:"source"`
	// Host is the Host a request to a replica carries, a name the workload's ENVOY_ALLOWED_HOSTS
	// holds.
	Host string `json:"host"`
	// Instances is how the replicas are found, host:port: the name of a headless Service that
	// publishes not-ready addresses, whose A records are every replica, ready or not, or an IP
	// literal for one replica.
	Instances string `json:"instances"`
}

// ReadinessPath is the listener's privacy-readiness route, which answers with a fresh read of
// the private projects from its privacy source.
const ReadinessPath = "/v1/privacy-readiness"

// maxReadinessBody bounds how much of a replica's answer is read: the answer is a few short
// fields.
const maxReadinessBody = 64 << 10

// readiness is a replica's answer at ReadinessPath: the replica that answered, whether it runs in
// required privacy mode, the source it reads, and how that read went (answered, failed, or none for
// a listener that reads nothing).
type readiness struct {
	Instance string `json:"instance"`
	Required bool   `json:"required"`
	Source   string `json:"source"`
	Read     string `json:"read"`
}

type replicaSlot struct {
	target  ListenerTarget
	address string
	port    string
}

// replicaEntry is one line of a proof's listener part: a replica to read (slot) and what reading
// it gave, or a workload whose replicas could not be found, which has no slot.
type replicaEntry struct {
	report ReplicaReport
	err    error
	slot   *replicaSlot
}

// readListeners reads every replica of every workload of targets, all at once, and answers one
// report per replica, workloads in targets' order and each workload's replicas by address, with
// an error for each one that failed. Instances that are an IP literal are that one replica, with
// no lookup; any other name is resolved with lookup. A workload whose instances cannot be read,
// or whose name resolves to no address, is one report of its own with read unknown, and an error
// saying it is UNKNOWN: a workload with no replica read has not passed.
func readListeners(ctx context.Context, lookup func(context.Context, string) ([]string, error), client *http.Client, targets []ListenerTarget) ([]ReplicaReport, []error) {
	var entries []replicaEntry
	for _, target := range targets {
		host, port, err := net.SplitHostPort(target.Instances)
		if err != nil {
			entries = append(entries, unknownWorkload(target, fmt.Sprintf("its instances %q are not host:port: %v", target.Instances, err)))
			continue
		}
		addresses, err := resolve(ctx, lookup, host)
		if err != nil {
			entries = append(entries, unknownWorkload(target, fmt.Sprintf("its instances %q resolve to no address: %v", target.Instances, err)))
			continue
		}
		slices.Sort(addresses)
		addresses = slices.Compact(addresses)
		if len(addresses) == 0 {
			entries = append(entries, unknownWorkload(target, fmt.Sprintf("its instances %q resolve to no address", target.Instances)))
			continue
		}
		for _, address := range addresses {
			entries = append(entries, replicaEntry{slot: &replicaSlot{target: target, address: address, port: port}})
		}
	}
	var wg sync.WaitGroup
	for i := range entries {
		if slot := entries[i].slot; slot != nil {
			wg.Go(func() { entries[i].report, entries[i].err = readReplica(ctx, client, *slot) })
		}
	}
	wg.Wait()
	reports := make([]ReplicaReport, 0, len(entries))
	var errs []error
	for _, e := range entries {
		reports = append(reports, e.report)
		if e.err != nil {
			errs = append(errs, e.err)
		}
	}
	return reports, errs
}

func resolve(ctx context.Context, lookup func(context.Context, string) ([]string, error), host string) ([]string, error) {
	if net.ParseIP(host) != nil {
		return []string{host}, nil
	}
	return lookup(ctx, host)
}

func unknownWorkload(target ListenerTarget, why string) replicaEntry {
	return replicaEntry{
		report: ReplicaReport{Workload: target.Name, Read: ReadUnknown, Error: why},
		err:    fmt.Errorf("busprojects: UNKNOWN: listener %s: %s, so none of its replicas was read", target.Name, why),
	}
}

// readReplica asks one replica for a fresh privacy read, GET ReadinessPath at its address and
// its workload's port with its workload's Host, and passes only a 200 whose body says the replica
// runs in required privacy mode, reads the workload's source, and that its read answered. The read
// goes over https to every address but a loopback one (readinessScheme), since the client presents
// Dispatch's credential with it (SetListeners). A 3xx fails it: client follows no redirect
// (SetListeners), since the address a redirect names is not the replica. The report carries what
// the replica said of itself whenever its body parsed.
func readReplica(ctx context.Context, client *http.Client, slot replicaSlot) (ReplicaReport, error) {
	report := ReplicaReport{Workload: slot.target.Name, Address: slot.address, Read: ReadFailed}
	fail := func(format string, args ...any) (ReplicaReport, error) {
		why := fmt.Sprintf(format, args...)
		report.Error = why
		return report, fmt.Errorf("busprojects: listener %s replica %s: %s", slot.target.Name, slot.address, why)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, readinessScheme(slot.address)+"://"+net.JoinHostPort(slot.address, slot.port)+ReadinessPath, nil)
	if err != nil {
		return fail("the request cannot be made: %v", err)
	}
	request.Host = slot.target.Host
	response, err := client.Do(request)
	if err != nil {
		return fail("did not answer: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxReadinessBody+1))
	if err != nil {
		return fail("answered %d, and its body could not be read: %v", response.StatusCode, err)
	}
	if len(body) > maxReadinessBody {
		return fail("answered %d with a body longer than %d bytes", response.StatusCode, maxReadinessBody)
	}
	var answer readiness
	parseErr := json.Unmarshal(body, &answer)
	if parseErr == nil {
		report.Instance, report.Required, report.Source = answer.Instance, answer.Required, answer.Source
	}
	switch {
	case response.StatusCode >= 300 && response.StatusCode < 400:
		return fail("answered %d, a redirect to %q, which the proof does not follow", response.StatusCode, response.Header.Get("Location"))
	case response.StatusCode != http.StatusOK && parseErr == nil:
		return fail("answered %d, read %q", response.StatusCode, answer.Read)
	case response.StatusCode != http.StatusOK:
		return fail("answered %d", response.StatusCode)
	case parseErr != nil:
		return fail("answered a body that does not parse: %v", parseErr)
	case !answer.Required:
		return fail("does not run in required privacy mode (required: false, read %q)", answer.Read)
	case answer.Source != slot.target.Source:
		return fail("reads %q, not its workload's source %q", answer.Source, slot.target.Source)
	case answer.Read != ReadAnswered:
		return fail("answered read %q, not %q", answer.Read, ReadAnswered)
	}
	report.Read = ReadAnswered
	return report, nil
}

// readinessScheme is the scheme of a read of the replica at address: https, unless address is a
// loopback IP address, where http is (a test's replica). A read carries Dispatch's credential and
// decides whether a replica runs in required privacy mode, so neither may cross a network in the
// clear, where anyone on the path could take the credential or answer for the replica.
func readinessScheme(address string) string {
	if ip := net.ParseIP(address); ip != nil && ip.IsLoopback() {
		return "http"
	}
	return "https"
}
