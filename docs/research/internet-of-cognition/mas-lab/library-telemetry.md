# mas-lab `library-telemetry/`

Subject: the directory `library-telemetry/` of the repository `mas-lab` of github.com/ioc-project.

Clone: `$IOC_CLONES/mas-lab`, commit 477441ddb82d1b07c76c36f1426d8a5be0b6194b.

`IOC_CLONES` is /legion/scratch/tmp/ioc-clones.

Every statement below carries a claim id.

The evidence for each id is a row in `mas-lab-library-telemetry.jsonl`.

## What it is

`library-telemetry` is a Python package, `mas.library.telemetry`.

It converts native MAS execution events into OpenTelemetry (OTel) spans, checks the spans against a span specification, and sends them to an OTLP collector [m-tel-purpose].

It is the inverse of `library-kg`, which turns OTel spans into a knowledge graph [m-tel-purpose].

It has a command line component, `mas-lab telemetry`, and step functions for the MAS Lab benchmark pipelines [m-tel-cli].

## What a trace is

A trace, in this package, starts as a native trace file named `events.jsonl` [m-tel-purpose].

A native trace is a stream of JSON lines, one line for each event of one run [m-tel-event-fields].

Each line is an event with an envelope.

The envelope fields in the shipped example are these [m-tel-event-fields]:

- `kind`, the event name, for example `llm_call_start`.

- `agent_id`, `run_id`, `turn_id`, `session_id`, `task_id`, `app_name` and `mas_id`, the identities.

- `call_id` and `parent_call_id`, which build the call tree.

- `block`, `summand`, `mealy_symbol` and `layer`, the internal classification of the event.

- `timestamp`, a float of seconds.

The other fields of a line depend on the kind, for example `input`, `messages`, `tools`, `decision`, `policy_name` or `total_tokens` [m-tel-event-fields].

The example trace holds 39 events of 29 kinds [m-tel-event-fields].

The package registers a handler for each kind it knows, and the handlers are 56 in number [m-tel-kinds].

The conversion rebuilds the span tree from `call_id` and `parent_call_id`, and it keeps the original timestamps [m-tel-purpose].

The output is a second file, `otel_sdk_spans.jsonl`, with one OTel span for each line [m-tel-purpose].

## How a run is replayed

Replay is offline.

The function `replay_events_file` reads the `events.jsonl` file, feeds the events to the same handlers the live plugin uses, and writes the spans [m-tel-replay].

Replay makes the trace and span identifiers from a seed with SHA-256, so two replays of the same input give the same identifiers [m-tel-replay-seed].

A `replay_speed` parameter can hold the original gaps between the events; the default, 0, has no delay [m-tel-replay-speed].

Because replay and live mode run the same handlers, the package can compare a replayed span set with a live span set [m-tel-purpose].

## What it depends on

The package declares no required dependency; every external dependency is an optional extra [m-tel-deps].

The extras are `convert` (the OTel SDK, the OTLP HTTP exporter and `ioa-observe-sdk>=1.0.49`), `verify` (`pyyaml`, `jsonschema`), `clickhouse` (`clickhouse-connect`) and `bench` (the MAS Lab bench component and `click`) [m-tel-deps].

The read-back path needs a ClickHouse server, and the push path needs an OTLP collector [m-tel-services].

The README states that the core modules do not import `mas.runtime` [m-tel-boundary-claim].

That statement is wrong at this commit: `conversion/delegation.py` imports two names from `mas.runtime.boundary.delegation` at module level [m-tel-boundary-false].

The live plugin also imports `mas.runtime.boundary.obs` [m-tel-plugin-import].

## What could cross into our runtime

The four classes that cross unchanged are a specification document, a vocabulary or ontology, a schema, and a prompt-shaped skill.

Two files are in the schema class: `schemas/otel_sdk_spans.schema.json`, a JSON Schema for the span envelope, and `schemas/mas.spanspec.yaml`, a level model (L1 to L4) of required and recommended attributes for each span name [m-tel-schemas].

`mas.spanspec.yaml` is also a vocabulary: it declares 22 span shapes and 72 distinct `mas.*` attribute names [m-tel-span-taxonomy] [m-tel-mas-attr-count].

Nothing in the directory is a prompt-shaped skill [m-tel-no-skill].

Everything else is Python code against MAS Lab's own event stream, so for us it is a rebuild in Go or TypeScript, not a part that crosses [m-tel-crossing-code].

The collector and the ClickHouse read-back are services beside Legion [m-tel-services].

## The trace vocabulary

The vocabulary is mixed, and it is not the OpenTelemetry generative-AI semantic convention alone.

The attribute names are held in `conversion/semconv.py` [m-tel-semconv-module].

The clone holds 121 lines with `gen_ai.` in `library-telemetry/` and 162 lines with `ioa_observe.` [m-tel-genai-count] [m-tel-ioa-count].

Some names are OTel generative-AI names, for example `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.operation.name`, `gen_ai.usage.input_tokens`, `gen_ai.input.messages` and `gen_ai.response.finish_reasons` [m-tel-semconv-module].

The OTel registry page for generative-AI attributes holds those names [m-tel-otel-registry].

Other names in the same module are not OTel names, but an extension of the `gen_ai.` prefix by the `ioa-observe-sdk`: `gen_ai.ioa.graph`, `gen_ai.ioa.graph.protocol`, `gen_ai.ioa.graph_dynamism` and `gen_ai.ioa.graph_determinism_score` [m-tel-semconv-module].

The string `gen_ai.ioa` does not occur in the fetched copy of the OTel registry page [m-tel-ioa-graph-not-otel].

The span specification also requires `gen_ai.system`, and recommends `gen_ai.usage.prompt_tokens` and `gen_ai.usage.completion_tokens`, which are the older forms of the token names [m-tel-legacy-names].

The third namespace is the package's own: the span names are `TaskCall`, `AgentCall`, `LLMCall`, `ToolCall` and 18 more, and the per-span attributes are `mas.*` [m-tel-span-taxonomy].

The level model names level L1 "OTel GenAI Semantic Conventions" and level L2 the "observe-sdk / GenAI attribute contract", so the project itself says that conformance to the OTel convention is only the first of four levels [m-tel-levels].

The specification file says, in a comment on the L1 shape of `LLMCall`, that the converter uses `mas.llm.*` in the place of the generative-AI attributes, and that errors are to be expected at that level [m-tel-l1-expect-errors].

Judgement for our gap: the `gen_ai.*` subset of this vocabulary is a usable reference for the names we would emit, but the parts that carry the multi-agent meaning, which are `ioa_observe.*`, `gen_ai.ioa.*` and `mas.*`, are the vendor's and not a standard [m-tel-semconv-module] [m-tel-ioa-graph-not-otel].
