# Components and interfaces

:::component{#dispatch name="Dispatch"}
It keeps the issues, the documents and the results of each project.
:::

:::component{#legion name="Legion"}
It runs the agents that work on the issues of a project.
:::

:::component{#legion-to-dispatch name="Legion to Dispatch" kind="interface" joins="dispatch legion"}
Legion reads issues and writes results through it.
:::

:::requirement{#legion-reads-issues-only-through-dispatch kind="structure" parent="legion-keeps-each-claim-in-dispatch" about="legion-to-dispatch"}
Legion reads each issue only through the interface to Dispatch.
:::

:::check{#no-issue-read-skips-the-interface serves="legion-reads-issues-only-through-dispatch" subject="graph" procedure="query"}
Claim: No read of an issue by Legion skips the interface to Dispatch.
Procedure: The query returns each read of an issue by Legion that does not go through the interface.
It must return none.
Failing example: A copy of the graph with one read of an issue that goes around the interface.
:::

:::term{#work-unit name="work unit"}
A work unit is work that makes one or more statements true.
:::
