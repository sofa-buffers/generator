package ir

// Worst-case encoded size of a message (PLAN §5.5).
//
// The wire format is language-agnostic: the same message definition encodes to
// the same bytes in every target, so its worst case is a property of the SCHEMA
// alone and is computed here, once, for all backends. Every per-type maximum is
// known exactly — nothing is estimated:
//
//	bool                 1     a single 0/1 varint byte
//	u8  / i8             2     7 value bits per varint byte; i* is ZigZag-mapped
//	u16 / i16            3     into the same width, so signed costs no more
//	u32 / i32 / enum     5
//	u64 / i64 / bitfield 10
//	fp32                 1 + 4 fixlen word + payload
//	fp64                 1 + 8
//	string / blob        fixlen word + maxlen
//
// Each field additionally pays its (id<<3)|wiretype header varint, an array its
// element-count varint, and a sequence its one-byte terminator. A struct's
// children are summed, a union is charged its largest option (structWireMax).
//
// A field with no schema bound — a string/blob without `maxlen`, an array
// without `count` — has NO worst case, and this walk says so by returning
// bounded == false. It never substitutes a number of its own: the caller decides
// what to do (see the `max_message_size` config key), because a fabricated bound
// that looks computed is worse than an admitted absence. Receiver-side decode
// limits (`max_dyn_*`) are deliberately NOT consulted: they cap what this peer
// accepts on decode, which is not what it may legitimately build on encode.

// DynCaps substitutes a receiver-side decode limit for a field's missing schema
// bound. It belongs ONLY to walks that size a DECODE buffer — how much this peer
// is willing to take in. It must never reach an encode-size walk: a cap on what
// I accept says nothing about what I may legitimately send, and sizing an encode
// buffer by it would refuse a message the schema permits.
type DynCaps struct {
	ArrayCount, StringLen, BlobLen int64
	HasArray, HasString, HasBlob   bool
}

func (c *DynCaps) arrayCount() (int64, bool) {
	if c == nil || !c.HasArray {
		return 0, false
	}
	return c.ArrayCount, true
}

func (c *DynCaps) payloadLen(k Kind) (int64, bool) {
	if c == nil {
		return 0, false
	}
	if k == KindString {
		return c.StringLen, c.HasString
	}
	return c.BlobLen, c.HasBlob
}

// MaxWireSize returns the worst-case encoded byte length of a payload — every
// field present, every bound exhausted — and whether that worst case exists at
// all. When bounded is false the size is meaningless and must not be used.
func MaxWireSize(fields []*Field) (size int64, bounded bool) {
	seen := map[string]bool{}
	for _, f := range fields {
		c, ok := fieldSize(f, seen, mode{})
		if !ok {
			return 0, false
		}
		size += c
	}
	return size, true
}

// MaxUnionWireSize is MaxWireSize for a union's options encoded as a payload of
// their own, with no frame around them — the shape a target that can encode a
// union type by itself writes. One option is written, so the worst case is the
// largest option (structWireMax says why that is exact).
func MaxUnionWireSize(options []*Field) (size int64, bounded bool) {
	seen := map[string]bool{}
	for _, f := range options {
		c, ok := fieldSize(f, seen, mode{})
		if !ok {
			return 0, false
		}
		size = max(size, c)
	}
	return size, true
}

// MaxFieldWireSize returns the worst-case ENCODED byte length of a single field,
// header included. seen guards recursion through the shared named-type graph; a
// nil map is accepted for a one-shot call.
func MaxFieldWireSize(f *Field, seen map[string]bool) (int64, bool) {
	return fieldSize(f, seen, mode{})
}

