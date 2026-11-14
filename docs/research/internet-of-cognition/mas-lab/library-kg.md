# mas-lab `library-kg/`

Subject: the directory `library-kg/` of the repository `mas-lab` of github.com/ioc-project.

Clone commit: 477441ddb82d1b07c76c36f1426d8a5be0b6194b, in `$IOC_CLONES/mas-lab`, where `IOC_CLONES` is `/legion/scratch/tmp/ioc-clones`.

Our own tree is read at commit 9542c6fbbecd184ccee73523ab3defa39d6cf23a.

Requirement: r3.

## What the library is

`library-kg` turns one agent run into one graph document [m-kg-purpose].

The input is a trace: a native `events.jsonl` file, or OpenTelemetry spans [m-kg-inputs].

The output is `kg.jsonld`: typed nodes and typed edges that follow the SCOPE MAS ontology [m-kg-purpose].

It is a trace-analysis library and not a store of record [m-kg-artifact].

Its own prose says the core does not import the rest of mas-lab [m-kg-selfcontained].

## The store

The store is a file [m-kg-artifact].

`KGArtifact` holds three fields: `nodes`, `edges` and `metadata` [m-kg-artifact].

It is written as one JSON-LD document and read back from one [m-kg-artifact].

Neo4j is the second store, and it is optional [m-kg-neo4j-optional].

A graph reaches Neo4j only through a push of a document that was built first [m-kg-neo4j-merge].

## The write primitive

The write primitive is "build the whole document, then write the whole document" [m-kg-build].

`normalize_events` and `extract_graph` read the events of a run and return the node and edge lists for it [m-kg-build].

`KGArtifact.save` writes the file with `write_text`, so the earlier content of that path is gone [m-kg-save-overwrite].

There is no record of an earlier value beside the new one [m-kg-no-history].

The one enrichment primitive is `annotate_kg_nodes` [m-kg-annotate].

It adds an annotation node and one edge for each matching node, and it does not change the document it is given [m-kg-annotate].

The annotation id is a hash of the source node id and the edge name, so a second run with a new value writes the same id again [m-kg-annotate].

The Neo4j write is `MERGE (n:Type {id: row.__id}) SET n += row.__props` [m-kg-neo4j-merge].

`SET n += ...` replaces the value of each property the row carries, in place [m-kg-neo4j-merge].

An edge is written the same way: `MERGE (a)-[r:TYPE]->(b)` and `SET r += row.props` [m-kg-neo4j-merge].

The push can first delete every node of a session, under `clear_session` [m-kg-neo4j-clear].

So a write overwrites, and a write keeps no history [m-kg-no-history].

No tuple carries the identity of its writer, and none carries its cause [m-kg-no-writer].

The word `provenance` in this code names an optional layer of context-contribution nodes of the trace, not the writer of a row [m-kg-provenance-meaning].

## The query surface

The query surface is four pure-Python classes over the loaded document: `KGIndex`, `FacetQuery`, `KGSource` and `KGView` [m-kg-query].

`KGIndex` walks edges, `KGSource` cuts a subgraph from a facet filter, `KGView` searches nodes of one type, and `FacetQuery` is the filter as plain data [m-kg-query].

There is no query language of its own [m-kg-query].

Cypher is reached only after a push to Neo4j [m-kg-neo4j-merge].

A second surface validates rather than queries: structural checks, SHACL shapes and attribute conformance [m-kg-validate].

## What it depends on

The one hard dependency is `ioc-scope-ontology[models]>=1.1.0`, from SCOPE [m-kg-deps].

The rest are extras: `rdflib` and `pyshacl` for validation, `jsonschema` and `pyyaml` for event checking, `neo4j` for the push, `ioc-scope-norm` for the OTel path, and the mas-lab bench component for the pipeline steps and the CLI [m-kg-deps].

Two data files stand apart from the Python: `schemas/events.schema.json` with `schemas/events.spec.yaml` for the event envelope [m-kg-events-schema], and `ontology/extensions/mas-kg-native-extensions.ttl` for the classes SCOPE does not yet declare [m-kg-ontology-ext].

## Their write primitive against ours

Their write primitive and ours differ in kind, not in degree.
Our graph is specified in dispatch://INFRA-1098, spec version 97, requirement R21, which says: "A tuple row holds `from`, `kind`, `to`, the time, the writer and the write that caused it. A tuple is appended and never updated, and ending a relation appends a retraction." [m-kg-our-append]
Our tuple is therefore an event with an author and a cause, and the current value of a node is a projection of those appended tuples, so the history of any value reads back [m-kg-our-append]; our current code already names the relation kinds that model uses [m-kg-our-edgekinds].
Their write is the opposite: the document is rebuilt from the trace and the file is replaced [m-kg-save-overwrite], the Neo4j row takes `SET n += props` in place [m-kg-neo4j-merge], and no row holds a writer or a cause [m-kg-no-writer].
That is right for their problem, because their graph is derived from a log that is itself the history, and an identical rebuild is cheap; it is wrong for ours, because our graph is the record people write into and argue over, where who wrote a fact and what caused it are the fact.
So their store answers "what did this run do", while ours must answer "who said this, when, and why" [m-kg-write-compare].

## What could cross

The four classes that can cross unchanged are a specification document, a vocabulary or ontology, a schema, and a prompt-shaped skill [m-kg-cross-classes].

Under that rule, two parts of `library-kg` could cross: the event schema `schemas/events.schema.json` with `schemas/events.spec.yaml` [m-kg-events-schema], and the ontology extension `ontology/extensions/mas-kg-native-extensions.ttl` [m-kg-ontology-ext].

Both are data files and neither needs a Python runtime [m-kg-events-schema] [m-kg-ontology-ext].

The ontology extension is of limited use alone, because it names itself a temporary file beside SCOPE's own ontology [m-kg-ontology-ext].

Everything else in the directory is Python that runs beside a run, so it crosses only as a service or as a rebuild [m-kg-cross-rest].

Whether either file is worth adopting for Legion is a judgement and not a measurement [m-kg-cross-verdict].
