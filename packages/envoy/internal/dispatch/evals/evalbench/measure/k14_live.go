package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// k14's open pages: each is one page of a project's Evals tab, a socket of its own subscribed to
// one evals view (the list GET /api/v1/projects/{key}/evals over the view's window, its compare
// range where it names one), held open for the whole of k14's writes, as a person's page is. A
// page holds what the page would show: the view's snapshot with every delta the server sent since
// applied to it, item by item (the evals view's items are sent eval by eval, by id,
// api/live_frames.go), and it keeps, for each item, every value it has held and when it came, so a
// write's update is read off the page itself, at the moment its frame arrived.

// pageItem is one value a page has held of an item: the item as canonical JSON ("" once the item
// left the page) and when the frame that brought it arrived.
type pageItem struct {
	value string
	at    time.Time
}

// livePage is one open page.
type livePage struct {
	name, project string
	window        window
	compare       *window
	path          string
	conn          *websocket.Conn

	mu      sync.Mutex
	ready   chan struct{}
	items   map[string]string
	history map[string][]pageItem
	deltas  int
	err     error
}

// canonical is value as JSON with its keys sorted and its numbers as written, so the same item a
// delta sends and a GET answers compares equal byte for byte whatever each encoder escaped.
func canonical(value json.RawMessage) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(decoded)
	return string(encoded), err
}

// itemsOf reads an evals list body's items by id, each as canonical JSON.
func itemsOf(body []byte) (map[string]string, error) {
	var answer struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, err
	}
	if answer.Items == nil {
		return nil, fmt.Errorf("the body holds no items: %.200s", body)
	}
	items := make(map[string]string, len(answer.Items))
	for _, raw := range answer.Items {
		id, value, err := keyed(raw)
		if err != nil {
			return nil, err
		}
		items[id] = value
	}
	return items, nil
}

// keyed is an item's id and its canonical JSON.
func keyed(raw json.RawMessage) (string, string, error) {
	var item struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &item); err != nil || item.ID == "" {
		return "", "", fmt.Errorf("an item with no id: %.200s", raw)
	}
	value, err := canonical(raw)
	return item.ID, value, err
}

// evalsView is the evals view of project over w, with compare where it names one, as the Evals tab
// subscribes it: the list query's own keys (its compare range and its page) beside the window.
func evalsView(project string, w window, compare *window) map[string]any {
	query := evalListQuery(w, compare, "")
	query.Del("from")
	query.Del("to")
	return map[string]any{"kind": "evals", "project": project, "window": w.live(), "query": query.Encode()}
}

// openPage opens one page: it subscribes the view over GET /ws/live with the agent token and waits
// for its snapshot, and reads every later frame until it is closed.
func openPage(ctx context.Context, base, token, name, project string, w window, compare *window) (*livePage, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second}
	conn, _, err := dialer.DialContext(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws/live", http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		return nil, fmt.Errorf("open the live socket of page %s: %w", name, err)
	}
	page := &livePage{name: name, project: project, window: w, compare: compare, path: evalListPath(project, w, compare, ""), conn: conn,
		ready: make(chan struct{}), items: map[string]string{}, history: map[string][]pageItem{}}
	if err := conn.WriteJSON(map[string]any{"type": "subscribe", "sub": name, "view": evalsView(project, w, compare)}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("subscribe page %s: %w", name, err)
	}
	go page.read()
	select {
	case <-page.ready:
	case <-time.After(2 * time.Minute):
		page.close()
		return nil, fmt.Errorf("page %s had no snapshot two minutes after it subscribed", name)
	}
	if err := page.failed(); err != nil {
		page.close()
		return nil, err
	}
	return page, nil
}

