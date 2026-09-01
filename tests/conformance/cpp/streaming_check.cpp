// Behavioural check for the C++ streaming API, in every profile.
//
// Both corelibs stream in both directions and always have: OStreamInline<N>
// takes a flush callback and drains its buffer as it fills, and IStreamObject
// holds the parse state so bytes can be fed exactly as they arrive. Nothing in
// the suite drove either. The capability was demonstrable but unverified.
//
// The property is that streaming is indistinguishable from the one-shot path:
//
//   1. serialize through a small flush callback must produce byte-for-byte what
//      encode() produces, however small the buffer.
//   2. feeding IStreamObject in chunks must produce the same value as a single
//      feed, wherever the chunk boundaries fall.
//   3. a cut inside a field must be reported incomplete, not handed over as a
//      half-filled value.
//
// Point 2 is the one that finds real bugs: at a chunk size of 1 every varint,
// every string and every array element is split across feeds, so any parse state
// the decoder fails to carry between calls shows up at once. Writing the Rust
// equivalent turned up a decoder that returned truncated messages as values;
// this is the same net, cast over C++.
//
// The message header and MSG_TYPE are supplied by run.sh via -include/-D, which
// knows the profile. Everything below is profile-independent by construction:
// only operations that mean the same thing whether a field is a std::string or a
// sofab::FixedString<N>, a std::vector or an InlineVector.

#include <cstdint>
#include <cstdio>
#include <cstring>
#include <span>
#include <string_view>
#include <type_traits>
#include <vector>

// corelib-cpp's IStreamObject takes a sofab::Limits -- the byte budget for one
// top-level field -- and offers no constructor that leaves it out (corelib-cpp#128;
// CORELIB_PLAN §6.2.1: a codec supplies no default for a number it was not given).
// The corelib-c-cpp one takes none, its containers being statically bounded, and
// does not declare the type at all, so the choice cannot be a `requires` test and
// run.sh passes it as a -D. This check is about chunk boundaries rather than caps,
// so the pure leg states the platform ceiling -- a number stated, not a mode.
#ifdef SOFAB_STREAM_LIMITS
#define SOFAB_STREAM_ARGS {sofab::Limits{SIZE_MAX}}
#else
#define SOFAB_STREAM_ARGS
#endif

static std::vector<std::uint8_t> g_sink;

// Populate the shapes that matter for chunked feeding: scalars at their widest
// varint, strings (which is what the reassembly buffer exists for), a native
// array (index assignment works in every storage mode) and a nested sequence.
static void fillMessage(MSG_TYPE &m)
{
    m.someu8 = 255;
    m.someu16 = 65535;
    m.someu32 = 4294967295u;
    m.someu64 = 18446744073709551614ull;   // the schema default IS u64 max
    m.somei16 = -32768;
    m.somei32 = -2147483647 - 1;
    m.somei64 = -9223372036854775807LL - 1;
    m.somefp32 = 2.5f;
    m.somefp64 = -1.0e300;
    m.somebool = !m.somebool;
    m.somestring = "0123456789-0123456789-0123456789-0123456789-01234";  // maxlen 50
    for (std::size_t i = 0; i < m.someintarray.size(); ++i)
    {
        m.someintarray[i] = -static_cast<std::int32_t>((i + 1) * 100000);
    }
    for (std::size_t i = 0; i < m.someuintarray.size(); ++i)
    {
        m.someuintarray[i] = static_cast<std::uint32_t>((i + 1) * 100000);
    }
    m.somestruct.nestedint = 7;
    m.somestruct.nestedstring = "nested-string-straddles";               // maxlen 32
    m.somestruct.nestedstruct.deepint = -99;

    // The array kinds whose decode destination is NOT the wire element type, and
    // the wrapper-sequence kinds. Chunked feeding is the only thing that tells
    // these apart from the native arrays above: a resumed field is delivered once
    // per chunk that carries part of it, into the destination it was handed, so
    // anything decoded through a per-delivery temporary keeps just the last
    // chunk's elements and silently drops the rest. Every array here carries a
    // schema default, so its size is already non-zero and the loops below only
    // have to give each element a value the default does not have.
    using EnumElem = std::remove_cvref_t<decltype(m.someenumarray[0])>;
    for (std::size_t i = 0; i < m.someenumarray.size(); ++i)
    {
        m.someenumarray[i] = static_cast<EnumElem>(i % 3);
    }
    for (std::size_t i = 0; i < m.someboolarray.size(); ++i)
    {
        m.someboolarray[i] = (i % 2 == 0);
    }
    for (std::size_t i = 0; i < m.somestringarray.size(); ++i)
    {
        // maxlen 16 per element, and long enough to straddle a small chunk.
        m.somestringarray[i].assign(std::string_view{"straddle-0123456"}.substr(0, 10 + i % 6));
    }
    for (std::size_t i = 0; i < m.someblobarray.size(); ++i)
    {
        // assign(initializer_list) is spelled the same on std::vector<uint8_t>
        // and on sofab::FixedBytes<N>. maxlen 8 per element.
        m.someblobarray[i].assign({std::uint8_t(i + 1), 0x7f, 0x00, 0xff});
    }
}

