package config

import (
	"reflect"
	"strings"
	"testing"
)

// The cluster's names as the sandbox tests write them, each a placeholder.
const (
	testWorkloadPool     = "example-project.svc.id.goog"
	testIdentityProvider = "https://container.googleapis.com/v1/projects/example-project/locations/us-central1/clusters/example-cluster"
)

// metadataLines is a `metadata` block under `runtime.kubernetes.sandbox.microvm`, at that block's
// indent, with the two members given; an empty one is left out.
func metadataLines(pool, provider string) string {
	lines := "        metadata:\n"
	if pool != "" {
		lines += "          workload_pool: " + pool + "\n"
	}
	if provider != "" {
		lines += "          identity_provider: " + provider + "\n"
	}
	return lines
}

// poolAudienceLines is a `runtime.kubernetes.agent_secrets` block whose member (audience or
// exchange_audience) is the Workload Identity pool the metadata tests name.
func poolAudienceLines(member string) string {
	return "    agent_secrets:\n      url: https://secrets.internal.example\n" + agentSecretsTokenFile + "      " + member + ": " + testWorkloadPool + "\n"
}

// computeClassRequired is the refusal of a sandbox that runs an issue pod in a microVM and names no
// ComputeClass for it.
const computeClassRequired = "runtime.kubernetes.sandbox.microvm.compute_class is required while runtime.kubernetes.sandbox.type is microvm" +
	" or runtime.kubernetes.sandbox.canary_issues names an issue: the ComputeClass whose nodes run the microVM pods"

