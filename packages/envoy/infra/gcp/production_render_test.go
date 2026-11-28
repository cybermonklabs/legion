package gcp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/integration/fake"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/pulumi/pulumi/sdk/v3/go/propertyrpc"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	comProvider "github.com/pulumi/pulumi/sdk/v3/go/pulumi/provider"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/infra/provider"
)

// This file is an instrument: it renders what a production consumer's program gets from the
// plugin, so that two versions of the plugin can be compared byte for byte. Each shape below is
// one component call with every input a production program sets and none it leaves unset, each
// value a placeholder of the same kind as the real one (a pinned image as name:tag@digest where the
// real one carries a tag, a secret from another resource as a secret with that resource's URN as
// its dependency, and so on). Each is constructed through the plugin's own Construct, as a
// consumer's `pulumi up` does, and the document holds every register and read request the
// construct sent the engine, sorted by URN, each invoke it made, and the construct's state.
//
// The document is the plugin's render alone. Two things a production apply also has are not in
// it: a consumer's own resource transforms, which the engine applies to a component's children
// after the plugin registers them, and the objects a Helm chart renders, which the kubernetes
// provider expands from the chart's values (the values are in the document; the objects are not).
//
// With PRODUCTION_RENDER_OUT set, TestProductionRender writes the document to that file. Without
// it, the test compares the document against testdata/production-render.golden and fails on
// any byte of difference, printing testify's whole diff of the two and the command that writes the
// golden again (prodRenderRegenerate). TestProductionRenderSeesAOneInputChange is the instrument's
// control: one input changed must change the document, and the comparison must report it.
// TestProductionRenderHarnessMatchesPulumitest holds the construct path the document takes to
// pulumitest.Construct's, which cannot serve the PriorityClass reads production's shapes make.

// prodRenderOutEnv names the file TestProductionRender writes the document to, in place of the
// comparison against prodRenderGolden. prodRenderRegenerate is the command, run from
// packages/envoy/infra, that writes the golden from the tree's render; it names the file by an
// absolute path, since go test runs the test in gcp/.
const (
	prodRenderOutEnv     = "PRODUCTION_RENDER_OUT"
	prodRenderGolden     = "production-render.golden"
	prodRenderRegenerate = prodRenderOutEnv + "=$PWD/gcp/testdata/" + prodRenderGolden + " go test ./gcp -run '^TestProductionRender$'"
)

// prodRenderConsumer is the URN prefix of a resource of the consumer's own program, which an input
// comes from or the component depends on.
const prodRenderConsumer = "urn:pulumi:s::p::"

// Seven user public keys, valid nkeys that the module's tests already use as placeholders, one per
// NATS user a production hub carries. No user shares another's key.
const (
	prodRenderKeyDispatch    = "UDCQRANOFSHHJXENVBTXNFSRUDIUXGXPNWC5ABJFJ7GBPO27F7UIBVXD"
	prodRenderKeyDaemon      = "UCYF4CJJNU55NFRALQXGPOAZRM5R5JBXI6OAZB2BHWJ4QTTB5XHFENDG"
	prodRenderKeyListener    = "UCKA5BQ7T62SA4THAXWQYT3TKRWD6SGKDXMPYZR2D7EQBYBTWA4RPYVW"
	prodRenderKeyMeasure     = "UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO"
	prodRenderKeyMove        = "UD466L6EBCM3YY5HEGHJANNTN4LSKTSUXTH7RILHCKEQMQHTBNLHJJXT"
	prodRenderKeySessions    = "UCNPQVMRWOOJWCD7XDVLH6MEOVFBMTV4LAQSKZYROSETYJVFAWDMNKVA"
	prodRenderKeyHubListener = "UBZEZDPQNFYMZ4J4XOLTB2IN5PBPZ6FZEGY55R623A4GPBPZLTQFKDYV"
)

// The pinned images, each of the form production pins it in: a digest alone for an Envoy build,
// a tag and a digest for an upstream image.
var (
	prodRenderEnvoyImage     = "example.invalid/envoy@sha256:" + strings.Repeat("1", 64)
	prodRenderTailscaleImage = "example.invalid/tailscale:v1.0.0@sha256:" + strings.Repeat("2", 64)
	prodRenderProxyImage     = "example.invalid/cloud-sql-proxy:2.0.0@sha256:" + strings.Repeat("3", 64)
	prodRenderToolsImage     = "example.invalid/envoy@sha256:" + strings.Repeat("4", 64)
	prodRenderNatsImage      = "example.invalid/nats:2.0.0-alpine@sha256:" + strings.Repeat("5", 64)
	prodRenderReloaderImage  = "example.invalid/nats-server-config-reloader:0.1.0@sha256:" + strings.Repeat("6", 64)
	prodRenderExporterImage  = "example.invalid/prometheus-nats-exporter:0.1.0@sha256:" + strings.Repeat("7", 64)
	prodRenderHubEnvoyImage  = "example.invalid/envoy@sha256:" + strings.Repeat("8", 64)
)

// prodRenderKubeconfig is a kubeconfig of the shape a GKE cluster's is: one cluster, one context
// and a user signing in through the GKE auth plugin.
const prodRenderKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: example-cluster
  cluster:
    server: https://192.0.2.1
    certificate-authority-data: ZXhhbXBsZS1jYQ==
contexts:
- name: example-cluster
  context:
    cluster: example-cluster
    user: example-cluster
current-context: example-cluster
users:
- name: example-cluster
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: gke-gcloud-auth-plugin
      provideClusterInfo: true
