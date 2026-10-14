package golang

import (
	"strings"
	"testing"
)

// The two surfaces of generator#747: a PUSH decoder (chunks handed to the
// message, rather than the message pulling them out of a reader or needing the
// whole image at once) and an encode into storage the CALLER owns, with the
// options the generated encoders use reachable from outside the package.
//
// Both are checked on a BOUNDED and an UNBOUNDED message, because the encode
// side has two arms: the bounded one sizes a buffer from the schema's worst
// case, the unbounded one drains a fixed scratch. EncodeInto is one shape for
// both -- the buffer is the caller's either way -- and the regression that would
// hide here is emitting it in the bounded arm only.

const pushDecodeBounded = `version: 1
messages:
  probe:
    payload:
      a: { id: 0, type: u32 }
      s: { id: 1, type: string, maxlen: 8 }
      rows:
        id: 2
        type: array
        items: { type: struct, count: 2, fields: { x: { id: 0, type: u8 } } }
`

const pushDecodeUnbounded = `version: 1
messages:
  probe:
    payload:
      a: { id: 0, type: u32 }
      s: { id: 1, type: string }
`

func TestGoPushDecoderAndEncodeInto(t *testing.T) {
	for _, tc := range []struct {
		name, def string
		bounded   bool
	}{
		{"bounded", pushDecodeBounded, true},
		{"unbounded", pushDecodeUnbounded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := genGo(t, schemaFromYAMLString(t, tc.def), map[string]any{})["probe.go"]
			want := []string{
				// The push decoder: a handle holding the message it fills and the
				// corelib's resumable decoder, and nothing else.
				"type Probe__Decoder struct {\n\tmsg *Probe\n\tdec *sofab.Decoder\n}",
				"func Probe__NewDecoder() *Probe__Decoder {\n\tm := Probe__New()\n\treturn &Probe__Decoder{msg: m, dec: sofab.NewDecoder(m)}\n}",
				// Feed BINDS the corelib's Feed. The body is the whole assertion:
				// anything more than a delegation here is parse state the generated
				// layer would be keeping in parallel with the corelib's own
				// (CORELIB_PLAN §5.3.1).
				"func (d *Probe__Decoder) Feed(chunk []byte) (sofab.Outcome, error) {\n\treturn d.dec.Feed(chunk)\n}",
				"func (d *Probe__Decoder) Message() *Probe { return d.msg }",
				// The caller-owned-buffer encode, and the options a caller driving
				// its own sofab.Encoder would otherwise have to restate.
				"func (m *Probe) EncodeInto(dst []byte) (int, error) {",
				"e, err := sofab.NewEncoderBuffer(dst, 0, _Probe__EncOpts...)",
				"func Probe__EncodeOptions() []sofab.Option {\n\treturn _Probe__EncOpts\n}",
			}
			for _, w := range want {
				if !strings.Contains(got, w) {
					t.Errorf("probe.go is missing:\n%s", w)
				}
			}
			// The bounded Encode allocates its worst-case buffer and goes through
			// EncodeInto rather than restating the same four corelib calls.
			if tc.bounded && !strings.Contains(got, "buf := make([]byte, Probe__MaxSize)\n\tn, err := m.EncodeInto(buf)") {
				t.Error("bounded Encode should allocate and delegate to EncodeInto")
			}
		})
	}
}

// Every message of the canonical example schema gets both surfaces: they are
// companions of the message, not of a field shape, so a schema cannot opt out
// of one.
func TestGoPushDecoderOnExample(t *testing.T) {
	got := genGo(t, exampleSchema(t), map[string]any{})["myfirstmessage.go"]
	for _, w := range []string{
		"func Myfirstmessage__NewDecoder() *Myfirstmessage__Decoder {",
		"func (d *Myfirstmessage__Decoder) Feed(chunk []byte) (sofab.Outcome, error) {",
		"func (d *Myfirstmessage__Decoder) Message() *Myfirstmessage { return d.msg }",
		"func (m *Myfirstmessage) EncodeInto(dst []byte) (int, error) {",
		"func Myfirstmessage__EncodeOptions() []sofab.Option {",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("myfirstmessage.go is missing:\n%s", w)
		}
	}
}

// EncodeInto is a method on the message, so a field spelled `encode_into` would
// be a Go compile error (a type cannot carry a field and a method of one name).
// It belongs on the reserved list with Encode and EncodeTo.
func TestGoEncodeIntoIsReserved(t *testing.T) {
	if !goReserved("EncodeInto") {
		t.Error("EncodeInto must be reserved: it is a method on every generated message")
	}
	got := genGo(t, schemaFromYAMLString(t, `version: 1
messages:
  probe:
    payload:
      encode_into: { id: 0, type: u32 }
`), map[string]any{})["probe.go"]
	if !strings.Contains(got, "EncodeInto_ uint32") {
		t.Errorf("a field named encode_into must be mangled, got:\n%s", firstStruct(got))
	}
}

// firstStruct is the generated struct declaration, for a readable failure.
func firstStruct(src string) string {
	i := strings.Index(src, "type Probe struct {")
	if i < 0 {
		return src
	}
	if j := strings.Index(src[i:], "\n}"); j >= 0 {
		return src[i : i+j+2]
	}
	return src[i:]
}
