:::requirement{#step-3 kind="behavior"}
Legion keeps the claim of an issue after the daemon starts again.
:::

:::check{#a-restart-keeps-the-claim serves="step-3" subject="code" procedure="agent"}
Claim: The daemon reads the claim of an issue after it starts again.
Procedure: An agent claims one issue, starts the daemon again and reads the claims.
Failing example: A daemon that keeps its claims in memory only.
:::
