package cpp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

// unionShapeYAML covers every option kind a union can hold, with default_id on
// a STRUCT option that is not the first, plus an array of unions whose default
// option is a string.
const unionShapeYAML = `
version: 1
messages:
  m:
    payload:
      u:
        id: 0
        type: union
        default_id: 2
        oneof:
          num:  { id: 0, type: u16, default: 5 }
          s:    { id: 1, type: string, maxlen: 8 }
          pt:   { id: 2, type: struct, fields: { x: { id: 0, type: i32, default: 7 } } }
          arr:  { id: 3, type: array, items: { type: u16, count: 4 } }
          strs: { id: 4, type: array, items: { type: string, count: 3, maxlen: 4 } }
          bl:   { id: 5, type: blob, maxlen: 4 }
          f:    { id: 6, type: fp32, default: 1.5 }
          box:  { id: 7, type: struct, fields: { z: { id: 0, type: u8, default: 3 } } }
      v: { id: 1, type: array, items: { type: union, count: 4, default_id: 1, oneof: { i: { id: 0, type: i32 }, s: { id: 1, type: string, maxlen: 8 } } } }
      w: { id: 2, type: u8 }
`

// unionFiles generates the project for src under cfg (namespace + emit:
// project added) and returns every file by path.
func unionFiles(t *testing.T, src string, cfg map[string]any) map[string]string {
	t.Helper()
	files, err := unionGenerate(t, src, cfg)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return files
}

func unionGenerate(t *testing.T, src string, cfg map[string]any) (map[string]string, error) {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "in.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := doc.Resolve()
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("invalid: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	full := map[string]any{"namespace": "sofabuffers", "emit": "project"}
	for k, v := range cfg {
		full[k] = v
	}
	gen, err := (&Backend{}).Generate(s, full)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, f := range gen {
		out[f.Path] = string(f.Content)
	}
	return out, nil
}

// section returns the text of the generated struct `name` (up to its closing
// "};" at column 0).
func section(t *testing.T, h, name string) string {
	t.Helper()
	i := strings.Index(h, "struct "+name+" : sofab::Message {")
	if i < 0 {
		t.Fatalf("no struct %s in:\n%s", name, h)
	}
	j := strings.Index(h[i:], "\n};\n")
	return h[i : i+j+4]
}

// The four C++ profiles: corelib-cpp with dynamic / static storage, and
// corelib-c-cpp with dynamic / static storage.
var unionProfiles = []struct {
	name string
	cfg  map[string]any
}{
	{"cpp", map[string]any{}},
	{"cpp-static", map[string]any{"allow_dynamic": false}},
	{"c-cpp-dynamic", map[string]any{"corelib": "c-cpp", "allow_dynamic": true}},
	{"c-cpp-static", map[string]any{"corelib": "c-cpp"}},
}

// TestCppUnionVariantShape: on corelib-cpp a union is a std::variant with one
// alternative per option in id order, holding default_id at its default from
// construction, and the public API is which()/has_/getter/set_/mutable_/reset().
func TestCppUnionVariantShape(t *testing.T) {
	h := unionFiles(t, unionShapeYAML, nil)["m.hpp"]
	u := section(t, h, "M_U")
	for _, want := range []string{
		"#include <variant>",
		"enum class Which : sofab::id {\n        num = 0,\n        s = 1,\n        pt = 2,",
		"Which which() const noexcept { return static_cast<Which>(_ids[_opts.index()]); }",
		"static constexpr sofab::id _ids[] = {0, 1, 2, 3, 4, 5, 6, 7};",
		"std::variant<std::uint16_t, std::string, M_U_Pt, std::vector<std::uint16_t>, std::vector<std::string>, std::vector<std::uint8_t>, float, M_U_Box> _opts{std::in_place_index<2>};",
		// a scalar option: by value, its declared default when not held
		"std::uint16_t num() const noexcept { return has_num() ? (*std::get_if<0>(&_opts)) : 5; }",
		"void set_num(std::uint16_t _v) noexcept { mutable_num() = _v; }",
		// select-if-not-held, at the option's own default
		"std::uint16_t &mutable_num() noexcept {\n        if (_opts.index() != 0) { _opts.emplace<0>(5); }",
		"float &mutable_f() noexcept {\n        if (_opts.index() != 6) { _opts.emplace<6>(1.5f); }",
		// a class option: by const reference, a default instance when not held
		"const M_U_Pt &pt() const noexcept {\n        if (has_pt()) { return (*std::get_if<2>(&_opts)); }\n        static const M_U_Pt _d{};",
		"M_U_Pt &mutable_pt() noexcept {\n        if (_opts.index() != 2) { _opts.emplace<2>(); }",
		"void reset() noexcept { mutable_pt().reset(); }",
		"bool _isDefault() const noexcept { return _opts.index() == 2 && (*std::get_if<2>(&_opts))._isDefault(); }",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q:\n%s", want, u)
		}
	}
	// No product type: no option is a plain member.
	for _, bad := range []string{"std::uint16_t num = ", "M_U_Pt pt = ", "std::string s = "} {
		if strings.Contains(u, bad) {
			t.Errorf("union %q still holds option member %q:\n%s", "M_U", bad, u)
		}
	}
	if strings.Contains(h, "#include <new>") {
		t.Errorf("corelib-cpp needs no placement new:\n%s", h)
	}
}

