// packages/envoy/cmd/cipher/modules_test.go
//
// The module lists of the two binaries that read a stored value: agent-secrets, which decodes a
// value before it writes one, and the broker, which mints from it. Each list is every module its
// binary links, so a module that enters either is weighed before it ships. The decoder
// (secrets.ParseValue) is the code each minter runs and the client calls, and the client links
// every module the broker's secrets package needs. The broker's list is read from here, beside the
// client's, so that one test run holds both (ci_job_reads.py names this listing).
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// agentSecretsModules is every module the agent-secrets binary links, by path without its
// version: the modules of the non-standard packages its build compiles, which are the ones
// debug/buildinfo records in the binary, with CGO_ENABLED=0, as the release builds it, for linux
// (.github/go-release-targets.json) and for darwin (.github/agent-secrets-client-targets.json),
// the GOOSes the release builds the client for: the two lists are the same.
var agentSecretsModules = []string{
	"cloud.google.com/go/auth",
	"cloud.google.com/go/auth/oauth2adapt",
	"cloud.google.com/go/compute/metadata",
	"cloud.google.com/go/iam",
	"cloud.google.com/go/secretmanager",
	"github.com/aws/aws-sdk-go-v2",
	"github.com/aws/aws-sdk-go-v2/config",
	"github.com/aws/aws-sdk-go-v2/credentials",
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds",
	"github.com/aws/aws-sdk-go-v2/internal/configsources",
	"github.com/aws/aws-sdk-go-v2/internal/endpoints/v2",
	"github.com/aws/aws-sdk-go-v2/internal/v4a",
	"github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding",
	"github.com/aws/aws-sdk-go-v2/service/internal/presigned-url",
	"github.com/aws/aws-sdk-go-v2/service/kms",
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager",
	"github.com/aws/aws-sdk-go-v2/service/signin",
	"github.com/aws/aws-sdk-go-v2/service/sso",
	"github.com/aws/aws-sdk-go-v2/service/ssooidc",
	"github.com/aws/aws-sdk-go-v2/service/sts",
	"github.com/aws/smithy-go",
	"github.com/cespare/xxhash/v2",
	"github.com/coreos/go-oidc/v3",
	"github.com/felixge/httpsnoop",
	"github.com/go-jose/go-jose/v4",
	"github.com/go-logr/logr",
	"github.com/go-logr/stdr",
	"github.com/google/s2a-go",
	"github.com/google/uuid",
	"github.com/googleapis/enterprise-certificate-proxy",
	"github.com/googleapis/gax-go/v2",
	"go.opentelemetry.io/auto/sdk",
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc",
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp",
	"go.opentelemetry.io/otel",
	"go.opentelemetry.io/otel/log",
	"go.opentelemetry.io/otel/metric",
	"go.opentelemetry.io/otel/trace",
	"golang.org/x/crypto",
	"golang.org/x/net",
	"golang.org/x/oauth2",
	"golang.org/x/sync",
	"golang.org/x/sys",
	"golang.org/x/term",
	"golang.org/x/text",
	"golang.org/x/time",
	"google.golang.org/api",
	"google.golang.org/genproto",
	"google.golang.org/genproto/googleapis/api",
	"google.golang.org/genproto/googleapis/rpc",
	"google.golang.org/grpc",
	"google.golang.org/protobuf",
}