// MaxFieldDecodeSpan returns the worst-case byte span a single field can occupy
// on the wire as RECEIVED, for sizing a decode/reassembly window.
//
// It differs from MaxFieldWireSize in two ways, both forced by what a decoder
// must tolerate rather than what an encoder produces:
//
//   - Every varint is charged its WIDEST LEB128 form (10 bytes), not the width
//     its declared type needs. CORELIB_PLAN §4.1 obliges a decoder to accept a
//     non-minimal encoding and normalize it, so a conformant peer may legally
//     pad a u32 to ten bytes. A window sized to the type-exact five would
//     reject that message.
//   - Receiver-side decode limits (max_dyn_*) stand in for missing schema
//     bounds, because those caps are precisely what this peer will accept.
func MaxFieldDecodeSpan(f *Field, seen map[string]bool, caps *DynCaps) (int64, bool) {
	return fieldSize(f, seen, mode{caps: caps, decode: true})
}

// mode selects which of the two questions the shared walk is answering.
type mode struct {
	caps   *DynCaps
	decode bool
}

// varint returns the bytes to charge for a value of kind k.
func (m mode) varint(k Kind) int64 {
	if m.decode {
		return 10 // a peer may pad any varint to its widest form (§4.1)
	}
	return scalarWireMax(k)
}

func fieldSize(f *Field, seen map[string]bool, m mode) (int64, bool) {
	caps := m.caps
	if seen == nil {
		seen = map[string]bool{}
	}
	// (id<<3)|wiretype, with the wiretype bits saturated: the header can never
	// be wider than this regardless of which wire type the field uses.
	hdr := varintLen(uint64(f.ID)<<3 | 7)

	switch f.Kind {
	case KindBool, KindU8, KindU16, KindU32, KindU64,
		KindI8, KindI16, KindI32, KindI64, KindEnum, KindBitfield:
		return hdr + m.varint(f.Kind), true

	case KindFP32:
		return hdr + 1 + 4, true
	case KindFP64:
		return hdr + 1 + 8, true

	case KindString, KindBlob:
		maxlen, ok := f.Maxlen, f.HasMaxlen
		if !ok {
			maxlen, ok = caps.payloadLen(f.Kind)
		}
		if !ok {
			return 0, false // unbounded payload
		}
		return hdr + fixlenWordLen(maxlen) + maxlen, true

	case KindArray:
		body, ok := arrayWireMax(f.Elem, f.ElemRef, f.ElemItems, f.Count,
			f.ElemMaxHas, f.ElemMax, seen, m)
		if !ok {
			return 0, false
		}
		return hdr + body, true

	case KindStruct, KindUnion:
		// sequence: header opens it, children, one-byte terminator. What the
		// children cost is structWireMax's question: a struct's sum, a union's
		// largest option.
		inner, ok := structWireMax(f.Ref, seen, m)
		if !ok {
			return 0, false
		}
		return hdr + inner + 1, true
	}
	return hdr, true
}

// structWireMax returns what a struct's or union's children cost inside their
// sequence frame. A type reached recursively has no static worst case, so a
// cycle reports unbounded, and one unbounded child — a union option included —
// makes the whole type unbounded.
//
// A struct is charged the SUM of its fields: every one can be on the wire at
// once.
//
// On ENCODE a union is charged its LARGEST option, and that is exact, not an
// estimate. A union holds exactly one option and an encoder writes at most that
// one (MESSAGE_SPEC §4.2): the default_id option only when it is off its
// default, any other held option always — even at its own default, as its value
// or the explicit empty form (§2). An option at its default is never larger than
// the same option filled to its bound, so the forced write cannot exceed the
// maximum, and holding the largest option filled to its bound reaches it.
//
// On DECODE (m.decode) a union keeps the sum of its options. A receiver must take
// a union frame carrying several options — the last correctly-typed one wins and
// the others are not INVALID (§7.4.1) — and a peer that still generates unions as
// product types sends exactly that: every option, once. A decode span sized to the
// largest option would refuse such a message.
func structWireMax(ref *TypeRef, seen map[string]bool, m mode) (int64, bool) {
	if ref == nil || ref.Target == nil || seen[ref.Key] {
		return 0, false
	}
	seen[ref.Key] = true
	defer delete(seen, ref.Key)

	largest := ref.Target.Category == CatUnion && !m.decode
	var inner int64
	for _, c := range ref.Target.Fields {
		cc, ok := fieldSize(c, seen, m)
		if !ok {
			return 0, false
		}
		if largest {
			inner = max(inner, cc)
		} else {
			inner += cc
		}
	}
	return inner, true
}

