# A failure in the shared workspace

Read it before you report a script that failed in the workspace, or a run whose output you did not capture (step 4 of `skill://legion-worker`).

A non-zero exit of a script in the shared workspace is not reportable until you run it again with `sha256sum <script>` just before and just after it, both lines kept beside the exit status:

- a changed hash is the race;
- an unchanged hash with a failure that comes again is a finding;
- an unchanged hash with a failure that does not come again is unclassifiable.

Three limits hold:

- A file's `mtime` proves little, because a checkout or a workspace rebuild stamps every path at once: an `mtime` inside the run's window proves nothing, one outside it still rules out the race, and `.jj/legion-created`, written once when the workspace was made, tells such a stamp from a real write.
- A pod replacement takes the `mtime` evidence of every run before it.
- A run whose output you did not capture can be neither confirmed nor cleared, so it is unclassifiable.
