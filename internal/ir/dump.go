package ir

import (
	"bytes"
	"encoding/json"
)

// dumpField is the stable JSON projection of a Field (a snapshot view used for
// golden testing and --dump-ir). Composite fields render their resolved target
// key, so the snapshot captures the shared-type graph.
type dumpField struct {
	Name       string    `json:"name"`
	ID         int64     `json:"id"`
	Kind       string    `json:"kind"`
	Ref        string    `json:"ref,omitempty"`
	Elem       string    `json:"elem,omitempty"`
	Count      int64     `json:"count,omitempty"`
	ElemRef    string    `json:"elem_ref,omitempty"`
	ElemItems  *dumpElem `json:"elem_items,omitempty"`
	Maxlen     *int64    `json:"maxlen,omitempty"`
	ElemMaxlen *int64    `json:"elem_maxlen,omitempty"`
	Decimals   *int      `json:"decimals,omitempty"`
	Deprecated bool      `json:"deprecated,omitempty"`
	HasDefault bool      `json:"has_default,omitempty"`
	Unit       string    `json:"unit,omitempty"`
}

// dumpElem is the JSON projection of a nested array element (array-of-array).
type dumpElem struct {
	Elem       string    `json:"elem"`
	Count      int64     `json:"count,omitempty"`
	ElemRef    string    `json:"elem_ref,omitempty"`
	ElemMaxlen *int64    `json:"elem_maxlen,omitempty"`
	ElemItems  *dumpElem `json:"elem_items,omitempty"`
}

type dumpNamed struct {
	Category string      `json:"category"`
	Key      string      `json:"key"`
	Inline   bool        `json:"inline,omitempty"`
	Fields   []dumpField `json:"fields,omitempty"`
	Consts   []dumpConst `json:"consts,omitempty"`
	Flags    []dumpFlag  `json:"flags,omitempty"`
}

type dumpConst struct {
	Name  string `json:"name"`
	Value int64  `json:"value"`
}

type dumpFlag struct {
	Name string `json:"name"`
	Pos  int64  `json:"pos"`
}

type dumpMessage struct {
	Name   string      `json:"name"`
	Fields []dumpField `json:"fields"`
}

type dumpSchema struct {
	Version  int           `json:"version"`
	Messages []dumpMessage `json:"messages"`
	Named    []dumpNamed   `json:"named"`
}

func projectField(f *Field) dumpField {
	d := dumpField{
		Name:       f.Name,
		ID:         f.ID,
		Kind:       f.Kind.String(),
		Deprecated: f.Deprecated,
		HasDefault: f.Default != nil,
		Unit:       f.Unit,
		Decimals:   f.Decimals,
	}
	if f.Ref != nil {
		d.Ref = f.Ref.Key
	}
	if f.Kind == KindArray || f.Kind == KindMap {
		d.Elem = f.Elem.String()
		d.Count = f.Count
		if f.ElemMaxHas {
			m := f.ElemMax
			d.ElemMaxlen = &m
		}
		if f.ElemRef != nil {
			d.ElemRef = f.ElemRef.Key
		}
		d.ElemItems = projectElem(f.ElemItems)
	}
	if f.HasMaxlen {
		m := f.Maxlen
		d.Maxlen = &m
	}
	return d
}

func projectElem(e *ArrayElem) *dumpElem {
	if e == nil {
		return nil
	}
	d := &dumpElem{Elem: e.Elem.String(), Count: e.Count, ElemItems: projectElem(e.ElemItems)}
	if e.ElemRef != nil {
		d.ElemRef = e.ElemRef.Key
	}
	if e.ElemMaxHas {
		m := e.ElemMax
		d.ElemMaxlen = &m
	}
	return d
}

func projectFields(fs []*Field) []dumpField {
	out := make([]dumpField, len(fs))
	for i, f := range fs {
		out[i] = projectField(f)
	}
	return out
}

// Dump renders the schema as deterministic, indented JSON — the canonical
// snapshot for golden tests and the --dump-ir CLI flag. Ordering is already
// deterministic in the IR (fields by id, named types by key).
func (s *Schema) Dump() []byte {
	ds := dumpSchema{Version: s.Version}
	for _, m := range s.Messages {
		ds.Messages = append(ds.Messages, dumpMessage{Name: m.Name, Fields: projectFields(m.Fields)})
	}
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		dn := dumpNamed{Category: categoryName(nt.Category), Key: nt.Key, Inline: nt.Inline}
		switch nt.Category {
		case CatStruct, CatUnion:
			dn.Fields = projectFields(nt.Fields)
		case CatEnum:
			for _, c := range nt.Consts {
				dn.Consts = append(dn.Consts, dumpConst{Name: c.Name, Value: c.Value})
			}
		case CatBitfield:
			for _, fl := range nt.Flags {
				dn.Flags = append(dn.Flags, dumpFlag{Name: fl.Name, Pos: fl.Pos})
			}
		}
		ds.Named = append(ds.Named, dn)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(ds)
	return buf.Bytes()
}

func categoryName(c Category) string {
	switch c {
	case CatStruct:
		return "struct"
	case CatUnion:
		return "union"
	case CatEnum:
		return "enum"
	case CatBitfield:
		return "bitfield"
	}
	return "unknown"
}
