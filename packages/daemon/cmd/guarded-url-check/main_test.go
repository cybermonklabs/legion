package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardedURLCheckAcceptsTheReviewedConfigurationBoundary(t *testing.T) {
	root := controlModule(t, `
func setting(getenv func(string) string) urlsetting.Value {
	return urlsetting.New(getenv("BROKER_ENVOY_URL"))
}

type valueAlias = urlsetting.Value

type unrelated struct{}
func (unrelated) Raw() string { return "not a URL setting" }
func unrelatedRaw() { _ = unrelated{}.Raw() }

func send(value valueAlias) {
	requestURL := value.Raw() + "/v1/messages/send"
	_ = requestURL
}
`, "")

	findings, err := run(root, []judgment{{
		Package:     "example.com/control",
		Function:    "send",
		Destination: "requestURL",
		Reason:      "builds the listener request URL",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("clean control reported %v", findings)
	}
}

func TestGuardedURLCheckSkipsPackagesWithoutDefaultBuildSource(t *testing.T) {
	root := controlModule(t, `func guarded(value urlsetting.Value) { _ = value }`, "")
	tagged := filepath.Join(root, "tagged")
	if err := os.Mkdir(tagged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tagged, "tagged_test.go"), []byte("package tagged\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	findings, err := run(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("tagged-only package reported %v", findings)
	}
}

func TestGuardedURLCheckRefusesEveryEscapeControl(t *testing.T) {
	for _, control := range []struct {
		name, body, imports, want string
		judged                    []judgment
	}{
		{
			name: "raw call without a judgement",
			body: `func direct(value urlsetting.Value) { raw := value.Raw(); _ = raw }`,
			want: "direct",
		},
		{
			name: "raw method value",
			body: `func methodValue(value urlsetting.Value) { read := value.Raw; raw := read(); _ = raw }`,
			want: "methodValue",
		},
		{
			name: "raw method expression",
			body: `func methodExpression(value urlsetting.Value) { read := urlsetting.Value.Raw; raw := read(value); _ = raw }`,
			want: "methodExpression",
		},
		{
			name: "returned raw method value",
			body: `func returnedMethod(value urlsetting.Value) func() string { return value.Raw }`,
			want: "returnedMethod",
		},
		{
			name: "raw method passed to a callback",
			body: `func callback(value urlsetting.Value) { consume := func(read func() string) { _ = read() }; consume(value.Raw) }`,
			want: "callback",
		},
		{
			name: "parenthesized raw call",
			body: `func parenthesized(value urlsetting.Value) { _ = (value.Raw)() }`,
			want: "parenthesized",
		},
		{
			name: "type alias raw call",
			body: `type guardedAlias = urlsetting.Value; func aliasRead(value guardedAlias) { _ = value.Raw() }`,
			want: "aliasRead",
		},
		{
			name: "copied raw method value",
			body: `func copiedMethod(value urlsetting.Value) { first := value.Raw; second := first; _ = second() }`,
			want: "copiedMethod",
		},
		{
			name: "judgement destination changed",
			body: `func changedDestination(value urlsetting.Value) { changedURL := value.Raw(); _ = changedURL }`,
			judged: []judgment{{
				Package:     "example.com/control",
				Function:    "changedDestination",
				Destination: "requestURL",
				Reason:      "the old request URL destination",
			}},
			want: "changedURL",
		},
		{
			name: "setting read into a plain string",
			body: `func plainRead(getenv func(string) string) urlsetting.Value { raw := getenv("BROKER_ENVOY_URL"); return urlsetting.New(raw) }`,
			want: "plainRead",
		},
		{
			name: "constant setting name",
			body: `func constantRead(getenv func(string) string) urlsetting.Value { const name = "BROKER_" + "ENVOY_URL"; raw := getenv(name); return urlsetting.New(raw) }`,
			want: "constantRead",
		},
		{
			name: "renamed environment reader",
			body: `func renamedRead(lookup func(string) string) urlsetting.Value { raw := lookup("BROKER_ENVOY_URL"); return urlsetting.New(raw) }`,
			want: "renamedRead",
		},
		{
			name:    "operating system environment reader",
			imports: `"os"`,
			body:    `func operatingSystemRead(value urlsetting.Value) string { return os.Getenv("BROKER_ENVOY_URL") }`,
			want:    "operatingSystemRead",
		},
		{
			name:    "operating system environment lookup",
			imports: `"os"`,
			body:    `func operatingSystemLookup(value urlsetting.Value) string { raw, _ := os.LookupEnv("BROKER_ENVOY_URL"); return raw }`,
			want:    "operatingSystemLookup",
		},
		{
			name:    "reflection on a guarded value",
			imports: `"reflect"`,
			body:    `func reflected(value urlsetting.Value) { _ = reflect.ValueOf(value) }`,
			want:    "reflect.ValueOf",
		},
		{
			name:    "unsafe on a guarded value",
			imports: `"unsafe"`,
			body:    `func unsafed(value urlsetting.Value) { _ = unsafe.Pointer(&value) }`,
			want:    "unsafe.Pointer",
		},
	} {
		t.Run(control.name, func(t *testing.T) {
			findings, err := run(controlModule(t, control.body, control.imports), control.judged)
			if err != nil {
				t.Fatal(err)
			}
			for _, finding := range findings {
				if strings.Contains(finding.String(), control.want) {
					return
				}
			}
			t.Fatalf("control %q was not refused by name: %v", control.name, findings)
		})
	}
}

func controlModule(t *testing.T, body, imports string) string {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Clean(filepath.Join(mustGetwd(t), "../../../.."))
	goMod := "module example.com/control\n\ngo 1.27.3\n\nrequire github.com/sjawhar/envoy v0.0.0\n\nreplace github.com/sjawhar/envoy => " + repo + "/packages/envoy\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatal(err)
	}
	source := "package control\n\nimport (\n\t\"github.com/sjawhar/envoy/urlsetting\"\n\t" + imports + "\n)\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(root, "control.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func TestGuardedURLCheckInspectsPackagesWithoutADirectGuardedImport(t *testing.T) {
	for name, source := range map[string]string{
		"plain setting read": `package indirect
func leak(getenv func(string) string) string { return getenv("BROKER_ENVOY_URL") }
`,
		"raw call through an imported field": `package indirect
import control "example.com/control"
func leak() string { return control.Setting.Raw() }
`,
		"reflection through an imported field": `package indirect
import (
	control "example.com/control"
	"reflect"
)
func leak() { _ = reflect.ValueOf(control.Setting) }
`,
		"unsafe through an imported field": `package indirect
import (
	control "example.com/control"
	"unsafe"
)
func leak() { _ = unsafe.Pointer(&control.Setting) }
`,
	} {
		t.Run(name, func(t *testing.T) {
			root := controlModule(t, `var Setting urlsetting.Value`, "")
			directory := filepath.Join(root, "indirect")
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "leak.go")
			if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			findings, err := run(root, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, finding := range findings {
				if finding.Position.Filename == path && strings.Contains(finding.Message, "leak") {
					return
				}
			}
			t.Fatalf("package without a direct urlsetting import was not inspected: %v", findings)
		})
	}
}

func TestGuardedURLCheckRefusesSourceItCannotTypeCheck(t *testing.T) {
	root := controlModule(t, `func invalid(`, "")
	if findings, err := run(root, nil); err == nil {
		t.Fatalf("invalid source returned a result instead of an inspection error: %v", findings)
	}
}

func TestGuardedURLCheckReportsAStaleJudgment(t *testing.T) {
	root := controlModule(t, `func guarded(value urlsetting.Value) { _ = value }`, "")
	findings, err := run(root, []judgment{{
		Package:     "example.com/control",
		Function:    "missing",
		Destination: "requestURL",
		Reason:      "the removed request URL extraction",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || !strings.Contains(findings[0].Message, "no longer exists") || !strings.Contains(findings[0].Message, "missing -> requestURL") {
		t.Fatalf("stale judgment was not reported: %v", findings)
	}
}
