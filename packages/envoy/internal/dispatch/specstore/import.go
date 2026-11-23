package specstore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/specchange"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// An approved level of The Spec is imported, not left out, and nothing the import cannot resolve
// is dropped (D9). A level is two documents of its issue: the level document, whose statements are
// requirement blocks with ids and no attributes, under headings - a statement under a heading
// `### Under <id>` stands under that statement - and whose `## Components` table names each
// component and interface; and the notes document, which gives each statement's kind, about and
// supports under a heading of its id. The import writes each statement whose fields resolve, and
// each component, as an approved node, with the person's approval of the level as provenance. A
// statement that does not resolve - a kind no approved form names, an about or a supports naming
// nothing, a field its notes write where the reader cannot place it, a parent nothing holds or that
// is itself held - is kept as a node of kind spec_import_hold, with its text, its provenance and the
// fields it lacks, until a change request that supplies them is approved (Apply clears the hold of
// each node it writes). A row of the Components table that reads as no component refuses the
// level (ParseLevel), so no component is left out either.

// The values of a hold: what it keeps, and the issue opened for its repair.
const (
	AttributeHold        = "hold"
	AttributeRepairIssue = "repair_issue"
)

// levelKinds maps a kind of the notes to the kind of a statement (R3): the notes of level one
// write two kinds of structure.
var levelKinds = map[string]specgraph.Kind{
	"behavior":                 specgraph.KindBehavior,
	"rule of structure":        specgraph.KindStructure,
	"description of structure": specgraph.KindStructure,
	"property":                 specgraph.KindProperty,
}

// LevelStatement is one statement of a level as its documents give it: its id, its text, its
// parent from the level's structure, the notes' kind, about and supports, as written, and each
// field its notes write where the reader cannot place it (Unread), which the import holds the
// statement for.
type LevelStatement struct {
	ID, Text, Parent string
	Kind             string
	About, Supports  []string
	Noted            bool
	Unread           []string
}

// Level is a level read from its two documents: its statements in their order, and its
// components and interfaces.
type Level struct {
	Statements []LevelStatement
	Components []specgraph.Component
}

// ParseLevel reads a level from its level document and its notes document, each through the
// document parser every write goes through (pmdoc.ParseRendering), as a change request's versions
// are read (specchange.FromVersions). A statement is each requirement block, wherever the level
// holds it, with the block's own text (specchange.BlockText). A heading of the document's top
// starts the part the blocks after it stand in: one of level two ends the part before it, and a
// heading `## Components` starts the part whose tables, wherever the part holds them, name each
// component and interface, one row each, read from the cells the parser gives the row
// (componentsOf); one of level three `Under <id>` sets the parent of the statements after it, and
// one of level three of any other title sets none. So a directive, a heading, a table row or a note
// that a code example shows is none of them. A row of a Components table that reads as no
// component is never left out: the level is refused (*Refusal, RefusalNodeInvalid) with a problem
// for each such row, as Import refuses a level whose nodes the approved spec could not hold, and
// nothing is written.
func ParseLevel(level, notes string) (Level, error) {
	doc, err := pmdoc.ParseRendering(level)
	if err != nil {
		return Level{}, fmt.Errorf("read the level: %w", err)
	}
	var parsed Level
	var unreadable []specgraph.Problem
	parent, components := "", false
	for _, block := range doc.Children {
		if block.Type == "heading" {
			title := strings.TrimSpace(pmdoc.TextContent(block))
			switch headingLevel(block) {
			case 2:
				parent, components = "", title == "Components"
			case 3:
				parent = ""
				if under, ok := strings.CutPrefix(title, "Under "); ok {
					parent = strings.TrimSpace(under)
				}
			}
			continue
		}
		var failed error
		pmdoc.Walk(block, func(node *pmdoc.Node) bool {
			switch {
			case components && node.Type == "table":
				read, problems, err := componentsOf(node)
				if err != nil {
					failed = fmt.Errorf("read the components table of the level: %w", err)
					return false
				}
				parsed.Components = append(parsed.Components, read...)
				unreadable = append(unreadable, problems...)
			case node.Type == "requirement":
				id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
				text, err := statementText(node)
				if err != nil {
					failed = fmt.Errorf("read the requirement %s of the level: %w", id, err)
					return false
				}
				parsed.Statements = append(parsed.Statements, LevelStatement{ID: id, Text: text, Parent: parent})
			}
			return true
		})
		if failed != nil {
			return Level{}, failed
		}
	}
	if len(unreadable) > 0 {
		return Level{}, &Refusal{Code: RefusalNodeInvalid, Detail: problemDetail(unreadable), Problems: unreadable}
	}
	noted, err := notesOf(notes)
	if err != nil {
		return Level{}, err
	}
	for index := range parsed.Statements {
		statement := &parsed.Statements[index]
		if note, held := noted[statement.ID]; held {
			statement.Noted, statement.Kind, statement.About, statement.Supports, statement.Unread = true, note.Kind, note.About, note.Supports, note.Unread
		}
	}
	return parsed, nil
}

