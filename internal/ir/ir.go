// Package ir is the language-neutral Intermediate Representation that every
// backend consumes (PLAN §8.2). It is a Composite tree: every code element
// implements Node (Accept for the Visitor pattern, Children for uniform
// traversal). The IR has NO dependency on any other internal package, so a
// backend imports only this — the dependency arrows point inward (§8.6).
//
// Two states share these types:
//   - post-Build (internal/model): TypeRefs may be unresolved (Target == nil).
//   - post-Analyze (internal/analysis): every TypeRef.Target points at the
//     single shared NamedType (the shared-type graph, §3.4) and the tree is
//     frozen — backends must treat it as immutable (§8.6).
package ir

// Kind enumerates every leaf/composite field shape the IR can carry.
type Kind int

const (
	KindInvalid Kind = iota
	KindU8
	KindU16
	KindU32
	KindU64
	KindI8
	KindI16
	KindI32
	KindI64
	KindFP32
	KindFP64
	KindBool
	KindString
	KindBlob
	KindArray    // fixed-count array of a scalar/string/blob element
	KindEnum     // -> NamedType (Enum)
	KindBitfield // -> NamedType (Bitfield)
	KindStruct   // -> NamedType (Struct)
	KindUnion    // -> NamedType (Union)
	KindMap      // map<K,V>: lowered to a wrapper array of a {key,value} entry struct
)

var kindNames = map[Kind]string{
	KindU8: "u8", KindU16: "u16", KindU32: "u32", KindU64: "u64",
	KindI8: "i8", KindI16: "i16", KindI32: "i32", KindI64: "i64",
	KindFP32: "fp32", KindFP64: "fp64", KindBool: "boolean",
	KindString: "string", KindBlob: "blob", KindArray: "array",
	KindEnum: "enum", KindBitfield: "bitfield", KindStruct: "struct", KindUnion: "union",
	KindMap: "map",
}

func (k Kind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	return "invalid"
}

// MaxNestingDepth is the hard SofaBuffers spec limit (PLAN §4.2). Every backend
// shares this one constant; analysis rejects definitions that exceed it.
const MaxNestingDepth = 256

// Node is the Composite interface implemented by every IR element.
type Node interface {
	// Accept dispatches to the matching Visitor method (double dispatch).
	Accept(v Visitor)
	// Children returns the contained nodes for generic recursive walks.
	Children() []Node
	// NodeName is a human label used in diagnostics and traversal logs.
	NodeName() string
}

// Visitor is the generation hook (§8.3). A backend implements it; unhandled
// kinds fall through to the default Walk.
type Visitor interface {
	VisitSchema(*Schema)
	VisitMessage(*Message)
	VisitNamedType(*NamedType)
	VisitField(*Field)
}

// Schema is the IR root: all messages plus the shared named-type graph.
type Schema struct {
	Version  int
	Messages []*Message
	// Named is the shared-type graph keyed by canonical name (e.g.
	// "struct/Point"). Struct/union/enum/bitfield reached via $ref resolve to a
	// single entry here, never duplicated (§3.4).
	Named map[string]*NamedType
	// order preserves a deterministic iteration order over Named.
	NamedOrder []string
}

func (s *Schema) Accept(v Visitor) { v.VisitSchema(s) }
func (s *Schema) NodeName() string { return "schema" }
func (s *Schema) Children() []Node {
	out := make([]Node, 0, len(s.Messages)+len(s.NamedOrder))
	for _, m := range s.Messages {
		out = append(out, m)
	}
	for _, name := range s.NamedOrder {
		out = append(out, s.Named[name])
	}
	return out
}

// Message is a top-level message: a name, optional summary, and an ordered
// payload of fields (one id scope).
type Message struct {
	Name    string
	Summary string
	Fields  []*Field
}

func (m *Message) Accept(v Visitor) { v.VisitMessage(m) }
func (m *Message) NodeName() string { return m.Name }
func (m *Message) Children() []Node {
	out := make([]Node, len(m.Fields))
	for i, f := range m.Fields {
		out[i] = f
	}
	return out
}

// Category distinguishes the four named-type flavours.
type Category int

const (
	CatStruct Category = iota
	CatUnion
	CatEnum
	CatBitfield
)

