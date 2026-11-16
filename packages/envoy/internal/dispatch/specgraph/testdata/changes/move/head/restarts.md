# Claims after a restart

:::check{#a-lost-claim-is-reported serves="a-claim-survives-a-restart" subject="production" procedure="agent"}
Claim: The daemon reports a claim that it cannot read after a restart.
Procedure: An agent stops the database during a restart and reads the log of the daemon.
It must find one report for the claim.
Failing example: A daemon that starts with no claims and writes no report.
:::

:::requirement{#a-claim-survives-a-restart kind="behavior" parent="legion-keeps-each-claim-in-dispatch"}
Legion keeps the claim of an issue after the daemon starts again.
:::

:::check{#a-claim-at-the-time-limit-stays serves="a-claim-survives-a-restart" subject="code" procedure="program" runner="go" selector="example.com/acme/daemon TestAClaimAtTheTimeLimitStays" job="go-checks" time_limit="720h"}
Claim: A claim that is 59 minutes old stays after a restart.
Procedure: The program makes a claim 59 minutes old, starts the daemon again and reads the claims.
It must find the claim.
Failing example: A daemon that removes each claim older than 58 minutes when it starts.
:::

:::check{#a-restart-keeps-the-claim serves="a-claim-survives-a-restart" subject="code" procedure="program" runner="go" selector="example.com/acme/daemon TestARestartKeepsTheClaim" job="go-checks"}
Claim: The daemon reads the claim of an issue after it starts again.
Procedure: The program claims one issue, starts the daemon again and reads the claims.
It must find the claim one time.
Failing example: A daemon that keeps its claims in memory only.
:::
