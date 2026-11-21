# Store a secret

Read this before you create a secret or replace its value.

`agent-secrets secret create NAME --owner me|shared|<email> --tier agent|human` creates one, and `agent-secrets secret set NAME` replaces the value of one that exists.
Both read the value from standard input or from a prompt with echo off, never from an argument: type it at the prompt, so it reaches no shell history.
`--tier agent` lets the owner's own sessions use the secret without asking, and every session for a `shared` one; any other session's request goes to the owner for approval.
`--tier human` sends every request to a person for approval: the owner, or anyone signed in to the secrets broker for a `shared` secret.
Where the broker's rules give the key a model judge, the judge decides each request that would go to a person, and a person decides only one the judge cannot decide; the `decides:` line of `agent-secrets secret show NAME` says which.

The `agent-secrets secret` forms (`list`, `show`, `create`, `set`, `retag`, `delete`, `restore`) call the broker's store under whatever cloud credential the environment gives them, and the store's IAM decides what each may read and write.
A write runs under the credential of the first source the command finds.
On Google Cloud the order is `CLOUDSDK_AUTH_ACCESS_TOKEN`, `GOOGLE_APPLICATION_CREDENTIALS`, `CLOUDSDK_ACTIVE_CONFIG_NAME` or `CLOUDSDK_CORE_ACCOUNT`, the well-known credentials file, the active gcloud account and last the metadata server; on AWS it is the AWS SDK's own: access keys in `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, then `AWS_WEB_IDENTITY_TOKEN_FILE`, then `AWS_PROFILE`, then `AWS_DEFAULT_PROFILE`, then the rest of the SDK's default chain.
Every form prints the credential it uses (`agent-secrets secret <form>: using …`) before it calls the store, and a store refusal names it and how to choose another.
On Google Cloud, a source that is present but broken stops the command; it never falls through to the machine's identity.
On AWS, a profile that holds no credential of its own signs in as the container's or the instance's role, and the `using …` line names it.

Never run one under a gcloud configuration or account that holds a person's login, because the write then runs as that person: ask the person to run it.
Where the box's active gcloud configuration holds a person's login, the active gcloud account, which comes before the metadata server, is that person's as well.
To write as the machine, name the machine's own gcloud login: set `CLOUDSDK_CORE_ACCOUNT` to the box's service account (on a Google VM `gcloud auth list` lists it), or `CLOUDSDK_ACTIVE_CONFIG_NAME` to the configuration that holds it.
`CLOUDSDK_CORE_ACCOUNT` outranks the account of any configuration, so unset it before you choose a configuration.

On Google Cloud, `create` makes the secret before it asks for the value, so a store that refuses the create asks for none.
Until the value is added, a failure or a signal deletes that empty secret again, except where the store's answer leaves open whether it stored the value: then `create` keeps the secret, exits 1, and names `agent-secrets secret show NAME` to check.
The broker calls a secret that waits for its first value pending for ten minutes from its making, and then refuses its name `UNKNOWN_SECRET`.
The create that made it deletes it when its prompt ends, at ten minutes or, where its owner was still typing then, when that entry ends; a create that was killed deletes nothing, and the secret stays in the store with no value.
Its owner sees it as `versions: none` in `agent-secrets secret show NAME`, and frees the name with `gcloud secrets delete <id> --project <project number>` (`<id>` is the part after `secrets/` of the `secret_name` that `show` prints), or stores the value with `agent-secrets secret set NAME`.
`agent-secrets secret delete NAME` only schedules the deletion 30 days out, and until then the name stays taken, so the next `secret create` of it is refused.

Tags, deletion and restore: https://cybermonklabs.github.io/legion/broker/guides/manage-a-secret/.
