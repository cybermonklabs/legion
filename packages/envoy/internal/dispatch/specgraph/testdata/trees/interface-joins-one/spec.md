:::component{#dispatch name="Dispatch"}
It keeps the issues, the documents and the results of each project.
:::

:::component{#legion name="Legion"}
It runs the agents that work on the issues of a project.
:::

:::component{#legion-to-dispatch name="Legion to Dispatch" kind="interface" joins="dispatch"}
Legion reads issues and writes results through it.
:::
