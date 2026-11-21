package busprojects

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"
)

// The outcomes the go-live report gives each hub and listener replica.
const (
	// AdmissionAdmitted is a hub that admitted the last proof's new connection.
	AdmissionAdmitted = "admitted"
	// AdmissionRefused is a hub that did not: its auth service refused the connection, or the
	// connection failed or did not finish within the proof's time. Error says which.
	AdmissionRefused = "refused"
	// AdmissionUnknown is a hub no proof has reached yet.
	AdmissionUnknown = "unknown"

	// ReadAnswered is a replica that answered the last proof's read: required privacy mode, its
	// workload's source, and a read that answered.
	ReadAnswered = "answered"
	// ReadFailed is a replica that answered anything else, or did not answer. Error says which.
	ReadFailed = "failed"
	// ReadUnknown is a replica no proof has read yet, and a workload whose replicas the last proof
	// could not find (Address empty).
	ReadUnknown = "unknown"
)

// Report is the go-live report: the last proof's result for every hub and every listener replica,
// failed or passing, whatever the database holds, so an operator can read it before any project is
// private (ReportHandler serves it; no route mounts that handler at this head). It holds no
// project and no channel.
type Report struct {
	// ProvedAt is when the proof the report holds ended; absent before any proof has.
	ProvedAt time.Time `json:"proved_at,omitzero"`
	// Hubs is every hub of DISPATCH_PRIVATE_HUBS, in that order.
	Hubs []HubReport `json:"hubs"`
	// Listeners is every replica the proof read, workloads in the inventory's order and each
	// one's replicas by address; before a proof under the inventory, one line per workload.
	Listeners []ReplicaReport `json:"listeners"`
}

// HubReport is one hub's line of the report.
type HubReport struct {
	// URL is the hub's servers, comma-separated, without any userinfo.
	URL string `json:"url"`
	// Admission is AdmissionAdmitted, AdmissionRefused or AdmissionUnknown.
	Admission string `json:"admission"`
	// Error is why the hub's proof failed, when it did.
	Error string `json:"error,omitempty"`
}

// ReplicaReport is one listener replica's line of the report.
type ReplicaReport struct {
	// Workload is the name of the replica's workload in DISPATCH_PRIVATE_LISTENERS.
	Workload string `json:"workload"`
	// Address is the replica's bare IP, one of the addresses its workload's instances resolved
	// to, with no port: the port is the workload's, in its instances.
	Address string `json:"address"`
	// Instance, Required and Source are what the replica said of itself: the instance that
	// answered, whether it runs in required privacy mode, and the source it reads.
	Instance string `json:"instance"`
	Required bool   `json:"required"`
	Source   string `json:"source"`
	// Read is ReadAnswered, ReadFailed or ReadUnknown.
	Read string `json:"read"`
	// Error is why the replica failed or is unknown, when it did or is.
	Error string `json:"error,omitempty"`
}

// Report is the go-live report of the last proof that ended, or, before one has, every hub and
// listener workload as unknown. A nil admission answers the empty report.
func (a *HubAdmission) Report() Report {
	if a == nil {
		return Report{Hubs: []HubReport{}, Listeners: []ReplicaReport{}}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return Report{ProvedAt: a.report.ProvedAt, Hubs: slices.Clone(a.report.Hubs), Listeners: slices.Clone(a.report.Listeners)}
}

// ReportHandler serves Report as JSON, with status 200 whatever it holds: the report is for an
// operator to read, and no probe reads its status, so a failed proof takes no Dispatch pod out of
// service. A nil admission serves the empty report.
func (a *HubAdmission) ReportHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(a.Report())
	})
}

func (a *HubAdmission) unproven(targets []ListenerTarget) Report {
	report := Report{Hubs: make([]HubReport, len(a.hubs)), Listeners: make([]ReplicaReport, len(targets))}
	for i, h := range a.hubs {
		report.Hubs[i] = HubReport{URL: h.url, Admission: AdmissionUnknown}
	}
	for i, target := range targets {
		report.Listeners[i] = ReplicaReport{Workload: target.Name, Read: ReadUnknown}
	}
	return report
}
