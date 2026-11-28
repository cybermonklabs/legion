## Layout

1. A component that shows several elements in a row names its groups, in a fixed order, in its doc comment. Each element is in one group.
2. When a row is too narrow, a whole group moves to the next line. The entries of a group may wrap inside it. Text that does not fit truncates inside its own element, with the whole text as its title, or wraps inside it. It never pushes the next element out.
3. A list of chips that comes from data shows at most 10 entries while it is closed, then "+N", which shows the rest in place. `components/CappedGroup.tsx` does this and marks the list `data-layout-group`.
4. Nothing scrolls sideways, and nothing is cut off sideways without an ellipsis. A code block, a picture, a data table and a box marked `data-layout-scroll="board"` may scroll inside their own box.
5. An element is in a header or a row only when a reader acts on it there, or when it says that something is wrong now. Every other property is in the component's Details, as a label and a value. Nothing is removed.
6. Nothing appears, hides or moves because of a state the reader cannot see or set. The reader sets the width and opens each disclosure.

`bun run e2e:layout` checks points 3 and 4, and the cut text of point 2, at 360, 900, 1280, 1440 and 1920 px. A reviewer checks the other points.
