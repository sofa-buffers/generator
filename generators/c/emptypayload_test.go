package c

import (
	"strings"
	"testing"
)

// emptyPayloadSrc: a message whose payload declares no fields, and a field whose
// struct declares none. Both are legal schemas -- MESSAGE_SPEC §2 makes the
// all-default message the empty byte string, so a field-less message is the ping
// -- and both used to produce C that only GCC would accept (generator#738).
const emptyPayloadSrc = "version: 1\nmessages:\n" +
	"  ping:\n    payload: {}\n" +
	"  holder:\n    payload:\n      s: { id: 0, type: struct, fields: {} }\n"

// TestCEmptyPayloadIsISOC: the generated library for a field-less type has no
// empty struct and no zero-length array -- both GCC extensions, and the first
// one has a sizeof that differs between C (0) and C++ (1).
func TestCEmptyPayloadIsISOC(t *testing.T) {
	files := genCFromYAML(t, emptyPayloadSrc)
	h, c := files["ping_sofab.h"], files["ping_sofab.c"]
	if h == "" || c == "" {
		t.Fatalf("missing generated files: %v", keysOf(files))
	}
	for _, want := range []string{
		"    char _novalue; /*!< Placeholder: this payload declares no fields. */",
		"#define MESSAGE_PING__MAX_SIZE 0",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q:\n%s", want, h)
		}
	}
	if strings.Contains(h, "typedef struct {\n} ") {
		t.Errorf("header still declares an empty struct:\n%s", h)
	}
	if !strings.Contains(c, "static const sofab_object_descr_field_t _message_ping__fields[1]; /* unused: no fields */") {
		t.Errorf("descriptor table is still a zero-length array:\n%s", c)
	}
	// The nested field-less STRUCT gets the same treatment, in the holder's header.
	if hh := files["holder_sofab.h"]; !strings.Contains(hh, "char _novalue;") {
		t.Errorf("a field-less struct member needs the placeholder too:\n%s", hh)
	}
}

// TestCEmptyPayloadHarnessIsISOC: the harness sizes its buffers from MAX_SIZE,
// which is 0 here, and reads nothing out of a field-less object.
func TestCEmptyPayloadHarnessIsISOC(t *testing.T) {
	files := genCProject(t, emptyPayloadSrc)
	main := files["harness/main.c"]
	if main == "" {
		t.Fatalf("no harness: %v", keysOf(files))
	}
	for _, want := range []string{
		"#define _SOFAB_BUF(n) ((n) > 0 ? (n) : 1)",
		"static unsigned char _bench_ping_wire[_SOFAB_BUF(MESSAGE_PING__MAX_SIZE)];",
		"unsigned char out[_SOFAB_BUF(MESSAGE_PING__MAX_SIZE)];",
		"    (void)o;", // to_json of a field-less object reads nothing
		"    (void)j;", // nor does from_json
	} {
		if !strings.Contains(main, want) {
			t.Errorf("harness missing %q", want)
		}
	}
	if strings.Contains(main, "out[MESSAGE_PING__MAX_SIZE]") {
		t.Errorf("harness still declares a zero-size array:\n%s", main)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