// NamedType is a shared struct/union/enum/bitfield in the graph. Struct/Union
// carry Fields (their own id scope); Enum carries Consts; Bitfield carries
// Flags.
type NamedType struct {
	Category Category
	Name     string // canonical, e.g. "Point" (unqualified)
	Key      string // graph key, e.g. "struct/Point" or an inline synthetic key
	Summary  string
	Inline   bool // true if it originated inline (not from $defs)

	Fields    []*Field        // struct/union
	Consts    []*EnumConst    // enum
	Flags     []*BitfieldFlag // bitfield
	DefaultID *int64          // union default_id (optional)
}

func (n *NamedType) Accept(v Visitor) { v.VisitNamedType(n) }
func (n *NamedType) NodeName() string { return n.Key }
func (n *NamedType) Children() []Node {
	out := make([]Node, 0, len(n.Fields))
	for _, f := range n.Fields {
		out = append(out, f)
	}
	return out
}

// Field is a single member of a message payload, struct, or union.
type Field struct {
	Name        string
	ID          int64
	Kind        Kind
	Description string
	Unit        string
	Deprecated  bool

	// Scalars / string / blob:
	Default   any   // typed per Kind (int64, float64, bool, string, []byte, []any); nil if absent
	HasMaxlen bool  // string/blob
	Maxlen    int64 // valid when HasMaxlen
	Decimals  *int  // fp32/fp64

	// Array (Kind == KindArray):
	Elem       Kind  // element kind
	HasCount   bool  // capacity present (count is optional → dynamic/unbounded)
	Count      int64 // element capacity (max); 0 when dynamic
	ElemMaxHas bool
	ElemMax    int64
	// Composite / nested array element (when Kind == KindArray):
	//   ElemRef   — set when Elem is enum/bitfield/struct/union
	//   ElemItems — set when Elem is array (array of arrays), recursive
	ElemRef   *TypeRef
	ElemItems *ArrayElem

	// Composite (enum/bitfield/struct/union): the resolved shared type.
	Ref *TypeRef
}

// ArrayElem describes the element of an array whose element is itself an array
// (array-of-array). It mirrors the array portion of a Field, recursively, so the
// nesting can be walked without a Field wrapper.
type ArrayElem struct {
	Elem       Kind       // inner element kind
	ElemRef    *TypeRef   // inner element composite (enum/bitfield/struct/union)
	HasCount   bool       // inner capacity present
	Count      int64      // inner array capacity
	ElemMaxHas bool       // inner string/blob element maxlen present
	ElemMax    int64      // inner string/blob element maxlen
	ElemItems  *ArrayElem // deeper nesting (inner element is itself an array)
}

func (f *Field) Accept(v Visitor) { v.VisitField(f) }
func (f *Field) NodeName() string { return f.Name }
func (f *Field) Children() []Node {
	var out []Node
	if f.Ref != nil && f.Ref.Target != nil {
		out = append(out, f.Ref.Target)
	}
	// Array element composite (enum/bitfield/struct/union), incl. nested arrays.
	if f.ElemRef != nil && f.ElemRef.Target != nil {
		out = append(out, f.ElemRef.Target)
	}
	for e := f.ElemItems; e != nil; e = e.ElemItems {
		if e.ElemRef != nil && e.ElemRef.Target != nil {
			out = append(out, e.ElemRef.Target)
		}
	}
	return out
}

// MapKey returns the key field of a KindMap field's entry struct (id 0). Valid
// only post-Analysis (ElemRef.Target resolved). Fields are id-sorted, so the key
// (id 0) is Fields[0] and the value (id 1) is Fields[1].
func (f *Field) MapKey() *Field { return f.ElemRef.Target.Fields[0] }

// MapValue returns the value field of a KindMap field's entry struct (id 1).
func (f *Field) MapValue() *Field { return f.ElemRef.Target.Fields[1] }

// TypeRef points a composite field at its shared NamedType. After analysis,
// Target is always non-nil; before, only Key is set.
type TypeRef struct {
	Key    string
	Target *NamedType
}

// EnumConst is one enum constant.
type EnumConst struct {
	Name        string
	Value       int64
	Description string
}

// BitfieldFlag is one bitfield flag.
type BitfieldFlag struct {
	Name        string
	Pos         int64
	Default     bool
	HasDefault  bool
	Description string
}

// Walk performs a default depth-first traversal, calling Accept on each node.
// A backend can use this for the parts of the tree it does not override.
func Walk(n Node, v Visitor) {
	n.Accept(v)
	for _, c := range n.Children() {
		Walk(c, v)
	}
}