`

// The explicit permission sets of two kinds of production user: the measurement workflow's, which
// reads JetStream's state and one lease key and owns one test stream, and the stream move's, which
// reads every stream and, on the hub the streams leave, takes their snapshots, and on the hub they
// arrive on, restores them. Each subscribes its own inbox prefix alone.
var (
	prodRenderMeasurePublish = []string{
		"$JS.API.INFO",
		"$JS.API.STREAM.NAMES",
		"$JS.API.STREAM.LIST",
		"$JS.API.STREAM.INFO.*",
		"$JS.API.CONSUMER.NAMES.*",
		"$JS.API.CONSUMER.LIST.*",
		"$JS.API.CONSUMER.INFO.*.*",
		"$JS.API.DIRECT.GET.KV_example_bucket.$KV.example_bucket.lease.>",
		"$JS.API.STREAM.CREATE.EXAMPLE_MEASURE",
		"$JS.API.STREAM.UPDATE.EXAMPLE_MEASURE",
		"$JS.API.STREAM.DELETE.EXAMPLE_MEASURE",
		"example-measure.>",
	}
	prodRenderMeasureSubscribe = []string{"_INBOX_example_measure.>"}
	prodRenderMoveSource       = []string{
		"$JS.API.INFO",
		"$JS.API.STREAM.NAMES",
		"$JS.API.STREAM.LIST",
		"$JS.API.STREAM.INFO.*",
		"$JS.API.STREAM.UPDATE.*",
		"$JS.API.STREAM.SNAPSHOT.*",
		"$JS.SNAPSHOT.ACK.>",
		"$JS.API.CONSUMER.NAMES.*",
		"$JS.API.CONSUMER.LIST.*",
		"$JS.API.CONSUMER.INFO.*.*",
	}
	prodRenderMoveDestination = []string{
		"$JS.API.INFO",
		"$JS.API.STREAM.NAMES",
		"$JS.API.STREAM.LIST",
		"$JS.API.STREAM.INFO.*",
		"$JS.API.STREAM.UPDATE.*",
		"$JS.API.STREAM.RESTORE.*",
		"$JS.SNAPSHOT.RESTORE.>",
		"$JS.API.STREAM.DELETE.*",
		"$JS.API.CONSUMER.NAMES.*",
		"$JS.API.CONSUMER.LIST.*",
		"$JS.API.CONSUMER.INFO.*.*",
	}
	prodRenderMoveSubscribe = []string{"_INBOX_example_move.>"}
)

// prodRenderShape is one component call of a production program: the component's type and name,
// its inputs, the provider references the program gives it and the resources its DependsOn names.
type prodRenderShape struct {
	shape     string
	typ       string
	name      string
	inputs    map[string]property.Value
	providers map[tokens.Package]p.ProviderReference
	dependsOn []resource.URN
}

// prodRenderShapes are the production calls, in the document's order: gcp:Dispatch as production
// calls it, and again with oidcAudience, Dispatch's service-token verifier, which a consumer sets
// once its pin carries the input; gcp:ClusteredNatsHub without its tailnet block, as production
// runs it before the hub's cutover, and with the block and its listener, as the cutover writes it;
// and gcp:NatsHub, the VM hub.
func prodRenderShapes() []prodRenderShape {
	withAudience := prodRenderDispatchInputs()
	withAudience["oidcAudience"] = prodRenderString("example-dispatch-audience")
	return []prodRenderShape{
		prodRenderDispatch("dispatch", prodRenderDispatchInputs()),
		prodRenderDispatch("dispatch-with-oidc-audience", withAudience),
		{
			shape: "clustered-hub", typ: "envoy-infra:gcp:ClusteredNatsHub", name: "example-clustered-hub",
			inputs: prodRenderClusteredInputs(),
		},
		{
			shape: "clustered-hub-with-tailnet", typ: "envoy-infra:gcp:ClusteredNatsHub", name: "example-clustered-hub",
			inputs: prodRenderClusteredTailnetInputs(),
			// The cutover's update waits for the window that frees the hub's tailnet name.
			dependsOn: []resource.URN{prodRenderFromConsumer("kubernetes:batch/v1:Job", "example-cutover-switch")},
		},
		{
			shape: "vm-hub", typ: "envoy-infra:gcp:NatsHub", name: "example-hub",
			inputs:    prodRenderVMHubInputs(),
			providers: map[tokens.Package]p.ProviderReference{"gcp": prodRenderProvider("gcp")},
			// The listener's seed secret exists before the hub boots.
			dependsOn: []resource.URN{prodRenderFromConsumer("gcp:secretmanager/secret:Secret", "example-nats-seed-example-hub")},
		},
	}
}

// prodRenderDispatch is a gcp:Dispatch call with the three providers production's program gives
// it, and the resources its DependsOn names: the migration census Job and the GKE Secret Syncs
// of the GitHub App's key and of the decision model's key.
func prodRenderDispatch(shape string, inputs map[string]property.Value) prodRenderShape {
	return prodRenderShape{
		shape: shape, typ: "envoy-infra:gcp:Dispatch", name: "example-dispatch",
		inputs: inputs,
		providers: map[tokens.Package]p.ProviderReference{
			"gcp": prodRenderProvider("gcp"), "random": prodRenderProvider("random"), "tailscale": prodRenderProvider("tailscale"),
		},
		dependsOn: []resource.URN{
			prodRenderFromConsumer("kubernetes:batch/v1:Job", "example-dispatch-census"),
			prodRenderFromConsumer("kubernetes:secret-sync.gke.io/v1:SecretSync", "example-dispatch-github-app-key"),
			prodRenderFromConsumer("kubernetes:secret-sync.gke.io/v1:SecretSync", "example-dispatch-decision-key"),
		},
	}
}

// prodRenderDispatchInputs are every input production's gcp:Dispatch call sets: the tailnet front
// door, Google sign-in, the in-cluster hub, the database and the pods' sizes, the secrets
// broker's relay, the Azure embedder and answerer, and the GitHub App.
func prodRenderDispatchInputs() map[string]property.Value {
	return map[string]property.Value{
		"region":         prodRenderString("us-central1"),
		"hostname":       prodRenderString("example-dispatch"),
		"namespace":      prodRenderString("example-dispatch"),
		"tailnetDnsName": prodRenderString("example-tailnet.ts.net"),
		"tags":           prodRenderStrings("tag:example-dispatch", "tag:example-dispatch-front-door"),
		// The program's own tailnet key resource mints it.
		"tailnetAuthKey": property.New("example-tailnet-auth-key").WithSecret(true).
			WithDependencies([]resource.URN{prodRenderFromConsumer("tailscale:index/tailnetKey:TailnetKey", "example-dispatch")}),
		"envoyImage":               prodRenderString(prodRenderEnvoyImage),
		"tailscaleImage":           prodRenderString(prodRenderTailscaleImage),
		"frontDoorToolsImage":      prodRenderString(prodRenderToolsImage),
		"cloudSqlProxyImage":       prodRenderString(prodRenderProxyImage),
		"replicas":                 prodRenderInt(1),
		"frontDoorReplicas":        prodRenderInt(2),
		"listenerReplicas":         prodRenderInt(1),
		"appPdbMinAvailable":       prodRenderInt(1),
		"frontDoorPdbMinAvailable": prodRenderInt(1),
		"listenerPdbMinAvailable":  prodRenderInt(1),
		"priorityClassName":        prodRenderString("example-shared-infra"),
		"natsUrl":                  prodRenderString("nats://example-hub.envoy.svc.cluster.local:4222"),
		"natsSeedSecret":           prodRenderString("projects/example-project/secrets/example-nats-seed-example-dispatch"),
		"natsInCluster":            property.New(true),
		"signInClientId":           prodRenderString("000000000000-example.apps.googleusercontent.com"),
		// A secret of the program's configuration.
		"signInClientSecret":       property.New("example-client-secret").WithSecret(true),
		"signInDomain":             prodRenderString("example.com"),
		"deletionProtection":       property.New(true),
		"databaseVersion":          prodRenderString("POSTGRES_18"),
		"databaseTier":             prodRenderString("db-custom-4-15360"),
		"databaseAvailabilityType": prodRenderString("REGIONAL"),
		"appCpu":                   prodRenderString("8"),
		"appMemory":                prodRenderString("12Gi"),
		"appEphemeralStorage":      prodRenderString("4Gi"),
		"proxyCpu":                 prodRenderString("250m"),
		"proxyMemory":              prodRenderString("512Mi"),
		"pprofPort":                prodRenderInt(6060),
		"existingNamespace":        property.New(true),
		"allowedNamespaces": prodRenderStrings("envoy", "example-legion", "example-batch-a", "example-batch-b",
			"example-batch-c", "example-sessions"),
		"agentSecretsUrl":           prodRenderString("https://example-secrets.example-tailnet.ts.net"),
		"agentSecretsAudience":      prodRenderString("example-secrets-audience"),
		"azureEmbedEndpoint":        prodRenderString("https://example-models.services.ai.azure.com/models"),
		"azureEmbedDeployment":      prodRenderString("example-embed"),
		"azureEmbedTokensPerMinute": prodRenderInt(500000),
		"azureTenantId":             prodRenderString("00000000-0000-4000-8000-000000000001"),
		"azureClientId":             prodRenderString("00000000-0000-4000-8000-000000000002"),
		"azureChatEndpoint":         prodRenderString("https://example-models.services.ai.azure.com/models"),
		"azureChatDeployment":       prodRenderString("example-chat"),
		"azureChatApiVersion":       prodRenderString("2024-05-01-preview"),
		"azureChatScope":            prodRenderString("https://cognitiveservices.azure.com/.default"),
		"azureChatTokensPerMinute":  prodRenderInt(500000),
		"appId":                     prodRenderString("123456"),
		"appClientId":               prodRenderString("Iv23example0000000000"),
		"appPrivateKeySecretName":   prodRenderString("example-dispatch-github-app-key"),
		"appPrivateKeySecretKey":    prodRenderString("private-key.pem"),
	}
}

// prodRenderClusteredInputs are every input production's gcp:ClusteredNatsHub call sets while the
// hub carries no tailnet block. The kubeconfig is the cluster's, read from another stack and made
// secret by the program.
func prodRenderClusteredInputs() map[string]property.Value {
	return map[string]property.Value{
		"kubeconfig":        property.New(prodRenderKubeconfig).WithSecret(true),
		"namespace":         prodRenderString("envoy"),
		"name":              prodRenderString("example-nats"),
		"serviceName":       prodRenderString("example-nats"),
		"natsImage":         prodRenderString(prodRenderNatsImage),
		"reloaderImage":     prodRenderString(prodRenderReloaderImage),
		"exporterImage":     prodRenderString(prodRenderExporterImage),
		"priorityClassName": prodRenderString("example-shared-infra"),
		"clientNamespaces":  prodRenderStrings("envoy", "example-legion", "example-dispatch", "example-sessions"),
		"monitorNamespaces": prodRenderStrings("example-legion"),
		"streamMaxBytes":    prodRenderInt(40 << 30),
		"natsCpu":           prodRenderString("1"),
		"natsMemory":        prodRenderString("5Gi"),
		"users":             prodRenderUsers(prodRenderMoveDestination),
		"sessionUser":       prodRenderSessionUser(),
	}
}

// prodRenderClusteredTailnetInputs are the clustered hub's inputs once the cutover writes the
// tailnet block: the hosts serving the hub's tailnet name, its session listener, the hosts' auth
// key and the copies JetStream keeps.
func prodRenderClusteredTailnetInputs() map[string]property.Value {
	inputs := prodRenderClusteredInputs()
	inputs["tailnet"] = prodRenderObject(map[string]property.Value{
		"serviceName":    prodRenderString("example-hub"),
		"tailnetDnsName": prodRenderString("example-tailnet.ts.net"),
		"tags":           prodRenderStrings("tag:example-hub-host"),
		"replicas":       prodRenderInt(2),
		"tailscaleImage": prodRenderString(prodRenderTailscaleImage),
		"listener": prodRenderObject(map[string]property.Value{
			"image":          prodRenderString(prodRenderHubEnvoyImage),
			"machineId":      prodRenderString("example-hub"),
			"nkey":           prodRenderString(prodRenderKeyHubListener),
			"seedSecretName": prodRenderString("example-hub-listener-seed"),
			"seedSecretKey":  prodRenderString("nats-seed"),
			"cpu":            prodRenderString("500m"),
			"memory":         prodRenderString("1Gi"),
		}),
	})
	inputs["tailnetAuthKey"] = property.New("example-tailnet-auth-key").WithSecret(true).
		WithDependencies([]resource.URN{prodRenderFromConsumer("tailscale:index/tailnetKey:TailnetKey", "example-hub-host")})
	inputs["natsReplicas"] = prodRenderInt(3)
	return inputs
}

// prodRenderVMHubInputs are every input production's gcp:NatsHub call sets.
func prodRenderVMHubInputs() map[string]property.Value {
	return map[string]property.Value{
		"users": prodRenderUsers(prodRenderMoveSource),
		"tailnetAuthKey": property.New("example-tailnet-auth-key").WithSecret(true).
			WithDependencies([]resource.URN{prodRenderFromConsumer("tailscale:index/tailnetKey:TailnetKey", "example-hub")}),
		"hostname":       prodRenderString("example-hub"),
		"tags":           prodRenderStrings("tag:example-hub"),
		"zone":           prodRenderString("us-central1-a"),
		"machineType":    prodRenderString("e2-standard-4"),
		"natsImage":      prodRenderString(prodRenderNatsImage),
		"tailscaleImage": prodRenderString(prodRenderTailscaleImage),
		"tailnetDnsName": prodRenderString("example-tailnet.ts.net"),
		"listener": prodRenderObject(map[string]property.Value{
			"image":      prodRenderString(prodRenderHubEnvoyImage),
			"seedSecret": prodRenderString("example-nats-seed-example-hub"),
			"nkey":       prodRenderString(prodRenderKeyHubListener),
		}),
		"sessionUser": prodRenderSessionUser(),
	}
}

// prodRenderUsers are a production hub's NATS users, one of each kind its list holds, in its
// order: Dispatch's, a Legion daemon's, the in-cluster listener's, the measurement workflow's and
// the stream move's, whose publish list move is the hub's side of the move.
func prodRenderUsers(move []string) property.Value {
	return property.New(property.NewArray([]property.Value{
		prodRenderObject(map[string]property.Value{
			"name": prodRenderString("example-dispatch"), "nkey": prodRenderString(prodRenderKeyDispatch),
			"dispatchMachineId": prodRenderString("example-dispatch"),
		}),
		prodRenderObject(map[string]property.Value{
			"name": prodRenderString("example-daemon"), "nkey": prodRenderString(prodRenderKeyDaemon),
			"daemon": property.New(true),
		}),
		prodRenderObject(map[string]property.Value{
			"name": prodRenderString("example-listener"), "nkey": prodRenderString(prodRenderKeyListener),
			"listenerMachineId": prodRenderString("example-listener"),
		}),
		prodRenderObject(map[string]property.Value{
			"name": prodRenderString("example-measure"), "nkey": prodRenderString(prodRenderKeyMeasure),
			"permissions": prodRenderPermissions(prodRenderMeasurePublish, prodRenderMeasureSubscribe),
		}),
		prodRenderObject(map[string]property.Value{
			"name": prodRenderString("example-move"), "nkey": prodRenderString(prodRenderKeyMove),
			"permissions": prodRenderPermissions(move, prodRenderMoveSubscribe),
		}),
	}))
}

// prodRenderSessionUser is the user a client presenting no credential becomes.
func prodRenderSessionUser() property.Value {
	return prodRenderObject(map[string]property.Value{
		"name": prodRenderString("example-members"), "nkey": prodRenderString(prodRenderKeySessions),
	})
}

func prodRenderPermissions(publish, subscribe []string) property.Value {
	return prodRenderObject(map[string]property.Value{
		"publish":   prodRenderObject(map[string]property.Value{"allow": prodRenderStrings(publish...)}),
		"subscribe": prodRenderObject(map[string]property.Value{"allow": prodRenderStrings(subscribe...)}),
	})
}

func prodRenderString(s string) property.Value { return property.New(s) }

func prodRenderInt(n int) property.Value { return property.New(float64(n)) }

func prodRenderObject(fields map[string]property.Value) property.Value {
	return property.New(property.NewMap(fields))
}

func prodRenderStrings(values ...string) property.Value {
	array := make([]property.Value, len(values))
	for i, v := range values {
		array[i] = property.New(v)
	}
	return property.New(property.NewArray(array))
}

// prodRenderFromConsumer is the URN of the consumer's own resource of type typ named name.
func prodRenderFromConsumer(typ, name string) resource.URN {
	return resource.URN(prodRenderConsumer + typ + "::" + name)
}

// prodRenderProvider is the reference to the consumer's provider of package pkg.
func prodRenderProvider(pkg string) p.ProviderReference {
	return p.ProviderReference{
		Urn: prodRenderFromConsumer("pulumi:providers:"+pkg, pkg+"-example"),
		ID:  resource.ID(pkg + "-example-id"),
	}
}

// prodRenderEngine is the mock engine behind one construct: the outputs the components read, as
// their providers compute them, and a record of every invoke.
type prodRenderEngine struct {
	mocks   *pulumitest.Mocks
	mu      sync.Mutex
	invokes []pulumi.MockCallArgs
}

func newProdRenderEngine() *prodRenderEngine {
	e := &prodRenderEngine{}
	e.mocks = &pulumitest.Mocks{Computed: prodRenderComputed, CallResult: e.call}
	return e
}

func (e *prodRenderEngine) call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	e.mu.Lock()
	e.invokes = append(e.invokes, args)
	e.mu.Unlock()
	if args.Token == "gcp:organizations/getProject:getProject" {
		return resource.PropertyMap{
			"projectId": resource.NewProperty("example-project"),
			"number":    resource.NewProperty("123456789012"),
		}, nil
	}
	return args.Args, nil
}

// prodRenderComputed answers what a provider computes: an account's email and member, a network's
// and a health check's self link, a template's unique self link, a group's name, a cluster's
// endpoint and CA, a database instance's connection name, a password's result, a Kubernetes
// Secret's resourceVersion and a Kubernetes ServiceAccount's uid.
func prodRenderComputed(args pulumi.MockResourceArgs) (string, resource.PropertyMap) {
	out := resource.PropertyMap{}
	switch args.TypeToken {
	case "gcp:serviceaccount/account:Account":
		if id, ok := args.Inputs["accountId"]; ok && id.IsString() {
			email := id.StringValue() + "@example-project.iam.gserviceaccount.com"
			out["email"] = resource.NewProperty(email)
			out["member"] = resource.NewProperty("serviceAccount:" + email)
		}
	case "gcp:compute/network:Network", "gcp:compute/healthCheck:HealthCheck":
		out["selfLink"] = resource.NewProperty(args.Name + "-self-link")
	case "gcp:compute/instanceTemplate:InstanceTemplate":
		out["selfLinkUnique"] = resource.NewProperty(args.Name + "-self-link-unique")
	case "gcp:compute/instanceGroupManager:InstanceGroupManager":
		out["name"] = resource.NewProperty(args.Name + "-group")
	case "gcp:container/cluster:Cluster":
		out["endpoint"] = resource.NewProperty("example-endpoint")
		out["masterAuth"] = resource.NewProperty(resource.PropertyMap{
			"clusterCaCertificate": resource.NewProperty("ZXhhbXBsZS1jYQ=="),
		})
		out["name"] = resource.NewProperty(args.Name)
	case "gcp:sql/databaseInstance:DatabaseInstance":
		out["connectionName"] = resource.NewProperty("example-project:us-central1:example-dispatch")
	case "random:index/randomPassword:RandomPassword":
		out["result"] = resource.NewProperty(args.Name + "-result")
	case "kubernetes:core/v1:Secret":
		out["metadata"] = resource.NewProperty(registeredMetadataWith(args, "resourceVersion", args.Name+"-resource-version"))
	case "kubernetes:core/v1:ServiceAccount":
		out["metadata"] = resource.NewProperty(registeredMetadataWith(args, "uid", mockServiceAccountUID(args.Name)))
	}
	return "", out
}

// prodRenderDocument constructs every shape and renders what each registered as one canonical
// JSON document: objects' keys sorted, numbers as written, every string holding a newline split
// into {"$lines": [...]} so that a change shows as the lines it changes.
func prodRenderDocument(t *testing.T, shapes []prodRenderShape) []byte {
	t.Helper()
	return prodRenderDocumentBy(t, shapes, prodRenderConstruct)
}

// prodRenderConstructor constructs shape through the plugin on engine's mocks.
type prodRenderConstructor func(t *testing.T, engine *prodRenderEngine, shape prodRenderShape) (p.ConstructResponse, error)

func prodRenderDocumentBy(t *testing.T, shapes []prodRenderShape, construct prodRenderConstructor) []byte {
	t.Helper()
	components := make([]any, 0, len(shapes))
	for _, shape := range shapes {
		components = append(components, prodRenderComponent(t, shape, construct))
	}
	return prodRenderEncode(t, map[string]any{"components": components})
}

// prodRenderPulumitest constructs shape with pulumitest.Construct, the path the package's own
// Construct tests take.
func prodRenderPulumitest(t *testing.T, engine *prodRenderEngine, shape prodRenderShape) (p.ConstructResponse, error) {
	t.Helper()
	return pulumitest.Construct(t, engine.mocks, shape.typ, shape.name, shape.inputs, false, shape.providers, shape.dependsOn...)
}

// prodRenderConstruct constructs shape as pulumitest.Construct does, with one difference.
// pulumitest.Construct serves the plugin beside pulumi-go-provider v1.7.0's integration/fake
// monitor, which implements no ReadResource, and production's gcp:Dispatch and
// gcp:ClusteredNatsHub each read the PriorityClass they are given ("method ReadResource not
// implemented"). So this serves the plugin as the engine does, through the plugin's gRPC server's
// Construct with that same monitor and fake engine behind it, the monitor answering a read as
// pulumi's own mock monitor answers one. TestProductionRenderHarnessMatchesPulumitest holds the two
// paths to the same bytes for every shape that reads nothing.
func prodRenderConstruct(t *testing.T, engine *prodRenderEngine, shape prodRenderShape) (p.ConstructResponse, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	envoyInfra, err := provider.New()
	require.NoError(t, err, "provider.New")
	engineAddr, _, err := fake.StartEngineServer(ctx, fake.NewEngineServer())
	require.NoError(t, err, "serve the fake engine")
	host, err := comProvider.NewHostClient(engineAddr)
	require.NoError(t, err, "dial the fake engine")
	defer host.Close()
	monitor := &prodRenderMonitor{ResourceMonitorServer: fake.NewResourceMonitorServer(engine.mocks), mocks: engine.mocks}
	monitorAddr, _, err := fake.StartMonitorServer(ctx, monitor)
	require.NoError(t, err, "serve the monitor")
	server, err := p.RawServer("envoy-infra", "0.0.1", envoyInfra)(host)
	require.NoError(t, err, "serve the plugin")
	providers := make(map[string]string, len(shape.providers))
	for pkg, ref := range shape.providers {
		providers[string(pkg)] = string(ref.Urn) + "::" + string(ref.ID)
	}
	dependencies := make([]string, len(shape.dependsOn))
	for i, urn := range shape.dependsOn {
		dependencies[i] = string(urn)
	}
	// The request pulumitest.Construct's ConstructRequest becomes on the wire: project "p" and
	// stack "s", no parent, the inputs as output values, which carry their secrets and
	// dependencies.
	response, err := server.Construct(ctx, &pulumirpc.ConstructRequest{
		Project: "p", Stack: "s", Config: map[string]string{}, ConfigSecretKeys: []string{},
		Parallel: 1, MonitorEndpoint: monitorAddr,
		Type: shape.typ, Name: shape.name, Inputs: propertyrpc.Marshal(property.NewMap(shape.inputs)),
		Providers: providers, Dependencies: dependencies, AcceptsOutputValues: true,
	})
	if err != nil {
		return p.ConstructResponse{}, err
	}
	state, err := propertyrpc.Unmarshal(response.GetState())
	if err != nil {
		return p.ConstructResponse{}, err
	}
	return p.ConstructResponse{Urn: resource.URN(response.GetUrn()), State: state}, nil
}

// prodRenderMonitor is integration/fake's monitor with a ReadResource, which records the read on
// the mocks and answers it as pulumi's own mock monitor (sdk/go/pulumi/mocks.go) does.
type prodRenderMonitor struct {
	*fake.ResourceMonitorServer
	mocks pulumi.MockResourceMonitor
}

func (m *prodRenderMonitor) ReadResource(_ context.Context, in *pulumirpc.ReadResourceRequest) (*pulumirpc.ReadResourceResponse, error) {
	stateIn, err := plugin.UnmarshalProperties(in.GetProperties(), plugin.MarshalOptions{KeepSecrets: true, KeepResources: true})
	if err != nil {
		return nil, err
	}
	_, state, err := m.mocks.NewResource(pulumi.MockResourceArgs{
		TypeToken: in.GetType(), Name: in.GetName(), Inputs: stateIn, Provider: in.GetProvider(), ID: in.GetId(), ReadRPC: in,
	})
	if err != nil {
		return nil, err
	}
	stateOut, err := plugin.MarshalProperties(state, plugin.MarshalOptions{KeepSecrets: true, KeepResources: true})
	if err != nil {
		return nil, err
	}
	return &pulumirpc.ReadResourceResponse{
		Urn: prodRenderURN("stack", "project", in.GetParent(), in.GetType(), in.GetName()), Properties: stateOut,
	}, nil
}

func prodRenderComponent(t *testing.T, shape prodRenderShape, construct prodRenderConstructor) map[string]any {
	t.Helper()
	engine := newProdRenderEngine()
	response, err := construct(t, engine, shape)
	require.NoError(t, err, "construct %s, %s %s", shape.shape, shape.typ, shape.name)
	registrations := engine.mocks.Registrations()
	require.NotEmpty(t, registrations, "construct %s registered nothing", shape.shape)

	// The engine names every URN in one stack and project, which each child's parent carries.
	var stack tokens.QName
	var project tokens.PackageName
	for _, r := range registrations {
		if parent := resource.URN(prodRenderParent(r)); parent != "" {
			stack, project = parent.Stack(), parent.Project()
			break
		}
	}
	require.NotEmpty(t, stack, "no registration of %s names a parent, so no URN can be named", shape.shape)

	type entry struct {
		urn, key string
		doc      map[string]any
	}
	entries := make([]entry, 0, len(registrations))
	for _, r := range registrations {
		urn, doc := prodRenderRegistration(t, stack, project, r)
		entries = append(entries, entry{urn: urn, key: prodRenderKey(t, doc), doc: doc})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].urn != entries[j].urn {
			return entries[i].urn < entries[j].urn
		}
		return entries[i].key < entries[j].key
	})
	rendered := make([]any, len(entries))
	for i, e := range entries {
		rendered[i] = e.doc
	}

	engine.mu.Lock()
	calls := slices.Clone(engine.invokes)
	engine.mu.Unlock()
	invokes := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		invokes = append(invokes, map[string]any{
			"token": call.Token, "provider": call.Provider, "args": prodRenderProperties(t, call.Args),
		})
	}
	sort.Slice(invokes, func(i, j int) bool { return prodRenderKey(t, invokes[i]) < prodRenderKey(t, invokes[j]) })

	return map[string]any{
		"shape":         shape.shape,
		"type":          shape.typ,
		"name":          shape.name,
		"constructUrn":  string(response.Urn),
		"state":         prodRenderProperties(t, resource.ToResourcePropertyMap(response.State)),
		"registrations": rendered,
		"invokes":       invokes,
	}
}

func prodRenderParent(r pulumitest.Registration) string {
	if r.RPC != nil {
		return r.RPC.GetParent()
	}
	if r.ReadRPC != nil {
		return r.ReadRPC.GetParent()
	}
	return ""
}

// prodRenderURN is the URN the engine gives a resource of type typ named name under parent.
func prodRenderURN(stack tokens.QName, project tokens.PackageName, parent, typ, name string) string {
	parentType := tokens.Type("")
	if u := resource.URN(parent); u != "" && u.QualifiedType() != resource.RootStackType {
		parentType = u.QualifiedType()
	}
	return string(resource.NewURN(stack, project, parentType, tokens.Type(typ), name))
}

// prodRenderRegistration renders one registration's whole request, with four fields cleared, each
// shown to vary while the render does not, and none diffed by the engine:
//   - stackTrace, the Go call stack of the registration: its frames hold this test's own call
//     lines, which differ between two renders in one process, and the build machine's paths;
//   - sourcePosition, the plugin source file and line that registered the resource: it names the
//     checkout's path, and moves whenever a line is added above the call;
//   - each transform callback's target and token, the address of the callback server a run
//     starts and the random id it gives the callback, both new on every run. The callback itself
//     stays, so the document still shows how many transforms a registration carries.
//
// The request's dependencies and each property's are sorted, since the engine reads them as sets.
func prodRenderRegistration(t *testing.T, stack tokens.QName, project tokens.PackageName,
	r pulumitest.Registration,
) (string, map[string]any) {
	t.Helper()
	switch {
	case r.RPC != nil:
		request := proto.Clone(r.RPC).(*pulumirpc.RegisterResourceRequest)
		request.SourcePosition, request.StackTrace = nil, nil
		for _, callback := range request.GetTransforms() {
			callback.Target, callback.Token = "", ""
		}
		slices.Sort(request.Dependencies)
		for _, deps := range request.GetPropertyDependencies() {
			slices.Sort(deps.Urns)
		}
		urn := prodRenderURN(stack, project, request.GetParent(), request.GetType(), request.GetName())
		return urn, map[string]any{"urn": urn, "register": prodRenderProto(t, request)}
	case r.ReadRPC != nil:
		request := proto.Clone(r.ReadRPC).(*pulumirpc.ReadResourceRequest)
		request.SourcePosition, request.StackTrace = nil, nil
		slices.Sort(request.Dependencies)
		urn := prodRenderURN(stack, project, request.GetParent(), request.GetType(), request.GetName())
		return urn, map[string]any{"urn": urn, "read": prodRenderProto(t, request)}
	}
	t.Fatalf("the registration of %s %s carries neither a register nor a read request", r.Type, r.Name)
	return "", nil
}

// prodRenderProperties renders a property map as the engine's wire form carries it: secrets,
// unknowns, resource references and output values kept.
func prodRenderProperties(t *testing.T, m resource.PropertyMap) any {
	t.Helper()
	s, err := plugin.MarshalProperties(m, plugin.MarshalOptions{
		KeepUnknowns: true, KeepSecrets: true, KeepResources: true, KeepOutputValues: true,
	})
	require.NoError(t, err)
	return prodRenderProto(t, s)
}

// prodRenderProto renders a message with protojson, then decodes it again, so the document's form
// is encoding/json's: protojson varies its whitespace from one build to another on purpose. Every
// secret value is then replaced by its digest (prodRenderHideSecrets).
func prodRenderProto(t *testing.T, m proto.Message) any {
	t.Helper()
	raw, err := protojson.MarshalOptions{Multiline: true, Indent: "  ", EmitUnpopulated: false}.Marshal(m)
	require.NoError(t, err)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var v any
	require.NoError(t, decoder.Decode(&v))
	return prodRenderSplitLines(prodRenderHideSecrets(t, v))
}

// prodRenderSecretSig is the key, and its value, that marks a secret in a property map as Pulumi's
// RPC carries it (sdk/go/common/resource/sig.Secret): the wrapped value is under "value".
const (
	prodRenderSecretSigKey   = "4dabf18193072939515e22adb298388d"
	prodRenderSecretSigValue = "1b47061264138c4ac30d75fd1eb44270"
)

// prodRenderHideSecrets replaces the value of every secret with "<secret sha256:<hex>>", the
// digest of its compact canonical encoding. The document stays exact, since any change to a
// secret's value changes its digest, and holds no credential-shaped text: the placeholders the
// mock engine computes for a generated password or a key would otherwise sit in it as written.
func prodRenderHideSecrets(t *testing.T, v any) any {
	t.Helper()
	switch v := v.(type) {
	case []any:
		for i := range v {
			v[i] = prodRenderHideSecrets(t, v[i])
		}
		return v
	case map[string]any:
		if v[prodRenderSecretSigKey] == prodRenderSecretSigValue {
			if value, ok := v["value"]; ok {
				sum := sha256.Sum256([]byte(prodRenderKey(t, value)))
				v["value"] = "<secret sha256:" + hex.EncodeToString(sum[:]) + ">"
			}
			return v
		}
		for key, value := range v {
			v[key] = prodRenderHideSecrets(t, value)
		}
		return v
	}
	return v
}

// prodRenderSplitLines replaces each string holding a newline with {"$lines": its lines}.
func prodRenderSplitLines(v any) any {
	switch v := v.(type) {
	case string:
		if !strings.Contains(v, "\n") {
			return v
		}
		lines := strings.Split(v, "\n")
		out := make([]any, len(lines))
		for i, line := range lines {
			out[i] = line
		}
		return map[string]any{"$lines": out}
	case []any:
		for i := range v {
			v[i] = prodRenderSplitLines(v[i])
		}
		return v
	case map[string]any:
		for key, value := range v {
			v[key] = prodRenderSplitLines(value)
		}
		return v
	}
	return v
}

func prodRenderEncode(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	require.NoError(t, encoder.Encode(v))
	return buf.Bytes()
}

// prodRenderKey is v's compact canonical encoding, to order entries by.
func prodRenderKey(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// TestProductionRender renders production's calls twice in one process, requires the two
// documents to be the same bytes, and then writes the document to PRODUCTION_RENDER_OUT or, with
// that unset, requires it to be testdata/production-render.golden byte for byte.
func TestProductionRender(t *testing.T) {
	got := prodRenderDocument(t, prodRenderShapes())
	require.Equal(t, string(got), string(prodRenderDocument(t, prodRenderShapes())), "two renders in one process differ, so the render is not deterministic")
	if out := os.Getenv(prodRenderOutEnv); out != "" {
		require.NoError(t, os.WriteFile(out, got, 0o644))
		t.Logf("wrote the production render, %d bytes, to %s", len(got), out)
		return
	}
	golden := filepath.Join("testdata", prodRenderGolden)
	want, err := os.ReadFile(golden)
	require.NoError(t, err, "read the golden; run %s from packages/envoy/infra to write it", prodRenderRegenerate)
	if !prodRenderCompare(t, golden, want, got) {
		t.FailNow()
	}
}

// prodRenderCompare is TestProductionRender's comparison of the document got against want, the
// golden read from golden: testify's Equal, which on a difference reports the whole diff and the
// command that writes the golden again.
func prodRenderCompare(t assert.TestingT, golden string, want, got []byte) bool {
	return assert.Equal(t, string(want), string(got), "production's render differs from %s; if the change is meant to move it, run %s from packages/envoy/infra and commit the golden it writes", golden, prodRenderRegenerate)
}

// prodRenderReport is a testing.T that records what a failed assertion would report and fails
// nothing, so the control below can run TestProductionRender's own comparison and read its
// verdict.
type prodRenderReport struct{ messages []string }

func (r *prodRenderReport) Errorf(format string, args ...any) {
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
}

// TestProductionRenderSeesAOneInputChange is the instrument's control: gcp:Dispatch's replicas,
// which reaches only the app Deployment's spec, moved from 1 to 2 must change the document, and
// the comparison TestProductionRender runs (prodRenderCompare) must fail, report the replica
// count's lines, and name the command that regenerates the golden.
func TestProductionRenderSeesAOneInputChange(t *testing.T) {
	base := prodRenderDocument(t, prodRenderShapes())
	shapes := prodRenderShapes()
	require.Equal(t, "dispatch", shapes[0].shape)
	require.Equal(t, prodRenderInt(1), shapes[0].inputs["replicas"], "the control changes an input production sets")
	shapes[0].inputs["replicas"] = prodRenderInt(2)
	changed := prodRenderDocument(t, shapes)
	var report prodRenderReport
	require.False(t, prodRenderCompare(&report, filepath.Join("testdata", prodRenderGolden), base, changed), "Dispatch's replicas moved from 1 to 2 and the comparison passed")
	require.Len(t, report.messages, 1)
	diff := report.messages[0][strings.Index(report.messages[0], "Diff:"):]
	require.Contains(t, diff, `"replicas"`, "the differing lines the comparison reports are the replica count's")
	require.Contains(t, report.messages[0], "PRODUCTION_RENDER_OUT=$PWD/gcp/testdata/production-render.golden go test ./gcp -run '^TestProductionRender$'",
		"the report names the command that regenerates the golden")
	t.Logf("the comparison reports the one-input change:\n%s", diff)
}

// TestProductionRenderHarnessMatchesPulumitest holds prodRenderConstruct to pulumitest.Construct:
// every shape, with its priorityClassName taken away so that it reads nothing, renders the same
// bytes through both, so the one thing prodRenderConstruct adds is the read.
func TestProductionRenderHarnessMatchesPulumitest(t *testing.T) {
	shapes := prodRenderShapes()
	for _, shape := range shapes {
		delete(shape.inputs, "priorityClassName")
	}
	ours := prodRenderDocumentBy(t, shapes, prodRenderConstruct)
	theirs := prodRenderDocumentBy(t, shapes, prodRenderPulumitest)
	require.Equal(t, string(theirs), string(ours), "prodRenderConstruct renders what pulumitest.Construct does not")
	require.NotContains(t, string(ours), `"read":`, "with no priorityClassName no shape reads anything")
	require.Contains(t, string(prodRenderDocument(t, prodRenderShapes())), `"read":`,
		"with production's priorityClassName the render holds the PriorityClass reads")
}
