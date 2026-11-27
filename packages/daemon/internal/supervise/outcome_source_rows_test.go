package supervise

import (
	gobuild "go/build"
	"go/constant"
	gotoken "go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The source test (TestOutcomeSourceHoldsThePolicyAtItsSites) reads source for want of a
// chokepoint: no type keeps a site from reading an agent's words. It finds no match in the four
// packages it reads today, and that shows nothing of whether it would find one. So this test
// plants each shape the negative control of c-the-outcome-is-not-a-string names, one per row, in a
// copy of one of the four packages, and runs the same analysis over the copy (refusalMatches and
// busyHeld): a row passes only where the analysis names the planted file, so a source or a matcher
// taken out of the analysis fails the row that plants it, and so does a package taken out of the
// ones the source test reads (outcomePaths). Each clean row plants the reads of the words the
// analysis lets pass — asking whether there are any, logging them, comparing an outcome or an
// error by identity, comparing the outcome a call minted from them, reading another field of a
// struct whose one field holds them — and must report nothing anywhere in the copy, so no row
// passes on a copy the analysis would report anyway.
func TestOutcomeSourceFindsEachPlantedShape(t *testing.T) {
	fileset := gotoken.NewFileSet()
	imports := outcomeImporter(t, fileset)
	bridge := checkOutcomeSource(t, fileset, imports, claudebridgePath, outcomeDirs[claudebridgePath])
	words := constant.StringVal(bridge.bridgeBusyWords(t).Val())
	copies := copyOutcomeSources(t)
	planted := func(match wordMatch) bool {
		return filepath.Base(fileset.Position(match.at.Pos()).Filename) == plantedFile
	}
	for _, row := range plantedShapes {
		t.Run(row.name, func(t *testing.T) {
			if !slices.Contains(outcomePaths, row.path) {
				t.Fatalf("the source test reads no package %s, where this row plants its shape", row.path)
			}
			pkg := plantShape(t, fileset, imports, row.path, copies[row.path], row.source)
			if row.clean {
				for _, match := range append(pkg.refusalMatches(t), pkg.busyHeld(t, words)...) {
					t.Errorf("%s, in %s: the analysis reports %s, where it should let the read pass", pkg.pos(match.at), match.in, match.what)
				}
				return
			}
			// One match in the planted file passes the row, so the busy words are looked for only
			// where no match of a refusal's words is there.
			found := pkg.refusalMatches(t)
			at := slices.IndexFunc(found, planted)
			if at < 0 {
				found = pkg.busyHeld(t, words)
				at = slices.IndexFunc(found, planted)
			}
			if at < 0 {
				t.Errorf("the analysis reports nothing in %s of %s: the source test no longer finds %s", plantedFile, row.path, row.name)
				return
			}
			t.Logf("%s, in %s: %s", pkg.pos(found[at].at), found[at].in, found[at].what)
		})
	}
}

// plantedFile is the file plantShape writes a row's shape to.
const plantedFile = "planted.go"

// copyOutcomeSources copies the non-test source of each package the source test reads
// (outcomePaths), as this build compiles it, into a directory of the test's own, and answers each
// copy's directory by import path. It copies each package once for all the rows, which run one
// after another, each planting its shape in its package's copy (plantShape).
func copyOutcomeSources(t *testing.T) map[string]string {
	t.Helper()
	copies := map[string]string{}
	for _, path := range outcomePaths {
		listed, err := gobuild.ImportDir(outcomeDirs[path], 0)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		for _, name := range listed.GoFiles {
			text, err := os.ReadFile(filepath.Join(outcomeDirs[path], name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), text, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		copies[path] = dir
	}
	return copies
}

// plantShape writes source as plantedFile into dir, the copy of the package at path
// (copyOutcomeSources), over the shape the row before it planted there, and type-checks the copy
// as that package (checkOutcomeSource), so the analysis reads the package as it would read it
// with the shape written into it, and no other row's.
func plantShape(t *testing.T, fileset *gotoken.FileSet, imports types.Importer, path, dir, source string) *goSource {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, plantedFile), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return checkOutcomeSource(t, fileset, imports, path, dir)
}

// plantedShapes is each shape the negative control of c-the-outcome-is-not-a-string names, one per
// row: a match of a refusal's text by a matcher of each of strings, bytes, slices, maps and
// regexp; by ==, !=, each ordered comparison, a switch case and a map key; a match of a constant
// holding the busy words; a match of a variable that ranges over a list or a map of the texts; a
// match of a struct field the text is written into in the same function, by a literal, its
// address, a positional literal or an assignment; a match of the result of LastRefusal(); and a
// match of err.Error() where err is a prompt send's answer. The matchers of cmp, path,
// path/filepath, fmt and sort follow, then a match of a text compared through a pointer to it; a
// prompt send's err.Error() in the stream, through its call and its Prompt, and in the runtime; a
// turn outcome's Message and its report's, which hold what LastRefusal returns; a conversion of
// the text to a type whose underlying type is string — runtime.Outcome or a package's own —
// compared by ==, as a map key, by a switch and by slices.Contains, and through a variable given
// it; a package-level string written in one function and compared in another; and a clean row
// for each of the four packages. The analysis finds each shape by one route alone — its source,
// the range, the field, the pointer, the conversion or the package's variable it passes through,
// if any, and its matcher, or the busy words its constant holds — so taking any of those out of
// the analysis fails that row.
var plantedShapes = []struct {
	name, path, source string
	// clean is a row of reads the analysis lets pass, which must report nothing.
	clean bool
}{
	{name: "strings.HasPrefix of RefusalReason in runtime", path: runtimePath, source: `package runtime

import "strings"

func plantedShape(err error) bool {
	return strings.HasPrefix(RefusalReason(err), "Agent is")
}
`},
	{name: "bytes.HasPrefix of a refusal's Reason in stream", path: streamPath, source: `package stream

import "bytes"

func plantedShape(refusal *RefusedError) bool {
	return bytes.HasPrefix([]byte(refusal.Reason), []byte("Agent is"))
}
`},
	{name: "slices.Contains of a response's Error in claudebridge", path: claudebridgePath, source: `package claudebridge

import (
	"slices"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

func plantedShape(answer shimwire.Response) bool {
	return slices.Contains([]string{"Agent is busy"}, answer.Error)
}
`},
	{name: "maps.Equal of RefusalReason in runtime", path: runtimePath, source: `package runtime

import "maps"

func plantedShape(err error) bool {
	return maps.Equal(map[string]bool{RefusalReason(err): true}, map[string]bool{"Agent is busy": true})
}
`},
	{name: "a regexp.Regexp's MatchString of a refusal's RefusalReason in stream", path: streamPath, source: `package stream

import "regexp"

func plantedShape(refusal *RefusedError) bool {
	return regexp.MustCompile("^Agent is").MatchString(refusal.RefusalReason())
}
`},
	{name: "== of a late refusal's Error in supervise", path: supervisePath, source: `package supervise

func plantedShape(refusal StreamLateRefusal) bool {
	return refusal.Error == "Agent is busy"
}
`},
	{name: "!= of a prompt result's Error in stream", path: streamPath, source: `package stream

func plantedShape(result PromptResult) bool {
	return result.Error != "Agent is busy"
}
`},
	{name: "< of RefusalReason in runtime", path: runtimePath, source: `package runtime

func plantedShape(err error) bool {
	return RefusalReason(err) < "Agent is busy"
}
`},
	{name: "<= of RefusalReason in runtime", path: runtimePath, source: `package runtime

func plantedShape(err error) bool {
	return RefusalReason(err) <= "Agent is busy"
}
`},
	{name: "> of RefusalReason in runtime", path: runtimePath, source: `package runtime

func plantedShape(err error) bool {
	return "Agent is busy" > RefusalReason(err)
}
`},
	{name: ">= of RefusalReason in runtime", path: runtimePath, source: `package runtime

func plantedShape(err error) bool {
	return RefusalReason(err) >= "Agent is"
}
`},
	{name: "a switch case on a response's Error in claudebridge", path: claudebridgePath, source: `package claudebridge

import "github.com/sjawhar/legion/daemon/internal/shimwire"

func plantedShape(answer shimwire.Response) bool {
	switch answer.Error {
	case "Agent is busy":
		return true
	}
	return false
}
`},
	{name: "a map key of a delivery's LastSendError in supervise", path: supervisePath, source: `package supervise

func plantedShape(d Delivery, busy map[string]bool) bool {
	return busy[d.LastSendError]
}
`},
	{name: "a constant holding the busy words in runtime", path: runtimePath, source: `package runtime

import "strings"

const plantedBusy = "Agent is already processing"

func plantedShape(answer string) bool {
	return strings.HasPrefix(answer, plantedBusy)
}
`},
	{name: "a variable that ranges over a list of RefusalReason in runtime", path: runtimePath, source: `package runtime

func plantedShape(err error) bool {
	for _, reason := range []string{RefusalReason(err)} {
		if reason == "Agent is busy" {
			return true
		}
	}
	return false
}
`},
	{name: "a variable that ranges over a map of RefusalReason in runtime", path: runtimePath, source: `package runtime

func plantedShape(err error) bool {
	for reason := range map[string]bool{RefusalReason(err): true} {
		if reason == "Agent is busy" {
			return true
		}
	}
	return false
}
`},
	{name: "a struct literal's field compared in the same function in supervise", path: supervisePath, source: `package supervise

type plantedNote struct{ words, kind string }

func plantedShape(refusal StreamLateRefusal) bool {
	note := plantedNote{words: refusal.Error, kind: "late"}
	return note.words == "Agent is busy"
}
`},
	{name: "a field of a struct literal's address compared in the same function in runtime", path: runtimePath, source: `package runtime

type plantedNote struct{ words, kind string }

func plantedShape(err error) bool {
	note := &plantedNote{words: RefusalReason(err), kind: "send"}
	return note.words == "Agent is busy"
}
`},
	{name: "a positional struct literal's field compared in the same function in claudebridge", path: claudebridgePath, source: `package claudebridge

import "github.com/sjawhar/legion/daemon/internal/shimwire"

type plantedNote struct{ kind, words string }

func plantedShape(answer shimwire.Response) bool {
	var note = plantedNote{"answer", answer.Error}
	return note.words == "Agent is busy"
}
`},
	{name: "a field assigned a refusal's Reason and compared in the same function in stream", path: streamPath, source: `package stream

type plantedNote struct{ words string }

func plantedShape(refusal *RefusedError) bool {
	var note plantedNote
	note.words = refusal.Reason
	return note.words == "Agent is busy"
}
`},
	{name: "strings.Contains of LastRefusal in supervise", path: supervisePath, source: `package supervise

import "strings"

func plantedShape(c Claim) bool {
	return strings.Contains(c.LastRefusal(), "Agent is")
}
`},
	{name: "strings.HasPrefix of a prompt send's err.Error() in supervise", path: supervisePath, source: `package supervise

import (
	"context"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

func plantedShape(ctx context.Context, conn runtime.Conn) bool {
	err := conn.Prompt(ctx, "delivery", "task")
	return err != nil && strings.HasPrefix(err.Error(), "Agent is")
}
`},
	{name: "cmp.Compare of RefusalReason in runtime", path: runtimePath, source: `package runtime

import "cmp"

func plantedShape(err error) bool {
	return cmp.Compare(RefusalReason(err), "Agent is busy") == 0
}
`},
	{name: "cmp.Less of RefusalReason in runtime", path: runtimePath, source: `package runtime

import "cmp"

func plantedShape(err error) bool {
	return cmp.Less(RefusalReason(err), "Agent is busy")
}
`},
	{name: "path.Match of RefusalReason in runtime", path: runtimePath, source: `package runtime

import "path"

func plantedShape(err error) bool {
	matched, _ := path.Match("Agent is *", RefusalReason(err))
	return matched
}
`},
	{name: "filepath.Match of RefusalReason in runtime", path: runtimePath, source: `package runtime

import "path/filepath"

func plantedShape(err error) bool {
	matched, _ := filepath.Match("Agent is *", RefusalReason(err))
	return matched
}
`},
	{name: "fmt.Sscan of RefusalReason in runtime", path: runtimePath, source: `package runtime

import "fmt"

func plantedShape(err error) bool {
	read, _ := fmt.Sscan(RefusalReason(err), new(string))
	return read == 1
}
`},
	{name: "fmt.Sscanf of RefusalReason in runtime", path: runtimePath, source: `package runtime

import "fmt"

func plantedShape(err error) bool {
	read, _ := fmt.Sscanf(RefusalReason(err), "Agent is %s", new(string))
	return read == 1
}
`},
	{name: "fmt.Sscanln of RefusalReason in runtime", path: runtimePath, source: `package runtime

import "fmt"

func plantedShape(err error) bool {
	read, _ := fmt.Sscanln(RefusalReason(err), new(string))
	return read == 1
}
`},
	{name: "sort.SearchStrings of RefusalReason in runtime", path: runtimePath, source: `package runtime

import "sort"

func plantedShape(err error) bool {
	return sort.SearchStrings([]string{"Agent is busy"}, RefusalReason(err)) == 0
}
`},
	{name: "a pointer to RefusalReason dereferenced and compared in the same function in runtime", path: runtimePath, source: `package runtime

func plantedShape(err error) bool {
	reason := RefusalReason(err)
	p := &reason
	return *p == "Agent is busy"
}
`},
	{name: "strings.HasPrefix of a prompt send's err.Error() through the stream's call in stream", path: streamPath, source: `package stream

import (
	"context"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

func (c *Conn) plantedShape(ctx context.Context) bool {
	_, err := c.call(ctx, shimwire.Prompt{ID: "planted", DeliveryID: "delivery", Message: "task"}, "planted", "delivery")
	return err != nil && strings.HasPrefix(err.Error(), "Agent is")
}
`},
	{name: "== of a prompt send's err.Error() through the stream's Prompt in stream", path: streamPath, source: `package stream

import "context"

func plantedShape(ctx context.Context, c *Conn) bool {
	err := c.Prompt(ctx, "delivery", "task")
	return err != nil && err.Error() == "Agent is busy"
}
`},
	{name: "strings.Contains of a runtime.Conn's Prompt err.Error() in runtime", path: runtimePath, source: `package runtime

import (
	"context"
	"strings"
)

func plantedShape(ctx context.Context, conn Conn) bool {
	err := conn.Prompt(ctx, "delivery", "task")
	return err != nil && strings.Contains(err.Error(), "Agent is")
}
`},
	{name: "== of a turn outcome's Message in supervise", path: supervisePath, source: `package supervise

func plantedShape(o TurnOutcome) bool {
	return o.Refused && o.Message == "Agent is busy"
}
`},
	{name: "strings.Contains of a turn outcome report's Message in supervise", path: supervisePath, source: `package supervise

import "strings"

func plantedShape(r RequestTurnOutcome) bool {
	return strings.Contains(r.Message, "credit balance")
}
`},
	{name: "== of runtime.Outcome of a late refusal's Error in supervise", path: supervisePath, source: `package supervise

import "github.com/sjawhar/legion/daemon/internal/runtime"

func plantedShape(refusal StreamLateRefusal) bool {
	return runtime.Outcome(refusal.Error) == runtime.OutcomeAgentBusy
}
`},
	{name: "== of Outcome of RefusalReason in runtime", path: runtimePath, source: `package runtime

func plantedShape(err error) bool {
	return Outcome(RefusalReason(err)) == OutcomeAgentBusy
}
`},
	{name: "a map key of Outcome of RefusalReason in runtime", path: runtimePath, source: `package runtime

func plantedShape(err error, busy map[Outcome]bool) bool {
	return busy[Outcome(RefusalReason(err))]
}
`},
	{name: "a switch on runtime.Outcome of a prompt result's Error in stream", path: streamPath, source: `package stream

import "github.com/sjawhar/legion/daemon/internal/runtime"

func plantedShape(result PromptResult) bool {
	switch runtime.Outcome(result.Error) {
	case runtime.OutcomeAgentBusy:
		return true
	}
	return false
}
`},
	{name: "== of a refusal's Reason converted to the package's own string type in stream", path: streamPath, source: `package stream

type plantedKind string

func plantedShape(refusal *RefusedError) bool {
	return plantedKind(refusal.Reason) == "Agent is"
}
`},
	{name: "slices.Contains of a response's Error converted to the package's own string type in claudebridge", path: claudebridgePath, source: `package claudebridge

import (
	"slices"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

type plantedKind string

func plantedShape(answer shimwire.Response) bool {
	return slices.Contains([]plantedKind{"Agent is busy"}, plantedKind(answer.Error))
}
`},
	{name: "a variable given runtime.Outcome of a late refusal's Error and compared in supervise", path: supervisePath, source: `package supervise

import "github.com/sjawhar/legion/daemon/internal/runtime"

func plantedShape(refusal StreamLateRefusal) bool {
	outcome := runtime.Outcome(refusal.Error)
	return outcome == runtime.OutcomeAgentBusy
}
`},
	{name: "a package-level string written in one function and compared in another in runtime", path: runtimePath, source: `package runtime

var plantedReason string

func plantedKeep(err error) { plantedReason = RefusalReason(err) }

func plantedShape() bool {
	return plantedReason == "Agent is busy"
}
`},
	{name: "clean reads in supervise", path: supervisePath, clean: true, source: `package supervise

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

func plantedShape(ctx context.Context, conn runtime.Conn, c Claim, refusal StreamLateRefusal) string {
	err := conn.Prompt(ctx, "delivery", "task")
	if err == nil || !errors.Is(err, runtime.ErrPromptRefused) || c.LastRefusal() == "" {
		return ""
	}
	if refusal.Error > "" && runtime.RefusalOutcome(err) == runtime.OutcomeAgentBusy && refusal.Outcome == runtime.OutcomeAgentBusy {
		return "busy"
	}
	return fmt.Sprintf("refused: %s (%s)", err.Error(), c.LastRefusal())
}

func plantedEnd(c Claim) bool {
	ended := EndedWork{Cause: c.LastRefusal(), Kinds: []string{"bash"}}
	ended.Kinds = append(ended.Kinds, "task")
	for _, kind := range ended.Kinds {
		if kind == "bash" {
			return ended.Cause != ""
		}
	}
	return slices.Contains(ended.Kinds, "task")
}

func plantedMinted(refused PromptRefused) bool {
	return sendOutcome(refused.Err) == runtime.OutcomeAgentBusy || runtime.RefusalOutcome(refused.Err) != runtime.OutcomePromptRefused
}
`},
	{name: "clean reads in runtime", path: runtimePath, clean: true, source: `package runtime

import (
	"errors"
	"fmt"
)

func plantedShape(err error) string {
	reason := RefusalReason(err)
	if reason == "" || !errors.Is(err, ErrPromptRefused) || RefusalOutcome(err) != OutcomeAgentBusy {
		return ""
	}
	return fmt.Sprintf("busy (%d bytes): %s", len(reason), reason)
}
`},
	{name: "clean reads in stream", path: streamPath, clean: true, source: `package stream

import "github.com/sjawhar/legion/daemon/internal/runtime"

func plantedShape(c *Conn, refusal *RefusedError, result PromptResult, code string) string {
	if refusal.Reason == "" && result.Error == "" {
		return ""
	}
	if c.promptRefusal(refusal.Reason, code) == runtime.OutcomeAgentBusy && result.Outcome == runtime.OutcomeAgentBusy {
		return "busy"
	}
	return "refused: " + refusal.RefusalReason() + "; " + result.Error
}
`},
	{name: "clean reads in claudebridge", path: claudebridgePath, clean: true, source: `package claudebridge

import "github.com/sjawhar/legion/daemon/internal/shimwire"

func plantedShape(answer shimwire.Response) bool {
	return !answer.Success && answer.Error != "" && answer.Code == shimwire.CodeSessionBusy
}
`},
}