// Each shape of `runtime.kubernetes.sandbox` the loader refuses, read the way
// `legion config check` reads a whole file (LoadForValidation, cmd/legion's checkConfig), so each
// refusal is what the operator reads there. Every message names the whole dotted key at fault.
// The rules that read scheduling, pod and the broker's block too fire only while an issue pod runs
// in a microVM.
func TestTheSandboxBlockRefusesEachShapeNamingItsKey(t *testing.T) {
	const microVM = "    sandbox:\n      type: microvm\n      microvm:\n        compute_class: example-microvm\n"
	const canary = "    sandbox:\n      canary_issues: [LEGSMOKE-7]\n      microvm:\n        compute_class: example-microvm\n"
	inert := func(pool, provider string) string {
		return "    sandbox:\n      microvm:\n" + metadataLines(pool, provider)
	}
	poolRefusal := func(pool string) string {
		return `runtime.kubernetes.sandbox.microvm.metadata.workload_pool "` + pool + `" must be the cluster's Workload Identity pool, <PROJECT_ID>.svc.id.goog`
	}
	providerRefusal := func(provider string) string {
		return `runtime.kubernetes.sandbox.microvm.metadata.identity_provider "` + provider + `" must be the cluster's identity provider,` +
			" https://container.googleapis.com/v1/projects/<PROJECT_ID>/locations/<LOCATION>/clusters/<CLUSTER>"
	}
	// audienceRefusal is the refusal of a broker audience that is the Workload Identity pool.
	audienceRefusal := func(member string) string {
		return "runtime.kubernetes.agent_secrets." + member + " must differ from runtime.kubernetes.sandbox.microvm.metadata.workload_pool (both " + testWorkloadPool + ")" +
			" while an issue pod runs in a microVM: the cluster projects one volume per audience, so the two tokens would be one"
	}
	for _, tc := range []struct {
		name, block, key, want string
	}{
		{
			name: "a type that is neither word", block: "    sandbox: {type: firecracker}\n",
			key: "runtime.kubernetes.sandbox.type", want: "runtime.kubernetes.sandbox.type must be 'gvisor' or 'microvm'",
		},
		{
			name: "an empty type", block: "    sandbox: {type: \"\"}\n",
			key: "runtime.kubernetes.sandbox.type", want: "runtime.kubernetes.sandbox.type must be 'gvisor' or 'microvm'",
		},
		{
			name: "the block a word", block: "    sandbox: microvm\n",
			key: "runtime.kubernetes.sandbox", want: "runtime.kubernetes.sandbox must be a mapping",
		},
		{
			name: "an unknown key in the block", block: "    sandbox: {runtime_class: microvm}\n",
			key: "runtime.kubernetes.sandbox.runtime_class", want: "unknown key runtime.kubernetes.sandbox.runtime_class",
		},
		{
			name: "an unknown key in microvm", block: "    sandbox: {microvm: {node_pool: example-pool}}\n",
			key: "runtime.kubernetes.sandbox.microvm.node_pool", want: "unknown key runtime.kubernetes.sandbox.microvm.node_pool",
		},
		{
			name: "an unknown key in metadata", block: inert(testWorkloadPool, testIdentityProvider) + "          audience: example-audience\n",
			key: "runtime.kubernetes.sandbox.microvm.metadata.audience", want: "unknown key runtime.kubernetes.sandbox.microvm.metadata.audience",
		},
		{
			name: "a canary entry that is no issue key", block: "    sandbox: {canary_issues: [legsmoke-7], microvm: {compute_class: example-microvm}}\n",
			key: "runtime.kubernetes.sandbox.canary_issues", want: `runtime.kubernetes.sandbox.canary_issues entry "legsmoke-7" is not an issue key, such as LEGION-208`,
		},
		{
			name: "a canary entry that is a project", block: "    sandbox: {canary_issues: [LEGSMOKE], microvm: {compute_class: example-microvm}}\n",
			key: "runtime.kubernetes.sandbox.canary_issues", want: `runtime.kubernetes.sandbox.canary_issues entry "LEGSMOKE" is not an issue key, such as LEGION-208`,
		},
		{
			name: "a canary entry named twice", block: "    sandbox: {canary_issues: [LEGSMOKE-7, LEGSMOKE-8, LEGSMOKE-7], microvm: {compute_class: example-microvm}}\n",
			key: "runtime.kubernetes.sandbox.canary_issues", want: "runtime.kubernetes.sandbox.canary_issues names LEGSMOKE-7 twice",
		},
		{
			name: "canary_issues a word", block: "    sandbox: {canary_issues: LEGSMOKE-7}\n",
			key: "runtime.kubernetes.sandbox.canary_issues", want: "runtime.kubernetes.sandbox.canary_issues must be an array of non-empty strings",
		},
		{
			name: "a microVM project with no microvm block", block: "    sandbox: {type: microvm}\n",
			key: "runtime.kubernetes.sandbox.microvm.compute_class", want: computeClassRequired,
		},
		{
			name: "a microVM project whose block names no compute class", block: "    sandbox: {type: microvm, microvm: {storage_class: example-microvm-disk}}\n",
			key: "runtime.kubernetes.sandbox.microvm.compute_class", want: computeClassRequired,
		},
		{
			name: "a canary list with no compute class", block: "    sandbox: {canary_issues: [LEGSMOKE-7]}\n",
			key: "runtime.kubernetes.sandbox.microvm.compute_class", want: computeClassRequired,
		},
		{
			name: "a blank compute class", block: "    sandbox: {type: microvm, microvm: {compute_class: \" \"}}\n",
			key: "runtime.kubernetes.sandbox.microvm.compute_class", want: "runtime.kubernetes.sandbox.microvm.compute_class must not be empty",
		},
		{
			name: "an empty metadata block", block: "    sandbox:\n      microvm:\n        metadata: {}\n",
			key:  "runtime.kubernetes.sandbox.microvm.metadata.workload_pool",
			want: "runtime.kubernetes.sandbox.microvm.metadata.workload_pool is required: the cluster's Workload Identity pool, <PROJECT_ID>.svc.id.goog",
		},
		{
			name: "metadata with no workload pool", block: inert("", testIdentityProvider),
			key:  "runtime.kubernetes.sandbox.microvm.metadata.workload_pool",
			want: "runtime.kubernetes.sandbox.microvm.metadata.workload_pool is required: the cluster's Workload Identity pool, <PROJECT_ID>.svc.id.goog",
		},
		{
			name: "metadata with no identity provider", block: inert(testWorkloadPool, ""),
			key: "runtime.kubernetes.sandbox.microvm.metadata.identity_provider",
			want: "runtime.kubernetes.sandbox.microvm.metadata.identity_provider is required: the cluster's identity provider," +
				" https://container.googleapis.com/v1/projects/<PROJECT_ID>/locations/<LOCATION>/clusters/<CLUSTER>",
		},
		{
			name: "a workload pool that is a service account", block: inert("legion@example-project.iam.gserviceaccount.com", testIdentityProvider),
			key: "runtime.kubernetes.sandbox.microvm.metadata.workload_pool", want: poolRefusal("legion@example-project.iam.gserviceaccount.com"),
		},
		{
			name: "a workload pool with no project", block: inert(".svc.id.goog", testIdentityProvider),
			key: "runtime.kubernetes.sandbox.microvm.metadata.workload_pool", want: poolRefusal(".svc.id.goog"),
		},
		{
			name: "a workload pool that is a project alone", block: inert("example-project", testIdentityProvider),
			key: "runtime.kubernetes.sandbox.microvm.metadata.workload_pool", want: poolRefusal("example-project"),
		},
		{
			name: "an identity provider on another host", block: inert(testWorkloadPool, "https://iam.googleapis.com/v1/projects/example-project/locations/us-central1/clusters/example-cluster"),
			key:  "runtime.kubernetes.sandbox.microvm.metadata.identity_provider",
			want: providerRefusal("https://iam.googleapis.com/v1/projects/example-project/locations/us-central1/clusters/example-cluster"),
		},
		{
			name: "an identity provider over http", block: inert(testWorkloadPool, "http://container.googleapis.com/v1/projects/example-project/locations/us-central1/clusters/example-cluster"),
			key:  "runtime.kubernetes.sandbox.microvm.metadata.identity_provider",
			want: providerRefusal("http://container.googleapis.com/v1/projects/example-project/locations/us-central1/clusters/example-cluster"),
		},
		{
			name: "an identity provider with an empty location", block: inert(testWorkloadPool, "https://container.googleapis.com/v1/projects/example-project/locations//clusters/example-cluster"),
			key:  "runtime.kubernetes.sandbox.microvm.metadata.identity_provider",
			want: providerRefusal("https://container.googleapis.com/v1/projects/example-project/locations//clusters/example-cluster"),
		},
		{
			name: "an identity provider with a segment past the cluster", block: inert(testWorkloadPool, testIdentityProvider+"/nodePools/default"),
			key: "runtime.kubernetes.sandbox.microvm.metadata.identity_provider", want: providerRefusal(testIdentityProvider + "/nodePools/default"),
		},
		{
			name: "an identity provider with no cluster", block: inert(testWorkloadPool, "https://container.googleapis.com/v1/projects/example-project/locations/us-central1"),
			key:  "runtime.kubernetes.sandbox.microvm.metadata.identity_provider",
			want: providerRefusal("https://container.googleapis.com/v1/projects/example-project/locations/us-central1"),
		},
		{
			name:  "the operator's node selector on sandbox.gke.io/runtime with a microVM project",
			block: "    scheduling: {node_selector: {sandbox.gke.io/runtime: gvisor}}\n" + microVM,
			key:   "runtime.kubernetes.scheduling.node_selector",
			want:  "runtime.kubernetes.scheduling.node_selector must not set sandbox.gke.io/runtime while an issue pod runs in a microVM (runtime.kubernetes.sandbox): the RuntimeClass places the pod",
		},
		{
			name: "the operator's toleration of sandbox.gke.io/runtime with a canary list",
			block: "    scheduling:\n      tolerations:\n        - {key: dedicated, operator: Equal, value: legion, effect: NoSchedule}\n" +
				"        - {key: sandbox.gke.io/runtime, operator: Exists, effect: NoSchedule}\n" + canary,
			key:  "runtime.kubernetes.scheduling.tolerations[1].key",
			want: "runtime.kubernetes.scheduling.tolerations[1].key must not be sandbox.gke.io/runtime while an issue pod runs in a microVM (runtime.kubernetes.sandbox): the RuntimeClass places the pod",
		},
		{
			name:  "GCE_METADATA_HOST in pod.env with the endpoint on a microVM project",
			block: "    pod: {env: {GCE_METADATA_HOST: metadata.internal.example}}\n" + microVM + metadataLines(testWorkloadPool, testIdentityProvider),
			key:   "runtime.kubernetes.pod.env",
			want: "runtime.kubernetes.pod.env must not set GCE_METADATA_HOST while runtime.kubernetes.sandbox.microvm.metadata is set and an issue pod runs in a microVM:" +
				" that pod's launcher sets it to the metadata endpoint it serves",
		},
		{
			name:  "GCE_METADATA_IP in pod.env with the endpoint on for a canary",
			block: "    pod: {env: {GCE_METADATA_IP: 192.0.2.10}}\n" + canary + metadataLines(testWorkloadPool, testIdentityProvider),
			key:   "runtime.kubernetes.pod.env",
			want: "runtime.kubernetes.pod.env must not set GCE_METADATA_IP while runtime.kubernetes.sandbox.microvm.metadata is set and an issue pod runs in a microVM:" +
				" that pod's launcher sets it to the metadata endpoint it serves",
		},
		{
			name:  "the broker's audience the Workload Identity pool with the endpoint on a microVM project",
			block: poolAudienceLines("audience") + microVM + metadataLines(testWorkloadPool, testIdentityProvider),
			key:   "runtime.kubernetes.agent_secrets.audience", want: audienceRefusal("audience"),
		},
		{
			name:  "the broker's exchange audience the Workload Identity pool with the endpoint on for a canary",
			block: poolAudienceLines("exchange_audience") + canary + metadataLines(testWorkloadPool, testIdentityProvider),
			key:   "runtime.kubernetes.agent_secrets.exchange_audience", want: audienceRefusal("exchange_audience"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, kubernetesFile+tc.block), noEnv)
			if err == nil {
				t.Fatalf("LoadForValidation succeeded, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("LoadForValidation error =\n%q\nwant\n%q", err.Error(), tc.want)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("LoadForValidation error %q names no %s", err.Error(), tc.key)
			}
		})
	}
}