// TestCppUnionTaggedShapeCCpp: on corelib-c-cpp (freestanding, no <variant>) a
// union is a tag plus a C++ union. The default constructor placement-news the
// default option; the copy operations switch on the tag. Static storage needs
// no destructor (every option is trivially destructible); allow_dynamic, whose
// options can own heap storage, ends the held option's lifetime on a switch and
// in the destructor.
func TestCppUnionTaggedShapeCCpp(t *testing.T) {
	for _, dyn := range []bool{false, true} {
		h := unionFiles(t, unionShapeYAML, map[string]any{"corelib": "c-cpp", "allow_dynamic": dyn})["m.hpp"]
		u := section(t, h, "M_U")
		if strings.Contains(h, "<variant>") || strings.Contains(u, "std::variant") {
			t.Errorf("dyn=%v: c-cpp must not use std::variant:\n%s", dyn, u)
		}
		str, vec := "sofab::FixedString<8>", "sofab::InlineVector<std::uint16_t, 4>"
		if dyn {
			str, vec = "std::string", "std::vector<std::uint16_t>"
		}
		for _, want := range []string{
			"#include <new>",
			// The tag leads; default_id is the nested union's initial member
			// through its default member initializer, and both the union's and
			// the class's default constructors are defaulted, never
			// user-provided, so `T{}` still zero-initializes.
			"M_U() noexcept = default;",
			"M_U(const M_U &_o) noexcept : sofab::Message(_o), _which(_o._which), _u(nullptr) { _copy(_o); }",
			"case Which::s: ::new (&_u.s) " + str + "(_o._u.s); break;",
			"Which which() const noexcept { return _which; }",
			"bool has_arr() const noexcept { return _which == Which::arr; }",
			"    Which _which = Which::pt;\n    union _Opts {\n        _Opts() noexcept = default;\n        struct _None { _None() noexcept {} };\n        explicit _Opts(std::nullptr_t) noexcept : _none() {}",
			"        " + vec + " arr;",
			"        _None _none;\n    } _u;\n};",
			"std::uint16_t num() const noexcept { return has_num() ? _u.num : 5; }",
			"::new (&_u.num) std::uint16_t(5);",
			"bool _isDefault() const noexcept { return _which == Which::pt && _u.pt._isDefault(); }",
		} {
			if !strings.Contains(h, want) {
				t.Errorf("dyn=%v: header missing %q:\n%s", dyn, want, u)
			}
		}
		hasDtor := strings.Contains(u, "~M_U() { _clear(); }")
		if hasDtor != dyn {
			t.Errorf("dyn=%v: destructor emitted = %v, want %v:\n%s", dyn, hasDtor, dyn, u)
		}
		if dyn {
			for _, want := range []string{
				"#include <memory>",
				"case Which::s: std::destroy_at(&_u.s); break;",
				"        if (_which != Which::s) {\n            _clear();\n            ::new (&_u.s) std::string();",
			} {
				if !strings.Contains(h, want) {
					t.Errorf("dynamic c-cpp missing %q:\n%s", want, u)
				}
			}
			// a scalar option owns nothing to destroy
			if strings.Contains(u, "std::destroy_at(&_u.num)") {
				t.Errorf("a scalar option needs no destroy_at:\n%s", u)
			}
		} else if strings.Contains(u, "_clear") || strings.Contains(h, "#include <memory>") {
			t.Errorf("static c-cpp storage is trivially destructible and needs no _clear():\n%s", u)
		}
	}
}

// TestCppUnionSerializeArms: default_id is written like an ordinary field
// (guarded, a struct option lazily framed); every other option is written
// whatever its value -- a scalar, string, blob and compact array unguarded, a
// struct option with the KEEPING write, a wrapper array closed with
// sequenceEndKeep -- on every profile.
func TestCppUnionSerializeArms(t *testing.T) {
	for _, p := range unionProfiles {
		u := section(t, unionFiles(t, unionShapeYAML, p.cfg)["m.hpp"], "M_U")
		clib := p.cfg["corelib"] == "c-cpp"
		st := func(i int, name string) string {
			if clib {
				return "_u." + name
			}
			return "(*std::get_if<" + strconv.Itoa(i) + ">(&_opts))"
		}
		for _, want := range []string{
			"(void)_os.writeLazy(2, " + st(2, "pt") + ");", // D: lazy, dropped at its default
			"(void)_os.write(7, " + st(7, "box") + ");",    // non-D struct: kept
			"(void)_os.write(0, " + st(0, "num") + ");",    // non-D scalar: no guard
			"(void)_os.write(6, " + st(6, "f") + ");",
			"(void)_os.write(1, " + st(1, "s") + ");",
			"(void)_os.write(3, " + st(3, "arr") + ");",
			"(void)_os.write(5, " + st(5, "bl") + ".data(), static_cast<std::int32_t>(" + st(5, "bl") + ".size()));",
			"(void)_os.sequenceBeginLazy(4);",
		} {
			if !strings.Contains(u, want) {
				t.Errorf("%s: serialize missing %q:\n%s", p.name, want, u)
			}
		}
		// the wrapper option's frame is kept, never dropped: the first closer
		// after its opener is the keeping one
		rest := u[strings.Index(u, "(void)_os.sequenceBeginLazy(4);"):]
		if k := strings.Index(rest, "(void)_os.sequenceEnd"); k < 0 || !strings.HasPrefix(rest[k:], "(void)_os.sequenceEndKeep();") {
			t.Errorf("%s: the wrapper option must close with sequenceEndKeep:\n%s", p.name, u)
		}
		// no non-D option carries a ≠-default guard
		for _, bad := range []string{st(0, "num") + " != 5", st(6, "f") + " != 1.5f", st(1, "s") + ".empty()", st(3, "arr") + ".empty()", "writeLazy(7"} {
			if strings.Contains(u, bad) {
				t.Errorf("%s: a non-default option is guarded (%q):\n%s", p.name, bad, u)
			}
		}
	}
	// A leaf default option keeps its guard.
	src := strings.Replace(unionShapeYAML, "default_id: 2", "default_id: 0", 1)
	u := section(t, unionFiles(t, src, nil)["m.hpp"], "M_U")
	for _, want := range []string{
		"if ((*std::get_if<0>(&_opts)) != 5) { (void)_os.write(0, (*std::get_if<0>(&_opts))); }",
		"(void)_os.write(2, (*std::get_if<2>(&_opts)));", // pt is no longer D: kept
	} {
		if !strings.Contains(u, want) {
			t.Errorf("default_id 0: serialize missing %q:\n%s", want, u)
		}
	}
}

