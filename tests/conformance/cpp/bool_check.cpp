// CORELIB_PLAN §4.4 on the decode side of a generated C++ message, in every
// profile (generator#581).
//
// A boolean is canonical on encode and tolerant on decode: every value other
// than 0 reads as true, is normalized away (the member holds 1, and a re-encode
// emits 1), and carries no width bound -- 256 and 2^64-1 are true, not INVALID
// and not truncated. A boolean ARRAY is generated with std::uint8_t elements
// (std::vector<bool> cannot be a decode destination), so the harness's JSON
// output cannot see this rule at all: it prints `v ? true : false`, and an
// element stored raw as 2 prints `true` exactly like a normalized one. This check
// inspects the stored bytes and the re-encoded wire instead.
//
// The defect it pins: read as a u8 array, 2 was kept and re-encoded as 02, and
// 256 was INVALID on corelib-c-cpp and silently decoded as FALSE on corelib-cpp.
//
// Every case runs twice: one-shot, and fed one byte at a time, so each element
// varint is split across feeds and has to be resumed into the same destination.
//
// Positions: a scalar, an array, a nested-array row, an array inside a struct,
// a union arm and a depth-3 row.
//
// The message header, MSG_TYPE and the stream arguments come from run.sh, which
// knows the profile. Nothing below depends on the storage mode: indexing and
// size() mean the same on std::vector and on sofab::InlineVector.

#include <cstdint>
#include <cstdio>
#include <cstring>
#include <vector>

#ifdef SOFAB_STREAM_LIMITS
#define SOFAB_STREAM_ARGS {sofab::Limits{SIZE_MAX}}
#else
#define SOFAB_STREAM_ARGS
#endif

// The varints under test: 2 (non-zero, fits a byte), 256 (does not fit a byte,
// and truncates to 0) and 2^64-1 (the widest varint there is).
#define V2 0x02
#define V256 0x80, 0x02
#define VMAX 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01

static int g_failures = 0;

static void fail(const char *surface, const char *what)
{
    std::printf("FAIL: [%s] %s\n", surface, what);
    ++g_failures;
}

// The byte actually stored, not the value a conversion to bool would produce:
// a raw 2 converts to true, which is exactly what hides it.
template <typename T>
static unsigned byteOf(const T &v)
{
    static_assert(sizeof(T) == 1, "a boolean member is one byte");
    unsigned char b;
    std::memcpy(&b, &v, 1);
    return b;
}

template <typename C>
static bool holds(const C &c, std::initializer_list<unsigned> want)
{
    if (c.size() != want.size()) return false;
    std::size_t i = 0;
    for (unsigned w : want)
        if (byteOf(c[i++]) != w) return false;
    return true;
}

// Decode `wire` one-shot or drip-fed. A decode that fails, or ends mid-field,
// is a failure: none of these values is INVALID under §4.4.
static bool decode(const std::vector<std::uint8_t> &wire, bool drip, MSG_TYPE &out)
{
    if (!drip)
        return MSG_TYPE::try_decode(wire.data(), wire.size(), out).ok();
    sofab::IStreamObject<MSG_TYPE> in SOFAB_STREAM_ARGS;
    bool ok = false;
    for (std::size_t i = 0; i < wire.size(); ++i)
    {
        const auto r = in.feed(wire.data() + i, 1);
        if (!r.ok() && !r.incomplete()) return false;
        ok = r.ok();
    }
    if (!ok) return false;
    out = *in;
    return true;
}

