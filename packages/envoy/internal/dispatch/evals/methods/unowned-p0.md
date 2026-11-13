**How it is measured.** `delivery.ListUnownedP0` lists the project's issues that have priority 0, a status other than done, no claim and no route. It reads them through the partial index `issues_unowned_p0`, so the count reads no other issue. Each refresh writes the count as the row of that UTC day, at the time of the refresh (`WriteUnownedP0Day`). A window's value is the count of its newest day (the `last` fold). The Delivery page and this eval count by the same function.

**What it does not count.** Issues that are done, issues of a lower priority, and issues that hold a claim or a route.
