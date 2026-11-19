-- 20261116123354_lens_catalogue.up.sql
--
-- INFRA-1950: the lens catalogue is nodes of the store (dispatch://INFRA-1950/spec#b-r-lens-catalogue).
-- This migration writes three things, each the way 20261113230000 writes a type node: as rows,
-- with the tuples each row is the projection of, and no event.
--
-- One. The `lens_kind` supertag. The kinds `lens`, `risk`, `constraint` and `interaction` are
-- already types of the store (20261109051232's closed list), and a lens's kind is a node of the
-- catalogue, so the type of those nodes is one more row. It declares no block type: a lens kind is
-- never a block of a document, so the block schema the server installs reads past it and its
-- version does not move.
--
-- Two. The attribute nodes of the supertags that declare no block type, each tied to its type by an
-- `attribute_of` tuple, so a reader of the store reads what each of this tree's types holds from
-- the store and not from Go: a lens kind's `words`; a lens's `verdict`; a risk's `heading`,
-- `early_warning` and `prevention`; a constraint's `standard` and `reference`, or its
-- `measurement`, `value` and where it was `taken_where`; and an interaction's `words`, the time it
-- `ends_at` and each `frame` of it. An interaction's project is the base attribute `project`
-- (graphstore.AttributeProject), which no attribute node declares. Each node carries what the seed
-- gives an attribute of a block type - its kind, its name, its store name and its position among
-- its type's attributes - and since none of these names is a base attribute, each is stored under
-- its own name.
--
-- Three. The ten lens kinds dispatch://INFRA-1297 records, each a `lens_kind` node holding its
-- words, and each with one `constrained_by` tuple to a `constraint` node holding the kind's primary
-- source (`standard`) and that source's `reference`, as INFRA-1950's research task read them in the
-- sources themselves. Every catalogue constraint is the far end of exactly one constrained_by tuple
-- from a lens_kind node, and this writes no other constraint node: that is the shape
-- dispatch://INFRA-3862's project clause reads as the catalogue, which every viewer sees. Every
-- node is project-visible and live, in no project and between no parties, made by this
-- migration's writer. A constraint's id is a uuid fixed here, so a second run writes the same node.
-- A kind added later is `envoy-dispatch lens-kind-add`, which writes the same shape
-- (graphstore.AddLensKind).
--
-- Running this file again writes the same rows: each node upsert moves every column but `made_at`
-- and `made_by`, and each tuple insert appends only a fact the node does not already carry. The
-- migration records no event, so every tuple carries the `source_event` sentinel 0. The triggers of
-- 20261109051232 announce the supertag and attribute rows once, at commit, and the server installs
-- the block schema those nodes build, which is the one it installed before.

-- The type nodes: the supertag, then its attribute nodes and those of the other store-only types.
with change as (
  select now() as at, '{"kind": "system", "id": "lens-catalogue"}'::jsonb as writer
),
attributes (supertag, name, kind, position) as (
  values ('lens_kind', 'words', 'string', 0),
         ('lens', 'verdict', 'string', 0),
         ('risk', 'heading', 'string', 0),
         ('risk', 'early_warning', 'string', 1),
         ('risk', 'prevention', 'string', 2),
         ('constraint', 'standard', 'string', 0),
         ('constraint', 'reference', 'string', 1),
         ('constraint', 'measurement', 'string', 2),
         ('constraint', 'value', 'string', 3),
         ('constraint', 'taken_where', 'string', 4),
         ('interaction', 'words', 'string', 0),
         ('interaction', 'ends_at', 'timestamp', 1),
         ('interaction', 'frame', 'string', 2)
),
nodes (kind, id, attributes) as (
  select 'supertag', 'lens_kind', '{}'::jsonb
  union all
  select 'attribute', attributes.supertag || '/' || attributes.name,
         jsonb_build_object('kind', attributes.kind, 'name', attributes.name,
                            'store_name', attributes.name, 'position', attributes.position)
  from attributes
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
-- The facts each node carries, read back off the row just written as the seed reads them: its two
-- base attributes, each value its attributes column holds (a string as itself, any other value as
-- its JSON text), and, for an attribute node, what ties it to its type. A reader of the schema
-- follows that tuple, never the spelling of the node's id.
facts (from_kind, from_id, kind, to_kind, to_id, at, who) as (
  select written.kind, written.id, fact.kind, 'value', fact.value, written.made_at, written.made_by
  from written
  cross join lateral (values ('visibility', written.visibility), ('state', written.state)) as fact(kind, value)
  union all
  select written.kind, written.id, attribute.key, 'value', attribute.value #>> '{}', written.made_at, written.made_by
  from written
  cross join lateral jsonb_each(written.attributes) as attribute
  union all
  select written.kind, written.id, 'attribute_of', 'supertag', split_part(written.id, '/', 1), written.made_at, written.made_by
  from written
  where written.kind = 'attribute'
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

-- The catalogue: each kind's lens_kind node and its constraint node, and the one constrained_by
-- tuple from the first to the second. The words, the sources and the references are INFRA-1950's
-- research task's reading of each source itself (the pull request body names each), and
-- `second-order` takes Bastiat, whose essay states the idea, where INFRA-1297 cited Chesterton's
-- fence, which the further kinds of 20261116123355 hold as a kind of its own.
with change as (
  select now() as at, '{"kind": "system", "id": "lens-catalogue"}'::jsonb as writer
),
catalogue (kind, words, source, standard, reference) as (
  values
    ('pre-mortem', 'the pre-mortem over failure modes', '5eed0000-1e45-4000-8000-000000000001',
     'Gary Klein, "Performing a Project Premortem", Harvard Business Review, September 2007 (Forethought section)',
     'Harvard Business Review vol. 85, September 2007; HBR reprint F0709A; https://hbr.org/2007/09/performing-a-project-premortem'),
    ('boring-solution', 'the boring or standard solution', '5eed0000-1e45-4000-8000-000000000002',
     'Dan McKinley, "Choose Boring Technology", mcfunley.com, 30 March 2015',
     'https://mcfunley.com/choose-boring-technology'),
    ('best-practice', 'best practice', '5eed0000-1e45-4000-8000-000000000003',
     'Robert C. Camp, Benchmarking: The Search for Industry Best Practices that Lead to Superior Performance, ASQC Quality Press, 1989',
     'Milwaukee, WI: ASQC Quality Press; New York: Quality Resources, 1989. ISBN 0-87389-058-2. Ch. 1 "Introduction", section "Benchmarking Defined", pp. 10–12.'),
    ('first-principles', 'first principles', '5eed0000-1e45-4000-8000-000000000004',
     'Aristotle, Physics, book I, chapter 1',
     'Physics I.1, 184a10–16 (Bekker); trans. R. P. Hardie and R. K. Gaye'),
    ('inversion', 'inversion', '5eed0000-1e45-4000-8000-000000000005',
     'Charles T. Munger, Harvard School commencement speech, Harvard School, Los Angeles, 13 June 1986',
     'Poor Charlie''s Almanack, ch. 4, "Talk One: Harvard School Commencement Speech", Stripe Press online edition: https://www.stripe.press/poor-charlies-almanack/talk-one'),
    ('trade-off', 'trade-off analysis', '5eed0000-1e45-4000-8000-000000000006',
     'Rick Kazman, Mark Klein, Paul Clements, ATAM: Method for Architecture Evaluation, Software Engineering Institute, Carnegie Mellon University, August 2000',
     'Technical Report CMU/SEI-2000-TR-004 (ESC-TR-2000-004); §7.2 "Sensitivity and Tradeoff Points", pp. 22–23; https://www.sei.cmu.edu/documents/629/2000_005_001_13706.pdf'),
    ('reversibility', 'reversibility as one-way against two-way doors', '5eed0000-1e45-4000-8000-000000000007',
     'Jeffrey P. Bezos, 2015 Letter to Shareholders, Amazon.com, Inc.',
     '"To our shareowners", section "Invention Machine"; Exhibit 99.1 to Amazon.com, Inc.''s Form 8-K filed 5 April 2016 (SEC accession 0001193125-16-530910); https://www.sec.gov/Archives/edgar/data/1018724/000119312516530910/d168744dex991.htm'),
    ('second-order', 'second-order effects', '5eed0000-1e45-4000-8000-000000000008',
     'Frédéric Bastiat, "What Is Seen and What Is Not Seen" ("Ce qu''on voit et ce qu''on ne voit pas"), 1850',
     'Selected Essays on Political Economy, trans. Seymour Cain, ed. George B. de Huszar (Irvington-on-Hudson, NY: Foundation for Economic Education, 1995), ch. 1, opening paragraph; https://www.econlib.org/book-chapters/chapter-chapter-1-what-is-seen-and-what-is-not-seen/'),
    ('conway', 'the Conway check', '5eed0000-1e45-4000-8000-000000000009',
     'Melvin E. Conway, "How Do Committees Invent?", Datamation, April 1968',
     'Datamation vol. 14, no. 4 (April 1968), pp. 28–31, F. D. Thompson Publications. Author''s reprint: https://www.melconway.com/research/committees.html'),
    ('botec', 'BOTEC, the back-of-the-envelope calculation', '5eed0000-1e45-4000-8000-000000000010',
     'Jon Bentley, "Programming Pearls: The Back of the Envelope", Communications of the ACM, March 1984',
     'Communications of the ACM 27(3), March 1984, pp. 180–184; doi:10.1145/357994.381168')
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
