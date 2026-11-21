package gcp

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
)

// The checks of the Identity-Aware Proxy door's inputs, which newDispatchKubernetes renders into
// the app's environment once each resolves: iapAudience (dispatchIAPAudience), and
// directoryAccount and directoryAdmin (dispatchDirectorySetting).

// iapAudienceRule is the audience IAP signs a request for when it reaches the app through a
// load balancer's backend service: /projects/<project number>/global/backendServices/<backend
// service id>, both numbers (https://cloud.google.com/iap/docs/signed-headers-howto, "Verifying
// the JWT payload", aud).
var iapAudienceRule = regexp.MustCompile(`^/projects/[0-9]+/global/backendServices/[0-9]+$`)

// dispatchIAPAudience is iapAudience as DISPATCH_IAP_AUDIENCE carries it, judged once it resolves:
// a seed is refused first, naming the input and never the value, as refuseDispatchSeed refuses
// one, and then any value of another form, naming the input and quoting the value, which is no
// secret. Dispatch holds each assertion's aud claim to this value as a string, so an audience of
// another form would admit no browser at all.
func dispatchIAPAudience(value pulumi.StringInput, dispatch string) pulumi.StringOutput {
	return value.ToStringOutput().ApplyT(func(v string) (string, error) {
		if bus.HoldsSeed(v) {
			return "", fmt.Errorf("%s: %w", dispatch, core.InputErrorf("iapAudience", "%w", core.SeedRefusal("iapAudience")))
		}
		if !iapAudienceRule.MatchString(v) {
			return "", fmt.Errorf("%s: %w", dispatch, core.InputErrorf("iapAudience",
				"iapAudience %q must be the audience IAP signs for a load balancer's backend service, /projects/<project number>/global/backendServices/<backend service id>", v))
		}
		return v, nil
	}).(pulumi.StringOutput)
}

// dispatchDirectorySetting is directoryAccount or directoryAdmin, input, as its
// DISPATCH_DIRECTORY_* variable carries it, judged once it resolves, as dispatchIAPAudience judges
// iapAudience: a blank or whitespace-only value, which Dispatch reads as unset, so it would not
// boot behind the IAP door, and one holding an nkey seed, are each refused naming input and
// never the value. ApplyT keeps the value's secrecy, so a delegated user a consumer holds as secret
// configuration stays secret in the rendered pod.
func dispatchDirectorySetting(value pulumi.StringInput, dispatch, input string) pulumi.StringOutput {
	return value.ToStringOutput().ApplyT(func(v string) (string, error) {
		if strings.TrimSpace(v) == "" {
			return "", fmt.Errorf("%s: %w", dispatch, core.InputErrorf(input,
				"%s is blank: Dispatch reads it as unset and refuses to boot behind the IAP door without it", input))
		}
		if bus.HoldsSeed(v) {
			return "", fmt.Errorf("%s: %w", dispatch, core.InputErrorf(input, "%w", core.SeedRefusal(input)))
		}
		return v, nil
	}).(pulumi.StringOutput)
}
