:::requirement{#a-stored-message-survives-a-restart kind="behavior" parent="envoy-delivers-each-message" about="envoy-delivery"}
Envoy delivers each stored message to its session after the listener starts again.
:::

:::check{#a-restart-loses-no-message serves="a-stored-message-survives-a-restart" subject="code" procedure="program" runner="go" selector="github.com/sjawhar/envoy/cmd/listener TestARestartLosesNoMessage" job="go-checks"}
Claim: A message that the listener stores before a restart reaches its session after the restart.
Procedure: The program stores one message, starts the listener again and reads the messages of the session.
It must find the message one time.
Failing example: A listener that deletes its stored messages when it starts.
:::

:::component{#envoy-delivery name="Envoy delivery" parent="envoy"}
It delivers each stored message to the session that it is for.
:::

:::component{#legion-to-dispatch name="Legion to Dispatch" kind="interface" joins="dispatch legion"}
Legion reads issues and writes results through it.
:::

:::term{#work-unit name="work unit"}
A work unit is work that makes one or more statements true.
:::
