// packages/envoy/internal/googleidentity/credentialsfile_test.go
//
// The credentials-file check reads a file as Google's library reads it: RepeatsKey finds two keys
// of one object that Go's JSON decoder puts in one place of the types the library decodes the file
// into, and those types (CredentialFields, certificateConfig) have the library's own shape. The
// readers that hand a file to the library run the check against local stand-ins in their own
// tests (identity/google, cmd/cipher).

package googleidentity

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestARepeatedKeyIsFoundWhereTheLibraryDecodesOnePlace: RepeatsKey finds two keys of one object
// that the decoder puts in one place of the types Google's library decodes the file into, wherever
// the object is: in a struct, two keys that name one field as strings.EqualFold matches them; in a
// map, two equal keys. Controls: header names that differ in case (a map), keys that name no field
// and what is under them, one field at two depths, and a number outside float64's range, which
// the decoder skips.
func TestARepeatedKeyIsFoundWhereTheLibraryDecodesOnePlace(t *testing.T) {
	file, config := CredentialFieldsType, certificateConfigType
	for _, row := range []struct {
		name    string
		schema  reflect.Type
		data    string
		repeats bool
	}{
		{"a field repeated exactly", file, `{"type":"a","type":"b"}`, true},
		{"a field repeated in another case", file, `{"type":"x","TYPE":"y"}`, true},
		{"a field repeated with ſ for s", file, `{"credential_source":{},"credential_ſource":{}}`, true},
		{"a field repeated through an escape", file, `{"type":"a","\u0074ype":"b"}`, true},
		{"a field repeated after a null", file, `{"credential_source":{"file":"a"},"credential_source":null}`, true},
		{"a field of the certificate repeated in another case", file, `{"credential_source":{"certificate":{"trust_chain_path":"a","TRUST_CHAIN_PATH":"b"}}}`, true},
		{"a header repeated exactly", file, `{"credential_source":{"headers":{"X-Example":"a","X-Example":"b"}}}`, true},
		{"a field of the configuration repeated with the Kelvin sign for k", config, `{"cert_configs":{"workload":{"key_path":"a","\u212aey_path":"b"}}}`, true},
		{"a field of the configuration repeated in another case", config, `{"cert_configs":{},"Cert_Configs":{}}`, true},
		{"control: headers whose names differ in case", file, `{"credential_source":{"headers":{"X-Example":"e","x-example":"e"}}}`, false},
		{"control: a key that names no field, in another case", file, `{"extension":1,"EXTENSION":2}`, false},
		{"control: keys repeated under a key that names no field", file, `{"extension":{"type":1,"type":2}}`, false},
		// enterprise-certificate-proxy's util.LoadSignerBinaryPath decodes libs.ecp, but only for a
		// configuration with no workload, which CertificateFailure refuses before the library reads it.
		{"control: keys repeated under libs, of which this client reads nothing", config, `{"libs":{"ecp":"a","ecp":"b"}}`, false},
		{"control: one field at two depths", file, `{"type":"a","credential_source":{"format":{"type":"b"}}}`, false},
		{"control: an object where the library decodes a string, which it refuses", file, `{"type":{"a":1,"a":2}}`, false},
		{"control: a number outside float64's range", file, `{"type":"external_account","unknown_extension":1e400}`, false},
		{"control: no object", file, `[1,"a",null]`, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			repeats, err := RepeatsKey([]byte(row.data), row.schema)
			if err != nil || repeats != row.repeats {
				t.Fatalf("repeatsKey(%s) = %t, %v; want %t", row.data, repeats, err, row.repeats)
			}
		})
	}
	for _, data := range []string{``, `{`, `{"a":1`, `{"a" 1}`, `{} {}`, `{}x`, `{"a":1,}`, `{"a":[1}`} {
		if _, err := RepeatsKey([]byte(data), file); err == nil {
			t.Errorf("repeatsKey(%q) answered no error; want one, as for data that is not one JSON value", data)
		}
	}
	// Data after the value is RepeatsKey's own error, and a repeat found before a defect answers
	// true with no error.
	if _, err := RepeatsKey([]byte(`{} {}`), file); err == nil || err.Error() != "more than one JSON value" {
		t.Errorf("repeatsKey of two values: %v; want the error more than one JSON value", err)
	}
	if repeats, err := RepeatsKey([]byte(`{"type":"a","type":"b"`), file); !repeats || err != nil {
		t.Errorf("repeatsKey of a truncated object that repeats a key = %t, %v; want true and no error", repeats, err)
	}
}

