:::requirement{#a-claim-survives-a-restart kind="behavior"}
Legion keeps the claim of an issue after the daemon starts again.
:::

:::check{#a-restart-keeps-the-claim serves="a-claim-survives-a-restart" subject="code" procedure="agent"}
Claim: The code in cmd/listener/main.go reads the claim of an issue after a restart.
Procedure: An agent claims one issue, starts the daemon again and reads the claims.
Failing example: A daemon that keeps its claims in memory only.
:::
