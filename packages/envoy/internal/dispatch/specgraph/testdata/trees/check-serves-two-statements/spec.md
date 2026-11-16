:::requirement{#a-claim-survives-a-restart kind="behavior"}
Legion keeps the claim of an issue after the daemon starts again.
:::

:::requirement{#a-claim-survives-a-database-restart kind="behavior"}
Legion keeps the claim of an issue after the database starts again.
:::

:::check{#a-restart-keeps-the-claim serves="a-claim-survives-a-restart" subject="code" procedure="agent"}
Claim: The daemon reads the claim of an issue after it starts again.
Procedure: An agent claims one issue, starts the daemon again and reads the claims.
Failing example: A daemon that keeps its claims in memory only.
:::

:::check{#a-database-restart-keeps-the-claim serves="a-claim-survives-a-database-restart" subject="code" procedure="agent"}
Claim: The daemon reads the claim of an issue after the database starts again.
Procedure: An agent claims one issue, starts the database again and reads the claims.
Failing example: A daemon that keeps its claims in memory only.
:::

:::check{#each-restart-keeps-the-claim serves="a-claim-survives-a-restart a-claim-survives-a-database-restart" subject="code" procedure="agent"}
Claim: The daemon reads the claim of an issue after each restart.
Procedure: An agent claims one issue, starts the daemon and the database again and reads the claims.
Failing example: A daemon that keeps its claims in memory only.
:::
