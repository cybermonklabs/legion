# The layout of a Kubernetes volume

`volume-layout.txt` is the layout the kubelet gives a Secret volume that projects keys through `items` with nested paths, as `find <mount> -printf '%y %P %l\n'` lists it.
A key cannot hold a `/`, so a folder with `roles/…` and `skills/<skill>/…` can be mounted from a Secret or a ConfigMap only through `items`, and the kubelet then shows the folder as links into a timestamped directory.
`TestReadTakesTheKubernetesVolumeLayout` builds this layout in a temporary directory and reads it with `texts.Read`.

## The command that captured it

```sh
find /run/legion/gh -printf '%y %P %l\n'
```

The mount is the `gh` volume of a Legion issue pod: a Secret volume at `/run/legion/gh` that projects its keys through `items` with nested paths, one directory for each GitHub owner.
Each line is the entry's type (`d`, `f` or `l`), its path under the mount (empty for the mount itself), and, for a link, its target.
The listing is in the order `find` printed it.

The kubelet's shape shows in it:

- `..data` is a link to the one timestamped directory, `..2026_11_25_21_26_25.2316854546`.

- That directory holds every file at its nested path.

- Each top-level entry (`renames.txt` and the three owner directories) is a link into `..data/<entry>`, so a reader that skips names starting with `..` and follows a link that stays inside the mount reads each file once.

## What was changed

Only names were read: the capture lists entries and opens no file.
The three owner directories carry the names of real GitHub owners, and the repository is public, so each owner name is replaced in every line with a neutral placeholder: `acme`, `example-user` and `example-org`.
The rest is as `find` printed it.
No pod name and no issue key is in the file.
