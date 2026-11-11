package gcp_test

import (
	"testing"

	"github.com/sjawhar/envoy/infra/gcp"
)

// name takes a DNS-1035 label (dispatch://INFRA-2739/artifact/open-items-md, decision 16). The
// resolver checks it with namespaceRule, which is DNS-1123 and admits a leading digit, while the
// chart renders <name>-headless and this change adds <name>-listener, each a Service name
// Kubernetes validates as a DNS-1035 label. So a hub named 9nats is accepted here and refused at
// apply, which is a refusal the operator meets in the cluster rather than in the preview. The
// serviceName input is already held to dns1035LabelRule (clusteredhub.go), and name is the same
// class of input.
func TestClusteredNatsHubRefusesANameThatIsNoDNS1035Label(t *testing.T) {
	type A = gcp.ClusteredNatsHubArgs
	requireClusteredRefusals(t, []clusteredRefusal{
		{"a name starting with a digit", func(a *A) { a.Name = "9nats" }, "name", []string{`name "9nats"`, "DNS-1035"}},
	})
}
