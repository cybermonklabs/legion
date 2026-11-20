# Claims of issues

:::component{#dispatch name="Dispatch"}
It keeps the issues, the documents and the results of each project.
:::

:::component{#legion name="Legion"}
It runs the agents that work on the issues of a project.
:::

:::requirement{#legion-keeps-each-claim-in-dispatch kind="behavior"}
Legion keeps the claim of each issue in Dispatch.
:::

:::requirement{#a-claim-survives-a-restart kind="behavior" parent="legion-keeps-each-claim-in-dispatch"}
Legion keeps the claim of an issue after the daemon starts again.
:::

:::check{#a-restart-keeps-the-claim serves="a-claim-survives-a-restart" subject="code" procedure="program" runner="go" selector="example.com/acme/daemon TestARestartKeepsTheClaim" job="go-checks"}
Claim: The daemon reads the claim of an issue after it starts again.
Procedure: The program claims one issue, starts the daemon again and reads the claims.
It must find the claim one time.
Failing example: A daemon that keeps its claims in memory only.
:::

:::requirement{#a-lost-claim-is-reported kind="behavior"}
Legion reports each claim that it cannot read after a restart.
:::

:::check{#the-log-names-a-lost-claim serves="a-lost-claim-is-reported" subject="production" procedure="agent"}
Claim: The daemon reports a claim that it cannot read after a restart.
Procedure: An agent stops the database during a restart and reads the log of the daemon.
It must find one report for the claim.
Failing example: A daemon that starts with no claims and writes no report.
:::
