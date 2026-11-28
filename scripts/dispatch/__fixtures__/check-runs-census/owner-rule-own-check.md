## Verification

Run the commands from the root of the checkout.

:::check{#c13 surface="cli" run="Ask the chat model of production one question and note its answer." when="before_merge" negative_control="A model that answers nothing fails."}
The check reads production's chat model at the head.
:::

The tester does not run `c13` again: it spends production's chat model, so the implementer's run
stands.
