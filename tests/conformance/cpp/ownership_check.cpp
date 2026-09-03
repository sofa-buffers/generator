// A decoded message OWNS its bytes (CORELIB_PLAN §6.7 / §6.7.1, generator#412),
// in every C++ profile.
//
// The rule: no value the codec delivers may outlive the callback it arrived in.
// §6.0 fixes that for `feed` -- a chunk is borrowed only for the duration of the
// call, so once it returns the caller may reuse, overwrite or FREE that memory
// and the decoded message must not be affected -- and §6.7.1 gives the one-shot
// path no exemption: `decode(buffer)` copies too, because a message whose
// lifetime depends on which entry point produced it is the divergence §6.7 ends.
//
// The oracle is DESTRUCTIVE, not comparative: encode a sample, decode it out of
// heap storage this program controls, destroy that storage, re-encode and diff.
// streaming_check.cpp next door cannot reach it -- every chunk it feeds points
// into a vector that stays alive and unmodified for the whole run, and a
// destination holding a window into one of those reads back perfectly.
//
// There is ONE decode surface here: generated `decode(data, len)` is an
// `IStreamObject` plus a single `feed`. So the axis is not the entry point, it
// is CHUNK SIZE -- a payload SPLIT across chunks is reassembled inside the
// corelib and copied out of its own storage whether or not anything wanted to
// alias, so small chunks alone never reach the whole-payload delivery path. The
// sweep ends at a size that carries the entire message.
//
// KNOWN REACH -- do not read a pass as "every field is copied":
//
//   * No generated C++ destination CAN alias, in any profile. The pure legs
//     store std::string / std::vector<std::uint8_t>, the c-cpp legs
//     sofab::FixedString<N> / FixedBytes<N> / InlineVector<T,N>; all own their
//     bytes, and corelib-cpp static_asserts a std::string_view destination away
//     with a message that cites §6 ("`feed` is the only way in ... there is no
//     configuration in which the view is safe"). The mutation issue #412 asks
//     for -- change a destination to keep a view -- is a COMPILE error here, not
//     a test failure. This leg is therefore a CORELIB regression net: what it
//     catches is a codec that starts deferring the copy, holding a pointer into
//     a fed chunk and reading it back later.
//   * That is why it is built with -fsanitize=address, the corelib's own C
//     sources included for the c-cpp profiles, and why every chunk is a separate
//     heap block freed the instant `feed` returns. Without ASan the value
//     comparison can print a pass while the message holds a dangling pointer:
//     freed heap usually still reads back the bytes that were in it.
//   * The scribble byte is 0x41 ('A'), not 0xff, for the reason the family
//     settled on: an aliased string destination must still RE-ENCODE, so the
//     oracle stays a byte comparison instead of becoming an encoder error that
//     unrelated causes could produce.
//
// The message header and MSG_TYPE are supplied by run.sh via -include/-D, which
// knows the profile; the fill is shared with streaming_check.cpp so the two
// cannot drift on which field kinds are populated.

#include <cstdint>
#include <cstdio>
#include <cstring>
#include <vector>

#include "sample.hpp"

// 'A': see the header note. An aliased string destination must still encode.
static constexpr std::uint8_t kScribble = 0x41;

static std::size_t g_failures = 0;

static void hexdump(const char *label, const std::vector<std::uint8_t> &b)
{
    std::printf("  %s ", label);
    for (const std::uint8_t x : b) { std::printf("%02x", x); }
    std::printf("\n");
}

// Re-encode and diff. Comparing bytes rather than fields keeps this independent
// of the storage types, and is the stronger statement anyway: two messages that
// encode identically ARE the same message on the wire.
static void mustMatch(const char *what,
                      const std::vector<std::uint8_t> &want,
                      const MSG_TYPE &got)
{
    const std::vector<std::uint8_t> re = got.encode();
    if (re != want)
    {
        std::printf("FAIL: %s: a decoded field aliased the storage it was decoded from\n", what);
        hexdump("want", want);
        hexdump("got ", re);
        ++g_failures;
    }
}

int main()
{
    MSG_TYPE sample;
    fillMessage(sample);

    const std::vector<std::uint8_t> want = sample.encode();
    if (want.empty()) { std::printf("FAIL: the sample encoded to nothing\n"); return 2; }

    // ---- 1. one-shot, out of heap storage that is then destroyed ---------
    // §6.7.1: `data` may be reused, overwritten or freed the moment decode
    // returns, so doing exactly that is a legitimate caller.
    {
        auto *buf = new std::uint8_t[want.size()];
        std::memcpy(buf, want.data(), want.size());

        MSG_TYPE got;
        // try_decode, not decode: decode is documented best-effort and never
        // reports failure, so a silently failed decode would leave an all-default
        // message that re-encodes identically twice and pass this vacuously.
        if (!MSG_TYPE::try_decode(buf, want.size(), got).ok())
        {
            std::printf("FAIL: one-shot try_decode failed\n");
            delete[] buf;
            return 1;
        }
        std::memset(buf, kScribble, want.size());
        delete[] buf;
        mustMatch("one-shot try_decode", want, got);
    }

    // ---- 2. streaming, one heap block per chunk, freed on return ---------
    // §6.0: the borrow ends when feed returns. The sweep ends at a size that
    // carries the whole message, the only one guaranteed to deliver every
    // payload inside a single chunk.
    for (const std::size_t size : {std::size_t{1}, std::size_t{2}, std::size_t{3},
                                   std::size_t{7}, std::size_t{16}, want.size()})
    {
        sofab::IStreamObject<MSG_TYPE> in SOFAB_STREAM_ARGS;
        bool atBoundary = false;
        bool broke = false;
        for (std::size_t off = 0; off < want.size(); off += size)
        {
            const std::size_t n = std::min(size, want.size() - off);
            // corelib-c-cpp's sofab_istream_feed asserts datalen > 0; n is never 0.
            auto *chunk = new std::uint8_t[n];
            std::memcpy(chunk, want.data() + off, n);
            const auto r = in.feed(chunk, n);
            std::memset(chunk, kScribble, n);
            delete[] chunk;
            if (!r.ok() && !r.incomplete())
            {
                std::printf("FAIL: chunk size %zu: feed at %zu reported %d\n",
                            size, off, static_cast<int>(r.code()));
                ++g_failures;
                broke = true;
                break;
            }
            atBoundary = r.ok();
        }
        if (broke) { continue; }
        // The last verdict is the one that says whether the stream ended
        // half-read; this family offers no way to ask afterwards.
        if (!atBoundary)
        {
            std::printf("FAIL: chunk size %zu: stream did not end at a field boundary\n", size);
            ++g_failures;
            continue;
        }
        char what[64];
        std::snprintf(what, sizeof what, "streaming feed(chunk=%zu)", size);
        mustMatch(what, want, *in);
    }

    if (g_failures > 0) { return 1; }
    std::printf("ownership: %zu bytes, decoded message owns them after the input was "
                "scribbled and freed -- one-shot + 6 chunk sizes\n", want.size());
    return 0;
}
