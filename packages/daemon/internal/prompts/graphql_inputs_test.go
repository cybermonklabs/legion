package prompts

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// githubGraphQLInputs is the checked-in copy of GitHub's schema that the mutation check reads: for
// each mutation a role prompt runs, the input type its `input` argument takes and that type's
// input fields, as GitHub's introspection answers them. The command that refreshes it is in the
// doc comment of TestEveryMutationARolePromptGivesWritesAnInputGitHubTakes.
const githubGraphQLInputs = "testdata/github-graphql-inputs.json"

// githubInputType is one mutation's input type in the copy: its name and its input fields, each
// with its type's kind and its default value. A field whose kind is NON_NULL and which has no
// default value is one GitHub requires.
type githubInputType struct {
	Name        string `json:"name"`
	InputFields []struct {
		Name         string  `json:"name"`
		DefaultValue *string `json:"defaultValue"`
		Type         struct {
			Kind string `json:"kind"`
		} `json:"type"`
	} `json:"inputFields"`
}

// readGitHubGraphQLInputs is the copy of GitHub's input types, by the mutation that takes each.
func readGitHubGraphQLInputs(t *testing.T) map[string]githubInputType {
	t.Helper()
	body, err := os.ReadFile(githubGraphQLInputs)
	if err != nil {
		t.Fatalf("read %s: %v", githubGraphQLInputs, err)
	}
	var inputs map[string]githubInputType
	if err := json.Unmarshal(body, &inputs); err != nil {
		t.Fatalf("decode %s: %v", githubGraphQLInputs, err)
	}
	return inputs
}

