package wirecheck_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/wirecheck"
)

// The fixtures below are one throwaway module each, named fixture: its package fixture is the
// client under check and its package fixture/wire is the wire package, so a finding names its
// type as the client package spells it (fixture.sendBody).
const fixtureWire = "fixture/wire"

// wireSource is the wire package every fixture's client may use: a request body, an answer row,
// the row with its id held as a type the client names (as listenerwire's SessionInfoAs is), and an
// error answer.
const wireSource = `package wire

type Body struct {
	Message string ` + "`json:\"message\"`" + `
	Payload string ` + "`json:\"payload\"`" + `
	Detail  *string ` + "`json:\"detail\"`" + `
}

type Row struct {
	SessionID string ` + "`json:\"session_id\"`" + `
}

type RowAs[Text any] struct {
	SessionID Text ` + "`json:\"session_id\"`" + `
}

type Failure struct {
	Error string ` + "`json:\"error\"`" + `
}
`

// fixtureModule writes a module holding the wire package and client, the source of package
// fixture, and returns its directory. The run is out of any workspace, so the module stands alone.
func fixtureModule(t *testing.T, client string) string {
	t.Helper()
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"go.mod":             "module fixture\n\ngo 1.27\n",
		"wire/wire.go":       wireSource,
		"fixture/fixture.go": client,
	})
	return dir
}

// envoyFixture is the module path of the fixture envoyFixtureModule writes: it lies under envoy's
// own, which is what lets its client import envoy's internal packages, internal/redact among them.
const envoyFixture = "github.com/sjawhar/envoy/wirecheckfixture"

// envoyFixtureModule writes the wire package and client, the source of package fixture, as module
// envoyFixture, in a workspace with this repository's envoy module, so the client reads an answer
// through envoy's own redact.Answer as the clients the check holds do, rather than through a stub
// whose method a change of the real one would leave behind. It returns the module's directory.
func envoyFixtureModule(t *testing.T, client string) string {
	t.Helper()
	envoy, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	goMod, err := os.ReadFile(filepath.Join(envoy, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	version := ""
	for line := range strings.Lines(string(goMod)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "go "); ok {
			version = v
			break
		}
	}
	if !strings.HasPrefix(string(goMod), "module github.com/sjawhar/envoy\n") || version == "" {
		t.Fatalf("%s is not envoy's go.mod naming its go version", filepath.Join(envoy, "go.mod"))
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"go.mod":             "module " + envoyFixture + "\n\ngo " + version + "\n",
		"go.work":            "go " + version + "\n\nuse (\n\t.\n\t" + envoy + "\n)\n",
		"wire/wire.go":       wireSource,
		"fixture/fixture.go": client,
	})
	t.Setenv("GOWORK", filepath.Join(dir, "go.work"))
	return dir
}

// writeFiles writes each source under dir at its relative name.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, source := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// check runs wirecheck.Check over the fixture's client package.
func check(t *testing.T, client string, transport ...string) []wirecheck.Finding {
	t.Helper()
	return wirecheck.Check(t, fixtureModule(t, client), []string{"./fixture"}, fixtureWire, transport)
}

// checkInEnvoy runs wirecheck.Check over the client of an envoyFixtureModule.
func checkInEnvoy(t *testing.T, client string) []wirecheck.Finding {
	t.Helper()
	return wirecheck.Check(t, envoyFixtureModule(t, client), []string{"./fixture"}, envoyFixture+"/wire", nil)
}

// failures is a testing.TB that records what Check fails with instead of failing the test, so a
// test can assert that Check fails and on what.
type failures struct {
	testing.TB
	messages []string
}

func (f *failures) Helper() {}

func (f *failures) Fatalf(format string, args ...any) {
	f.messages = append(f.messages, fmt.Sprintf(format, args...))
}

func (f *failures) Errorf(format string, args ...any) {
	f.messages = append(f.messages, fmt.Sprintf(format, args...))
}

func (f *failures) Fatal(args ...any) { f.messages = append(f.messages, fmt.Sprint(args...)) }

func (f *failures) Error(args ...any) { f.messages = append(f.messages, fmt.Sprint(args...)) }

