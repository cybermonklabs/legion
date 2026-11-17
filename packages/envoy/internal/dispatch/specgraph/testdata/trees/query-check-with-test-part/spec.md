:::requirement{#a-claim-survives-a-restart kind="behavior"}
Legion keeps the claim of an issue after the daemon starts again.
:::

:::check{#each-claim-is-in-the-graph serves="a-claim-survives-a-restart" subject="graph" procedure="query" runner="go" selector="example.com/acme/daemon TestEachClaimIsInTheGraph" job="go-checks"}
Claim: The graph holds each claim that the daemon made before a restart.
Procedure: The query returns each claim that the daemon made and that the graph does not hold.
Failing example: A copy of the graph with one claim removed.
:::
