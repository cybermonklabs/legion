## Verification

Run the commands from the root of the checkout.

:::check{#c-real-use surface="browser" run="Open the page at the head and note what it shows." when="before_merge" negative_control="An empty page fails."}
The testing step's role walks the page at each head it tests.
:::

## Who runs what

Each check is recorded once at a head, by one role: the implementer, unless the check's own text
names another.
