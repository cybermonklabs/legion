# Threads and read states

## Terms

- **read state**: The record of whether the person opened an ask.
- **agent thread**: One conversation of an agent, with its own id, in which the agent works.

## Statements

:::requirement{#read-state-set}
Dispatch sets the read state of an ask when the person opens the ask.
:::

:::requirement{#thread-in-ask}
Each ask that an agent opens names the agent thread in which the agent opened it.
:::
