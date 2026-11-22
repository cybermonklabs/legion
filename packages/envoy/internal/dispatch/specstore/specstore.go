// Package specstore holds the approved spec of each spec source of a project in Dispatch's graph
// (D3 of the work-units plan, R2): one node of kind spec_node per statement, check, component and
// interface, addressed `<project>/<owner>/<repo>/<id>`, so an id is unique in its repository (R1)
// and two repositories of one project keep their own. Each approved text is one value `version`
// appended to the node - the node's fields as specgraph holds them, its version number, the time
// of the act that wrote it and that act's provenance - and a removal one value `removed`. Nothing
// updates or deletes a fact, so a node's history is its tuples in the order they were appended,
// read from the one graph store. Approved builds specgraph.Approved from them, printing each
// version's block with specgraph's Print at the read, so a change of the file form reprints every
// version, and it is the one reader of the approved spec. Apply writes a person's approval of a
// change request; Import writes an approved level (import.go).
package specstore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/specchange"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// The node kinds of the approved spec, each its own supertag
// (migration 20261122060000_spec_change_supertags): a node of The Spec, and a statement of an
// approved level the import could not resolve (import.go).
const (
	KindNode = "spec_node"
	KindHold = "spec_import_hold"
)

// The values a node of the approved spec carries, each appended, never moved: one approved text,
// and one removal.
const (
	AttributeVersion = "version"
	AttributeRemoved = "removed"
)

// The acts that write the approved spec (Provenance.Act).
const (
	// ActApproval is a person's approval of a change request.
	ActApproval = "approval"
	// ActBroughtBack is a check the approval of a statement's earlier text brought back with it
	// (R25), with that approval's provenance.
	ActBroughtBack = "brought_back"
	// ActImport is the import of an approved level, under the person's approval of it.
	ActImport = "import"
)

// Address is the node of the approved spec id of the repository repo (`owner/name`) of project.
func Address(project, repo, id string) string { return project + "/" + repo + "/" + id }

// Document is one version of a document an act read: its artifact, its name, its version and the
// digest an approval of it covers (artifact_version_text_sha256). Ask and Review are the approval
// request and the review row of the person's own approval of that version where the act read it
// from an approval of that document alone - the notes of a level whose approval request named the
// spec alone (Import) - and empty where the act's own approval named the version.
type Document struct {
	ArtifactID string  `json:"artifact_id"`
	Name       string  `json:"name,omitempty"`
	Version    int     `json:"version"`
	Digest     string  `json:"digest"`
	Ask        *string `json:"ask,omitempty"`
	Review     *string `json:"review,omitempty"`
}

// Provenance is what wrote one fact of the approved spec: the act, the issue whose change request
// or level it wrote, the documents and versions the person's approval named with their digests,
// the approval request it answered, the review row of artifact_reviews that holds the approval
// where the act read it (an import; an approval's row is the one its request and document name,
// whose id the write phase chooses), the person, and for a check brought back the statement whose
// earlier text brought it.
type Provenance struct {
	Act             string      `json:"act"`
	Issue           string      `json:"issue"`
	Spec            *Document   `json:"spec,omitempty"`
	Verification    *Document   `json:"verification,omitempty"`
	Notes           *Document   `json:"notes,omitempty"`
	Ask             *string     `json:"ask,omitempty"`
	Review          *string     `json:"review,omitempty"`
	Person          model.Actor `json:"person"`
	BroughtBackWith string      `json:"brought_back_with,omitempty"`
}

// Version is one approved text of a node, as its `version` value holds it: the version's number,
// the time of the act that wrote it, the node's kind and fields, and the act's provenance.
type Version struct {
	Version    int             `json:"version"`
	At         time.Time       `json:"at"`
	Kind       string          `json:"kind"`
	Node       json.RawMessage `json:"node"`
	Provenance Provenance      `json:"provenance"`
}

// Removal is one removal of a node, as its `removed` value holds it: the version it removed, the
// time of the act and its provenance.
type Removal struct {
	Version    int        `json:"version"`
	At         time.Time  `json:"at"`
	Provenance Provenance `json:"provenance"`
}

// History is every fact of one node of the approved spec, in the order the store appended them:
// each version, and each removal, interleaved in Events.
type History struct {
	ID       string
	Address  string
	Versions []Version
	Removals []Removal
	// Events is, for each fact in order, the index of its version, or -1-i for removal i.
	Events []int
}

// Removed is whether the node's last fact is a removal.
func (h *History) Removed() bool {
	return len(h.Events) > 0 && h.Events[len(h.Events)-1] < 0
}

// Current is the node's current version, nil where it holds none or its last fact removed it.
func (h *History) Current() *Version {
	if len(h.Versions) == 0 || h.Removed() {
		return nil
	}
	return &h.Versions[len(h.Versions)-1]
}

// AsOf is the version the node held just before at, nil where it held none or a removal before at
// came after its last version then.
func (h *History) AsOf(at time.Time) *Version {
	var held *Version
	for _, event := range h.Events {
		switch {
		case event >= 0 && h.Versions[event].At.Before(at):
			held = &h.Versions[event]
		case event < 0 && h.Removals[-1-event].At.Before(at):
			held = nil
		}
	}
	return held
}

