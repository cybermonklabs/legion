-- 20261116123355_lens_catalogue_further_kinds.up.sql
--
-- The further lens kinds, each from a primary source (plan task T10;
-- spec requirement r-lens-catalogue). This change's research task read the primary
-- sources of the analyses practitioners report using to review a design or a spec, and this writes
-- each kind it found a source for into the catalogue 20261116123354 seeded, in the same shape: one
-- `lens_kind` node holding its words, and one `constrained_by` tuple to a `constraint` node holding
-- the kind's source (`standard`) and that source's `reference`. A kind no source it read supports
-- is not here. The pull request body gives the reading, kind by kind, with each source.
--
-- `chestertons-fence` is the kind that the lens research the catalogue is seeded from (a record
-- outside this repository) cited Chesterton's fence for under `second-order`: the passage is about
-- knowing why a thing is there before it is removed, which is a kind of its own, so `second-order`
-- took Bastiat (20261116123354) and Chesterton stands here.
--
-- Every catalogue constraint is the far end of exactly one constrained_by tuple from a lens_kind
-- node, and this writes no other constraint node, so cybermonklabs/legion#4177's project clause
-- (the catalogue list of its refs/catalogue.go) reads every node here as the catalogue, which every
-- viewer sees. Every node is project-visible and live, in no project and between no parties, made
-- by the catalogue's writer, and each constraint's id is a uuid fixed here.
--
-- Each statement is one insert of literal `values` rows, every row written out whole, as
-- 20261116123354 writes its own: graph_nodes and graph_tuples are above the census's size limit in
-- production, and on a table above it the census admits only literal `values` rows, each statement
-- with a fixed row count. So no row is read back off another (no `select`, no CTE) and none is
-- skipped where the store already holds it (no `on conflict`, no `not exists`): the file runs
-- once, as the runner runs every migration, and a second run is refused on graph_nodes' primary
-- key and writes nothing. The nodes stand in ascending (kind, id), all before any tuple, and the
-- tuples in the order of their node, then their kind and far end. Every row is dated
-- '2026-11-16 12:33:55+00', a literal at this migration's version, so a second run over its own
-- rows writes the same rows. No event records it, so every row carries the sentinel 0. No type node
-- moves, so the block schema the server installs does not.

