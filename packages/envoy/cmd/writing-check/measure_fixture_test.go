package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// syntheticCorpus is a cases file of fresh, made-up writes in several families and two
// projects. Each write of family f with index i holds paragraphs written for this test.
func syntheticCorpus(perFamily map[string]int) []Case {
	var cases []Case
	event := int64(1000)
	add := func(project, issue, surface, field, source, text string) {
		cases = append(cases, Case{EventID: event, Project: project, Issue: issue, RootIssue: issue,
			SourceID: source, Surface: surface, Field: field, Text: text, SHA256: sha256Hex([]byte(text)),
			ActorKind: "session", EventType: "test.event"})
	}
	for _, family := range sortedKeys(perFamily) {
		for i := range perFamily[family] {
			event++
			project := []string{"ALPHA", "BETA"}[i%2]
			issue := fmt.Sprintf("%s-%d", project, 1+i%7)
			source := fmt.Sprintf("%s-%d", family, i)
			para := fmt.Sprintf("The tide table lists %d high tides for May.\n\nThe kettle boils in %d seconds.", i+2, 40+i)
			switch family {
			case "comment":
				add(project, issue, surfaceComment, "body", source, para)
			case "message":
				add(project, issue, surfaceMessage, "body", source, para+"\n\n- The first step waters the plants.\n- The second step feeds the cat.")
			case "document":
				add(project, issue, surfaceDocument, "markdown", source, "# Garden notes\n\n"+para+"\n\n```sh\necho hi\n\necho bye\n```\n")
				add(project, issue, surfaceVersionSummary, "version.summary", source, "Adds the garden notes.")
			case "result":
				add(project, issue, surfaceResultRun, "run", source, "Ran the oven timer test.")
				add(project, issue, surfaceResultObserved, "observed", source, para)
				add(project, issue, surfaceResultNegativeControl, "negative_control", source, "The cold oven test failed as it should.")
			case "issue_title":
				add(project, issue, surfaceIssueTitle, "title", source, fmt.Sprintf("Fix the garden hose number %d", i))
			default:
				panic("no fixture for family " + family)
			}
		}
	}
	return cases
}

// writeCases writes cases as a JSONL file in dir and answers its path.
func writeCases(t *testing.T, dir, name string, cases []Case) string {
	t.Helper()
	var b strings.Builder
	for _, c := range cases {
		line, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// readJSONL reads a JSONL file into values of T.
func readJSONL[T any](t *testing.T, path string) []T {
	t.Helper()
	var out []T
	if err := eachLine(path, func(line []byte, _ int) error {
		var v T
		if err := json.Unmarshal(line, &v); err != nil {
			return err
		}
		out = append(out, v)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
