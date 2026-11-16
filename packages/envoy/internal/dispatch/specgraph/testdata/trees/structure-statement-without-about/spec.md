:::requirement{#legion-reads-issues-only-through-dispatch kind="structure"}
Legion reads each issue only through the interface to Dispatch.
:::

:::check{#no-issue-read-skips-the-interface serves="legion-reads-issues-only-through-dispatch" subject="graph" procedure="query"}
Claim: No read of an issue by Legion skips the interface to Dispatch.
Procedure: The query returns each read of an issue by Legion that does not go through the interface.
Failing example: A copy of the graph with one read of an issue that goes around the interface.
:::
