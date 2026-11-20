:::requirement{#the-probe-tests-pass kind="behavior"}
The two probe tests pass at each commit of a branch.
:::

:::check{#probe-passes serves="the-probe-tests-pass" subject="code" procedure="program" runner="go" selector="example.com/acme/probe TestProbe" job="go-checks"}
Claim: The probe test passes.
Procedure: The program runs the probe test one time.
Failing example: A probe test that fails.
:::

:::check{#second-probe-passes serves="the-probe-tests-pass" subject="code" procedure="program" runner="go" selector="example.com/acme/probe TestSecondProbe" job="go-checks"}
Claim: The second probe test passes.
Procedure: The program runs the second probe test one time.
Failing example: A second probe test that fails.
:::
