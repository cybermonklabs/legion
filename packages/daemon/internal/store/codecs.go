package store

import (
	"context"
	"fmt"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sjawhar/legion/daemon/internal/store/storetext"
)

// registerStorableCodecs is the AfterConnect of every pool PoolConfig configures: the codec rule.
// On the new connection's type map it wraps pgx's own codec of each text type (text, varchar,
// bpchar, name) in a storableCodec that applies storetext.TextBytes, and pgx's own codec of each
// JSON type (json, jsonb) in one that applies storetext.JSON. The daemon's columns are text,
// text[], jsonb and bytea; the other types are wrapped too, so a statement that types a parameter
// as one of them ($1::varchar, say) passes the rule as well. bytea is not wrapped: it holds bytes,
// not text.
//
// The rule is a codec's because only the codec knows the type of the parameter it encodes, which
// decides between the text rule and the JSON rule: the same Go string can be a text column's
// value or a jsonb column's JSON text. pgx learns a parameter's type from the server, which is why
// PoolConfig refuses the two exec modes that never ask it.
func registerStorableCodecs(_ context.Context, conn *pgx.Conn) error {
	types := conn.TypeMap()
	for _, name := range []string{"text", "varchar", "bpchar", "name"} {
		if err := wrapCodec(types, name, storetext.TextBytes, false); err != nil {
			return err
		}
	}
	if err := wrapCodec(types, "json", storetext.JSON, false); err != nil {
		return err
	}
	return wrapCodec(types, "jsonb", storetext.JSON, true)
}

// wrapCodec registers on types, under name's own name and OID, a storableCodec over the codec
// types holds for name now. Elements of an array of that type are wrapped too: pgx's ArrayCodec
// encodes each element by the element type's OID through the same map.
func wrapCodec(types *pgtype.Map, name string, rule func([]byte) []byte, versioned bool) error {
	inner, ok := types.TypeForName(name)
	if !ok {
		return fmt.Errorf("the connection's type map has no %s type to wrap in the store's text rule", name)
	}
	types.RegisterType(&pgtype.Type{
		Name:  inner.Name,
		OID:   inner.OID,
		Codec: &storableCodec{Codec: inner.Codec, rule: rule, versioned: versioned},
	})
	return nil
}

// storableCodec is pgx's own codec of a text or JSON type with the store's text rule applied to
// every value it encodes. Decoding, FormatSupported and PreferredFormat are the inner codec's, so
// a value is read back as it is stored.
type storableCodec struct {
	pgtype.Codec
	// rule is the rule of the type: storetext.TextBytes or storetext.JSON. It returns its argument
	// itself when nothing changes, and a new slice otherwise.
	rule func([]byte) []byte
	// versioned is jsonb's: its binary format starts with a version byte, which the rule does not
	// read.
	versioned bool
}

// PlanEncode is the inner codec's plan, with the rule applied to the bytes it appends. A value the
// inner codec plans no encoding for, but pgx's map would turn into a plain string, is unwrapped
// here as the map would unwrap it, and the string is planned by the inner codec: a *string
// (pgtype.TryWrapDerefPointerEncodePlan, so a nil one stays NULL) and a value of a named string
// type (pgtype.TryWrapFindUnderlyingTypeEncodePlan). Left to the map, such a value would become a
// plain string in text format, which the map sends as it is, past every codec
// (pgtype.Map.planEncode); that is also why the argument rule (storableArgs) gives a plain string
// that the rule changes as a *string.
func (c *storableCodec) PlanEncode(m *pgtype.Map, oid uint32, format int16, value any) pgtype.EncodePlan {
	skip := 0
	if c.versioned && format == pgtype.BinaryFormatCode {
		skip = 1
	}
	if plan := c.Codec.PlanEncode(m, oid, format, value); plan != nil {
		return &storablePlan{next: plan, rule: c.rule, skip: skip}
	}
	unwrap, plain, ok := plainString(value)
	if !ok {
		return nil
	}
	plan := c.Codec.PlanEncode(m, oid, format, plain)
	if plan == nil {
		return nil
	}
	unwrap.SetNext(plan)
	return &storablePlan{next: unwrap, rule: c.rule, skip: skip}
}

// plainString is pgx's own unwrapping of a *string or a value of a named string type to a plain
// string, and false for any other value.
func plainString(value any) (pgtype.WrappedEncodePlanNextSetter, any, bool) {
	if _, ok := value.(*string); ok {
		return pgtype.TryWrapDerefPointerEncodePlan(value)
	}
	if value != nil && reflect.TypeOf(value).Kind() == reflect.String {
		return pgtype.TryWrapFindUnderlyingTypeEncodePlan(value)
	}
	return nil, nil, false
}

// storablePlan runs the inner plan and then applies the rule to the bytes that plan appended,
// after skip bytes of the format's own (jsonb's version byte). When the rule changes them, the
// result is written over them. A value the rule does not change costs no allocation.
type storablePlan struct {
	next pgtype.EncodePlan
	rule func([]byte) []byte
	skip int
}

func (p *storablePlan) Encode(value any, buf []byte) ([]byte, error) {
	start := len(buf) + p.skip
	out, err := p.next.Encode(value, buf)
	if err != nil || out == nil || len(out) < start {
		return out, err
	}
	encoded := out[start:]
	storable := p.rule(encoded)
	if len(storable) == len(encoded) && (len(encoded) == 0 || &storable[0] == &encoded[0]) {
		return out, nil
	}
	// The rule never writes into its argument and gives a new slice when it changes it, so
	// storable shares no memory with out.
	return append(out[:start], storable...), nil
}
