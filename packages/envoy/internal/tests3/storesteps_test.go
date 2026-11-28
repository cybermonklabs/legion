package tests3

//ci:reads .github/workflows/dispatch-browser.yaml TestTheStoreStepsRunTheRuleWholeUnderTheirBounds reads the shard job's two file-store steps from it under ../../../..
//ci:reads .github/scripts/dispatch-browser-wait-for-store.sh TestTheStoreStepsRunTheRuleWholeUnderTheirBounds reads the script the shard job's wait step runs under ../../../..

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	waitStep       = "Wait for the S3-compatible server"
	checkStep      = "Check that no write grew the file store"
	waitScriptPath = ".github/scripts/dispatch-browser-wait-for-store.sh"
	prepareLine    = `packages/envoy/internal/tests3/one-collection.sh prepare dispatch-e2e-objects "$RUNNER_TEMP/weed.log"`
	checkLine      = `packages/envoy/internal/tests3/one-collection.sh check dispatch-e2e-objects "$RUNNER_TEMP/weed.log"`
)

// portWait matches the wait script's deadline for the S3 port's first answer, with its seconds.
var portWait = regexp.MustCompile(`deadline=\$\(\( \$\(date \+%s\) \+ (\d+) \)\)\nuntil s3 \d+ "the S3 port's first request"`)

// joins are the words that, ending the line before, would let the shell act on the failure of the
// line after it.
var joins = []string{`\`, "&&", "||", "|", "if", "while", "until", "!"}

// storeStepFindings answers what the shard job's file-store steps break of their rule: each runs
// one-collection.sh whole, as a statement of its own at the top of its script, so a failed prepare
// or check ends the step, and each step has a timeout-minutes, so a store that stops answering ends
// it by its own name. workflow is dispatch-browser.yaml, wait the script the wait step runs and
// script one-collection.sh. It reads lines, not shell: a line test is all these few lines need.
func storeStepFindings(workflow, wait, script string) []string {
	var findings []string
	var parsed struct {
		Jobs map[string]struct {
			Steps []struct {
				Name    string `yaml:"name"`
				Run     string `yaml:"run"`
				Timeout int    `yaml:"timeout-minutes"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(workflow), &parsed); err != nil {
		return []string{fmt.Sprintf("dispatch-browser.yaml: %v", err)}
	}
	for _, want := range []struct{ name, run string }{{waitStep, waitScriptPath}, {checkStep, checkLine}} {
		found := 0
		for _, step := range parsed.Jobs["shard"].Steps {
			if step.Name != want.name {
				continue
			}
			found++
			if run := strings.TrimSpace(step.Run); run != want.run {
				findings = append(findings, fmt.Sprintf("step %q runs %q, not %q", want.name, run, want.run))
			}
			if step.Timeout <= 0 {
				findings = append(findings, fmt.Sprintf("step %q has no timeout-minutes, so a store that stops answering holds it to the job's", want.name))
			}
		}
		if found != 1 {
			findings = append(findings, fmt.Sprintf("the shard job has %d steps named %q, not one", found, want.name))
		}
	}
	for _, file := range []struct{ name, text string }{{waitScriptPath, wait}, {"one-collection.sh", script}} {
		lines := strings.Split(file.text, "\n")
		if lines[0] != "#!/bin/sh" {
			findings = append(findings, fmt.Sprintf("%s: the first line is %q, not #!/bin/sh", file.name, lines[0]))
		}
		first := true
		for _, line := range lines[1:] {
			code := strings.TrimSpace(line)
			if code == "" || strings.HasPrefix(code, "#") {
				continue
			}
			if first && code != "set -eu" {
				findings = append(findings, fmt.Sprintf("%s: the first statement is %q, not set -eu", file.name, code))
			}
			if !first && (code == "set" || strings.HasPrefix(code, "set ")) {
				findings = append(findings, fmt.Sprintf("%s: %q after set -eu, which may turn off the exit on a failed command", file.name, code))
			}
			first = false
		}
	}
	at := map[string][]int{}
	previous := ""
	for i, line := range strings.Split(wait, "\n") {
		code := strings.TrimSpace(line)
		if code == "" || strings.HasPrefix(code, "#") {
			continue
		}
		if strings.Contains(code, "one-collection.sh") {
			if line != prepareLine && line != checkLine {
				findings = append(findings, fmt.Sprintf("%s:%d runs one-collection.sh other than as the prepare or check line alone: %q", waitScriptPath, i+1, line))
			}
			for _, join := range joins {
				if previous == join || strings.HasSuffix(previous, " "+join) {
					findings = append(findings, fmt.Sprintf("%s:%d: the line before ends in %q, so the shell acts on the failure of %q", waitScriptPath, i+1, join, line))
				}
			}
			at[line] = append(at[line], i)
		}
		previous = code
	}
	for _, line := range []string{prepareLine, checkLine} {
		if len(at[line]) != 1 {
			findings = append(findings, fmt.Sprintf("%s holds %q %d times, not once", waitScriptPath, line, len(at[line])))
		}
	}
	if len(at[prepareLine]) == 1 && len(at[checkLine]) == 1 && at[prepareLine][0] > at[checkLine][0] {
		findings = append(findings, fmt.Sprintf("%s runs check before prepare", waitScriptPath))
	}
	ports := portWait.FindAllStringSubmatch(wait, -1)
	if want := fmt.Sprint(int(portDeadline.Seconds())); len(ports) != 1 || ports[0][1] != want {
		findings = append(findings, fmt.Sprintf("%s: the S3 port's wait (%v) does not end once, at portDeadline's %s s", waitScriptPath, ports, want))
	}
	return findings
}

