package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// newsRecord is one design-news record of a task's text as its reader reads it
// (texts.DesignNewsRecord, workflow.RenderNews): each field by its key, each row of each table by
// its column, and the words of each block by its field path, such as items[0].reason.
type newsRecord struct {
	fields map[string]string
	rows   map[string][]map[string]string
	words  map[string]string
}

// newsKindLine is the line that opens a design-news record.
const newsKindLine = "kind: " + string(texts.DesignNews)

// parseNewsRecords is each design-news record of text, a task the daemon wrote, in its order: each
// record opens on its kind line at the start of text or after a blank line, and runs to the first
// line that is no field, no table row and no block of its own, a blank line or the reviewer's round
// line among them. It answers an error for a record whose table holds fewer rows than its header
// names, or whose block never closes.
func parseNewsRecords(text string) ([]newsRecord, error) {
	lines := strings.Split(text, "\n")
	var records []newsRecord
	for i := 0; i < len(lines); i++ {
		if lines[i] != newsKindLine || (i > 0 && lines[i-1] != "") {
			continue
		}
		rec := newsRecord{fields: map[string]string{}, rows: map[string][]map[string]string{}, words: map[string]string{}}
		for ; i < len(lines); i++ {
			line := lines[i]
			if open := goldenOpenTag.FindStringSubmatch(line); open != nil {
				closing := "</" + open[1] + ">"
				end := i + 1
				for end < len(lines) && lines[end] != closing {
					end++
				}
				if end == len(lines) {
					return nil, fmt.Errorf("the block %s of a news record never closes", open[1])
				}
				path, _, _ := strings.Cut(open[1], "-")
				for _, attribute := range goldenAttribute.FindAllStringSubmatch(open[2], -1) {
					if attribute[1] == "field" {
						path = goldenUnescape(attribute[2])
					}
				}
				rec.words[path] = strings.Join(lines[i+1:end], "\n")
				i = end
				continue
			}
			match := goldenLine.FindStringSubmatch(line)
			if match == nil {
				break
			}
			key, count, header, rest := match[1], match[2], match[3], match[4]
			if header == "" {
				value, err := goldenValue(rest)
				if err != nil {
					return nil, fmt.Errorf("the field %s of a news record: %w", key, err)
				}
				rec.fields[key] = value
				continue
			}
			columns := strings.Split(header, ",")
			var n int
			fmt.Sscan(count, &n)
			for range n {
				i++
				if i >= len(lines) || !strings.HasPrefix(lines[i], "  ") {
					return nil, fmt.Errorf("the table %s of a news record holds fewer rows than %d", key, n)
				}
				cells, err := goldenSplit(lines[i][2:])
				if err != nil || len(cells) != len(columns) {
					return nil, fmt.Errorf("the row %q of the table %s of a news record: %v", lines[i], key, err)
				}
				row := map[string]string{}
				for j, column := range columns {
					row[column] = cells[j]
				}
				rec.rows[key] = append(rec.rows[key], row)
			}
		}
		records = append(records, rec)
	}
	return records, nil
}

// newsRecords is parseNewsRecords, failing the test where text holds a record it cannot read.
func newsRecords(t *testing.T, text string) []newsRecord {
	t.Helper()
	records, err := parseNewsRecords(text)
	if err != nil {
		t.Fatalf("%v:\n%s", err, text)
	}
	return records
}

// oneNewsRecord is the one design-news record of text, failing the test where text holds another
// count of them.
func oneNewsRecord(t *testing.T, text string) newsRecord {
	t.Helper()
	records := newsRecords(t, text)
	if len(records) != 1 {
		t.Fatalf("the task holds %d news records, want one:\n%s", len(records), text)
	}
	return records[0]
}

// newsRecordOf is the design-news record of text to audience about issue, failing the test where
// text holds none.
func newsRecordOf(t *testing.T, text, audience, issue string) newsRecord {
	t.Helper()
	for _, rec := range newsRecords(t, text) {
		if rec.fields["audience"] == audience && rec.fields["issue"] == issue {
			return rec
		}
	}
	t.Fatalf("the task holds no news record to %s of %s:\n%s", audience, issue, text)
	return newsRecord{}
}

// newsItemsIn is the items of every design-news record of text, in their order (items).
func newsItemsIn(t *testing.T, text string) []string {
	t.Helper()
	var items []string
	for _, rec := range newsRecords(t, text) {
		items = append(items, rec.items()...)
	}
	return items
}

// newsSaid is what every design-news record of text says, a line for each: its gate (gate), each
// item (items) and each section and block it names (named). A test reads a fact of the news as a
// part of one of these lines.
func newsSaid(t *testing.T, text string) string {
	t.Helper()
	var lines []string
	for _, rec := range newsRecords(t, text) {
		if gate := rec.gate(); gate != "" {
			lines = append(lines, gate)
		}
		lines = append(lines, rec.items()...)
		lines = append(lines, rec.named()...)
	}
	return strings.Join(lines, "\n")
}

