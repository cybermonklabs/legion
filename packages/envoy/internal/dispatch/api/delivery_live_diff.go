package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// deliveryLiveEncoded is one computed body, encoded once: the body as the GET route writes it, and
// each list item, map entry, count and whole field with its revision, so every subscriber of the
// view diffs against the one encoding. revs is every revision the body holds: a snapshot sends it,
// and a subscriber sent a delta against it holds it afterwards. It is shared by every subscriber
// of the view and never mutated.
type deliveryLiveEncoded struct {
	kind   string
	body   json.RawMessage
	items  map[string][]deliveryLiveItem
	maps   map[string]map[string]deliveryLiveValue
	counts map[string]map[string]map[string]int
	fields map[string]deliveryLiveValue
	revs   DeliveryLiveRevs
}

// deliveryLiveItem is one list item: its id (a string id as it is, a numeric one in decimal), its
// encoding and its revision.
type deliveryLiveItem struct {
	id  string
	raw json.RawMessage
	rev string
}

// deliveryLiveValue is one map entry's or whole field's encoding and revision.
type deliveryLiveValue struct {
	raw json.RawMessage
	rev string
}

// deliveryLiveMapReviser is a body that revises and encodes the entries of a map of its own, from
// the typed values each entry is built of, so encodeDeliveryBody neither reads such an entry back
// out of the body's encoding nor encodes it again to revise it: a Legion board's issues
// (legionBoardResponse's mapRevisions and encodedMap), thousands of entries on each recompute.
type deliveryLiveMapReviser interface {
	// mapRevisions is the revision of every entry of the map the body writes under name, by the
	// entry's key, and false for a map it does not revise.
	mapRevisions(name string) (map[string]string, bool)
	// encodedMap is the name of that map, each of its entries' encoding as json.Marshal of the
	// body writes the entry, none for a nil map, and the body with the map left out (nil), whose
	// json.Marshal writes every other field of the body. encodeDeliveryBody writes the body from
	// the two (composeDeliveryBody), byte for byte the body json.Marshal writes, and reads back
	// only the fields of the rest.
	encodedMap() (name string, entries []deliveryLiveEntry, rest any, err error)
}

// deliveryLiveEntry is one entry of a map a body encodes itself (deliveryLiveMapReviser): its key
// and its encoding.
type deliveryLiveEntry struct {
	key string
	raw json.RawMessage
}

