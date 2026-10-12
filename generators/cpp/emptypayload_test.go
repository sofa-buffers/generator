package cpp

import (
	"strings"
	"testing"
)

// emptyPayloadSrc: a message whose payload declares no fields, beside one whose
// struct field declares none. Both are legal schemas (MESSAGE_SPEC §2 makes the
// all-default message the empty byte string, so a field-less message is the
// ping), and the header for either failed to compile under the
// -Wall -Wextra -Werror this suite builds generated code with (generator#738).
const emptyPayloadSrc = "version: 1\nmessages:\n" +
	"  ping:\n    payload: {}\n" +
	"  holder:\n    payload:\n      s: { id: 0, type: struct, fields: {} }\n"

// TestCppEmptyPayloadNamesNoUnusedParameter: deserialize's stream parameter is
// named only where an arm reads it, which is the rule _size and _count already
// follow. A type binding no field reads nothing from the stream.
func TestCppEmptyPayloadNamesNoUnusedParameter(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts map[string]any
	}{
		{"cpp", map[string]any{"namespace": "m"}},
		{"cpp-static", map[string]any{"namespace": "m", "allow_dynamic": false}},
		{"c-cpp", map[string]any{"namespace": "m", "corelib": "c-cpp", "allow_dynamic": true}},
		{"c-cpp-static", map[string]any{"namespace": "m", "corelib": "c-cpp", "allow_dynamic": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := genHeader(t, emptyPayloadSrc, "ping.hpp", tc.opts)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			if !strings.Contains(h, "void deserialize(sofab::IStreamImpl &, sofab::id _id,") {
				t.Errorf("a field-less message must leave the stream parameter unnamed:\n%s", h)
			}
			if strings.Contains(h, "void deserialize(sofab::IStreamImpl &_is,") {
				t.Errorf("the stream parameter is named but no arm reads it:\n%s", h)
			}
		})
	}

	// A type that DOES bind a field keeps the name, so the rule is "unused", not
	// "always unnamed".
	h := headerFromYAML(t, "version: 1\nmessages:\n  B:\n    payload:\n"+
		"      s: { id: 1, type: string, maxlen: 16 }\n", "b.hpp")
	if !strings.Contains(h, "void deserialize(sofab::IStreamImpl &_is,") {
		t.Errorf("a message with a field must name the stream:\n%s", h)
	}
}

// TestCppEmptyStructNamesNoUnusedParameter: the same for a struct member whose
// own payload is empty -- the holder names the stream (it binds the struct), the
// struct does not (it binds nothing).
func TestCppEmptyStructNamesNoUnusedParameter(t *testing.T) {
	h, err := genHeader(t, emptyPayloadSrc, "holder.hpp", map[string]any{"namespace": "m"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if n := strings.Count(h, "void deserialize(sofab::IStreamImpl &, sofab::id _id,"); n != 1 {
		t.Errorf("expected exactly the field-less struct to leave it unnamed, got %d:\n%s", n, h)
	}
	if !strings.Contains(h, "void deserialize(sofab::IStreamImpl &_is,") {
		t.Errorf("the holder binds the struct and must name the stream:\n%s", h)
	}
}
