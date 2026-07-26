package generator

import (
	"fmt"

	"github.com/sofa-buffers/generator/internal/ir"
)

// MaxMessageSizeKey is the config key holding the ceiling a generated message
// buffer may reach. It is the ONE place a number is chosen when the schema
// cannot supply one, so no backend has to invent its own.
const MaxMessageSizeKey = "max_message_size"

// DefaultMaxMessageSize is that central default, used when the key is unset.
const DefaultMaxMessageSize = 4096

// MessageSize is the resolved worst-case encode size of one message.
//
// Bounded distinguishes the two cases generated code must not conflate: a size
// DERIVED from the schema (exact — the message can never exceed it) and a size
// IMPOSED by configuration because some field is unbounded (a ceiling — the
// message could in principle exceed it, and an encode that would is refused).
// Backends emit the derived number as MAX_SIZE alone, and the imposed one as
// MAX_SIZE_LIMIT with MAX_SIZE aliasing it, so a reader can tell which kind of
// number they are looking at.
type MessageSize struct {
	Size    int64
	Bounded bool
}

// SizePolicy is the max_message_size configuration, resolved once per target.
type SizePolicy struct {
	limit    int64
	explicit bool
}

// NewSizePolicy reads max_message_size from a target's config, falling back to
// DefaultMaxMessageSize.
func NewSizePolicy(cfg map[string]any) SizePolicy {
	switch v := cfg[MaxMessageSizeKey].(type) {
	case int:
		if v > 0 {
			return SizePolicy{limit: int64(v), explicit: true}
		}
	case int64:
		if v > 0 {
			return SizePolicy{limit: v, explicit: true}
		}
	case float64:
		if v > 0 {
			return SizePolicy{limit: int64(v), explicit: true}
		}
	}
	return SizePolicy{limit: DefaultMaxMessageSize}
}

// Limit is the ceiling in effect, configured or default.
func (p SizePolicy) Limit() int64 { return p.limit }

// Resolve computes a message's worst-case encoded size from the schema
// (ir.MaxWireSize) and falls back to the configured ceiling when a reachable
// field has no bound.
//
// When max_message_size is set EXPLICITLY it is also an assertion: a schema
// whose computed worst case exceeds it cannot fit the transport it was
// configured for, and that is reported here rather than discovered at runtime.
// The default value never triggers that check — it exists only to fill the
// unbounded case, so raising a schema past 4096 bytes stays legal unless the
// user has actually declared a smaller budget.
func (p SizePolicy) Resolve(msg string, fields []*ir.Field) (MessageSize, error) {
	size, bounded := ir.MaxWireSize(fields)
	if !bounded {
		return MessageSize{Size: p.limit, Bounded: false}, nil
	}
	if p.explicit && size > p.limit {
		return MessageSize{}, fmt.Errorf(
			"message %q has a worst-case encoded size of %d bytes, which exceeds the configured %s of %d; "+
				"either raise the limit or tighten a count/maxlen in the schema",
			msg, size, MaxMessageSizeKey, p.limit)
	}
	return MessageSize{Size: size, Bounded: true}, nil
}