// State is the approved spec of one repository of a project, every node's history by id.
type State struct {
	Project, Repo string
	Nodes         map[string]*History
}

// Read reads the approved spec of the repository repo of project, every node with its whole
// history, in one statement.
func Read(ctx context.Context, q graphstore.Queryer, project, repo string) (State, error) {
	return read(ctx, q, project, repo, "")
}

// ReadNode reads the whole history of the node id of the approved spec of the repository repo of
// project, nil where the approved spec never held it.
func ReadNode(ctx context.Context, q graphstore.Queryer, project, repo, id string) (*History, error) {
	state, err := read(ctx, q, project, repo, id)
	if err != nil {
		return nil, err
	}
	return state.Nodes[id], nil
}

// read is Read, of the one node id where id is not "".
func read(ctx context.Context, q graphstore.Queryer, project, repo, id string) (State, error) {
	state := State{Project: project, Repo: repo, Nodes: map[string]*History{}}
	prefix := Address(project, repo, "")
	rows, err := q.Query(ctx, `
		select n.id, t.kind, t.to_id
		from graph_nodes n
		join graph_tuples t on t.from_kind = n.kind and t.from_id = n.id
		where n.kind = $1 and n.supertag = $1 and starts_with(n.id, $2) and ($5 = '' or n.id = $2 || $5)
		  and t.kind = any($3) and t.to_kind = $4 and t.asserted
		order by t.seq
	`, KindNode, prefix, []string{AttributeVersion, AttributeRemoved}, graphstore.KindValue, id)
	if err != nil {
		return State{}, fmt.Errorf("read the approved spec of %s: %w", prefix, err)
	}
	defer rows.Close()
	for rows.Next() {
		var address, kind, value string
		if err := rows.Scan(&address, &kind, &value); err != nil {
			return State{}, fmt.Errorf("read the approved spec of %s: %w", prefix, err)
		}
		id := address[len(prefix):]
		history := state.Nodes[id]
		if history == nil {
			history = &History{ID: id, Address: address}
			state.Nodes[id] = history
		}
		switch kind {
		case AttributeVersion:
			var version Version
			if err := json.Unmarshal([]byte(value), &version); err != nil {
				return State{}, fmt.Errorf("read version of the approved node %s: %w", address, err)
			}
			history.Events = append(history.Events, len(history.Versions))
			history.Versions = append(history.Versions, version)
		case AttributeRemoved:
			var removal Removal
			if err := json.Unmarshal([]byte(value), &removal); err != nil {
				return State{}, fmt.Errorf("read a removal of the approved node %s: %w", address, err)
			}
			history.Removals = append(history.Removals, removal)
			history.Events = append(history.Events, -len(history.Removals))
		}
	}
	if err := rows.Err(); err != nil {
		return State{}, fmt.Errorf("read the approved spec of %s: %w", prefix, err)
	}
	return state, nil
}

// Approved is the approved spec of the repository repo of project as specgraph holds it: the one
// reader of it (D3).
func Approved(ctx context.Context, q graphstore.Queryer, project, repo string) (specgraph.Approved, error) {
	state, err := Read(ctx, q, project, repo)
	if err != nil {
		return specgraph.Approved{}, err
	}
	return state.Approved()
}

// Approved is the state as specgraph holds an approved spec: each node with each version's block
// as Print writes its fields now, and removed where its last fact is a removal.
func (s State) Approved() (specgraph.Approved, error) {
	approved := specgraph.Approved{Nodes: make(map[string]specgraph.ApprovedNode, len(s.Nodes))}
	for id, history := range s.Nodes {
		removed, partly := history.Removed(), false
		node := specgraph.ApprovedNode{ID: id, Removed: &removed, PartlyChecked: &partly}
		for _, version := range history.Versions {
			block, err := version.Block()
			if err != nil {
				return specgraph.Approved{}, fmt.Errorf("print version %d of the approved node %s: %w", version.Version, history.Address, err)
			}
			node.Versions = append(node.Versions, specgraph.ApprovedVersion{Version: version.Version, At: version.At, Block: block})
		}
		approved.Nodes[id] = node
	}
	return approved, nil
}

// Block is the version's node as Print writes it.
func (v Version) Block() (string, error) {
	node, err := DecodeNode(v.Kind, v.Node)
	if err != nil {
		return "", err
	}
	return printNode(node)
}

