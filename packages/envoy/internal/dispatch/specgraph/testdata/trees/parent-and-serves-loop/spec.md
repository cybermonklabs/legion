:::requirement{#a-claim-survives-a-restart kind="behavior" serves="legion-keeps-each-claim"}
Legion keeps the claim of an issue after the daemon starts again.
:::

:::requirement{#legion-keeps-each-claim kind="behavior" parent="a-claim-survives-a-restart"}
Legion keeps the claim of each issue in Dispatch.
:::

:::check{#a-claim-is-in-dispatch serves="legion-keeps-each-claim" subject="code" procedure="agent"}
Claim: Dispatch holds the claim of an issue that Legion claims.
Procedure: An agent claims one issue and reads the issue in Dispatch.
Failing example: A daemon that keeps its claims in memory only.
:::