// brokerModules is every module the broker binary links, read as agentSecretsModules is, from
// the broker built with CGO_ENABLED=0 for linux, the one GOOS its image runs
// (packages/envoy/docker/Dockerfile).
var brokerModules = []string{
	"authelia.com/provider/oauth2",
	"cloud.google.com/go/auth",
	"cloud.google.com/go/auth/oauth2adapt",
	"cloud.google.com/go/compute/metadata",
	"cloud.google.com/go/iam",
	"cloud.google.com/go/secretmanager",
	"github.com/anthropics/anthropic-sdk-go",
	"github.com/aws/aws-sdk-go-v2",
	"github.com/aws/aws-sdk-go-v2/config",
	"github.com/aws/aws-sdk-go-v2/credentials",
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds",
	"github.com/aws/aws-sdk-go-v2/feature/rds/auth",
	"github.com/aws/aws-sdk-go-v2/internal/configsources",
	"github.com/aws/aws-sdk-go-v2/internal/endpoints/v2",
	"github.com/aws/aws-sdk-go-v2/internal/v4a",
	"github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding",
	"github.com/aws/aws-sdk-go-v2/service/internal/presigned-url",
	"github.com/aws/aws-sdk-go-v2/service/kms",
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager",
	"github.com/aws/aws-sdk-go-v2/service/signin",
	"github.com/aws/aws-sdk-go-v2/service/sso",
	"github.com/aws/aws-sdk-go-v2/service/ssooidc",
	"github.com/aws/aws-sdk-go-v2/service/sts",
	"github.com/aws/smithy-go",
	"github.com/bahlo/generic-list-go",
	"github.com/buger/jsonparser",
	"github.com/cespare/xxhash/v2",
	"github.com/coder/websocket",
	"github.com/coreos/go-oidc/v3",
	"github.com/dgraph-io/ristretto",
	"github.com/dustin/go-humanize",
	"github.com/felixge/httpsnoop",
	"github.com/go-jose/go-jose/v4",
	"github.com/go-logr/logr",
	"github.com/go-logr/stdr",
	"github.com/google/s2a-go",
	"github.com/google/uuid",
	"github.com/googleapis/enterprise-certificate-proxy",
	"github.com/googleapis/gax-go/v2",
	"github.com/grafana/ai-sdk",
	"github.com/grafana/ai-sdk/providers/anthropic",
	"github.com/grafana/ai-sdk/providers/azure",
	"github.com/grafana/ai-sdk/providers/openai",
	"github.com/grafana/ai-sdk/providers/openai-compatible",
	"github.com/hashicorp/go-cleanhttp",
	"github.com/hashicorp/go-retryablehttp",
	"github.com/invopop/jsonschema",
	"github.com/jackc/pgpassfile",
	"github.com/jackc/pgservicefile",
	"github.com/jackc/pgx/v5",
	"github.com/jackc/puddle/v2",
	// The NATS client and the auth callout's keys and claims: the broker runs the hub's NATS
	// auth service (internal/cipher/natscallout), which connects to the hub as a NATS client and
	// signs each answer. klauspost/compress and nuid come in under nats.go.
	"github.com/klauspost/compress",
	"github.com/nats-io/jwt/v2",
	"github.com/nats-io/nats.go",
	"github.com/nats-io/nkeys",
	"github.com/nats-io/nuid",
	"github.com/openai/openai-go/v3",
	"github.com/pb33f/go-yaml",
	"github.com/pb33f/ordered-map/v2",
	"github.com/pkg/errors",
	"github.com/santhosh-tekuri/jsonschema/v6",
	"github.com/standard-webhooks/standard-webhooks/libraries",
	"github.com/stretchr/testify",
	"github.com/tetratelabs/wazero",
	"github.com/tidwall/gjson",
	"github.com/tidwall/match",
	"github.com/tidwall/pretty",
	"github.com/tidwall/sjson",
	"github.com/wasilibs/go-pgquery",
	"github.com/wasilibs/wazero-helpers",
	"go.opentelemetry.io/auto/sdk",
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc",
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp",
	"go.opentelemetry.io/otel",
	"go.opentelemetry.io/otel/log",
	"go.opentelemetry.io/otel/metric",
	"go.opentelemetry.io/otel/trace",
	"go.yaml.in/yaml/v3",
	"golang.org/x/crypto",
	"golang.org/x/net",
	"golang.org/x/oauth2",
	"golang.org/x/sync",
	"golang.org/x/sys",
	"golang.org/x/text",
	"golang.org/x/time",
	"google.golang.org/api",
	"google.golang.org/genproto",
	"google.golang.org/genproto/googleapis/api",
	"google.golang.org/genproto/googleapis/rpc",
	"google.golang.org/grpc",
	"google.golang.org/protobuf",
}