// want asserts findings are exactly the types named, in order, each passed to call.
func want(t *testing.T, findings []wirecheck.Finding, call string, types ...string) {
	t.Helper()
	if len(findings) != len(types) {
		t.Fatalf("findings = %v, want %d naming %v", findings, len(types), types)
	}
	for i, finding := range findings {
		if finding.Type != types[i] || finding.Call != call {
			t.Errorf("finding %d = %+v, want type %q passed to %q", i, finding, types[i], call)
		}
		if !strings.HasSuffix(finding.Pos.Filename, filepath.Join("fixture", "fixture.go")) || finding.Pos.Line == 0 {
			t.Errorf("finding %d is at %v, want a line of fixture/fixture.go", i, finding.Pos)
		}
	}
}

const sendFixture = `package fixture

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"fixture/wire"
)

type sendBody struct {
	Message string ` + "`json:\"message\"`" + `
}

func Send(ctx context.Context, url string) error {
	body, err := json.Marshal(sendBody{Message: "hello"})
	if err != nil {
		return err
	}
	return post(ctx, url, body)
}

func SendWire(ctx context.Context, url string) error {
	body, err := json.Marshal(wire.Body{Message: "hello"})
	if err != nil {
		return err
	}
	return post(ctx, url, body)
}

func post(ctx context.Context, url string, body []byte) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	return response.Body.Close()
}
`

// A body the client declares itself, though it has the wire body's one field and its JSON name,
// is the one finding: the same send of the wire package's own type is none.
func TestCheckFindsACopiedBody(t *testing.T) {
	want(t, check(t, sendFixture, "fixture/fixture.post"), "encoding/json.Marshal", "fixture.sendBody")
}

func TestCheckFindsAnAnonymousStruct(t *testing.T) {
	source := strings.Replace(sendFixture, `json.Marshal(sendBody{Message: "hello"})`,
		"json.Marshal(struct {\n\t\tMessage string `json:\"message\"`\n\t}{Message: \"hello\"})", 1)
	findings := check(t, source, "fixture/fixture.post")
	if len(findings) != 1 || !strings.HasPrefix(findings[0].Type, "struct{Message string") {
		t.Fatalf("findings = %v, want one naming the anonymous struct", findings)
	}
}

func TestCheckFindsAMap(t *testing.T) {
	source := strings.Replace(sendFixture, `json.Marshal(sendBody{Message: "hello"})`,
		`json.Marshal(map[string]string{"message": "hello"})`, 1)
	want(t, check(t, source, "fixture/fixture.post"), "encoding/json.Marshal", "map[string]string")
}

// A JSON document carried as a string inside a wire body is the body's payload, not a body of
// its own, so its marshal is no finding; the same marshal sent as the body itself is one.
func TestCheckIgnoresAPayloadString(t *testing.T) {
	source := strings.Replace(sendFixture, `body, err := json.Marshal(sendBody{Message: "hello"})`,
		`payload, err := json.Marshal(sendBody{Message: "hello"})
	if err != nil {
		return err
	}
	body, err := json.Marshal(wire.Body{Message: "hello", Payload: string(payload)})`, 1)
	if findings := check(t, source, "fixture/fixture.post"); len(findings) != 0 {
		t.Fatalf("findings = %v, want none for a payload carried as a string", findings)
	}

	sentWhole := strings.Replace(source, `return post(ctx, url, body)
}

func SendWire`, `return post(ctx, url, append(body, payload...))
}

func SendWire`, 1)
	want(t, check(t, sentWhole, "fixture/fixture.post"), "encoding/json.Marshal", "fixture.sendBody")
}

func TestCheckFindsAMarshalIndentBody(t *testing.T) {
	source := strings.Replace(sendFixture, `json.Marshal(sendBody{Message: "hello"})`,
		`json.MarshalIndent(sendBody{Message: "hello"}, "", "  ")`, 1)
	want(t, check(t, source, "fixture/fixture.post"), "encoding/json.MarshalIndent", "fixture.sendBody")
}

// A marshal converted to a string is a payload only where the string goes into a body's field: the
// same string sent as the request body itself is the body, and is judged.
func TestCheckFindsABodySentAsAString(t *testing.T) {
	source := strings.Replace(sendFixture, `return post(ctx, url, body)
}

func SendWire`, `return post(ctx, url, []byte(string(body)))
}

func SendWire`, 1)
	want(t, check(t, source, "fixture/fixture.post"), "encoding/json.Marshal", "fixture.sendBody")
}

