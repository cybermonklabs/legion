:::requirement{#a-claim-survives-a-restart kind="behavior"}
Legion keeps the claim of an issue after the daemon starts again.
:::

:::check{#a-restart-keeps-the-claim serves="a-claim-survives-a-restart" subject="code" procedure="agent"}
Claim: The daemon reads the claim of an issue after it starts again.
Procedure: An agent claims one issue, starts the daemon again and reads the claims.
:::