// read reads the page's frames until the socket closes: its snapshot, then each delta, each applied
// as it arrives.
func (p *livePage) read() {
	readied := false
	ready := func() {
		if !readied {
			readied = true
			close(p.ready)
		}
	}
	defer ready()
	for {
		var frame struct {
			Type  string          `json:"type"`
			Sub   string          `json:"sub"`
			Body  json.RawMessage `json:"body"`
			Items map[string]struct {
				Upsert []json.RawMessage `json:"upsert"`
				Remove []string          `json:"remove"`
			} `json:"items"`
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := p.conn.ReadJSON(&frame); err != nil {
			p.fail(fmt.Errorf("page %s: read the live socket: %w", p.name, err))
			return
		}
		at := time.Now()
		switch {
		case frame.Type == "error":
			p.fail(fmt.Errorf("page %s: the server refused the view: %s %s", p.name, frame.Code, frame.Message))
			return
		case frame.Sub != p.name:
		case frame.Type == "snapshot":
			items, err := itemsOf(frame.Body)
			if err != nil {
				p.fail(fmt.Errorf("page %s: read its snapshot: %w", p.name, err))
				return
			}
			p.mu.Lock()
			p.items = items
			for id, value := range items {
				p.history[id] = append(p.history[id], pageItem{value: value, at: at})
			}
			p.mu.Unlock()
			ready()
		case frame.Type == "delta":
			changes := frame.Items["items"]
			p.mu.Lock()
			p.deltas++
			for _, raw := range changes.Upsert {
				id, value, err := keyed(raw)
				if err != nil {
					p.mu.Unlock()
					p.fail(fmt.Errorf("page %s: read a delta: %w", p.name, err))
					return
				}
				p.items[id] = value
				p.history[id] = append(p.history[id], pageItem{value: value, at: at})
			}
			for _, id := range changes.Remove {
				delete(p.items, id)
				p.history[id] = append(p.history[id], pageItem{at: at})
			}
			p.mu.Unlock()
		}
	}
}

// fail records the page's first failure.
func (p *livePage) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil {
		p.err = err
	}
}

// failed is the page's failure, nil while it reads.
func (p *livePage) failed() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// value is what the page shows of item id now ("" where it holds none).
func (p *livePage) value(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.items[id]
}

// shown is every item the page shows now.
func (p *livePage) shown() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.items)
}

// changesOf is every value the page held of item id that arrived after from, in order.
func (p *livePage) changesOf(id string, from time.Time) []pageItem {
	p.mu.Lock()
	defer p.mu.Unlock()
	var after []pageItem
	for _, one := range p.history[id] {
		if one.at.After(from) {
			after = append(after, one)
		}
	}
	return after
}

// close unsubscribes nothing: it closes the socket, as a page that is closed does.
func (p *livePage) close() {
	_ = p.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	_ = p.conn.Close()
}

// pageReach is how one page met one write (reachOf): the moment it first showed want, the value a
// fresh read answers once the write has settled, and showed it to the end; how many frames changed
// the watched item after the write; and whether every item it shows then is the fresh read's
// (differing names the ids that are not, sorted).
type pageReach struct {
	page      string
	reached   *time.Time
	updates   int
	differing []string
}

// reachOf is how page met a write sent at sent, whose watched item fresh answers as want once it
// has settled, and whose items fresh answers whole as freshItems.
func reachOf(page *livePage, id string, sent time.Time, want string, freshItems map[string]string) pageReach {
	reach := pageReach{page: page.name}
	changes := page.changesOf(id, sent)
	reach.updates = len(changes)
	for at := len(changes) - 1; at >= 0 && changes[at].value == want; at-- {
		reach.reached = &changes[at].at
	}
	shown := page.shown()
	for _, key := range slices.Sorted(maps.Keys(freshItems)) {
		if shown[key] != freshItems[key] {
			reach.differing = append(reach.differing, key)
		}
	}
	for key := range shown {
		if _, held := freshItems[key]; !held {
			reach.differing = append(reach.differing, key)
		}
	}
	slices.Sort(reach.differing)
	return reach
}
