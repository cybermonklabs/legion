## Verification

Run the commands from the root of the checkout.

:::check{#c-real-use surface="browser" run="Open the page at the head and note what it shows." when="before_merge" negative_control="An empty page fails."}
The testing step's role walks the page at each head it tests.
:::

## Who runs what

The implementer records each `before_merge` check with no `ci_check` at the head it hands off.
The **tester repeats**
no check.
