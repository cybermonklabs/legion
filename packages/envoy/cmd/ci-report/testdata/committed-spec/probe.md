# The checks of the probe package

:::requirement{#the-probe-works kind="behavior"}
The probe package gives the right answer at each commit.
:::

:::check{#alpha-passes serves="the-probe-works" subject="code" procedure="program" runner="go" selector="example.com/acc/probe TestAlpha" job="go-checks"}
Claim: The alpha test of the probe package passes.
Procedure: The program runs the alpha test one time.
It must find no failure.
Failing example: An alpha test that fails.
:::

:::check{#beta-passes serves="the-probe-works" subject="code" procedure="program" runner="go" selector="example.com/acc/probe TestBeta" job="go-checks"}
Claim: The beta test of the probe package passes.
Procedure: The program runs the beta test one time.
It must find no failure.
Failing example: A beta test that fails.
:::
