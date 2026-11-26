:::requirement{#messages-reach-sessions kind="behavior"}
Envoy delivers each message to the session that it is for.
:::

:::requirement{#a-stored-message-survives-a-restart kind="behavior" parent="messages-reach-sessions"}
Envoy delivers each stored message to its session after the listener starts again.
:::

:::check{#a-production-restart-loses-no-message serves="a-stored-message-survives-a-restart" subject="production" procedure="program" runner="go" selector="github.com/sjawhar/envoy/cmd/listener TestAProductionRestartLosesNoMessage" job="production-checks"}
Claim: A message that production stores before a restart reaches its session after the restart.
Procedure: The program stores one message in production, starts the listener again and reads the messages of the session.
It must find the message one time.
Failing example: A listener that deletes its stored messages when it starts.
:::

:::check{#the-graph-holds-a-delivery-of-each-message serves="a-stored-message-survives-a-restart" subject="graph" procedure="query"}
Claim: Each message record of the graph has a delivery record.
Procedure: The query reads each message record of the graph.
It returns each message record that has no delivery record.
Failing example: A copy of the graph with one message record that has no delivery record.
:::