// Every GraphQL mutation a role prompt gives writes an input that GitHub's schema takes: each
// field of its inline `input` object is a field its input type holds, and it gives each field the
// type requires. GitHub refuses any other input whole, so one wrong name leaves the role unable to
// act.
//
// The check reads every part the package embeds, roles/ and go/. It reads a mutation wherever a
// part writes one, so a command in a form that no pattern of gh's flags matches is read too. The
// skills are not read: they are under the repository's root, outside this module, and a read of
// them would put skills/ on the daemon's Go job, which a skill edit then runs whole.
//
// The copy is testdata/github-graphql-inputs.json. Refresh it with the command below, run from
// this package's directory. Introspection reads the schema and writes nothing. A prompt that runs
// a new mutation adds the mutation's name to the IN(...) list first. GitHub's answer leaves out
// deprecated mutations and deprecated input fields, so a prompt that uses one fails here too.
//
//	gh api graphql \
//	  -f query='{__type(name:"Mutation"){fields{name args{name type{ofType{name inputFields{name defaultValue type{kind name ofType{kind name}}}}}}}}}' \
//	  --jq '[.data.__type.fields[] | select(.name | IN("dequeuePullRequest", "enqueuePullRequest", "resolveReviewThread"))
//	    | {key: .name, value: (.args[] | select(.name == "input") | .type.ofType)}] | from_entries' |
//	  jq . > testdata/github-graphql-inputs.json
func TestEveryMutationARolePromptGivesWritesAnInputGitHubTakes(t *testing.T) {
	inputs := readGitHubGraphQLInputs(t)
	read := 0
	for dir, parts := range map[string]embed.FS{"roles": roleParts, "go": goParts} {
		err := eachPart(parts, dir, func(name string, body []byte) error {
			mutations, problems := mutationInputProblems(dir+"/"+name, string(body), inputs)
			read += mutations
			for _, problem := range problems {
				t.Error(problem)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the %s parts: %v", dir, err)
		}
	}
	// The merger's enqueue and dequeue and the implementer's thread resolution are mutations of
	// these parts, so a read that finds none is a broken read, not a clean one.
	if read == 0 {
		t.Fatal("read no mutation in the role prompts")
	}
}

// The check names what its reader must change: the part and its line, the mutation, and the
// field with the fields its type holds.
func TestTheMutationCheckNamesThePartTheMutationAndTheFieldsItsTypeHolds(t *testing.T) {
	inputs := readGitHubGraphQLInputs(t)
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{"an input field the type does not hold, in place of one it requires",
			"```bash\ngh api graphql -f query='mutation($pr:ID!){dequeuePullRequest(input:{pullRequestId:$pr}){mergeQueueEntry{position}}}' \\\n```",
			[]string{
				"roles/merger.md:2: mutation dequeuePullRequest: DequeuePullRequestInput holds no input field pullRequestId; it holds clientMutationId, id",
				"roles/merger.md:2: mutation dequeuePullRequest: DequeuePullRequestInput requires id, which the input leaves out",
			}},
		{"the corrected dequeue",
			"gh api graphql -f query='mutation($pr:ID!){dequeuePullRequest(input:{id:$pr}){mergeQueueEntry{position}}}'",
			nil},
		{"a mutation the copy holds no input type for",
			"gh api graphql -f query='mutation($pr:ID!){closePullRequest(input:{pullRequestId:$pr}){pullRequest{state}}}'",
			[]string{"roles/merger.md:1: mutation closePullRequest: testdata/github-graphql-inputs.json holds no input type for it; " +
				"add closePullRequest to the IN(...) list of the refresh command in graphql_inputs_test.go and run it"}},
		{"an input the check cannot read",
			"gh api graphql -f query='mutation($in:DequeuePullRequestInput!){dequeuePullRequest(input:$in){clientMutationId}}'",
			[]string{"roles/merger.md:1: mutation dequeuePullRequest passes `input` as a variable, so the check cannot read its fields; " +
				"write the DequeuePullRequestInput object inline"}},
		{"a mutation that gives no input",
			"gh api graphql -f query='mutation($pr:ID!){dequeuePullRequest(id:$pr){clientMutationId}}'",
			[]string{"roles/merger.md:1: mutation dequeuePullRequest gives no `input` argument; write its fields in an inline DequeuePullRequestInput object"}},
		{"a document that does not close",
			"gh api graphql -f query='mutation($pr:ID!){dequeuePullRequest(input:{id:$pr}){mergeQueueEntry{position}'",
			[]string{"roles/merger.md:1: a mutation the check cannot read: the document ends before its { closes"}},
		{"the word in prose",
			"Every code or repository mutation is a phase worker's. Use jj, never git mutations.",
			nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, problems := mutationInputProblems("roles/merger.md", tc.text, inputs); !slices.Equal(problems, tc.want) {
				t.Errorf("problems = %q, want %q", problems, tc.want)
			}
		})
	}
}

// graphQLMutation is where a GraphQL mutation document starts in a part's text: the keyword, an
// optional operation name, then the document's variables or its selection set. The word in prose
// ("every repository mutation is …") is followed by neither, so prose is not read as a document.
var graphQLMutation = regexp.MustCompile(`\bmutation(\s+[_A-Za-z][_0-9A-Za-z]*)?\s*[({]`)

// mutationInputProblems reads every mutation document in text, the part named part. It returns how
// many mutations they run, and every way their inputs break the copy of GitHub's schema: a
// mutation the copy holds no input type for, an input that is not an inline object, an input field
// the type does not hold, a field the type requires that the input leaves out, and a document that
// does not parse. Each problem names the part and the line.
func mutationInputProblems(part, text string, inputs map[string]githubInputType) (int, []string) {
	read := 0
	var problems []string
	for _, match := range graphQLMutation.FindAllStringIndex(text, -1) {
		fields, err := parseMutation(text, match[0])
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s:%d: a mutation the check cannot read: %v", part, lineOf(text, match[0]), err))
			continue
		}
		for _, field := range fields {
			read++
			where := fmt.Sprintf("%s:%d: mutation %s", part, lineOf(text, field.at), field.name)
			problems = append(problems, field.problems(where, inputs)...)
		}
	}
	return read, problems
}

// lineOf is the line of text, counted from 1, that the offset at stands on.
func lineOf(text string, at int) int {
	return strings.Count(text[:at], "\n") + 1
}

