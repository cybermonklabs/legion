package ompsessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// The custom entries pi-envoy writes into a session (packages/pi-envoy/extensions/envoy.ts):
// roleClaimEntry `{role}` each time the session claims an Envoy role (`{role: null}`, an older
// release's hand-back), managedEntry `{session_id}` when Legion drives the session.
const (
	roleClaimEntry = "envoy-role-claim"
	managedEntry   = "legion-managed-session"
)

// headBytes bounds what a scan reads of a session's head for its title slot and header, which are
// far smaller: the slot is 256 bytes and the header one short record.
const headBytes = 64 << 10

// Description is what a session says about itself: its title, header and the Legion role it
// claimed.
type Description struct {
	// Title and TitleSource are the title Oh My Pi shows (its title slot's, else the header's),
	// empty when it has none.
	Title       string
	TitleSource string
	// ID, Timestamp, Cwd and ParentSession are the header's; ParentSession is set for a subagent's
	// session.
	ID            string
	Timestamp     string
	Cwd           string
	ParentSession string
	// RoleClaim is the role token the session's last envoy-role-claim entry names, empty when it
	// has none or that entry handed the role back.
	RoleClaim string
	// ManagedSession is the session id the session's legion-managed-session entry names, empty
	// when it has none.
	ManagedSession string
}

// Describe reads a session's content for its Description: the title slot and header from its first
// records, and the role claim entries from every record. A line that is not JSON is skipped, as Oh
// My Pi skips it.
func Describe(content []byte) Description {
	var d Description
	_ = describe(&d, conversation.ContentLines(content), false)
	return d
}

// DescribeFile reads the session file at path for its Description, a line at a time. withClaims
// false stops after the header, reading only the file's first lines.
func DescribeFile(path string, withClaims bool) (Description, error) {
	file, err := os.Open(path)
	if err != nil {
		return Description{}, err
	}
	defer file.Close()
	var d Description
	if err := describe(&d, conversation.ReaderLines(file), !withClaims); err != nil {
		return Description{}, fmt.Errorf("read session %s: %w", path, err)
	}
	return d, nil
}

// describe fills d from read, stopping after the header when headOnly.
func describe(d *Description, read func() ([]byte, error), headOnly bool) error {
	first, sawHeader := true, false
	var headerTitle, headerSource string
	var slotTitle, slotSource string
	var hasSlot bool
	for {
		line, err := read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if !sawHeader {
			line = bytes.TrimSpace(line)
			wasFirst := first
			first = false
			if !json.Valid(line) {
				continue
			}
			if wasFirst {
				slotTitle, slotSource, hasSlot = ParseTitleSlot(line)
				if hasSlot {
					continue
				}
			}
			sawHeader = true
			var header struct {
				Type          string `json:"type"`
				ID            string `json:"id"`
				Title         string `json:"title"`
				TitleSource   string `json:"titleSource"`
				Timestamp     string `json:"timestamp"`
				Cwd           string `json:"cwd"`
				ParentSession string `json:"parentSession"`
			}
			if json.Unmarshal(line, &header) == nil && header.Type == "session" {
				d.ID, d.Timestamp, d.Cwd, d.ParentSession = header.ID, header.Timestamp, header.Cwd, header.ParentSession
				headerTitle, headerSource = header.Title, header.TitleSource
			}
			if headOnly {
				break
			}
			continue
		}
		if !bytes.Contains(line, []byte(roleClaimEntry)) && !bytes.Contains(line, []byte(managedEntry)) {
			continue
		}
		var custom struct {
			Type       string `json:"type"`
			CustomType string `json:"customType"`
			Data       struct {
				Role      *string `json:"role"`
				SessionID *string `json:"session_id"`
			} `json:"data"`
		}
		if json.Unmarshal(bytes.TrimSpace(line), &custom) != nil || custom.Type != "custom" {
			continue
		}
		switch {
		case custom.CustomType == roleClaimEntry:
			d.RoleClaim = ""
			if custom.Data.Role != nil {
				d.RoleClaim = *custom.Data.Role
			}
		case custom.CustomType == managedEntry && custom.Data.SessionID != nil:
			d.ManagedSession = *custom.Data.SessionID
		}
	}
	// The title slot, when the file has one, is the title: an empty one clears the header's
	// (session-loader.ts applyTitleSlot).
	if hasSlot {
		d.Title, d.TitleSource = slotTitle, slotSource
	} else {
		d.Title, d.TitleSource = headerTitle, headerSource
	}
	return nil
}

