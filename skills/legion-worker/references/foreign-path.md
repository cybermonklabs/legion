# A path you did not write

Read this before the turn's first `jj new` or commit, in a workspace you share, and list the paths of the working copy and of your branch against origin:

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" diff -r @ --name-only
jj -R "$LEGION_WORKSPACE" diff --from "legion/<issue>@origin" --to @ --name-only
```

Every path yours: commit and push as your role prompt says. Before this branch's first push, read the second list against your default branch at origin instead of `legion/<issue>@origin`.

- The path is only in the second list, or already inside a commit of yours: split nothing, and tell the architect.
- The path is in the first list (the working copy `@`): take your own paths out of `@`, without `jj new` first:

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" split -m '<issue>: <what you did>' <your paths…>
jj -R "$LEGION_WORKSPACE" metaedit -r @- --update-author
jj -R "$LEGION_WORKSPACE" bookmark set legion/<issue> -r @-
jj -R "$LEGION_WORKSPACE" git push --bookmark legion/<issue>
```

1. Always pass `-m`: it writes your trailers on the commit that holds your paths. This split is the one exception to never splitting the working copy you found.
2. The metaedit fills an empty author, which `jj git push` refuses.
3. The other role's path stays in the remainder `@`. Never put the bookmark on the remainder: that pushes the other role's file.
4. Read the first list again, then tell the architect what it held.
5. Recover forward (`jj new`, `jj restore <paths>`), never with `jj undo`, `jj abandon` or `jj op restore`.