// mutationField is one mutation a document runs: its name, the offset of that name in the part's
// text, and the value of its `input` argument.
type mutationField struct {
	name  string
	at    int
	input graphQLValue
}

// graphQLValue is an argument's value as the check reads it: its kind (`object`, `variable`,
// `list` or `scalar`, and empty where the argument is not given), and an object's field names.
type graphQLValue struct {
	kind   string
	fields []string
}

// problems is every way the field's input breaks the input type the copy holds for it, each one
// starting with where.
func (field mutationField) problems(where string, inputs map[string]githubInputType) []string {
	inputType, known := inputs[field.name]
	if !known {
		return []string{fmt.Sprintf("%s: %s holds no input type for it; add %s to the IN(...) list of the refresh command in graphql_inputs_test.go and run it",
			where, githubGraphQLInputs, field.name)}
	}
	switch field.input.kind {
	case "object":
	case "":
		return []string{fmt.Sprintf("%s gives no `input` argument; write its fields in an inline %s object", where, inputType.Name)}
	default:
		return []string{fmt.Sprintf("%s passes `input` as a %s, so the check cannot read its fields; write the %s object inline",
			where, field.input.kind, inputType.Name)}
	}
	holds := make([]string, 0, len(inputType.InputFields))
	for _, inputField := range inputType.InputFields {
		holds = append(holds, inputField.Name)
	}
	var problems []string
	for _, name := range field.input.fields {
		if !slices.Contains(holds, name) {
			problems = append(problems, fmt.Sprintf("%s: %s holds no input field %s; it holds %s", where, inputType.Name, name, strings.Join(holds, ", ")))
		}
	}
	for _, inputField := range inputType.InputFields {
		if inputField.Type.Kind == "NON_NULL" && inputField.DefaultValue == nil && !slices.Contains(field.input.fields, inputField.Name) {
			problems = append(problems, fmt.Sprintf("%s: %s requires %s, which the input leaves out", where, inputType.Name, inputField.Name))
		}
	}
	return problems
}

// parseMutation reads the mutation document that starts at the offset at of text, and returns
// each mutation its selection set runs. It reads only what the check needs: each mutation's name
// and arguments. It skips the variables and each mutation's own selection set by their brackets.
func parseMutation(text string, at int) ([]mutationField, error) {
	s := &graphQLScanner{text: text, pos: at}
	s.next() // the keyword `mutation`, which the match found
	token := s.next()
	if isGraphQLName(token) {
		token = s.next() // the operation's name
	}
	if token == "(" {
		if err := s.skip("(", ")"); err != nil {
			return nil, err
		}
		token = s.next()
	}
	if token != "{" {
		return nil, fmt.Errorf("%q stands where the selection set belongs", token)
	}
	var fields []mutationField
	for {
		name := s.next()
		if name == "}" {
			break
		}
		if !isGraphQLName(name) {
			return nil, fmt.Errorf("%q stands where a mutation's name belongs", name)
		}
		field := mutationField{name: name, at: s.start}
		if s.peek() == ":" { // an alias, then the mutation's own name
			s.next()
			if field.name = s.next(); !isGraphQLName(field.name) {
				return nil, fmt.Errorf("the alias %s names no mutation", name)
			}
		}
		if s.peek() == "(" {
			s.next()
			for {
				argument := s.next()
				if argument == ")" {
					break
				}
				if !isGraphQLName(argument) || s.next() != ":" {
					return nil, fmt.Errorf("an argument of %s is not `name: value`", field.name)
				}
				value, err := s.value()
				if err != nil {
					return nil, fmt.Errorf("the argument %s of %s: %w", argument, field.name, err)
				}
				if argument == "input" {
					field.input = value
				}
			}
		}
		if s.peek() == "{" {
			s.next()
			if err := s.skip("{", "}"); err != nil {
				return nil, err
			}
		}
		fields = append(fields, field)
	}
	if len(fields) == 0 {
		return nil, errors.New("the selection set runs no mutation")
	}
	return fields, nil
}