// TestTheAgentSecretsBinaryLinksItsListedModules lists the modules agent-secrets links for each
// GOOS the release builds it for, with the release's CGO_ENABLED=0, and holds them to
// agentSecretsModules.
func TestTheAgentSecretsBinaryLinksItsListedModules(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			requireModules(t, "agent-secrets for "+goos, ".", agentSecretsModules, "CGO_ENABLED=0", "GOOS="+goos)
		})
	}
}

// TestTheAgentSecretsBinaryBuildsWithoutThePrompt builds agent-secrets for targets the release does
// not ship and the README says build without the value prompt, a value piped in standing for it:
// Windows, FreeBSD, and a 32-bit Linux architecture.
func TestTheAgentSecretsBinaryBuildsWithoutThePrompt(t *testing.T) {
	for _, target := range []struct{ goos, goarch string }{{"windows", "amd64"}, {"freebsd", "amd64"}, {"linux", "386"}} {
		t.Run(target.goos+"/"+target.goarch, func(t *testing.T) {
			cmdtest.BuildCommand(t, cmdtest.Command{Name: "agent-secrets", Env: append(os.Environ(), "CGO_ENABLED=0", "GOOS="+target.goos, "GOARCH="+target.goarch)})
		})
	}
}

// TestTheBrokerBinaryLinksItsListedModules lists the modules the broker, ../cipherd, links for
// linux as its image builds it, and holds them to brokerModules.
func TestTheBrokerBinaryLinksItsListedModules(t *testing.T) {
	t.Parallel()
	requireModules(t, "the broker for linux", filepath.Join("..", "cipherd"), brokerModules, "CGO_ENABLED=0", "GOOS=linux")
}

// requireModules fails t unless the command in dir, built with env, links exactly the modules
// want lists, by path without version, naming each module it links that want does not list and
// each want lists that it does not link. A module that enters is a dependency to weigh before it
// ships; one that leaves is taken off the list by the change that drops it.
//
// The modules are read with go list -deps under env, not from a built binary: a binary's
// debug/buildinfo records the module of every non-standard package its build compiles, and
// go list -deps lists those packages for the same GOOS, GOARCH and CGO_ENABLED without compiling
// one, where a build of each GOOS compiled every dependency again on each run (the build cache
// holds none of them). The listing must name the command's own package, so an empty or failed
// listing fails rather than reads as a binary that links nothing.
func requireModules(t *testing.T, what, dir string, want []string, env ...string) {
	t.Helper()
	list := exec.Command("go", "list", "-deps", "-f", "{{.Name}} {{with .Module}}{{if .Main}}main{{else}}{{.Path}}{{end}}{{else}}std{{end}}", ".")
	list.Dir = dir
	list.Env = append(os.Environ(), env...)
	out, err := list.Output()
	if err != nil {
		stderr := ""
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = string(exit.Stderr)
		}
		t.Fatalf("go list -deps of %s in %s with %q: %v\n%s", what, dir, env, err, stderr)
	}
	var linked []string
	commands := 0
	for line := range strings.Lines(string(out)) {
		name, module, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			t.Fatalf("go list -deps of %s printed %q, not a package name and its module", what, line)
		}
		if name == "main" {
			commands++ // a main package is imported by nothing, so this is the command itself
		}
		if module != "main" && module != "std" && !slices.Contains(linked, module) {
			linked = append(linked, module)
		}
	}
	if commands != 1 {
		t.Fatalf("go list -deps of %s in %s listed %d main packages, not the one command, so it read no build: %q", what, dir, commands, out)
	}
	slices.Sort(linked)
	var entered, left []string
	for _, path := range linked {
		if !slices.Contains(want, path) {
			entered = append(entered, path)
		}
	}
	for _, path := range want {
		if !slices.Contains(linked, path) {
			left = append(left, path)
		}
	}
	if len(entered) > 0 || len(left) > 0 {
		t.Fatalf("%s links modules its list does not name: %q; and no longer links modules its list names: %q", what, entered, left)
	}
}