// headingLevel is the level of a heading, 1 to 6.
func headingLevel(node *pmdoc.Node) int {
	switch level := node.Attrs["level"].(type) {
	case int:
		return level
	case int64:
		return int(level)
	case float64:
		return int(level)
	}
	return 1
}

// statementText is the text of a requirement block as a change request's reader reads a
// statement's: the lines of the block's own markdown (specchange.BlockText) between its opening
// line and its closing one, each without its trailing white space, and without the blank lines
// at either end.
func statementText(block *pmdoc.Node) (string, error) {
	text, err := specchange.BlockText(block)
	if err != nil {
		return "", err
	}
	_, body, _ := strings.Cut(text, "\n")
	end := strings.LastIndexByte(body, '\n')
	if end < 0 {
		return "", nil
	}
	lines := strings.Split(body[:end], "\n")
	for index := range lines {
		lines[index] = strings.TrimRight(lines[index], " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), nil
}

// componentsOf is each component and interface the body rows of a Components table name, read from
// the cells the document parser gives each row, so each cell keeps every character it holds and an
// escaped pipe stays in its cell: a row of five cells - its id, `component` or `interface`, its name,
// its purpose, and its parent or the components it joins. The id, the kind and the last cell are
// read as the cells' text; the name and the purpose as each cell's own markdown, as a document
// holding the cell's paragraph alone renders it, so a code span keeps its backticks and a pipe
// reads as a pipe. A row that reads as no component - of another number of cells, with an empty id,
// or of another kind - is a problem (bad-component) naming the row, never left out.
func componentsOf(table *pmdoc.Node) ([]specgraph.Component, []specgraph.Problem, error) {
	var components []specgraph.Component
	var problems []specgraph.Problem
	for _, row := range table.Children {
		if row.Type != "table_row" {
			continue
		}
		text := make([]string, len(row.Children))
		for index, cell := range row.Children {
			text[index] = strings.TrimSpace(pmdoc.TextContent(cell))
		}
		var why string
		switch {
		case len(text) != 5:
			why = fmt.Sprintf("holds %d cells, want 5: its id, component or interface, its name, its purpose, and its parent or the components it joins", len(text))
		case text[0] == "":
			why = "names no id"
		case text[1] != "component" && text[1] != "interface":
			why = fmt.Sprintf("is of the kind %q, want component or interface", text[1])
		}
		if why != "" {
			node := "-"
			if len(text) > 0 && text[0] != "" {
				node = text[0]
			}
			problems = append(problems, specgraph.Problem{Pos: specgraph.Position{File: "Components", Line: len(components) + len(problems) + 1},
				Node: node, Rule: "bad-component", Effect: specgraph.LosesNode,
				Detail: fmt.Sprintf("bad-component: the Components row %q reads as no component: it %s", strings.Join(text, " | "), why)})
			continue
		}
		name, err := cellMarkdown(row.Children[2])
		if err != nil {
			return nil, nil, err
		}
		purpose, err := cellMarkdown(row.Children[3])
		if err != nil {
			return nil, nil, err
		}
		component := specgraph.Component{ID: text[0], Interface: text[1] == "interface", Name: name, Purpose: purpose}
		if related := text[4]; related != "none" && related != "" {
			if component.Interface {
				// specgraph.Component holds the components an interface joins sorted and unique, as
				// a change request's parse writes them, so an approved interface compares equal to
				// the same interface a later request names as context.
				component.Joins = sortedUnique(splitIDs(related))
			} else {
				component.Parent = related
			}
		}
		components = append(components, component)
	}
	return components, problems, nil
}

// cellMarkdown is a table cell's content as a document holding its paragraph alone renders it.
func cellMarkdown(cell *pmdoc.Node) (string, error) {
	markdown, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: cell.Children})
	return strings.TrimSpace(markdown), err
}

