// Package texts holds the one list of the texts that the daemon sends an agent session, and the
// code that writes those texts. It imports only the standard library, claim and phase, so every
// package of the daemon that builds a text can import it.
//
// # The list
//
// Each text has a kind. The file kinds.go holds every kind constant and the one list of
// entries. An entry names the kind, its form, the zero value of its record type, its readers and
// its urgency rule. The form is one of these:
//
//   - Task: a text that the daemon sends as a prompt. The parts that a start puts before a task
//     are tasks too.
//   - Notice: a notice to the architect that owns the issue.
//   - ControllerNotice: a notice to the controller.
//   - RoleMessage: a message that the daemon publishes to a role.
//
// Readers are the roles that read the kind's text: the architect that owns the issue, the
// controller, or the phase roles that the kind reaches. The slice is shared, so a caller does not
// change it.
//
// A script reads each quoted lower-case word of kinds.go as a kind. So kinds.go has no import,
// no struct tag and no quoted word that is not a kind name. A test of this package holds these
// rules.
//
// # Records
//
// A record is a struct that declares the fields of one kind. Each kind has a record type of its
// own, named for its constant: PhaseFinishedRecord for PhaseFinished. The record types are in
// these files:
//
//   - records_task.go: the tasks;
//   - records_start.go: the parts that a start puts before a task;
//   - records_notice.go: the notices to the architect that owns an issue;
//   - records_controller.go: the notices to the controller;
//   - records_role.go: the role messages;
//   - records.go: the parts that several records share, and the helpers that write a pull request,
//     the command that lists its checks, the review that ends a stuck round and the short digest of
//     an image the same way in every text.
//
// Each field with a json tag is a field of the kind, and its json name is the key. A field with no
// json tag, or with the tag "-", is not a field of the kind. An embedded struct with no json tag
// gives its fields in its place, as encoding/json reads it. A record opens with its kind and its
// issue (TaskHead, NoticeHead, RoleHead). Three records do not: the look task, whose lead line
// names the issue; the controller's start message, which has no field; and the hook prompt, whose
// first line names the hook and the issue.
//
// A notice stays one record.Notice on the wire. So the json name of each field of a notice's
// record is a key of record.Notice, a key has one Go type in every notice record, and a key that
// record.Notice had before these records keeps the JSON shape it had. A nested shape of a notice,
// such as the failure or the catch-up, is declared here, and record either names it as an alias
// (record.NoticeFailure) or keeps a copy of the same shape (record.CatchUp).
// A role message is published as an object of its record's fields, apart from the hook prompt,
// which is published as its text (HookPromptRecord.Text).
//
// A field of a task record holds one of these values:
//
//   - a primitive: a string, a bool, an integer, a time, or a pointer to one of them;
//   - a list of primitives;
//   - a list of records that are flat and uniform: each field of each record holds a
//     primitive, and each record of the list has the same keys.
//
// Render refuses every other value with an error: a map, a nested record, a float, a list of
// lists, and a list of records that are not flat or not uniform. A record of a list that leaves
// out a field by omitempty, where another record of the list has it, is not uniform.
//
// # Render
//
// Render writes a task record as its text. The text has these parts, in this order:
//
//  1. The lead line, where the record has the method Lead() string. Render writes the line
//     word for word. The look task uses it for its first line.
//  2. The fields, in the order of their declaration, in the form that encode of
//     @toon-format/toon 4.1.1 writes with its default options. A field is a key: value line. A
//     list of primitives is one key[n]: a,b line. A list of records is a key[n]{f,g}: line,
//     then one row for each record, with an indent of two spaces. An empty list or a nil list
//     is key: []. A string that TOON cannot write bare is quoted and escaped as TOON does it.
//     TOON has no escape for the next line (U+0085) and the line and paragraph separators
//     (U+2028, U+2029), which a reader takes for a line break, so each of them in a string is
//     written as a space, and the string stays on its line.
//     A time is RFC 3339 in UTC, with the fraction of a second only where the time has one.
//     A nil pointer is null. Omitempty and omitzero leave out a field as encoding/json does.
//  3. The kept sentences, where the record has the method Kept() []string. Each sentence is
//     a line of its own, word for word. These are the sentences that another requirement
//     keeps by their words.
//  4. The blocks of the record's Words fields, in the order of their declaration.
//
// The text has no other part, and no new line at its end.
//
// # Words
//
// A task record holds the words of another author in a field of type Words or *Words. It is not
// a key: value line. Render draws it as a block after the fields, and the field's json name is
// the name of the block. A Words field of a record in a list draws its block too. That block has
// the attribute field, which gives the path of the words, such as kept[1].answer. The index of
// the path counts from 0, as a JSON path does, so kept[1] is the second record of kept. A nil
// *Words, and Words with no text and no mark, draw no block.
//
// A notice or role-message record holds the words of another author in a string field with the
// struct tag texts:"words", since its payload is an object and not a text. Such a record also
// declares the field Words, of type map[string]WordsMeta with the json name words: the payload's
// words object. It names each words field that is set by the field's path, with the author, the
// time, and the rest or the mark that more follows. A record with no words field declares no words
// object. WordsPaths lists the paths of the words fields of a record that are set, in the order
// of their declaration, such as summary or catch_up.kept.decisions[1].answer, for the words object
// of a payload. A task names no words object: Render draws its blocks itself.
//
// Render draws the blocks of a text together, after the rest of the text. The tag of each block
// is the first of its name, name-1, name-2 and on that neither the lines before the blocks (the
// lead, the fields and the kept sentences) nor the words of any block of the text hold as an
// opening or a closing tag (<tag or </tag), and that no earlier block of the text took. A field
// can hold another author's words too, such as an issue's title or a check's name, so nothing in
// the text can open or close the tag of a block but that block's own lines, and no two blocks
// share a tag. Block draws one block, as the whole of its text. Joined joins the records of one
// task, a start's own record and the news record of each audience, and draws every block of every
// record again under the tags the same rule gives them against all the other text of the task. A
// text that holds no record line (Drawn) is no record this release drew: an operator's task, or a
// task an earlier release queued, which is prose. Joined sends such a start as its author wrote it.
// InFront puts the notes a start puts before a task in front of that task's text, and draws the
// task's blocks again under the tags the same rule gives them against those notes too, since a
// note can hold words that are not the daemon's. InFrontOfWords puts them in front of a text no
// record drew, which goes as its author wrote it, and draws again against it the notes' blocks and
// those of each record drawn after it instead. The opening tag carries these attributes, in this
// order, each only where it is set:
//
//   - by: the author, where the fields of the text do not name the author;
//   - at: the time of the act, where the fields of the text do not give the time;
//   - field: the path of the words in a list;
//   - rest: the count of UTF-16 units that the bound cut off;
//   - more: true, where the event marked its body as cut or reached the daemon as a
//     payload_omitted stand-in, so the count of the rest is not known.
//
// An attribute value escapes markup, control characters below U+0020 and the three unescaped
// line breaks as XML character references. The words after it stay as given, between new lines.
//
// The bounds count UTF-16 units, as Dispatch counts a body: OneAct for the words of one act,
// EachOfSeveral for each act where a text carries several, and Summary for a worker's summary.
// Bound never splits a character outside the Basic Multilingual Plane. Carry applies a bound
// and sets rest or more. The daemon cuts words when it writes a text, never when it stores
// them. A notice's payload names each field that holds another author's words in its words
// object, keyed by the path of the field, with a WordsMeta for each.
//
// # Urgency
//
// A task has no urgency. It always starts a turn. A notice or a role message has the urgency
// that its entry's rule gives for the reader and the variant of the text:
//
//   - Low, where the reader can act on the text at its next turn;
//   - Med, where the reader must act on it before then.
//
// An entry with no rule gives Med. UrgencyOf reads the list. A notice's urgency comes from
// its row each time the daemon publishes it, so its payload does not carry the urgency.
package texts
