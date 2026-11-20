# Probe checks of one CI job

:::requirement{#a-claim-survives-a-restart kind="behavior"}
Legion keeps the claim of an issue after the daemon starts again.
:::

:::check{#the-claim-stays-after-a-restart serves="a-claim-survives-a-restart" subject="code" procedure="program" runner="go" selector="example.com/acme/probe/claims TestTheClaimStaysAfterARestart" job="go-checks"}
Claim: The daemon reads the claim of an issue after it starts again.
Procedure: The program claims one issue, starts the daemon again and reads the claims.
It must find the claim one time.
Failing example: A daemon that keeps its claims in memory only.
:::

:::check{#the-owner-stays-after-a-restart serves="a-claim-survives-a-restart" subject="code" procedure="program" runner="go" selector="example.com/acme/probe/claims TestTheOwnerStaysAfterARestart" job="go-checks"}
Claim: The daemon reads the owner of a claim after it starts again.
Procedure: The program claims one issue for one session, starts the daemon again and reads the owner.
It must find the same session.
Failing example: A daemon that writes an empty owner when it starts.
:::

:::check{#the-lock-stays-after-a-restart serves="a-claim-survives-a-restart" subject="code" procedure="program" runner="go" selector="example.com/acme/probe/claims TestTheLockStaysAfterARestart" job="go-checks"}
Claim: The daemon keeps the lock of a claim after it starts again.
Procedure: The program locks one claim, starts the daemon again and reads the lock.
It must find the lock.
Failing example: A daemon that removes each lock when it starts.
:::

:::check{#the-daemon-waits-for-the-database serves="a-claim-survives-a-restart" subject="code" procedure="program" runner="go" selector="example.com/acme/probe/claims TestTheWaitIsSkipped" job="go-checks"}
Claim: The daemon waits for its database before it reads a claim.
Procedure: The program stops the database, starts the daemon and reads the claims.
The daemon must wait for the database.
Failing example: A daemon that starts with no claims when its database is stopped.
:::

:::requirement{#a-read-of-the-claims-ends-in-time kind="property"}
Each read of the claims ends in 5 seconds or less.
:::

:::check{#a-read-of-one-claim-ends-in-time serves="a-read-of-the-claims-ends-in-time" subject="code" procedure="program" runner="go" selector="example.com/acme/probe/slow TestTheQuickReadEnds" job="web-checks"}
Claim: A read of one claim ends in 1 second or less.
Procedure: The program writes one claim and reads it.
The read must end in 1 second or less.
Failing example: A read that waits for a lock that nobody holds.
:::

:::check{#a-read-of-many-claims-ends-in-time serves="a-read-of-the-claims-ends-in-time" subject="code" procedure="program" runner="go" selector="example.com/acme/probe/slow TestTheSlowReadEnds" job="go-checks"}
Claim: A read of 1000 claims ends in 5 seconds or less.
Procedure: The program writes 1000 claims and reads them.
The read must end in 5 seconds or less.
Failing example: A read that asks the database once for each claim.
:::

:::requirement{#each-claim-is-in-one-place kind="behavior"}
The daemon reads each claim from one place.
:::

:::check{#the-build-reads-the-claim serves="each-claim-is-in-one-place" subject="code" procedure="program" runner="go" selector="example.com/acme/probe/broken TestTheBuildReadsTheClaim" job="go-checks"}
Claim: The daemon finds a claim by the key of its issue.
Procedure: The program claims one issue and finds the claim by the key of the issue.
It must find one claim.
Failing example: A daemon that finds a claim only by the name of its session.
:::

:::check{#the-claim-has-one-name serves="each-claim-is-in-one-place" subject="code" procedure="program" runner="go" selector="example.com/acme/probe/twice TestTheNameRunsTwice" job="go-checks"}
Claim: The daemon gives each claim one name.
Procedure: The program claims one issue and reads the name of the claim two times.
It must read the same name.
Failing example: A daemon that makes a new name each time it reads a claim.
:::

:::check{#the-page-shows-the-claim serves="each-claim-is-in-one-place" subject="code" procedure="program" runner="bun" selector="packages/web/src/claims.test.ts" job="go-checks"}
Claim: The page of an issue shows the claim that the daemon reads.
Procedure: The program claims one issue and opens the page of the issue.
The page must show the claim.
Failing example: A page that shows the claim from the time when the page opened.
:::
