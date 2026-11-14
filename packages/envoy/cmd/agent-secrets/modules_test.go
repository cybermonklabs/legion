// packages/envoy/cmd/agent-secrets/modules_test.go
//
// The module lists of the two binaries that read a stored value: agent-secrets, which decodes a
// value before it writes one, and the broker, which mints from it. Each list is every module its
// binary links, so a module that enters either is weighed before it ships. The decoder
// (secrets.ParseValue) is the code each minter runs and the client calls, and the client links
// every module the broker's secrets package needs. The broker is built from here, beside the
// client, so that one test run holds both (ci_job_reads.py names this cross-build).
package main

import (
	"debug/buildinfo"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// agentSecretsModules is every module the agent-secrets binary links, by path without its
// version, as debug/buildinfo reads them from the binary built with CGO_ENABLED=0, as the
// release builds it, for linux (.github/go-release-targets.json) and for darwin
// (.github/agent-secrets-client-targets.json), the GOOSes the release builds the client for: the
// two lists are the same.
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

// TestTheAgentSecretsBinaryLinksItsListedModules builds agent-secrets in this directory
// for each GOOS the release builds it for, as the release builds it, and holds the modules the
// binary links to agentSecretsModules.
func TestTheAgentSecretsBinaryLinksItsListedModules(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			binary := cmdtest.BuildCommand(t, cmdtest.Command{Name: "agent-secrets", Env: append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos)})
			requireModules(t, binary, agentSecretsModules)
		})
	}
}

// TestTheBrokerBinaryLinksItsListedModules builds the broker, ../broker, for linux as its
// image builds it, and holds the modules the binary links to brokerModules.
func TestTheBrokerBinaryLinksItsListedModules(t *testing.T) {
	binary := cmdtest.BuildCommand(t, cmdtest.Command{Name: "broker", Dir: filepath.Join("..", "broker"), Target: ".", Env: append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")})
	requireModules(t, binary, brokerModules)
}

// requireModules fails t unless the binary links exactly the modules want lists, by path without
// version, naming each module it links that want does not list and each want lists that it does
// not link. A module that enters is a dependency to weigh before it ships; one that leaves is
// taken off the list by the change that drops it.
func requireModules(t *testing.T, binary string, want []string) {
	t.Helper()
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatalf("read the build info of %s: %v", binary, err)
	}
	var linked []string
	for _, dep := range info.Deps {
		linked = append(linked, dep.Path)
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
		t.Fatalf("%s links modules its list does not name: %q; and no longer links modules its list names: %q", filepath.Base(binary), entered, left)
	}
}
