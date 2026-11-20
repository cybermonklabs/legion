# Checks of tests that quote the words of a timeout

:::requirement{#the-outcome-is-the-tests kind="behavior"}
Each result holds the outcome that its test reported.
:::

:::check{#passes-saying-timeout serves="the-outcome-is-the-tests" subject="code" procedure="program" runner="go" selector="example.com/acc/output TestSaysTimedOut" job="go-checks"}
Claim: The test that quotes the words of a timeout passes.
Procedure: The program runs the test one time.
It must find no failure.
Failing example: A test that fails.
:::

:::check{#fails-saying-timeout serves="the-outcome-is-the-tests" subject="code" procedure="program" runner="go" selector="example.com/acc/output TestFailsSayingTimedOut" job="go-checks"}
Claim: The test that quotes the words of a timeout and fails passes.
Procedure: The program runs the test one time.
It must find no failure.
Failing example: A test that fails.
:::

:::check{#slow-test serves="the-outcome-is-the-tests" subject="code" procedure="program" runner="go" selector="example.com/acc/slow TestSlow" job="go-checks"}
Claim: The slow test ends before the time limit.
Procedure: The program runs the slow test one time.
It must find no failure.
Failing example: A test that waits for 60 seconds.
:::