// TestTheMirroredTypesAreGooglesLibrarys: CredentialFields and certificateConfig, from which
// RepeatsKey reads which objects are structs and which are maps, have the shape of the types
// Google's library decodes a credentials file and a certificate configuration into, read from the
// library's own source in the module this package builds with: each field's JSON name, and a
// struct, a pointer, a map or a leaf of one kind (string, bool, int, interface and the rest) where
// the library has one. A type of the library's that the check cannot name fails it. Controls: the
// library's types with credential_source a struct and not a pointer, headers a string and not a
// map, type an any and not a string, headers a map of any and not of string, or type a named type
// the check does not know, each fail it.
func TestTheMirroredTypesAreGooglesLibrarys(t *testing.T) {
	list := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "cloud.google.com/go/auth")
	out, err := list.Output()
	if err != nil {
		t.Fatalf("go list -m cloud.google.com/go/auth: %v", err)
	}
	dir := strings.TrimSpace(string(out))
	for _, row := range []struct {
		file, root string
		mirror     reflect.Type
	}{
		{"internal/credsfile/filetype.go", "ExternalAccountFile", CredentialFieldsType},
		{"internal/transport/cert/workload_cert.go", "certificateConfig", certificateConfigType},
	} {
		types := structTypes(t, filepath.Join(dir, row.file))
		if reason := mirrorFailure(astShape(types[row.root], types), reflectShape(row.mirror)); reason != "" {
			t.Errorf("%s in %s %s", row.root, row.file, reason)
		}
	}

	const differs = "has the shape"
	for _, control := range []struct {
		name string
		edit func(types map[string]ast.Expr)
		want string
	}{
		{"credential_source a struct", func(types map[string]ast.Expr) {
			field := fieldTagged(t, types["ExternalAccountFile"], "credential_source")
			field.Type = field.Type.(*ast.StarExpr).X
		}, differs},
		{"headers a string", func(types map[string]ast.Expr) {
			fieldTagged(t, types["CredentialSource"], "headers").Type = ast.NewIdent("string")
		}, differs},
		{"type an any", func(types map[string]ast.Expr) {
			fieldTagged(t, types["ExternalAccountFile"], "type").Type = ast.NewIdent("any")
		}, differs},
		{"headers a map of any", func(types map[string]ast.Expr) {
			fieldTagged(t, types["CredentialSource"], "headers").Type = &ast.MapType{Key: ast.NewIdent("string"), Value: ast.NewIdent("any")}
		}, differs},
		{"type a named type", func(types map[string]ast.Expr) {
			fieldTagged(t, types["ExternalAccountFile"], "type").Type = ast.NewIdent("credentialKind")
		}, "names a type this check does not know: credentialKind"},
	} {
		types := structTypes(t, filepath.Join(dir, "internal/credsfile/filetype.go"))
		control.edit(types)
		if got := mirrorFailure(astShape(types["ExternalAccountFile"], types), reflectShape(CredentialFieldsType)); !strings.Contains(got, control.want) {
			t.Errorf("control: with %s, the check says %q; want it to say %q", control.name, got, control.want)
		}
	}
}

// unknownType is how astShape writes a type it cannot name.
var unknownType = regexp.MustCompile(`unknown\(([^)]*)\)`)

// mirrorFailure is why library, the shape of a type of Google's library, is not mirror, the shape
// of its mirror, and "" where the two are one: a type of the library's that astShape cannot name,
// or another shape.
func mirrorFailure(library, mirror string) string {
	if unknown := unknownType.FindAllStringSubmatch(library, -1); unknown != nil {
		var names []string
		for _, match := range unknown {
			names = append(names, match[1])
		}
		return "names a type this check does not know: " + strings.Join(names, ", ")
	}
	if library != mirror {
		return "has the shape\n  " + library + "\nand the mirror\n  " + mirror
	}
	return ""
}

// structTypes is every struct type the Go file at path declares, by name.
func structTypes(t *testing.T, path string) map[string]ast.Expr {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]ast.Expr{}
	ast.Inspect(parsed, func(n ast.Node) bool {
		if spec, ok := n.(*ast.TypeSpec); ok {
			if _, ok := spec.Type.(*ast.StructType); ok {
				types[spec.Name.Name] = spec.Type
			}
		}
		return true
	})
	return types
}