// TestCppUnionDecodeGateThenSelect: every decode arm selects its option only
// past the §7.3 test for the option's declared kind. A scalar is selected inside
// the `if` its read returns -- on corelib-cpp a read into a temporary, on c-cpp
// readMatch, which binds and reports the test the stream applies to that bind.
// On c-cpp a string/blob/array/wrapper-array option hands its select to the
// corelib's selecting read, which calls it behind its own test; a struct/union
// option, and every non-scalar kind on corelib-cpp, spells the test in front of
// the select (on c-cpp as one delivered() comparison).
func TestCppUnionDecodeGateThenSelect(t *testing.T) {
	u := section(t, unionFiles(t, unionShapeYAML, nil)["m.hpp"], "M_U")
	for _, want := range []string{
		"{ std::uint64_t _v; if (_is.read(_v)) { if (_v > 65535) { _is.invalidate(); return; } mutable_num() = static_cast<std::uint16_t>(_v); } }",
		"{ float _v{}; if (sofab::read(_is, _v)) { mutable_f() = _v; } }",
		"if (_is.wire() != sofab::detail::Wire::Fixlen || _is.fixType() != sofab::detail::Fix::String) break;\n            sofab::readString(_is, mutable_s(), 8);",
		"if (_is.wire() != sofab::detail::Wire::Fixlen || _is.fixType() != sofab::detail::Fix::Blob) break;\n            sofab::readBlob(_is, mutable_bl(), 4);",
		"if (_is.wire() != sofab::detail::Wire::SequenceStart) break;\n            sofab::read(_is, mutable_pt());",
		"if (_is.wire() != sofab::detail::Wire::ArrayUnsigned) break;\n            sofab::readArray(_is, mutable_arr(), 4, sofab::ElemBound::of<std::uint16_t>());",
		"if (_is.wire() != sofab::detail::Wire::SequenceStart) break;\n            { sofab::StringSeq _r0{mutable_strs(), 3, 4, -1, -1}; sofab::read(_is, _r0); }",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("corelib-cpp decode missing %q:\n%s", want, u)
		}
	}
	// No arm resets a held option: the only emplace is the one behind the
	// "not held" test inside mutable_<opt>().
	if n, m := strings.Count(u, "emplace<"), strings.Count(u, "if (_opts.index() != "); n != m {
		t.Errorf("every emplace must sit behind a not-held test (%d emplace, %d tests):\n%s", n, m, u)
	}

	c := section(t, unionFiles(t, unionShapeYAML, map[string]any{"corelib": "c-cpp"})["m.hpp"], "M_U")
	for _, want := range []string{
		// a scalar: the deferred read overwrites the whole value, so the select
		// is the tag alone, set once the bind reported the match
		"if (_is.readMatch(_u.num)) { _which = Which::num; }",
		"if (_is.readMatch(_u.f)) { _which = Which::f; }",
		// string/blob/array: the corelib selects behind its own test, once
		"case 1:\n            _is.readString([this]() -> auto & { return mutable_s(); }, _size, 8);",
		"case 5:\n            _is.readBlob([this]() -> auto & { return mutable_bl(); }, _size, 4);",
		"case 3:\n            _is.readArray([this]() -> auto & { return mutable_arr(); }, _count, 4);",
		"_is.readSequence(_r0, [this]() -> auto & { return mutable_strs(); }); }",
		// a struct: read(Message&) has no test of its own to select behind
		"if (!_is.delivered(sofab::Wire::SequenceStart)) break;\n            _is.read(mutable_pt());",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("c-cpp decode missing %q:\n%s", want, c)
		}
	}
	// The footprint leg spells no two-compare gate anywhere.
	for _, bad := range []string{"_is.wire()", "_is.fixType()"} {
		if strings.Contains(c, bad) {
			t.Errorf("c-cpp decode spells %q; the test is delivered()/readMatch:\n%s", bad, c)
		}
	}
	// allow_dynamic: the option left behind may own heap storage, so a scalar
	// select ends it first -- and only when the option is not already held.
	d := section(t, unionFiles(t, unionShapeYAML, map[string]any{"corelib": "c-cpp", "allow_dynamic": true})["m.hpp"], "M_U")
	want := "if (_is.readMatch(_u.num) && _which != Which::num) { _clear(); _which = Which::num; }"
	if !strings.Contains(d, want) {
		t.Errorf("dynamic c-cpp decode missing %q:\n%s", want, d)
	}
}