// encodeDeliveryBody encodes body, a view of kind's GET body, by its kind's field classes. It
// refuses a list item without an id, and two items of one list with the same id, since either
// would leave a page holding something no delta can reach. A body that encodes a map of its own
// (deliveryLiveMapReviser: a Legion board's issues entries, each encoded and revised from the
// typed values the board built it of) is written from those entries and the rest of the body
// (composeDeliveryBody), and the map's revisions, which must name every entry it holds and no
// other, are the body's own; every other body is json.Marshal's, read back by field, and a map
// entry's revision is that of its encoding (deliveryLiveRevisionWithout). The duration is how
// long its revisions took, the recompute's revision stage (deliveryRecomputeTimes), which the
// caller takes from its own time of the call to leave the encode stage. The revisions are timed
// where they are made, entry by entry, since each entry's encoding is read just before its
// revision in the same loop, and a map the body revises as one call.
func encodeDeliveryBody(kind string, body any) (deliveryLiveEncoded, time.Duration, error) {
	var revising time.Duration
	classes, ok := deliveryLiveKinds[kind]
	if !ok {
		return deliveryLiveEncoded{}, revising, fmt.Errorf("delivery live: unknown view kind %q", kind)
	}
	var raw json.RawMessage
	var top map[string]json.RawMessage
	var own string
	var owned []deliveryLiveEntry
	reviser, revises := body.(deliveryLiveMapReviser)
	if revises {
		name, entries, rest, err := reviser.encodedMap()
		if err != nil {
			return deliveryLiveEncoded{}, revising, fmt.Errorf("delivery live: encode %s body: %w", kind, err)
		}
		if raw, top, err = composeDeliveryBody(name, entries, rest); err != nil {
			return deliveryLiveEncoded{}, revising, fmt.Errorf("delivery live: encode %s body: %w", kind, err)
		}
		own, owned = name, entries
	} else {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return deliveryLiveEncoded{}, revising, fmt.Errorf("delivery live: encode %s body: %w", kind, err)
		}
		if err := json.Unmarshal(raw, &top); err != nil {
			return deliveryLiveEncoded{}, revising, fmt.Errorf("delivery live: read %s body: %w", kind, err)
		}
	}
	field := func(name string) (json.RawMessage, error) {
		value, ok := top[name]
		if !ok {
			return nil, fmt.Errorf("delivery live: the %s body has no field %q", kind, name)
		}
		return value, nil
	}
	encoded := deliveryLiveEncoded{
		kind:   kind,
		body:   raw,
		items:  make(map[string][]deliveryLiveItem, len(classes.lists)),
		maps:   make(map[string]map[string]deliveryLiveValue, len(classes.maps)),
		counts: make(map[string]map[string]map[string]int, len(classes.counts)),
		fields: make(map[string]deliveryLiveValue, len(classes.whole)+len(classes.withChanges)),
		revs: DeliveryLiveRevs{
			Items:  make(map[string]map[string]string, len(classes.lists)),
			Maps:   make(map[string]map[string]string, len(classes.maps)),
			Counts: make(map[string]map[string]map[string]string, len(classes.counts)),
			Fields: make(map[string]string, len(classes.whole)+len(classes.withChanges)),
		},
	}

	for _, list := range classes.lists {
		value, err := field(list)
		if err != nil {
			return deliveryLiveEncoded{}, revising, err
		}
		var items []json.RawMessage
		if err := json.Unmarshal(value, &items); err != nil {
			return deliveryLiveEncoded{}, revising, fmt.Errorf("delivery live: read %s.%s: %w", kind, list, err)
		}
		encodedItems := make([]deliveryLiveItem, 0, len(items))
		revs := make(map[string]string, len(items))
		for _, item := range items {
			id, err := deliveryLiveItemID(item)
			if err != nil {
				return deliveryLiveEncoded{}, revising, fmt.Errorf("delivery live: %s.%s: %w", kind, list, err)
			}
			if _, duplicate := revs[id]; duplicate {
				return deliveryLiveEncoded{}, revising, fmt.Errorf("delivery live: %s.%s holds id %q twice", kind, list, id)
			}
			started := time.Now()
			rev := deliveryLiveRevision(item)
			revising += time.Since(started)
			revs[id] = rev
			encodedItems = append(encodedItems, deliveryLiveItem{id: id, raw: item, rev: rev})
		}
		encoded.items[list] = encodedItems
		encoded.revs.Items[list] = revs
	}

	for name, excluded := range classes.maps {
		if revises && name == own {
			started := time.Now()
			revs, revised := reviser.mapRevisions(name)
			revising += time.Since(started)
			if !revised {
				return deliveryLiveEncoded{}, revising, fmt.Errorf("delivery live: %s.%s is encoded by its body and revised by none", kind, name)
			}
			if len(revs) != len(owned) {
				return deliveryLiveEncoded{}, revising, fmt.Errorf("delivery live: %s.%s revises %d entries and holds %d", kind, name, len(revs), len(owned))
			}
			values := make(map[string]deliveryLiveValue, len(owned))
			for _, entry := range owned {
				rev, held := revs[entry.key]
				if !held {
					return deliveryLiveEncoded{}, revising, fmt.Errorf("delivery live: %s.%s[%q] has no revision", kind, name, entry.key)
				}
				values[entry.key] = deliveryLiveValue{raw: entry.raw, rev: rev}
			}
			encoded.maps[name] = values
			encoded.revs.Maps[name] = revs
			continue
		}
		value, err := field(name)
		if err != nil {
			return deliveryLiveEncoded{}, revising, err
		}
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(value, &entries); err != nil {
			return deliveryLiveEncoded{}, revising, fmt.Errorf("delivery live: read %s.%s: %w", kind, name, err)
		}
		revs := make(map[string]string, len(entries))
		values := make(map[string]deliveryLiveValue, len(entries))
		for key, entry := range entries {
			started := time.Now()
			rev, err := deliveryLiveRevisionWithout(entry, excluded)
			revising += time.Since(started)
			if err != nil {
				return deliveryLiveEncoded{}, revising, fmt.Errorf("delivery live: %s.%s[%q]: %w", kind, name, key, err)
			}
			values[key] = deliveryLiveValue{raw: entry, rev: rev}
			revs[key] = rev
		}
		encoded.maps[name] = values
		encoded.revs.Maps[name] = revs
	}

	for _, name := range classes.counts {
		value, err := field(name)
		if err != nil {
			return deliveryLiveEncoded{}, revising, err
		}
		var facets map[string]map[string]int
		if err := json.Unmarshal(value, &facets); err != nil {
			return deliveryLiveEncoded{}, revising, fmt.Errorf("delivery live: read %s.%s: %w", kind, name, err)
		}
		revs := make(map[string]map[string]string, len(facets))
		for facet, counts := range facets {
			facetRevs := make(map[string]string, len(counts))
			started := time.Now()
			for key, count := range counts {
				facetRevs[key] = deliveryLiveCountRevision(count)
			}
			revising += time.Since(started)
			revs[facet] = facetRevs
		}
		encoded.counts[name] = facets
		encoded.revs.Counts[name] = revs
	}

	for name, excluded := range classes.whole {
		value, err := field(name)
		if err != nil {
			return deliveryLiveEncoded{}, revising, err
		}
		started := time.Now()
		rev, err := deliveryLiveRevisionWithout(value, excluded)
		revising += time.Since(started)
		if err != nil {
			return deliveryLiveEncoded{}, revising, fmt.Errorf("delivery live: %s.%s: %w", kind, name, err)
		}
		encoded.fields[name] = deliveryLiveValue{raw: value, rev: rev}
		encoded.revs.Fields[name] = rev
	}
	for _, name := range classes.withChanges {
		value, err := field(name)
		if err != nil {
			return deliveryLiveEncoded{}, revising, err
		}
		started := time.Now()
		rev := deliveryLiveRevision(value)
		revising += time.Since(started)
		encoded.fields[name] = deliveryLiveValue{raw: value, rev: rev}
		encoded.revs.Fields[name] = rev
	}
	return encoded, revising, nil
}

