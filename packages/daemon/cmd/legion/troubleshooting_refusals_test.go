package main

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// troubleshootingPage is the repository's own troubleshooting page, read from this package's
// directory as the shipped-example test reads the shipped legion.yaml.
const troubleshootingPage = "../../../../docs/site/src/content/docs/legion/troubleshooting.md"

// daemonWillNotStart is the heading of the refusal table every row below is taken from.
const daemonWillNotStart = "## The daemon will not start"

// refusalFixtures is every row of that table a fixture drives: the row's refusal text, as the
// `It says` cell writes it, and a fixture that makes `legion start --check-config` — the loader
// (config.LoadForValidation) and the boot gate (daemon.CheckStart) behind it — print a message
// the row matches. A row whose wording the code no longer produces fails here, printing the row
// and the message, which is the whole point of the table: an operator reads the row and greps the
// daemon's output for it.
var refusalFixtures = map[string]func(t *testing.T) string{
	"runtime.kubernetes.image must be pinned by digest (@sha256:…)": func(t *testing.T) string {
		return refusalFile{kubernetes: true, drop: []string{"image:"},
			pod: "    image: ghcr.io/cybermonklabs/legion-worker:latest\n"}.refuse(t)
	},
	"bind 127.0.0.1 is not an address a pod can reach, …": func(t *testing.T) string {
		return refusalFile{kubernetes: true, drop: []string{"bind:"}, top: "bind: 127.0.0.1\n"}.refuse(t)
	},
	"bind 127.0.0.1 is loopback, where no pod reaches the worker stream, whatever advertise_host names; …": func(t *testing.T) string {
		return refusalFile{kubernetes: true, drop: []string{"bind:"},
			top: "bind: 127.0.0.1\nadvertise_host: legion.internal.example\n"}.refuse(t)
	},
	"advertise_host must be an IP address or a DNS name, …": func(t *testing.T) string {
		return refusalFile{kubernetes: true, top: "advertise_host: http://legion.internal.example\n"}.refuse(t)
	},
	// The Kelvin sign, which no ASCII name holds and which the refusal prints as its \u escape.
	`<key> must be an IP address or an ASCII DNS name; got "…\u212a…"`: func(t *testing.T) string {
		return refusalFile{kubernetes: true, drop: []string{"bind:"}, top: "bind: legion\u212a.internal.example\n"}.refuse(t)
	},
	"<key> is required when runtime is kubernetes: …": func(t *testing.T) string {
		return refusalFile{kubernetes: true, drop: []string{"nats_urls:"}}.refuse(t)
	},
	"worker_stop_timeout_seconds must be at least 9 when runtime is kubernetes: …": func(t *testing.T) string {
		return refusalFile{kubernetes: true, top: "worker_stop_timeout_seconds: 5\n"}.refuse(t)
	},
	"projects must configure <PROJECT>, the daemon's own project": func(t *testing.T) string {
		// The daemon's own project is refused only beside the dispatch_url a workflow boot reads.
		return refusalFile{kubernetes: true, drop: []string{"project:"}, top: "project: ABSENT\n"}.refuse(t)
	},
	"postgres_dsn is required (or set LEGION_POSTGRES_DSN)": func(t *testing.T) string {
		return refusalFile{drop: []string{"postgres_dsn:"}}.refuse(t)
	},
	"unknown key <key>": func(t *testing.T) string {
		return refusalFile{top: "worker_backend: {}\n"}.refuse(t)
	},
	"omp_invocation is not used when runtime is kubernetes: …": func(t *testing.T) string {
		return refusalFile{kubernetes: true, top: "omp_invocation: omp\n"}.refuse(t)
	},
	"worker_backends.architect is claude (Claude Code), which runs only planner, implementer, tester, reviewer, merger: …": func(t *testing.T) string {
		return refusalFile{top: "worker_backends: {architect: claude}\n"}.refuse(t)
	},
	"worker_backends runs a role on claude, and boot is missing …": func(t *testing.T) string {
		// The boot gate's refusal, so the daemon's environment must name none of the three.
		for _, variable := range []string{"LEGION_CLAUDE_PATH", "LEGION_BUN_PATH", "LEGION_CLAUDE_PLUGIN_DIR"} {
			t.Setenv(variable, "")
			if err := os.Unsetenv(variable); err != nil {
				t.Fatal(err)
			}
		}
		return refusalFile{top: "worker_backends: {implementer: claude}\n"}.refuse(t)
	},
	"role_config_files.<role> entry <file> is under no runtime.kubernetes.pod.volume_mounts mount_path: …": func(t *testing.T) string {
		return refusalFile{kubernetes: true, top: "role_config_files: {tester: [/etc/legion-fallback/fallback.yml]}\n"}.refuse(t)
	},
	"role_config_files.<role> names <file>, which the <role>'s Oh My Pi could not read: …": func(t *testing.T) string {
		missing := filepath.Join(t.TempDir(), "missing.yml")
		return refusalFile{top: "role_config_files: {implementer: [" + missing + "]}\n"}.refuse(t)
	},
	"role_config_files.<role> …": func(t *testing.T) string {
		return refusalFile{top: "role_config_files: {implementer: []}\n"}.refuse(t)
	},
	"worker_stall_backstop_hours.<role> (<bound>) is below the whole stall ladder the <role>'s own plugin runs (<longest call> + <grace> + 2 × <step> = <ladder>, where the first number is …): …": func(t *testing.T) string {
		return refusalFile{top: "worker_stall_backstop_hours: {implementer: 4}\n"}.refuse(t)
	},
	`projects.<KEY>.memory.mode must be off, on, or random (got "…")`: func(t *testing.T) string {
		return refusalFile{project: "    memory: {mode: sometimes, config_files: [/etc/legion-operator/memory.yml]}\n"}.refuse(t)
	},
	"projects.<KEY>.memory is <mode> and names no config_files: …": func(t *testing.T) string {
		return refusalFile{project: "    memory: {mode: on}\n"}.refuse(t)
	},
	"projects.<KEY>.memory names config_files, and worker_backends.<role> is claude, which reads no PI_CONFIG_FILES: …": func(t *testing.T) string {
		return refusalFile{top: "worker_backends: {implementer: claude}\n",
			project: "    memory: {mode: on, config_files: [/etc/legion-operator/memory.yml]}\n"}.refuse(t)
	},
	"projects.<KEY>.memory.config_files entry <file> is under no runtime.kubernetes.pod.volume_mounts mount_path: …": func(t *testing.T) string {
		return refusalFile{kubernetes: true,
			project: "    memory: {mode: on, config_files: [/etc/legion-operator/memory.yml]}\n"}.refuse(t)
	},
	"projects.<KEY>.memory.config_files names <file>, which a role drawn into that arm could not read: …": func(t *testing.T) string {
		missing := filepath.Join(t.TempDir(), "missing.yml")
		return refusalFile{project: "    memory: {mode: on, config_files: [" + missing + "]}\n"}.refuse(t)
	},
	"projects.<KEY>.memory.config_files …": func(t *testing.T) string {
		return refusalFile{project: "    memory: {mode: on, config_files: [/etc/legion-operator/memory.yml, /etc/legion-operator/memory.yml]}\n"}.refuse(t)
	},
	"instructions file <path> of experiment memory variant memory could not be read: …": func(t *testing.T) string {
		dir := t.TempDir()
		overlay, missing := filepath.Join(dir, "memory.yml"), filepath.Join(dir, "missing.md")
		if err := os.WriteFile(overlay, []byte("retry:\n  modelFallback: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return refusalFile{project: "    memory: {mode: on, config_files: [" + overlay + "], instructions: " + missing + "}\n"}.refuse(t)
	},
	// The declared experiments. `experiments` is read on the project as the `memory` block is, so
	// each of these is one project key and the kubernetes base only where a pod mount is in view.
	"projects.<KEY>.memory and projects.<KEY>.experiments.memory are one experiment written twice: the memory block is the experiment named memory, so write the block or the key, not both": func(t *testing.T) string {
		return refusalFile{project: "    memory: {}\n    experiments: {memory: {variants: [{name: memory}, {name: none}]}}\n"}.refuse(t)
	},
	"projects.<KEY>.experiments.memory names the variants …: its variants must be exactly memory and none, since a draw of it is served to every pod as the issue state's memoryGroup, whose wire enum is those two words, and a third makes the whole body fail to parse": func(t *testing.T) string {
		return refusalFile{project: "    experiments: {memory: {variants: [{name: long}, {name: short}]}}\n"}.refuse(t)
	},
	"projects.<KEY>.experiments must be a mapping of an experiment's name to its block": func(t *testing.T) string {
		return refusalFile{project: "    experiments: [plan-length]\n"}.refuse(t)
	},
	`projects.<KEY>.experiments key "…" must match %s: an experiment's name is recorded with every draw it makes`: func(t *testing.T) string {
		return refusalFile{project: "    experiments: {Plan Length: {variants: [{name: long}, {name: control}]}}\n"}.refuse(t)
	},
	`projects.<KEY>.experiments.<NAME>.mode must be off, on, or random (got "…")`: func(t *testing.T) string {
		return refusalFile{project: "    experiments: {plan-length: {mode: sometimes, variants: [{name: long}, {name: control}]}}\n"}.refuse(t)
	},
	`projects.<KEY>.experiments.<NAME>.unit must be tree or issue (got "…")`: func(t *testing.T) string {
		return refusalFile{project: "    experiments: {plan-length: {unit: pod, variants: [{name: long}, {name: control}]}}\n"}.refuse(t)
	},
	"projects.<KEY>.experiments.<NAME>.variants is required: an experiment draws among the variants its block lists": func(t *testing.T) string {
		return refusalFile{project: "    experiments: {plan-length: {mode: random}}\n"}.refuse(t)
	},
	"projects.<KEY>.experiments.<NAME> is on and names no on_variant: …": func(t *testing.T) string {
		return refusalFile{project: "    experiments: {plan-length: {mode: on, variants: [{name: long}, {name: control}]}}\n"}.refuse(t)
	},
	"projects.<KEY>.experiments.<NAME>.variants.<ARM>.weight must be a whole number above zero (got <n>): …": func(t *testing.T) string {
		return refusalFile{project: "    experiments: {plan-length: {variants: [{name: long, weight: 0}, {name: control}]}}\n"}.refuse(t)
	},
	"projects.<KEY>.experiments.<NAME>.variants.<ARM> names config_files, and worker_backends.<role> is claude, which reads no PI_CONFIG_FILES: …": func(t *testing.T) string {
		return refusalFile{top: "worker_backends: {implementer: claude}\n",
			project: "    experiments: {plan-length: {mode: random, variants: [{name: long, config_files: [/etc/legion-operator/plan.yml]}, {name: control}]}}\n"}.refuse(t)
	},
	"projects.<KEY>.experiments.<NAME>.variants.<ARM>.config_files entry <file> is under no runtime.kubernetes.pod.volume_mounts mount_path: …": func(t *testing.T) string {
		return refusalFile{kubernetes: true,
			project: "    experiments: {plan-length: {mode: random, variants: [{name: long, config_files: [/etc/legion-operator/plan.yml]}, {name: control}]}}\n"}.refuse(t)
	},
	"projects.<KEY>.experiments.<NAME>.variants.<ARM>.config_files …": func(t *testing.T) string {
		return refusalFile{project: "    experiments: {plan-length: {variants: [{name: long, config_files: [/etc/legion-operator/plan.yml, /etc/legion-operator/plan.yml]}, {name: control}]}}\n"}.refuse(t)
	},
	"instructions file <path> of experiment <experiment> variant <variant> could not be read: …": func(t *testing.T) string {
		dir := t.TempDir()
		overlay, missing := filepath.Join(dir, "plan.yml"), filepath.Join(dir, "missing.md")
		if err := os.WriteFile(overlay, []byte("retry:\n  modelFallback: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return refusalFile{project: "    experiments: {plan-length: {mode: random, variants: [{name: long, config_files: [" +
			overlay + "], instructions: " + missing + "}, {name: control}]}}\n"}.refuse(t)
	},
	`projects.<KEY>.commit_issue_trailer_repos[<n>] must be "owner/name" (got "…")`: func(t *testing.T) string {
		return refusalFile{project: "    commit_issue_trailer_repos: [acme]\n"}.refuse(t)
	},
	"projects.<KEY>.commit_issue_trailer_repos[<n>] (<owner>/<name>) and projects.<KEY>.commit_issue_trailer_repos[<m>] (<owner>/<name>) name one repository: repository_renames makes … one repository, so the list names it twice; name it once": func(t *testing.T) string {
		return refusalFile{top: "repository_renames: [{from: acme/widgets, to: acme/gadgets}]\n",
			project: "    commit_issue_trailer_repos: [acme/widgets, acme/gadgets]\n"}.refuse(t)
	},
}

// refusalsNoFixtureDrives is every row of that table no fixture can drive, and why. Neither is a
// refusal the configuration's own read makes, which is all `legion start --check-config` runs.
var refusalsNoFixtureDrives = map[string]string{
	"<PROJECT> is already running (pid <n>)": "`legion start` itself prints it, from this machine's registry of running daemons, " +
		"after the configuration is read: a fixture would have to register a daemon, and --check-config, which claims no team, never reads the registry",
	`capability <name> is open: <detail>; to record a decision, add to legion.yaml: capabilities.decided.<name>: "<reason>"`: "the row marks it a report rather than a refusal: " +
		"the check prints it after the Config OK line and exits 0, so no fixture makes it the message a refusal carries",
}

// Every row of the troubleshooting page's refusal table (the table under `## The daemon will not
// start`) is held to a message the daemon can produce. A row is matched this one way: the row's
// refusal text is a pattern in which `…`, every `<…>` placeholder and a `%s` each stand for one
// or more characters of any kind and every other character stands for itself, and the pattern
// must match the fixture's message whole, from its first character to its last — so a row that
// quotes wording the code no longer produces fails, and a row cannot pass by being a prefix of
// something else. A row whose cell quotes two texts joined by "or" is satisfied by either; the
// bare key names such a cell also quotes (`daemon_url`, `advertise_host`) hold no space and are no
// texts. A row in neither table fails naming the row, as does a table row naming no row of the
// page.
func TestTheTroubleshootingTableHoldsEveryRefusalToAMessageTheLoaderProduces(t *testing.T) {
	rows := troubleshootingRows(t)
	named := make(map[string]bool, len(rows))
	compared := 0
	for _, row := range rows {
		texts := refusalTexts(row)
		if len(texts) == 0 {
			t.Errorf("the row %q quotes no refusal text: every row names what the daemon says", row)
			continue
		}
		name := texts[0]
		named[name] = true
		if reason, undrivable := refusalsNoFixtureDrives[name]; undrivable {
			if _, both := refusalFixtures[name]; both {
				t.Errorf("the row %q is in both tables: it is driven by a fixture, or it is not", name)
			}
			if reason == "" {
				t.Errorf("the row %q names no reason no fixture drives it", name)
			}
			continue
		}
		fixture, driven := refusalFixtures[name]
		if !driven {
			t.Errorf("the row %q is in neither table: add it to refusalFixtures with a fixture that makes the daemon print it, or to refusalsNoFixtureDrives with the reason no fixture can", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			says := fixture(t)
			for _, text := range texts {
				if refusalPattern(t, text).MatchString(says) {
					return
				}
			}
			t.Errorf("the row says\n\t%s\nand its fixture's refusal is\n\t%s", strings.Join(texts, "\nor\t"), says)
		})
		compared++
	}
	for _, table := range []struct {
		name string
		rows []string
	}{
		{"refusalFixtures", slices.Sorted(maps.Keys(refusalFixtures))},
		{"refusalsNoFixtureDrives", slices.Sorted(maps.Keys(refusalsNoFixtureDrives))},
	} {
		for _, name := range table.rows {
			if !named[name] {
				t.Errorf("%s names %q, which is no row of the table: a row that moved is renamed here too, a row that went is deleted here", table.name, name)
			}
		}
	}
	t.Logf("rows compared: %d of the table's %d", compared, len(rows))
	if compared == 0 {
		t.Fatal("no row was compared: a table that holds nothing proves nothing")
	}
}

// refusalWildcard is what a row's text leaves open: `…`, a `<…>` placeholder, and a `%s` tail.
var refusalWildcard = regexp.MustCompile(`…|<[^<>]*>|%s`)

// codeSpan is one `…` span of a row's cell.
var codeSpan = regexp.MustCompile("`([^`]+)`")

// refusalPattern is one row's text as the expression a refusal must match whole.
func refusalPattern(t *testing.T, text string) *regexp.Regexp {
	t.Helper()
	var expression strings.Builder
	expression.WriteString(`\A`)
	last := 0
	for _, at := range refusalWildcard.FindAllStringIndex(text, -1) {
		expression.WriteString(regexp.QuoteMeta(text[last:at[0]]))
		expression.WriteString(`(?s:.+)`)
		last = at[1]
	}
	expression.WriteString(regexp.QuoteMeta(text[last:]))
	expression.WriteString(`\z`)
	pattern, err := regexp.Compile(expression.String())
	if err != nil {
		t.Fatalf("the row %q is no pattern: %v", text, err)
	}
	return pattern
}

// refusalTexts is the code spans of a row's `It says` cell that are refusal texts: a refusal holds
// a space, where the bare keys a cell also quotes do not. The first is the row's name in the two
// tables above, the rest the alternatives the cell joins with "or".
func refusalTexts(cell string) []string {
	var texts []string
	for _, span := range codeSpan.FindAllStringSubmatch(cell, -1) {
		if strings.Contains(span[1], " ") {
			texts = append(texts, span[1])
		}
	}
	return texts
}

// troubleshootingRows is the `It says` cell of every row of the table under `## The daemon will
// not start`, in the page's order.
func troubleshootingRows(t *testing.T) []string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate command test source")
	}
	page, err := os.ReadFile(filepath.Join(filepath.Dir(source), troubleshootingPage))
	if err != nil {
		t.Fatal(err)
	}
	at := strings.Index(string(page), daemonWillNotStart+"\n")
	if at < 0 {
		t.Fatalf("%s has no %q heading", troubleshootingPage, daemonWillNotStart)
	}
	var rows []string
	for _, line := range strings.Split(string(page)[at+len(daemonWillNotStart):], "\n") {
		if strings.HasPrefix(line, "## ") {
			break
		}
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) != 4 || strings.TrimSpace(cells[0]) != "" || strings.TrimSpace(cells[3]) != "" {
			t.Fatalf("the row %q is no two-cell row; a cell holding a `|` needs another reader here", line)
		}
		says := strings.TrimSpace(cells[1])
		if says == "It says" || strings.Trim(says, "- ") == "" {
			continue // the header row and the one under it
		}
		rows = append(rows, says)
	}
	if len(rows) == 0 {
		t.Fatalf("%s holds no row under %q", troubleshootingPage, daemonWillNotStart)
	}
	return rows
}

// refusalFile is the legion.yaml one fixture writes: a file `legion start --check-config` passes,
// minus every base line drop names and plus the fixture's own keys, so a fixture's refusal is the
// one thing it asks for. Its Postgres is unreachable and its App keys are never run, as every
// check-config fixture's are.
type refusalFile struct {
	kubernetes bool     // the kubernetes base rather than the tmux one
	drop       []string // base lines whose text starts with one of these are left out
	top        string   // extra top-level lines
	pod        string   // extra lines of the runtime.kubernetes block, four spaces in
	project    string   // extra lines of projects.DEMO, four spaces in
}

const tmuxRefusalBase = `project: DEMO
port: 13370
postgres_dsn: postgres://legion:legion@127.0.0.1:1/legion
operator_token_file: ./operator-token
github_apps:
  implement: { app_id: "1", private_key: implement-test-key }
  review: { app_id: "2", private_key: review-test-key }
`

const kubernetesRefusalBase = `project: DEMO
postgres_dsn: postgres://legion:legion@127.0.0.1:1/legion
bind: 10.0.0.5
daemon_url: http://10.0.0.5:13370
envoy_url: http://envoy-listener.internal.example:9020
envoy_token_file: ./envoy-token
operator_token_file: ./operator-token
dispatch_url: https://dispatch.internal.example
dispatch_token_file: ./dispatch-token
nats_urls: [nats://nats.internal.example:4222]
github_apps:
  implement: { app_id: "1", private_key_command: "exit 1" }
  review: { app_id: "2", private_key_command: "exit 1" }
`

// kubernetesRefusalRuntime is the runtime block, written last so that a fixture's own pod lines
// extend it.
const kubernetesRefusalRuntime = `runtime:
  kubernetes:
    namespace: legion
    image: ghcr.io/cybermonklabs/legion-worker@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    storage_class: gp2
    session_store: postgres
    session_dsn_secret: SESSION_DSN
    kubeconfig: ./kubeconfig
    context: legion-daemon
`

// refusalKubeconfig names the context the kubernetes base names and nothing the check dials.
const refusalKubeconfig = `apiVersion: v1
kind: Config
clusters: [{name: example, cluster: {server: "https://192.0.2.20:6443"}}]
users: [{name: legion-daemon, user: {token: placeholder}}]
contexts: [{name: legion-daemon, context: {cluster: example, user: legion-daemon, namespace: legion}}]
current-context: legion-daemon
`

// refuse runs the real `legion start --check-config` over the file and answers the one refusal it
// printed, without the `legion start: ` the command prefixes it with.
func (f refusalFile) refuse(t *testing.T) string {
	t.Helper()
	config := f.write(t)
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)
	const prefix = "legion start: "
	says := errb.String()
	if code != 1 || out.Len() != 0 || !strings.HasPrefix(says, prefix) || !strings.HasSuffix(says, "\n") {
		t.Fatalf("legion start --check-config = %d, stdout %q, stderr %q; want 1, nothing, and one refusal", code, out.String(), says)
	}
	return strings.TrimSuffix(strings.TrimPrefix(says, prefix), "\n")
}

// write writes the file and the files it names, and answers its path.
func (f refusalFile) write(t *testing.T) string {
	t.Helper()
	legionState(t)
	omp, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_OMP_PATH", omp)
	t.Setenv("LEGION_GH_PATH", omp)
	dir := t.TempDir()
	beside := map[string]string{"operator-token": "operator\n"}
	base := tmuxRefusalBase
	if f.kubernetes {
		base = kubernetesRefusalBase
		beside["envoy-token"] = "envoy\n"
		beside["dispatch-token"] = "dispatch\n"
		beside["kubeconfig"] = refusalKubeconfig
	}
	var body strings.Builder
	fmt.Fprintf(&body, "state_dir: %s\n", filepath.Join(dir, "state"))
	kept := func(block string) {
		for _, line := range strings.Split(strings.TrimSuffix(block, "\n"), "\n") {
			if slices.ContainsFunc(f.drop, func(prefix string) bool { return strings.HasPrefix(strings.TrimSpace(line), prefix) }) {
				continue
			}
			body.WriteString(line + "\n")
		}
	}
	kept(base)
	body.WriteString(f.top)
	body.WriteString("projects:\n  DEMO:")
	if f.project == "" {
		body.WriteString(" {}\n")
	} else {
		body.WriteString("\n" + f.project)
	}
	if f.kubernetes {
		kept(kubernetesRefusalRuntime)
		body.WriteString(f.pod)
	}
	config := filepath.Join(dir, "legion.yaml")
	beside[config] = body.String()
	for name, contents := range beside {
		if !filepath.IsAbs(name) {
			name = filepath.Join(dir, name)
		}
		if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return config
}