// A payload string may reach its body's field through a variable, by value or by its address, as
// the daemon's publisher and the broker's wake carry theirs, and by an assignment to the field of a
// body that is then sent; it is a payload all the same.
func TestCheckIgnoresAPayloadCarriedThroughAVariable(t *testing.T) {
	source := strings.Replace(sendFixture, `body, err := json.Marshal(sendBody{Message: "hello"})`,
		`payload, err := json.Marshal(sendBody{Message: "hello"})
	if err != nil {
		return err
	}
	text := string(payload)
	detail := string(payload)
	outer := wire.Body{Message: text}
	outer.Payload = string(payload)
	outer.Detail = &detail
	body, err := json.Marshal(outer)`, 1)
	if findings := check(t, source, "fixture/fixture.post"); len(findings) != 0 {
		t.Fatalf("findings = %v, want none for a payload carried through a variable", findings)
	}
}

// The variable a marshal or a conversion is given to may be declared beforehand and assigned with
// a plain =, which go/types records as a use of it: that assignment is the variable's own write,
// not a use of what it holds.
func TestCheckIgnoresAPayloadAssignedToADeclaredVariable(t *testing.T) {
	source := strings.Replace(sendFixture, `body, err := json.Marshal(sendBody{Message: "hello"})`,
		`var payload []byte
	var err error
	payload, err = json.Marshal(sendBody{Message: "hello"})
	if err != nil {
		return err
	}
	var text string
	text = string(payload)
	body, err := json.Marshal(wire.Body{Message: "hello", Payload: text})`, 1)
	if findings := check(t, source, "fixture/fixture.post"); len(findings) != 0 {
		t.Fatalf("findings = %v, want none for a payload assigned to a declared variable", findings)
	}
}

// A conversion given to one variable of several, by an assignment or a declaration of several
// values, is held by that variable alone.
func TestCheckIgnoresAPayloadHeldByOneOfSeveralVariables(t *testing.T) {
	source := strings.Replace(sendFixture, `body, err := json.Marshal(sendBody{Message: "hello"})`,
		`payload, err := json.Marshal(sendBody{Message: "hello"})
	if err != nil {
		return err
	}
	text, count := string(payload), 1
	var detail, other = string(payload), 2
	body, err := json.Marshal(wire.Body{Message: text, Payload: detail})
	_ = count + other`, 1)
	if findings := check(t, source, "fixture/fixture.post"); len(findings) != 0 {
		t.Fatalf("findings = %v, want none for a payload held by one of several variables", findings)
	}
}

// MarshalIndent's result carried as a payload is exempt as Marshal's is.
func TestCheckIgnoresAMarshalIndentPayload(t *testing.T) {
	source := strings.Replace(sendFixture, `body, err := json.Marshal(sendBody{Message: "hello"})`,
		`payload, err := json.MarshalIndent(sendBody{Message: "hello"}, "", "  ")
	if err != nil {
		return err
	}
	body, err := json.Marshal(wire.Body{Message: "hello", Payload: string(payload)})`, 1)
	if findings := check(t, source, "fixture/fixture.post"); len(findings) != 0 {
		t.Fatalf("findings = %v, want none for a MarshalIndent payload", findings)
	}
}

// A string goes into a body's field only where that field is one of a wire type's: a map, a struct
// of the client's own, or an anonymous struct the string is put in and read back out of before it
// is sent carries the body past nothing.
func TestCheckFindsABodySentThroughAValueOfItsOwn(t *testing.T) {
	for name, carrier := range map[string]string{
		"a map": `held := map[string]string{"body": string(body)}
	return post(ctx, url, []byte(held["body"]))`,
		"a struct of its own": `held := request{body: string(body)}
	return post(ctx, url, []byte(held.body))`,
		"an anonymous struct": `var held struct{ body string }
	held.body = string(body)
	return post(ctx, url, []byte(held.body))`,
	} {
		t.Run(name, func(t *testing.T) {
			source := strings.Replace(sendFixture, `return post(ctx, url, body)
}

func SendWire`, carrier+`
}

type request struct{ body string }

func SendWire`, 1)
			want(t, check(t, source, "fixture/fixture.post"), "encoding/json.Marshal", "fixture.sendBody")
		})
	}
}

