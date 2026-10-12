package c

import (
	"strings"
	"testing"
)

// initWalkSrc: one message per shape that decides whether <msg>__init needs the
// descriptor walk at all (generator#737).
const initWalkSrc = "version: 1\nmessages:\n" +
	"  plain:\n    payload:\n" +
	"      n: { id: 0, type: u32 }\n" +
	"      s: { id: 1, type: string, maxlen: 8 }\n" +
	"      b: { id: 2, type: blob, maxlen: 4 }\n" +
	"      a: { id: 3, type: array, items: { type: u16, count: 3 } }\n" +
	"      sa: { id: 4, type: array, items: { type: string, count: 2, maxlen: 4 } }\n" +
	"  withdef:\n    payload:\n      n: { id: 0, type: u32, default: 4242 }\n" +
	"  nesteddef:\n    payload:\n" +
	"      s: { id: 0, type: struct, fields: { x: { id: 0, type: u8, default: 7 } } }\n" +
	"  uniondef:\n    payload:\n" +
	"      u: { id: 0, type: union, default_id: 1, oneof: { a: { id: 0, type: u8 }, b: { id: 1, type: u8 } } }\n" +
	"  unionzero:\n    payload:\n" +
	"      u: { id: 0, type: union, oneof: { a: { id: 0, type: u8 }, b: { id: 1, type: u8 } } }\n"

// initOf returns the body of <msg>__init from the generated source.
func initOf(t *testing.T, files map[string]string, msg string) string {
	t.Helper()
	src, ok := files[msg+"_sofab.c"]
	if !ok {
		t.Fatalf("no %s_sofab.c", msg)
	}
	open := "void message_" + msg + "__init(message_" + msg + "_t *msg) {"
	i := strings.Index(src, open)
	if i < 0 {
		t.Fatalf("no init for %s in:\n%s", msg, src)
	}
	rest := src[i+len(open):]
	return rest[:strings.Index(rest, "\n}")]
}

// TestCInitDropsTheWalkWithoutDefaults is generator#737: the initializer was a
// bulk clear PLUS a runtime descriptor walk, and for a message whose tree
// declares no non-zero default the walk writes nothing but zeros over memory the
// memset just zeroed -- at ~26 ns per field visit, because its per-field clear
// has a runtime size and compiles to a `rep stosb` each at -Os.
//
// The memset stays either way: it is the cheap half, it covers a sized buffer's
// companion length member and a holder's element count (issue #128), and it is
// what makes the padding deterministic -- the walk reaches none of those.
func TestCInitDropsTheWalkWithoutDefaults(t *testing.T) {
	files := genCFromYAML(t, initWalkSrc)

	for _, msg := range []string{"plain", "unionzero"} {
		body := initOf(t, files, msg)
		if !strings.Contains(body, "memset(msg, 0, sizeof(*msg));") {
			t.Errorf("%s: the bulk clear must stay:%s", msg, body)
		}
		if strings.Contains(body, "sofab_object_init") {
			t.Errorf("%s: no descriptor in the tree carries a default image, so the walk "+
				"writes only zeros the memset already wrote:%s", msg, body)
		}
	}

	// A default anywhere in the tree keeps the walk: at the message's own leaf, in
	// a NESTED descriptor's image (the recursion), or as a union tag -- a union
	// whose default option is not option 0 seeds `which` from its image, which no
	// memset can do.
	for _, msg := range []string{"withdef", "nesteddef", "uniondef"} {
		body := initOf(t, files, msg)
		if !strings.Contains(body, "sofab_object_init(&message_"+msg+"__descr, msg);") {
			t.Errorf("%s: a default image in the tree needs the walk:%s", msg, body)
		}
	}
}

// TestCInitWalkMatchesTheDescriptorDecision: treeHasDefaults must agree with
// emitDescriptor, which decides per object whether to emit an image at all. A
// walk kept where no image exists is the cost #737 removes; a walk dropped where
// one exists would lose a declared default, so the two are pinned together --
// WITH_DEFAULTS or a non-NULL union image in the file iff the walk is emitted.
func TestCInitWalkMatchesTheDescriptorDecision(t *testing.T) {
	files := genCFromYAML(t, initWalkSrc)
	for _, msg := range []string{"plain", "withdef", "nesteddef", "uniondef", "unionzero"} {
		src := files[msg+"_sofab.c"]
		hasImage := strings.Contains(src, "SOFAB_OBJECT_DESCR_WITH_DEFAULTS") ||
			(strings.Contains(src, "SOFAB_OBJECT_DESCR_UNION") && !strings.Contains(src, ", NULL, message_"))
		walks := strings.Contains(initOf(t, files, msg), "sofab_object_init")
		if hasImage != walks {
			t.Errorf("%s: descriptor carries an image = %v, but init walks = %v:\n%s",
				msg, hasImage, walks, src)
		}
	}
}
