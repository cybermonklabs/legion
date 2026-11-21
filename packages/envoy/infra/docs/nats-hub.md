# `envoy-infra:gcp:NatsHub`

NATS with JetStream on one Google Cloud VM that joins a tailnet, where its clients reach it, as
the NATS server `envoy-infra:host:Listener` connects to. How to install the plugin, give a
component its providers, and write a host's NATS seed is in [the module's README](../README.md).

`envoy-infra:gcp:ClusteredNatsHub` serves the same users from three NATS servers on a GKE cluster
instead of one VM: [The clustered hub](#the-clustered-hub-gcpclusterednatshub), at the end of this
document, says how it differs in running, updating and watching it.

## What `gcp:NatsHub` deploys

A hub whose `hostname` is `<h>` registers these resources, each a child of the hub and named after
`<h>` rather than after the hub's own name:

| Resource | Type | What it is |
| --- | --- | --- |
| `<h>-hub` | `gcp:serviceaccount/account:Account` | The VM's service account, account id `<h>-hub`. |
| `<h>-tailnet-auth-key` | `gcp:secretmanager/secret:Secret` | The secret `<h>-tailnet-auth-key`, replicated automatically. |
| `<h>-tailnet-auth-key` | `gcp:secretmanager/secretVersion:SecretVersion` | Its version, holding `tailnetAuthKey`. |
| `<h>-tailnet-auth-key-accessor` | `gcp:secretmanager/secretIamMember:SecretIamMember` | The service account's grant of `roles/secretmanager.secretAccessor` on that secret. |
| `<h>-listener-seed-accessor` | `gcp:secretmanager/secretIamMember:SecretIamMember` | Only with `listener`: the same grant on `listener.seedSecret`, a secret the consumer declares and writes. |
| `<h>-network` | `gcp:compute/network:Network` | Only without `network`: an auto-mode network, with a subnetwork in every region. |
| `<h>-health-checks` | `gcp:compute/firewall:Firewall` | The hub's one firewall rule: tcp/8222 from Google's health-check ranges, `130.211.0.0/22` and `35.191.0.0/16`, to the VM's network tag `<h>-hub`. |
| `<h>-healthz` | `gcp:compute/healthCheck:HealthCheck` | An HTTP check of NATS's `/healthz` on port 8222, every 10 seconds, unhealthy after 6 failures. |
| `<h>` | `gcp:compute/instanceTemplate:InstanceTemplate` | The VM: `machineType`, a boot disk from Container-Optimized OS's `cos-stable` image family, the data disk, an external address for its way out to the image registries and the tailnet (see [The VM's external address](#the-vms-external-address)), the service account with the `cloud-platform` scope, the tag `<h>-hub`, and the cloud-config as its `user-data` (see [What the hub's VM runs](#what-the-hubs-vm-runs)). |
| `<h>` | `gcp:compute/instanceGroupManager:InstanceGroupManager` | The zonal group of that one VM, in `zone`. It keeps the data disk (see [The hub's data disk](#the-hubs-data-disk)), recreates the VM once the health check finds it unhealthy, from an initial delay after the VM is created that covers the first boot's read of the data disk (see [The first boot's read](#the-first-boots-read)), and replaces it when the template changes (see [Updating a hub](#updating-a-hub)). It is created after the key's version and the account's grants, since its first VM fetches the key when it boots and its listener fetches its seed when it starts, and `pulumi up` waits until it is `STABLE`. |

Every resource is in the gcp provider's project. The template's name is `<h>-` and a suffix the
provider adds, and the network, the firewall rule, the health check and the group are named after
their resource names with a random suffix, such as `<h>-healthz-292ee38`. The account id and the
secret id are exact, so two hubs in one project need two hostnames.

The hub's outputs are `natsUrl`, `nats://<h>:4222`, where its clients reach it over the tailnet,
and `instanceGroup`, the group's name.

## The hub's inputs

| Input | Default | What it is |
| --- | --- | --- |
| `users` | none | The NATS server's nkey users. See [The hub's users](#the-hubs-users). At least one is required unless the hub has a `listener`. |
| `tailnetAuthKey` | required | A secret: the Tailscale auth key the VM joins the tailnet with. Use a single-use, pre-approved key: the hub's service account can read it for as long as the hub exists, and so can root on the VM, the NATS container included, until the node has joined, so a reusable key would let either add devices with the hub's tags. The tailnet must let it advertise `tags`. May come from another resource or stack. |
| `hostname` | required | The VM's name on the tailnet and `natsUrl`'s host: 2 to 26 lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen (`^[a-z][a-z0-9-]{0,24}[a-z0-9]$`). It also names every child. |
| `tags` | required | The tailnet tags the VM advertises when it joins, each `tag:<name>`. At least one. |
| `zone` | required | The zone of the VM and its data disk, such as `us-central1-a`. Changing it makes a new group, with a new data disk: see [Updating a hub](#updating-a-hub). |
| `machineType` | `e2-small` | The VM's machine type. |
| `diskType` | `pd-balanced` | The type of the data disk the group creates for the hub's first VM: `pd-balanced` or `pd-ssd`, the types whose documented read rates the first boot's read of the disk is timed by (see [The first boot's read](#the-first-boots-read)). |
| `diskSizeGb` | `20` | The size, in GB, of that disk: at most 252 for `pd-balanced` and 506 for `pd-ssd`, the largest whose first-boot read fits the group's initial delay. |
| `natsImage` | required | The NATS server image, pinned by digest: `<name>@sha256:<64 lowercase hex digits>`. Its entrypoint takes nats-server's arguments, as the official `nats` image's does. |
| `tailscaleImage` | required | Tailscale's image, pinned the same way. The unit configures it through containerboot's `TS_*` variables, as Tailscale's own image reads them. |
| `tailnetDnsName` | unset | The tailnet's DNS name, one label under `ts.net` (`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.ts\.net$`), such as `example-tailnet.ts.net`. Required for a hub with a `listener`, since the hub renders the address that listener answers on, `<h>.<tailnetDnsName>`, from it into the listener's `ENVOY_ALLOWED_HOSTS`; a hub without a listener may leave it unset. A plain value the program knows when it runs, never another resource's output. |
| `network` | created | The name or self link of the VPC network the VM and the firewall rule are in. Unset, the hub creates one, which has no firewall rule but the hub's. The VM has an external address and takes the network's rules for all its instances, so on a network whose firewall admits SSH from anywhere, as the default network's `default-allow-ssh` does, its sshd is reachable from the internet: see [The VM's external address](#the-vms-external-address). May come from another resource or stack. |
| `subnetwork` | unset | The VM's subnetwork, for a network with custom subnetworks: one in `zone`'s region. Only with `network`. May come from another resource or stack. |
| `listener` | unset | The hub's own Envoy listener: `image`, the listener's image pinned as the images above are; `seedSecret`, the id of a Secret Manager secret in the hub's project holding its NATS user's seed; `nkey`, that seed's public key; `allowedHosts`, any further address a client dials this hub by, beyond the hub's own `<h>.<tailnetDnsName>`, which it always serves; and the listener's broker check, `agentSecretsUrl`, `agentSecretsAudience` and `publicUrl`, set together or not at all. Each `allowedHosts` entry is a plain host name or IP address, rendered as written into the listener's `ENVOY_ALLOWED_HOSTS`. Two more inputs are optional: `senderMessagesPerMinute`, from 1 to 60000, and `senderMessageBurst`, at least 1 and only with a rate, the listener's limit on the messages each agent session sends. See [The hub's listener](#the-hubs-listener), [The listener's broker check](#the-listeners-broker-check) and [The listener's sender limit](#the-listeners-sender-limit). |
| `sessionUser` | unset | `name` and `nkey`, the NATS user a client presenting no credential becomes, granted `SessionPermissions`. See [The hub's users](#the-hubs-users). |
| `authCallout` | unset | The auth service's callout (dispatch://INFRA-840), a `nats:AuthCallout`, each of whose fields is required: `issuer`, the account public key whose seed signs the service's answers; `xkey`, the curve public key the server seals each request to, without which nats-server sends each connecting client's token to the service unsealed; and `serviceUser`, the `name` of the entry of `users` the service connects as. See [The auth service](#the-auth-service). Unset, the hub renders exactly what it rendered before callouts existed. |
| `privateProjects` | `false` | The deployment may make a Dispatch project private (R12 of dispatch://INFRA-3862). Refused without `authCallout`, beside a `sessionUser`, beside any entry of `users` but `authCallout.serviceUser`, and when `authCallout.serviceUser` is not a `permissions` entry or is granted more than the auth service's grant. See [The auth service](#the-auth-service). |

Only `tailnetAuthKey`, `network` and `subnetwork` may come from another resource's output. Every
other input takes a value the program knows when it runs, and the plugin refuses an output there,
naming the input. So the cloud-config is known at preview.

The hub refuses these before it registers anything, with an error naming the input:

- an nkey seed, found as `envoy-infra seed` finds one (`bus.HoldsSeed`: any type, either case,
  whole, inside a longer value, split by `.`, `-`, `_` or whitespace, or percent-encoded), in the
  hub's own name or an input it carries into the user data, a child or a refusal: `hostname`,
  `tailnetDnsName`, a tag, `zone`, `machineType`, `diskType`, an image, a user's `machineId`,
  `listenerMachineId` or `dispatchMachineId`, or the listener's `image`, `seedSecret`,
  `agentSecretsUrl`, `agentSecretsAudience`, `publicUrl` or one of its `allowedHosts`. The
  error names the input by its path and never its value, as
  `users[0].machineId: an nkey user seed, which is never an input of the hub (not echoed)`, and a
  seed in the hub's own name as `envoy-infra:gcp:NatsHub: the component's name holds an nkey seed
  (not echoed)`. `network`, `subnetwork` and `tailnetAuthKey` may come from another resource, so a
  seed in any of them fails the preview or update once it resolves, with the same error, before
  any child is registered with it: a NATS seed passed as the Tailscale key is refused before a
  Secret Manager version holds it, and the error never carries the secret. A seed in a user's
  `name` or a rule entry is refused by the authorization, as [The hub's users](#the-hubs-users)
  says, and a seed as a user's `nkey` is refused as an `nkey` that is no public key, without
  quoting it;
- a `hostname` outside the rule above;
- a `tailnetDnsName` outside the rule above, whenever it is set;
- no `tags`, or a tag that is not `tag:` followed by a letter and then letters, digits and
  hyphens, Tailscale's rule for a tag;
- an empty `zone`;
- an image that is not `<name>@sha256:<64 lowercase hex digits>`, with only letters, digits and
  `._/:-` in `<name>`: whitespace, `$` or `%` would split or expand the unit's command;
- an empty `machineType` or `diskType`, or a `diskSizeGb` below 1;
- a `diskType` other than `pd-balanced` or `pd-ssd`: `pd-standard` reads 0.12 MiB/s per GiB with
  no baseline, so its first-boot read takes over 8,500 s at any size, past the most the group
  waits, and an E2 VM, as the default `e2-small` is, attaches `hyperdisk-balanced` only by
  arrangement with Google's account team;
- a `diskSizeGb` above the largest for its `diskType`, with an error naming that size, as
  `diskSizeGb 253 is too large for a pd-balanced data disk: … the largest diskSizeGb that fits is
  252`;
- no `tailnetAuthKey`;
- `subnetwork` without `network`, since the firewall rule needs the network;
- no `users` and no `listener`, or a user refused as [The hub's users](#the-hubs-users) lists;
- a `listener` whose `image` is not pinned as above, whose `seedSecret` is not a secret id
  (`^[A-Za-z0-9_-]{1,255}$`, Secret Manager's rule, so a resource path such as
  `projects/<p>/secrets/<s>` is refused) or is the hub's own `<h>-tailnet-auth-key`, or whose
  `nkey` is empty. The hub also refuses a `listener.senderMessagesPerMinute` outside 1 to 60000,
  a `listener.senderMessageBurst` below 1, and a `listener.senderMessageBurst` with no rate, as
  `listener.senderMessageBurst is given without listener.senderMessagesPerMinute, and the listener
  refuses a burst with no rate when it boots`
  ([The listener's sender limit](#the-listeners-sender-limit));
- a `listener` on a hub with no `tailnetDnsName`, since the hub renders the address its listener
  answers on from it, as `tailnetDnsName is required for a hub with a listener: …`. Pulumi
  refuses the hub while it previews it, before anything is registered;
- a `listener.allowedHosts` entry that is not a plain host name or IP address, named by its own
  index in that list, as `listener.allowedHosts[0] is "hub.internal.example:9020": want a host
  name or address with no port, scheme or path`: an entry carrying a port, a scheme, a path or a
  wildcard such as `*.example-tailnet.ts.net`. The listener refuses every such entry in its own
  `ENVOY_ALLOWED_HOSTS` when it boots, so a hub whose listener could not serve is refused before
  anything registers; the plugin additionally refuses an entry that is empty or carries
  surrounding whitespace, which the listener would have trimmed and served, since the entry is
  rendered into a comma-joined `docker run` argument where a space would split it;
- a `listener` broker check that sets one or two of `agentSecretsUrl`, `agentSecretsAudience` and
  `publicUrl` and not all three, naming the input left out and the one that asks for it, as
  `listener.publicUrl is required when listener.agentSecretsUrl is set: …`. The listener reads
  the three as one set and refuses a partial one when it boots, so the hub refuses it while
  Pulumi previews, rather than deploying a VM whose listener will not start;
- a value of that broker check carrying whitespace or a control character, as
  `listener.agentSecretsUrl holds whitespace or a control character, which
  ENVOY_AGENT_SECRETS_URL cannot carry (not echoed)`: each is rendered as one `docker run`
  argument, which a space would split, and the listener trims what it reads, so a value that is
  whitespace alone is no value at all there;
- a `sessionUser` whose `nkey` is empty;
- a missing provider (see [Providers](../README.md#providers)).

## The hub's users

`users` holds the NATS server's nkey users, and `listener` and `sessionUser` add one each. Each of
`users` is `name`, `nkey` and exactly one of:

- `machineId`: the user of one host, whose listener and sessions all connect as it, granted
  `HostPermissions(machineId)` (see [Permission sets](../README.md#permission-sets)). Give it the
  listener's own `machineId`: the set names that machine's durable consumer and queue group.
- `listenerMachineId`: the user of one listener alone, which no session shares, granted
  `ListenerHostPermissions(listenerMachineId)`: the listener's own set, the session consumers'
  calls included, with a plain `notifications.>` subscribe nothing uses any more. Give it that
  listener's `machineId`. Use it where the listener's process is the only one holding the seed,
  so it keeps its sessions' consumers; a host whose sessions connect as the same user takes
  `machineId` instead, since a session holding this set could create, pull and delete any
  session's consumer.
- `dispatchMachineId`: the user shared by `envoy-dispatch` and the listener beside it, granted
  `DispatchPermissions(dispatchMachineId)`. Give it that listener's `machineId`, since the set
  names its durable consumer and queue group.
- `daemon: true`: the dedicated user of one Legion daemon, granted `DaemonPermissions()` (see
  [Permission sets](../README.md#permission-sets)). Its seed belongs only to that daemon.
- `permissions`: `{publish, subscribe}`, each a `nats:Rules`, an `allow` list and an optional
  `deny` list of subject patterns, for any other client.

`daemon: false` selects no permission set. `nats:Rules` is the one rules type every NATS component
takes, whatever its cloud, and `nkey` is named as `aws:NatsService`'s users name it, so the two
components spell a user's key and rules alike.

`ListenerPermissions`, and through it `ListenerHostPermissions`, `HostPermissions` and
`DispatchPermissions`, holds the notification stream's stored-message get,
`$JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS`: a read-only read of one stored message at a time.
It can create, change or delete nothing. `SessionPermissions` does not hold it, so no session
reads the stream's stored messages itself; each takes its own from the consumer its listener
relays. The same sets hold, for each key-value bucket
they open, the update of the bucket's stream, `$JS.API.STREAM.UPDATE.KV_<bucket>`, beside its info
and create: a start, and the program's keeper every 30 seconds after, raises a bucket with fewer
copies than `ENVOY_NATS_REPLICAS` with that update. This hub is one server, which refuses a
replica count above 1, so the programs that use it run with `ENVOY_NATS_REPLICAS` unset or 1 (the
hub's own listener sets none), and at 1 neither sends a raise.

The buckets those sets grant are each one `bus.ListenerBuckets()` names, `envoy_interests`,
`envoy_roles`, `envoy_sessions`, `envoy_known_sessions`, `legion_agents`, `envoy_ci_state` and
`envoy_dpop_proofs`, with `envoy_webhook_redelivery` for `DispatchPermissions` alone.
Each is granted to every one of these users, and
[Permission sets](../README.md#permission-sets) says what that lets a session on a host write.
A listener whose user's grant does not yet carry `envoy_known_sessions` runs without that bucket
until the grant reaches it ([Permission sets](../README.md#permission-sets)).

The listener's send route asks whether a session's plugin still holds a NATS connection with one
core request on the session's relay subject, `notifications.envoy.deliver.<session id>`, answered
on the request's own reply inbox.
It needs no rule of its own; [Permission sets](../README.md#permission-sets) names the entries
that carry it.

The sets of a user no session shares, `ListenerPermissions` and `ListenerHostPermissions`, alone
hold the session consumers' calls on every consumer of the stream (`$JS.API.CONSUMER.CREATE`,
`INFO`, `DELETE` and `MSG.NEXT` on `ENVOY_NOTIFICATIONS.*`, the create with a filter subject on
`ENVOY_NOTIFICATIONS.*.>`, `$JS.ACK.ENVOY_NOTIFICATIONS.*.>`, and the stream's consumer list
`$JS.API.CONSUMER.LIST.ENVOY_NOTIFICATIONS`, which a listener that starts relaying every session
at once reads instead of one info each), with which a listener keeps one pull consumer per
session. A NATS wildcard is a whole token, so those entries reach every consumer of the stream,
other listeners' and Dispatch's included, which is why a user any session shares holds none:
`HostPermissions` and `DispatchPermissions` keep their per-durable scope. A listener connecting as
a `machineId` or a `dispatchMachineId` user therefore keeps its own durable alone and refuses
every registration that asks for consumer delivery `503 no_grant`, so a session registering
through it is not registered at all and receives nothing until its listener's grant carries the
session consumers. Every set above subscribes to the role lanes in the queue group every listener
shares, `envoy-roles`, beside the machine's own, since each user runs a listener.

`nkey` is the public key `envoy-infra seed generate` prints, and the seed stays in the secret
store it wrote (see [NATS seeds](../README.md#nats-seeds)): a host's listener reads it from there
into its `seed` with the hub's `natsUrl` as its `natsUrls`, and a Legion daemon reads its own. In the authorization `core`
renders (see [NATS authorization](../README.md#nats-authorization)), each of `users` is a keyed
user with `HasSeed` set. The listener's user follows them, named `<h>`, with `listener.nkey` and
`ListenerPermissions(<h>)` and `HasSeed` set. `sessionUser` comes last, with
`SessionPermissions()` and no seed, as the authorization's `noAuthUser`: a client that presents no
credential becomes that user, and one that presents a key the server does not know is refused.
Without `sessionUser`, no user stands for a client presenting no credential, so the server refuses
every client without the seed of one of its users. Give `sessionUser.nkey` a public key whose seed
nobody keeps (`nk -gen user -pubout`, keeping only the `U…` line): no client presents it, and a
client holding its seed would only be the same session user. The rendered file, headed
`# Rendered by envoy-infra. Public keys only.`, is `/etc/nats/nats-server.conf` on the VM; the
VM's metadata holds it in the clear.

A session user lets anything that reaches 4222 publish and subscribe as `SessionPermissions()`
allows: it reads the relay subject of any session, so it can take another session's messages and
acknowledge them away, and it publishes agent stream frames and answers stream replay requests
for any session id, so it can fake any agent's live stream in Dispatch. Beyond the relay
subjects and the agent stream's control subject it reads no notification topic at all. With one,
the tailnet's policy is the gate on 4222: which devices it lets reach the hub's 4222 decides who
is on the bus.
A `listener` opens a second door, port 9020, with or without a session user, and it grants far
more ([The hub's listener](#the-hubs-listener)).

A user is refused when it names more than one of `machineId`, `listenerMachineId`,
`dispatchMachineId`, `daemon: true` and `permissions`, or none, or when its `nkey` is empty, named
by its index alone, as `users[1]: nkey is required`, since these come before the authorization has
checked the names. A `machineId`, `listenerMachineId` or `dispatchMachineId` names the durable
consumer `listener-<id>` and queue group `envoy-listener-<id>`. The shared machine-ID rule refuses
an empty id; leading or trailing whitespace; `.`, `*`, `>`, `/` or `\`; a control character; and
any whitespace other than an ordinary space; and any id over 246 bytes, so `listener-<id>` fits
NATS's 255-byte consumer-name limit. An ordinary space is accepted only inside the id and becomes a hyphen in both names. The refusal names the field itself, as
`users[1].dispatchMachineId contains '.', which the listener's NATS names or the hub's grant entries cannot carry`. Two ids that give one durable name, of any kind
(`a b` and `a-b` collide), or one giving the hub's own listener's (the `hostname`'s), would put
two listeners on one durable and queue group, so the later one is refused, as
`users[2]: dispatchMachineId gives the same durable name as users[0].machineId` or
`… as the hub's own listener`. Every `dispatchMachineId` user also binds Dispatch's fixed durables
`delivery-events` and `delivery-jobs`, so a second one is refused the same way, as
`users[3]: dispatchMachineId gives the same durable name as users[2].dispatchMachineId`. So a
machine's credential is rotated in its single entry, never beside it: the consumer declares a new
secret, empty (see [Making a seed](../README.md#making-a-seed)); `envoy-infra seed generate` writes
the new seed into it (it refuses a secret that does not exist or already holds a version); the
entry's `nkey` becomes the key it printed; and the client is pointed at the new secret and
restarted. A change to the hub's users rewrites its user data, which replaces the VM, and from the
`nkey` change until its own restart the client is refused. A user is refused too for each of the
authorization's own refusals under [NATS authorization](../README.md#nats-authorization), named as
`users[<index>] "<name>"`: a name of another form or one two users share, an `nkey` that is not a
user public key (a seed included), or an empty `allow`. The authorization refuses a seed in a
user's name or a rule entry before any of those, naming the path and not the value, as
`users[1]: name holds an nkey seed (not echoed)` or
`users[1] "example-operator": publish.allow[0] holds an nkey seed (not echoed)`. The listener's
user and the session user are named by their inputs in the same refusals, as
`listener "<h>": nkey repeats users[0] "example-host-a"` or
`sessionUser "<name>": nkey is not a user public key …`; a user of `users` named `<h>` is refused
as the listener's user repeating it, as `listener "<h>": name repeats users[<index>]`.

### The auth service

With `authCallout`, the server admits every client that is not one of its users through the auth
service (dispatch://INFRA-840), which reads the client's token and answers with that connection's
grant, as [NATS authorization](../README.md#nats-authorization) renders it: one `auth_callout`
block inside the authorization block, whose `auth_users` is the key of every user above, the
session user's included (a hub with `authCallout` has no listener user, below). After the
authorization block come the ping lines, `ping_interval: "20s"` and `ping_max: 2`, so a hung
replica of the auth service leaves the callout's queue group within 80 s. A static user
connects by its seed and the session user's client with no credential, and neither reaches the
callout. The authorization block sets no `timeout`, so nats-server's
default authorization timeout of 2 s stands, and the auth service must answer within 1.5 s:
whatever the timeout, a Go client (nats.go) fails at about 2 s, since the server sends it its first
PING after 2 s plus up to 20% (nats-server's server/client.go `setFirstPingTimer`) even while its
CONNECT still waits for the callout, and nats.go reads that PING as an error (the README's
[NATS authorization](../README.md#nats-authorization) gives the measurement). `authCallout.xkey`
is required: without it nats-server sends each connecting client's token to the auth service
unsealed, on `$SYS.REQ.USER.AUTH`. The service connects as `authCallout.serviceUser`, an entry of
`users` granted, as `permissions`, publish `$SYS._INBOX.>` and subscribe `$SYS.REQ.USER.AUTH`;
with `privateProjects` the hub enforces it, refusing a service user of any other kind and one
granted more (below). There is no `accounts` block: the service places each client in the global
account, beside the static users, so turning the callout on moves no stream.

A hub with `authCallout` runs no `listener` of its own yet: the listener connects as a user whose
seed the hub stores, and its service identity, with which it would connect through the auth
service instead, arrives with dispatch://INFRA-840 (its task t-e-13). Until then `listener` beside
`authCallout` is refused naming both, as `listener and authCallout: …`, before anything registers.

`privateProjects` says the deployment may make a Dispatch project private, which R12 of
dispatch://INFRA-3862 allows only while every client connects through the auth service. So it is
refused, as a `core.InputError` naming `privateProjects`, without `authCallout`
(`privateProjects: a project can be private only while the server admits each client through the
auth service, and the authorization has no callout`); beside a `sessionUser`
(`privateProjects: sessionUser "<name>" is noAuthUser: a client presenting no credential would
become it while a project is private`); beside any entry of `users` but the service's
(`privateProjects: users[<i>] "<name>": a stored client seed while a project is private`); with an
`authCallout.serviceUser` that names an entry of `users` other than a `permissions` entry
(`privateProjects: authCallout.serviceUser "<name>" names users[<i>], a
<machineId|listenerMachineId|dispatchMachineId|daemon> user, whose seed its client stores; the auth
service connects as a permissions user of its own`); and with a service user granted more than
the auth service's own grant, which subscribes `$SYS.REQ.USER.AUTH`, alone or with a queue group,
and publishes only its replies, under `$SYS._INBOX.` (`privateProjects: callout.serviceUser
"<name>": <publish|subscribe>.allow[<i>] "<entry>" is wider than the auth service's grant, which
subscribes $SYS.REQ.USER.AUTH and publishes only its replies under $SYS._INBOX.; with a wider grant
the service user is a client that connects past the auth service`). A refusal of the callout
itself, such as an `issuer` that is no account public key, a missing `xkey` or a `serviceUser`
naming no user, names `authCallout`, as `authCallout: callout.issuer is not an account public key
…` or `authCallout: callout.xkey is required: …`. A seed in any of its fields is refused naming the
field by the hub's path, as `authCallout.issuer: an nkey user seed, which is never an input of the
hub (not echoed)`.

nats-server's monitoring port, 8222, answers `/connz`, `/subsz` and `/jsz` with no authentication,
and they name every client's subjects, connections and consumers, a private project's among them.
So `gcp:ClusteredNatsHub` refuses `monitorNamespaces` beside `privateProjects`, as
`monitorNamespaces and privateProjects: nats-server's monitoring port, 8222, answers /connz, /subsz
and /jsz with no authentication, …`, and a private hub's NetworkPolicy admits 8222 from no pod. Its
chart's headless Service, `<n>-headless`, also lists 8222, but it adds no reach: it has no cluster
address (`clusterIP: None`), so its name resolves to the pods' own addresses, and the NetworkPolicy
on those pods admits only the ports its rules name, whatever name a caller dialled. No reader of the
hub needs 8222 from another namespace: the kubelet probes from the pod's own node, which the
NetworkPolicy does not fence; the exporter in each pod reads `localhost:8222`; `kubectl
port-forward` and `exec` reach a pod through the API server; and Managed Prometheus reads the
exporter's port, 7777. `gcp:NatsHub` opens 8222 only to Google Cloud's health-check probe ranges,
`130.211.0.0/22` and `35.191.0.0/16` (its firewall `<h>-health-checks` and the VM's own iptables
rule), from which its group's health check reads `/healthz`; no input widens that, so it has
nothing to refuse.

`privateProjects` is checked when the hub is planned, and renders nothing itself. What it demands
(the callout, no session user, no client user beside the auth service's) reaches a running server
only when that server restarts. On `gcp:ClusteredNatsHub` each pod takes it as the pod roll
restarts that pod (below), and until then it keeps its old `no_auth_user` and seeded users. On
`gcp:NatsHub` the VM takes it when the group replaces the VM. So the order holds both ways. Turn
on Dispatch's `DISPATCH_PRIVATE_PROJECTS` only after the hub's update has completed on every server:
every pod of the clustered hub has restarted, or the VM hub's new VM is up. Turn the hub's
`privateProjects` off only after no project is private.

## The hub's listener

With `listener`, the VM runs Envoy's listener beside NATS, as machine `<h>`. It listens on the
VM's loopback, `127.0.0.1:9020`, and reaches NATS there too, on `nats://127.0.0.1:4222`. Tailscale
Serve forwards the tailnet's connections to the hub's port 9020 to that address. A self-subscribed
client such as pi-envoy or claude-envoy registers and sends through
`http://<h>.<tailnet>:9020`; it receives directly from NATS, with no listener of its own. A client
that registers a port is refused: the listener answers 400 naming `port`, since it pushes to no
session's port. Register with no port and receive from the session's own NATS consumer. The
listener's `/v1` API takes no bearer, which the listener allows on a loopback address, or off
loopback where `ENVOY_API_ALLOW_UNAUTHENTICATED=1` says something else fences the port, as the
clustered hub's tailnet-only session listener sets it (`packages/envoy/cmd/listener/apiauth.go`).
On this VM the listener is on loopback, so the tailnet's policy for port 9020 is the only thing that
decides who may call it; on the clustered hub the listener's own NetworkPolicy is a second fence
there, and a clustered hub listener that pods of the cluster reach requires a token instead
([The session listener](#the-session-listener)).
Either way a caller that gets through gets the whole API with no credential: it can publish to any
topic outside the agent topics with a `source` of its choosing, under the listener's user's
publish grant (Dispatch, GitHub, Slack, Legion and role notifications included), claim a role,
list every session and delete one, and read any session's row by naming its id (the listener
does not check that the caller is that session). That holds whether or not the hub has a
`sessionUser`, so a hub without one still has an open bus for any device the policy lets reach
9020: grant 9020 only to devices that may act as any agent.

Because the API takes no bearer, a web page open on any device the policy lets reach 9020 could
otherwise post to it: a browser sends a cross-site form without asking. The listener refuses any
request body whose `Content-Type` is not `application/json`, which a page can send cross-site only
after a CORS preflight the listener never grants (`apiauth.go`). That refusal shipped in
legion-envoy-v13.9.0, so pin `listener.image` to that release or a later one; an older image
leaves 9020 open to any page a tailnet user visits. The `Content-Type` refusal alone would not
stop DNS rebinding, so the listener checks the request's `Host` too: a listener whose `/v1` takes
no bearer and no OIDC verifier serves loopback and the names in `ENVOY_ALLOWED_HOSTS` alone,
which the hub renders from `tailnetDnsName` and `listener.allowedHosts`, and answers every other
`Host`, an empty one included, with 421 (`packages/envoy/cmd/listener/apiauth.go`, `apiAuth` and
`servesHost`; `/healthz`, `/metrics` and the `/webhook/` routes are exempt, and `/livez` never
reaches the check). A page whose own name an attacker repoints at the hub's tailnet address
sends that name as the `Host`, so it is refused with the rest. A client that dials the hub by any
other address, its bare MagicDNS label `<h>` or its tailnet IP among them, is refused the same
way unless that address is in `listener.allowedHosts`. The guard shipped in
legion-envoy-v13.11.1, so an image older than that release checks no `Host` at all, and on such
an image a rebinding page still has the whole API.
A browser that asks before a public page reaches a private or tailnet address (Chrome's Local
Network Access) blocks that; any other browser on a device the policy admits to 9020 is a way in,
which is one more reason to grant 9020 narrowly.

The deployment that builds this hub hands its clients `http://<h>.<tailnetDnsName>:9020`, the
address the hub renders into `ENVOY_ALLOWED_HOSTS`, so those clients are served as they stand.
Whoever moves a hub's listener to an image carrying the guard should check what its clients
actually dial before the move — a bare MagicDNS label, a tailnet IP address, a name of their
own — and put everything but the hub's own `<h>.<tailnetDnsName>` in `listener.allowedHosts`:
an address left out of both is answered 421 from the moment the new VM boots.

The listener connects as the listener's user (see [The hub's users](#the-hubs-users)) with the
seed `listener.seedSecret` holds. The hub's service account gets
`roles/secretmanager.secretAccessor` on that secret and nothing more; the consumer declares the
secret and writes the seed with `envoy-infra seed generate`, whose printed key is `listener.nkey`
(see [Making a seed](../README.md#making-a-seed)). The seed reaches the listener's environment as
`NATS_NKEY_SEED` through `docker run --env-file`, which Docker reads as root, since the listener's
image runs as an unprivileged user.

### The listener's broker check

`listener.agentSecretsUrl`, `listener.agentSecretsAudience` and `listener.publicUrl` are what
this listener needs to check a caller's broker access token, and the hub renders each, as
written, into the listener's `docker run` command: `ENVOY_AGENT_SECRETS_URL`,
`ENVOY_AGENT_SECRETS_GOOGLE_AUDIENCE` and `ENVOY_PUBLIC_URL`, in that order, after the variables
above.

- `agentSecretsUrl` is the secrets broker, the origin whose introspection route the listener asks
  about each token it is presented.
- `agentSecretsAudience` is the audience this VM's own Google identity token is minted for, read
  from the metadata server at call time: it is the identity each introspection presents, which is
  how the broker knows which service is asking. This hub is a VM, so it is the VM's answer; the
  projected Kubernetes token a listener in a pod presents instead is no input here, since the
  listener refuses that file beside an audience.
- `publicUrl` is the origin a caller reaches this listener's `/v1` at, such as
  `http://<h>.<tailnetDnsName>:9020`. A DPoP proof's target URI is checked against it and the
  request's path, never against a forwarded header, so a caller behind a proxy cannot choose the
  URL its proof names.

The listener reads the three as one set: with none of them it takes no broker token at all, and
with any of them it requires all three. So the hub takes them the same way and refuses a partial
set while Pulumi previews, naming the input left out. A hub that sets none of the three renders
the cloud-config it rendered before these inputs existed, byte for byte, so they replaced no
running hub's VM.

### The listener's sender limit

`listener.senderMessagesPerMinute` and `listener.senderMessageBurst` limit how many messages one
agent session sends through this listener, on `/v1/messages/send` and `/v1/messages/publish`
together. The hub renders each into the listener's `docker run` command only when it is set:
`ENVOY_SENDER_MESSAGES_PER_MINUTE` and `ENVOY_SENDER_MESSAGE_BURST`, in that order, after
`NATS_URLS` and before the broker check's variables. The rate is a whole number from 1 to 60000,
and unset it sets no limit. The burst is at least 1 and only with a rate, and unset it equals the
rate.

The listener keeps one count for each sending session. The count starts at the burst and gains
one message every 60 seconds divided by the rate, up to the burst. The counts are the listener
process's own, so a restart of the listener, or a new VM, fills each count again. The sender is
the session that a secrets-broker token proves, else the request's `source_session`. A message
with another `source`, such as `dispatch` or `envoy`, is not counted, with a secrets-broker token
or without one. Neither is a message with no sender, which is how the Legion daemon sends its
notices.

A message that finds its sender's count at 0 is answered HTTP `429`. The listener does not store
it, keep it for a later holder of a role or report a wake for it. `Retry-After` gives the whole
seconds, rounded up and at least 1, until the count holds one message again. The JSON body holds
`reason: "sender_rate_limited"`, `retry_after_seconds`, `messages_per_minute`, `burst`, and an
`error` that is the sentence `Your sending is rate limited. This message was not sent. This
listener lets one session send <rate> messages a minute and <burst> at once. You can send it again
after <seconds> seconds.` with the values filled in. Where a value in the sentence is 1, its noun
is singular: `1 message a minute`, `after 1 second`. The request's log line has the outcome
`sender_rate_limited` and the field `sender`.

A counted message costs one when the listener stores it, may have stored it (any answer after an
earlier forward of the same role send, or a `503` after the listener sent the message to the
stream and the write's acknowledgement did not arrive in time) or reports a wake for it. A role
send's `504` costs one whether or not a forward went out, also when the window ended before the
first forward. The listener does not tell a `504` that forwarded nothing from one whose forward
the stream may hold. A refusal before any store or wake report, such as a `400`, `403`, `413` or
a `503` before the listener sent anything (one while it waits for NATS to reconnect), costs
nothing. A repeat the listener recognises costs nothing too: the stream's `duplicate` answer, a
repeat of a role message kept for the role's next holder, and a repeat of a role forward the route
made in the last 10 minutes. So does an echo, a role message from the session that holds the role.
A `duplicate` that answers the listener's own second send, made after its connection to NATS
closed under the first, costs one, since the first may have stored the message. A role send's
echo, repeat or `403` costs nothing only while the send has made no forward, since an earlier
forward may have stored the message. A repeat that finds the count at 0 is refused as a new
message is. The clients show the sentence and do not send the message again by themselves, except
the Legion stall notice, whose sender waits the seconds and sends it again at most 3 times
([The listener's sender limit](../README.md#the-listeners-sender-limit)).

## What the hub's VM runs

The VM boots Container-Optimized OS (COS), which mounts `/` read-only and keeps `/etc` and
`/mnt/disks` on a tmpfs that does not outlive a boot
([COS file system](https://cloud.google.com/container-optimized-os/docs/concepts/disks-and-filesystem)),
so cloud-init applies the hub's cloud-config at every boot. The whole document, for the hub of
[A NATS hub, in YAML](#a-nats-hub-in-yaml), is `gcp/testdata/cloud-config.golden` in this module.

`bootcmd` runs first, in this order:

1. It loads the `tun` module unless `/dev/net/tun` exists, and makes the mount point,
   `/mnt/disks/data`.
2. It mounts the data disk, `/dev/disk/by-id/google-data`, formatting it first only when it is
   proven empty end to end: `blkid -p` finds no filesystem on it (exit status 2), and GNU `cmp`,
   which COS ships, reads the whole device, as many bytes as `blockdev --getsize64` reports,
   against `/dev/zero` and finds only zeros. cmp stops at the first byte that is not zero. A disk
   blkid finds a filesystem on is not read: it is checked with `fsck.ext4 -p` and mounted when fsck
   left no error it could not fix (exit status below 4). Every other disk is left unmounted and
   unwritten. Among them is an ext4 disk whose first MiB or primary superblock was zeroed: blkid
   finds no filesystem on it, yet it still holds its files' blocks, which `mkfs.ext4`'s discard
   would destroy, and a damaged superblock alone `e2fsck` repairs from a backup.
3. A disk left unmounted gets one line, at every boot that leaves it, naming the device and the
   reason and nothing the disk holds, as
   `envoy-hub: data disk /dev/disk/by-id/google-data left unmounted: blkid finds no filesystem on it, and it does not read as zeros to its end; see docs/nats-hub.md, "Repairing the data disk"`.
   The other reasons are `blkid -p exited <n>`, `blockdev --getsize64 exited <n>` or
   `cmp exited <n> reading it` after blkid found no filesystem, `mkfs.ext4 exited <n>`,
   `fsck.ext4 -p exited <n>` and `mount exited <n>`. An unmounted disk keeps NATS and Tailscale
   down, since their units refuse to start without it, so the group recreates the VM on the same
   disk, again and again, and no boot formats it: see
   [Repairing the data disk](#repairing-the-data-disk).
4. It makes `nats/` and `tailscale/` under the mount point, `/run/envoy`, mode 0700, and an empty
   `/run/envoy/tailnet-auth-key`, mode 0600, so the key's bind mount into the Tailscale container
   is always of a file.

`write_files` then writes the authorization to `/etc/nats/nats-server.conf`, the secret fetcher to
`/etc/envoy/fetch-secret`, the joined script to `/etc/envoy/tailnet-joined`, and Tailscale Serve's
configuration to `/etc/tailscale-serve/serve.json`. With a `listener`, it also writes the seed
script to `/etc/envoy/listener-seed-env`, and the Serve configuration forwards port 9020; without
one, the configuration is `{}`, which clears a forward from a previous VM. It then writes the
systemd units to `/etc/systemd/system/`, and `runcmd` reloads systemd and starts the units in this
order. They are started and never enabled, since `/etc` does not outlive the boot.

| Unit | What it does |
| --- | --- |
| `fetch-tailnet-key.service` | A oneshot, after `network-online.target`, that stays active once it exits: `/etc/envoy/fetch-secret /run/envoy/tailnet-auth-key <h>-tailnet-auth-key`. It runs only on a mounted data disk without `/mnt/disks/data/tailnet-joined`, so no boot after the node has joined fetches the key. |
| `tailscaled.service` | `docker run --name tailscaled` of `tailscaleImage` on the host's network, with `NET_ADMIN` and `/dev/net/tun`, its state in `/mnt/disks/data/tailscale`, the key file mounted read-only, `TS_AUTHKEY=file:/run/envoy/tailnet-auth-key`, `TS_AUTH_ONCE=true`, kernel networking (`TS_USERSPACE=false`), `TS_HOSTNAME=<h>` and `--advertise-tags` with `tags`. It mounts `/etc/tailscale-serve` read-only and sets `TS_SERVE_CONFIG=/etc/tailscale-serve/serve.json`: containerboot applies that configuration once the node is up, and again whenever the file changes, watching its directory (tailscale v1.104.1 `cmd/containerboot/serve.go`). With a `listener`, the configuration forwards port 9020; without one it is `{}`, which clears a prior forward. It wants the fetch and starts after it ends, but does not require it. |
| `tailnet-joined.service` | Started after `tailscaled`, on a mounted data disk without the marker: `/etc/envoy/tailnet-joined /mnt/disks/data/tailnet-joined /run/envoy/tailnet-auth-key`. Every 5 seconds the script asks the Tailscale container's CLI for `status --json`, and once its `BackendState` is `Running` it empties the key file, which stays for the container's bind mount, and only once that has succeeded writes the marker. A simple service, so its start returns at once; a run that fails, as one whose key drop fails does, is run again 10 seconds later. |
| `nats-firewall.service` | A oneshot that stays active once it exits: it adds `INPUT -p tcp -s 130.211.0.0/22,35.191.0.0/16 --dport 8222 -j ACCEPT` and `INPUT -i tailscale0 -p tcp --dport 4222 -j ACCEPT` to the host's iptables with COS's own `/sbin/iptables`, each unless it is there. COS's host firewall accepts incoming connections for SSH alone ([COS firewall](https://cloud.google.com/container-optimized-os/docs/how-to/firewall)): it filters with nf_tables and drops INPUT by default. So without the first rule the health check never reaches NATS, and without the second the tailnet never does either, since Tailscale's image writes its own accept rule for `tailscale0` with iptables-legacy, which does not admit what nf_tables drops. |
| `nats.service` | `docker run --name nats` of `natsImage` on the host's network, with `/mnt/disks/data/nats` as `/data` and the configuration file mounted read-only, and the arguments `-c /etc/nats/nats-server.conf -js --store_dir /data -m 8222 --server_name <h>`. It is stopped with `docker stop -t 30`. It wants `nats-firewall` and starts after it. |
| `envoy-listener.service` | Only with a `listener`. Before each start, `/etc/envoy/listener-seed-env /run/envoy/listener.env <seedSecret>` writes the seed to that env file; then `docker run --name envoy-listener` of `listener.image` on the host's network, with `--env-file /run/envoy/listener.env`, `PORT=9020`, `ENVOY_LISTEN_HOST=127.0.0.1`, `ENVOY_ALLOWED_HOSTS=<h>.<tailnetDnsName>` and each of `listener.allowedHosts` after it, comma-separated, `ENVOY_MACHINE_ID=<h>` and `NATS_URLS=nats://127.0.0.1:4222`, in that order, as `gcp/testdata/cloud-config.golden` shows. `ENVOY_SENDER_MESSAGES_PER_MINUTE` and `ENVOY_SENDER_MESSAGE_BURST` follow them, each only when its input is set ([The listener's sender limit](#the-listeners-sender-limit)). `TimeoutStartSec=600` exceeds the fetcher's 590-second maximum: six attempts, each with three 30-second curl limits, separated by five 10-second waits. A seed that cannot be fetched fails the start, and the restart fetches again. It is stopped with `docker stop -t 30`, since the listener's own shutdown takes up to about 20 seconds. It wants `nats` and starts after it. |

The container units require `docker.service`, refuse to start unless `/mnt/disks/data` is a mount
point, so that their state never lands on the tmpfs beneath, and restart 10 seconds after their
container exits, or after their start fails.

The secret fetcher runs under `/bin/sh` with curl, jq and base64. It takes the VM's project id and
an access token for its service account from the metadata server, reads the secret's `latest`
version through Secret Manager's REST API, and moves the decoded value onto the file it is given
from a temporary file readable by root alone, only when the value is not empty. A new grant can
take a while to reach Secret Manager, so it tries six times, ten seconds apart, and exits 1 after
the sixth failure. Nothing of the value or the token is printed or passed in a command's
arguments. `GCE_METADATA_HOST` and `SECRET_MANAGER_ENDPOINT` replace the metadata server's host
and Secret Manager's base URL, which its tests use. The seed script fetches the listener's seed
with it into a temporary file, refuses a seed of more than one line, which would add a variable of
the secret's choosing to the listener's environment, and writes `NATS_NKEY_SEED=<seed>` to
`/run/envoy/listener.env`, readable by root alone, removing the fetched file.

NATS listens for clients on 4222 and serves its monitoring endpoints on 8222, on every interface.
The hub's VPC firewall rule and `nats-firewall` admit Google's health checks to 8222, and
`nats-firewall` admits the tailnet's interface, `tailscale0`, to 4222; the hub opens nothing else. So
the tailnet's policy decides which peers reach 4222. 8222's `/varz`, `/connz` and `/jsz` stay
closed to the tailnet, as does every port that neither `nats-firewall`, Serve nor COS's own SSH
rule (tcp/22, accepted on every interface) opens, since Tailscale's own accept rule for
`tailscale0` does not admit what COS's nf_tables policy drops (see
[What a first Google Cloud deployment proves](#what-a-first-google-cloud-deployment-proves)). The
listener's port 9020 needs no host firewall rule: tailscaled takes a connection to a port Serve
publishes before the host's iptables sees it, and opens its own connection to `127.0.0.1:9020`.
On a Linux host with tailscaled 1.104.1 in kernel mode, a port Serve published still answered
over the tailnet while iptables dropped that port on `tailscale0`, and a port Serve did not
publish timed out. Serve publishes nothing else.

## The VM's external address

The template gives the VM an ephemeral external address, and the hub has no input that drops it.
The address is the VM's way out: to the registries `natsImage` and `tailscaleImage` are pulled
from, and to Tailscale's coordination and relay servers. A VM without one reaches the internet only
through [Cloud NAT](https://cloud.google.com/nat/docs/overview) on its subnetwork's region, which
a consumer's network need not have.

The address is also a way in. Besides the hub's own rule, the VM takes every ingress rule of
`network` that applies to all of the network's instances. The default network has four
([pre-populated rules](https://cloud.google.com/firewall/docs/firewalls)): `default-allow-ssh`
(tcp:22 from `0.0.0.0/0`), `default-allow-rdp`, `default-allow-icmp` and `default-allow-internal`
(every port from `10.128.0.0/9`). COS's host firewall then accepts SSH and nothing else that the
hub has not opened, so on such a network the VM's sshd is reachable from the internet. To avoid
that, leave `network` unset, so the hub creates a network holding only its own rule, or name a
network with no ingress rule from outside it that applies to every instance. The hub offers no
way to run with Cloud NAT and no external address: a consumer whose VMs must have no external
address can't use this version of `gcp:NatsHub`.

## The hub's data disk

The template's second disk has the device name `data` and outlives its VM (`autoDelete: false`),
and the group holds it as a stateful disk it never deletes (`deleteRule: NEVER`). Google Cloud
keeps a stateful disk through autohealing, updates and recreation and reattaches it to the new VM,
and when the group itself is deleted it detaches the disk and leaves it
([preserved state](https://cloud.google.com/compute/docs/instance-groups/preserved-state)). The
disk holds both containers' state:

- `/mnt/disks/data/nats`, the NATS container's `/data`: JetStream's store, the stream and the
  key-value buckets;
- `/mnt/disks/data/tailscale`, the Tailscale container's `/var/lib/tailscale` (`TS_STATE_DIR`):
  the node's keys and its login.

Keeping the tailnet state there is what lets a recreated VM rejoin the tailnet as the same node,
under the same name, without logging in again. With `TS_AUTH_ONCE=true`, containerboot logs in
with `TS_AUTHKEY` only when the state holds no login (tailscale v1.104.1
`cmd/containerboot/main.go:583-682`), so the VM logs in with `tailnetAuthKey` at its first boot
alone. Once the node runs, `tailnet-joined` empties the key file, then writes
`/mnt/disks/data/tailnet-joined`, and no later boot fetches the key: it is on the VM no longer
than the first login needs. A drop that fails writes no marker, so the unit tries again this boot
and every later boot fetches and drops the key again until a drop succeeds.
The marker is not tailscaled's own state file, which tailscaled writes before it logs in (tailscale
v1.104.1 `ipn/ipnlocal/local.go:3185-3190`), so a first boot whose login failed fetches the key
again. The same is why `tailscaled.service` does not require the fetch: on a later boot the key file
stays empty, and the node comes up from its state. A node removed from the tailnet therefore does
not log in again; to have it join anew, put a new key in `tailnetAuthKey`, run `pulumi up`, delete
`/mnt/disks/data/tailnet-joined` on the VM and restart it.

The group reattaches the same disk to every later VM, so a later `diskSizeGb` or `diskType`, which
changes only the template, should leave the disk at the size and type it was created with. Google
Cloud's documentation implies this without saying it, and a first deployment confirms it (see
[What a first Google Cloud deployment proves](#what-a-first-google-cloud-deployment-proves)).
Keep both at the disk's own values: the group's initial delay is derived from them. Deleting the
hub leaves the disk, detached, in `zone`, as the preserved-state page documents, and leaves the
hub's node on the tailnet; remove both yourself once you no longer need them.

### The first boot's read

A boot on a disk blkid finds no filesystem on reads the whole disk before it formats it: the
first boot does, on a new disk, and so does every boot on a disk it leaves unmounted. A boot on
the hub's own ext4 disk reads nothing. The group's initial delay, the time it waits after creating
a VM before the health check counts, is derived so that the read fits, in seconds:

```text
initialDelaySec = 300 + ceil(1.25 × (S + S/64 + 1024) / R)
```

`S` is the disk's size in MiB, `diskSizeGb` × 1024 (taking GB as GiB, the larger reading), and `R`
the read rate assumed for `diskType`, in MiB/s. 300 seconds cover the boot and the two image
pulls. `S` is the read, and `S/64` and 1024 MiB are what `mkfs.ext4` writes, its inode tables (256
bytes per 16 KiB, all written at once) and at most 1 GiB of journal. A quarter more is the margin.
`R` is 100 MiB/s for `pd-balanced`, below the 140 MiB/s baseline Google Cloud documents for it,
and 200 MiB/s for `pd-ssd`, its documented floor: an E2 shared-core VM, as `e2-small` is, reads a
persistent disk at 200 MiB/s at most, below `pd-ssd`'s 240 MiB/s baseline
([persistent disk performance](https://cloud.google.com/compute/docs/disks/performance)). The
default, 20 GiB of `pd-balanced`, waits 573 seconds. A group waits at most 3600 seconds, so the
largest disks are 252 GiB of `pd-balanced` (3589 seconds) and 506 GiB of `pd-ssd` (3596), and the
hub refuses a larger `diskSizeGb`. Once the delay has passed, the health check, every 10 seconds
and unhealthy after 6 failures, finds a VM still reading unhealthy within another minute, and
the group recreates it; the next boot reads from the start again. `pulumi up` waits for the group
to be stable, which a group is once its VMs are running and healthy
([MIG status](https://cloud.google.com/compute/docs/instance-groups/getting-info-about-migs)), so
the group's create and update timeouts are the initial delay after the provider's own 15 minutes
(pulumi-gcp v10.1.0's upstream `resource_compute_instance_group_manager.go:49-53`): 24 minutes 33
seconds by default.

On a Linux workstation, cmp itself reads far faster than those rates: GNU cmp 3.10, reading a loop
device over a sparse file against `/dev/zero` as the entry does, took 18.1 seconds for 20 GiB
(1133 MiB/s), 222.7 for 252 GiB (1158 MiB/s) and 458.2 for 506 GiB (1131 MiB/s). A sparse file's
holes are read from no disk, so these numbers are cmp's and the kernel's cost alone, on a faster
CPU than an e2-small's shared core, and say nothing of a persistent disk's rate, which only a
first deployment measures (see
[What a first Google Cloud deployment proves](#what-a-first-google-cloud-deployment-proves)).

## Repairing the data disk

A disk the boot leaves unmounted (see [What the hub's VM runs](#what-the-hubs-vm-runs)) keeps NATS
down, so the health check fails and, once the initial delay has passed, the group recreates the
VM on the same disk. Nothing writes a disk the boot refuses, so the next VM refuses it again, with
the same line, and the group recreates that one too: the loop goes on, and no boot formats the
disk, until the disk is repaired. To repair it, stop the group's repairs, keep a snapshot, repair
the disk on the hub's own VM, which keeps it attached, restart the VM, then undo each step.

The commands below follow gcloud 561.0.0's help for each and parse under it; none has been run
against a real group. `<group>` is the hub's `instanceGroup` output, `<zone>` its `zone` and `<h>`
its `hostname`.

1. Stop the group recreating the VM. The health check stays; the group no longer repairs a failed
   or unhealthy VM:

   ```sh
   gcloud compute instance-groups managed update <group> --zone <zone> --default-action-on-vm-failure=do-nothing
   ```

2. Find the VM, its data disk and its network, and read the line naming why the disk is unmounted
   (the data disk is the one whose `deviceName` is `data`; `<disk>` and `<network>` are the last
   part of its `source` and of the network's link):

   ```sh
   gcloud compute instance-groups managed list-instances <group> --zone <zone>
   gcloud compute instances describe <vm> --zone <zone> --format='yaml(disks[].deviceName,disks[].source,networkInterfaces[].network)'
   gcloud compute instances get-serial-port-output <vm> --zone <zone> | grep 'envoy-hub: data disk'
   ```

3. Keep a snapshot of the disk before anything writes it:

   ```sh
   gcloud compute disks snapshot <disk> --zone <zone> --snapshot-names=<h>-data-before-repair
   ```

4. Reach the VM. The network the hub creates admits no SSH, so add a rule for the range IAP's TCP
   forwarding connects from
   ([IAP TCP forwarding](https://cloud.google.com/iap/docs/using-tcp-forwarding)), for the VM's
   tag alone, and connect through IAP, which needs `roles/iap.tunnelResourceAccessor`:

   ```sh
   gcloud compute firewall-rules create <h>-repair-ssh --network <network> --allow tcp:22 --source-ranges 35.235.240.0/20 --target-tags <h>-hub
   gcloud compute ssh <vm> --zone <zone> --tunnel-through-iap
   ```

5. On the VM, repair the disk as its line says. For a filesystem whose primary superblock is
   damaged, check it from the first backup superblock, at block 32768 of the 4 KiB blocks
   `mkfs.ext4` makes on a disk of 1 GiB or more, and let e2fsck fix what it finds:

   ```sh
   sudo e2fsck -fy -b 32768 -B 4096 /dev/disk/by-id/google-data
   ```

   e2fsck exits 1 when it has fixed errors. On a 1 GiB loop disk whose superblock alone was
   zeroed, this brought back every file, byte for byte. A disk whose whole first MiB is gone has
   also lost the start of its inode tables, the root directory's among them: the same command
   makes it mountable but empty, every file gone (on the same loop disk it left none). Run it there
   only if you mean to give up what the disk held; until then the files' blocks are still in the
   snapshot from step 3, for a recovery tool to read. A disk that holds nothing worth keeping, such
   as one whose first format stopped partway, is formatted only by your decision:
   `sudo mkfs.ext4 -m 0 -E lazy_itable_init=0,lazy_journal_init=0,discard /dev/disk/by-id/google-data`.

6. Restart the VM, so that its boot finds the filesystem and mounts the disk, and check that the
   line is gone and the health check turns `HEALTHY`:

   ```sh
   gcloud compute instances reset <vm> --zone <zone>
   ```

7. Undo steps 4 and 1, and delete the snapshot once the hub is healthy and you no longer need it:

   ```sh
   gcloud compute firewall-rules delete <h>-repair-ssh
   gcloud compute instance-groups managed update <group> --zone <zone> --default-action-on-vm-failure=repair
   gcloud compute snapshots delete <h>-data-before-repair
   ```

Whether the line reaches the serial console, whether `do-nothing` stops the loop, and whether
e2fsck's repair, on a real disk, leaves what JetStream needs are for a first deployment to show
(see [What a first Google Cloud deployment proves](#what-a-first-google-cloud-deployment-proves)).

## Updating a hub

An instance template cannot change once it is created
([instance templates](https://cloud.google.com/compute/docs/instance-templates)), and the hub's
holds the cloud-config, so a change to anything the cloud-config is rendered from (`users`,
`listener` and its `allowedHosts`, `sessionUser`, `authCallout`, `tags`, `tailnetDnsName`,
`natsImage`, `tailscaleImage`) or to `machineType`, `diskType`, `diskSizeGb`, `network` or
`subnetwork` makes a new template. So does a plugin release that changes the rendering itself:
the release that added `listener` and `sessionUser` renamed the secret fetcher and gives every hub
a Serve configuration, so upgrading to it replaces every existing hub's VM once, with the outage
below, even a hub that sets neither input. The release that rendered `ENVOY_ALLOWED_HOSTS` into
the listener's unit
replaced, once and with that same outage, the VM of every hub that runs a listener, and no hub
without one. This release adds `$JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS`, the `envoy-roles`
role-lane subscription and, for each key-value bucket the set opens,
`$JS.API.STREAM.UPDATE.KV_<bucket>` to `ListenerPermissions`, and so to `HostPermissions` and
`DispatchPermissions` (which also gains the update for the webhook redelivery bucket), and the
session consumers' entries to `ListenerPermissions` alone, so a hub's own `listener` user gains
them while its `machineId` and `dispatchMachineId` users keep their per-durable scope; that
changes the rendered users of every hub with a `listener`, a `machineId` user or a
`dispatchMachineId` user: moving such a hub to it replaces its VM once, with the outage below, and
a hub with none of them keeps its VM. It also adds the `listenerMachineId` kind, whose user holds
those entries beside the plain `notifications.>` subscribe, for a listener whose user no session
shares; a hub gains it only when its own users name it.
The release that adds `envoy_known_sessions` to `bus.ListenerBuckets()` adds that bucket's calls to
`ListenerPermissions`, and so to `ListenerHostPermissions`, `HostPermissions` and
`DispatchPermissions`, which changes the rendered users of every hub with a `listener`, a
`machineId`, a `listenerMachineId` or a `dispatchMachineId` user: upgrading such a hub to it
replaces its VM once, with the outage below, and a hub with none of them keeps its VM.
The release that adds `listener.senderMessagesPerMinute` and `listener.senderMessageBurst` renders
neither variable for a hub that sets neither input, so such a hub keeps its VM. Setting, changing
or removing either input changes the listener's `docker run` command in the cloud-config, so it
replaces the VM once, with the outage below, and the new VM's listener starts with every sender's
count full.
The cloud-config is the instance
template's `user-data`, and a template cannot change. The group then replaces its VM at once
(`PROACTIVE`, minimal action `REPLACE`), deleting it before it creates the next (`RECREATE`, no
surge, at most one unavailable). NATS is down from the old VM's stop until the new one's `nats`
unit starts, and its clients reconnect then. The new VM gets the same data disk, so it keeps the
stream and rejoins the tailnet as the same node. Its boot disk comes from the `cos-stable` family
as it is then, so a new template also brings whichever COS release the family names at that
moment. Once the new VM is up, check what its listener serves, which the replacement itself does
not prove: its `listener API hosts:` boot line (`docker logs envoy-listener` on the VM, or the
serial output) must name `<h>.<tailnetDnsName>` as the clients dial it, or a `/v1` route at
`http://<h>.<tailnetDnsName>:9020` must answer 200 from a device on the tailnet. The group's
autohealing check reads NATS's `/healthz` on 8222 and nothing dials 9020, so a `tailnetDnsName`
that passes the shape rule without being this tailnet's name, or a MagicDNS collision that gave
the node `<h>-1` rather than `<h>`, leaves a healthy group whose listener answers 421 to every
client.

Three inputs work otherwise:

- `tags` reach the tailnet only when the node first logs in. containerboot runs `tailscale up`,
  with `--advertise-tags`, for a node that is not logged in, and otherwise `tailscale set`, which
  passes `--accept-dns=false` and `--hostname` and never `TS_EXTRA_ARGS` (tailscale v1.104.1
  `cmd/containerboot/main.go:583-682`, `cmd/containerboot/tailscaled.go:145-146` and `:169-185`).
  A change to `tags` still replaces the VM; change a joined node's tags on the tailnet itself.
- `hostname` names every child, so a new one is a new hub: Pulumi creates a new set of children,
  whose VM gets a new data disk, so a new tailnet node and an empty stream, then deletes the old
  children, all but the old disk; the old node stays on the tailnet. The new node logs in with
  `tailnetAuthKey`, which must then be a new single-use key: the hub's first login spent the last.
- `zone` replaces the group itself, whose zone cannot change (pulumi-gcp v10.1.0's upstream
  `resource_compute_instance_group_manager.go:120-126`). The group's name is generated, so Pulumi
  creates the new group, under a new name, before it deletes the old one. The new group's VM gets
  a new, empty data disk, so the stream starts empty, and logs in with `tailnetAuthKey`, which must
  then be a new single-use key. It asks for `<h>` while the old node still holds it, so
  Tailscale names it `<h>-1` ([machine names](https://tailscale.com/docs/concepts/machine-names)),
  and `natsUrl` names the old node, which goes offline when the old group is deleted. The old disk
  is left detached in the old zone. To move a hub to another zone:

  1. Set `zone`, and a new single-use `tailnetAuthKey`, and run `pulumi up`. It returns once
     it has deleted the old group, and with it the old VM.
  2. Remove the old node from the tailnet
     ([remove a device](https://tailscale.com/docs/features/access-control/device-management/how-to/remove)).
  3. Rename the new node from `<h>-1` to `<h>` (Edit machine name, on the machine names page
     above), which also renames it in MagicDNS.

  Until the rename, `natsUrl` names the old, offline node, so the hub's clients reconnect only
  after it, to the new VM's empty stream.

## What a first Google Cloud deployment proves

The unit tests run the hub under Pulumi's mocks, check its cloud-config against the golden, and
run the secret fetcher, the listener's seed script, the joined script and the data disk's entry
under `/bin/sh` against fakes and stubs; the seed command's Secret Manager store runs against a
fake too. The live tests run the listener as the listener's user and a session with no credential
as the session user on real NATS servers. None of them runs
Google Cloud, COS or a tailnet, so the first deployment is where these hold or fail. Watch them on
its first `pulumi up`:

- `/dev/net/tun` is present at boot on COS 121 without `modprobe`, as it was on a scratch VM;
  `bootcmd`'s `modprobe tun` stays for a release where it is not. The Tailscale container starts
  with it.
- `docker exec tailscaled tailscale --socket=/tmp/tailscaled.sock status --json` answers on COS
  with Tailscale v1.104.1's image, and reports `"BackendState": "Running"` once the node has
  joined, so `tailnet-joined` empties the key file and writes its marker.
- The data disk is at `/dev/disk/by-id/google-data` when `bootcmd` runs, and a new persistent
  disk reads as zeros to its end, so the first boot formats it; `fsck.ext4` runs on COS as its
  documentation shows; and after `gcloud compute instance-groups managed recreate-instances` the
  new VM finds its filesystem (`blkid -p` exits 0), reads nothing and formats nothing, and keeps
  the stream and its tailnet node.
- How long the first boot's read of a real persistent disk takes, unmeasured: the initial delay
  assumes 100 MiB/s for `pd-balanced` and 200 MiB/s for `pd-ssd` (see
  [The first boot's read](#the-first-boots-read)). The first boot's serial output, or
  `journalctl -b` on the VM, shows when the read began and when `mkfs.ext4` ran.
- The line a disk left unmounted gets reaches the serial console and the journal, and the steps of
  [Repairing the data disk](#repairing-the-data-disk), none run against a real group:
  `--default-action-on-vm-failure=do-nothing` stops the group recreating the VM, and IAP's SSH
  reaches COS.
- Two statements this README takes from Google Cloud's documentation: a later `diskSizeGb` or
  `diskType` leaves the kept data disk at the size and type it was created with, and deleting the
  hub leaves that disk behind, detached (see [The hub's data disk](#the-hubs-data-disk)).
- The health check turns `HEALTHY` through COS's host firewall once `nats-firewall` has run, and
  `pulumi up`'s wait for `STABLE` ends within the provider's timeout despite autohealing's
  initial delay, 573 seconds for the default disk. Google Cloud accepts `maxSurgeFixed: 0` with
  `maxUnavailableFixed: 1` on a zonal group of one, and the template's `namePrefix`.
- Clients reach 4222 over the tailnet through `nats-firewall`'s rule with a real tailscaled. COS
  121 filters with nf_tables (`iptables -V` prints `iptables v1.8.13 (nf_tables)`) and drops INPUT
  by default, while Tailscale v1.104.1's image writes its own accept rule for `tailscale0` with
  iptables-legacy. On a scratch COS VM, with a veth named `tailscale0`, a connection to 4222 on
  that interface timed out until the rule was added and then opened, and one to 8222 there, with
  no rule, still timed out. tailscaled itself did not run on that VM.
- The key fetcher runs on COS's `/bin/sh`, curl, jq, base64 and mktemp, and the service account's
  grant reaches Secret Manager within the fetcher's six attempts at the first boot. When every
  attempt fails, containerboot, finding no key, exits at its 60-second boot timeout
  (`TS_BOOT_TIMEOUT`), and `tailscaled.service`'s restart runs the fetch again; that loop was
  measured on systemd 255, not on COS's own.
- containerboot applies `/etc/tailscale-serve/serve.json` once the node is up, so
  `tailscale serve status --json` on the VM's Tailscale container shows the forward of 9020, and
  `http://<h>.<tailnet>:9020/healthz` answers over the tailnet from a device the policy lets reach
  port 9020. That containerboot watches the file's directory was read in its source, not run.
- `envoy-listener.service`'s `ExecStartPre` failing, as it does until the seed's grant reaches
  Secret Manager, is restarted by `Restart=always` 10 seconds later. That was measured on systemd
  255, not on COS's own.
- `envoy-infra seed` against Secret Manager itself: that `versions/latest` names a newest version
  that is `DISABLED` or `DESTROYED`, that `GetSecretVersion` on `latest` answers `NOT_FOUND` for a
  secret with no version, that `ListSecretVersions` with no filter lists `DESTROYED` versions, and
  that `GetSecret` answers `expire_time` for a secret given a `ttl` or an `expireTime`. The store's
  lines under [What `check` and `verify` print](../README.md#what-check-and-verify-print) rest on
  these. Also that `AccessSecretVersion` on `versions/1` answers `NOT_FOUND` for a secret with no
  version and `FAILED_PRECONDITION` for a disabled or destroyed version 1, which `generate`'s check
  before the write rests on; that, after `generate`'s write, `AccessSecretVersion` on a `latest`
  that names no version yet answers `NOT_FOUND`; and how often the listing and `latest` show the
  write within `generate`'s pauses, 7.75 seconds in all. Google documents both as eventually
  consistent, and such operations as converging typically within minutes, but may take a few
  hours. A write they do not show in time ends as an error after writing, though `generate` read
  version 1 back by number; `generate` run again then refuses the secret, which holds version 1,
  and `check` and `verify` confirm the write once `latest` names it.

## A NATS hub, in YAML

The whole `Pulumi.yaml` of a project with one hub in the project `example-project`, reading the
tailnet auth key from a secret config value. `example-hub`, `example-project`, the
`example.invalid` images and each `<...>` are placeholders for your own values. Its users are
`example-host-a`, the host whose listener [the module's YAML example](../README.md#yaml)
runs; `example-operator`, a client with permissions of its own; and `example-dispatch`, the user
`envoy-dispatch` shares with its listener. It runs a listener of its own, whose seed is in the secret `example-hub-listener-seed`, and admits a client presenting no
credential as `example-sessions`. The users' public keys are the placeholders `core`'s tests use,
and the listener's and the session user's are placeholders too: put in the ones
`envoy-infra seed generate` printed for your users and your listener, and one whose seed you
discarded for the session user.
Its `pluginDownloadURL` is the address the release built into the plugin from the repository it
ran in, `https://github.com/cybermonklabs/legion/releases/download/envoy-infra-v${VERSION}` today.

```yaml
name: envoy-nats
runtime: yaml
packages:
  envoy-infra:
    source: envoy-infra
    version: <version>
    pluginDownloadURL: https://github.com/cybermonklabs/legion/releases/download/envoy-infra-v${VERSION}
config:
  tailnetAuthKey:
    type: string
    secret: true
resources:
  gcp-example-project:
    type: pulumi:providers:gcp
    properties:
      project: example-project
    options:
      version: 10.1.0
  example-hub:
    type: envoy-infra:gcp:NatsHub
    properties:
      hostname: example-hub
      tailnetDnsName: example-tailnet.ts.net
      zone: us-central1-a
      tags:
        - tag:example-host
      tailnetAuthKey: ${tailnetAuthKey}
      natsImage: example.invalid/nats@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
      tailscaleImage: example.invalid/tailscale@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
      users:
        - name: example-host-a
          nkey: UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO
          machineId: example-host-a
        - name: example-operator
          nkey: UCYF4CJJNU55NFRALQXGPOAZRM5R5JBXI6OAZB2BHWJ4QTTB5XHFENDG
          permissions:
            publish:
              allow: [">"]
            subscribe:
              allow: [">"]
              deny: ["$SYS.>"]
        - name: example-dispatch
          nkey: UDCQRANOFSHHJXENVBTXNFSRUDIUXGXPNWC5ABJFJ7GBPO27F7UIBVXD
          dispatchMachineId: example-dispatch
        - name: example-listener-a
          nkey: UCKA5BQ7T62SA4THAXWQYT3TKRWD6SGKDXMPYZR2D7EQBYBTWA4RPYVW
          listenerMachineId: example-listener-a
      listener:
        image: example.invalid/envoy@sha256:00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff
        seedSecret: example-hub-listener-seed
        nkey: UBZEZDPQNFYMZ4J4XOLTB2IN5PBPZ6FZEGY55R623A4GPBPZLTQFKDYV
      sessionUser:
        name: example-sessions
        nkey: UCNPQVMRWOOJWCD7XDVLH6MEOVFBMTV4LAQSKZYROSETYJVFAWDMNKVA
    options:
      providers:
        - ${gcp-example-project}
outputs:
  natsUrl: ${example-hub.natsUrl}
  instanceGroup: ${example-hub.instanceGroup}
```

The gcp provider is pinned at 10.1.0, the pulumi-gcp release the hub is built against. In the
project's directory, with your Pulumi backend selected:

```sh
pulumi stack init <stack>
pulumi config set --secret tailnetAuthKey <tailnet auth key>
pulumi preview
```

The preview plans `+ 13 to create`: the stack, the gcp provider `gcp-example-project`, the hub
`example-hub`, and the hub's ten children as
[What `gcp:NatsHub` deploys](#what-gcpnatshub-deploys) lists them, the network and the listener's
seed grant among them. The secret `example-hub-listener-seed` must exist in `example-project`
before `pulumi up`, and hold the listener's seed before the VM starts its listener. Its
outputs are `natsUrl: "nats://example-hub:4222"` and `instanceGroup`, `example-hub-` and a random
suffix. The template's `user-data` is `gcp/testdata/cloud-config.golden` byte for byte. `pulumi up`
then applies it. The listener of the module's first example reaches this hub with `natsUrls` set
to `nats://example-hub:4222` and the seed of `example-host-a`.

## The clustered hub, `gcp:ClusteredNatsHub`

`envoy-infra:gcp:ClusteredNatsHub` serves this hub's users from three nats-server pods of a GKE
cluster, one per zone, rather than from one VM. [Its section of the module's
README](../README.md#gcpclusterednatshub) lists what it deploys, its inputs and refusals, its
sizing and what the consumer prepares, with an example. This section is what differs from
`gcp:NatsHub` in running it.

### The same users

Both hubs render their users through one function, `authorization` in `gcp/natshub.go`, and
`core`'s `Render` under the same header, so the same `users`, listener and `sessionUser` give the
same authorization byte for byte (`TestClusteredNatsHubRendersTheVMHubsAuthorization`). Everything
[The hub's users](#the-hubs-users) says holds for the clustered hub, with one difference: the
listener's user is named after its `machineId` (`listener.machineId` or `tailnet.listener.machineId`,
wherever the listener is declared), which defaults to `tailnet.serviceName` with `tailnet` and is
required without it, where the VM names it after its `hostname`. A deployment that moves from a
`gcp:NatsHub` whose `hostname` is `envoy-hub` keeps the machine id `envoy-hub`, by naming its
Tailscale Service `envoy-hub` or by setting `machineId`: the listener's user is then the one the
VM rendered, with `ListenerPermissions(envoy-hub)`, and its durable is the VM listener's
`listener-envoy-hub`, so a durable restored with the streams is the one the new listener binds.
The hub runs one listener, in a pod of its own, as that machine id.

The rendered `auth.conf` is a key of the chart's ConfigMap `<n>-config`, beside `nats.conf`, which
includes it; like the VM's metadata, the ConfigMap holds it in the clear, public keys only.

### A reload, not a replacement

A users change replaces the VM of a `gcp:NatsHub` (see [Updating a hub](#updating-a-hub)). On the
clustered hub it rewrites `auth.conf` in place, since the hub's own provider updates ConfigMaps
rather than replacing them, and the chart's config reloader beside each server, which watches
`nats.conf` and `auth.conf`, signals nats-server to reload once the kubelet has updated the mounted
file. No server restarts and no client reconnects: on three local nats-server 2.15.1 processes with
the real reloader 0.24.0, a client kept its connection id with zero reconnects while a user was
added (it could then connect) and removed (it was then refused), and every server's PID stayed the
same. The chart's own checksum annotation, which would restart the pods on any change to the
ConfigMap, is off.

The session user is the exception. nats-server 2.15.1 refuses a whole reload that changes
`no_auth_user`, logging `config reload not supported for NoAuthUser`, and keeps the users it had,
so a new session user takes effect only when a server restarts. The hub therefore writes the
session user's public key into the hub pods' annotation `envoy-infra.sjawhar.dev/no-auth-user`
(absent without a session user): adding, removing or changing the session user changes the pod
template, and the StatefulSet replaces its pods one at a time, each server restarting on its own
claim while the other two serve. Without `authCallout`, any other users change leaves the pod
template as it was; while `authCallout` is set, a users change that adds, removes or rekeys a user
rolls the pods too (the callout, below), and only a change of a grant, or of a name, which is only
a comment beside its key, leaves the template as it was. A users change made in the same update as
a session-user change applies, with it, as each pod restarts. The roll must start from the new
`auth.conf`: a pod restarted on the old file keeps the old session user, and the reload that later
brings the new file is refused. The same holds for the store's roll below, from the new
`nats.conf`, and for the callout's, from the new `auth.conf`. So the StatefulSet's annotation
`config.kubernetes.io/depends-on: /namespaces/<namespace>/ConfigMap/<n>-config` makes pulumi-kubernetes
apply the StatefulSet only after the ConfigMap, with or without a session user, and a ConfigMap
update that fails stops the update before any pod rolls.

The file store's limit is the second exception. nats-server 2.15.1 refuses a reload that lowers
`max_file_store`, logging `config reload not supported for decreasing jetstream max memory and
store`, and a reload is all or nothing against the options the server runs with: once `nats.conf`
holds a lower limit, every later reload is refused too, a users change in `auth.conf` among them,
until the server restarts. The local cluster test shows it: with a lower `max_file_store` written
beside a new user, the reload was refused and the user was refused too, and the restarted server
admitted it. The hub therefore also writes the limit into the hub pods' annotation
`envoy-infra.sjawhar.dev/max-file-store`, so any change of `maxFileStoreGb`, given or derived from
`streamMaxBytes` and `diskSizeGb`, rolls the pods onto the new `nats.conf`; a raise, which
nats-server could reload, rolls them as well. Each pod the roll restarts starts on the new
`nats.conf`, by the same depends-on order as the session user's roll: a pod restarted on the old
file would keep the old limit, and the reload that later lowers it would be refused, with every
reload after it. A disk growth whose limit follows `diskSizeGb` therefore rolls the pods too,
unless `maxFileStoreGb` holds the limit the hub runs with (the README's disk-growth steps).

The callout is the third exception. nats-server 2.15.1 refuses a whole reload that adds, removes or
changes `auth_callout`, logging `config reload not supported for AuthCallout`
(`gcp/clusteredhub_callout_live_test.go` measures it, with a grant change on the same server as the
control that reloads), and `auth_users` lists every user's key, so adding or removing a user changes
it too. The ping lines that follow the authorization block beside a callout (the README's
[NATS authorization](../README.md#nats-authorization)) come and go only with that block, so the one
reload of them the server would take is an edit of their value (`server/reload.go:1751-1754`).
`pingIntervalOption.Apply` only logs (`server/reload.go:670-672`), and `setPingTimer` reads the
interval when it re-arms (`client.go:6037-6050`), so each connection would take such an edit at
its timer's next re-arm, within one interval of 20 s. The hub writes the SHA-256 of `auth.conf`'s
callout lines, `core.Authorization.CalloutLines()` (the `auth_callout` block, which has no
`timeout` line, and the ping lines), into the hub pods' annotation
`envoy-infra.sjawhar.dev/auth-callout` (absent without `authCallout`), which rolls the pods, by the
same depends-on order as the session user's roll, on a change of either: a pod restarted on the old
`auth.conf` would keep the old callout, and the reload that later brings the new one would be
refused. The ping lines are in the digest so that every server applies an edit of them at one known
moment, its pod's restart, with every connection's timer started fresh. The routes' own ping in
`nats.conf`'s `cluster` block
([below](#two-ports-guarded-by-the-networkpolicy-alone)) appears and goes with the callout, so it
always arrives with a roll. A change of a user's grant or name alone leaves the lines and the pods
as they were, and the reloader applies it. A
`gcp:NatsHub` needs none of this: its configuration is the instance template's `user-data`
(`gcp/natshub.go:807`), and the group replaces its VM on a new template (`PROACTIVE`, `REPLACE`,
`RECREATE`: `gcp/natshub.go:834-840`), so any configuration change restarts its server already.

<a id="two-ports-guarded-by-the-networkpolicy-alone"></a>

### Two ports, guarded by the NetworkPolicy alone unless `routeAuthorization` has reached `authorization`

Without `routeAuthorization`, the servers' routes carry no credential: any process that reaches a
hub pod's port 6222 can join the cluster as a server. The NetworkPolicy `<n>` admits 6222 from the
hub pods alone, and that is then the routes' only guard. A NetworkPolicy cannot fence a pod's own
node, so [Route authorization](#route-authorization) adds a credential, which fences a process that
can connect to 6222 and not one with access to a node's network namespace or its traffic, nor one
that joined as a route before `authorization` applied (see that section).

Beside a callout, the routes keep their own ping. The top-level `ping_interval` of 20 s the callout
adds to `auth.conf` would also govern the routes, since a route reads `cluster.ping_interval` and
falls back to the top-level value (`client.go:5957-5964`, `:6042-6049`, `:7124`). A route is pinged
on every tick whether or not it carries traffic (`:5968-5970`), and a PING queues behind pending
data. So a route whose queue takes longer than 2 to 3 intervals to drain is closed as stale and
loses what is in transit: a measurement with a 3 s interval lost 30 of 40 MB. Beside a callout
only, the hub's chart values therefore merge `ping_interval: "30s"` and `ping_max: 2` into
`nats.conf`'s `cluster` block (`config.cluster.merge`). These are the routes' effective values
without a callout: the 2-minute default capped at `routeMaxPingInterval`, 30 s
(`server/route.go:144`, `:152`; `client.go:6005-6010`), and the default `ping_max` of 2
(`const.go:123`; a route falls back to the top-level value, `client.go:5983-5986`). Only client
connections get the 20 s ping. Without a callout the cluster block is as it was. `gcp:NatsHub` and
the AWS service run one server each, with no routes. `gcp/clusteredhub_ping_live_test.go` runs two
servers on the rendered configuration, the route through a proxy limited to 0.4 MB/s: with the pin,
a 40 MB burst does not close the route as stale within 60 s of the burst's start, and without it
the top-level 20 s ping does, within 60.5 s of the moment the sending server has queued the burst.

The session listener's `/v1` on 9020 is the second port a policy guards. Declared as
`tailnet.listener` with no `clientNamespaces`, that listener carries
`ENVOY_API_ALLOW_UNAUTHENTICATED=1` and no `ENVOY_API_TOKEN`, and the component takes no token
input for it, so its `/v1` answers with no credential at all. The NetworkPolicy `<n>-listener`
then admits TCP 9020 from the tailnet hosts' pods alone, and that is what keeps every other pod of
the cluster off `/v1`. A listener declared as the hub's own `listener`, or given
`clientNamespaces`, is admitted to by the pods of those namespaces as well, and requires each
caller's projected ServiceAccount token instead of the flag, so the policy is not its only guard
([The session listener](#the-session-listener)).

Both guards need a cluster that enforces NetworkPolicy, as GKE Autopilot does with GKE Dataplane
V2. A cluster whose network plugin does not enforce it accepts both objects and admits every
caller, and the deployment looks healthy either way. The two policies' rules are pinned by the
unit tests (`TestClusteredNatsHubNetworkPolicy` and `TestClusteredNatsHubListenerNetworkPolicy`);
the local cluster test runs its three servers on loopback with no policy at all, so it proves the
routes and the rendered configuration, never the policy's enforcement, which is the cluster's.

### Route authorization

`routeAuthorization` gives the routes a credential: the user `route` and a password the component
names by reference and never reads, the key `secretKey` of the existing Secret `secretName` in
`namespace`. The nats container takes it as `NATS_ROUTE_PASSWORD`, from a `secretKeyRef`, and
`NATS_ROUTE_URLS`, the three pods' route URLs with `$(NATS_ROUTE_PASSWORD)` as each URL's
password, which Kubernetes expands since the password's variable comes first in the container's
env. `nats.conf` names only the variables, as `$NATS_ROUTE_URLS` and `$NATS_ROUTE_PASSWORD`, so
the ConfigMap never holds the credential. The chart's own `config.cluster.routeURLs.user` and
`password` are not used: the chart writes them into `nats.conf` as literals.

`stage` is how far the rollout has gone, and each stage is one apply after the one before it has
finished:

| Stage | What the chart renders | What the apply does |
| --- | --- | --- |
| `env` | The two variables in the nats container. `nats.conf` is as without `routeAuthorization`. | Rolls the pods, one at a time; no route changes. |
| `routes` | Also `routes: $NATS_ROUTE_URLS` in the `cluster` block. | The reloader applies it: each server presents the credential on the routes it opens. |
| `authorization` | Also `authorization { user: "route", password: $NATS_ROUTE_PASSWORD }` in the `cluster` block. | The reloader applies it: each server refuses a route that does not present the credential. |

The component renders no TLS for the cluster, so NATS sends each route URL's password unencrypted,
on every route a server opens. The route URLs name the pods by their headless Service, by the
absolute name `<n>-<i>.<n>-headless.<namespace>.svc.cluster.local.:6222` with the trailing dot,
which resolves to the pods' internal IPs and nothing else: a relative name that failed while a
pod is re-created would go down the resolver's search list to `<n>-<i>.<n>-headless.svc.cluster.local`,
Service `<n>-<i>` of a namespace named `<n>-headless`, and a route sends its credential before it
reads its peer's INFO (`server/route.go:2087-2094`). The address a server
gossips to its peers for another server is the one it saw on that server's connection, its pod IP
(nats-server 2.15.1 `server/route.go:859-871`), so the hub's own servers never send the password
outside the cluster's VPC. `noAdvertise` is no part of that: it only keeps the cluster's addresses
from clients (`opts.go:6338`, `route.go:737`, `:2794`). The one exception is a process that joined
as a route before `authorization` applied, below: it can name any address, and a server dials it
with the password. Between nodes, the route traffic has only Google's default VPC encryption, with
Google-held keys, and neither NATS nor GKE adds any: VM-to-VM connections inside Google's
production network are integrity-protected and encrypted
([Encryption in transit](https://cloud.google.com/docs/security/encryption-in-transit), "Google
Cloud virtual network authentication and encryption"). On each node the password is in clear, so
the residual is node-level: a process with access to a node's network namespace or its traffic,
which can read the password and then join as a route, and the infrastructure provider. The
credential fences a process that can connect to port 6222, and not that one. Route TLS fences
both, and it is this residual's fix. Its cost is a CA and per-server
certificates, new component inputs, the `tls://` switch and a staged rollout: the chart has the
switch (`config.cluster.tls`, a `secretName` holding `tls.crt` and `tls.key`, and `tlsCA` for the
CA that verifies a peer), but the component exposes none of it and mints no certificate, and a
route URL of `tls://` replaces `nats://` in `NATS_ROUTE_URLS` when it is on.

The order holds because nats-server reads a variable of `nats.conf` from its own environment at
each load. A `nats.conf` that names `$NATS_ROUTE_URLS` before the container holds it fails every
reload (`variable reference for 'NATS_ROUTE_URLS' on line … can not be found`), a later users
change's included, and a pod that restarts on it exits 1. So `env` is complete on every pod before
`routes`, and every server opens its routes with the credential before `authorization` refuses a
route without one. Measured on three local servers of nats-server 2.15.1, a three-copy stream
written every 20 ms throughout, the `routes` reloads and then the `authorization` reloads, one
server at a time 20 s apart, split no mesh and lost no acknowledged write.

A reload closes only the routes the server accepted that now fail the credential check, never one
it opened itself (`server/reload.go:2367-2376`, `route.go:2484-2530`). So a process that joined as
a route during `env` or `routes` keeps the routes the hub's servers opened to it, and once
`authorization` applies it can read the password: it sends one INFO naming another server with
`auth_required: true`, and the server that reads it dials that address as an implicit route with
the user `route` and the route password (`route.go:702-713`, `:1092-1094`). Measured on local
servers of nats-server 2.15.1 in this change's review: after the `authorization` reload, the hub's
server dialed the address such a process named with the user and the password; without the reload
it sent no credential. So `authorization` fences a process that tries to join after it, and not one
that joined before.

So the rollout runs two commands of `envoy-infra`, as an identity that may `get` the
StatefulSet and each of its pods, and `create` `pods/portforward` and `get` `pods/log` on each pod,
all of which a Role grants by name: through a kubeconfig, or in a pod with none, such as a Job, as
the pod's ServiceAccount, which kubectl then reads the cluster through:

```sh
# once env has rolled every pod:
envoy-infra routes record --namespace envoy --statefulset envoy-nats --out routes-record.json
# once authorization has reached every pod:
envoy-infra routes check --namespace envoy --statefulset envoy-nats --record routes-record.json
```

`routes record` saves each pod's `uid`, its nats container's `restartCount` and `startedAt`, the
time, and the kubeconfig context it read through with its cluster's API server, or, in a pod with
no kubeconfig and no `--context` given, the context `in-cluster` and the server
`https://$KUBERNETES_SERVICE_HOST:$KUBERNETES_SERVICE_PORT`. `routes check` requires the record's
context and cluster of its own, exit 2 otherwise, since a hub of the same names on
another cluster has other pods, which the check would read as a new container on every pod. It
refuses until the `env` roll has finished: while any pod is not Ready with a running
nats container, is not on the update revision of a finished roll of the StatefulSet (its
`controller-revision-hash`, and the StatefulSet's `observedGeneration`, `updatedReplicas`, `currentRevision`
and `updateRevision`), or names no route variables in its nats container, since between two pods of a
roll every pod can be Ready while one still runs the revision before, and a pod the roll replaces
after the record has a log the record does not cover. It refuses as well while any pod's route
port already requires authorization: the record vouches that each log reaches back
to `env`, and one taken later can follow a restart whose log holds none of the routes opened at
`env` or `routes`. `routes check` reads, in one pass over the pods, each pod's IP from the API
server, each server's `server_id` from its `/varz` and its routes from its `/routez` through a
port-forward to 8222, its route port through a port-forward to 6222, and its nats container's
log as `kubectl logs --timestamps` gives it, then reads the pods again. It exits 0 when every
rule below holds; 1 when one shows the password may be read; and 2 when it could not read all it
needs, when the log carries no time of the container runtime's, when authorization is not yet in
force, left no line of the reload that loaded it, or loaded within 10 s of a pod's start, when the
mesh is not whole or the servers do not hold one password, when a route connection from another
address named no server, was still open when the log was read, or was logged in the 5 s before
authorization loaded, when the log holds a line the server did not write, or when a pod changed
during the check. It prints no password,
since it never has one, and quotes what a peer chose, a route's `remote_id` and `remote_name` and
an address from a log line, so no peer can write a line of its own on the operator's terminal. So
anything that can reach a pod's port 6222 can keep the check from reaching 0, though not make it
pass.

Exit 0 says that no process but the hub's servers held a route, or was handed the password, since
the record was taken, from the logs that reach back to it; it says nothing of any time before the
record. So a record is valid only for a `secretKey` that has never been under `authorization`: a
record of a hub whose current `secretKey` was under `authorization` before, after a rollback below
`authorization` and a restart of every pod, vouches for logs that lost the connections made under
that first `authorization`, and the check against it can exit 0 of a hub whose password a peer
read. After a rollback below `authorization`, or any exit 1, rotate to a new `secretKey` first
(below), and record at its `env`. `routes record` prints this condition with each record; it
cannot check it, since a pod that restarted keeps no log of the reload before, and nothing else in
the cluster records which keys were under `authorization`.

Every log rule reads only lines in the form nats-server's logger writes (`logger/log.go`: the
process id, the date and time to the microsecond, the level), at one level, and matches their
message: whole for the start line and `Reloaded: cluster`; whole, or by the prefix
before its digest, for `Reloaded server configuration`; by prefix for a failed reload; by a
pattern from the message's start for a route connection's lines; and by substring for a
credentialed URL, in the server's lines of itself alone. Never does a rule match the end of a line
or text anywhere in a connection's line: a connection's lines carry text its peer chose, a
client's `-ERR` text whole at the end of a `Client Error` line (`server/client.go:2268`), so such
a rule could be met or tripped by any client. Measured in this change's review: a client's line
ending in `[INF] Starting nats-server` passed a rotated log as whole, and one ending in a `Route
connection created` line forced exit 1.

A whole line in that form can still be another process's: nats-server prints a route peer's
server name unquoted when the route closes (`Router connection closed: <reason> - Remote: %s`,
`server/client.go:2125`), so a name that holds a newline writes the lines after it, and the auth
callout prints its service's error text the same way (`server/auth_callout.go:460`). Measured in
this change's review: before `authorization`, a route peer with no credential forged a `Removed
route` line with a password in it, which forced exit 1; after it, a reader forged a start line
timed at the container's start, and with the log cut at it the check said clean. So the check
reads the time the container runtime read each line at, which no process in the pod sets, and a
line in the logger's form is the server's only when it carries the start line's process id and
its logger time is within 2 s of the runtime's. Any other is exit 2, and no rule reads it. A peer
that writes the server's process id and the current time passes that test, and the process id is
no secret: the chart runs each pod with `shareProcessNamespace: true` beside its config reloader
(the chart's `files/stateful-set/pod-template.yaml`), so nats-server's process id is a small
number, and one route name can hold a forged line for each likely id. The auth service's error
text is the same path, for any client that reaches the client port, at any stage: so the auth
service must put no newline a client chose into its error text, and one that does lets a client
force exit 1 as a route peer can during `env` and `routes` (below). A forged line of either kind
can make the check stricter, and can turn exit 1 for that peer's own connections into exit 2, but
cannot make it pass, on one condition: authorization loads more than 10 s after each pod's
container started, which the `env` roll and the two applies after it give, each one after the one
before has finished. Within those 10 s a reader with the password could forge a start line the
start rule takes, so the check holds to it: when the runtime read the `Reloaded: cluster` line of
the reload that loaded authorization within 10 s of the container's `startedAt`, it is exit 2.
Measured in this change's review: env, the record, routes, authorization and the reader within
4 s of the start let a forged start line pass; the same line written more than 11 s after the
start gave exit 1. A log with any line that carries no time of the runtime's is judged by no rule,
since which of its lines the server wrote cannot be told, and is exit 2; `routes check` always
reads with `--timestamps`.

- **Every route is the hub's own.** Every route's `ip` is one of the pod IPs and every `remote_id`
  another pod's `server_id`, from that pod's own IP, and each pod has a route to each of its
  peers. The `ip` is the TCP peer's address, which the peer does not choose; the `remote_id` is
  the id the peer sends (`server/route.go:820`), and every route port's INFO shows its server's
  id, so a `remote_id` counts only from the IP of the pod whose server answers it. A route's
  `remote_name` proves nothing: a peer chooses its own name, and nats-server refuses an accepted
  route's name only when it equals the accepting server's own (`server/route.go:745-751`;
  `isDuplicateServerName` runs for gateways alone, `gateway.go:1090`, `:1166`), so the process
  this check looks for can join as `<n>-1` and pass a check of names. Nor is the route's `port`
  6222: `/routez` gives the TCP peer's port, and a route a peer opened comes from an ephemeral
  one, so one server's four routes to another showed four ports.
- **No peer that left had the password.** `/routez` shows only who still holds a route, so a
  process that read the password and then left passes it. The log keeps it: every
  `Route connection created` line (`server/route.go:2104`) names the connection's peer. The rule's
  window opens at the first line the container runtime read within 5 s before the `Reloaded:
  cluster` line (`server/reload.go:470`) of the reload that loaded authorization, by the
  runtime's time, which no line a peer writes can move, as it could move a marker such as the
  reload's `Trapped "hangup" signal`. In that window, the peer of each such line is a pod IP, by
  IP alone, or the pod's own loopback. Lines before the window are not judged: an implicit route
  carries the password only once authorization applies (`route.go:1092-1094`), and the `env`
  roll gives each new pod a new IP, so the logs of the pods it rolled first name the old IPs of
  the ones it rolled after. A log with no `Reloaded: cluster` line among the server's own lines
  has no window. When the route port shows authorization in force, or cannot be read, that is
  exit 2: the record vouches that the container had no authorization when it was recorded, so it
  loaded authorization by a reload, and that reload's line was lost or has not reached the pod;
  judging the whole log would read the `env` roll's old pod IPs as after authorization. A
  connection from another address is exit 1 only when it was logged after the `Reloaded:
  cluster` line and formed a route by the log, its close line naming a server; every other one
  is exit 2. One logged before that line, in the window, may predate the reload's new
  configuration, which the reload takes before it logs the line (`server/reload.go:1561`), and
  so carried no password, or may be a route the hub dialed after, with it (`route.go:1055-1093`).
  One still open when the log was read, with no login refused and no close line, may be an
  accepted connection still waiting for its login: nats-server logs the connection as it
  accepts it, before any CONNECT, and closes one that logs in to no route at the cluster's auth
  timeout (`server/route.go:2070-2073`, 2 s by default), so any process that reaches 6222 can hold
  one open while the log is read, and one that held a route when `/routez` was read is named
  there. And one whose peer named no server: one whose login the credential refused
  (`authentication error` on its `rid`), or one whose close line names no server, with no
  `- Remote:` (it closed at once, waited out the auth timeout, sent only part of a CONNECT, or
  named itself with an empty name, which the close line does not show, `server/client.go:2124`).
  Such a connection can still have carried the password: on one the hub opens it sends the
  password before the peer names itself (`server/route.go:2087-2094`), and a peer that names
  itself with an empty name holds a route like any other; measured in this change's review, such
  a peer the hub dialed got the password 4 times. The log does not say which side opened a
  connection, so the check neither condemns nor clears any of these. Find what ran at that
  address, and treat the password as read unless it is accounted for. Exit 1 there would let any
  process that reaches 6222 force a rotation, whose `env` and `routes` applies open the routes to
  it again. The exceptions. During `env` and `routes`, a process that reaches 6222 can still force
  exit 1 by a forged line in the server's form, in three ways: a `Removed route` line with a
  password in it; a `Failed to reload server configuration` line at ERR, which the failed-reload
  rule (below) counts whatever it says; or a `Reloaded: cluster` line, which opens the window before that
  process's own route connections. It times the line now and writes it once for each likely
  process id of the server's, within one route name, so that one of them carries the id the check
  reads as the server's (above); a client can do the same at any stage through an auth service
  that puts a newline it chose into its error text (above). And at any stage, a process that can
  make the server log enough, such as with failed logins on its client or route port, can rotate
  the log past its start line, which is exit 1 by the next rule, since a rotated log can hide a
  process that read the password.
- **The log reaches back to the `env` roll.** Each pod's `uid`, `restartCount` and `startedAt`
  are the ones `routes record` saved, its log begins with `Starting nats-server`
  (`server/server.go:2295`) in the logger's form, and the container runtime read that line from
  1 s before to 10 s after the container's `startedAt`. `kubectl logs` returns only the
  latest log file, which the kubelet rotates at 10 MiB by default
  ([Logging architecture](https://kubernetes.io/docs/concepts/cluster-administration/logging/)),
  and the process this check looks for can rotate the earlier lines away, for example with failed
  client logins. A pod restarted since the record, or a log that does not begin at its start
  line, keeps no log of the routes before it, so neither can be ruled out. A pod whose identity
  or container differs when the check reads the pods again after the pass is exit 2: what the
  pass read of it may be of two containers, its log may be of a container that started during
  the pass, and the next check finds the restart. So its start line finding is exit 2 too, though
  the new container's start line was read after the start the pass first read.
- **Authorization is in force with a password.** `/varz` shows no route authorization
  (`ClusterOptsVarz` has no field for it), so the check reads the INFO each route port sends a new
  connection first, and requires `"auth_required":true`, and a `Reloaded server configuration`
  line after the reload that loaded authorization. Without either, the apply may have succeeded
  while every server still takes a route with no credential: a reload that fails leaves the
  server on its old configuration. `auth_required` holds whenever the user is set, an empty
  password too, which nats-server takes, so the check also sends each route port a route login
  with no user and one with the user `route` and no password, each named as a gateway's so a
  login the credential took is refused next and joins nothing (`server/route.go:3065-3073`), and
  requires `-ERR 'Authorization Violation'` for both.
- **No reload failed, and no line holds a credentialed URL.** A `Failed to reload server
  configuration` line can hold the route password whole: a password outside the minter's rule
  below is printed in it (`integer … is out of the range` with the value, or `error parsing route
  url` with the URL), and so is a start on that file, which exits 1. The check cannot tell which
  failure printed it, so any failed reload counts. So does any INF line the server wrote of
  itself, not of a connection, that holds a URL with a user and a password, anywhere in the log:
  a rollback's `Removed route` lines (below) are such lines. The auth callout's WRN lines, which
  carry its service's error text, are not read for it.
- **The hub holds one password.** A pod that started with a password its peers do not hold has
  routes to none of them, and their logs name its failed route logins (`authentication error -
  User "route"`). When a pod's routes do not reach all its peers and its log names such logins
  from pod IPs since authorization, the check says the servers do not hold one password; the
  pods are brought to one password from `env`, below.

Exit 1 means: treat the password as read, cut the reach of any process the check names, and
change the password, below. One process neither read can see: one in a hub pod's own network
namespace, such as the reloader or the exporter, has that pod's IP, so the log check passes it,
and `remote_id` catches it only while it holds a route under an id other than its own pod's
server's, which it can send as its own. Nor does the credential fence it: the chart runs each pod
with `shareProcessNamespace: true`, so a process of the pod that runs as the nats container's user,
or with the capability to trace it, can read the password from that container's environment in
`/proc`, as it can read its traffic. The credential fences a process that can only connect to 6222.
A change of the password goes back to
`env`, which drops `authorization` from `nats.conf`, so the routes are open again during its `env`
and `routes` applies, and its rollout runs `routes record` and `routes check` again. The component
cannot make that check, since it runs at an apply and reads no pod; the rollout that applies the
stages runs it.

Lowering `stage` below `routes`, or removing `routeAuthorization` from a hub at `routes` or
`authorization`, makes each server log every route URL it removes with the route password in it
(nats-server 2.15.1 `server/reload.go:543`). So after such a rollback, or a failed reload, treat
the old password as read by whoever can read the hub's logs, and change it. A new value under the
same Secret key reaches no running pod: a container reads a `secretKeyRef` only when it starts, so
the `env` apply would do nothing. A second sync entry for the same Secret Manager secret does not
help either: the sync names its source by the secret's ID, so both entries would read the same
version. Mint the new password into a new Secret Manager secret, sync it under a new `secretKey`,
check by its hash, never by its value, that the key holds the new password, set
`routeAuthorization` to that key at `env` so the apply rolls the pods, then apply `routes` and
`authorization` again.

A new `secretKey` at `authorization`, without `stage: env` in the same apply, splits the mesh: the
change rolls the pods at any stage, and a pod that starts with the new password at
`authorization` is refused by every peer still on the old one. Measured in this change's review:
three servers at `authorization`, one restarted with a new value, had no route to either peer 15 s
later. A new value under the same key does the same at the next pod start, an eviction or a node
upgrade, with no apply at all, so never add a version to the route password's Secret Manager
secret, and pin its sync to a version number.

Measured on two local servers: lowering `routes` to `env` logged 4 `Removed route` lines carrying
the password. The forward step from `env` to `routes` logs one `Removed route` line for each route
connection the server opened to a URL it replaces, the URL as `nats.conf` named it before, with no
user and no password: the new URL differs only in its user and password, and a reload compares
route URLs whole (`urlsAreEqual`, `reflect.DeepEqual`, `server/util.go:178-180`; `reload.go:530-543`).
Measured on three local servers: 8, 4 and 0 such lines. No forward stage logs the password.

The password holds letters and digits only, a letter first, and at least 32 characters. It is
written into the route URLs whole, and nats-server types a variable's value as it types a literal
of `nats.conf`: a password of digits alone is an integer it refuses (`could not be parsed: integer
… is out of the range`, with the value), one such as `1k` is a size, and `yes`, `on`, `true` or
`False` is a boolean, which nats-server refuses with an error that does not print it; the length
rules those words out, and an empty password, which nats-server takes, the route port check above
refuses. The component never sees the password, so its minter holds it to that.

`gcp/clusteredhub_route_test.go` pins each stage's chart values, and
`gcp/clusteredhub_route_live_test.go` renders each stage with helm: the variables in the rendered
StatefulSet, in order; `nats.conf` at `env` byte for byte as without the input; no literal
credential in any stage, where the chart's own `routeURLs` credential, its control, writes four;
`nats-server -t` accepting the `authorization` render with the variables set and refusing it
without them and with a password of digits alone; and three servers on that render forming their
mesh while a server with no credential, and one with a wrong password, are refused, where the
same server with no credential joins a server on the `env` render.

`routes/` tests the check's rules on a hub held in memory, one row per rule with a clean hub as
its control, and its kubectl reads through a fake kubectl. `gcp/clusteredhub_routecheck_live_test.go`
runs `routes record` and `routes check`'s functions over three local servers that move through
the stages: the servers alone pass, and fail at `routes` alone; an impostor named `<n>-1` that
joined during `env` fails by `ip` and `remote_id`, and one at a pod's own address by
`remote_id` alone; a peer that read the password after `authorization` and left fails by the log;
a peer that left before `authorization` passes, though the logs name it before that reload; a
password of digits fails every server's `authorization` reload, which logs it, and the check; a
refused route login from another address is exit 2; a client's lines meet no rule and trip none,
and a log rotated to begin at one fails; a rollback's `Removed route` lines fail; a record taken
after `authorization` is refused; an empty password fails, and a stranger with the user `route`
and no password joins that hub; a restart during the check fails; and a refused `/varz`, a
rotated log and a restart since the record each fail. A route peer's lines written through its
server name before `authorization` are exit 2, not 1; a reader's start line written through its
name and first in a cut log fails, whether it is timed at the container's start or now; three
connections from other addresses whose peer named no server (closed at once, silent until the
auth timeout, a partial CONNECT) are exit 2, and so is a route peer with the password that names
itself with an empty name, though a hub server held its route; a client's line naming a refusal
of a held route with the password leaves exit 1; and a route peer whose name ends as a route
connection's line passes. The staged hub, the stamping of its servers' log lines with the time the
test first saw each, as a container runtime stamps them, and the route peers and clients the
scenarios speak as are in `gcp/clusteredhub_routecheck_fixture_live_test.go`.

### The tailnet hosts

With `tailnet`, each of `tailnet.replicas` host pods is a tailnet node of its own, named after its
pod, and a host of the Tailscale Service `svc:<tailnet.serviceName>`. A Tailscale Service sends a
client to a host, not to a host per port, so every host serves both of the Service's ports: 4222,
forwarded as TCP to the hub's client Service, and 9020, forwarded to the session listener's own
cluster Service. Clients reach them as the hub's `tailnetNatsUrl`,
`nats://<tailnet.serviceName>.<tailnetDnsName>:4222`, and `tailnetEnvoyUrl`, the same name on
9020. A `tailnet.listener` with no `clientNamespaces` takes no bearer there, as on the VM, so
[The hub's listener](#the-hubs-listener) holds for port 9020 of the Service as written. A
listener declared as the hub's own `listener`, or given `clientNamespaces`, requires each tailnet
caller's token too, as it requires every other caller's ([The session
listener](#the-session-listener)).

Each host's advertiser advertises the Service as soon as its node may. So a listener fault leaves
the hub's 4222 on the tailnet, and every host keeps serving both ports while the listener's own
pod is unready. Nothing drains a host when NATS or the listener is out of reach: the split gives
that signal up on purpose, and an alert reads the listener's own pod instead (see [Metrics and
alerts](#metrics-and-alerts)). In-cluster clients, which reach the client Service directly, are
unaffected by any of it.

A host pod is ready only while its node hosts the Service, approved, so the Deployment's rollout
(`maxUnavailable: 0`, `maxSurge: 1`) and its budget (`maxUnavailable: 1`) count only hosts that
serve: a new host serves before an old one stops. A stopping host drains the Service, waits 20
seconds for its clients to move to another host and logs its node out. The advertiser exits on
its SIGTERM at once, and the pod's grace period of 60 seconds holds the tailscale side alone, 55
seconds of it.

### The session listener

The session listener runs in a workload of its own, the Deployment `<n>-listener`, and in no host
pod. One pod runs it, as its `machineId`, and no input changes that count.
Two copies would contend for the one durable `listener-<machine id>`: the second waits 135
seconds for it (`cmd/listener/durable.go`, `durableBindDeadline`), then exits, and is restarted
again and again. That holds until cybermonklabs/legion#2404 gives the listener a delivery lease.
The module's README lists the five objects the listener's name carries
([The session listener](../README.md#the-session-listener)).

How it is reached. Each host forwards the Tailscale Service's port 9020 to the Service
`<n>-listener` in the hub's namespace, and the pods of `clientNamespaces` dial that Service at
`<n>-listener.<namespace>.svc.cluster.local:9020`. The NetworkPolicy `<n>-listener` admits TCP
9020 from the tailnet hosts' pods with `tailnet`, then from the pods of each `clientNamespaces`
namespace, and every other port of the listener's pod is admitted to nobody. The listener binds
`0.0.0.0` and carries no `ENVOY_API_TOKEN`. What lets its `/v1` answer off loopback depends on who
reaches it:

- A `tailnet.listener` with no `clientNamespaces` is reached by the tailnet hosts alone and
  carries `ENVOY_API_ALLOW_UNAUTHENTICATED=1`, so its `/v1` answers with no credential. The
  NetworkPolicy is what keeps `/v1` to the hosts, and it keeps it only where the cluster enforces
  NetworkPolicy. The listener answers 421 to every `Host` that is neither a loopback name or
  address nor one of `ENVOY_ALLOWED_HOSTS`, which stops a page that rebinds its own name rather
  than a caller the NetworkPolicy admits, since such a caller reaches `/v1` with `Host:
  localhost`.
- A listener declared as the hub's own `listener`, with or without `tailnet`, or a
  `tailnet.listener` with `clientNamespaces`, carries the token check instead:
  `ENVOY_OIDC_ISSUER`, `ENVOY_OIDC_AUDIENCE` and `ENVOY_OIDC_ACCOUNTS`, from its `oidcIssuer`,
  `oidcAudience` and `oidcAccounts`. Every caller, a tailnet host's included, presents a
  projected ServiceAccount token that issuer signed for that audience, of a subject the list
  names; any other request gets 401, whatever the NetworkPolicy admits. It renders no
  `ENVOY_ALLOWED_HOSTS`, since a listener that requires a bearer serves `/v1` to any `Host`.
  Its image must be `legion-envoy-v30.32.0` or later, the first release whose listener reads
  `ENVOY_OIDC_ACCOUNTS`. An older listener reads the issuer and audience, so it still requires a
  bearer, but takes no account list and admits every subject that issuer signs a token for the
  audience, so any pod the NetworkPolicy admits could call `/v1` as any session. The component
  reads the image only as a digest, which says nothing of its release, so it cannot refuse an
  older one: the deployment pins a digest of that release or later.

The hub's own NetworkPolicy `<n>` admits this pod to NATS's 4222 beside the hosts.

How it is updated. A change of the listener's image, size or environment rolls the one pod with
`maxUnavailable: 0` and `maxSurge: 1`, so the new pod starts before the old one stops. The new pod
is Ready only once its `/v1` serves, since its startup probe passes only then (below), and `/v1`
opens without waiting for the durable, which the old pod holds until it stops, so the new pod's
`/v1` cannot wait on the old pod. The new pod's listener starts its bind of the durable right
after its `/v1` opens, and the old pod is sent SIGTERM only once the new pod is Ready, at most one
10-second period of the startup probe after that open. The old pod's stop then takes 33 seconds
in all: it keeps answering for 5 seconds, the time its removal from the Service's endpoints takes
to reach every node's proxy, and then stops in 28 seconds, inside its grace period of 40. That is
a margin of 7 seconds (`listenerGraceMargin`), which covers the kubelet's own latency between the
preStop hook's return and SIGTERM, the container's teardown and a late tick on a loaded node. It
does not cover a listener that runs over its own deadlines. What has to fit under the new pod's
135-second bind deadline (`durableBindDeadline`), counted from its `/v1` open, is the startup
probe's lag behind that open plus the old pod's stop. A hung old pod holds its durable for all 40
seconds, so that lag may be up to 95 seconds, and the probe's period keeps it at most 10. A new
pod that never becomes Ready leaves the old one serving: nothing stops it, and the new one restarts
on its bind deadline, or on its startup probe when its `/v1` never opens. The Tailscale Service's
port 9020 refuses connections while no listener pod is ready, and its port 4222 serves through
it. Setting, changing or removing the listener's `senderMessagesPerMinute` or `senderMessageBurst`
is a change of the listener's environment, so it rolls the pod the same way. The new pod's listener
starts with every sender's count full, as any restart of the listener does.

What is watched. The startup probe runs `/bin/sh -c` of
`curl -fsS --max-time 5 http://127.0.0.1:9020/metrics | grep -q '^envoy_active_interests '` every
10 seconds, with a 6-second timeout and 60 failures allowed: ten minutes for `/v1` to open, after
which the kubelet restarts the container. `/metrics` takes no credential, and the listener writes
the sample line `envoy_active_interests <n>` there only once its `/v1` gate is open and its
interest list can be served; the pattern never matches the gauge's `# HELP` or `# TYPE` line,
which every scrape carries. The kubelet runs neither of the other two probes, and the pod is not
Ready, until this one has passed. A listener older than `legion-envoy-v29.15.0` writes the sample
from its first answer, so there the probe passes at that answer. Readiness and liveness read
`/livez` with `curl -sS`, which the listener answers 204 without reading the bus, and which exits
0 whatever the HTTP status, so both are status-blind: a NATS outage or any other fault of the
hub, which every pod shares, neither takes the one pod out of the Service nor restarts it. Only
30 seconds of no answer at all takes the pod out of the Service (readiness, every 10 seconds, 3
failures allowed), and a minute of it restarts the container (liveness, every 10 seconds, 6
failures allowed). No probe reads `/healthz`, which the listener answers 200 `starting` from its
first answer, before its `/v1` opens.
The PodDisruptionBudget `<n>-listener` allows one unavailable pod, so a node drain or an
Autopilot upgrade moves the pod rather than stalling on it. The pod-down alert below names this
pod and its container.

### Metrics and alerts

Each hub pod's `prom-exporter` container runs prometheus-nats-exporter 0.20.2 with the chart's
arguments (`-connz -routez -subz -varz -prefix=nats -use_internal_server_id -jsz=all` and the
server's monitoring URL), and Google Managed Prometheus scrapes it through the PodMonitoring `<n>`
every 30 seconds. Its stream metrics, as the local cluster test logged them, are:

- `nats_stream_consumer_count`
- `nats_stream_first_seq`
- `nats_stream_last_seq`
- `nats_stream_limit_bytes`
- `nats_stream_limit_messages`
- `nats_stream_subject_count`
- `nats_stream_total_bytes`
- `nats_stream_total_messages`

Each series is one server's copy of one stream: every server holding a copy serves one series of
each metric for it, so a stream of three copies has three `nats_stream_last_seq` series at rest,
two while one of its servers is down, and a stream of one copy has one. Every series carries the
exporter's stream labels (prometheus-nats-exporter 0.20.2, `collector/jsz.go`): `server_id`,
`server_name`, `cluster`, `domain`, `meta_leader`, `is_meta_leader`, `account`, `account_name`,
`account_id`, `stream_name`, `stream_leader`, `is_stream_leader` and `stream_raft_group`.
`server_name` is the pod's name, `<n>-0` to `<n>-2`, and `stream_name` the stream's: the
listener's notification stream is `ENVOY_NOTIFICATIONS`, and a KV bucket is the stream
`KV_<bucket>`. Google Managed Prometheus adds its own target labels beside these.

No metric says whether a copy is current, so a copy counts as current when its `last_seq` equals
the highest any copy of that stream reports. The streams with fewer than three current copies are:

```promql
count by (stream_name) (
  nats_stream_last_seq
    == on (stream_name) group_left ()
  max by (stream_name) (nats_stream_last_seq)
) < 3
```

A stream matches when a server holding a copy is down, when a copy is behind, or when the stream
keeps fewer than three copies. With `natsReplicas: 3` every stream and bucket the listener creates
keeps three; a stream another client creates with fewer always matches, so scope the query with a
`stream_name` matcher, such as `{stream_name=~"ENVOY_NOTIFICATIONS|KV_.+"}` on both selectors,
when the hub holds any. Each exporter scrapes its own server at its own moment, so under steady
writes one copy can trail another by a few messages in a single scrape: give the alert a `for`
duration longer than a few scrapes. A stream whose every server is down has no series at all,
which the pod-down alert covers.

A pod that is down, a hub pod (`<n>-0` to `<n>-2`, container `nats`), a tailnet host
(`<n>-tailnet-…`, container `tailscale`) or the session listener (`<n>-listener-…`, container
`listener`), is read from Cloud Monitoring's GKE system metrics for containers,
`kubernetes.io/container/*` on the `k8s_container` resource, by `namespace_name`, `pod_name` and
`container_name`: for example `kubernetes.io/container/uptime` absent, or
`kubernetes.io/container/restart_count` rising. GKE reports them itself, so they hold for a pod
whose exporter is gone with it.