// Comparing the re-encoded bytes rather than the fields keeps this independent
// of the storage types, and is the stronger statement anyway: two messages that
// encode identically ARE the same message on the wire.
static bool sameMessage(const MSG_TYPE &a, const MSG_TYPE &b)
{
    return a.encode() == b.encode();
}

static void collect(std::span<const std::uint8_t> bytes)
{
    g_sink.insert(g_sink.end(), bytes.begin(), bytes.end());
}

int main()
{
    MSG_TYPE out;
    fillMessage(out);

    // ---- 1. streaming encode is byte-identical --------------------------

    const std::vector<std::uint8_t> oneShot = out.encode();
    if (oneShot.empty()) { std::printf("FAIL: message encoded to nothing\n"); return 1; }

    // 7 bytes: smaller than any single field, so the callback fires mid-value
    // and the stream has to carry its buffer state across flushes.
    g_sink.clear();
    {
        sofab::OStreamInline<7> os{collect};
        out.serialize(os);
        os.flush();
    }
    if (g_sink.size() != oneShot.size() ||
        std::memcmp(g_sink.data(), oneShot.data(), oneShot.size()) != 0)
    {
        std::printf("FAIL: streaming encode differs from encode(): %zu vs %zu bytes\n",
                    g_sink.size(), oneShot.size());
        return 1;
    }

    // ---- 2. chunked decode is value-identical ---------------------------

    MSG_TYPE expect;
    if (!MSG_TYPE::try_decode(oneShot.data(), oneShot.size(), expect).ok())
    {
        std::printf("FAIL: one-shot decode failed\n");
        return 1;
    }

    for (std::size_t size : {std::size_t{1}, std::size_t{2}, std::size_t{3},
                             std::size_t{5}, std::size_t{16}, std::size_t{64}})
    {
        sofab::IStreamObject<MSG_TYPE> in SOFAB_STREAM_ARGS;
        // Neither ok() nor incomplete() means "the message is done": the wire
        // format has no top-level end marker, so a chunk ending on a field
        // boundary reports ok() even when more follows. Feed everything; the
        // caller's framing is what decides, and the LAST verdict is the one that
        // says whether the stream ended half-read.
        //
        // The verdict has to be kept as it goes past, because this family offers
        // no way to ask afterwards: corelib-c-cpp's sofab_istream_feed asserts
        // datalen > 0, so the zero-length end-of-input probe that corelib-rs
        // documents (and corelib-cpp happens to accept) is not available here.
        bool atBoundary = false;
        for (std::size_t off = 0; off < oneShot.size(); off += size)
        {
            const std::size_t n = std::min(size, oneShot.size() - off);
            const auto r = in.feed(oneShot.data() + off, n);
            if (!r.ok() && !r.incomplete())
            {
                std::printf("FAIL: chunk size %zu: feed reported %d\n",
                            size, static_cast<int>(r.code()));
                return 1;
            }
            atBoundary = r.ok();
        }
        if (!atBoundary)
        {
            std::printf("FAIL: chunk size %zu: stream did not end at a boundary\n", size);
            return 1;
        }
        if (!sameMessage(*in, expect))
        {
            std::printf("FAIL: chunk size %zu: decoded value differs from the one-shot decode\n", size);
            return 1;
        }
    }

    // ---- 3. a cut inside a field is not a value -------------------------

    // Truncation is not automatically an error: the format has no end marker and
    // no required fields, so a message cut on a field boundary is a valid shorter
    // message. What must hold is that a cut INSIDE a field is reported as
    // incomplete rather than accepted. Both outcomes are counted, so the check
    // cannot pass by being uniformly strict or uniformly lax.
    std::size_t incompletes = 0, boundaries = 0;
    for (std::size_t cut = 1; cut < oneShot.size(); ++cut)
    {
        sofab::IStreamObject<MSG_TYPE> in SOFAB_STREAM_ARGS;
        const auto r = in.feed(oneShot.data(), cut);
        if (r.incomplete())   { ++incompletes; }
        else if (r.ok())      { ++boundaries; }
        else
        {
            std::printf("FAIL: truncating at %zu reported %d\n", cut, static_cast<int>(r.code()));
            return 1;
        }
    }
    if (incompletes == 0)
    {
        std::printf("FAIL: no truncation was incomplete -- the boundary probe never fires\n");
        return 1;
    }
    if (boundaries == 0)
    {
        std::printf("FAIL: every truncation was incomplete -- a cut on a field boundary should decode\n");
        return 1;
    }

    std::printf("streaming: %zu bytes byte-identical through a 7-byte buffer; "
                "value-identical at 6 chunk sizes; of %zu truncations %zu incomplete, %zu on a boundary\n",
                oneShot.size(), oneShot.size() - 1, incompletes, boundaries);
    return 0;
}