// Each shape of `runtime.kubernetes.sandbox` the loader accepts, and what it settles to. A file
// that names no block runs every issue pod in gVisor. A microvm block's storage class is
// runtime.kubernetes.storage_class (gp2 here) when the block names none. A microvm block while no
// issue pod runs in a microVM is accepted and does nothing: the operator's own sandbox.gke.io/runtime
// selector and GCE_METADATA_HOST variable stand beside it, as a revert of a canary leaves them, and
// so does a broker audience that is the block's Workload Identity pool.
func TestTheSandboxBlockSettlesEachAcceptedShape(t *testing.T) {
	metadata := &MetadataConfig{WorkloadPool: testWorkloadPool, IdentityProvider: testIdentityProvider}
	for _, tc := range []struct {
		name, block string
		want        SandboxConfig
	}{
		{name: "no block", want: SandboxConfig{Type: SandboxGVisor}},
		{name: "an empty block", block: "    sandbox: {}\n", want: SandboxConfig{Type: SandboxGVisor}},
		{
			name: "gvisor written out with an empty canary list", block: "    sandbox: {type: gvisor, canary_issues: []}\n",
			want: SandboxConfig{Type: SandboxGVisor},
		},
		{
			name: "a microVM project with its compute class", block: "    sandbox: {type: microvm, microvm: {compute_class: example-microvm}}\n",
			want: SandboxConfig{Type: SandboxMicroVM, MicroVM: &MicroVMConfig{ComputeClass: "example-microvm", StorageClass: "gp2"}},
		},
		{
			name:  "a microVM project with a storage class of its own",
			block: "    sandbox: {type: microvm, microvm: {compute_class: example-microvm, storage_class: example-microvm-disk}}\n",
			want:  SandboxConfig{Type: SandboxMicroVM, MicroVM: &MicroVMConfig{ComputeClass: "example-microvm", StorageClass: "example-microvm-disk"}},
		},
		{
			name:  "a canary list on a gVisor project",
			block: "    sandbox: {canary_issues: [LEGSMOKE-7, LEGSMOKE-12], microvm: {compute_class: example-microvm}}\n",
			want: SandboxConfig{Type: SandboxGVisor, CanaryIssues: []string{"LEGSMOKE-7", "LEGSMOKE-12"},
				MicroVM: &MicroVMConfig{ComputeClass: "example-microvm", StorageClass: "gp2"}},
		},
		{
			name:  "a canary list on a microVM project",
			block: "    sandbox: {type: microvm, canary_issues: [LEGSMOKE-7], microvm: {compute_class: example-microvm}}\n",
			want: SandboxConfig{Type: SandboxMicroVM, CanaryIssues: []string{"LEGSMOKE-7"},
				MicroVM: &MicroVMConfig{ComputeClass: "example-microvm", StorageClass: "gp2"}},
		},
		{
			name:  "a microVM project with the metadata endpoint",
			block: "    sandbox:\n      type: microvm\n      microvm:\n        compute_class: example-microvm\n" + metadataLines(testWorkloadPool, testIdentityProvider),
			want:  SandboxConfig{Type: SandboxMicroVM, MicroVM: &MicroVMConfig{ComputeClass: "example-microvm", StorageClass: "gp2", Metadata: metadata}},
		},
		{
			name:  "an inert microvm block with metadata and no microVM issue",
			block: "    sandbox:\n      canary_issues: []\n      microvm:\n        compute_class: example-microvm\n" + metadataLines(testWorkloadPool, testIdentityProvider),
			want:  SandboxConfig{Type: SandboxGVisor, MicroVM: &MicroVMConfig{ComputeClass: "example-microvm", StorageClass: "gp2", Metadata: metadata}},
		},
		{
			name: "an inert microvm block with no compute class beside the operator's own selector and variable",
			block: "    scheduling: {node_selector: {sandbox.gke.io/runtime: gvisor}}\n    pod: {env: {GCE_METADATA_HOST: metadata.internal.example}}\n" +
				"    sandbox:\n      microvm:\n" + metadataLines(testWorkloadPool, testIdentityProvider),
			want: SandboxConfig{Type: SandboxGVisor, MicroVM: &MicroVMConfig{StorageClass: "gp2", Metadata: metadata}},
		},
		{
			name:  "an inert microvm block with metadata beside a broker audience that is its pool",
			block: poolAudienceLines("audience") + "    sandbox:\n      microvm:\n" + metadataLines(testWorkloadPool, testIdentityProvider),
			want:  SandboxConfig{Type: SandboxGVisor, MicroVM: &MicroVMConfig{StorageClass: "gp2", Metadata: metadata}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile+tc.block), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if got := cfg.Runtime.Kubernetes.Sandbox; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("runtime.kubernetes.sandbox = %s, want %s", sandboxString(got), sandboxString(tc.want))
			}
		})
	}
}