// noteFields is each field a note writes, by the name its item gives it.
var noteFields = map[string]string{"Kind": "kind", "About": "about", "Supports": "supports"}

// notesOf reads, from a notes document parsed as the level is, the kind, about and supports of
// each statement under a heading `### <id>` of the document's top, from the blocks after it up to
// the next such heading: the items `Kind: …`, `About: …` and `Supports: …` of its lists, at every
// depth, each from the item's first paragraph, read whole. Two headings of one id are one note. A
// field the note writes where no such item holds it - a paragraph of its own, an item's later
// paragraph, an item in a quote or in a table - or writes twice is unread (LevelStatement.Unread),
// with no value, and the import holds the statement for it, so no field is left out of a statement
// the import writes. A code block is an example, and none of its lines is a field.
func notesOf(notes string) (map[string]LevelStatement, error) {
	doc, err := pmdoc.ParseRendering(notes)
	if err != nil {
		return nil, fmt.Errorf("read the notes of the level: %w", err)
	}
	readings := map[string]*noteReading{}
	var current *noteReading
	for _, block := range doc.Children {
		if block.Type == "heading" {
			if headingLevel(block) == 3 {
				id := strings.TrimSpace(pmdoc.TextContent(block))
				if readings[id] == nil {
					readings[id] = &noteReading{statement: LevelStatement{ID: id}, read: map[string]bool{}}
				}
				current = readings[id]
			}
			continue
		}
		if current != nil {
			current.visit(block, isList(block))
		}
	}
	noted := make(map[string]LevelStatement, len(readings))
	for id, reading := range readings {
		noted[id] = reading.statement
	}
	return noted, nil
}

// noteReading is one note as notesOf reads it: the statement's fields so far, and each field read
// from an item.
type noteReading struct {
	statement LevelStatement
	read      map[string]bool
}

// visit reads the fields node holds: listed is whether node is a list, or an item of one, that only
// lists and their items hold, from the top of the document down.
func (n *noteReading) visit(node *pmdoc.Node, listed bool) {
	switch node.Type {
	case "code_block":
		return
	case "paragraph":
		n.field(node, false)
		return
	case "list_item":
		if listed {
			for index, child := range node.Children {
				if index == 0 && child.Type == "paragraph" {
					n.field(child, true)
					continue
				}
				n.visit(child, isList(child))
			}
			return
		}
	}
	listed = listed && isList(node)
	for _, child := range node.Children {
		n.visit(child, listed)
	}
}

// field reads paragraph as a field of the note where it writes one: from a list item's first
// paragraph (placed) the first time, else as unread, with no value.
func (n *noteReading) field(paragraph *pmdoc.Node, placed bool) {
	name, value, written := strings.Cut(strings.ReplaceAll(pmdoc.TextContent(paragraph), "\n", " "), ":")
	field, known := noteFields[name]
	switch {
	case !written || !known || slices.Contains(n.statement.Unread, field):
		return
	case !placed || n.read[field]:
		n.statement.Unread = append(n.statement.Unread, field)
		n.set(field, "")
		return
	}
	n.read[field] = true
	n.set(field, value)
}

// set gives the statement's field the value its note writes.
func (n *noteReading) set(field, value string) {
	switch field {
	case "kind":
		n.statement.Kind = strings.TrimSpace(value)
	case "about":
		n.statement.About = splitIDs(value)
	case "supports":
		n.statement.Supports = splitIDs(value)
	}
}

