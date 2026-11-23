package model

import "time"

// SpecSource is one repository whose approved spec a project holds: an issue of that repository
// holds a change request, which a person's approval writes into the approved spec of the
// repository. Repo is `owner/name`, lower-cased as a repository mapping stores it; Branch is the
// branch whose files The Spec of the repository is read from.
type SpecSource struct {
	Project   string    `json:"project"`
	Repo      string    `json:"repo"`
	Branch    string    `json:"branch"`
	Enabled   bool      `json:"enabled"`
	CreatedBy Actor     `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SpecDocumentRead is one version of a document an act of the approved spec read, with the digest
// a person's approval of it covers, and the approval request and review row of the person's own
// approval of that version where the act read it from an approval of that document alone (the
// notes of an imported level).
type SpecDocumentRead struct {
	ArtifactID string  `json:"artifact_id"`
	Name       string  `json:"name,omitempty"`
	Version    int     `json:"version"`
	Digest     string  `json:"digest"`
	Ask        *string `json:"ask,omitempty"`
	Review     *string `json:"review,omitempty"`
}

// SpecProvenance is what wrote one version or removal of a node of the approved spec: the act
// (approval, brought_back or import), the issue, the documents and versions the person's approval
// named, the approval request, the review row of artifact_reviews that holds the approval, the
// person, and for a check brought back the statement whose earlier text brought it.
type SpecProvenance struct {
	Act             string            `json:"act"`
	Issue           string            `json:"issue"`
	Spec            *SpecDocumentRead `json:"spec,omitempty"`
	Verification    *SpecDocumentRead `json:"verification,omitempty"`
	Notes           *SpecDocumentRead `json:"notes,omitempty"`
	Ask             *string           `json:"ask,omitempty"`
	Review          *string           `json:"review,omitempty"`
	Person          Actor             `json:"person"`
	BroughtBackWith string            `json:"brought_back_with,omitempty"`
}

// SpecNodeVersion is one approved text of a node: its number, the time of the act that wrote it,
// its block as The Spec's files print it now, and its provenance.
type SpecNodeVersion struct {
	Version    int            `json:"version"`
	At         time.Time      `json:"at"`
	Block      string         `json:"block"`
	Provenance SpecProvenance `json:"provenance"`
}

// SpecNodeRemoval is one removal of a node: the version it removed, the time and the provenance.
type SpecNodeRemoval struct {
	Version    int            `json:"version"`
	At         time.Time      `json:"at"`
	Provenance SpecProvenance `json:"provenance"`
}

// SpecNodeRead is one node of the approved spec of a repository of a project: its id, its kind
// (statement, check, component or interface), whether the approved spec removed it, each version
// oldest first, and each removal.
type SpecNodeRead struct {
	Project  string            `json:"project"`
	Repo     string            `json:"repo"`
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`
	Removed  bool              `json:"removed"`
	Versions []SpecNodeVersion `json:"versions"`
	Removals []SpecNodeRemoval `json:"removals"`
}

// SpecImportHold is one statement of an approved level its import could not resolve: its id, its
// text, the fields it lacks, and the provenance of the approval of its level.
type SpecImportHold struct {
	ID         string         `json:"id"`
	Text       string         `json:"text"`
	Lacks      []string       `json:"lacks"`
	Provenance SpecProvenance `json:"provenance"`
}

// SpecLevelImport is what one import of an approved level did: the level's issue and repository,
// each node it wrote, each hold of the level still open, and the issue opened for their repair,
// null while none is open.
type SpecLevelImport struct {
	Issue       string           `json:"issue"`
	Repo        string           `json:"repo"`
	Written     []string         `json:"written"`
	Held        []SpecImportHold `json:"held"`
	RepairIssue *string          `json:"repair_issue"`
}

// SpecRequestStaleNode is one node of a refused change request that the approved spec changed
// after the request was written: the version the request read (0 for none) and the approved spec's
// current one (0 where it holds none or removed it).
type SpecRequestStaleNode struct {
	ID      string `json:"id"`
	Base    int    `json:"base"`
	Current int    `json:"current"`
	Removed bool   `json:"removed,omitempty"`
}

// SpecRequestStaleEventPayload is the payload of spec.request_stale, on the issue whose change
// request a person's approval was refused because the approved spec changed after its owner wrote
// it: the issue, the approval request, each stale node, and the reason, so the owner writes it
// again against the approved spec and opens a new approval request.
type SpecRequestStaleEventPayload struct {
	Issue           string                 `json:"issue"`
	ApprovalRequest *string                `json:"approval_request"`
	Statements      []SpecRequestStaleNode `json:"statements"`
	Reason          string                 `json:"reason"`
}

// SpecApprovedWrittenNode is one fact an act wrote into an approved spec: the node, its kind, the
// version it wrote or, for a removal, the version it removed, and whether it removed the node.
type SpecApprovedWrittenNode struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Version int    `json:"version"`
	Removed bool   `json:"removed,omitempty"`
}

// SpecApprovedWrittenEventPayload is the payload of spec.approved_written, on the issue whose act
// wrote the approved spec of its repository: a person's approval of its change request
// (`approval`) or the import of its approved level (`import`), and each node the act wrote. The
// approved spec is the repository's, so every other issue of that repository reads its change
// request against what this act wrote, though nothing of that issue moved.
type SpecApprovedWrittenEventPayload struct {
	Issue string                    `json:"issue"`
	Repo  string                    `json:"repo"`
	Act   string                    `json:"act"`
	Nodes []SpecApprovedWrittenNode `json:"nodes"`
}