// fieldTagged is the field of the struct expr whose JSON name is name.
func fieldTagged(t *testing.T, expr ast.Expr, name string) *ast.Field {
	t.Helper()
	for _, field := range expr.(*ast.StructType).Fields.List {
		if jsonName(field) == name {
			return field
		}
	}
	t.Fatalf("no field %s", name)
	return nil
}

// jsonName is the JSON name a struct field's tag gives it, or its Go name.
func jsonName(field *ast.Field) string {
	if field.Tag != nil {
		tag, err := strconv.Unquote(field.Tag.Value)
		if err == nil {
			if name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ","); name != "" {
				return name
			}
		}
	}
	return field.Names[0].Name
}

// leafKinds names, as reflect.Kind names it, the kind of each of Go's predeclared types a field can
// have: byte and rune are uint8 and int32, and any an interface.
var leafKinds = map[string]string{
	"bool": "bool", "string": "string", "int": "int", "int8": "int8", "int16": "int16", "int32": "int32",
	"int64": "int64", "uint": "uint", "uint8": "uint8", "uint16": "uint16", "uint32": "uint32",
	"uint64": "uint64", "uintptr": "uintptr", "float32": "float32", "float64": "float64",
	"complex64": "complex64", "complex128": "complex128", "byte": "uint8", "rune": "int32", "any": "interface",
}

// astShape is the shape of a type expression of a file whose struct types are types: a leaf's
// kind (leafKinds, and "interface" for an interface with no methods), "*" and the shape pointed
// at, "map[" key "]" value, "[]" element, or "{" each field's JSON name and shape "}", in name
// order. Any other type, such as a named type that is no struct of the file or a type of another
// package, is "unknown(" its name or Go type ")".
func astShape(expr ast.Expr, types map[string]ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		if named, ok := types[e.Name]; ok {
			return astShape(named, types)
		}
		if kind, ok := leafKinds[e.Name]; ok {
			return kind
		}
		return "unknown(" + e.Name + ")"
	case *ast.InterfaceType:
		if len(e.Methods.List) == 0 {
			return "interface"
		}
	case *ast.StarExpr:
		return "*" + astShape(e.X, types)
	case *ast.MapType:
		return "map[" + astShape(e.Key, types) + "]" + astShape(e.Value, types)
	case *ast.ArrayType:
		return "[]" + astShape(e.Elt, types)
	case *ast.StructType:
		var fields []string
		for _, field := range e.Fields.List {
			fields = append(fields, jsonName(field)+":"+astShape(field.Type, types))
		}
		slices.Sort(fields)
		return "{" + strings.Join(fields, ",") + "}"
	}
	return fmt.Sprintf("unknown(%T)", expr)
}

// reflectShape is the shape of a type, as astShape writes it, a leaf as its kind.
func reflectShape(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Pointer:
		return "*" + reflectShape(t.Elem())
	case reflect.Map:
		return "map[" + reflectShape(t.Key()) + "]" + reflectShape(t.Elem())
	case reflect.Slice, reflect.Array:
		return "[]" + reflectShape(t.Elem())
	case reflect.Struct:
		var fields []string
		for i := range t.NumField() {
			field := t.Field(i)
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "" {
				name = field.Name
			}
			fields = append(fields, name+":"+reflectShape(field.Type))
		}
		slices.Sort(fields)
		return "{" + strings.Join(fields, ",") + "}"
	}
	return t.Kind().String()
}

// TestACredentialSourceReadsAsGooglesLibraryReadsIt: CredentialFields decodes credential_source
// as the pointer Google's library decodes it into, so a null drops what came before it, as the
// library's does, and certificateConfig its workload the same way.
func TestACredentialSourceReadsAsGooglesLibraryReadsIt(t *testing.T) {
	fields, err := ReadCredentialFields([]byte(`{"credential_source":{"certificate":{"certificate_config_location":"/workload.json"}},` +
		`"credential_source":null,"credential_source":{"certificate":{"use_default_certificate_config":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if certificate := fields.Certificate(); certificate == nil || certificate.Config != "" || !certificate.UseDefault {
		t.Fatalf("the certificate source read %+v; want the default configuration and no location, as the library reads it", certificate)
	}
	var config certificateConfig
	if err := json.Unmarshal([]byte(`{"cert_configs":{"workload":{"cert_path":"/c","key_path":"/k"},"workload":null}}`), &config); err != nil {
		t.Fatal(err)
	}
	if config.CertConfigs.Workload != nil {
		t.Fatalf("the workload read %+v; want none, as the library reads it", config.CertConfigs.Workload)
	}
}