// isList is whether node is a list.
func isList(node *pmdoc.Node) bool {
	return node.Type == "bullet_list" || node.Type == "ordered_list"
}

// splitIDs is the ids a list names, separated by commas or spaces; "none" names none.
func splitIDs(list string) []string {
	var ids []string
	for _, id := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if id = strings.Trim(id, "`."); id != "" && id != "none" {
			ids = append(ids, id)
		}
	}
	return ids
}

// Hold is one statement an import could not resolve, as its node keeps it: its id, its text, the
// fields it lacks, the fields it has, the provenance of the approval of its level, and the issue
// opened for its repair, "" until one is.
type Hold struct {
	ID          string            `json:"id"`
	Text        string            `json:"text"`
	Lacks       []string          `json:"lacks"`
	Fields      map[string]string `json:"fields,omitempty"`
	Provenance  Provenance        `json:"provenance"`
	RepairIssue string            `json:"-"`
}

// Holds is each open hold of the repository repo of project, by id, sorted: a hold node that is
// live, with the hold it keeps and the repair issue named on it.
func Holds(ctx context.Context, q graphstore.Queryer, project, repo string) ([]Hold, error) {
	prefix := Address(project, repo, "")
	rows, err := q.Query(ctx, `
		select n.id,
		       (select t.to_id from graph_tuples t where t.from_kind = n.kind and t.from_id = n.id and t.kind = $3
		          and t.to_kind = $5 and t.asserted order by t.seq desc limit 1),
		       coalesce((select t.to_id from graph_tuples t where t.from_kind = n.kind and t.from_id = n.id and t.kind = $4
		          and t.to_kind = $5 and t.asserted order by t.seq desc limit 1), '')
		from graph_nodes n
		where n.kind = $1 and n.supertag = $1 and starts_with(n.id, $2) and n.state = 'live'
		order by n.id
	`, KindHold, prefix, AttributeHold, AttributeRepairIssue, graphstore.KindValue)
	if err != nil {
		return nil, fmt.Errorf("read the holds of %s: %w", prefix, err)
	}
	defer rows.Close()
	var holds []Hold
	for rows.Next() {
		var address string
		var value *string
		var repair string
		if err := rows.Scan(&address, &value, &repair); err != nil {
			return nil, fmt.Errorf("read the holds of %s: %w", prefix, err)
		}
		var hold Hold
		if value != nil {
			if err := json.Unmarshal([]byte(*value), &hold); err != nil {
				return nil, fmt.Errorf("read the hold %s: %w", address, err)
			}
		}
		hold.ID, hold.RepairIssue = address[len(prefix):], repair
		holds = append(holds, hold)
	}
	return holds, rows.Err()
}

// openHolds is each open hold of the repository whose node written writes.
func openHolds(ctx context.Context, q graphstore.Queryer, project, repo string, written []Written) ([]Hold, error) {
	if len(written) == 0 {
		return nil, nil
	}
	holds, err := Holds(ctx, q, project, repo)
	if err != nil {
		return nil, err
	}
	var cleared []Hold
	for _, hold := range holds {
		if slices.ContainsFunc(written, func(entry Written) bool { return entry.ID == hold.ID && !entry.Removed }) {
			cleared = append(cleared, hold)
		}
	}
	return cleared, nil
}

// clearedHold is the node of the hold of the repository repo of project stated gone: the approval
// that wrote its statement repaired it.
func clearedHold(hold Hold, project, repo string, who model.Actor, at time.Time) graphstore.Node {
	node := specNode(KindHold, Address(project, repo, hold.ID), project, who, at)
	node.State, node.Restates = graphstore.StateGone, false
	return node
}

// marshalValue is a value's JSON text, as a value tuple holds it.
func marshalValue(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode a fact of the approved spec: %w", err)
	}
	return string(data), nil
}

