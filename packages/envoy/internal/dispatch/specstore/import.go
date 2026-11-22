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
// nothing, a parent nothing holds or that is itself held - is kept as a node of kind
// spec_import_hold, with its text, its provenance and the fields it lacks, until a change request
// that supplies them is approved (Apply clears the hold of each node it writes).

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
// parent from the level's structure, and the notes' kind, about and supports, as written.
type LevelStatement struct {
	ID, Text, Parent string
	Kind             string
	About, Supports  []string
	Noted            bool
}

// Level is a level read from its two documents: its statements in their order, and its
// components and interfaces.
type Level struct {
	Statements []LevelStatement
	Components []specgraph.Component
}

// ParseLevel reads a level from its level document and its notes document.
func ParseLevel(level, notes string) (Level, error) {
	var parsed Level
	lines := strings.Split(strings.ReplaceAll(level, "\r\n", "\n"), "\n")
	parent, components := "", false
	for index := 0; index < len(lines); index++ {
		line := strings.TrimRight(lines[index], " \t")
		switch {
		case strings.HasPrefix(line, "## "):
			parent, components = "", strings.TrimSpace(line[3:]) == "Components"
			continue
		case strings.HasPrefix(line, "### "):
			parent = ""
			if under, ok := strings.CutPrefix(strings.TrimSpace(line[4:]), "Under "); ok {
				parent = strings.TrimSpace(under)
			}
			continue
		}
		if components && strings.HasPrefix(line, "|") {
			cells := tableCells(line)
			if len(cells) != 5 || cells[0] == "Id" || strings.Trim(cells[0], "-: ") == "" {
				continue
			}
			component := specgraph.Component{ID: cells[0], Interface: cells[1] == "interface", Name: cells[2], Purpose: cells[3]}
			if related := cells[4]; related != "none" && related != "" {
				if component.Interface {
					// specgraph.Component holds the components an interface joins sorted and unique, as
					// a change request's parse writes them, so an approved interface compares equal to
					// the same interface a later request names as context.
					component.Joins = sortedUnique(splitIDs(related))
				} else {
					component.Parent = related
				}
			}
			parsed.Components = append(parsed.Components, component)
			continue
		}
		name, attrs, ok := pmdoc.ParseTypedDirectiveOpen(line)
		if !ok || name != "requirement" {
			continue
		}
		id, _ := attrs[pmdoc.BlockIDAttr].(string)
		var body []string
		for index++; index < len(lines) && strings.TrimRight(lines[index], " \t") != ":::"; index++ {
			body = append(body, strings.TrimRight(lines[index], " \t"))
		}
		parsed.Statements = append(parsed.Statements, LevelStatement{ID: id, Text: strings.TrimSpace(strings.Join(body, "\n")), Parent: parent})
	}
	noted := notesOf(notes)
	for index := range parsed.Statements {
		statement := &parsed.Statements[index]
		if note, held := noted[statement.ID]; held {
			statement.Noted, statement.Kind, statement.About, statement.Supports = true, note.Kind, note.About, note.Supports
		}
	}
	return parsed, nil
}

// notesOf reads the kind, about and supports of each statement of a notes document, under a
// heading `### <id>`, from its `- Kind:`, `- About:` and `- Supports:` lines.
func notesOf(notes string) map[string]LevelStatement {
	noted := map[string]LevelStatement{}
	current := ""
	for _, raw := range strings.Split(strings.ReplaceAll(notes, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if heading, ok := strings.CutPrefix(line, "### "); ok {
			current = strings.TrimSpace(heading)
			noted[current] = LevelStatement{ID: current}
			continue
		}
		if current == "" {
			continue
		}
		note := noted[current]
		switch {
		case strings.HasPrefix(line, "- Kind:"):
			note.Kind = strings.TrimSpace(strings.TrimPrefix(line, "- Kind:"))
		case strings.HasPrefix(line, "- About:"):
			note.About = splitIDs(strings.TrimPrefix(line, "- About:"))
		case strings.HasPrefix(line, "- Supports:"):
			note.Supports = splitIDs(strings.TrimPrefix(line, "- Supports:"))
		}
		noted[current] = note
	}
	return noted
}

// tableCells is the cells of one row of a markdown table, trimmed.
func tableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(strings.TrimSuffix(line, "|"), "|")
	cells := strings.Split(line, "|")
	for index := range cells {
		cells[index] = strings.TrimSpace(cells[index])
	}
	return cells
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
	// lacks is each field of a statement no approved form or node resolves; held statements are
	// found again until none is added, since a statement under a held one, or supporting one, is
	// held too.
	lacks := map[string][]string{}
	for _, statement := range in.Level.Statements {
		var missing []string
		if _, ok := levelKinds[statement.Kind]; !ok {
			missing = append(missing, "kind")
		}
		for _, id := range statement.About {
			if kind := approvedKind(id); !components[id] && kind != specchange.KindComponent && kind != specchange.KindInterface {
				missing = append(missing, "about")
				break
			}
		}
		for _, id := range statement.Supports {
			if !statements[id] && approvedKind(id) != specchange.KindStatement {
				missing = append(missing, "supports")
				break
			}
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