// composeDeliveryBody writes the body of a view whose body encodes its map under name itself
// (deliveryLiveMapReviser's encodedMap), and answers its top-level fields by name. The body is
// the encoding of rest, the body with that map left out, with the map's null in it replaced by the
// map written from its entries: in their keys' sorted order, each key as encoding/json writes a
// string, or null where there are none for a nil map. That is how json.Marshal writes a map of
// strings, so the body is byte for byte json.Marshal's of the whole body. Only rest is read back,
// for its fields, and never the map's entries; the field under name is the map as written.
func composeDeliveryBody(name string, entries []deliveryLiveEntry, rest any) (json.RawMessage, map[string]json.RawMessage, error) {
	without, err := json.Marshal(rest)
	if err != nil {
		return nil, nil, err
	}
	top := map[string]json.RawMessage{}
	decoder := json.NewDecoder(bytes.NewReader(without))
	if _, err := decoder.Token(); err != nil {
		return nil, nil, fmt.Errorf("read the body without %s: %w", name, err)
	}
	at := -1
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, nil, fmt.Errorf("read the body without %s: %w", name, err)
		}
		member, _ := token.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, nil, fmt.Errorf("read the body without %s: %w", name, err)
		}
		if member != name {
			top[member] = value
			continue
		}
		if string(value) != "null" {
			return nil, nil, fmt.Errorf("the body without %s holds %s in its place, not null", name, value)
		}
		at = int(decoder.InputOffset()) - len("null")
	}
	if at < 0 {
		return nil, nil, fmt.Errorf("the body has no field %q", name)
	}
	slices.SortFunc(entries, func(a, b deliveryLiveEntry) int { return strings.Compare(a.key, b.key) })
	size := len(without)
	for _, entry := range entries {
		size += len(`"":,`) + len(entry.key) + len(entry.raw)
	}
	body := make([]byte, 0, size)
	body = append(body, without[:at]...)
	if entries == nil {
		body = append(body, "null"...)
	} else {
		body = append(body, '{')
		for index, entry := range entries {
			if index > 0 {
				body = append(body, ',')
			}
			body = append(append(appendJSONString(body, entry.key), ':'), entry.raw...)
		}
		body = append(body, '}')
	}
	written := len(body)
	body = append(body, without[at+len("null"):]...)
	top[name] = body[at:written]
	return body, top, nil
}