// ImportInput is one import of an approved level into the repository Repo of Project: the
// level, its issue, the provenance of the person's approval of it, and who runs the import.
type ImportInput struct {
	Project, Repo, Issue string
	Level                Level
	Provenance           Provenance
	Who                  model.Actor
}

// Imported is what one import wrote: each node, each hold it kept and the time of the act.
type Imported struct {
	At      time.Time
	Written []Written
	Held    []Hold
}

// Import writes an approved level into the approved spec of the repository, in the caller's
// transaction and under its lock: each component and interface, and each statement whose fields
// resolve, as a node's first version, with in's provenance; and each statement that does not as a
// hold. A node the approved spec already holds or held is left as it stands, and a statement with
// an open or a cleared hold is not held again, so an import run again writes only what the one
// before could not; a statement it writes whose hold is open clears that hold. A level whose nodes
// the approved spec could not hold - a loop, a node of no kind - is refused (*Refusal), writing
// nothing.
func Import(ctx context.Context, tx pgx.Tx, in ImportInput) (Imported, error) {
	if err := Lock(ctx, tx, in.Project, in.Repo); err != nil {
		return Imported{}, err
	}
	state, err := Read(ctx, tx, in.Project, in.Repo)
	if err != nil {
		return Imported{}, err
	}
	known, err := holdNodes(ctx, tx, in.Project, in.Repo)
	if err != nil {
		return Imported{}, err
	}
	components := map[string]bool{}
	for _, component := range in.Level.Components {
		components[component.ID] = true
	}
	statements := map[string]bool{}
	for _, statement := range in.Level.Statements {
		statements[statement.ID] = true
	}
	approvedKind := func(id string) string {
		if history, held := state.Nodes[id]; held {
			if current := history.Current(); current != nil {
				return current.Kind
			}
		}
		return ""
	}
	// lacks is each field of a statement no approved form or node resolves, or that its notes write
	// where the reader cannot place it (LevelStatement.Unread); held statements are found again until
	// none is added, since a statement under a held one, or supporting one, is held too.
	lacks := map[string][]string{}
	for _, statement := range in.Level.Statements {
		var missing []string
		unread := func(field string) bool { return slices.Contains(statement.Unread, field) }
		if _, ok := levelKinds[statement.Kind]; !ok || unread("kind") {
			missing = append(missing, "kind")
		}
		resolves := !unread("about")
		for _, id := range statement.About {
			if kind := approvedKind(id); !components[id] && kind != specchange.KindComponent && kind != specchange.KindInterface {
				resolves = false
				break
			}
		}
		if !resolves {
			missing = append(missing, "about")
		}
		resolves = !unread("supports")
		for _, id := range statement.Supports {
			if !statements[id] && approvedKind(id) != specchange.KindStatement {
				resolves = false
				break
			}
		}
		if !resolves {
			missing = append(missing, "supports")
		}
		if statement.Parent != "" && !statements[statement.Parent] && approvedKind(statement.Parent) != specchange.KindStatement {
			missing = append(missing, "parent")
		}
		if len(missing) > 0 {
			lacks[statement.ID] = missing
		}
	}
	for grew := true; grew; {
		grew = false
		for _, statement := range in.Level.Statements {
			if _, held := lacks[statement.ID]; held {
				continue
			}
			if _, held := lacks[statement.Parent]; statement.Parent != "" && held {
				lacks[statement.ID], grew = []string{"parent"}, true
				continue
			}
			for _, id := range statement.Supports {
				if _, held := lacks[id]; held {
					lacks[statement.ID], grew = []string{"supports"}, true
					break
				}
			}
		}
	}
	var plan []planned
	for _, component := range in.Level.Components {
		if _, held := state.Nodes[component.ID]; !held {
			plan = append(plan, planned{id: component.ID, node: component})
		}
	}
	var holds []Hold
	for _, statement := range in.Level.Statements {
		if _, held := state.Nodes[statement.ID]; held {
			continue
		}
		if missing, held := lacks[statement.ID]; held {
			if !known[statement.ID] {
				holds = append(holds, Hold{ID: statement.ID, Text: statement.Text, Lacks: missing, Provenance: in.Provenance,
					Fields: holdFields(statement)})
			}
			continue
		}
		plan = append(plan, planned{id: statement.ID, node: specgraph.Statement{
			ID: statement.ID, Text: statement.Text, Kind: levelKinds[statement.Kind], Parent: statement.Parent,
			Serves: sortedUnique(statement.Supports), About: sortedUnique(statement.About),
		}})
	}
	if refusal := judgeMerged(state, plan); refusal != nil {
		return Imported{}, refusal
	}
	if len(plan) == 0 && len(holds) == 0 {
		return Imported{}, nil
	}
	var at time.Time
	if err := tx.QueryRow(ctx, `select clock_timestamp()`).Scan(&at); err != nil {
		return Imported{}, fmt.Errorf("read the time of the import: %w", err)
	}
	at = at.UTC()
	nodes, tuples, written, err := writesOf(state, plan, in.Provenance, in.Who, at)
	if err != nil {
		return Imported{}, err
	}
	// A statement an import before this one held, whose fields resolve now - the person approved
	// the notes since - is written, and its hold cleared, as an approval of its change request does.
	cleared, err := openHolds(ctx, tx, in.Project, in.Repo, written)
	if err != nil {
		return Imported{}, err
	}
	for _, hold := range cleared {
		nodes = append(nodes, clearedHold(hold, in.Project, in.Repo, in.Who, at))
	}
	for _, hold := range holds {
		address := Address(in.Project, in.Repo, hold.ID)
		text, err := marshalValue(hold)
		if err != nil {
			return Imported{}, err
		}
		nodes = append(nodes, specNode(KindHold, address, in.Project, in.Who, at))
		tuples = append(tuples, graphstore.Tuple{
			FromKind: KindHold, FromID: address, Kind: AttributeHold, ToKind: graphstore.KindValue, ToID: text,
			At: at, Who: in.Who, SourceEvent: graphstore.NoEvent, Asserted: true, Changes: true,
		})
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueWrite(b, nodes, tuples)
		return nil
	}); err != nil {
		return Imported{}, err
	}
	return Imported{At: at, Written: written, Held: holds}, nil
}