// The exemption reads where a marshalled string goes, not where it ends up: once it goes into a
// field of a wire type's value it is taken as a payload, whatever is done with that field
// afterwards. So a body put into a listenerwire field and read back out to be sent whole is no
// finding. Following each field to the request would mean tracing every value a wire body's field
// flows to; the check does not, and this pins that it does not, so a change to it is a decision
// rather than a surprise.
func TestCheckTakesAStringInAWireFieldAsAPayloadWhateverFollows(t *testing.T) {
	source := strings.Replace(sendFixture, `return post(ctx, url, body)
}

func SendWire`, `held := wire.Body{Payload: string(body)}
	return post(ctx, url, []byte(held.Payload))
}

func SendWire`, 1)
	if findings := check(t, source, "fixture/fixture.post"); len(findings) != 0 {
		t.Fatalf("findings = %v, want none: a string in a wire type's field is a payload, whatever is done with the field after", findings)
	}
}

const answerFixture = `package fixture

import (
	"context"
	"encoding/json"
	"net/http"

	"fixture/wire"
)

type Client struct{ base string }

type row struct {
	SessionID string ` + "`json:\"session_id\"`" + `
}

func (c *Client) get(ctx context.Context, route string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+route, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(request)
}

func (c *Client) Rows(ctx context.Context) ([]row, error) {
	response, err := c.get(ctx, "/rows")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, refusal(response)
	}
	var rows []row
	err = json.NewDecoder(response.Body).Decode(&rows)
	return rows, err
}

func (c *Client) WireRows(ctx context.Context) ([]wire.Row, error) {
	response, err := c.get(ctx, "/rows")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var rows []wire.Row
	err = json.NewDecoder(response.Body).Decode(&rows)
	return rows, err
}

func refusal(response *http.Response) error {
	var body struct {
		Error string ` + "`json:\"error\"`" + `
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return err
	}
	var failure wire.Failure
	_ = json.NewDecoder(response.Body).Decode(&failure)
	return nil
}

// Format marshals a row for a log line. It makes no call and reads no answer, so its marshal is
// none of the check's business.
func Format(r row) string {
	text, _ := json.Marshal(r)
	return string(text) + "."
}
`

// An answer read by a function that called a helper returning the response, and an error answer
// read by a function handed the response, are both read from the listener; a marshal in a
// function that touches no request or answer is not.
func TestCheckFindsACopiedAnswer(t *testing.T) {
	findings := check(t, answerFixture)
	if len(findings) != 2 {
		t.Fatalf("findings = %v, want the copied row and the anonymous error body", findings)
	}
	if findings[0].Type != "fixture.row" || findings[0].Call != "(*encoding/json.Decoder).Decode" {
		t.Errorf("finding 0 = %+v, want fixture.row passed to (*encoding/json.Decoder).Decode", findings[0])
	}
	if !strings.HasPrefix(findings[1].Type, "struct{Error string") {
		t.Errorf("finding 1 = %+v, want the anonymous error body", findings[1])
	}
}

const bodyReaderFixture = `package fixture

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"fixture/wire"
)

type refusal struct {
	Error string ` + "`json:\"error\"`" + `
}

func Send(ctx context.Context, url string, body []byte) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	return readRefusal(answer)
}

// readRefusal decodes the answer bytes a party call hands it.
func readRefusal(answer []byte) error {
	var body refusal
	if err := json.Unmarshal(answer, &body); err != nil {
		return err
	}
	var failure wire.Failure
	return json.NewDecoder(bytes.NewReader(answer)).Decode(&failure)
}

// parseConfig decodes bytes nothing of the other party's handed it.
func parseConfig(data []byte) (refusal, error) {
	var config refusal
	err := json.Unmarshal(data, &config)
	return config, err
}

func Load(path string) (refusal, error) {
	return parseConfig([]byte(path))
}
`

// A function that decodes the bytes of an answer a party call read and handed it is read as part
// of that call, so moving a decode into a helper does not hide it; the same decode reached from
// no party call is none of the check's business.
func TestCheckReadsABodyAPartyCallHandsAHelper(t *testing.T) {
	want(t, check(t, bodyReaderFixture), "encoding/json.Unmarshal", "fixture.refusal")
}

