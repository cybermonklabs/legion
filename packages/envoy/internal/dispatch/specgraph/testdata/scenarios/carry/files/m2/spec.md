:::requirement{#delivery-is-fast kind="property"}
Envoy delivers a message to a connected session in less than one second.
:::

:::check{#production-delivery-takes-under-a-second serves="delivery-is-fast" subject="production" procedure="query"}
Claim: Each delivery of the last hour took less than one second.
Procedure: The query reads the time of each delivery of the last hour.
It returns each delivery that took one second or more.
Failing example: A copy of the delivery records with one delivery that took two seconds.
:::

:::requirement{#messages-reach-sessions kind="behavior"}
Envoy delivers each message to the session that it is for.
:::

:::requirement{#a-stored-message-survives-a-restart kind="behavior" parent="messages-reach-sessions"}
Envoy delivers each stored message to its session after the listener starts again.
:::

:::check{#a-restart-loses-no-message serves="a-stored-message-survives-a-restart" subject="code" procedure="program" runner="go" selector="github.com/sjawhar/envoy/cmd/listener TestARestartLosesNoMessage" job="go-checks"}
Claim: A message that the listener stores before a restart reaches its session after the restart.
Procedure: The program stores one message, starts the listener again and reads the messages of the session.
It must find the message one time.
Failing example: A listener that deletes its stored messages when it starts.
:::
