# Motivation

!!! quote "Silent"
    *"None of these [cognitive failures] produce a latency spike, an exception, or an
    error code. They are operationally silent."*
    — [Silent Failures, Loud Consequences](https://outshift.cisco.com/blog/ai-ml/cognitive-challenges-in-multi-agent-systems)

!!! quote "Loud"
    *"Five agents do not catch the error; they ratify it. And the system's output
    arrives with the strongest possible success signal a debate can produce:
    everyone agreed."*
    — [Silent Failures, Loud Consequences](https://outshift.cisco.com/blog/ai-ml/cognitive-challenges-in-multi-agent-systems)

## The problem

Multi-agent systems now **decide**: which peer to ask, whether the evidence is enough,
when the work is done. Monitoring still measures execution: latency, errors, cost.

A **cognitive failure** is an agent doing a *plausible* thing for a broken reason. It
verifies without examining, agrees without reconciling, or asserts without knowing.
The trace shows success, so no existing signal fires.

## The consequences

Being invisible does not make these failures minor. Measured against a ground truth,
debate accuracy runs at 0.99 with no cognitive failure present, 0.81 with two
categories in the trace, and 0.16 at three — approximately the rate expected from
guessing. Verification collapse alone costs 87 accuracy points.

The failure modes also compound in a way that inverts the usual benefit of
redundancy. Where the mechanism that would normally catch an error is itself
compromised — dissent suppressed, or verification no longer verifying — additional
agents do not correct the error. Under verification collapse, five agents reach
unanimous agreement on a wrong answer 75% of the time.

This is explored in more detail in the [tech blog](https://outshift.cisco.com/blog/ai-ml/cognitive-challenges-in-multi-agent-systems).

## From diagnosis to response

Cognitive failures can be split into two classes:

- **Structural** failures trace back to a specific design decision, so they can be fixed once identified.
- **Emergent** failures arise at runtime from the task, the context or the model itself,
  so no design change removes them and they have to be caught while the system runs.

Each class of failure requires its own answer, as summarized in the table below:

| Kind | Cause | Answer |
|---|---|---|
| **Structural** | A specific design decision (e.g. a crash-only test gate) | Name it, reproduce it, design it away with [MAS-Lab](https://ioc-project.github.io/mas-lab/) |
| **Emergent** | The task, the context, or intrinsic model behaviour | Detect and correct at runtime |

IoC addresses the structural class with [MAS-Lab](https://ioc-project.github.io/mas-lab/), an open-source framework
allowing to iterate on the design of a MAS and quantifying the improvements (or regressions) from
one version to another.

The emergent class of failures is addressed by IoC with [Cognition Protocols](https://ioc-project.github.io/cognition-protocols/) for runtime
coordination, detection built on the [SCOPE](https://ioc-project.github.io/scope/) observability substrate, and potentially
[MAS-Lab](https://ioc-project.github.io/mas-lab/) to validate each claim.

## Source

[*Silent Failures, Loud Consequences: Cognitive Challenges in Multi-Agent Systems.*](https://outshift.cisco.com/blog/ai-ml/cognitive-challenges-in-multi-agent-systems), September 2026.
