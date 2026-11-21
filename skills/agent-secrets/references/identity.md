# The broker identity, and a person's own machine

Read this when the client exits 1 naming no broker identity or `agent-secrets-helper`, and when you run on a person's own machine.

The client takes its broker identity from the first of four sources that has one, and needs no registered agent session: a launcher's key directory, the person's own `agent-secrets login`, the helper for a process inside a session it registered, and the machine's own cloud identity, which it finds with no setting once the broker names its exchange audience.
`agent-secrets identity --source` names the source it would use.
When the command exits 1 naming no broker identity, each source says why it had nothing:

- **`helper: this process is in no session the helper … registered`.** The process inherited the helper's socket but is not in a session the helper registered. That alone stops nothing: the client goes on to the machine.
- **`machine: the broker at … names no exchange audience`.** The broker accepts no machine identity yet. Restart the agent as a session: `agent-secrets register --wait 10 --exec -- <the agent's command>`. `agent-secrets login` refuses on this machine until the person sets `AGENT_SECRETS_MACHINE_IDENTITY` (`metadata` or `pool`).
- **`this machine's agent-secrets-helper holds no credential at the secrets broker`.** The part after `the helper:` names the cure.
  `run: agent-secrets machine login`: the person logs the machine in, as https://cybermonklabs.github.io/legion/broker/guides/log-a-machine-in/ shows.
  `the helper's trade loop is trading …`: the helper gets one itself; report it, and retry later.
  `the helper stopped trading …`: nothing here cures it, so report it with the helper's log.

A stopped helper is not a missing one. Where the helper is installed, or `AGENT_SECRETS_HELPER_SOCK` names its socket, and the socket is gone, the command exits 1 with `agent-secrets-helper at … so the helper is stopped`, and it does not act as the machine instead.
Report that line; the person starts the helper (`systemctl --user start agent-secrets-helper`).

On `UNKNOWN_SECRET` you stop, as `skill://agent-secrets` says. You may tell the secret's owner how to find the cause: `agent-secrets secret list` and `agent-secrets secret show NAME`, under a cloud sign-in that may read the store (`gcloud auth login` against a broker on Google Cloud, an `AWS_PROFILE` against one on AWS).