-- The nodes: each kind's lens_kind node and its constraint node.
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
values
  ('constraint', '5eed0000-1e45-4000-8000-000000000011', 'constraint', 'project', 'live', '{"standard": "G. K. Chesterton, The Thing, 1929, chapter \"The Drift from Domesticity\"", "reference": "The Thing, London: Sheed & Ward, 1929, ch. 4 \"The Drift from Domesticity\"; p. 29 in the 1946 Sheed & Ward reprint. The US edition is The Thing: Why I Am a Catholic, New York: Dodd, Mead, 1930."}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000012', 'constraint', 'project', 'live', '{"standard": "Loren Kohnfelder and Praerit Garg, \"The threats to our products\", Microsoft Interface (internal journal), 1 April 1999", "reference": "https://shostack.org/files/microsoft/The-Threats-To-Our-Products.docx"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000013', 'constraint', 'project', 'live', '{"standard": "U.S. Department of Defense, MIL-STD-1629A, Procedures for Performing a Failure Mode, Effects and Criticality Analysis, 24 November 1980", "reference": "MIL-STD-1629A, §3.1.15, p. 4; cancelled 4 August 1998 (Notice 3). ASSIST record: https://quicksearch.dla.mil/qsDocDetails.aspx?ident_number=37027. Copy: https://www.dsiintl.com/wp-content/uploads/2017/04/mil_std_1629a.pdf"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000014', 'constraint', 'project', 'live', '{"standard": "D. L. Parnas, \"On the Criteria To Be Used in Decomposing Systems into Modules\", Communications of the ACM, December 1972", "reference": "Communications of the ACM 15(12), December 1972, pp. 1053–1058; doi:10.1145/361598.361623"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000015', 'constraint', 'project', 'live', '{"standard": "J. H. Saltzer, D. P. Reed, D. D. Clark, \"End-to-End Arguments in System Design\", ACM Transactions on Computer Systems, November 1984", "reference": "ACM Transactions on Computer Systems 2(4), November 1984, pp. 277–288; doi:10.1145/357401.357402. Author''s copy: https://web.mit.edu/Saltzer/www/publications/endtoend/endtoend.pdf"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000016', 'constraint', 'project', 'live', '{"standard": "M. E. Fagan, \"Design and Code Inspections to Reduce Errors in Program Development\", IBM Systems Journal, 1976", "reference": "IBM Systems Journal 15(3), 1976, pp. 182–211; doi:10.1147/sj.153.0182"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000017', 'constraint', 'project', 'live', '{"standard": "Werner Vogels, \"Working Backwards\", All Things Distributed, 1 November 2006", "reference": "https://www.allthingsdistributed.com/2006/11/working_backwards.html"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000018', 'constraint', 'project', 'live', '{"standard": "Hyrum Wright, \"Hyrum''s Law: An observation on Software Engineering\", hyrumslaw.com", "reference": "https://www.hyrumslaw.com/"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000019', 'constraint', 'project', 'live', '{"standard": "Defense Science Board, Task Force on the Role and Status of DoD Red Teaming Activities, September 2003", "reference": "Office of the Under Secretary of Defense for Acquisition, Technology, and Logistics, Washington, DC, September 2003, ch. II, p. 2; https://www.govinfo.gov/content/pkg/GOVPUB-D-PURL-LPS81083/pdf/GOVPUB-D-PURL-LPS81083.pdf"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000020', 'constraint', 'project', 'live', '{"standard": "Ron Jeffries, \"You''re NOT gonna need it!\", XProgramming, 4 April 1998", "reference": "https://ronjeffries.com/xprog/articles/practices/pracnotneed/"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000021', 'constraint', 'project', 'live', '{"standard": "Malte Ubl, \"Design Docs at Google\", Industrial Empathy, 6 July 2020", "reference": "https://www.industrialempathy.com/posts/design-docs-at-google/"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000022', 'constraint', 'project', 'live', '{"standard": "Taiichi Ohno, Toyota Production System: Beyond Large-Scale Production, Productivity Press, 1988; first published in Japanese as Toyota seisan hoshiki, Diamond, 1978", "reference": "ch. 2 \"Evolution of the Toyota Production System\", first section \"Repeating Why Five Times\", p. 17; ISBN 0-915299-14-3"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'alternatives-considered', 'lens_kind', 'project', 'live', '{"words": "alternatives considered: the other designs that would reach the goal, and why this one wins"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'chestertons-fence', 'lens_kind', 'project', 'live', '{"words": "Chesterton''s fence: why the thing is there, before it is removed"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'end-to-end', 'lens_kind', 'project', 'live', '{"words": "the end-to-end argument: can this function be done completely and correctly only at the end points?"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'five-whys', 'lens_kind', 'project', 'live', '{"words": "the five whys: ask why until the root cause shows"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'fmea', 'lens_kind', 'project', 'live', '{"words": "failure mode and effects analysis: how each part can fail, what each failure does, ranked by severity"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'hyrums-law', 'lens_kind', 'project', 'live', '{"words": "Hyrum''s law: which observable behaviours users will come to depend on"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'information-hiding', 'lens_kind', 'project', 'live', '{"words": "information hiding: which design decisions are likely to change, and which module hides each one"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'inspection', 'lens_kind', 'project', 'live', '{"words": "the design inspection: read the design against a checklist of error types, to find errors and not to redesign"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'red-team', 'lens_kind', 'project', 'live', '{"words": "the red team: a deliberate challenge to the plan''s assumptions"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'threat-model', 'lens_kind', 'project', 'live', '{"words": "the threat model: spoofing, tampering, repudiation, information disclosure, denial of service and elevation of privilege (STRIDE)"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'working-backwards', 'lens_kind', 'project', 'live', '{"words": "working backwards from the customer: the press release and the FAQ first"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'yagni', 'lens_kind', 'project', 'live', '{"words": "YAGNI: is each part needed now, and not only foreseen?"}', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0);

-- Each node's two base attributes and each value it holds, and each kind's tie to its source,
-- dated and signed as its lens_kind node.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values
  ('constraint', '5eed0000-1e45-4000-8000-000000000011', 'reference', 'value', 'The Thing, London: Sheed & Ward, 1929, ch. 4 "The Drift from Domesticity"; p. 29 in the 1946 Sheed & Ward reprint. The US edition is The Thing: Why I Am a Catholic, New York: Dodd, Mead, 1930.', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000011', 'standard', 'value', 'G. K. Chesterton, The Thing, 1929, chapter "The Drift from Domesticity"', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000011', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000011', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000012', 'reference', 'value', 'https://shostack.org/files/microsoft/The-Threats-To-Our-Products.docx', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000012', 'standard', 'value', 'Loren Kohnfelder and Praerit Garg, "The threats to our products", Microsoft Interface (internal journal), 1 April 1999', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000012', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000012', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000013', 'reference', 'value', 'MIL-STD-1629A, §3.1.15, p. 4; cancelled 4 August 1998 (Notice 3). ASSIST record: https://quicksearch.dla.mil/qsDocDetails.aspx?ident_number=37027. Copy: https://www.dsiintl.com/wp-content/uploads/2017/04/mil_std_1629a.pdf', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000013', 'standard', 'value', 'U.S. Department of Defense, MIL-STD-1629A, Procedures for Performing a Failure Mode, Effects and Criticality Analysis, 24 November 1980', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000013', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000013', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000014', 'reference', 'value', 'Communications of the ACM 15(12), December 1972, pp. 1053–1058; doi:10.1145/361598.361623', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000014', 'standard', 'value', 'D. L. Parnas, "On the Criteria To Be Used in Decomposing Systems into Modules", Communications of the ACM, December 1972', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000014', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000014', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000015', 'reference', 'value', 'ACM Transactions on Computer Systems 2(4), November 1984, pp. 277–288; doi:10.1145/357401.357402. Author''s copy: https://web.mit.edu/Saltzer/www/publications/endtoend/endtoend.pdf', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000015', 'standard', 'value', 'J. H. Saltzer, D. P. Reed, D. D. Clark, "End-to-End Arguments in System Design", ACM Transactions on Computer Systems, November 1984', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000015', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000015', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000016', 'reference', 'value', 'IBM Systems Journal 15(3), 1976, pp. 182–211; doi:10.1147/sj.153.0182', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000016', 'standard', 'value', 'M. E. Fagan, "Design and Code Inspections to Reduce Errors in Program Development", IBM Systems Journal, 1976', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000016', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000016', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000017', 'reference', 'value', 'https://www.allthingsdistributed.com/2006/11/working_backwards.html', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000017', 'standard', 'value', 'Werner Vogels, "Working Backwards", All Things Distributed, 1 November 2006', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000017', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000017', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000018', 'reference', 'value', 'https://www.hyrumslaw.com/', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000018', 'standard', 'value', 'Hyrum Wright, "Hyrum''s Law: An observation on Software Engineering", hyrumslaw.com', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000018', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000018', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000019', 'reference', 'value', 'Office of the Under Secretary of Defense for Acquisition, Technology, and Logistics, Washington, DC, September 2003, ch. II, p. 2; https://www.govinfo.gov/content/pkg/GOVPUB-D-PURL-LPS81083/pdf/GOVPUB-D-PURL-LPS81083.pdf', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000019', 'standard', 'value', 'Defense Science Board, Task Force on the Role and Status of DoD Red Teaming Activities, September 2003', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000019', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000019', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000020', 'reference', 'value', 'https://ronjeffries.com/xprog/articles/practices/pracnotneed/', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000020', 'standard', 'value', 'Ron Jeffries, "You''re NOT gonna need it!", XProgramming, 4 April 1998', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000020', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000020', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000021', 'reference', 'value', 'https://www.industrialempathy.com/posts/design-docs-at-google/', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000021', 'standard', 'value', 'Malte Ubl, "Design Docs at Google", Industrial Empathy, 6 July 2020', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000021', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000021', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000022', 'reference', 'value', 'ch. 2 "Evolution of the Toyota Production System", first section "Repeating Why Five Times", p. 17; ISBN 0-915299-14-3', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000022', 'standard', 'value', 'Taiichi Ohno, Toyota Production System: Beyond Large-Scale Production, Productivity Press, 1988; first published in Japanese as Toyota seisan hoshiki, Diamond, 1978', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000022', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000022', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'alternatives-considered', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000021', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'alternatives-considered', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'alternatives-considered', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'alternatives-considered', 'words', 'value', 'alternatives considered: the other designs that would reach the goal, and why this one wins', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'chestertons-fence', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000011', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'chestertons-fence', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'chestertons-fence', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'chestertons-fence', 'words', 'value', 'Chesterton''s fence: why the thing is there, before it is removed', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'end-to-end', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000015', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'end-to-end', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'end-to-end', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'end-to-end', 'words', 'value', 'the end-to-end argument: can this function be done completely and correctly only at the end points?', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'five-whys', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000022', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'five-whys', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'five-whys', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'five-whys', 'words', 'value', 'the five whys: ask why until the root cause shows', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'fmea', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000013', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'fmea', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'fmea', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'fmea', 'words', 'value', 'failure mode and effects analysis: how each part can fail, what each failure does, ranked by severity', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'hyrums-law', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000018', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'hyrums-law', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'hyrums-law', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'hyrums-law', 'words', 'value', 'Hyrum''s law: which observable behaviours users will come to depend on', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'information-hiding', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000014', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'information-hiding', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'information-hiding', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'information-hiding', 'words', 'value', 'information hiding: which design decisions are likely to change, and which module hides each one', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'inspection', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000016', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'inspection', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'inspection', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'inspection', 'words', 'value', 'the design inspection: read the design against a checklist of error types, to find errors and not to redesign', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'red-team', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000019', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'red-team', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'red-team', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'red-team', 'words', 'value', 'the red team: a deliberate challenge to the plan''s assumptions', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'threat-model', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000012', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'threat-model', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'threat-model', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'threat-model', 'words', 'value', 'the threat model: spoofing, tampering, repudiation, information disclosure, denial of service and elevation of privilege (STRIDE)', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'working-backwards', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000017', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'working-backwards', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'working-backwards', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'working-backwards', 'words', 'value', 'working backwards from the customer: the press release and the FAQ first', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'yagni', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000020', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'yagni', 'state', 'value', 'live', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'yagni', 'visibility', 'value', 'project', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'yagni', 'words', 'value', 'YAGNI: is each part needed now, and not only foreseen?', '2026-11-16 12:33:55+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true);