// sandboxString is c with its pointers followed, for a failure message.
func sandboxString(c SandboxConfig) string {
	text := string(c.Type) + " canary=" + strings.Join(c.CanaryIssues, ",")
	if c.MicroVM != nil {
		text += " microvm={compute_class=" + c.MicroVM.ComputeClass + " storage_class=" + c.MicroVM.StorageClass
		if c.MicroVM.Metadata != nil {
			text += " metadata={" + c.MicroVM.Metadata.WorkloadPool + " " + c.MicroVM.Metadata.IdentityProvider + "}"
		}
		text += "}"
	}
	return text
}

// AnyMicroVM is true while any issue pod runs in a microVM: the project's type is microvm, or the
// canary list names an issue. Which issue's pod runs where is the runtime's rule
// (internal/runtime/sandbox's SandboxConfig.Of), tested there.
func TestSandboxAnyMicroVMWhileAnIssuePodRunsInAMicroVM(t *testing.T) {
	gVisorProject := SandboxConfig{Type: SandboxGVisor, CanaryIssues: []string{"LEGSMOKE-7"}}
	microVMProject := SandboxConfig{Type: SandboxMicroVM, CanaryIssues: []string{"LEGSMOKE-7"}}
	for _, tc := range []struct {
		name   string
		config SandboxConfig
		want   bool
	}{
		{"a gVisor project with no canary", SandboxConfig{Type: SandboxGVisor}, false},
		{"the zero value", SandboxConfig{}, false},
		{"a gVisor project with a canary", gVisorProject, true},
		{"a microVM project", SandboxConfig{Type: SandboxMicroVM}, true},
		{"a microVM project with a canary", microVMProject, true},
	} {
		if got := tc.config.AnyMicroVM(); got != tc.want {
			t.Errorf("%s: AnyMicroVM() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