// unionArmShapesYAML is one more field of the unionShapeYAML message whose
// options reach every decode-arm shape the c-cpp leg distinguishes: scalar
// options sharing a bind type (two fp32; an enum and an i8 of the same backing),
// and array options whose read names the destination through a view or more
// than once (enum, boolean, struct and row arrays), which keep the gate.
const unionArmShapesYAML = `      k: { id: 5, type: union, oneof: { f1: { id: 0, type: fp32 }, f2: { id: 1, type: fp32, default: 2.5 }, ea: { id: 2, type: array, items: { type: enum, count: 2, enum: { X: 0, Y: 1 } } }, ba: { id: 3, type: array, items: { type: boolean, count: 2 } }, sa: { id: 4, type: array, items: { type: struct, count: 2, fields: { p: { id: 0, type: u8 } } } }, rows: { id: 5, type: array, items: { type: array, count: 2, items: { type: u16, count: 3 } } }, e1: { id: 6, type: enum, enum: { P: 0, Q: 1 } }, i1: { id: 7, type: i8 } } }
`

// TestCppUnionSameBindArmsShareOneCase: on c-cpp, scalar options bound through
// the same C++ type share one decode arm -- the Which enumerators are the option
// ids, so the arm takes the tag from the id. Options of different bind types
// keep an arm each, and corelib-cpp is unchanged.
func TestCppUnionSameBindArmsShareOneCase(t *testing.T) {
	src := unionShapeYAML + unionArmShapesYAML
	c := section(t, unionFiles(t, src, map[string]any{"corelib": "c-cpp"})["m.hpp"], "M_K")
	for _, want := range []string{
		"case 0:\n        case 1:\n            if (_is.readMatch(_u.f1)) { _which = static_cast<Which>(_id); }",
		"case 6:\n        case 7:\n",
		"if (_is.readMatch(reinterpret_cast<std::int8_t &>(_u.e1))) { _which = static_cast<Which>(_id); }",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("c-cpp decode missing %q:\n%s", want, c)
		}
	}
	d := section(t, unionFiles(t, src, map[string]any{"corelib": "c-cpp", "allow_dynamic": true})["m.hpp"], "M_K")
	want := "if (_is.readMatch(_u.f1) && _which != static_cast<Which>(_id)) { _clear(); _which = static_cast<Which>(_id); }"
	if !strings.Contains(d, want) {
		t.Errorf("dynamic c-cpp decode missing %q:\n%s", want, d)
	}
	// A lone scalar option names its own enumerator (the M_U union).
	u := section(t, unionFiles(t, src, map[string]any{"corelib": "c-cpp"})["m.hpp"], "M_U")
	if strings.Contains(u, "static_cast<Which>(_id)") {
		t.Errorf("an option with no same-type sibling must not take its tag from the id:\n%s", u)
	}
	p := section(t, unionFiles(t, src, nil)["m.hpp"], "M_K")
	if strings.Contains(p, "static_cast<Which>(_id)") || strings.Contains(p, "case 0:\n        case 1:") {
		t.Errorf("corelib-cpp arms must stay one per option:\n%s", p)
	}
}

// TestCppUnionGateKeptWhereNoSelectingRead: an array option whose c-cpp read
// binds through a view (enum/boolean) or names the destination more than once
// (a dynamic struct array reserves it first) has no selecting overload to hand
// its select to, so it keeps the gate in front of mutable_<opt>().
func TestCppUnionGateKeptWhereNoSelectingRead(t *testing.T) {
	src := unionShapeYAML + unionArmShapesYAML
	for _, p := range []map[string]any{{"corelib": "c-cpp"}, {"corelib": "c-cpp", "allow_dynamic": true}} {
		c := section(t, unionFiles(t, src, p)["m.hpp"], "M_K")
		for _, want := range []string{
			"if (!_is.delivered(sofab::Wire::ArraySigned)) break;",   // ea: enum array (RawArray view)
			"if (!_is.delivered(sofab::Wire::ArrayUnsigned)) break;", // ba: boolean array (RawArray view)
		} {
			if !strings.Contains(c, want) {
				t.Errorf("%v: decode missing %q:\n%s", p, want, c)
			}
		}
		if strings.Contains(c, "{&[this]") || strings.Contains(c, "[this]() -> auto & { return mutable_ea(); }") {
			t.Errorf("%v: a view-bound array must not take a selector:\n%s", p, c)
		}
	}
	d := section(t, unionFiles(t, src, map[string]any{"corelib": "c-cpp", "allow_dynamic": true})["m.hpp"], "M_K")
	if !strings.Contains(d, "if (!_is.delivered(sofab::Wire::SequenceStart)) break;\n            { static sofab::MessageSeq<") {
		t.Errorf("dynamic struct array option must keep the gate:\n%s", d)
	}
}