// Lock takes the transaction-scoped lock of the approved spec of the repository repo of project.
// Every writer of that approved spec takes it before it reads the spec it writes against, so two
// approvals at one base are judged one after the other, the second against what the first wrote.
func Lock(ctx context.Context, tx pgx.Tx, project, repo string) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('dispatch-spec-source'), hashtext($1))`, Address(project, repo, "")); err != nil {
		return fmt.Errorf("lock the approved spec of %s/%s: %w", project, repo, err)
	}
	return nil
}

// statementJSON, checkJSON and componentJSON are the fields of a node as a version holds them.
type statementJSON struct {
	ID     string   `json:"id"`
	Text   string   `json:"text"`
	Kind   string   `json:"kind"`
	Parent string   `json:"parent,omitempty"`
	Serves []string `json:"serves,omitempty"`
	About  []string `json:"about,omitempty"`
}

type checkJSON struct {
	ID             string `json:"id"`
	Serves         string `json:"serves"`
	Claim          string `json:"claim,omitempty"`
	ProcedureText  string `json:"procedure_text,omitempty"`
	FailingExample string `json:"failing_example,omitempty"`
	Subject        string `json:"subject,omitempty"`
	Procedure      string `json:"procedure,omitempty"`
	Runner         string `json:"runner,omitempty"`
	Selector       string `json:"selector,omitempty"`
	Job            string `json:"job,omitempty"`
	TimeLimit      string `json:"time_limit,omitempty"`
}

type componentJSON struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Purpose   string   `json:"purpose"`
	Parent    string   `json:"parent,omitempty"`
	Interface bool     `json:"interface,omitempty"`
	Joins     []string `json:"joins,omitempty"`
}

// EncodeNode is a node's kind and fields as a version holds them.
func EncodeNode(node any) (string, json.RawMessage, error) {
	var kind string
	var fields any
	switch n := node.(type) {
	case specgraph.Statement:
		kind, fields = specchange.KindStatement, statementJSON{ID: n.ID, Text: n.Text, Kind: string(n.Kind), Parent: n.Parent, Serves: n.Serves, About: n.About}
	case specgraph.Check:
		c := checkJSON{ID: n.ID, Serves: n.Serves, Claim: n.Claim, ProcedureText: n.ProcedureText, FailingExample: n.FailingExample,
			Subject: string(n.Subject), Procedure: string(n.Procedure)}
		if n.Test != nil {
			c.Runner, c.Selector, c.Job = n.Test.Runner, n.Test.Selector, n.Test.Job
		}
		if n.TimeLimit > 0 {
			c.TimeLimit = n.TimeLimit.String()
		}
		kind, fields = specchange.KindCheck, c
	case specgraph.Component:
		kind = specchange.KindComponent
		if n.Interface {
			kind = specchange.KindInterface
		}
		fields = componentJSON{ID: n.ID, Name: n.Name, Purpose: n.Purpose, Parent: n.Parent, Interface: n.Interface, Joins: n.Joins}
	default:
		return "", nil, fmt.Errorf("specstore: %T is no node of The Spec", node)
	}
	raw, err := json.Marshal(fields)
	return kind, raw, err
}

// DecodeNode is the node a version holds, by its kind and fields.
func DecodeNode(kind string, raw json.RawMessage) (any, error) {
	switch kind {
	case specchange.KindStatement:
		var s statementJSON
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return specgraph.Statement{ID: s.ID, Text: s.Text, Kind: specgraph.Kind(s.Kind), Parent: s.Parent, Serves: s.Serves, About: s.About}, nil
	case specchange.KindCheck:
		var c checkJSON
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		check := specgraph.Check{ID: c.ID, Serves: c.Serves, Claim: c.Claim, ProcedureText: c.ProcedureText, FailingExample: c.FailingExample,
			Subject: specgraph.Subject(c.Subject), Procedure: specgraph.Procedure(c.Procedure)}
		if c.Runner != "" || c.Selector != "" || c.Job != "" {
			check.Test = &specgraph.Test{Runner: c.Runner, Selector: c.Selector, Job: c.Job}
		}
		if c.TimeLimit != "" {
			limit, err := time.ParseDuration(c.TimeLimit)
			if err != nil {
				return nil, err
			}
			check.TimeLimit = limit
		}
		return check, nil
	case specchange.KindComponent, specchange.KindInterface:
		var c componentJSON
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		return specgraph.Component{ID: c.ID, Name: c.Name, Purpose: c.Purpose, Parent: c.Parent, Interface: c.Interface, Joins: c.Joins}, nil
	}
	return nil, fmt.Errorf("specstore: %q is no kind of node of The Spec", kind)
}

// printNode is the block Print writes for node.
func printNode(node any) (string, error) {
	switch n := node.(type) {
	case specgraph.Statement:
		return n.Print()
	case specgraph.Check:
		return n.Print()
	case specgraph.Component:
		return n.Print()
	}
	return "", fmt.Errorf("specstore: %T is no node of The Spec", node)
}

// withoutPosition is node with no position, as a version holds it: a position names where a block
// stood in a file or an issue, and no version keeps it.
func withoutPosition(node any) any {
	switch n := node.(type) {
	case specgraph.Statement:
		n.Pos = specgraph.Position{}
		return n
	case specgraph.Check:
		n.Pos = specgraph.Position{}
		return n
	case specgraph.Component:
		n.Pos = specgraph.Position{}
		return n
	}
	return node
}

// sortedIDs is the ids of the state's nodes, sorted.
func (s State) sortedIDs() []string {
	ids := make([]string, 0, len(s.Nodes))
	for id := range s.Nodes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
