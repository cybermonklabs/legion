# Components and terms

:::component{#billing name="Billing"}
It sends a bill for each order.
:::

:::component{#orders name="Orders"}
It keeps the orders that customers send and cancel.
:::

:::component{#orders-to-billing name="Orders to billing" kind="interface" joins="billing orders"}
Orders sends each order to billing through it.
:::

:::component{#shipping name="Shipping"}
It sends the products of each paid order.
:::

:::term{#order name="order"}
An order is a request of a customer for one or more products.
:::
