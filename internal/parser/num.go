package parser

import (
	"math"
	"math/big"
)

// asInt coerces a decoded YAML/JSON scalar to int64 when it is an exact
// integer. It accepts int/int64/uint64 and integral float64 within the
// double-safe range. Values needing the full 64-bit width (u64) are handled by
// checkInt64Range, which also accepts strings.
func asInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case uint64:
		if x <= math.MaxInt64 {
			return int64(x), true
		}
		return 0, false
	case float64:
		if x == math.Trunc(x) && isSafeInteger(x) {
			return int64(x), true
		}
		return 0, false
	default:
		return 0, false
	}
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	default:
		return 0, false
	}
}

func isSafeInteger(f float64) bool {
	const maxSafe = 9007199254740991.0 // 2^53 - 1
	return f >= -maxSafe && f <= maxSafe
}

func mustBig(s string) *big.Int {
	n, _ := new(big.Int).SetString(s, 10)
	return n
}

func int64ToBig(n int64) *big.Int   { return big.NewInt(n) }
func uint64ToBig(n uint64) *big.Int { return new(big.Int).SetUint64(n) }

var (
	i64Min = mustBig("-9223372036854775808")
	i64Max = mustBig("9223372036854775807")
	u64Min = big.NewInt(0)
	u64Max = mustBig("18446744073709551615")
)

func in64Range(n *big.Int, kind string) bool {
	if kind == "u64" {
		return n.Cmp(u64Min) >= 0 && n.Cmp(u64Max) <= 0
	}
	return n.Cmp(i64Min) >= 0 && n.Cmp(i64Max) <= 0
}
