package c

import (
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// cDefaultInit returns the C initializer expression for a leaf field's schema
// default and whether it is worth emitting. A zero or absent default returns
// ("", false): the descriptor then omits the const default image and the corelib
// compares the field against zero storage, so no .rodata is spent for the common
// all-default-zero case. Sequences (struct/union and wrapper-sequence arrays) are
// never routed here — they carry their defaults in their own descriptor.
func (g *gen) cDefaultInit(f *ir.Field) (string, bool) {
	// A bitfield derives its default from the set flags, not a field Default.
	if f.Kind == ir.KindBitfield {
		if bits := g.bitfieldDefault(f); bits != 0 {
			return cMaskLit(bits), true
		}
		return "", false
	}
	if f.Default == nil {
		return "", false
	}
	switch f.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		if s := scalarLit(f.Default); !isZeroInt(s) {
			return intLit(f.Kind, s), true
		}
	case ir.KindBool:
		if b, ok := f.Default.(bool); ok && b {
			return "1", true // the member is a uint8_t
		}
	case ir.KindFP32:
		if s := floatLit(f.Default); !isZeroFloat(s) {
			return s + "f", true
		}
	case ir.KindFP64:
		if s := floatLit(f.Default); !isZeroFloat(s) {
			return s, true
		}
	case ir.KindString:
		if s, ok := f.Default.(string); ok && s != "" {
			return fmt.Sprintf("%q", s), true // char[N] is zero-padded past the string
		}
	case ir.KindBlob:
		if s, ok := f.Default.(string); ok {
			if raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), "")); err == nil && !allZero(raw) {
				return fmt.Sprintf("{ %s }", byteList(raw)), true
			}
		}
	case ir.KindArray:
		return g.cArrayDefaultInit(f)
	}
	return "", false
}

