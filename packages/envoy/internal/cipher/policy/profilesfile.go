package policy

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// profilesFile is the profiles file as its author writes it: one JSON object, {"kinds": [...],
// "profiles": [...]}. Kinds is optional, since a file whose names are none of them behind the
// proxy declares no kind, and a file written before kinds existed has none.
type profilesFile struct {
	Kinds    []proxyrules.Kind `json:"kinds"`
	Profiles *[]profileEntry   `json:"profiles"`
}

// decodeProfiles is the strict read both readers of a profiles file share (ParseProfiles,
// DecodeProfiles). It decodes the file once, with encoding/json/v2 under encoding/json's own
// options, so a name matches a field whatever its case, an object may repeat a name and a string
// that is not UTF-8 is read with U+FFFD in its place, as encoding/json read every file the broker
// served before; but it rejects a member the profiles file does not name, and it reports
// each fault as encoding/json/v2 does, with the JSON pointer and the byte offset of the fault, from
// which the refusal names the field and the byte offset, line and column, since a person reads the
// file by line. It refuses:
//
//   - a file that is empty or white space alone, which holds no object, and a file that ends inside
//     its object;
//   - text that is not JSON, naming the byte at fault, such as the trailing comma of a list;
//   - a field the broker does not know, at any depth, naming where its name starts, so a file
//     written for a later broker is not served with that field's rule missing;
//   - a value of another type than its field takes, and a file that is a JSON value but an object;
//   - a file that is null, which would otherwise read as a file of no profiles;
//   - anything after the file's one object but white space: a second value no reader applies would
//     be a rule its author believes is in force, and an object that ends at a stray bracket is one
//     its author meant to go on;
//   - a file with no profiles list, or a null one: a file of no profiles says so, {"profiles": []};
//   - a profile's proxy key that is neither a kind's name nor an object (proxyField), naming the
//     form it is and the forms it takes.
func decodeProfiles(data []byte) ([]proxyrules.Kind, []profileEntry, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(data), json.DefaultOptionsV1())
	var file *profilesFile
	err := jsonv2.UnmarshalDecode(dec, &file, json.DefaultOptionsV1(), jsonv2.RejectUnknownMembers(true), json.ReportErrorsWithLegacySemantics(false))
	var semantic *jsonv2.SemanticError
	var syntactic *jsontext.SyntacticError
	var form *proxyFormError
	switch {
	case errors.Is(err, io.EOF):
		return nil, nil, errors.New("the file is empty: it holds no JSON object")
	case errors.As(err, &semantic) && errors.Is(err, jsonv2.ErrUnknownName):
		return nil, nil, fmt.Errorf("unknown field %q%s, at %s", semantic.JSONPointer.LastToken(), within(semantic.JSONPointer.Parent()), position(data, semantic.ByteOffset))
	case errors.As(err, &semantic) && errors.As(err, &form):
		return nil, nil, fmt.Errorf("%s %s, at %s", dotted(semantic.JSONPointer), form.why, position(data, semantic.ByteOffset))
	case errors.As(err, &semantic) && semantic.JSONPointer == "":
		return nil, nil, fmt.Errorf("the file is a JSON %s, not one JSON object, at %s", kindWords(semantic.JSONKind), position(data, semantic.ByteOffset))
	case errors.As(err, &semantic):
		return nil, nil, fmt.Errorf("%s is not a %s, at %s%s", dotted(semantic.JSONPointer), semantic.GoType, position(data, semantic.ByteOffset), because(semantic.Err))
	case errors.As(err, &syntactic) && errors.Is(err, io.ErrUnexpectedEOF):
		return nil, nil, fmt.Errorf("not JSON: the file ends inside its JSON object, at %s", position(data, syntactic.ByteOffset))
	case errors.As(err, &syntactic):
		return nil, nil, fmt.Errorf("not JSON: %v%s, at %s", syntactic.Err, within(syntactic.JSONPointer), position(data, syntactic.ByteOffset))
	case err != nil:
		return nil, nil, fmt.Errorf("not one JSON object: %w", err)
	}
	if file == nil {
		return nil, nil, errors.New("the file is null: it holds no JSON object")
	}
	// The decoder stops at the end of the object, before the white space after it, so whatever
	// follows that white space is a second value, a stray bracket or a word alike.
	if rest := bytes.TrimLeft(data[dec.InputOffset():], " \t\r\n"); len(rest) > 0 {
		return nil, nil, fmt.Errorf("the file goes on after its one JSON object, at %s", position(data, int64(len(data)-len(rest))))
	}
	if file.Profiles == nil {
		return nil, nil, errors.New(`"profiles" is missing or null: the file is one JSON object {"profiles": [...]}, and a file of no profiles is {"profiles": []}`)
	}
	return file.Kinds, *file.Profiles, nil
}

