package c

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// defaultImageSrc: a message whose defaults sit on members of several widths,
// beside members with none -- the shape that decides how big the default image
// is (generator#741).
const defaultImageSrc = "version: 1\nmessages:\n  m:\n    payload:\n" +
	"      plain64: { id: 0, type: u64 }\n" +
	"      plain32: { id: 1, type: u32 }\n" +
	"      str:     { id: 2, type: string, maxlen: 24 }\n" +
	"      dflt8:   { id: 3, type: u8, default: 7 }\n" +
	"      dflt32:  { id: 4, type: u32, default: 4242 }\n" +
	"      plain8:  { id: 5, type: u8 }\n"

// TestCDefaultImageIsThePrefixOfTheObject is generator#741: the image used to be
// a copy of the WHOLE struct, because the corelib addresses it through each
// field's own offset -- 824 bytes of .rodata on one realworld schema to carry 16
// bytes of actual defaults. It is now the prefix of the object that carries
// defaults, which only works because the defaulted members are ordered first.
func TestCDefaultImageIsThePrefixOfTheObject(t *testing.T) {
	files := genCFromYAML(t, defaultImageSrc)
	h, c := files["m_sofab.h"], files["m_sofab.c"]
	if h == "" || c == "" {
		t.Fatal("no generated files")
	}

	// 1. the defaulted members come first, widest-first among themselves
	order := memberOrder(t, h)
	if len(order) < 2 || order[0] != "dflt32" || order[1] != "dflt8" {
		t.Errorf("the defaulted members must lead, widest first, got %v", order)
	}

	// 2. the image type repeats exactly those members, in that order
	img := between(t, c, "typedef struct {", "} _message_m__defimg_t;")
	for _, want := range []string{"uint32_t dflt32;", "uint8_t dflt8;"} {
		if !strings.Contains(img, want) {
			t.Errorf("the image type must carry %q:\n%s", want, img)
		}
	}
	for _, unwanted := range []string{"plain64", "plain32", "str", "plain8"} {
		if strings.Contains(img, unwanted) {
			t.Errorf("the image must stop after the defaults, found %q:\n%s", unwanted, img)
		}
	}

	// 3. one offsetof test per image member, so the layout claim is checked by the
	//    compiler and not by this comment
	for _, m := range []string{"dflt32", "dflt8"} {
		want := "typedef char _message_m__defimg_at_" + m +
			"[(offsetof(_message_m__defimg_t, " + m + ") == offsetof(message_m_t, " + m + ")) ? 1 : -1];"
		if !strings.Contains(c, want) {
			t.Errorf("missing the layout proof for %s:\n%s", m, c)
		}
	}

	// 4. and the image is what the descriptor points at
	if !strings.Contains(c, "static const _message_m__defimg_t _message_m__defaults = {") {
		t.Errorf("the image must be declared at the prefix type:\n%s", c)
	}
	if strings.Contains(c, "static const message_m_t _message_m__defaults") {
		t.Errorf("the image must not be a copy of the whole object:\n%s", c)
	}
}

// TestCNoDefaultsNoImage: without a default there is no image and no prefix type
// at all -- the plain descriptor compares against zero, as it always has.
func TestCNoDefaultsNoImage(t *testing.T) {
	files := genCFromYAML(t, "version: 1\nmessages:\n  m:\n    payload:\n"+
		"      a: { id: 0, type: u32 }\n      b: { id: 1, type: u8 }\n")
	c := files["m_sofab.c"]
	for _, unwanted := range []string{"defimg_t", "__defaults", "WITH_DEFAULTS"} {
		if strings.Contains(c, unwanted) {
			t.Errorf("a schema with no default must emit no image, found %q:\n%s", unwanted, c)
		}
	}
}

// TestCDefaultedMemberOrderKeepsTheWire: the member order is a C layout choice
// and nothing else -- the descriptor is emitted in id order, which is what the
// wire follows.
func TestCDefaultedMemberOrderKeepsTheWire(t *testing.T) {
	c := genCFromYAML(t, defaultImageSrc)["m_sofab.c"]
	ids := regexp.MustCompile(`SOFAB_OBJECT_FIELD\w*\((\d+),`).FindAllStringSubmatch(c, -1)
	var got []string
	for _, m := range ids {
		got = append(got, m[1])
	}
	want := []string{"0", "1", "2", "3", "4", "5"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the descriptor must stay in id order, got %v", got)
	}
}