const noPartyFixture = `package fixture

import "encoding/json"

type row struct{ ID string }

func Format(r row) string {
	text, _ := json.Marshal(r)
	return string(text) + "."
}
`

// A package with nothing that sends a request or reads an answer is a check that could see
// nothing, so it fails as UNKNOWN, naming the package, rather than passing clean.
func TestCheckWithNoTransportIsUnknown(t *testing.T) {
	recorded := &failures{TB: t}
	findings := wirecheck.Check(recorded, fixtureModule(t, noPartyFixture), []string{"./fixture"}, fixtureWire, nil)
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none from a check that failed", findings)
	}
	if len(recorded.messages) == 0 || !strings.Contains(strings.Join(recorded.messages, "\n"), "UNKNOWN") ||
		!strings.Contains(strings.Join(recorded.messages, "\n"), "fixture/fixture") {
		t.Fatalf("Check failed with %q, want UNKNOWN naming fixture/fixture", recorded.messages)
	}
}

// A transport named that is no function of the checked packages would leave its callers unread,
// so it fails the check, naming it, rather than narrowing what the check sees.
func TestCheckRefusesATransportThatNamesNoFunction(t *testing.T) {
	recorded := &failures{TB: t}
	wirecheck.Check(recorded, fixtureModule(t, sendFixture), []string{"./fixture"}, fixtureWire, []string{"fixture/fixture.psot"})
	if !strings.Contains(strings.Join(recorded.messages, "\n"), "fixture/fixture.psot") {
		t.Fatalf("Check failed with %q, want a failure naming the transport fixture/fixture.psot", recorded.messages)
	}
}

const helperFixture = `package fixture

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"fixture/wire"
)

type B struct{ client *http.Client }

type reply struct {
	Lease string ` + "`json:\"lease\"`" + `
}

func (b *B) do(req *http.Request, out any) error {
	response, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (b *B) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://cipher"+path, nil)
	if err != nil {
		return err
	}
	return b.do(req, out)
}

func (b *B) Lease(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://cipher/lease", nil)
	if err != nil {
		return "", err
	}
	var out reply
	if err := b.do(req, &out); err != nil {
		return "", err
	}
	return out.Lease, nil
}

func (b *B) WireRow(ctx context.Context) (wire.Row, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://cipher/row", nil)
	if err != nil {
		return wire.Row{}, err
	}
	var out wire.Row
	err = b.do(req, &out)
	return out, err
}
`

// A helper that hands its own any to a JSON call is a transport helper, and each caller's
// argument for it is judged where the caller passes it: a copy there is the finding, at the
// call site, and a wire type is none.
func TestCheckFollowsAnyThroughATransportHelper(t *testing.T) {
	want(t, check(t, helperFixture), "(*fixture/fixture.B).do", "fixture.reply")
}

func TestCheckFollowsNestedHelpers(t *testing.T) {
	source := helperFixture + `
func (b *B) Nested(ctx context.Context) (string, error) {
	var out reply
	err := b.get(ctx, "/nested", &out)
	return out.Lease, err
}
`
	findings := check(t, source)
	if len(findings) != 2 {
		t.Fatalf("findings = %v, want the direct call's and the nested helper's", findings)
	}
	if findings[1].Type != "fixture.reply" || findings[1].Call != "(*fixture/fixture.B).get" {
		t.Errorf("finding 1 = %+v, want fixture.reply passed to (*fixture/fixture.B).get", findings[1])
	}
}

// A value whose static type is an interface names no type the check could hold to the wire
// package, so it is a finding of its own.
func TestCheckFindsAnUntypedArgument(t *testing.T) {
	source := strings.Replace(helperFixture, `	var out wire.Row
	err = b.do(req, &out)`, `	var out wire.Row
	var v any = &out
	err = b.do(req, v)`, 1)
	findings := check(t, source)
	if len(findings) != 2 || findings[1].Type != "UNTYPED" {
		t.Fatalf("findings = %v, want the copied reply and then UNTYPED", findings)
	}
}

// answerDecode is the full name of the decode every client of this repository reads an answer's
// body with (redact.Answer reads it; the answer census of cmd/redact-gate holds them to it).
const answerDecode = "(github.com/sjawhar/envoy/internal/redact.Answer).Decode"