// arrayWireMax returns an array's payload cost, excluding the field's own header.
//
// An array with no `count` is unbounded whatever its element type — the single
// check that belongs at the top, not repeated per element kind (it was once
// missing from exactly one of four branches in one backend, which sized an
// unbounded array at zero bytes).
//
// Numeric/enum/bool/bitfield elements use the native count-prefixed array wire
// type; fp32/fp64 additionally carry one fixlen word for the whole array, even
// when empty. Every other element kind — string, blob, struct, union, nested
// array — lowers to a wrapper sequence whose child ids are the element indices.
func arrayWireMax(elem Kind, ref *TypeRef, items *ArrayElem, count int64,
	elemMaxHas bool, elemMax int64, seen map[string]bool, m mode) (int64, bool) {

	caps := m.caps

	if count <= 0 {
		c, ok := caps.arrayCount()
		if !ok {
			return 0, false // no `count` -> unbounded
		}
		count = c
	}
	// Worst-case header of a wrapper element: the last index is the largest id.
	idHdr := varintLen(uint64(count)<<3 | 7)

	switch elem {
	// The wrapper cases below cost `elements + 1`, NOT `1 + elements + 1`: the
	// field's own header IS the sequence_begin, and only the terminator is
	// extra. Every per-backend model carried that surplus byte per wrapper
	// array; a max-fill encode measured 143 bytes against a computed 145.
	case KindString, KindBlob:
		if !elemMaxHas {
			var ok bool
			if elemMax, ok = caps.payloadLen(elem); !ok {
				return 0, false
			}
		}
		per := idHdr + fixlenWordLen(elemMax) + elemMax
		return count*per + 1, true

	case KindStruct, KindUnion:
		// The element's own frame holds a struct's fields or one union option,
		// charged exactly as for a field of that type (structWireMax).
		inner, ok := structWireMax(ref, seen, m)
		if !ok {
			return 0, false
		}
		per := idHdr + inner + 1 // element sequence header + body + terminator
		return count*per + 1, true

	case KindArray:
		if items == nil {
			return 0, false
		}
		inner, ok := arrayWireMax(items.Elem, items.ElemRef, items.ElemItems,
			items.Count, items.ElemMaxHas, items.ElemMax, seen, m)
		if !ok {
			return 0, false
		}
		return count*(idHdr+inner) + 1, true

	case KindFP32:
		return varintLen(uint64(count)) + 1 + count*4, true
	case KindFP64:
		return varintLen(uint64(count)) + 1 + count*8, true

	default: // native varint array
		return varintLen(uint64(count)) + count*m.varint(elem), true
	}
}

// scalarWireMax is the widest LEB128 encoding a value of this kind can reach.
// Signed kinds are ZigZag-mapped onto the same width as their unsigned peer, so
// they cost the same. An enum is carried in a 32-bit value, a bitfield in a
// 64-bit one.
func scalarWireMax(k Kind) int64 {
	switch k {
	case KindBool:
		return 1
	case KindU8, KindI8:
		return 2
	case KindU16, KindI16:
		return 3
	case KindU32, KindI32, KindEnum:
		return 5
	case KindFP32:
		return 4
	case KindFP64:
		return 8
	default: // u64 / i64 / bitfield
		return 10
	}
}

// fixlenWordLen is the length of the (len<<3)|subtype word introducing a
// string/blob payload of n bytes.
func fixlenWordLen(n int64) int64 { return varintLen(uint64(n) << 3) }

// varintLen returns the number of bytes in the LEB128 encoding of x.
func varintLen(x uint64) int64 {
	n := int64(1)
	for x >= 0x80 {
		x >>= 7
		n++
	}
	return n
}
