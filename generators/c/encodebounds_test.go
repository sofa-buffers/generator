package c

import (
	"bytes"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// encodeBoundsYAML is the shape the encode-bound rule covers: a string and a
// blob with a maxlen, a numeric array and an array<string> with a count, and a
// nested struct's array.
const encodeBoundsYAML = `
version: 1
messages:
  M:
    payload:
      s: { id: 0, type: string, maxlen: 4 }
      b: { id: 1, type: blob, maxlen: 4 }
      au: { id: 2, type: array, items: { type: u32, count: 3 } }
      as: { id: 3, type: array, items: { type: string, count: 3, maxlen: 4 } }
      n:
        id: 4
        type: struct
        fields:
          arr: { id: 0, type: array, items: { type: u16, count: 2 } }
`

// TestEncodeBoundsLiveInTheCorelib: the C target refuses an over-maxlen string at
// encode (generator#656) without a single generated guard. The bound is the
// member's own storage, char[maxlen + 1], which the descriptor already hands the
// corelib as the field size; sofab_object_encode reads the string bounded by it
// and refuses one that fills it without a terminator. So the generated code
// carries no length check and no maxlen literal in its encode path, for any
// bound or field count -- a per-schema copy of that check would be a static
// helper (CLAUDE.md), and a second, redundant one.
func TestEncodeBoundsLiveInTheCorelib(t *testing.T) {
	for _, src := range []string{encodeBoundsYAML, strings.ReplaceAll(encodeBoundsYAML, "maxlen: 4", "maxlen: 9")} {
		out := genCFromYAML(t, src)
		var h, c string
		for p, v := range out {
			switch filepath.Ext(p) {
			case ".h":
				h = v
			case ".c":
				c = v
			}
		}
		want := "char s[5];"
		if strings.Contains(src, "maxlen: 9") {
			want = "char s[10];"
		}
		if !strings.Contains(h, want) {
			t.Errorf("the string member is the bound the corelib reads: want %q in\n%s", want, h)
		}
		i := strings.Index(c, "__encode(const ")
		if i < 0 {
			t.Fatalf("no encode function in\n%s", c)
		}
		enc := c[i:]
		for _, bad := range []string{"strlen", "strnlen", "SOFAB_RET_E_ARGUMENT", "SOFAB_DISABLE_ENCODE_BOUNDS", "__len >", ".len >"} {
			if strings.Contains(enc, bad) {
				t.Errorf("generated encode must not carry a bound check (%q); the corelib owns it:\n%s", bad, enc)
			}
		}
	}
}

// TestBoundNotesSayWhatEncodeDoes: the member notes state what the encoder does
// with an over-bound value -- a string is refused, a length or count companion
// past its capacity is clamped (the documented clamp contract) -- and never the
// decoder-only "never truncated" for a companion the encoder does clamp.
func TestBoundNotesSayWhatEncodeDoes(t *testing.T) {
	var h string
	for p, v := range genCFromYAML(t, encodeBoundsYAML) {
		if filepath.Ext(p) == ".h" {
			h = v
		}
	}
	for _, want := range []string{
		"Schema bound: maxlen 4 -- the capacity is in the type; a value that fills it without a terminator is longer, and encode refuses it (SOFAB_RET_E_ARGUMENT). Over 4 on the wire is INVALID.",
		"Schema bound: maxlen 4 -- b__len carries the length; encode clamps b__len to at most 4. Over 4 on the wire is INVALID.",
		"Schema bound: count 3 is a capacity; au__len carries the length -- elements set without it encode an EMPTY array; encode clamps au__len to at most 3. Over 3 on the wire is INVALID.",
		"Schema bound: count 3 is a capacity; as.len carries the length -- elements set without it encode an EMPTY array; encode clamps as.len to at most 3. Over 3 on the wire is INVALID. Element maxlen 4: an element that fills its storage without a terminator is refused at encode.",
		"Schema bound: count 2 is a capacity; arr__len carries the length",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("missing note %q in\n%s", want, h)
		}
	}
	if strings.Contains(h, "never truncated") {
		t.Errorf("a C note must not promise \"never truncated\": encode clamps a companion\n%s", h)
	}
}

// TestHarnessStoresAnOverlongStringUnterminated: the project harness stores a
// JSON string longer than the member's maxlen the way a caller's over-long copy
// would -- its first cap bytes, no terminator -- so the encoder sees an
// over-bound value and refuses it. Those cap bytes may end inside a UTF-8
// character; only the refusal keeps that cut off the wire, so the harness must
// not terminate (and so silently shorten) the value itself.
func TestHarnessStoresAnOverlongStringUnterminated(t *testing.T) {
	hs := genCProject(t, encodeBoundsYAML)["harness/main.c"]
	if !strings.Contains(hs, "if (L >= cap) { memcpy(dst, s, cap); return; }") {
		t.Errorf("json_to_str must store an over-long value unterminated:\n%s", hs)
	}
	if strings.Contains(hs, "L = cap - 1") {
		t.Errorf("json_to_str must not terminate an over-long value (encode would accept the cut):\n%s", hs)
	}
}

