# List rows a page at a time

## Goal

The widgets CLI prints a long list one page at a time. `listRows(rows, limit)` gives the rows of one page.

## Requirements

:::requirement{#r1}
**R1.** `listRows(rows, limit)` returns at most `limit` rows, in the order of `rows`. From: the issue.
:::

:::requirement{#r2}
**R2.** `listRows` refuses a `limit` that is not a whole number above 0 with a `RangeError`. From: the issue.
:::
