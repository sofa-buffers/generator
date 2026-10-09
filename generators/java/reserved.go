package java

import (
	"strings"

	"github.com/sofa-buffers/generator/internal/naming"
)

// One reserved-name list for the Java backend (generator#239): every name a
// schema field cannot take as a field of a generated class. Java keeps fields
// and methods apart, so a field `encode` beside the method encode() is legal
// and needs no entry. What a field does collide with:
//
//   - a keyword;
//   - a static field the class declares itself (javaStatics);
//   - a name the class body uses as the qualifier of an expression
//     (`Seq.reset(...)`, `java.util.Arrays.equals(...)`): inside the class a
//     field of that name OBSCURES the type or package (JLS 6.4.2), so the
//     expression no longer compiles (javaQualifiers).
//
// Java has no identifier escape, so a name on the list is mangled with a
// trailing underscore; the JSON key is a separate string literal and the wire
// is keyed by id, so neither changes. The struct path (javaIdent) reads the
// list; a union's slots are `_`-led (unionSlot) and need none of it. Names
// can no longer end with `_` or differ only in case and underscores (the
// validator's naming rules), so a mangled field meets no other field.
//
// The type-level half -- the names a generated CLASS cannot take -- is
// javaTypeNames below. TestJavaNamesInScope keeps javaStatics, javaQualifiers
// and javaTypeNames equal to what the generated files actually use.

// javaKeywords are Java reserved words. Java has no raw-identifier escape, so a
// field with such a name is mangled (trailing underscore); the JSON key keeps the
// original name (emitted as a separate string literal).
var javaKeywords = map[string]bool{
	"abstract": true, "assert": true, "boolean": true, "break": true, "byte": true,
	"case": true, "catch": true, "char": true, "class": true, "const": true,
	"continue": true, "default": true, "do": true, "double": true, "else": true,
	"enum": true, "extends": true, "final": true, "finally": true, "float": true,
	"for": true, "goto": true, "if": true, "implements": true, "import": true,
	"instanceof": true, "int": true, "interface": true, "long": true, "native": true,
	"new": true, "package": true, "private": true, "protected": true, "public": true,
	"return": true, "short": true, "static": true, "strictfp": true, "super": true,
	"switch": true, "synchronized": true, "this": true, "throw": true, "throws": true,
	"transient": true, "try": true, "void": true, "volatile": true, "while": true,
	"true": true, "false": true, "null": true, "var": true, "record": true, "yield": true,
}

// javaStatics are the static fields every generated message class declares.
// (A union's option-id constants `<OPT>_ID` all end in `_ID`, which neither
// entry does (unionIDConst); the _arrdef_* constants start with `_`, which no
// schema name does.)
var javaStatics = map[string]bool{"MAX_SIZE": true, "MAX_SIZE_LIMIT": true}

// javaQualifiers are the names a generated class body uses as the qualifier of
// an expression: corelib and java.util types (`List.of(...)` in an array
// default), and the `java` package root of a
// fully qualified call.
var javaQualifiers = map[string]bool{
	"Arrays": true, "DecodeStatus": true, "List": true, "OStream": true, "Seq": true,
	"SofabError": true, "System": true, "java": true,
}

// javaIdent is the field a schema field is reached through: the schema name,
// with a trailing underscore where it is on the list.
func javaIdent(name string) string {
	if javaKeywords[name] || javaStatics[name] || javaQualifiers[name] {
		return name + "_"
	}
	return name
}

// locChild is the decoder frame of the field `name` below the frame `loc`. The
// frames are named by the schema path joined with `_` and looked up by name
// (locIndex), so an underscore INSIDE a name is doubled: the path a.b is
// Root_a_b and a field a_b is Root_a__b. Without it the two shared one frame and
// the decoder silently stored the field a_b into a.b. A schema name starts with
// a letter, so the doubling cannot be read the other way.
func locChild(loc, name string) string {
	return loc + "_" + strings.ReplaceAll(name, "_", "__")
}

// javaTypeNames is the type-level half of the list (ARCHITECTURE §8, "Naming"):
// every simple name a generated class file of the package uses for something
// other than a schema type. A class of the generated package shadows the
// on-demand imports (`org.sofabuffers.sofab.*`, `java.util.*`) and java.lang
// alike (JLS 6.4.1), and a nested class shadows every outer class of its name, so
// a type identifier on this list is escaped with a trailing underscore
// (escapeType) -- a type identifier never ends with one. TestJavaNamesInScope
// keeps the list equal to what the generated files use.
//
// The harness (project mode) is not on it: it lives in its own package and names
// every generated class fully qualified, so no schema name reaches it.
var javaTypeNames = map[string]string{
	// declared by the generated code itself
	"Decoder": "the nested incremental decoder class of every message",
	// corelib-java (org.sofabuffers.sofab.*)
	"ArrayKind":      "corelib: array element kind in Visitor.arrayBegin",
	"Bound":          "corelib: schema/receiver bound handed to Seq and IStream",
	"DecodeStatus":   "corelib: tryDecode/feed verdict",
	"FixlenType":     "corelib: the fixlen subtype in Visitor.fixlenBegin",
	"IStream":        "corelib: the decoder stream",
	"OStream":        "corelib: the encoder stream",
	"PayloadAcc":     "corelib: string/blob chunk accumulator",
	"Seq":            "corelib: array placement helpers",
	"Sofab":          "corelib: validation helpers",
	"SofabError":     "corelib: error codes",
	"SofabException": "corelib: the checked decode error",
	"Visitor":        "corelib: the decode visitor interface",
	// java.io / java.util (imported on demand)
	"IOException": "java.io: serialize's throws clause",
	"Arrays":      "java.util: array compares",
	"ArrayList":   "java.util: wrapper array storage",
	"List":        "java.util: wrapper array type",
	// java.lang
	"Boolean":               "java.lang: boolean array element",
	"Deprecated":            "java.lang: the @Deprecated annotation",
	"Double":                "java.lang: fp64 array element",
	"Exception":             "java.lang: decode()'s catch",
	"Float":                 "java.lang: fp32 array element",
	"IllegalStateException": "java.lang: Decoder.finish()",
	"Long":                  "java.lang: boxed integer element, Long.parseUnsignedLong",
	"Object":                "java.lang: the bulk-array destination",
	"Override":              "java.lang: the @Override annotation",
	"RuntimeException":      "java.lang: encode()/decode() wrap",
	"String":                "java.lang: string fields",
	"SuppressWarnings":      "java.lang: the @SuppressWarnings annotation",
	"System":                "java.lang: System.arraycopy",
}

// escapeType is a type identifier as the class it names: with a trailing
// underscore where the generated code already means something by it
// (javaTypeNames), or where the class file it names cannot exist on Windows
// (naming.IsDeviceStem: a message `con` is class Con_ in Con_.java, not
// Con.java). A type identifier never ends with `_`, so the escaped spelling is
// no other type's. Roles and private names are built from the unescaped
// identifier.
func escapeType(t string) string {
	if _, ok := javaTypeNames[t]; ok || naming.IsDeviceStem(t) {
		return t + "_"
	}
	return t
}