// graphQLScanner reads the tokens of one GraphQL document in a part's text. A single quote or a
// backtick ends the document: neither can stand in GraphQL outside a string, and they are the
// shell quoting and the Markdown code span that a part writes a document in.
type graphQLScanner struct {
	text  string
	pos   int // where the read of the next token starts
	start int // where the last token read starts
}

// next returns the next token, or "" at the end of the document: a name, a number, a string,
// `...`, or one punctuator. It skips whitespace, commas and comments, which GraphQL ignores.
func (s *graphQLScanner) next() string {
	for s.pos < len(s.text) {
		c := s.text[s.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			s.pos++
			continue
		case c == '#':
			for s.pos < len(s.text) && s.text[s.pos] != '\n' {
				s.pos++
			}
			continue
		case c == '\'' || c == '`':
			return ""
		}
		s.start = s.pos
		switch {
		case c == '-' || (c >= '0' && c <= '9'):
			s.pos++
			for s.pos < len(s.text) && strings.IndexByte("0123456789.eE+-", s.text[s.pos]) >= 0 {
				s.pos++
			}
		case isGraphQLNameByte(c):
			for s.pos < len(s.text) && isGraphQLNameByte(s.text[s.pos]) {
				s.pos++
			}
		case c == '"':
			s.pos++
			for s.pos < len(s.text) && s.text[s.pos] != '"' {
				if s.text[s.pos] == '\\' {
					s.pos++
				}
				s.pos++
			}
			s.pos = min(s.pos+1, len(s.text))
		case strings.HasPrefix(s.text[s.pos:], "..."):
			s.pos += 3
		default:
			s.pos++
		}
		return s.text[s.start:s.pos]
	}
	return ""
}

// peek returns the next token without reading past it.
func (s *graphQLScanner) peek() string {
	pos, start := s.pos, s.start
	token := s.next()
	s.pos, s.start = pos, start
	return token
}

// skip reads past the rest of a bracketed group whose opening token was just read, up to the
// closing token that balances it.
func (s *graphQLScanner) skip(open, close string) error {
	for depth := 1; depth > 0; {
		switch s.next() {
		case "":
			return fmt.Errorf("the document ends before its %s closes", open)
		case open:
			depth++
		case close:
			depth--
		}
	}
	return nil
}

// value reads one argument value: a variable, an object with its field names, a list, or a
// scalar (a name, a number or a string).
func (s *graphQLScanner) value() (graphQLValue, error) {
	switch token := s.next(); {
	case token == "$":
		if !isGraphQLName(s.next()) {
			return graphQLValue{}, errors.New("a `$` names no variable")
		}
		return graphQLValue{kind: "variable"}, nil
	case token == "{":
		object := graphQLValue{kind: "object"}
		for {
			name := s.next()
			if name == "}" {
				return object, nil
			}
			if !isGraphQLName(name) || s.next() != ":" {
				return graphQLValue{}, fmt.Errorf("the object field %q is not `name: value`", name)
			}
			if _, err := s.value(); err != nil {
				return graphQLValue{}, err
			}
			object.fields = append(object.fields, name)
		}
	case token == "[":
		for s.peek() != "]" {
			if _, err := s.value(); err != nil {
				return graphQLValue{}, err
			}
		}
		s.next()
		return graphQLValue{kind: "list"}, nil
	case isGraphQLName(token) || strings.HasPrefix(token, `"`) || strings.HasPrefix(token, "-") ||
		(token != "" && token[0] >= '0' && token[0] <= '9'):
		return graphQLValue{kind: "scalar"}, nil
	default:
		return graphQLValue{}, fmt.Errorf("%q stands where a value belongs", token)
	}
}

// isGraphQLName says whether a token is a GraphQL name: it starts with a letter or `_`.
func isGraphQLName(token string) bool {
	return token != "" && isGraphQLNameByte(token[0]) && (token[0] < '0' || token[0] > '9')
}

// isGraphQLNameByte says whether c can stand in a GraphQL name: a letter, a digit or `_`.
func isGraphQLNameByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
