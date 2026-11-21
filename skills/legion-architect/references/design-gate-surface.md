# Showing the surface before you ask

Part of `skill://legion-architect/references/design-gate.md`, "Show the surface before you ask".
Read this when this tree builds or changes a surface a person opens or reads.

## Which changes need the round

Whether a change needs the round is your judgement. These readings inform it:

- A new surface a person opens needs it, and so does a large visual change to one: a region, a
  panel, a column or a step of a flow added, removed or moved. The larger the visual change, the
  more a page the person can click is worth over a picture: a person judges a layout from a
  picture, and judges a flow only by acting on it.
- A small change inside a shape that stands (a label, a copy change, a style fix, a bug fix) does
  not, unless the person's words do not settle what it should show, which is itself a reason to
  show them.
- A surface that is only text (an alert, a report, a command's printed output) carries that
  literal text in the spec, at the values it will carry, and needs no round.
- A change with no surface a person reads needs nothing.

## The round

Show the surface rather than describe it, let the person act on it where the flow is the
question, ask what they expected rather than whether they approve, and show another round on an
answer that asks for changes.

1. Build the surface as a page from real readings of the real system. Draw the empty state and the
   worst realistic data (the largest counts, the longest texts and the error states the real
   system holds), or say which states you left out. Mark every invented figure in the picture, and
   draw an existing surface as it is now beside the new one.
2. Use the cheapest picture that answers the question: a labelled sketch where it answers it, or
   the real surface built read-only where that costs no more than the mock-up.
3. Take the picture with the pod's Chromium and put it on the issue:

   ```bash
   chromium --headless --disable-gpu --no-sandbox --window-size=1440,900 \
     --screenshot=<file.png> file://<page.html>
   dispatch artifact --issue <KEY> --name <name>.png --path <file.png>
   ```

4. For a large visual change, or where the question is about a flow rather than a layout, build
   the page so the person can click it, put it up beside the pictures as a `file` artifact, and
   say in your question what works in it and what does not.
5. Ask one question with two to four answers, one of them that this is the wrong surface. Write no
   code for the round, and end your turn.
6. Put the agreed pictures in the spec, each with a line on what it shows and which readings are
   real, and one line on the states they do not show. The spec's words still govern the build.

You end the round, not the person: when their answer is carried into the pictures or the spec, go
on to the approval request with the pictures as they then stand. A person who stops answering
holds nothing up.

## The order

- The round writes no spec version of its own: the agreed pictures go into the version you first
  submit for approval.
- A tree whose gate is open starts no round for the work in flight and writes no spec version for
  a picture: a new version closes the gate, which holds every merge of the tree until a person
  approves again; the running phases go on.
- A picture that must change after approval is a new version only when what is built changes, and
  then you request approval again ("After approval" in
  `skill://legion-architect/references/design-gate.md`).
- Under `Design gate steps: none`, put the picture in the spec and ask nobody.
- The surfaces children build are the root architect's.
