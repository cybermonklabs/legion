package gcp_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
)

// firewallUnitPath is where the VM's cloud-config writes the host firewall's unit.
const firewallUnitPath = "/etc/systemd/system/nats-firewall.service"

// With monitorThroughIap the hub registers one more child, a firewall rule admitting IAP's TCP
// forwarding range, 35.235.240.0/20, to tcp:8222 alone on the instances that run as the hub's own
// account (never a tag), and its VM's host firewall admits the same range to the same port beside
// the health checks; every other child and every other file of the cloud-config is as without it.
// The control is the hub without it, whose children TestNatsHubRegistersEveryChildWithTheListedInputs
// lists whole, its user data the golden file: the two runs differ in that rule and that unit alone.
func TestNatsHubMonitorThroughIapOpensTheMonitoringPortToIapAlone(t *testing.T) {
	args := exampleArgs()
	args.MonitorThroughIap = true
	m, err := run(t, args, nil, true)
	require.NoError(t, err)

	doc, userData, err := gcp.CloudConfigOf(args)
	require.NoError(t, err)
	base, baseUserData, err := gcp.CloudConfigOf(exampleArgs())
	require.NoError(t, err)
	require.Equal(t, readGolden(t, "cloud-config.golden"), baseUserData, "control: without the input the user data is as before")
	require.NotEqual(t, baseUserData, userData, "the input rewrites the user data, so the group replaces the VM")

	firewall := fileOf(t, doc, firewallUnitPath)
	require.Contains(t, firewall, "\nDescription=Open the host firewall to Google's health checks and IAP's TCP forwarding on NATS's monitoring port, and to the tailnet on its client port\n")
	last := -1
	for _, rule := range []string{
		"INPUT -p tcp -s 130.211.0.0/22,35.191.0.0/16 --dport 8222 -j ACCEPT",
		"INPUT -i tailscale0 -p tcp --dport 4222 -j ACCEPT",
		"INPUT -p tcp -s 35.235.240.0/20 --dport 8222 -j ACCEPT",
	} {
		line := "\nExecStart=/bin/sh -c '/sbin/iptables -w -C " + rule + " || /sbin/iptables -w -A " + rule + "'\n"
		at := strings.Index(firewall, line)
		require.Greater(t, at, last, "the unit adds %q, after the rules before it:\n%s", rule, firewall)
		last = at
	}
	require.Equal(t, 3, strings.Count(firewall, "ExecStart="), "and no other rule:\n%s", firewall)
	require.NotContains(t, fileOf(t, base, firewallUnitPath), "35.235.240.0/20", "control: without the input the VM admits no IAP range")
	require.Len(t, doc.WriteFiles, len(base.WriteFiles))
	for i, f := range base.WriteFiles {
		if f.Path != firewallUnitPath {
			require.Equal(t, f, doc.WriteFiles[i], "%s is as without the input", f.Path)
		}
	}
	require.Equal(t, base.Bootcmd, doc.Bootcmd)
	require.Equal(t, base.Runcmd, doc.Runcmd)

	want := wantChildren(t, "", "")
	for _, c := range want {
		if c.typ == templateType {
			c.inputs["metadata"] = map[string]any{"user-data": userData}
		}
	}
	want = append(want, child{firewallType, "example-hub-iap-monitor", map[string]any{
		"network":               "example-hub-network-self-link",
		"allows":                []any{map[string]any{"protocol": "tcp", "ports": []any{"8222"}}},
		"sourceRanges":          []any{"35.235.240.0/20"},
		"targetServiceAccounts": []any{"example-hub-hub@example-project.iam.gserviceaccount.com"},
	}})
	requireChildren(t, m, hubURN, want, nil)
}
