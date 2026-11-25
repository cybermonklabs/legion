-- 20261122040952_design_gate_document_issue.up.sql — a design gate row records the issue whose
-- document it is bound to, so a carried root's texts name the old root's document a person
-- approves (INFRA-3686).
--
-- A root a hand-over created with its old root's gate (admit's readmit) holds a copy of each of the
-- old root's gate rows, bound to the old root's documents: a person approves the old root's
-- document, and Dispatch sends that document's events by the old root's key. The texts that name
-- a gate step's document as a dispatch:// link, the architect's start task and catch-up
-- (`gates[].document`) and the gate-moved message (`document`), built that link from the row's own
-- issue, so for a carried row they named the carried root's own document, which no person approves,
-- beside the old root's artifact and version.
--
-- design_gates.document_issue is record.DesignGate.DocumentIssue: the issue whose document
-- artifact_id is. A root's own registration writes the root itself (workflow's gateRegistered).
-- The carry writes the issue of the old row's document: the one the old row records, or the one
-- the store's rows on that document say (record.Store.DocumentIssues, below), and none where they
-- say none (admit's readmit). An approval of a carried row's document at or above the copy's
-- version, of a step the deployment gates, writes the issue that sent it on a carried row that
-- records none (workflow's applyArtifact and copyTakes); no other artifact event of the document
-- reaches the copy. A write that names none keeps the value the row holds while the row stays on
-- the same document (record.Postgres.PutGate).
--
-- The backfill fills every row on a document whose own registration the store holds. That
-- registration is the row on the artifact of an issue that is no root a hand-over created with
-- its gate at the generation the issue stands at (handed_over_roots): register_gate refuses a
-- document of another issue, and only that hand-over writes a copy, onto such a root. So the
-- registration's issue is the document's own, and each row on the artifact takes it: the
-- registration itself, and each copy, however many hand-overs it came through, since a copy of a
-- copy stays on the first document. A row of a document with no such registration row, or with
-- such rows of more than one issue, keeps null: the old root's re-admission cleared its rows, or
-- the root that registered the document was itself carried in with its gate, so the store cannot
-- tell a registration from a copy. record.Store.DocumentIssues reads the store by the same rule
-- when a text is built, for a row that records none (workflow's stepDocuments); where the store
-- says nothing, the daemon asks Dispatch which issue holds the artifact where it sends the text,
-- and the text gives the bare artifact reference where Dispatch does not answer
-- (workflow.NameStartDocuments and NameCatchUpDocuments).
--
-- What an older daemon binary reads on this schema. Its design_gates reads and writes name the
-- columns it knows, so it reads every row and ignores the column. Its upsert leaves the column as
-- it stands, and its insert leaves it null. So a carry it makes names no document issue, which
-- this release reads from the store or from Dispatch after a roll forward, and a registration it
-- makes over a carried row of the same step on another document keeps that row's old root, which
-- this release then names as the document's issue after a roll forward;
-- docs/kubernetes.md ("Rolling back the release that carries a person's words in the daemon's
-- texts") gives the statement that clears it before that roll forward. This migration adds no key
-- to an outbox payload.
--
-- There is no down file: migrations here are forward-only.
alter table design_gates add column document_issue text;

update design_gates g set document_issue = registered.issue
from (select r.artifact_id, min(r.issue) as issue from design_gates r
	left join issues i on i.key = r.issue
	where not exists (select 1 from handed_over_roots h where h.issue = r.issue and h.generation = i.generation and h.gate_carried)
	group by r.artifact_id
	having count(distinct r.issue) = 1) registered
where g.artifact_id = registered.artifact_id and g.document_issue is null;
