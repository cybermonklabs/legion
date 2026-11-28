package textcheck

//ci:reads skills/**/*.md TestNoSkillOrRolePromptRestatesAnAgentSecretsInvocation walks ../../../../skills and reads every .md file under it
//ci:reads packages/daemon/internal/prompts/**/*.md TestNoSkillOrRolePromptRestatesAnAgentSecretsInvocation walks ../../../daemon/internal/prompts and reads every .md file under it

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// agentSecretsInvocation matches the client's name followed by an argument, which starts a
// command line: `agent-secrets NAME -- …`, `agent-secrets status <request_id>`. Its first group is
// set only for `agent-secrets --help`, the one invocation a skill or a role prompt gives. The
// client's name alone, its stderr prefix `agent-secrets:`, and a name built on it
// (`agent-secrets-placeholder-`, `skill://agent-secrets`) match nothing.
var agentSecretsInvocation = regexp.MustCompile(`(?:^|[^\w/.-])agent-secrets[ \t]+(--help\b)?`)

// TestNoSkillOrRolePromptRestatesAnAgentSecretsInvocation: no code in a skill or a role prompt
// gives an agent-secrets command but `agent-secrets --help`. A copy of the client's command line
// cannot be kept in step with the client: three Legion skill commands were copied while the client
// took --github-owner, --github-repo and --github-token, and after it removed them each exited 2
// and ran nothing, while the help said what replaced them. So a skill or a role prompt says when to
// reach for the client and what the help does not carry (a secret's name, the API a command
// calls), and points at `agent-secrets --help` for the run itself. The help's own command is held
// right by cmd/cipher's TestTheRunFormsHelpSaysHowToReadAGitHubProfilesToken, which runs it.
//
// Code is a fenced block or an inline code span, read one line at a time. The product's own
// documentation of the client, the docs site's broker pages and the client's README, is the
// place these texts point at and is not read here.
func TestNoSkillOrRolePromptRestatesAnAgentSecretsInvocation(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "..", "..")
	var files []string
	for _, dir := range []string{"skills", filepath.Join("packages", "daemon", "internal", "prompts")} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".md") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var bad []string
	pointers, sawSkill := 0, false
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		sawSkill = sawSkill || rel == "skills/agent-secrets/SKILL.md"
		for _, code := range markdownCode(string(b)) {
			for _, m := range agentSecretsInvocation.FindAllStringSubmatchIndex(code.text, -1) {
				if m[2] >= 0 {
					pointers++
					continue
				}
				bad = append(bad, fmt.Sprintf("%s:%d: %q", rel, code.line, strings.TrimSpace(code.text)))
			}
		}
	}
	// A walk that read the wrong directory, or a code reader that found no code, would pass with
	// nothing to report, so the check first shows it saw the agent-secrets skill and the pointers
	// it is meant to leave.
	if !sawSkill || pointers == 0 {
		t.Fatalf("read %d files, the agent-secrets skill among them: %t, and %d `agent-secrets --help` in their code; the check read nothing it is meant to check", len(files), sawSkill, pointers)
	}
	if len(bad) > 0 {
		t.Fatalf("code that restates an agent-secrets invocation; point at `agent-secrets --help` for the run and keep only what the help does not say:\n%s", strings.Join(bad, "\n"))
	}
}

// codeText is one piece of a markdown file's code: a line inside a fenced block, or one inline
// code span, with the number of the line it is on.
type codeText struct {
	line int
	text string
}

// markdownCode returns the code of text: each line inside a fence of ``` or ~~~ (indented under a
// list item or not), and each inline code span of any other line. A span opens with a run of
// backticks and closes at the next run of the same length on that line.
func markdownCode(text string) []codeText {
	var out []codeText
	fence := ""
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
				continue
			}
			out = append(out, codeText{i + 1, line})
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence = trimmed[:3]
			continue
		}
		for rest := line; ; {
			open := strings.IndexByte(rest, '`')
			if open < 0 {
				break
			}
			n := len(rest[open:]) - len(strings.TrimLeft(rest[open:], "`"))
			body := rest[open+n:]
			end := strings.Index(body, strings.Repeat("`", n))
			if end < 0 {
				break
			}
			out = append(out, codeText{i + 1, body[:end]})
			rest = body[end+n:]
		}
	}
	return out
}
