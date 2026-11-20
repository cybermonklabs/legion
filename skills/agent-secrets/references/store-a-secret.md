# Store a secret

Read this before you create a secret or replace its value.

`agent-secrets secret create NAME --owner me|shared|<email> --tier agent|human` creates one, and `agent-secrets secret set NAME` replaces the value of one that exists.
Both read the value from standard input or from a prompt with echo off, never from an argument: type it at the prompt, so it reaches no shell history.
They run under the cloud credential the environment gives them and print it first (`agent-secrets secret <form>: using …`).
Never run one under a gcloud configuration or account that holds a person's login, because the write then runs as that person: ask the person to run it.
Tags, deletion and restore: https://cybermonklabs.github.io/legion/broker/guides/manage-a-secret/.
