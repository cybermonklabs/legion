## Verification

Run the commands from the root of the checkout.

:::check{#c-real-use surface="browser" run="Open the page at the head and note what it shows." when="before_merge" negative_control="An empty page fails."}
The testing step's role walks the page at each head it tests.
:::

## Who runs what

A role records a check's result at a head from the CI run at that head, names the run, and does
not run the row again.