// memberOrder is the struct members of the generated header, in declaration
// order (the companion length member of a sized buffer is skipped: it shares its
// declaration with the buffer).
func memberOrder(t *testing.T, header string) []string {
	t.Helper()
	body := between(t, header, "typedef struct {", "} message_m_t;")
	var out []string
	for _, line := range strings.Split(body, "\n") {
		m := regexp.MustCompile(`^\s+\S+ (\w+)[;\[]`).FindStringSubmatch(line)
		if m != nil && !strings.HasSuffix(m[1], "__len") {
			out = append(out, m[1])
		}
	}
	return out
}

func between(t *testing.T, s, open, close string) string {
	t.Helper()
	i := strings.Index(s, open)
	if i < 0 {
		t.Fatalf("no %q in:\n%s", open, s)
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		t.Fatalf("no %q after %q", close, open)
	}
	return rest[:j]
}

// TestCPrefixImageRefusesAnOlderCorelib: the prefix image is an ABI change the
// corelib cannot detect on its own -- an older one would read past the image at
// run time and compile cleanly doing it (generator#741). The generated header
// has to refuse the pairing.
func TestCPrefixImageRefusesAnOlderCorelib(t *testing.T) {
	const guard = "#if !defined(SOFAB_OBJECT_DESCR_WITH_PREFIX_DEFAULTS)"

	h := genCFromYAML(t, defaultImageSrc)["m_sofab.h"]
	if !strings.Contains(h, guard) {
		t.Errorf("a message with a default image must guard on the extent macro:\n%s", h)
	}
	if !strings.Contains(h, "which this corelib-c-cpp predates") {
		t.Errorf("the guard must name the fix:\n%s", h)
	}

	// and a schema with no image asks for nothing new
	h = genCFromYAML(t, "version: 1\nmessages:\n  m:\n    payload:\n      a: { id: 0, type: u32 }\n")["m_sofab.h"]
	if strings.Contains(h, guard) {
		t.Errorf("a message with no default image must not require the macro:\n%s", h)
	}
}

// TestCDefaultImageFallsBackToTheWholeObject: the descriptor describes a prefix
// image with one byte, so a prefix over 255 bytes has no extent that says how
// far it reaches (corelib-c-cpp, "Descriptor limits"). The image is then the
// whole object -- extent 0, "all of it" -- which is what the backend emitted
// before generator#741. A defaulted `string<maxlen: 300>` is a legal schema and
// stays one: the .rodata win is what gets dropped, never the schema.
func TestCDefaultImageFallsBackToTheWholeObject(t *testing.T) {
	// char[255] of image: the last size the extent byte still describes.
	c := genCFromYAML(t, defaultedString(254))["m_sofab.c"]
	if !strings.Contains(c, "SOFAB_OBJECT_DESCR_WITH_PREFIX_DEFAULTS") {
		t.Errorf("a 255-byte prefix still fits the extent:\n%s", c)
	}

	// char[256]: one byte over, so the whole object goes over instead
	c = genCFromYAML(t, defaultedString(255))["m_sofab.c"]
	if !strings.Contains(c, "const sofab_object_descr_t message_m__descr = SOFAB_OBJECT_DESCR_WITH_DEFAULTS(") {
		t.Errorf("a 256-byte prefix must hand over the whole object:\n%s", c)
	}
	if !strings.Contains(c, "static const message_m_t _message_m__defaults = {") {
		t.Errorf("the fallback image is an object, not a prefix type:\n%s", c)
	}
	for _, unwanted := range []string{"defimg_t", "WITH_PREFIX_DEFAULTS"} {
		if strings.Contains(c, unwanted) {
			t.Errorf("no prefix machinery in the fallback, found %q:\n%s", unwanted, c)
		}
	}
	if !strings.Contains(c, "over SOFAB_OBJECT_DESCR_EXTENT_MAX") {
		t.Errorf("the fallback must say why it is one:\n%s", c)
	}

	// ... and such a file asks nothing new of the corelib: its descriptor is the
	// one every corelib has always read.
	h := genCFromYAML(t, defaultedString(255))["m_sofab.h"]
	if strings.Contains(h, "SOFAB_OBJECT_DESCR_WITH_PREFIX_DEFAULTS") {
		t.Errorf("a whole-object image must not require the prefix macro:\n%s", h)
	}
}

// defaultedString is a one-field schema whose image is char[maxlen+1].
func defaultedString(maxlen int) string {
	return "version: 1\nmessages:\n  m:\n    payload:\n" +
		"      s: { id: 0, type: string, maxlen: " + strconv.Itoa(maxlen) + ", default: hi }\n"
}
