---
title: Overview
---

# Internet of Cognition

<p class="eyebrow">Problem statement</p>

Agents can exchange messages reliably. They do not yet reach a shared
interpretation of the work they exchange messages about.

Several capable agents assigned to one task will exchange messages correctly and
still produce an outcome worse than any of them would produce alone. They commit
contradictory findings to shared state, interpret the same intermediate result
differently, and terminate a session without a validated agreement. None of this
surfaces as an error: there is no exception, no latency deviation and no failed
call.

Left unaddressed, these failures degrade the quality and correctness of the result
while remaining invisible to conventional monitoring. At that point the premise of
multi-agent collaboration — that a group of agents produces a better outcome than
any one of them alone — can no longer be relied upon.

[Why this happens, and what it costs](overview/motivation.md){ .md-button }

## Required capabilities

Three capabilities are required that a message transport does not provide. Agent
systems currently implement none of them.

<div class="grid cards" markdown>

-   **Shared intent**

    ---

    Agreement on what is being solved, in whose terms, and what constitutes
    completion, established before the work starts rather than inferred afterwards.

-   **Shared context**

    ---

    Memory that persists beyond the session, so that results established in one
    interaction are available in the next rather than being discarded.

-   **Shared reasoning**

    ---

    Conclusions that agents build on rather than restate, so that a decision
    reflects the evidence held across the group.

</div>

## Architectural principles

Five principles shape the architecture. Each addresses one of the capabilities
described above.

**Convergence rather than delivery.** An agent transport — A2A or MCP, carried over
SLIM — succeeds when a message arrives intact. The **Cognition Protocols** operate
above the transport under a stronger success condition: every participant ends the
exchange holding the same interpretation of what is being solved, in whose terms,
and what was agreed. The unit of exchange is a fragment of an agent's cognitive
state rather than a payload.

**Insights rather than additional telemetry.** An agent's telemetry records what
that agent did. Collective behaviour is a property of the relationships between
agents, so no single agent's telemetry contains it. The **Cognition Fabric** therefore *recomposes* the
interaction first: one execution model, one trajectory under a single session
identity, from which it builds a knowledge graph of agents, tasks and concepts. The
analysis runs on that graph, never directly on the raw telemetry spans, and its
output is a set of **Insights**: consolidated findings about the collective
behaviour, each traced back to the event that produced it. A named cognitive failure
is a good example of such insights.

**Understanding accumulates.** Findings are written back, so that one engine's
conclusion becomes another's input and each submission arrives enriched by what is
already established. Without this persistence the system is episodic: each session
begins from an empty state.

**Capability is granted by tier.** The fabric exposes exactly three surfaces, in increasing order of intrusiveness — **Analytics** (read-only),
**Active** (interact with agents and tools) and **Enforcing** (block, modify,
inject). What an engine can do is determined entirely by which tiers it binds,
and only Enforcing needs a component in the traffic path.

**Detection closes a loop.** A detected cognitive failure becomes an event that
re-opens alignment among the agents while the session is still running. Three loops
operate over the same fabric: the Cognition Protocol control loop, engine-to-agent interaction, and
enforcement.

## The architecture

<div class="ioc-figure">
<div class="ioc-figbar"><span>IoC architecture</span><button class="ioc-toggle" id="ioc-avail" type="button" aria-pressed="false">Available today</button></div>
--8<-- "assets/diagrams/ioc-architecture-simplified.svg"
</div>

<p class="ioc-reading" id="ioc-avail-note" hidden>Greyed: the Cognition Fabric's API, management and sync planes, the Cognition Engines, and the enforcement and interaction loops &mdash; designed, not yet shipped.</p>

The diagram reads from the bottom. Agents operate on an **A2A / SLIM / MCP**
substrate, which the [AgentBridge](https://shadi.agntcy.org/agentbridge/) joins as a declared endpoint. **Cognition
Protocols** operate above it to establish shared intent and reach decisions. The
**Cognition Fabric** holds memory and contracts, with **SCOPE and the Platform
Engines** within it converting telemetry into named cognitive failures. **Cognition
Engines** form the judgment tier above. **MAS-Lab** spans the stack, since any
component can be run and measured within it.

[Every component, and how they compose](overview/architecture.md){ .md-button .md-button--primary }

## Components documented here

Three components have their own sections. The **Cognition Fabric** and the
**Cognition Engines** appear in the architecture above and are described under
[Architecture](overview/architecture.md); they do not yet have dedicated sections.

<div class="grid cards" markdown>

-   :material-protocol: **Cognition Protocols**

    ---

    The protocol family that establishes and validates shared cognitive state.
    **ACCORD** establishes shared intent by coverage; **CONCORD** selects an outcome
    under the conflict that remains.

    [:octicons-arrow-right-24: Cognition Protocols](https://ioc-project.github.io/cognition-protocols/)

-   :material-eye-outline: **SCOPE**

    ---

    Supplies the comprehension layer: ingestion, normalization, the multi-layer
    knowledge graph, and the analysis engines that convert traces into insight.

    [:octicons-arrow-right-24: SCOPE](https://ioc-project.github.io/scope/)

-   :material-flask-outline: **MAS-Lab**

    ---

    The validation environment. Repeatable, observable and governed configurations
    in which protocol claims are measured against a matched baseline.

    [:octicons-arrow-right-24: MAS-Lab](https://ioc-project.github.io/mas-lab/)

</div>

## Adoption

Adoption is incremental rather than a migration. Three cumulative consumption modes
are defined. The first adds the protocol skills to existing agents, optionally
through the AgentBridge; the second adds SCOPE and the Platform Engine, which closes
the loop by firing the protocol response for the failure it detects; the third runs
the system under MAS-Lab for benchmarking and validation.

[Explore the consumption modes](how-to/index.md){ .md-button .md-button--primary }

## How to read this site

| Topic | Start at |
|---|---|
| Why the project exists | [Motivation](overview/motivation.md) |
| How the pieces fit together | [Overview → Architecture](overview/architecture.md) |
| What agents actually exchange | [Cognition Protocols](https://ioc-project.github.io/cognition-protocols/) |
| How the system sees what happened | [SCOPE](https://ioc-project.github.io/scope/) |
| How the claims are measured | [MAS-Lab](https://ioc-project.github.io/mas-lab/) |
| How to adopt it in an existing MAS | [Getting Started](how-to/index.md) |


## Tutorials & Demo

!!! info "In preparation"
    Each component section carries its own Tutorials and Demos page. Walkthroughs
    are being written; the MAS-Lab quickstart and SCOPE instrumentation guide are
    available today.

| Component | Tutorials & Demo |
|---|---|
| Cognition Protocols | [Demos](https://ioc-project.github.io/cognition-protocols/demos/) |
| SCOPE | [Getting Started](https://ioc-project.github.io/scope/tutorials/) |
| MAS-Lab | [Quickstart and tutorials](https://ioc-project.github.io/mas-lab/tutorials/) |
| Adoption path across components | [Getting Started](how-to/index.md) |

## Related reading

- [Internet of Cognition](https://outshift.cisco.com/internet-of-cognition) — the
  vision, and the *Scaling Out Superintelligence* whitepaper
- [Silent Failures, Loud Consequences](https://outshift.cisco.com/blog/ai-ml/cognitive-challenges-in-multi-agent-systems) — the empirical study behind the
  problem statement.