int main()
{
    // Header bytes: (id << 3) | wire type, with 0 = unsigned, 3 = unsigned
    // array, 6 = sequence start and 7 = sequence end.
    const std::vector<std::uint8_t> scalar = {
        0x00, V256,                                      // flag = 256
    };
    // Only values that fit a byte: a u8 read accepts these, so this case isolates
    // the normalization from the width bound -- stored raw, 2 and 5 stay 2 and 5.
    const std::vector<std::uint8_t> small = {
        0x0b, 0x03, V2, 0x00, 0x05,                      // flags = [2, 0, 5]
    };
    const std::vector<std::uint8_t> arrays = {
        0x00, VMAX,                                      // flag = 2^64-1
        0x0b, 0x05, V2, 0x01, 0x00, V256, VMAX,          // flags = [2, 1, 0, 256, max]
        0x16,                                            // rows: sequence start
        0x03, 0x02, V2, V256,                            //   row 0 = [2, 256]
        0x0b, 0x03, VMAX, 0x00, 0x05,                    //   row 1 = [max, 0, 5]
        0x07,                                            // rows: sequence end
        0x1e,                                            // nested: sequence start
        0x03, 0x03, 0x00, V256, 0x05,                    //   inner = [0, 256, 5]
        0x07,                                            // nested: sequence end
        0x26,                                            // choice: sequence start
        0x03, 0x03, V256, 0x00, V2,                      //   bits = [256, 0, 2]
        0x07,                                            // choice: sequence end
        0x2e,                                            // cube: sequence start
        0x06,                                            //   row 0: sequence start
        0x03, 0x02, V2, VMAX,                            //     [2, max]
        0x0b, 0x01, 0x00,                                //     [0]
        0x07,                                            //   row 0: sequence end
        0x0e,                                            //   row 1: sequence start
        0x03, 0x01, V256,                                //     [256]
        0x07,                                            //   row 1: sequence end
        0x07,                                            // cube: sequence end
    };

    // The same message written from normalized values: what a re-encode of the
    // tolerant input must reproduce byte for byte.
    MSG_TYPE canonScalar;
    canonScalar.flag = true;
    const std::vector<std::uint8_t> wantScalar = canonScalar.encode();
    MSG_TYPE canonArrays;
    canonArrays.flag = true;
    canonArrays.flags.resize(5);
    for (std::size_t i = 0; i < 5; ++i) canonArrays.flags[i] = (i == 2) ? 0 : 1;
    canonArrays.rows.resize(2);
    canonArrays.rows[0].resize(2);
    canonArrays.rows[0][0] = 1; canonArrays.rows[0][1] = 1;
    canonArrays.rows[1].resize(3);
    canonArrays.rows[1][0] = 1; canonArrays.rows[1][1] = 0; canonArrays.rows[1][2] = 1;
    canonArrays.nested.inner.resize(3);
    canonArrays.nested.inner[0] = 0; canonArrays.nested.inner[1] = 1; canonArrays.nested.inner[2] = 1;
    canonArrays.choice.bits.resize(3);
    canonArrays.choice.bits[0] = 1; canonArrays.choice.bits[1] = 0; canonArrays.choice.bits[2] = 1;
    canonArrays.cube.resize(2);
    canonArrays.cube[0].resize(2);
    canonArrays.cube[0][0].resize(2);
    canonArrays.cube[0][0][0] = 1; canonArrays.cube[0][0][1] = 1;
    canonArrays.cube[0][1].resize(1);
    canonArrays.cube[0][1][0] = 0;
    canonArrays.cube[1].resize(1);
    canonArrays.cube[1][0].resize(1);
    canonArrays.cube[1][0][0] = 1;
    const std::vector<std::uint8_t> wantArrays = canonArrays.encode();
    MSG_TYPE canonSmall;
    canonSmall.flags.resize(3);
    canonSmall.flags[0] = 1; canonSmall.flags[1] = 0; canonSmall.flags[2] = 1;
    const std::vector<std::uint8_t> wantSmall = canonSmall.encode();

    int cases = 0;
    for (bool drip : {false, true})
    {
        const char *surface = drip ? "one byte per feed" : "one-shot";

        MSG_TYPE s;
        ++cases;
        if (!decode(scalar, drip, s))
            fail(surface, "a scalar boolean of 256 must decode (it is true, not INVALID)");
        else
        {
            if (byteOf(s.flag) != 1) fail(surface, "a scalar boolean of 256 must be stored as 1");
            if (s.encode() != wantScalar) fail(surface, "a scalar boolean of 256 must re-encode as 1");
        }

        MSG_TYPE m;
        ++cases;
        if (!decode(small, drip, m))
            fail(surface, "flags [2, 0, 5] must decode");
        else
        {
            if (!holds(m.flags, {1, 0, 1})) fail(surface, "flags [2, 0, 5] must be stored as [1, 0, 1]");
            if (m.encode() != wantSmall) fail(surface, "flags [2, 0, 5] must re-encode as [1, 0, 1]");
        }

        MSG_TYPE a;
        ++cases;
        if (!decode(arrays, drip, a))
        {
            fail(surface, "boolean arrays holding 2, 256 and 2^64-1 must decode (true, not INVALID)");
            continue;
        }
        if (byteOf(a.flag) != 1) fail(surface, "a scalar boolean of 2^64-1 must be stored as 1");
        if (!holds(a.flags, {1, 1, 0, 1, 1}))
            fail(surface, "flags [2, 1, 0, 256, max] must be stored as [1, 1, 0, 1, 1]");
        if (a.rows.size() != 2 || !holds(a.rows[0], {1, 1}) || !holds(a.rows[1], {1, 0, 1}))
            fail(surface, "rows [[2, 256], [max, 0, 5]] must be stored as [[1, 1], [1, 0, 1]]");
        if (!holds(a.nested.inner, {0, 1, 1}))
            fail(surface, "nested.inner [0, 256, 5] must be stored as [0, 1, 1]");
        if (!holds(a.choice.bits, {1, 0, 1}))
            fail(surface, "choice.bits [256, 0, 2] must be stored as [1, 0, 1]");
        if (a.cube.size() != 2 || a.cube[0].size() != 2 || a.cube[1].size() != 1 ||
            !holds(a.cube[0][0], {1, 1}) || !holds(a.cube[0][1], {0}) || !holds(a.cube[1][0], {1}))
            fail(surface, "cube [[[2, max], [0]], [[256]]] must be stored as [[[1, 1], [0]], [[1]]]");
        if (a.encode() != wantArrays)
            fail(surface, "the decoded boolean arrays must re-encode with every true element as 1");
    }

    if (g_failures) return 1;
    std::printf("booleans (§4.4): %d cases, each one-shot and one byte per feed -- every "
                "non-zero value stored and re-encoded as 1\n", cases / 2);
    return 0;
}