// newsTells says whether some line of what the news of text says holds fragment (newsSaid).
func newsTells(t *testing.T, text, fragment string) bool {
	t.Helper()
	return strings.Contains(newsSaid(t, text), fragment)
}

// newsCount is how many times fragment stands in what the news of text says (newsSaid).
func newsCount(t *testing.T, text, fragment string) int {
	t.Helper()
	return strings.Count(newsSaid(t, text), fragment)
}

// items is each item of the record on one line: its step, its artifact, the version approved now,
// the version taken, yours where the record names it, what changed, and the reason where a block
// gives one.
func (r newsRecord) items() []string {
	lines := make([]string, len(r.rows["items"]))
	for i, row := range r.rows["items"] {
		line := fmt.Sprintf("%s %s %s taken=%s", row["step"], row["artifact"], row["version"], row["taken"])
		if yours, named := row["yours"]; named {
			line += " yours=" + yours
		}
		line += " " + row["changes"]
		if reason, given := r.words[fmt.Sprintf("items[%d].reason", i)]; given {
			line += fmt.Sprintf(" reason=%q", reason)
		}
		lines[i] = line
	}
	return lines
}

// members is each member of a root architect's summary on one line: its key, its phase, how it is
// told and why, its holder where the record names one, and the issue whose architect holds it.
func (r newsRecord) members() []string {
	lines := make([]string, len(r.rows["members"]))
	for i, row := range r.rows["members"] {
		lines[i] = fmt.Sprintf("%s %s told=%s why=%s holder=%s architect_of=%s", row["issue"], row["phase"], row["told"], row["why"],
			row["holder"], row["architect_of"])
	}
	return lines
}

// member is the line of the member issue (members), "" where the record names none.
func (r newsRecord) member(issue string) string {
	for i, row := range r.rows["members"] {
		if row["issue"] == issue {
			return r.members()[i]
		}
	}
	return ""
}

// owed is each item a member is owed on one line: the member and the line of the item (items).
func (r newsRecord) owed() []string {
	items := r.items()
	lines := make([]string, len(r.rows["owed"]))
	for i, row := range r.rows["owed"] {
		var at int
		fmt.Sscan(row["item"], &at)
		item := "no item"
		if at < len(items) {
			item = items[at]
		}
		lines[i] = row["issue"] + ": " + item
	}
	return lines
}

// gate is the gate the record carries on one line, "" where it carries none: open or closed, its
// step, its artifact, its version and what it does to a merge.
func (r newsRecord) gate() string {
	if r.fields["gate"] == "" {
		return ""
	}
	return fmt.Sprintf("%s %s %s %s merge=%s", r.fields["gate"], r.fields["gate_step"], r.fields["gate_artifact"], r.fields["gate_version"],
		r.fields["merge"])
}

// named is each heading and block id the record's section and block rows name, by the words of
// their blocks, in their order: what a reader reads of what changed.
func (r newsRecord) named() []string {
	var names []string
	for i, row := range r.rows["sections"] {
		heading := r.words[fmt.Sprintf("sections[%d].heading", i)]
		if row["unit"] == texts.NewsUnitAfterSection {
			heading = "after " + r.words[fmt.Sprintf("sections[%d].after_heading", i)]
		}
		names = append(names, fmt.Sprintf("%s %s %s", row["item"], heading, row["change"]))
	}
	for i, row := range r.rows["blocks"] {
		names = append(names, fmt.Sprintf("%s %s %s %s", row["item"], row["type"], r.words[fmt.Sprintf("blocks[%d].id", i)], row["change"]))
	}
	return names
}

// wantNewsLines fails the test unless got is want, line for line.
func wantNewsLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the news's %s =\n  %s\nwant\n  %s", what, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// publishedGates is, for each role message published to token's role topic, in order, the gate a
// gate-moved message carries on the line gate writes ("open spec artifact-800 4 merge=released"),
// and "" for a message of another kind: the fields its reader reads, as the outbox published them.
func (r *newsPathRig) publishedGates(t *testing.T, token claim.Token) []string {
	t.Helper()
	var gates []string
	for _, published := range r.notices.publishes() {
		if published.topic != notify.RoleTopicPrefix+string(token) {
			continue
		}
		encoded, err := json.Marshal(published.payload)
		if err != nil {
			t.Fatalf("encode the payload of %q: %v", published.message, err)
		}
		var moved texts.GateMovedRecord
		if err := json.Unmarshal(encoded, &moved); err != nil || moved.Kind != texts.GateMoved {
			gates = append(gates, "")
			continue
		}
		gates = append(gates, fmt.Sprintf("%s %s %s %d merge=%s", moved.Gate, moved.Step, moved.Artifact, moved.Version, moved.Merge))
	}
	return gates
}
