package boot_test

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/identity/boot"
)

const module = "github.com/sjawhar/envoy"

// registryPackages may import a provider package: the two registries of the kinds of a build.
var registryPackages = []string{
	module + "/internal/cipher/identity/boot",
	module + "/internal/cipher/identity/providers",
}

// forbiddenToProviders are the broker's consumers of identity, which a provider must never import:
// a provider is a leaf that the consumers reach through the registry, never the other way round.
var forbiddenToProviders = []string{
	module + "/internal/cipher/config",
	module + "/internal/cipher/api",
	module + "/internal/cipher/enroll",
	module + "/internal/cipher/oauth",
	module + "/internal/cipher/oauthstore",
	module + "/internal/cipher/requests",
	module + "/internal/cipher/natscallout",
	module + "/internal/cipher/machine",
	module + "/internal/cipherclient",
}

// TestEachProviderStaysInItsPackage: no code outside a provider's package names its kind (spec
// R6). It lists each non-test package of the module with its direct imports and fails when a
// package other than the provider's own and the two registries imports a provider package, and
// when a provider package imports a consumer of identity. It fails when go list lists no package
// or the registry holds no kind: a check that enumerated nothing has not passed.
func TestEachProviderStaysInItsPackage(t *testing.T) {
	kinds := boot.Kinds()
	if len(kinds) == 0 {
		t.Fatal("boot.Kinds() is empty: the registry holds no provider, so this check enumerated nothing")
	}
	providers := map[string]bool{}
	for _, kind := range kinds {
		providers[module+"/internal/cipher/identity/"+string(kind)] = true
	}
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	list := exec.Command("go", "list", "-f", "{{.ImportPath}} {{join .Imports \" \"}}", "./...")
	list.Dir = root
	out, err := list.Output()
	if err != nil {
		var stderr string
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = string(exit.Stderr)
		}
		t.Fatalf("go list ./... in %s: %v\n%s", root, err, stderr)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("go list ./... in %s listed no package, so this check enumerated nothing", root)
	}
	var seenProvider int
	for _, line := range lines {
		fields := strings.Fields(line)
		pkg, imports := fields[0], fields[1:]
		if providers[pkg] {
			seenProvider++
			for _, imported := range imports {
				if slices.Contains(forbiddenToProviders, imported) {
					t.Errorf("the provider package %s imports %s; a provider reaches no consumer of identity", pkg, imported)
				}
			}
			continue
		}
		if slices.Contains(registryPackages, pkg) {
			continue
		}
		for _, imported := range imports {
			if providers[imported] {
				t.Errorf("%s imports the provider package %s; only the provider's own package and the registries (identity/boot, identity/providers) name a provider", pkg, imported)
			}
		}
	}
	if seenProvider != len(providers) {
		t.Errorf("go list listed %d of the %d provider packages %v", seenProvider, len(providers), kinds)
	}
}

// TestNoTokenTypeLiteralOutsideItsProvider: a provider's token types are its own strings
// (kubernetes.SubjectTokenType, github.SubjectTokenType, google.SubjectTokenType,
// google.PersonTokenType, google.ActorTokenTypeMachine), so no non-test file outside the
// provider packages may spell one out: a second copy of a wire value is the drift the registry
// exists to stop. It fails when it reads no Go file, since a check that enumerated nothing has
// not passed.
func TestNoTokenTypeLiteralOutsideItsProvider(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// The token-type URNs, built from parts so this file holds none of them whole either.
	const prefix = "urn:envoy:params:oauth:token-type:"
	types := []string{
		prefix + "kubernetes-service-account", prefix + "github-workflow-run",
		prefix + "google-identity", prefix + "person-google-identity",
		prefix + "machine-google-identity",
	}
	provider := filepath.Join(root, "internal", "cipher", "identity")
	var read int
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.IsDir() && (entry.Name() == "node_modules" || entry.Name() == ".git"):
			return filepath.SkipDir
		case entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		case strings.HasPrefix(path, provider+string(filepath.Separator)):
			return nil
		}
		read++
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, tokenType := range types {
			if strings.Contains(string(source), tokenType) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s spells out the token type %s; it is the provider's own constant (internal/cipher/identity/<kind>)", rel, tokenType)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if read == 0 {
		t.Fatalf("no Go file under %s was read, so this check enumerated nothing", root)
	}
}
