package textcheck

//ci:reads packages/envoy/**/*.go !packages/envoy/infra/** !packages/envoy/**/testdata/** containerImporters and dockerDriverFiles walk ../.. but infra, node_modules and testdata, and read every .go file they find, test files included (TestNoPackageOutsideTheRigsImportsOrDrivesAContainerRuntime)

import (
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The check below reads every Go file of the module, so a test package that holds it is selected by
// a change to any of them. It lived in internal/testnats, the NATS rig nearly every test of the
// module imports, and so made that rig's own tests run on every change; here it runs with the
// module's other checks of its source, in the job with no database.

// envoyModule is the module's path, and envoyRoot its directory, from this package's own.
const (
	envoyModule = "github.com/sjawhar/envoy"
	envoyRoot   = "../.."
)

// containerRigs are the packages of this module that may reach testcontainers: the NATS rig, and the
// two whose containers are not NATS (internal/testpg runs pgvector, internal/tests3 SeaweedFS). Every
// other package is a finding, whichever testcontainers package it imports: modules/nats is the
// obvious way to start a NATS container, and testcontainers.Run(ctx, testnats.Image) is the one
// that needs no such import.
var containerRigs = []string{
	envoyModule + "/internal/testnats",
	envoyModule + "/internal/testpg",
	envoyModule + "/internal/tests3",
}

// containerModule is the import prefix every testcontainers package shares.
const containerModule = "github.com/testcontainers/testcontainers-go"

// dockerDriver matches a file of this module driving a container runtime by hand: exec's run with
// docker as the command, however the call spells its context argument, and testcontainers' own
// Docker client. The context argument runs to the end of the line rather than to the first ')',
// since the module already passes context.Background() inline and a test's own is t.Context().
// It is written from pieces so that this file, which has to name them, is not itself a finding.
var dockerDriver = regexp.MustCompile(
	`exec\.Command(Context)?\([^\n]*` + `"docker"` + `|NewDockerClient` + `WithOpts`)

// allowedDockerDrivers are the files of this module, by their path from packages/envoy, that may
// drive docker by hand, each with why: a test whose claim only a container's own limits can
// prove. The walk leaves each out (dockerDriverFiles); every other file that drives docker is a
// finding.
var allowedDockerDrivers = map[string]string{
	"cmd/dispatch/agent_memory_group_test.go": "check c20: the kernel's out-of-memory killer acts on " +
		"the agent before the server in an 8 GiB memory group with swap off, which only a container " +
		"gives; it skips unless DISPATCH_TEST_MEMORY_GROUP=1, which the dispatch-memory-group job alone " +
		"sets, on a runner that has Docker",
}

// No package of this module outside the rigs above imports a container runtime directly or drives
// docker by hand: Envoy's agents have no container runtime, and a test that needs one cannot run
// where they do. This pins those two ways to get one — a direct import of testcontainers by a file
// an untagged build of the package or of its tests compiles, and a hand-driven docker in any file —
// against the packages allowed to have them, and a hand-driven docker also against its two
// exceptions, the nested infra module and the files allowedDockerDrivers names with their reasons.
// It does not prove that no other package needs a runtime: a package that imports an allowlisted
// rig passes and still starts that rig's container, as cmd/listener's tests do through testpg.URL
// whenever DISPATCH_TEST_DATABASE_URL is unset. internal/smoke's containers are behind the smoke
// build tag and the NATS rig's own behind natslive, so the untagged build reads neither import. The
// docker walk does not go by tag: it reads every .go file, those included, and passes them because
// none drives docker by hand.
func TestNoPackageOutsideTheRigsImportsOrDrivesAContainerRuntime(t *testing.T) {
	t.Run("imports", func(t *testing.T) {
		findings, read := containerImporters(t)
		if read == 0 {
			t.Fatalf("read no Go file an untagged build compiles under %s", envoyRoot)
		}
		for _, finding := range findings {
			t.Errorf("package %s imports %s, which belongs to a container runtime; a test that needs a NATS server starts it with testnats.Start, StartConfig or URL", finding.pkg, finding.imported)
		}
	})
	t.Run("docker", func(t *testing.T) {
		files, read := dockerDriverFiles(t)
		if read == 0 {
			t.Fatalf("read no Go file under %s", envoyRoot)
		}
		for _, file := range files {
			t.Errorf("%s drives a container runtime itself; a test that has to stop a server's traffic uses testnats.Server.Freeze, and one that has to stop the server, Server.Stop", file)
		}
	})
}

// The file walk's matcher recognises a hand-driven docker in every way a test of this module spells
// the call, the context argument included: the module already passes context.Background() inline,
// and t.Context() is the test's own. Each line is assembled from pieces so this file, which the walk
// also reads, is not a finding itself.
func TestTheDockerDriverMatchCatchesEveryContextArgument(t *testing.T) {
	const run, docker = "exec." + "Command", `"dock` + `er"`
	for _, line := range []string{
		run + "(" + docker + `, "pause", id)`,
		run + "Context(ctx, " + docker + `, "pause", id)`,
		run + "Context(context.Background(), " + docker + `, "pause", id)`,
		run + "Context(t.Context(), " + docker + `, "unpause", id)`,
		"client.NewDocker" + "ClientWithOpts(client.FromEnv)",
	} {
		if !dockerDriver.MatchString(line) {
			t.Errorf("the docker-driver match misses %s", line)
		}
	}
	if line := run + `("go", "list", "./...")`; dockerDriver.MatchString(line) {
		t.Errorf("the docker-driver match flags %s, which runs no container", line)
	}
}

// The import walk finds an import of testcontainers in a test file, in a file of the package, and
// not in a file an untagged build leaves out, and names the package, not the file: the three
// readings a finding rests on, against a module written for this test.
func TestTheImportWalkReadsWhatAnUntaggedBuildCompiles(t *testing.T) {
	root := t.TempDir()
	write := func(name, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runtime := strconv.Quote(containerModule + "/modules/nats")
	write("go.mod", "module "+envoyModule+"\n")
	write("plain/plain.go", "package plain\n\nimport _ "+runtime+"\n")
	write("intest/a.go", "package intest\n")
	write("intest/a_test.go", "package intest_test\n\nimport _ "+runtime+"\n")
	write("tagged/a.go", "//go:build smoke\n\npackage tagged\n\nimport _ "+runtime+"\n")
	write("clean/a.go", "package clean\n\nimport _ \"strings\"\n")
	write("nested/go.mod", "module other\n")
	write("nested/a.go", "package nested\n\nimport _ "+runtime+"\n")
	findings, read := importersUnder(t, root)
	var got []string
	for _, finding := range findings {
		got = append(got, finding.pkg)
	}
	want := []string{envoyModule + "/intest", envoyModule + "/plain"}
	if !slices.Equal(got, want) {
		t.Errorf("the walk names %v, want %v: a test file's import counts, a tagged file's and a nested module's do not", got, want)
	}
	if read != 4 {
		t.Errorf("the walk read %d files an untagged build compiles, want 4 (plain, intest's two, clean)", read)
	}
}

// containerImport is a package of this module importing a testcontainers package, and the import
// it was found by: a package reaching only testcontainers-go/wait has imported neither a runtime
// client nor modules/nats, and the finding has to say which it did import.
type containerImport struct{ pkg, imported string }

// containerImporters lists every package of this module, outside the rigs allowed one, whose
// untagged build or test build imports a testcontainers package, with the first such import of
// each, and how many files it read.
func containerImporters(t *testing.T) ([]containerImport, int) {
	t.Helper()
	findings, read := importersUnder(t, envoyRoot)
	var kept []containerImport
	for _, finding := range findings {
		if !slices.Contains(containerRigs, finding.pkg) {
			kept = append(kept, finding)
		}
	}
	return kept, read
}

// importersUnder reads, per package directory of the module rooted at root (another module's
// directory, testdata, node_modules, vendor and a directory go ignores left out, as `go list ./...`
// leaves them), the imports of each Go file an untagged build of the package or of its tests
// compiles, as the default build context matches it, and lists each package importing a
// testcontainers package with the first such import in file order.
func importersUnder(t *testing.T, root string) ([]containerImport, int) {
	t.Helper()
	files := token.NewFileSet()
	context := build.Default
	first := map[string]string{}
	read := 0
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if p != root && (name == "node_modules" || name == "vendor" || name == "testdata" ||
				strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "go.mod")); p != root && err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		if match, err := context.MatchFile(filepath.Dir(p), entry.Name()); err != nil || !match {
			return err
		}
		file, err := parser.ParseFile(files, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		read++
		dir, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return err
		}
		pkg := path.Join(envoyModule, filepath.ToSlash(dir))
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if _, found := first[pkg]; !found && (imported == containerModule || strings.HasPrefix(imported, containerModule+"/")) {
				first[pkg] = imported
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the module's Go files under %s: %v", root, err)
	}
	var findings []containerImport
	for pkg, imported := range first {
		findings = append(findings, containerImport{pkg: pkg, imported: imported})
	}
	slices.SortFunc(findings, func(a, b containerImport) int { return strings.Compare(a.pkg, b.pkg) })
	return findings, read
}

// dockerDriverFiles lists every Go file under packages/envoy that drives a container runtime
// itself, by its path from there, and how many it read. Two exceptions are left out: the nested
// infra module, whose pulumilive lane runs a real stack of containers on a host that has a
// runtime, and the files allowedDockerDrivers names, each with the reason it needs a container of
// its own.
func dockerDriverFiles(t *testing.T) ([]string, int) {
	t.Helper()
	var drivers []string
	read := 0
	err := filepath.WalkDir(envoyRoot, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if p == filepath.Join(envoyRoot, "infra") || entry.Name() == "node_modules" || entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		text, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		read++
		if dockerDriver.Match(text) {
			relative, err := filepath.Rel(envoyRoot, p)
			if err != nil {
				return err
			}
			if relative = filepath.ToSlash(relative); allowedDockerDrivers[relative] == "" {
				drivers = append(drivers, relative)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", envoyRoot, err)
	}
	return drivers, read
}
