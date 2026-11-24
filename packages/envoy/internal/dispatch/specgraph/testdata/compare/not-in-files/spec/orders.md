# Orders

:::requirement{#acme-keeps-each-order kind="behavior"}
ACME keeps each order that a customer sends.
:::

:::requirement{#an-order-survives-a-restart kind="behavior" parent="acme-keeps-each-order"}
ACME keeps each stored order after the order service starts again.
:::

:::check{#a-restart-keeps-each-order serves="an-order-survives-a-restart" subject="code" procedure="program" runner="go" selector="example.com/acme/orders TestARestartKeepsEachOrder" job="go-checks"}
Claim: The order service reads each stored order after it starts again.
Procedure: The program stores one order, starts the order service again and reads the orders.
It must find the order one time.
Failing example: An order service that keeps its orders in memory only.
:::
