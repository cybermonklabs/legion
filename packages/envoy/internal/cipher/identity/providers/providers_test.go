package providers_test

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/providers"
)

// TestSourcesAreTodaysValues: the identities AGENT_SECRETS_MACHINE_IDENTITY takes, the provider
// whose tokens each makes and whether the broker mints a launcher credential for it are the ones
// a deployment set before the registry, in the order a refusal lists them.
func TestSourcesAreTodaysValues(t *testing.T) {
	sources := providers.Sources()
	if len(sources) == 0 {
		t.Fatal("the registry holds no identity source, so this check enumerated nothing")
	}
	var names []string
	var kinds []identity.Kind
	var launcher []bool
	for _, source := range sources {
		names = append(names, source.Name)
		kinds = append(kinds, source.Provider)
		launcher = append(launcher, source.Launcher)
	}
	if want := []string{"kubernetes", "metadata", "pool", "github-actions"}; !slices.Equal(names, want) {
		t.Errorf("the sources are %v, want %v", names, want)
	}
	if want := []identity.Kind{"kubernetes", "google", "google", "github"}; !slices.Equal(kinds, want) {
		t.Errorf("the sources' providers are %v, want %v", kinds, want)
	}
	if want := []bool{false, true, true, false}; !slices.Equal(launcher, want) {
		t.Errorf("the sources' launcher flags are %v, want %v: the broker mints no launcher credential for a pod's token or a workflow run's", launcher, want)
	}
	for _, name := range names {
		if _, held := providers.Source(name); !held {
			t.Errorf("Source(%q) finds nothing, though the registry lists it", name)
		}
	}
	if _, held := providers.Source("device-login"); held {
		t.Error("Source(\"device-login\") finds a source: the device login is the TypeScript client's own, which no Go tool trades")
	}
}

// TestTheServiceTokenSourcesAreTodaysVariables: the one way a service of the bus connects by a
// provider's own token is the Google provider's, with the variable, flag, usage and settings
// description it had before the registry, so natsprobe's flags and `envoy-dispatch settings`
// print what they always did.
func TestTheServiceTokenSourcesAreTodaysVariables(t *testing.T) {
	sources := providers.ServiceTokenSources()
	if len(sources) != 1 {
		t.Fatalf("the registry holds %d service-token sources, want the Google provider's alone", len(sources))
	}
	source := sources[0]
	if source.Provider != "google" {
		t.Errorf("the service-token source's provider is %q, want google", source.Provider)
	}
	if source.Variable != "NATS_SERVICE_GOOGLE_AUDIENCE" {
		t.Errorf("its variable is %q, want NATS_SERVICE_GOOGLE_AUDIENCE", source.Variable)
	}
	if source.Flag != "service-google-audience" {
		t.Errorf("its flag is %q, want service-google-audience", source.Flag)
	}
	if want := "connect as a service presenting this VM's Google identity token for this audience"; source.Usage != want {
		t.Errorf("its usage is %q, want %q", source.Usage, want)
	}
	if source.Description == "" || source.Reader == nil {
		t.Errorf("its settings description or reader is missing: %q, %v", source.Description, source.Reader != nil)
	}
	if reader := source.Reader("example-audience"); reader == nil {
		t.Error("the source read no token reader for an audience")
	}
}

// TestTheActorAndPersonTypesComeFromTheirProvider: a person's sign-in is presented under the
// Google provider's person type, and the machine's own token beside it under its actor type; a
// subject of another kind takes no actor.
func TestTheActorAndPersonTypesComeFromTheirProvider(t *testing.T) {
	person, held := providers.PersonTokenType()
	if !held || person == "" {
		t.Fatal("the registry names no person token type, so a sign-in could be presented under none")
	}
	machine, ok := providers.ActorTokenType("urn:envoy:params:oauth:token-type:google-identity")
	if !ok || machine == "" {
		t.Fatal("a machine's Google identity token takes no actor type")
	}
	if machine == "urn:envoy:params:oauth:token-type:google-identity" {
		t.Error("the actor type is the subject's own; a machine's token is presented under a type of its own there")
	}
	if _, ok := providers.ActorTokenType("urn:envoy:params:oauth:token-type:kubernetes-service-account"); ok {
		t.Error("a pod's token takes an actor type: only a person's trade carries one")
	}
}

// TestSourceVariablesAreEachNamedOnce: the variables a machine's environment carries are every
// source's, in first-seen order and each once, with the token file marked a path. The two GitHub
// sets for each job (github-actions) are no deployment's setting, so none of them is carried.
func TestSourceVariablesAreEachNamedOnce(t *testing.T) {
	variables := providers.SourceVariables()
	if len(variables) == 0 {
		t.Fatal("the registry names no source variable, so this check enumerated nothing")
	}
	var names []string
	seen := map[string]int{}
	for _, variable := range variables {
		names = append(names, variable.Name)
		seen[variable.Name]++
	}
	want := []string{
		"AGENT_SECRETS_MACHINE_TOKEN_FILE", "AGENT_SECRETS_MACHINE_AUDIENCE",
		"AGENT_SECRETS_POOL_AUDIENCE", "AGENT_SECRETS_POOL_ACCOUNT",
	}
	if !slices.Equal(names, want) {
		t.Errorf("the source variables are %v, want %v", names, want)
	}
	for name, count := range seen {
		if count != 1 {
			t.Errorf("%s is named %d times, want once", name, count)
		}
	}
	for _, variable := range variables {
		if path := variable.Name == "AGENT_SECRETS_MACHINE_TOKEN_FILE"; variable.Path != path {
			t.Errorf("%s is marked path=%t, want %t", variable.Name, variable.Path, path)
		}
	}
}
