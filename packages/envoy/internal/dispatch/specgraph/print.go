package specgraph

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// ErrUnprintable is Print's refusal of a node that no block can hold: a block of it would read back
// as another node, or as no node at all. The error that wraps it names the node and the field.
var ErrUnprintable = errors.New("unprintable")

// valueReferences writes the two characters of an attribute value that would otherwise end it or
// decode as a character reference, as pmdoc.ParseTypedDirectiveOpen reads a value back.
var valueReferences = strings.NewReplacer("&", "&amp;", `"`, "&quot;")

// Print writes the statement as its requirement block, ending in "\n", which Parse reads back as
// the statement.
func (s Statement) Print() (string, error) {
	p := newPrinter("requirement", s.ID)
	p.attr("kind", string(s.Kind))
	p.attr("parent", s.Parent)
	p.list("serves", s.Serves)
	p.list("about", s.About)
	p.text("text", s.Text)
	return p.done()
}

// Print writes the check as its check block, ending in "\n", which Parse reads back as the check.
func (c Check) Print() (string, error) {
	p := newPrinter("check", c.ID)
	p.attr("serves", c.Serves)
	p.attr("subject", string(c.Subject))
	p.attr("procedure", string(c.Procedure))
	if c.Test != nil {
		if *c.Test == (Test{}) {
			p.refuse("test", "a test part names a runner, a selector or a job")
		}
		p.attr("runner", c.Test.Runner)
		p.attr("selector", c.Test.Selector)
		p.attr("job", c.Test.Job)
	}
	switch {
	case c.TimeLimit > 0:
		p.attr("time_limit", c.TimeLimit.String())
	case c.TimeLimit < 0:
		p.refuse("time limit", "a time limit is a positive duration")
	}
	p.field("claim", checkFields[0], c.Claim, true)
	p.field("procedure", checkFields[1], c.ProcedureText, false)
	p.field("failing example", checkFields[2], c.FailingExample, false)
	return p.done()
}

// Print writes the component as its component block, ending in "\n", which Parse reads back as the
// component.
func (c Component) Print() (string, error) {
	p := newPrinter("component", c.ID)
	p.attr("name", c.Name)
	if c.Interface {
		p.attr("kind", "interface")
	}
	p.attr("parent", c.Parent)
	p.list("joins", c.Joins)
	p.text("purpose", c.Purpose)
	return p.done()
}

// Print writes the term as its term block, ending in "\n", which Parse reads back as the term.
func (t Term) Print() (string, error) {
	p := newPrinter("term", t.ID)
	p.attr("name", t.Name)
	p.text("definition", t.Definition)
	return p.done()
}

// printer writes one block: its opening line, then its body, and the first refusal of a field.
type printer struct {
	block, id string
	open      strings.Builder
	body      strings.Builder
	err       error
}

func newPrinter(block, id string) *printer {
	p := &printer{block: block, id: id}
	p.open.WriteString(":::" + block + "{")
	if id == "" {
		p.refuse("id", "a block has an id")
	}
	p.open.WriteString(pmdoc.TypedBlockIDAttribute(id))
	return p
}

// refuse records that field cannot be printed, for the reason given, when no earlier field was.
func (p *printer) refuse(field, reason string) {
	if p.err == nil {
		p.err = fmt.Errorf("%w: %s %q, %s: %s", ErrUnprintable, p.block, p.id, field, reason)
	}
}

// attr writes one attribute, leaving out an empty value.
func (p *printer) attr(name, value string) {
	if value == "" {
		return
	}
	if strings.ContainsAny(value, "\n\r") {
		p.refuse(name, "an attribute value is one line")
	}
	p.open.WriteString(" " + name + `="` + valueReferences.Replace(value) + `"`)
}

// list writes one list attribute, its ids sorted and separated by one space.
func (p *printer) list(name string, ids []string) {
	p.attr(name, recordList(ids))
}

// text writes a body that is the block's text.
func (p *printer) text(field, value string) {
	lines, ok := p.lines(field, value)
	if !ok {
		return
	}
	for _, line := range lines {
		if strings.HasPrefix(line, ":::") {
			p.refuse(field, "a line starts with :::")
		}
		p.body.WriteString(line + "\n")
	}
}

// field writes one field of a check's body, leaving out an empty one. The first line follows the
// field's marker; each later line continues the field, so it cannot start with ::: or with a
// field's marker.
func (p *printer) field(field, marker, value string, oneLine bool) {
	lines, ok := p.lines(field, value)
	if !ok || len(lines) == 0 {
		return
	}
	if oneLine && len(lines) > 1 {
		p.refuse(field, "it is one line")
	}
	p.body.WriteString(marker + " " + lines[0] + "\n")
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, ":::") {
			p.refuse(field, "a line starts with :::")
		}
		if _, _, starts := fieldStart(line); starts {
			p.refuse(field, "a line starts with a field's marker")
		}
		p.body.WriteString(line + "\n")
	}
}

// lines is value's lines, refusing what Parse would read back otherwise: a carriage return, a line
// that ends in white space, and a leading or trailing blank line. ok is false when value is
// refused.
func (p *printer) lines(field, value string) ([]string, bool) {
	if value == "" {
		return nil, true
	}
	if strings.Contains(value, "\r") {
		p.refuse(field, "a text holds no carriage return")
		return nil, false
	}
	lines := strings.Split(value, "\n")
	if lines[0] == "" || lines[len(lines)-1] == "" {
		p.refuse(field, "a text starts and ends with a line that is not blank")
		return nil, false
	}
	for _, line := range lines {
		if strings.TrimRightFunc(line, unicode.IsSpace) != line {
			p.refuse(field, "a line ends in white space")
			return nil, false
		}
	}
	return lines, true
}

// done is the block, or the first refusal.
func (p *printer) done() (string, error) {
	if p.err != nil {
		return "", p.err
	}
	return p.open.String() + "}\n" + p.body.String() + ":::\n", nil
}