// holdFields is what a hold keeps of a statement's fields beside its text, as the level wrote them.
func holdFields(statement LevelStatement) map[string]string {
	fields := map[string]string{}
	for name, value := range map[string]string{
		"kind": statement.Kind, "parent": statement.Parent,
		"about": strings.Join(statement.About, " "), "supports": strings.Join(statement.Supports, " "),
	} {
		if value != "" {
			fields[name] = value
		}
	}
	return fields
}

// holdNodes is each id of the repository that has a hold node, open or cleared.
func holdNodes(ctx context.Context, q graphstore.Queryer, project, repo string) (map[string]bool, error) {
	prefix := Address(project, repo, "")
	rows, err := q.Query(ctx, `select id from graph_nodes where kind = $1 and supertag = $1 and starts_with(id, $2)`, KindHold, prefix)
	if err != nil {
		return nil, fmt.Errorf("read the holds of %s: %w", prefix, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the holds of %s: %w", prefix, err)
	}
	known := make(map[string]bool, len(ids))
	for _, id := range ids {
		known[id[len(prefix):]] = true
	}
	return known, nil
}

// RepairIssueFacts is the facts that name issue the repair of each hold: one value on each hold's
// node.
func RepairIssueFacts(project, repo string, holds []Hold, issue string, who model.Actor, at time.Time) []graphstore.Tuple {
	tuples := make([]graphstore.Tuple, 0, len(holds))
	for _, hold := range holds {
		tuples = append(tuples, graphstore.Tuple{
			FromKind: KindHold, FromID: Address(project, repo, hold.ID), Kind: AttributeRepairIssue,
			ToKind: graphstore.KindValue, ToID: issue, At: at, Who: who, SourceEvent: graphstore.NoEvent, Asserted: true,
		})
	}
	return tuples
}

// sortedUnique is ids sorted, each once; nil for none.
func sortedUnique(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	return slices.Compact(sorted)
}
