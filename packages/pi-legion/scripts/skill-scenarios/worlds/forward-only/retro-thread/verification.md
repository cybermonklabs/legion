# Verification

:::check{#c-list-rows surface="cli" run="bun test src/list.test.ts" when="before_merge" negative_control="A listRows that returns every row fails the first test."}
`bun test src/list.test.ts` passes: `listRows` returns at most the limit, in order, and refuses a limit below 1.
:::