// ParseTitleSlot reads the fixed-width first line Oh My Pi keeps a session's title in
// (session-title-slot.ts). It returns false for a line that is not a title slot.
func ParseTitleSlot(line []byte) (title, source string, ok bool) {
	var slot struct {
		Type      string  `json:"type"`
		V         int     `json:"v"`
		Title     *string `json:"title"`
		Source    *string `json:"source"`
		UpdatedAt *string `json:"updatedAt"`
		Pad       *string `json:"pad"`
	}
	if json.Unmarshal(line, &slot) != nil || slot.Type != "title" || slot.V != 1 || slot.Title == nil || slot.UpdatedAt == nil || slot.Pad == nil {
		return "", "", false
	}
	if slot.Source != nil {
		if *slot.Source != "auto" && *slot.Source != "user" {
			return "", "", false
		}
		source = *slot.Source
	}
	return *slot.Title, source, true
}

// Titled is a session of the table with its title and header.
type Titled struct {
	Path string
	Description
}

// Titles lists every session the table holds with its title and header: the title from the row's
// title column, where Oh My Pi keeps a title it sets under SQL storage, or, when that is NULL (a
// row Import wrote), from the title slot or header of the content's head; the header from the
// content's head, read as Read reads a session, the row's content and then its parts in offset
// order, since Oh My Pi's own full write leaves the row's content empty and keeps the whole
// session in its parts (Replace). RoleClaim and ManagedSession are not read: Describe over the
// session's whole content reads them. A database without the session tables returns no sessions.
func Titles(ctx context.Context, conn *Conn) ([]Titled, error) {
	var titled []Titled
	err := inTransaction(ctx, conn, true, func(p pinned) error {
		titled = nil // A retried try starts over (inTransaction).
		// A part starting at headBytes or past it is wholly past the head, since the bytes before it
		// are exactly its start_offset, so the head is the row's content and the parts before that
		// mark, each cut to the head's length and taken in order.
		rows, err := p.query(ctx,
			"SELECT path, title, title_source, head FROM ("+
				"SELECT path, title, title_source, 0 AS kind, 0::bigint AS start_offset, substr(content, 1, $1) AS head FROM public.omp_session_files "+
				"UNION ALL "+
				"SELECT p.path, f.title, f.title_source, 1, p.start_offset, substr(p.content, 1, $1) "+
				"FROM public.omp_session_files_parts p JOIN public.omp_session_files f ON f.path = p.path WHERE p.start_offset < $1"+
				") pieces ORDER BY path, kind, start_offset", headBytes)
		if err != nil {
			return err
		}
		defer rows.Close()
		var head bytes.Buffer
		var path string
		var title, source *string
		started := false
		finishRow := func() error {
			if !started {
				return nil
			}
			var d Description
			if err := describe(&d, conversation.ContentLines(head.Bytes()), true); err != nil {
				return err
			}
			if title != nil {
				d.Title, d.TitleSource = *title, ""
				if source != nil {
					d.TitleSource = *source
				}
			}
			titled = append(titled, Titled{Path: path, Description: d})
			return nil
		}
		for rows.Next() {
			var rowPath, piece string
			var rowTitle, rowSource *string
			if err := rows.Scan(&rowPath, &rowTitle, &rowSource, &piece); err != nil {
				return err
			}
			if !started || rowPath != path {
				if err := finishRow(); err != nil {
					return err
				}
				head.Reset()
				path, title, source, started = rowPath, rowTitle, rowSource, true
			}
			if head.Len() < headBytes {
				head.WriteString(piece)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return finishRow()
	})
	if isUndefinedTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return titled, nil
}
