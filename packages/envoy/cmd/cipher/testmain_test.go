package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/cmdtest"
)

// inheritedSettings are the variables of the environment `go test` starts in that changed a
// verdict of this package when set (one variable at a time, the whole package, every other
// variable unset), so TestMain unsets them before any test runs. A test that needs one sets it
// itself (t.Setenv, or the environment of the process it starts), as the tests of each already do.
var inheritedSettings = []string{
	// A value that is no duration makes the command refuse before it makes any call
	// (enrollWait): 74 tests with AGENT_SECRETS_ENROLL_WAIT=soon.
	"AGENT_SECRETS_ENROLL_WAIT",
	// The settings file test requires the variable it does not set to come from the file.
	"AGENT_SECRETS_MACHINE_IDENTITY",
	// A session names its projects; the real helper and broker the register, enroll and grant
	// tests run cannot check them with Dispatch (503 PROJECTS_UNCHECKED).
	"AGENT_SECRETS_PROJECTS",
	// The AWS SDK reads these, and the store forms name each one that is set in a failure.
	"AWS_ENDPOINT_URL",
	"AWS_ENDPOINT_URL_STS",
	"AWS_ENDPOINT_URL_SECRETS_MANAGER",
	"AWS_CONTAINER_CREDENTIALS_FULL_URI",
	"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
	"AWS_CA_BUNDLE",
	// The AWS SDK reads these too: one sets aside the endpoint stand-ins the AWS rows set, and the
	// other asks for a FIPS endpoint the stand-ins are not.
	"AWS_IGNORE_CONFIGURED_ENDPOINT_URLS",
	"AWS_USE_FIPS_ENDPOINT",
	// gcloud reads these as auth properties, so the principal a refusal names is unknown.
	"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE",
	"CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE",
	// The Google client libraries take it as the client's universe domain and compare it with the
	// credentials' own (cloud.google.com/go/auth grpctransport), so a GCP row's call to its
	// stand-in fails on the mismatch.
	"GOOGLE_CLOUD_UNIVERSE_DOMAIN",
}

// TestMain unsets inheritedSettings before the package's tests run, then runs them through
// storetest.Main, which drops the test binary's own database that storetest.URL makes each test's
// schema in, inside cmdtest.Run, which removes the binaries they built. A test binary this
// package's tests start again as a helper unsets them too. It unsets nothing that changed no
// verdict: TMPDIR, the trust bundle variables and the rest of what the code under test reads stay
// as the caller set them.
func TestMain(m *testing.M) {
	for _, name := range inheritedSettings {
		if err := os.Unsetenv(name); err != nil {
			fmt.Fprintf(os.Stderr, "unset %s: %v\n", name, err)
			os.Exit(1)
		}
	}
	os.Exit(cmdtest.Run(func() int { return storetest.Main(m) }))
}
