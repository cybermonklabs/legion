// The program TestLiveGoConsumersCanUseTheSDK builds against the Go SDK `pulumi package add`
// generates from the plugin, and envoy-smoke's fetch step builds once before the tests, so the
// module cache holds every module its `go mod tidy` reads.
package main

import (
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/sdk/go/envoyinfra/host"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		_, err := host.NewListener(ctx, "example-host-a", &host.ListenerArgs{
			MachineId: "example-host-a",
			NatsUrls:  pulumi.StringArray{pulumi.String("nats://127.0.0.1:4222")},
			Image:     "ghcr.io/cybermonklabs/legion/envoy:example",
			Seed:      pulumi.String("SUEXAMPLESEED"),
			Adopt:     host.AdoptArgs{Container: pulumi.String("listener-example-host-a")},
		})
		return err
	})
}
