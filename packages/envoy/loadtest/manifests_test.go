package main

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The cluster copy's manifests (cluster/manifests), as cluster/cluster.sh renders them, and the
// endpoint flags cluster.sh gives every runner job. The copy's namespace in this test is
// copyNamespace, which the projected-token subjects carry.
const copyNamespace = "loadtest"

// copyManifestValues is one value of every placeholder the manifests hold, each of the shape
// cluster.sh's render_manifest checks it against (PLACEHOLDER_KIND), as cluster.test.sh's
// template case gives them.
var copyManifestValues = []string{
	"NAMESPACE=" + copyNamespace, "RUN_ID=lt-20261028t120000-abcd", "STORAGE_CLASS=premium-rwo",
	"OIDC_ISSUER=https://container.googleapis.com/v1/projects/example-project/locations/us-west1/clusters/example",
	"ENVOY_IMAGE=ghcr.io/cybermonklabs/legion/envoy@sha256:" + strings.Repeat("7", 64),
	"LISTENER_IMAGE=ghcr.io/cybermonklabs/legion/envoy@sha256:" + strings.Repeat("7", 64),
	"PG_IMAGE=mirror.gcr.io/pgvector/pgvector@sha256:" + strings.Repeat("7", 64),
	"SOCAT_IMAGE=mirror.gcr.io/alpine/socat@sha256:" + strings.Repeat("7", 64),
	"RUNNER_IMAGE=mirror.gcr.io/library/debian@sha256:" + strings.Repeat("7", 64),
	"PG_TIER=db-custom-16-61440", "PG_CPU=16", "PG_MEMORY=60Gi", "PG_VOLUME=100Gi",
	"PG_MAX_CONNECTIONS=800", "PG_SHARED_BUFFERS=20480MB", "PG_EFFECTIVE_CACHE_SIZE=24576MB",
	"PG_WORK_MEM=4MB", "PG_MAINTENANCE_WORK_MEM=64MB", "PG_MAX_WAL_SIZE=1504MB",
	"PG_PASSWORD=" + strings.Repeat("a1", 24), "DISPATCH_TOKEN=" + strings.Repeat("b2", 32), "SIGNING_KEY=" + strings.Repeat("c3", 32),
}

// copyManifests is every manifest cluster.sh applies.
var copyManifests = []string{"secrets.yaml.tmpl", "postgres.yaml", "nats.yaml", "listener.yaml", "dispatch.yaml", "runner.yaml"}

// clusterShell runs script under bash with cluster/cluster.sh sourced, the way cluster.test.sh
// and render-nats.sh read its functions, and answers what it printed.
func clusterShell(t *testing.T, script string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"-c", "set -euo pipefail; source cluster/cluster.sh; " + script, "cluster-shell"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", errors.New(strings.TrimSpace(err.Error() + ": " + stderr.String()))
	}
	return stdout.String(), nil
}

// manifestEnv is one entry of a container's env.
type manifestEnv struct {
	Name      string `yaml:"name"`
	Value     string `yaml:"value"`
	ValueFrom any    `yaml:"valueFrom"`
}

// manifestPodSpec is what this test reads of a pod's spec.
type manifestPodSpec struct {
	ServiceAccountName string `yaml:"serviceAccountName"`
	Containers         []struct {
		Name         string        `yaml:"name"`
		Env          []manifestEnv `yaml:"env"`
		VolumeMounts []struct {
			Name      string `yaml:"name"`
			MountPath string `yaml:"mountPath"`
			ReadOnly  bool   `yaml:"readOnly"`
		} `yaml:"volumeMounts"`
	} `yaml:"containers"`
	Volumes []struct {
		Name      string `yaml:"name"`
		Projected *struct {
			Sources []struct {
				ServiceAccountToken *struct {
					Audience string `yaml:"audience"`
					Path     string `yaml:"path"`
				} `yaml:"serviceAccountToken"`
			} `yaml:"sources"`
		} `yaml:"projected"`
	} `yaml:"volumes"`
}

