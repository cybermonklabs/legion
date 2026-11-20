# Checks of tests with long output and the words of a timeout

:::requirement{#the-output-is-read kind="behavior"}
Each result keeps the verdict and the start of what its test wrote.
:::

:::check{#three-k-output serves="the-output-is-read" subject="code" procedure="program" runner="go" selector="example.com/acc/output TestThreeK" job="go-checks"}
Claim: The test that writes 3000 characters passes.
Procedure: The program runs the test one time.
It must find no failure.
Failing example: A test that fails.
:::

:::check{#seventy-k-output serves="the-output-is-read" subject="code" procedure="program" runner="go" selector="example.com/acc/output TestSeventyK" job="go-checks"}
Claim: The test that writes 70 KiB passes.
Procedure: The program runs the test one time.
It must find no failure.
Failing example: A test that fails.
:::