// The shard job's file-store steps run one-collection.sh whole under bounds a reader sees, and each
// fault this test names, planted, gives a finding.
func TestTheStoreStepsRunTheRuleWholeUnderTheirBounds(t *testing.T) {
	read := func(path string) string {
		text, err := os.ReadFile("../../../../" + path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(text)
	}
	workflow, wait := read(".github/workflows/dispatch-browser.yaml"), read(waitScriptPath)
	if findings := storeStepFindings(workflow, wait, oneCollection); len(findings) > 0 {
		t.Fatalf("the file-store steps break their rule:\n%s", strings.Join(findings, "\n"))
	}
	waitTimeout := "        timeout-minutes: 3\n        run: " + waitScriptPath + "\n"
	checkTimeout := "        timeout-minutes: 2\n        run: " + checkLine + "\n"
	for _, plant := range []struct{ name, file, old, replacement, want string }{
		{"prepare run in the background", "wait", prepareLine + "\n", prepareLine + " &\n", "other than as the prepare or check line alone"},
		{"prepare's failure taken by ||", "wait", prepareLine + "\n", prepareLine + " || true\n", "other than as the prepare or check line alone"},
		{"check in an if's condition", "wait", checkLine + "\n", "if " + checkLine + "; then :; fi\n", "other than as the prepare or check line alone"},
		{"prepare on the line after an &&", "wait", prepareLine + "\n", "true &&\n" + prepareLine + "\n", `the line before ends in "&&"`},
		{"a second prepare line", "wait", prepareLine + "\n", prepareLine + "\n" + prepareLine + "\n", "2 times, not once"},
		{"the wait step without timeout-minutes", "workflow", waitTimeout, "        run: " + waitScriptPath + "\n", "has no timeout-minutes"},
		{"the check step without timeout-minutes", "workflow", checkTimeout, "        run: " + checkLine + "\n", "has no timeout-minutes"},
		{"set +e after set -eu in the wait script", "wait", "set -eu\n", "set -eu\nset +e\n", "after set -eu"},
		{"set +u after set -eu in one-collection.sh", "script", "set -eu\n", "set -eu\nset +u\n", "after set -eu"},
		{"a port wait of 90 s", "wait", "$(date +%s) + 120 ))", "$(date +%s) + 90 ))", "portDeadline's 120 s"},
	} {
		texts := map[string]string{"workflow": workflow, "wait": wait, "script": oneCollection}
		texts[plant.file] = replaceOnce(t, plant.file, texts[plant.file], plant.old, plant.replacement)
		findings := storeStepFindings(texts["workflow"], texts["wait"], texts["script"])
		if !strings.Contains(strings.Join(findings, "\n"), plant.want) {
			t.Errorf("planted %s: findings %q, want one holding %q", plant.name, findings, plant.want)
		}
		t.Logf("planted %s: %d findings: %s", plant.name, len(findings), strings.Join(findings, " | "))
	}
}