// blobDefaultRawLen returns the decoded byte length of a blob field's schema
// default and whether it is worth materializing. It mirrors cDefaultInit's blob
// case (same base64 decode, same all-zero => absent-default convention) so the
// companion _len init and the default image stay in lockstep.
func blobDefaultRawLen(f *ir.Field) (int64, bool) {
	if f.Kind != ir.KindBlob {
		return 0, false
	}
	s, ok := f.Default.(string)
	if !ok {
		return 0, false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil || allZero(raw) {
		return 0, false
	}
	return int64(len(raw)), true
}

// arrayDefaultLen returns the element count of a compact array field's declared
// default, and whether it is worth materializing. MESSAGE_SPEC §3 makes `count`
// a capacity and the wire count the length, so a short `default` stays short —
// it is NOT padded out to N. The length is the value here, so unlike
// cArrayDefaultInit this reports an all-zero default too ([0,0,0] is a
// three-element array, not the empty one); only an absent or empty default has
// nothing to record, and that is the zero the memset already leaves.
func arrayDefaultLen(f *ir.Field) (int64, bool) {
	if f.Kind != ir.KindArray {
		return 0, false
	}
	vals, ok := f.Default.([]any)
	if !ok || len(vals) == 0 {
		return 0, false
	}
	n := int64(len(vals))
	if n > f.Count { // validation rejects this; clamp so storage is never overrun
		n = f.Count
	}
	return n, true
}

// cArrayDefaultInit renders a native scalar array's schema default as a C brace
// initializer ("{ e0, e1, ... }"); ("", false) when there is no default or every
// element is zero (the array member is then left zero-initialized). Composite /
// wrapper-sequence arrays never reach here (they route through the sequence path
// in collect and carry no whole-field default).
func (g *gen) cArrayDefaultInit(f *ir.Field) (string, bool) {
	vals, ok := f.Default.([]any)
	if !ok || len(vals) == 0 {
		return "", false
	}
	parts := make([]string, len(vals))
	nonZero := false
	for i, v := range vals {
		var e string
		switch f.Elem {
		case ir.KindFP32:
			e = floatLit(v) + "f"
			nonZero = nonZero || !isZeroFloat(floatLit(v))
		case ir.KindFP64:
			e = floatLit(v)
			nonZero = nonZero || !isZeroFloat(e)
		case ir.KindBool:
			if b, ok := v.(bool); ok && b {
				e, nonZero = "1", true
			} else {
				e = "0"
			}
		case ir.KindBitfield:
			// The same hole as the scalar default, one level in: an element mask
			// with bit 63 set is written as a bare decimal, which no signed type
			// holds, so the initializer is `integer constant is so large that it
			// is unsigned` — a hard error under -pedantic-errors.
			if bits, err := strconv.ParseUint(scalarLit(v), 10, 64); err == nil {
				e = cMaskLit(bits)
			} else {
				// Reachable, and deliberately a pass-through. internal/parser's
				// checkArrayElem has no "bitfield" arm, so an element default that
				// is not a plain decimal — a negative, a float, a quoted "0x10", a
				// magnitude past uint64 — reaches codegen unchecked (measured).
				// Parsing it as a mask anyway would silently change the VALUE (-1
				// would become 0), so the text is carried through unaltered and the
				// C compiler judges it, which for the one spelling this fix is
				// about — a decimal wider than uint64 — it does: "integer constant
				// is too large for its type". The validator gap is generator#482;
				// this arm goes away when the parser rejects those shapes.
				e = scalarLit(v)
			}
			nonZero = nonZero || !isZeroInt(scalarLit(v))
		default: // integer / enum
			e = intLit(f.Elem, scalarLit(v))
			nonZero = nonZero || !isZeroInt(scalarLit(v))
		}
		parts[i] = e
	}
	if !nonZero {
		return "", false
	}
	return "{ " + strings.Join(parts, ", ") + " }", true
}

// bitfieldDefault folds a bitfield field's flag defaults into its backing value.
func (g *gen) bitfieldDefault(f *ir.Field) uint64 {
	var bits uint64
	for _, fl := range f.Ref.Target.Flags {
		if fl.HasDefault && fl.Default {
			bits |= 1 << uint(fl.Pos)
		}
	}
	return bits
}

// cMaskLit renders a bitfield mask as a C integer constant.
//
// The "ULL" suffix is not decoration. C11 6.4.4.1 looks an UNSUFFIXED decimal
// constant up in the SIGNED list only, so the mask for position 63 —
// 9223372036854775808 — has no type at all in strict C99+: GCC accepts it as an
// extension with "integer constant is so large that it is unsigned", which is a
// warning under -pedantic and an ERROR under -pedantic-errors. The suffix is
// harmless on a narrower backing type (uint8_t..uint32_t), where the constant
// converts to the member's type as usual.
//
// Every place this backend turns a mask into C source text goes through here —
// the scalar default and the element of an array-of-bitfield default — so a
// third site cannot drift from the first two, which is how generator#480
// existed at all.
func cMaskLit(bits uint64) string {
	return fmt.Sprintf("%dULL", bits)
}

// intLit widens an integer literal to its C member type: a "ULL" suffix for
// u64 (so a value above LLONG_MAX is not mis-typed), and for i64 an "LL" suffix
// — except INT64_MIN, whose magnitude overflows long long when written as a
// negated literal, so it is emitted as the canonical "(-MAX - 1)" form.
func intLit(kind ir.Kind, s string) string {
	switch kind {
	case ir.KindU64:
		return s + "ULL"
	case ir.KindI64:
		if v, err := strconv.ParseInt(s, 10, 64); err == nil && v == math.MinInt64 {
			return "(-9223372036854775807LL - 1)"
		}
		return s + "LL"
	default:
		return s
	}
}

// scalarLit renders an integer/enum default. Values that overflow int64 arrive
// as an exact decimal string (e.g. u64 max) and are passed through verbatim.
func scalarLit(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// floatLit renders a floating-point default, always with a decimal point so a
// following "f" suffix is a valid C float constant (e.g. "1" -> "1.0", never
// "1f").
func floatLit(v any) string {
	var fv float64
	switch x := v.(type) {
	case float64:
		fv = x
	case int:
		fv = float64(x)
	case int64:
		fv = float64(x)
	default:
		return "0.0"
	}
	s := fmt.Sprintf("%g", fv)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// byteList renders raw bytes as a comma-separated C hex list ("0x01, 0x02").
func byteList(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("0x%02x", x)
	}
	return strings.Join(parts, ", ")
}

func isZeroInt(s string) bool { return s == "0" || s == "-0" }

func isZeroFloat(s string) bool {
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && f == 0
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
