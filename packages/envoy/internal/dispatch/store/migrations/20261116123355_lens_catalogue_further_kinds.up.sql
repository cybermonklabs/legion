-- 20261116123355_lens_catalogue_further_kinds.up.sql
--
-- INFRA-1950: the further lens kinds, each from a primary source (plan task T10;
-- dispatch://INFRA-1950/spec#b-r-lens-catalogue). INFRA-1950's research task read the primary
-- sources of the analyses practitioners report using to review a design or a spec, and this writes
-- each kind it found a source for into the catalogue 20261116123354 seeded, in the same shape: one
-- `lens_kind` node holding its words, and one `constrained_by` tuple to a `constraint` node holding
-- the kind's source (`standard`) and that source's `reference`. A kind no source it read supports
-- is not here. The pull request body gives the reading, kind by kind, with each source.
--
-- `chestertons-fence` is the kind INFRA-1297 cited Chesterton's fence for under `second-order`:
-- the passage is about knowing why a thing is there before it is removed, which is a kind of its
-- own, so `second-order` took Bastiat (20261116123354) and Chesterton stands here.
--
-- Every catalogue constraint is the far end of exactly one constrained_by tuple from a lens_kind
-- node, and this writes no other constraint node, so dispatch://INFRA-3862's project clause reads
-- every node here as the catalogue, which every viewer sees. Every node is project-visible and
-- live, in no project and between no parties, made by the catalogue's writer, and each
-- constraint's id is a uuid fixed here, so running this file again writes the same rows: each node
-- upsert moves every column but `made_at` and `made_by`, and each tuple insert appends only a fact
-- the node does not already carry. No event records it, so every tuple carries the sentinel 0. No
-- type node moves, so the block schema the server installs does not.
with change as (
  select now() as at, '{"kind": "system", "id": "lens-catalogue"}'::jsonb as writer
),
catalogue (kind, words, source, standard, reference) as (
  values
    ('chestertons-fence', 'Chesterton''s fence: why the thing is there, before it is removed', '5eed0000-1e45-4000-8000-000000000011',
     'G. K. Chesterton, The Thing, 1929, chapter "The Drift from Domesticity"',
     'The Thing, London: Sheed & Ward, 1929, ch. 4 "The Drift from Domesticity"; p. 29 in the 1946 Sheed & Ward reprint. The US edition is The Thing: Why I Am a Catholic, New York: Dodd, Mead, 1930.'),
    ('threat-model', 'the threat model: spoofing, tampering, repudiation, information disclosure, denial of service and elevation of privilege (STRIDE)', '5eed0000-1e45-4000-8000-000000000012',
     'Loren Kohnfelder and Praerit Garg, "The threats to our products", Microsoft Interface (internal journal), 1 April 1999',
     'https://shostack.org/files/microsoft/The-Threats-To-Our-Products.docx'),
    ('fmea', 'failure mode and effects analysis: how each part can fail, what each failure does, ranked by severity', '5eed0000-1e45-4000-8000-000000000013',
     'U.S. Department of Defense, MIL-STD-1629A, Procedures for Performing a Failure Mode, Effects and Criticality Analysis, 24 November 1980',
     'MIL-STD-1629A, §3.1.15, p. 4; cancelled 4 August 1998 (Notice 3). ASSIST record: https://quicksearch.dla.mil/qsDocDetails.aspx?ident_number=37027. Copy: https://www.dsiintl.com/wp-content/uploads/2017/04/mil_std_1629a.pdf'),
    ('information-hiding', 'information hiding: which design decisions are likely to change, and which module hides each one', '5eed0000-1e45-4000-8000-000000000014',
     'D. L. Parnas, "On the Criteria To Be Used in Decomposing Systems into Modules", Communications of the ACM, December 1972',
     'Communications of the ACM 15(12), December 1972, pp. 1053–1058; doi:10.1145/361598.361623'),
    ('end-to-end', 'the end-to-end argument: can this function be done completely and correctly only at the end points?', '5eed0000-1e45-4000-8000-000000000015',
     'J. H. Saltzer, D. P. Reed, D. D. Clark, "End-to-End Arguments in System Design", ACM Transactions on Computer Systems, November 1984',
     'ACM Transactions on Computer Systems 2(4), November 1984, pp. 277–288; doi:10.1145/357401.357402. Author''s copy: https://web.mit.edu/Saltzer/www/publications/endtoend/endtoend.pdf'),
    ('inspection', 'the design inspection: read the design against a checklist of error types, to find errors and not to redesign', '5eed0000-1e45-4000-8000-000000000016',
     'M. E. Fagan, "Design and Code Inspections to Reduce Errors in Program Development", IBM Systems Journal, 1976',
     'IBM Systems Journal 15(3), 1976, pp. 182–211; doi:10.1147/sj.153.0182'),
    ('working-backwards', 'working backwards from the customer: the press release and the FAQ first', '5eed0000-1e45-4000-8000-000000000017',
     'Werner Vogels, "Working Backwards", All Things Distributed, 1 November 2006',
     'https://www.allthingsdistributed.com/2006/11/working_backwards.html'),
    ('hyrums-law', 'Hyrum''s law: which observable behaviours users will come to depend on', '5eed0000-1e45-4000-8000-000000000018',
     'Hyrum Wright, "Hyrum''s Law: An observation on Software Engineering", hyrumslaw.com',
     'https://www.hyrumslaw.com/'),
    ('red-team', 'the red team: a deliberate challenge to the plan''s assumptions', '5eed0000-1e45-4000-8000-000000000019',
     'Defense Science Board, Task Force on the Role and Status of DoD Red Teaming Activities, September 2003',
     'Office of the Under Secretary of Defense for Acquisition, Technology, and Logistics, Washington, DC, September 2003, ch. II, p. 2; https://www.govinfo.gov/content/pkg/GOVPUB-D-PURL-LPS81083/pdf/GOVPUB-D-PURL-LPS81083.pdf'),
    ('yagni', 'YAGNI: is each part needed now, and not only foreseen?', '5eed0000-1e45-4000-8000-000000000020',
     'Ron Jeffries, "You''re NOT gonna need it!", XProgramming, 4 April 1998',
     'https://ronjeffries.com/xprog/articles/practices/pracnotneed/'),
    ('alternatives-considered', 'alternatives considered: the other designs that would reach the goal, and why this one wins', '5eed0000-1e45-4000-8000-000000000021',
     'Malte Ubl, "Design Docs at Google", Industrial Empathy, 6 July 2020',
     'https://www.industrialempathy.com/posts/design-docs-at-google/'),
    ('five-whys', 'the five whys: ask why until the root cause shows', '5eed0000-1e45-4000-8000-000000000022',
     'Taiichi Ohno, Toyota Production System: Beyond Large-Scale Production, Productivity Press, 1988; first published in Japanese as Toyota seisan hoshiki, Diamond, 1978',
     'ch. 2 "Evolution of the Toyota Production System", first section "Repeating Why Five Times", p. 17; ISBN 0-915299-14-3')
),
nodes (kind, id, attributes) as (
  select 'lens_kind', catalogue.kind, jsonb_build_object('words', catalogue.words)
  from catalogue
  union all
  select 'constraint', catalogue.source, jsonb_build_object('standard', catalogue.standard, 'reference', catalogue.reference)
  from catalogue
),
written as (
  insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
  select nodes.kind, nodes.id, nodes.kind, 'project', 'live', nodes.attributes, change.at, change.writer, 0
  from nodes, change
  order by nodes.kind, nodes.id
  on conflict (kind, id) do update set
    supertag = excluded.supertag,
    visibility = excluded.visibility,
    state = excluded.state,
    attributes = excluded.attributes,
    source_event = excluded.source_event
  returning kind, id, visibility, state, attributes, made_at, made_by
),
-- Each node's two base attributes and each value it holds, read back off the row just written,
-- and each kind's tie to its source, dated and signed as its lens_kind node.
facts (from_kind, from_id, kind, to_kind, to_id, at, who) as (
  select written.kind, written.id, fact.kind, 'value', fact.value, written.made_at, written.made_by
  from written
  cross join lateral (values ('visibility', written.visibility), ('state', written.state)) as fact(kind, value)
  union all
  select written.kind, written.id, attribute.key, 'value', attribute.value #>> '{}', written.made_at, written.made_by
  from written
  cross join lateral jsonb_each(written.attributes) as attribute
  union all
  select written.kind, written.id, 'constrained_by', 'constraint', catalogue.source, written.made_at, written.made_by
  from written
  join catalogue on written.kind = 'lens_kind' and written.id = catalogue.kind
)
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select facts.from_kind, facts.from_id, facts.kind, facts.to_kind, facts.to_id, facts.at, facts.who, 0, null, true
from facts
where not exists (
  select 1 from graph_tuples held
  where held.from_kind = facts.from_kind and held.from_id = facts.from_id and held.kind = facts.kind
    and held.to_kind = facts.to_kind and held.to_id = facts.to_id
    and held.source_event = 0 and held.asserted
)
order by facts.from_kind, facts.from_id, facts.kind, facts.to_id;