// TestCppUnionTrivialSpecialsStayImplicit: on c-cpp a union whose options are
// all trivially copyable -- every scalar union, in either storage mode -- keeps
// every implicit special member: default_id is placed by the nested union's own
// constructor and the tag by its initializer, so not even the default
// constructor is user-provided. That is what keeps the footprint profile's
// scalar unions (the bench's SensorSample) at the cost of a tag, and `T{}` a
// zero-initialization as for a struct.
func TestCppUnionTrivialSpecialsStayImplicit(t *testing.T) {
	src := `
version: 1
messages:
  m:
    payload:
      s: { id: 0, type: union, oneof: { analog: { id: 0, type: fp32 }, digital: { id: 1, type: boolean }, counter: { id: 2, type: u32 } } }
      t: { id: 1, type: union, oneof: { name: { id: 0, type: string, maxlen: 4 }, raw: { id: 1, type: array, items: { type: u8, count: 2 } } } }
`
	for _, dyn := range []bool{false, true} {
		h := unionFiles(t, src, map[string]any{"corelib": "c-cpp", "allow_dynamic": dyn})["m.hpp"]
		s := section(t, h, "M_S")
		for _, want := range []string{"_Opts() noexcept = default;", "float analog{0.0f};", "Which _which = Which::analog;"} {
			if !strings.Contains(s, want) {
				t.Errorf("dyn=%v: scalar union does not start at analog = 0 (%q):\n%s", dyn, want, s)
			}
		}
		for _, bad := range []string{"M_S(", "operator=", "~M_S()", "_copy", "_clear", "~_Opts()", "_Opts(std::nullptr_t)"} {
			if strings.Contains(s, bad) {
				t.Errorf("dyn=%v: an all-scalar union must keep the implicit %q:\n%s", dyn, bad, s)
			}
		}
		// FixedString / InlineVector options are trivially copyable too under
		// static storage; std::string / std::vector ones are not.
		tt := section(t, h, "M_T")
		if got := strings.Contains(tt, "M_T(const M_T &_o)"); got != dyn {
			t.Errorf("dyn=%v: string/array union user-provided copy = %v, want %v:\n%s", dyn, got, dyn, tt)
		}
		if got := strings.Contains(tt, "~M_T() { _clear(); }"); got != dyn {
			t.Errorf("dyn=%v: string/array union destructor = %v, want %v:\n%s", dyn, got, dyn, tt)
		}
	}
}

// TestCppUnionSplitTypesKeepTheirDefault: a $defs union used with two
// default_ids is two types, each constructed at -- and omitted at -- its own
// default option.
func TestCppUnionSplitTypesKeepTheirDefault(t *testing.T) {
	src := `
version: 1
$defs:
  union:
    Pick:
      n: { id: 0, type: u16, default: 6 }
      t: { id: 1, type: struct, fields: { k: { id: 0, type: u8, default: 2 } } }
messages:
  m:
    payload:
      a: { id: 0, type: union, default_id: 1, oneof: { $ref: "#/$defs/union/Pick" } }
      b: { id: 1, type: union, oneof: { $ref: "#/$defs/union/Pick" } }
`
	h := unionFiles(t, src, nil)["m.hpp"]
	for _, want := range []string{
		"struct Pick_default_T : sofab::Message {",
		"struct Pick_default_N : sofab::Message {",
		"Pick_default_T a = {};",
		"Pick_default_N b = {};",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q:\n%s", want, h)
		}
	}
	tt := section(t, h, "Pick_default_T")
	nn := section(t, h, "Pick_default_N")
	if !strings.Contains(tt, "_opts{std::in_place_index<1>}") || !strings.Contains(tt, "(void)_os.writeLazy(1, ") || !strings.Contains(tt, "(void)_os.write(0, (*std::get_if<0>(&_opts)));") {
		t.Errorf("Pick_default_t must default to t and force n:\n%s", tt)
	}
	if !strings.Contains(nn, "_opts{std::in_place_index<0>, 6}") || !strings.Contains(nn, "if ((*std::get_if<0>(&_opts)) != 6)") || !strings.Contains(nn, "(void)_os.write(1, (*std::get_if<1>(&_opts)));") {
		t.Errorf("Pick_default_n must default to n = 6 and force t:\n%s", nn)
	}
}

// TestCppUnionAccessorNames: an option named like a member the union declares
// itself is mangled in its getter and Which enumerator, while its role
// accessors are built from the plain name; an option named like another's role
// accessor (`set_foo` beside `foo`) takes the escape in its getter, so the two
// generate and spell every accessor once.
func TestCppUnionAccessorNames(t *testing.T) {
	src := `
version: 1
messages:
  m:
    payload:
      u: { id: 0, type: union, oneof: { which: { id: 0, type: u8 }, reset: { id: 1, type: u8 }, delete: { id: 2, type: u8 } } }
`
	u := section(t, unionFiles(t, src, nil)["m.hpp"], "M_U")
	for _, want := range []string{
		"which_ = 0,", "reset_ = 1,", "delete_ = 2,",
		"std::uint8_t which_() const noexcept",
		"void set_reset(std::uint8_t _v) noexcept",
		"bool has_delete() const noexcept",
		"void reset() noexcept { mutable_which() = 0; }",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("header missing %q:\n%s", want, u)
		}
	}
	clash := `
version: 1
messages:
  m:
    payload:
      u: { id: 0, type: union, oneof: { foo: { id: 0, type: u8 }, set_foo: { id: 1, type: u8 } } }
`
	c := section(t, unionFiles(t, clash, nil)["m.hpp"], "M_U")
	for _, want := range []string{
		"std::uint8_t foo() const noexcept", "void set_foo(std::uint8_t _v) noexcept",
		"std::uint8_t set_foo_() const noexcept", "void set_set_foo(std::uint8_t _v) noexcept",
		"foo = 0,", "set_foo_ = 1,",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("header missing %q:\n%s", want, c)
		}
	}
}

// TestCppUnionHarnessJSON: the harness prints exactly the held option and reads
// one back by selecting the option its member names.
func TestCppUnionHarnessJSON(t *testing.T) {
	j := unionFiles(t, unionShapeYAML, nil)["harness/_json.hpp"]
	for _, want := range []string{
		"inline void to_json(const M_U &o, std::ostream &out) {\n    out << '{';\n    switch (o.which()) {\n    case M_U::Which::num:\n        out << \"\\\"num\\\":\";",
		"    case M_U::Which::pt:\n        out << \"\\\"pt\\\":\";\n    to_json(o.pt(), out);\n        break;",
		"    c = sofab_json_get(j, \"num\");\n    if (c) {\n        auto &_o = o.mutable_num();\n        _o = static_cast<std::uint16_t>(sofab_json_u64(c));",
		"        auto &_o = o.mutable_pt();\n        from_json(c, _o);",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("json.hpp missing %q:\n%s", want, j)
		}
	}
}