const answerDecodeFixture = `package fixture

import (
	"context"
	"net/http"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/oauth2call"

	"github.com/sjawhar/envoy/wirecheckfixture/wire"
)

type row struct {
	SessionID string ` + "`json:\"session_id\"`" + `
}

func get(ctx context.Context, url string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(request)
}

func Row(ctx context.Context, url string) (row, error) {
	response, err := get(ctx, url)
	if err != nil {
		return row{}, err
	}
	defer response.Body.Close()
	answer, err := redact.ReadAnswer(response, 1<<20)
	if err != nil {
		return row{}, err
	}
	var copied row
	err = answer.Decode(&copied)
	return copied, err
}

func WireRow(ctx context.Context, url string) (wire.Row, error) {
	response, err := get(ctx, url)
	if err != nil {
		return wire.Row{}, err
	}
	defer response.Body.Close()
	answer, err := redact.ReadAnswer(response, 1<<20)
	if err != nil {
		return wire.Row{}, err
	}
	var declared wire.Row
	err = answer.Decode(&declared)
	return declared, err
}

// label is the client's own type for the row's id, as Dispatch's client holds a holder's state
// as its own holderState: the wire package's generic row declares the body, and its type argument
// only how the client holds a word of it.
type label string

func GenericRows(ctx context.Context, url string) (wire.RowAs[string], wire.RowAs[label], error) {
	response, err := get(ctx, url)
	if err != nil {
		return wire.RowAs[string]{}, wire.RowAs[label]{}, err
	}
	defer response.Body.Close()
	answer, err := redact.ReadAnswer(response, 1<<20)
	if err != nil {
		return wire.RowAs[string]{}, wire.RowAs[label]{}, err
	}
	var plain wire.RowAs[string]
	if err := answer.Decode(&plain); err != nil {
		return plain, wire.RowAs[label]{}, err
	}
	var labelled wire.RowAs[label]
	err = answer.Decode(&labelled)
	return plain, labelled, err
}

// AliasRow reads its answer as a module outside envoy's does, through oauth2call, whose Answer is
// redact's under another name.
func AliasRow(ctx context.Context, url string) (row, error) {
	response, err := get(ctx, url)
	if err != nil {
		return row{}, err
	}
	defer response.Body.Close()
	answer, err := oauth2call.ReadAnswer(response, 1<<20)
	if err != nil {
		return row{}, err
	}
	var copied row
	err = answer.Decode(&copied)
	return copied, err
}
`

// An answer read through redact.Answer, the one way a client of this repository reads one, is
// decoded by its Decode: a copy decoded there is a finding, through oauth2call's name for the
// answer as through redact's, and the wire package's own type is none, an instance of its generic
// one among them, whatever type the client gives it as an argument.
func TestCheckFindsACopyDecodedThroughAnAnswer(t *testing.T) {
	want(t, checkInEnvoy(t, answerDecodeFixture), answerDecode, "fixture.row", "fixture.row")
}

const answerReaderFixture = `package fixture

import (
	"context"
	"net/http"

	"github.com/sjawhar/envoy/internal/redact"

	"github.com/sjawhar/envoy/wirecheckfixture/wire"
)

type refusal struct {
	Error string ` + "`json:\"error\"`" + `
}

func Send(ctx context.Context, url string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, err := redact.ReadAnswer(response, 1<<20)
	if err != nil {
		return err
	}
	return readRefusal(answer)
}

// readRefusal decodes the answer a party call hands it.
func readRefusal(answer redact.Answer) error {
	var body refusal
	if err := answer.Decode(&body); err != nil {
		return err
	}
	var failure wire.Failure
	return answer.Decode(&failure)
}

// parseStored decodes an answer nothing of the other party's handed it.
func parseStored(stored redact.Answer) (refusal, error) {
	var kept refusal
	err := stored.Decode(&kept)
	return kept, err
}

func Load(stored redact.Answer) (refusal, error) {
	return parseStored(stored)
}
`

// A function that decodes an Answer a party call read and handed it is read as part of that call,
// as one decoding the answer's bytes is, so moving the decode into a helper does not hide it; the
// same decode reached from no party call is none of the check's business.
func TestCheckReadsAnAnswerAPartyCallHandsAHelper(t *testing.T) {
	want(t, checkInEnvoy(t, answerReaderFixture), answerDecode, "fixture.refusal")
}
