# Contracts

- A limit or word that the Dispatch server and a client both enforce is one exported constant here, which `scripts/gen-go.ts` emits into Go as `contracts.<Name>`.
  Add it to that script's imports and its render, and never write the value a second time, in either language.

- Dispatch, the listener and each plugin release on their own, and each takes these shapes at the commit it was built from, so a writer and a reader of one shape are often of different releases.
  Add a field as optional, and keep every reader taking a field it does not know without refusing it.

- A `.passthrough()` keeps unknown keys only at its own level: a plain `z.object` nested in it still drops every key it does not declare.
  Declare a new wire field in each nested `z.object` that a reader takes it from.

Where this file and the code disagree, the code is right: follow the code and correct this file.
