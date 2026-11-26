## Your pod

Every role of your issue runs in one pod and shares its memory limit, so a command that fills it can end every role's turn. In the pod, run only a compile of what you touched, the tests you added or changed by name, and `go vet -tags e2e ./...`. A whole test suite, a whole module's tests, a browser suite and a large build run in CI on the pull request, or where your deployment's instructions send heavy work.