// manifestDoc is one object of a rendered manifest: a Pod's spec is its pod spec, a Deployment's
// is under spec.template.
type manifestDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	StringData map[string]string `yaml:"stringData"`
	Spec       struct {
		manifestPodSpec `yaml:",inline"`
		Template        struct {
			Spec manifestPodSpec `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// podSpec is the pod spec of the object of kind and name in docs.
func podSpec(t *testing.T, docs []manifestDoc, kind, name string) manifestPodSpec {
	t.Helper()
	for _, d := range docs {
		if d.Kind == kind && d.Metadata.Name == name {
			if kind == "Pod" {
				return d.Spec.manifestPodSpec
			}
			return d.Spec.Template.Spec
		}
	}
	t.Fatalf("no %s %s among the rendered objects", kind, name)
	return manifestPodSpec{}
}

// renderCopyManifests is every manifest of the copy rendered by cluster.sh's render_manifest, the
// function apply_manifest renders each one through, decoded object by object.
func renderCopyManifests(t *testing.T) map[string][]manifestDoc {
	t.Helper()
	out := map[string][]manifestDoc{}
	for _, m := range copyManifests {
		rendered, err := clusterShell(t, `file=$1; shift; render_manifest "$MANIFESTS/$file" "$@"`, append([]string{m}, copyManifestValues...)...)
		if err != nil {
			t.Errorf("cluster.sh's render_manifest refused %s: %v", m, err)
			continue
		}
		dec := yaml.NewDecoder(strings.NewReader(rendered))
		for {
			var d manifestDoc
			if err := dec.Decode(&d); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("%s as rendered is not YAML: %v", m, err)
			}
			out[m] = append(out[m], d)
		}
	}
	return out
}

// listenerTokenMount is the path in a pod at which its projected token for audience is mounted,
// read off its containers' mounts and its volumes; empty when it mounts none.
func listenerTokenMount(spec manifestPodSpec, audience string) string {
	for _, v := range spec.Volumes {
		if v.Projected == nil {
			continue
		}
		for _, s := range v.Projected.Sources {
			if s.ServiceAccountToken == nil || s.ServiceAccountToken.Audience != audience {
				continue
			}
			for _, c := range spec.Containers {
				for _, m := range c.VolumeMounts {
					if m.Name == v.Name && m.ReadOnly {
						return m.MountPath + "/" + s.ServiceAccountToken.Path
					}
				}
			}
		}
	}
	return ""
}

// envOf is a container's env by name, for the container named in spec.
func envOf(t *testing.T, spec manifestPodSpec, container string) map[string]manifestEnv {
	t.Helper()
	for _, c := range spec.Containers {
		if c.Name == container {
			env := map[string]manifestEnv{}
			for _, e := range c.Env {
				env[e.Name] = e
			}
			return env
		}
	}
	t.Fatalf("no container %s", container)
	return nil
}

// TestTheLoadtestListenerTakesItsCallersProjectedTokens: the copy's listener binds 0.0.0.0 and
// takes no shared token, so each of its /v1 callers -- the run's Dispatch and the runner pod
// loadgen runs in -- presents a projected token of an account of its own, which the listener's
// ENVOY_OIDC_ACCOUNTS lists and nothing else does: each runs as that account and mounts its token
// for the listener's audience, read-only, at /var/run/secrets/envoy-listener/token. The listener
// takes a send's role mark from Dispatch's account alone. Every runner job cluster.sh starts names
// that file as -listener-token-file and gives no -listener-token. No object of the copy holds
// ENVOY_API_TOKEN, nor ENVOY_TOKEN, which Dispatch refuses. And no manifest declares a
// ServiceAccount: the load-test Role may write none (the deployment's cluster grants fence them),
// so the deployment declares the two accounts in the namespace and the manifests name them.
func TestTheLoadtestListenerTakesItsCallersProjectedTokens(t *testing.T) {
	const projectedToken = "/var/run/secrets/envoy-listener/token"
	rendered := renderCopyManifests(t)
	listener := envOf(t, podSpec(t, rendered["listener.yaml"], "Deployment", "listener"), "envoy-listener")
	audience := listener["ENVOY_OIDC_AUDIENCE"].Value
	if audience == "" || listener["ENVOY_OIDC_ISSUER"].Value == "" {
		t.Fatalf("the listener verifies no projected token: ENVOY_OIDC_ISSUER %q, ENVOY_OIDC_AUDIENCE %q", listener["ENVOY_OIDC_ISSUER"].Value, audience)
	}
	accounts := strings.FieldsFunc(listener["ENVOY_OIDC_ACCOUNTS"].Value, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
	callers := []struct {
		caller, manifest, kind, name, container, account string
	}{
		{"the run's Dispatch", "dispatch.yaml", "Deployment", "dispatch", "envoy-dispatch", "dispatch-loadtest"},
		{"the runner pod", "runner.yaml", "Pod", "runner", "runner", "loadgen-runner"},
	}
	for _, c := range callers {
		spec := podSpec(t, rendered[c.manifest], c.kind, c.name)
		subject := "system:serviceaccount:" + copyNamespace + ":" + c.account
		if spec.ServiceAccountName != c.account {
			t.Errorf("%s (%s) runs as ServiceAccount %q, want %s, the account its projected token names", c.caller, c.manifest, spec.ServiceAccountName, c.account)
		}
		if !slices.Contains(accounts, subject) {
			t.Errorf("%s presents a token of %s, which the listener's ENVOY_OIDC_ACCOUNTS (%q) does not list, so the listener refuses it", c.caller, subject, listener["ENVOY_OIDC_ACCOUNTS"].Value)
		}
		if got := listenerTokenMount(spec, audience); got != projectedToken {
			t.Errorf("%s (%s) mounts its projected token for the listener's audience %q at %q, want it read-only at %s", c.caller, c.manifest, audience, got, projectedToken)
		}
		if _, ok := envOf(t, spec, c.container)["ENVOY_TOKEN"]; ok {
			t.Errorf("%s (%s) is given ENVOY_TOKEN, a shared bearer", c.caller, c.manifest)
		}
	}
	if len(accounts) != len(callers) {
		t.Errorf("the listener's ENVOY_OIDC_ACCOUNTS lists %q, want the %d callers' accounts and no other", accounts, len(callers))
	}
	if principal := listener["ENVOY_DISPATCH_PRINCIPAL"].Value; principal != "system:serviceaccount:"+copyNamespace+":dispatch-loadtest" {
		t.Errorf("the listener's ENVOY_DISPATCH_PRINCIPAL is %q, want Dispatch's own account: a role mark is taken from it alone", principal)
	}
	for m, docs := range rendered {
		for _, d := range docs {
			if d.Kind == "ServiceAccount" {
				t.Errorf("%s declares ServiceAccount %s: the load-test Role writes no ServiceAccount, so the deployment declares the copy's accounts and the manifests only name them", m, d.Metadata.Name)
			}
			if _, ok := d.StringData["ENVOY_API_TOKEN"]; ok {
				t.Errorf("%s's %s %s holds ENVOY_API_TOKEN, the listener's shared token", m, d.Kind, d.Metadata.Name)
			}
			for _, spec := range []manifestPodSpec{d.Spec.manifestPodSpec, d.Spec.Template.Spec} {
				for _, container := range spec.Containers {
					for _, e := range container.Env {
						if e.Name == "ENVOY_API_TOKEN" {
							t.Errorf("%s's %s %s gives container %s ENVOY_API_TOKEN, the listener's shared token", m, d.Kind, d.Metadata.Name, container.Name)
						}
					}
				}
			}
		}
	}

	flags, err := clusterShell(t, `NS=$1; endpoints step-1; printf '%s\n' "${ENDPOINTS[@]}"`, copyNamespace)
	if err != nil {
		t.Fatalf("cluster.sh's endpoints: %v", err)
	}
	args := strings.Split(strings.TrimSpace(flags), "\n")
	if i := slices.Index(args, "-listener-token-file"); i < 0 || i+1 >= len(args) || args[i+1] != projectedToken {
		t.Errorf("cluster.sh gives a runner job %q, want -listener-token-file %s, the runner's own projected token", args, projectedToken)
	}
	if slices.Contains(args, "-listener-token") {
		t.Errorf("cluster.sh gives a runner job -listener-token: %q", args)
	}
}
