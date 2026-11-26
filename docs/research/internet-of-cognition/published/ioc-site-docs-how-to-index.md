---
title: Getting Started
---

# Getting Started

<p class="eyebrow">Two ways to adopt IoC</p>

There are two ways into the Internet of Cognition, and they answer different
questions.
**Run** puts IoC into a multi-agent system you operate.
**Test** measures what it changes, before or alongside that.

Neither path requires the other. A team that wants the benefit in production can start
with Run; a team that needs evidence of the benefits first can start with Test.

<div class="grid cards" markdown>

-   :material-rocket-launch-outline: **Run**

    ---

    Instrument your multi-agent system and insert the Cognition Protocols, so the
    system handles the cognitive challenges it meets and you can see what it did.

    [:octicons-arrow-right-24: Go to Run](#run)

-   :material-flask-outline: **Test**

    ---

    Reproduce the existing labs, or build your own, to measure what cognitive
    failures cost and what the Cognition Protocols are worth on your own system.

    [:octicons-arrow-right-24: Go to Test](#test)

</div>

## :material-rocket-launch-outline: Run

<p class="eyebrow">Cognition Protocols + SCOPE</p>

Running IoC means two things, and they are independent — either is useful on its own,
and together they close the loop.

**Insert the Cognition Protocols** into your agents, so that when a cognitive
challenge arises the system has a mechanism to deal with it: ACCORD establishes
shared intent before the work starts, and CONCORD reaches a decision when the agents
pull apart. Both ship as skills, so nothing in the application has to be rewritten.

**Instrument your system for SCOPE**, which recomposes the per-agent traces into one
multi-agent trajectory and gives you monitoring and explainability over the system as
a whole — including the cognitive failures that latency and error-rate monitoring
structurally cannot see.

| Component | What it adds | Start here |
|---|---|---|
| **Cognition Protocols** | ACCORD and CONCORD, deployed as skills in the agents or in the AgentBridge | [Cognition Protocols — Installation](https://ioc-project.github.io/cognition-protocols/getting-started/) |
| **SCOPE** | Instrumentation, trajectory recomposition, detection of cognitive failures, analysis and explainability | [SCOPE — Getting Started](https://ioc-project.github.io/scope/tutorials/) |

Done together, SCOPE is what tells you a cognitive failure happened and the protocols
are what act on it.

## :material-flask-outline: Test

<p class="eyebrow">MAS-Lab, with the Cognition Protocols and SCOPE inside it</p>

Testing means running the system under **MAS-Lab**, which integrates both the
Cognition Protocols and SCOPE so that a claim about either can be measured rather
than asserted.

You can **reproduce the existing labs** — the cognitive-failure studies and the
Cognition Protocols evaluation — and you can **write new ones**, against either the
sample multi-agent systems MAS-Lab ships or your own system.

What that buys you is a number instead of an argument: what a given cognitive failure
actually costs on a system like yours, and what the Cognition Protocols are worth
against a matched baseline, with SCOPE's sensing layer embedded to supply the
evidence.

| What you want | Start here |
|---|---|
| Install MAS-Lab and run a first example, then reproduce or write labs | [MAS-Lab](https://ioc-project.github.io/mas-lab/) |

## Which to start with

| If you&hellip; | Start with |
|---|---|
| operate a multi-agent system and want the benefit in production | **Run** |
| have to justify adoption with numbers before changing production | **Test** |
| are deciding which cognitive failures matter on your own system | **Test** |
| already run OpenTelemetry instrumentation | **Run**, beginning with SCOPE |
| own the agents and want coordination benefit first | **Run**, beginning with the protocols |

The two are not exclusive, and most teams end up with both: Test to establish what
a change is worth, Run to put it in front of real traffic.

## Related reading

- [Overview &rarr; Architecture](../overview/architecture.md) &mdash; the components in full
- [Overview &rarr; Motivation](../overview/motivation.md) &mdash; what cognitive failures cost, measured