// TestCppUnionCompilesOnEveryProfile builds the generated union code under
// -Wall -Wextra -Werror against both corelibs, in both storage modes. Gated on
// the corelib checkouts (SOFAB_CPP_DIR, SOFAB_C_DIR), like the C backend's gate.
func TestCppUnionCompilesOnEveryProfile(t *testing.T) {
	cpp, cc := os.Getenv("SOFAB_CPP_DIR"), os.Getenv("SOFAB_C_DIR")
	if cpp == "" || cc == "" {
		t.Skip("set SOFAB_CPP_DIR and SOFAB_C_DIR to corelib checkouts to run the compile gate")
	}
	gxx, err := exec.LookPath("g++")
	if err != nil {
		t.Skip("g++ not found")
	}
	src := unionShapeYAML + `
      q: { id: 3, type: union, default_id: 1, oneof: { big: { id: 0, type: u64 }, sig: { id: 1, type: i64, default: -3 } } }
      n: { id: 4, type: union, oneof: { in: { id: 0, type: union, default_id: 1, oneof: { a: { id: 0, type: u8 }, b: { id: 1, type: boolean, default: true } } }, c: { id: 1, type: array, items: { type: blob, count: 2, maxlen: 3 } } } }
` + unionArmShapesYAML
	for _, p := range unionProfiles {
		dir := t.TempDir()
		for path, content := range unionFiles(t, src, p.cfg) {
			full := filepath.Join(dir, path)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		inc := "-I" + filepath.Join(cpp, "include")
		if p.cfg["corelib"] == "c-cpp" {
			inc = "-I" + filepath.Join(cc, "src", "include")
		}
		// -O2 and real code generation, not -fsyntax-only: the flow warnings
		// (-Wmaybe-uninitialized over an inlined `buf_[i] = T{}` of a union
		// element) only exist once GCC optimises.
		args := []string{"-std=c++20", "-O2", "-Wall", "-Wextra", "-Werror", "-c", "-o", filepath.Join(dir, "main.o"), inc,
			"-I" + filepath.Join(cc, "test", "shared"), "-I" + dir, filepath.Join(dir, "harness", "main.cpp")}
		if out, err := exec.Command(gxx, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: generated union C++ failed to compile:\n%s", p.name, out)
		}
	}
}

// sameBindYAML is a union whose scalar options pair up by bind type (two fp32;
// an enum and an i8 of the same backing) beside a string option, for
// TestCppUnionSameBindArmsDecode.
const sameBindYAML = `
version: 1
messages:
  g:
    payload:
      k: { id: 0, type: union, oneof: { f1: { id: 0, type: fp32 }, f2: { id: 1, type: fp32, default: 2.5 }, s: { id: 2, type: string, maxlen: 4 }, e1: { id: 6, type: enum, enum: { P: 0, Q: 1 } }, i1: { id: 7, type: i8 } } }
      w: { id: 1, type: u8 }
`

// sameBindMain drives the shared arms at run time: a shared arm must still
// select the option its id names, keep the last correctly-typed option, and
// switch nothing on a mistyped child.
const sameBindMain = `#include "g.hpp"
#include <cstdio>
#include <string_view>

using namespace sofabuffers;
static int fails = 0;
#define CHECK(c) do { if (!(c)) { std::printf("FAIL line %d: %s\n", __LINE__, #c); fails++; } } while (0)

static G roundtrip(const G &in) {
    std::uint8_t buf[G::_maxSize];
    std::size_t n = in.encodeTo(buf, sizeof buf);
    G out;
    CHECK(G::try_decode(buf, n, out).ok());
    return out;
}

static G frame(void (*fill)(sofab::OStream &)) {
    sofab::OStream os{64};
    os.sequenceBeginLazy(0);
    fill(os);
    os.sequenceEnd();
    os.write(1, std::uint8_t{7});
    G out;
    CHECK(G::try_decode(os.data(), os.bytesUsed(), out).ok());
    CHECK(out.w == 7);
    return out;
}

int main() {
    { G g; g.k.set_f2(3.5f); G o = roundtrip(g); CHECK(o.k.has_f2() && o.k.f2() == 3.5f); }
    { G g; g.k.set_f2(2.5f); G o = roundtrip(g); CHECK(o.k.has_f2() && o.k.f2() == 2.5f); }
    { G g; g.k.set_f1(4.0f); G o = roundtrip(g); CHECK(o.k.has_f1() && o.k.f1() == 4.0f); }
    { G g; g.k.set_i1(-5); G o = roundtrip(g); CHECK(o.k.has_i1() && o.k.i1() == -5); }
    { G g; g.k.set_e1(G_K_E1::Q); G o = roundtrip(g); CHECK(o.k.has_e1() && o.k.e1() == G_K_E1::Q); }
    { G o = frame([](sofab::OStream &os) { os.write(1, 2.0); });  CHECK(o.k.has_f1() && o.k.f1() == 0.0f); }
    { G o = frame([](sofab::OStream &os) { os.write(7, std::uint8_t{3}); }); CHECK(o.k.has_f1()); }
    { G o = frame([](sofab::OStream &os) { os.write(0, 1.0f); os.write(1, 2.0f); }); CHECK(o.k.has_f2() && o.k.f2() == 2.0f); }
    { G o = frame([](sofab::OStream &os) { os.write(1, 2.0f); os.write(0, 1.0f); }); CHECK(o.k.has_f1() && o.k.f1() == 1.0f); }
    { G o = frame([](sofab::OStream &os) { os.write(6, std::int8_t{1}); os.write(7, std::int8_t{-2}); }); CHECK(o.k.has_i1() && o.k.i1() == -2); }
    { G o = frame([](sofab::OStream &os) { os.write(7, std::int8_t{-2}); os.write(6, std::int8_t{1}); }); CHECK(o.k.has_e1() && o.k.e1() == G_K_E1::Q); }
    { G o = frame([](sofab::OStream &os) { os.write(2, std::string_view{"abcd"}); os.write(1, 6.0f); }); CHECK(o.k.has_f2() && o.k.f2() == 6.0f); }
    { G o = frame([](sofab::OStream &os) { os.write(1, 6.0f); os.write(2, std::string_view{"ab"}); });
      CHECK(o.k.has_s() && std::string_view(o.k.s().data(), o.k.s().size()) == "ab"); }
    { G o = frame([](sofab::OStream &os) { os.write(1, 6.0f); os.write(2, std::uint8_t{1}); }); CHECK(o.k.has_f2() && o.k.f2() == 6.0f); }
    return fails ? 1 : 0;
}
`

// TestCppUnionSameBindArmsDecode runs sameBindMain against corelib-c-cpp in both
// storage modes: the shared scalar arms (tag from the id), the selecting string
// read, a mistyped child that must not switch, and last-option-wins in both
// directions -- none of which check_union's schema pairs up by bind type.
func TestCppUnionSameBindArmsDecode(t *testing.T) {
	r := newCCppRunner(t)
	for _, dyn := range []bool{false, true} {
		r.run(t, sameBindYAML, "g.hpp", sameBindMain, map[string]any{"corelib": "c-cpp", "allow_dynamic": dyn})
	}
}

// cCppRunner compiles corelib-c-cpp's C sources once (SOFAB_C_DIR) for tests
// that build and run a generated c-cpp header against it; it skips when the
// checkout or a compiler is missing.
type cCppRunner struct {
	gxx, inc string
	objs     []string
}

func newCCppRunner(t *testing.T, cflags ...string) *cCppRunner {
	t.Helper()
	cc := os.Getenv("SOFAB_C_DIR")
	if cc == "" {
		t.Skip("set SOFAB_C_DIR to a corelib-c-cpp checkout to run the run-time gate")
	}
	gxx, err := exec.LookPath("g++")
	if err != nil {
		t.Skip("g++ not found")
	}
	gcc, err := exec.LookPath("gcc")
	if err != nil {
		t.Skip("gcc not found")
	}
	r := &cCppRunner{gxx: gxx, inc: "-I" + filepath.Join(cc, "src", "include")}
	obj := t.TempDir()
	for _, src := range []string{"istream", "ostream", "object", "utf8"} {
		o := filepath.Join(obj, src+".o")
		args := append([]string{"-std=c99", "-O2", r.inc}, cflags...)
		args = append(args, "-c", filepath.Join(cc, "src", src+".c"), "-o", o)
		if out, err := exec.Command(gcc, args...).CombinedOutput(); err != nil {
			t.Fatalf("corelib %s.c: %s", src, out)
		}
		r.objs = append(r.objs, o)
	}
	return r
}

// run generates schema under cfg, builds main against the generated header hdr
// and runs it; cxxflags are added to the C++ compile and link.
func (r *cCppRunner) run(t *testing.T, schema, hdr, main string, cfg map[string]any, cxxflags ...string) {
	t.Helper()
	dir := t.TempDir()
	files := unionFiles(t, schema, cfg)
	if err := os.WriteFile(filepath.Join(dir, hdr), []byte(files[hdr]), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.cpp"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "run")
	args := append([]string{"-std=c++20", "-O2", "-Wall", "-Wextra", "-Werror", r.inc, "-I" + dir}, cxxflags...)
	args = append(args, filepath.Join(dir, "main.cpp"), "-o", exe)
	args = append(args, r.objs...)
	if out, err := exec.Command(r.gxx, args...).CombinedOutput(); err != nil {
		t.Fatalf("%v: compile:\n%s", cfg, out)
	}
	if out, err := exec.Command(exe).CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", cfg, err, out)
	}
}

// copyYAML: default_id is a struct option whose string member defaults to a
// value too long for std::string's inline buffer, so constructing that option
// allocates under allow_dynamic; the other option is a string.
const copyYAML = `
version: 1
messages:
  c:
    payload:
      k: { id: 0, type: union, oneof: { d: { id: 0, type: struct, fields: { n: { id: 0, type: string, maxlen: 80, default: "a default long enough to live on the heap, not inline" } } }, s: { id: 1, type: string, maxlen: 80 } } }
`

// copyMain copies unions holding each option, by construction and by
// assignment, and checks what the copy holds.
const copyMain = `#include "c.hpp"
#include <cstdio>
#include <string>

using namespace sofabuffers;
static int fails = 0;
#define CHECK(c) do { if (!(c)) { std::printf("FAIL line %d: %s\n", __LINE__, #c); fails++; } } while (0)

static std::string str(const auto &v) { return std::string(v.data(), v.size()); }

int main() {
    const std::string x(60, 'x');
    C a;
    a.k.set_s(x);
    { C b(a); CHECK(b.k.has_s() && str(b.k.s()) == x); }
    { auto k(a.k); CHECK(k.has_s() && str(k.s()) == x); }
    { C b; b = a; CHECK(b.k.has_s() && str(b.k.s()) == x); }
    { C d; C b(d); CHECK(b.k.has_d() && str(b.k.d().n) == str(d.k.d().n)); }
    { C d; C b; b.k.set_s(x); b = d; CHECK(b.k.has_d() && str(b.k.d().n) == str(d.k.d().n)); }
    return fails ? 1 : 0;
}
`

// TestCppUnionCopyStartsNoOption: the copy constructor of a c-cpp union whose
// options are not trivially copyable must start its storage with no option
// alive before it places the held one. A union constructor naming no variant
// member constructs default_id from its default member initializer, and
// placing another option over that leaks default_id's heap storage under
// allow_dynamic, which LeakSanitizer reports here.
func TestCppUnionCopyStartsNoOption(t *testing.T) {
	for _, dyn := range []bool{false, true} {
		h := unionFiles(t, copyYAML, map[string]any{"corelib": "c-cpp", "allow_dynamic": dyn})["c.hpp"]
		s := section(t, h, "C_K")
		for _, want := range []string{"explicit _Opts(std::nullptr_t) noexcept : _none() {}", "_None _none;"} {
			if !strings.Contains(s, want) {
				t.Errorf("dyn=%v: _Opts(nullptr) must start the do-nothing _none member (%q):\n%s", dyn, want, s)
			}
		}
	}
	san := []string{"-fsanitize=address,undefined", "-fno-sanitize-recover=all", "-fno-omit-frame-pointer"}
	r := newCCppRunner(t, san...)
	t.Setenv("ASAN_OPTIONS", "detect_leaks=1")
	for _, dyn := range []bool{false, true} {
		r.run(t, copyYAML, "c.hpp", copyMain, map[string]any{"corelib": "c-cpp", "allow_dynamic": dyn}, san...)
	}
}

// TestCppMakefileSetsNoUnionSwitch: corelib-c-cpp has no union switch, so no
// generated Makefile -- c-cpp with or without a union, or corelib: cpp -- sets
// one.
func TestCppMakefileSetsNoUnionSwitch(t *testing.T) {
	const plain = `
version: 1
messages:
  p:
    payload:
      s: { id: 0, type: struct, fields: { a: { id: 0, type: u8, default: 3 } } }
`
	for name, c := range map[string]struct {
		src string
		cfg map[string]any
	}{
		"c-cpp without a union": {plain, map[string]any{"corelib": "c-cpp"}},
		"c-cpp with a union":    {unionShapeYAML, map[string]any{"corelib": "c-cpp"}},
		"corelib: cpp":          {unionShapeYAML, map[string]any{}},
	} {
		got := unionFiles(t, c.src, c.cfg)["Makefile"]
		if strings.Contains(got, "_UNION_SUPPORT") || strings.Contains(got, "SOFAB_DEFINES") {
			t.Errorf("%s: Makefile must not set a union switch:\n%s", name, got)
		}
	}
}

// TestCppUnionCorelibCapabilityGuard: a corelib: c-cpp header whose message
// reaches a union refuses a corelib-c-cpp without SOFAB_OBJECT_DESCR_UNION (one
// that predates tagged unions, so has no readMatch) by name, the C header's
// test and wording; a union-free one, and every corelib: cpp header, does not
// ask. The build half hides the macro and checks the guard's message is what
// the compiler reports.
func TestCppUnionCorelibCapabilityGuard(t *testing.T) {
	const plain = `
version: 1
messages:
  p:
    payload:
      w: { id: 0, type: u8 }
`
	guard := "#if !defined(SOFAB_OBJECT_DESCR_UNION)"
	msg := "which this corelib-c-cpp predates"
	h := unionFiles(t, unionShapeYAML, map[string]any{"corelib": "c-cpp"})["m.hpp"]
	if !strings.Contains(h, "#include \"sofab/object.h\"\n"+guard) || !strings.Contains(h, msg) {
		t.Errorf("a c-cpp message with a union must refuse a corelib without SOFAB_OBJECT_DESCR_UNION:\n%s", h)
	}
	for name, got := range map[string]string{
		"c-cpp without a union": unionFiles(t, plain, map[string]any{"corelib": "c-cpp"})["p.hpp"],
		"corelib: cpp union":    unionFiles(t, unionShapeYAML, nil)["m.hpp"],
	} {
		if got == "" || strings.Contains(got, guard) || strings.Contains(got, "sofab/object.h") {
			t.Errorf("%s: must not ask for SOFAB_OBJECT_DESCR_UNION:\n%s", name, got)
		}
	}

	cc := os.Getenv("SOFAB_C_DIR")
	if cc == "" {
		t.Skip("set SOFAB_C_DIR to a corelib-c-cpp checkout to run the build half")
	}
	gxx, err := exec.LookPath("g++")
	if err != nil {
		t.Skip("g++ not found")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "m.hpp"), []byte(h), 0o644); err != nil {
		t.Fatal(err)
	}
	tu := "#include \"sofab/object.h\"\n#undef SOFAB_OBJECT_DESCR_UNION\n#include \"m.hpp\"\n"
	if err := os.WriteFile(filepath.Join(dir, "old.cpp"), []byte(tu), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(gxx, "-std=c++20", "-fsyntax-only", "-I"+filepath.Join(cc, "src", "include"), "-I"+dir,
		filepath.Join(dir, "old.cpp")).CombinedOutput()
	if err == nil || !strings.Contains(string(out), msg) {
		t.Fatalf("a corelib without SOFAB_OBJECT_DESCR_UNION must fail with the guard's message, got err=%v:\n%s", err, out)
	}
}