// proxyField is a profile's proxy key as the file writes it, which takes one of two forms: a
// kind's name as a string (the profile is served behind that kind until its source carries a
// proxy tag), or an object, a proxy block. ParseProfiles serves a block of kind dpop alone, a
// capability the broker mints and holds, and refuses every other block, which the kinds replaced;
// DecodeProfiles keeps them all, for `profiles check --against` reading a file written before the
// kinds. A JSON null is no proxy key at all, as it was when the key took a block alone. Every form
// is transitional: the string goes once every source a profile names carries its own proxy tag,
// the dpop block once the minted capabilities are a kind of the file, and the old blocks once no
// file before the kinds is left for `--against` to read; then the key and this type go.
type proxyField struct {
	kind  string
	block *proxyEntry
}

// proxyFormError is why a proxy key's value is neither of proxyField's forms. decodeProfiles names
// the key and the byte it starts at beside it.
type proxyFormError struct{ why string }

func (e *proxyFormError) Error() string { return e.why }

// UnmarshalJSONFrom reads one of proxyField's forms from dec, under the options of the file's own
// strict read (decodeProfiles), so a block's unknown member is refused as any other field is. A
// number, a list, a boolean and an empty string are refused, each named, since none of them names a
// kind or a block.
func (f *proxyField) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	const takes = ": it takes the name of one of the file's kinds, as a string, or a proxy block of kind dpop"
	switch kind := dec.PeekKind(); kind {
	case '"':
		var name string
		if err := jsonv2.UnmarshalDecode(dec, &name); err != nil {
			return err
		}
		if name == "" {
			return &proxyFormError{why: "is an empty string" + takes}
		}
		f.kind = name
		return nil
	case '{':
		var block proxyEntry
		if err := jsonv2.UnmarshalDecode(dec, &block); err != nil {
			return err
		}
		f.block = &block
		return nil
	default:
		if _, err := dec.ReadValue(); err != nil {
			return err
		}
		return &proxyFormError{why: "is a JSON " + kindWords(kind) + takes}
	}
}

// position reads offset, the index from zero of the byte at fault that encoding/json/v2 reports,
// as "byte offset N, line L, column C", each counted from one, as a person counts them. A column
// counts bytes, not characters, as the offset does.
func position(data []byte, offset int64) string {
	at := min(max(offset, 0), int64(len(data)))
	line, column := 1, 1
	for _, b := range data[:at] {
		if b == '\n' {
			line, column = line+1, 1
		} else {
			column++
		}
	}
	return fmt.Sprintf("byte offset %d, line %d, column %d", offset+1, line, column)
}

// dotted is pointer as the file's refusals name a field, profiles.1.github.accounts.
func dotted(pointer jsontext.Pointer) string {
	return strings.Join(slices.Collect(pointer.Tokens()), ".")
}

// within is " in " and the dotted name of the object a fault is in, and "" for the file's own
// object.
func within(pointer jsontext.Pointer) string {
	if pointer == "" {
		return ""
	}
	return " in " + dotted(pointer)
}

// because is ": " and why a value of the right kind is still no value of its field, such as a
// number past a uint32, and "" where the kind alone was wrong.
func because(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
}

// kindWords names the JSON value a file is in place of its one object, or a proxy key is in place
// of its two forms, as a person calls it.
func kindWords(kind jsontext.Kind) string {
	switch kind {
	case '[':
		return "array"
	case '"':
		return "string"
	case '0':
		return "number"
	case 't', 'f':
		return "boolean"
	case '{':
		return "object"
	}
	return kind.String()
}
