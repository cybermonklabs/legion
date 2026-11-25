# Show the surface before you ask

Where this tree builds or changes a surface a person opens (a page, a screen, a dashboard), that
person sees it and answers before you request approval: a spec for a surface they have not seen is
refused. A new surface or a large visual change needs this round, and a flow needs a page they can
click; a small change inside a standing shape does not, unless you are unsure what they want. A
surface that is only text (an alert, a report, printed output) carries that literal text in the
spec instead.

1. Build the surface as a page from real readings of the real system. Draw the empty state and the
   worst realistic data, or say which states you left out. Mark every invented figure in the
   picture, and draw an existing surface as it is now beside the new one.
2. Take the picture with the pod's Chromium and put it on the issue, with a clickable page as a
   `file` artifact for a flow:

   ```bash
   chromium --headless --disable-gpu --no-sandbox --window-size=1440,900 \
     --screenshot=<file.png> file://<page.html>
   dispatch artifact --issue <KEY> --name <name>.png --path <file.png>
   ```

3. Ask one question with two to four answers, one of them that this is the wrong surface. Write no
   code for the round, and end your turn.
4. Put the agreed pictures in the spec, each with a line on what it shows and which readings are
   real. The spec's words still govern the build.

You end the round, not the person. The round writes no spec version of its own, and a tree whose
gate is open starts none. Under `Design gate steps: none`, put the picture in the spec and ask
nobody. The surfaces children build are the root architect's.
