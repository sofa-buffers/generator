// Package naming holds the identifier encodings every backend builds its
// schema-derived names from (docs/ARCHITECTURE.md §8, "Naming").
//
// The encodings are injective on what the validator accepts, which is the
// point of the three naming rules (internal/parser):
//
//   - a name matches ^[A-Za-z][A-Za-z0-9]*(_[A-Za-z0-9]+)*$ — no "__", no
//     trailing "_";
//   - names in one scope have distinct folds (Fold);
//   - messages and every $defs category are one scope.
//
// From those, every function here keeps distinct inputs distinct:
//
//   - Pascal never yields "_", so TypeIdent can join path segments with "_"
//     and still be split back into them;
//   - a TypeIdent never contains "__" and never ends with "_", so a backend
//     can add a role with "__" and escape a reserved identifier with a
//     trailing "_" without meeting any other TypeIdent;
//   - a name never contains "__", so CPath can join verbatim segments with
//     "___" and a C backend can add a role with "__".
//
// A backend that composes its identifiers from these channels, and from
// generator-owned names that start with "_" (no schema name does), gets
// distinct identifiers by construction.
package naming

import (
	"regexp"
	"strings"
)

// NameRe is the spelling of every user-chosen name (the validator's first
// naming rule): letters and digits, starting with a letter, with single
// underscores between them — no "__", no trailing "_".
var NameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*(_[A-Za-z0-9]+)*$`)

// IsDeviceStem reports whether a file whose name starts with stem (up to its
// first ".") cannot exist on Windows: CON, PRN, AUX, NUL, COM0-9 and LPT0-9
// are reserved device names there whatever the extension and the case. A
// backend that names a file after a schema name escapes such a stem in its
// file channel.
func IsDeviceStem(stem string) bool {
	s := strings.ToLower(stem)
	switch s {
	case "con", "prn", "aux", "nul":
		return true
	}
	return len(s) == 4 && (strings.HasPrefix(s, "com") || strings.HasPrefix(s, "lpt")) &&
		s[3] >= '0' && s[3] <= '9'
}

// Fold is what a name keeps once case and underscores are dropped. Two names
// of one scope never share a fold (the validator's second naming rule).
func Fold(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + 'a' - 'A')
		case c != '_':
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Pascal is a name in PascalCase: split at "_", each part's first letter
// upper-cased, the rest kept, the parts concatenated: `vehicle_telemetry` ->
// VehicleTelemetry, `fooBar` -> FooBar, `a_1b` -> A1b, `HTTPServer` ->
// HTTPServer.
//
// It never yields "_", always starts with an upper-case letter, and folds to
// Fold(name), so two names of one scope never share it.
func Pascal(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	up := true
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '_' {
			up = true
			continue
		}
		if up && c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		up = false
		b.WriteByte(c)
	}
	return b.String()
}

// TypeIdent is the flat type identifier of a schema path: each segment in
// Pascal, joined with "_". A message or $defs type is its own name
// (`vehicle_telemetry` -> VehicleTelemetry, $defs struct `point` -> Point);
// the inline struct of field `a` in message `m` is M_A, and so is the inline
// element struct of an array field `a` (a field declares at most one inline
// type).
//
// Distinct paths give distinct identifiers: splitting at "_" recovers the
// Pascal segments, and each segment is unique in its scope. The result has
// no "__", no trailing "_", and starts with an upper-case letter.
func TypeIdent(path []string) string {
	parts := make([]string, len(path))
	for i, seg := range path {
		parts[i] = Pascal(seg)
	}
	return strings.Join(parts, "_")
}

// CPath is the path spelling for a target that keeps names verbatim (C):
// segments joined with "___". A name has no "__", so no name and no role a
// backend adds with "__" can produce the separator.
func CPath(path []string) string {
	return strings.Join(path, "___")
}

// Lower is TypeIdent for a case-insensitive context — a file name, a module
// that folds case: each segment folded, joined with "_". Fold keeps segments
// of one scope apart, so it is as injective as TypeIdent, on any filesystem.
func Lower(path []string) string {
	parts := make([]string, len(path))
	for i, seg := range path {
		parts[i] = Fold(seg)
	}
	return strings.Join(parts, "_")
}