// TestHarnessHandsEncodeAnUnclampedLength: the harness stores at most the
// capacity's elements or bytes but sets the length companion to the JSON length
// unclamped, as a caller's over-long length would be. So the clamp the
// conformance cases observe is the encoder's (sofab_object_encode), not the
// harness's own.
func TestHarnessHandsEncodeAnUnclampedLength(t *testing.T) {
	hs := genCProject(t, encodeBoundsYAML)["harness/main.c"]
	for _, want := range []string{
		"o->b__len = (uint8_t)json_to_bytes(c, o->b, sizeof(o->b), (uint8_t)-1);",
		"size_t _s0 = _n0 > 3 ? 3 : _n0;",
		"o->au__len = (uint32_t)json_len(_n0, (uint32_t)-1);",
		"o->as.len = (uint8_t)json_len(_n0, (uint8_t)-1);",
		"for (size_t _i0 = 0; _i0 < _s0; _i0++) {",
		"return json_len(n, lmax);",
	} {
		if !strings.Contains(hs, want) {
			t.Errorf("harness missing %q:\n%s", want, hs)
		}
	}
	if strings.Contains(hs, "if (_n0 > 3) _n0 = 3;") {
		t.Errorf("the harness must not clamp the length it hands encode:\n%s", hs)
	}
}

// TestEncodeBoundsOptOut builds the generated project against the corelib and
// encodes an over-maxlen string, a string at its bound and an over-capacity
// blob: by default the string is refused; with -DSOFAB_DISABLE_ENCODE_BOUNDS
// (which the project Makefile passes through CFLAGS to the corelib sources) it
// is emitted as its whole, bounded buffer. A blob length and array counts past
// their capacity reach encode unclamped (the harness passes the JSON length), and
// sofab_object_encode clamps them in both builds: each encodes exactly as the
// value cut to its capacity.
func TestEncodeBoundsOptOut(t *testing.T) {
	corelib := os.Getenv("SOFAB_C_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_C_CORELIB to a corelib-c-cpp checkout to run the build half")
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not found")
	}
	files := genCProject(t, encodeBoundsYAML)
	run := func(dir, in string) (string, error) {
		cmd := exec.Command(filepath.Join(dir, "harness", "harness"), "encode", "M")
		cmd.Stdin = strings.NewReader(in)
		var out bytes.Buffer
		cmd.Stdout = &out
		err := cmd.Run()
		return hex.EncodeToString(out.Bytes()), err
	}
	for _, tc := range []struct {
		name, cflags, overString string
	}{
		{"checks on", "", ""},
		{"opt-out", "-DSOFAB_DISABLE_ENCODE_BOUNDS", "022a7878787878"},
	} {
		dir := writeProject(t, files)
		args := []string{"-C", dir, "SOFAB_C_CORELIB=" + corelib, "WARNFLAGS=" + strings.Join(strictWarnings, " ")}
		if tc.cflags != "" {
			args = append(args, "CFLAGS="+tc.cflags)
		}
		if out, err := exec.Command("make", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: build failed: %v\n%s", tc.name, err, out)
		}
		got, err := run(dir, `{"s":"xxxxx"}`)
		if tc.overString == "" {
			if err == nil {
				t.Errorf("%s: an over-maxlen string must be refused, encoded %s", tc.name, got)
			}
		} else if err != nil || got != tc.overString {
			t.Errorf("%s: an over-maxlen string must encode as its bounded buffer %s, got %s (err %v)", tc.name, tc.overString, got, err)
		}
		if got, err := run(dir, `{"s":"xxxx"}`); err != nil || got != "022278787878" {
			t.Errorf("%s: a string at its bound must encode, got %s (err %v)", tc.name, got, err)
		}
		if got, err := run(dir, `{"b":[1,2,3,4,5,6]}`); err != nil || got != "0a2301020304" {
			t.Errorf("%s: encode clamps an over-maxlen blob length to its capacity, got %s (err %v)", tc.name, got, err)
		}
		for _, c := range []struct{ over, at string }{
			{`{"au":[1,2,3,4,5]}`, `{"au":[1,2,3]}`},
			{`{"as":["a","b","c","d"]}`, `{"as":["a","b","c"]}`},
			{`{"n":{"arr":[1,2,3]}}`, `{"n":{"arr":[1,2]}}`},
		} {
			want, err := run(dir, c.at)
			if err != nil || want == "" {
				t.Fatalf("%s: %s must encode (err %v)", tc.name, c.at, err)
			}
			if got, err := run(dir, c.over); err != nil || got != want {
				t.Errorf("%s: encode clamps the count of %s to its capacity: want %s, got %s (err %v)", tc.name, c.over, want, got, err)
			}
		}
	}
}
