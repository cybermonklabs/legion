:::requirement{#a-claim-survives-a-restart kind="behavior"}
Legion keeps the claim of an issue after the daemon starts again.
:::

:::check{#a-restart-keeps-the-claim serves="a-claim-survives-a-restart" subject="code" procedure="program" runner="go" selector="example.com/acme/daemon TestARestartKeepsTheClaim" job="go-checks"}
Claim: The daemon reads the claim of an issue after it starts again.
Procedure: The program claims one issue, starts the daemon again and reads the claims.
It must find the claim one time.
Failing example: A daemon that keeps its claims in memory only.
:::