// deliveryLiveItemID is a list item's "id": a string as it is, a number in its JSON form.
func deliveryLiveItemID(item json.RawMessage) (string, error) {
	var keyed struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(item, &keyed); err != nil {
		return "", err
	}
	switch {
	case len(keyed.ID) == 0 || string(keyed.ID) == "null":
		return "", errors.New("an item has no id")
	case keyed.ID[0] == '"':
		var id string
		if err := json.Unmarshal(keyed.ID, &id); err != nil {
			return "", err
		}
		return id, nil
	default:
		return string(keyed.ID), nil
	}
}

// deliveryLiveCountRevision is a count's revision: its JSON encoding's.
func deliveryLiveCountRevision(count int) string {
	return deliveryLiveRevision(strconv.AppendInt(nil, int64(count), 10))
}

// deliveryLiveRevisionWithout is a whole field's or a map entry's revision, over its encoding
// without the subfields excluded leaves out: the timeline's measures move their computed_at,
// window and freshness on every compute, and a page is sent the measures only when a number in
// them moved.
func deliveryLiveRevisionWithout(value json.RawMessage, excluded []string) (string, error) {
	if len(excluded) == 0 {
		return deliveryLiveRevision(value), nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil {
		return "", err
	}
	for _, name := range excluded {
		delete(object, name)
	}
	kept, err := json.Marshal(object)
	if err != nil {
		return "", err
	}
	return deliveryLiveRevision(kept), nil
}

// snapshot is the snapshot frame of e for one subscription: the body, the change position it
// reflects, when that change was recorded, and every revision.
func (e deliveryLiveEncoded) snapshot(sub string, seq int64, changedAt *time.Time) DeliveryLiveSnapshot {
	return DeliveryLiveSnapshot{
		Type: "snapshot", Sub: sub, Seq: strconv.FormatInt(seq, 10), ChangedAt: changedAt,
		Body: e.body, Revs: e.revs,
	}
}

// deliveryLiveNull is a map entry's value in a delta when the key left.
var deliveryLiveNull = json.RawMessage("null")

// diffDeliveryBody is what a subscriber holding held is sent so that it holds next: every list item
// whose revision differs (whole, in the body's order) or that left (by id), every map entry and
// count that differs (null for one that left), and every whole field that differs. The bool is
// false when none of those differs, and the delta is then not sent; a field sent only with changes
// (the window) never makes a delta by itself, and is included whenever it moved. Sub, Seq and
// ChangedAt are the caller's.
// After a delta is sent, the subscriber holds next.revs.
func diffDeliveryBody(next deliveryLiveEncoded, held DeliveryLiveRevs) (DeliveryLiveDelta, bool) {
	classes := deliveryLiveKinds[next.kind]
	delta := DeliveryLiveDelta{
		Type:   "delta",
		Set:    map[string]json.RawMessage{},
		Items:  map[string]DeliveryLiveItemChanges{},
		Maps:   map[string]map[string]json.RawMessage{},
		Counts: map[string]map[string]map[string]*int{},
	}
	changed := false

	for _, list := range classes.lists {
		heldRevs := held.Items[list]
		nextRevs := next.revs.Items[list]
		changes := DeliveryLiveItemChanges{Upsert: []json.RawMessage{}, Remove: []string{}}
		revs := map[string]*string{}
		for _, item := range next.items[list] {
			if heldRevs[item.id] == item.rev {
				continue
			}
			rev := item.rev
			changes.Upsert = append(changes.Upsert, item.raw)
			revs[item.id] = &rev
		}
		for id := range heldRevs {
			if _, kept := nextRevs[id]; !kept {
				changes.Remove = append(changes.Remove, id)
				revs[id] = nil
			}
		}
		if len(revs) == 0 {
			continue
		}
		slices.Sort(changes.Remove)
		delta.Items[list] = changes
		if delta.Revs.Items == nil {
			delta.Revs.Items = map[string]map[string]*string{}
		}
		delta.Revs.Items[list] = revs
		changed = true
	}

	for name := range classes.maps {
		heldRevs := held.Maps[name]
		nextValues := next.maps[name]
		values := map[string]json.RawMessage{}
		revs := map[string]*string{}
		for key, value := range nextValues {
			if heldRevs[key] == value.rev {
				continue
			}
			rev := value.rev
			values[key] = value.raw
			revs[key] = &rev
		}
		for key := range heldRevs {
			if _, kept := nextValues[key]; !kept {
				values[key] = deliveryLiveNull
				revs[key] = nil
			}
		}
		if len(revs) == 0 {
			continue
		}
		delta.Maps[name] = values
		if delta.Revs.Maps == nil {
			delta.Revs.Maps = map[string]map[string]*string{}
		}
		delta.Revs.Maps[name] = revs
		changed = true
	}

	for _, name := range classes.counts {
		heldFacets := held.Counts[name]
		nextFacets := next.counts[name]
		nextRevs := next.revs.Counts[name]
		counts := map[string]map[string]*int{}
		revs := map[string]map[string]*string{}
		note := func(facet, key string, count *int, rev *string) {
			if counts[facet] == nil {
				counts[facet] = map[string]*int{}
				revs[facet] = map[string]*string{}
			}
			counts[facet][key] = count
			revs[facet][key] = rev
		}
		for facet, values := range nextFacets {
			heldRevs := heldFacets[facet]
			for key, count := range values {
				rev := nextRevs[facet][key]
				if heldRevs[key] == rev {
					continue
				}
				count := count
				note(facet, key, &count, &rev)
			}
		}
		for facet, heldRevs := range heldFacets {
			for key := range heldRevs {
				if _, kept := nextFacets[facet][key]; !kept {
					note(facet, key, nil, nil)
				}
			}
		}
		if len(counts) == 0 {
			continue
		}
		delta.Counts[name] = counts
		if delta.Revs.Counts == nil {
			delta.Revs.Counts = map[string]map[string]map[string]*string{}
		}
		delta.Revs.Counts[name] = revs
		changed = true
	}

	for name := range classes.whole {
		value := next.fields[name]
		if held.Fields[name] == value.rev {
			continue
		}
		delta.Set[name] = value.raw
		if delta.Revs.Fields == nil {
			delta.Revs.Fields = map[string]string{}
		}
		delta.Revs.Fields[name] = value.rev
		changed = true
	}

	if !changed {
		return DeliveryLiveDelta{}, false
	}
	for _, name := range classes.withChanges {
		value := next.fields[name]
		if held.Fields[name] == value.rev {
			continue
		}
		delta.Set[name] = value.raw
		if delta.Revs.Fields == nil {
			delta.Revs.Fields = map[string]string{}
		}
		delta.Revs.Fields[name] = value.rev
	}
	return delta, true
}
