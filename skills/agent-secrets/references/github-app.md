# A GitHub App secret

Read this before you use a GitHub App secret.

- For a GitHub App secret, `--github-owner <account> --github-token` gives the command that one account's token, the string an `Authorization` header carries; `--github-repo` and `--github-read-only` narrow it further.
  Without `--github-token` the name holds the JSON object of installations, and GitHub answers it `401`.
- The broker checks a narrowing before the approval request, so a narrowing it cannot serve costs nobody an approval.
  It refuses the request, nothing runs, and the status is 1, in five cases:

  - an account the secret's `github-owners` tag does not list,
  - a run that names no GitHub App secret,
  - a name behind the airlock (the bullet below),
  - a narrowing GitHub itself refuses, such as a repository the App's installation does not reach, or read access the App does not hold,
  - an account the App is not installed on, which the message says in the words `has no installation on`. Install the App on that account, or narrow to an account it is on.

  The message says which of the five it is.
  In two cases the refusal comes when the grant is read instead: a request every name of which is granted at once, which no person decides, and a request on which the broker reaches no verdict, for example because GitHub is out of reach or refuses the App itself, or because the check passes its own time bounds.
- A GitHub narrowing and an airlocked name cannot share a run: ask for the GitHub App secret in a run of its own.
