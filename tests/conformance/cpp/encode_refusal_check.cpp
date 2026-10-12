// A refused encode is distinguishable from the all-default message, in every
// profile (generator#736).
//
// Both produce zero bytes. An all-default message encodes to nothing and that is
// a legal, round-trippable value (MESSAGE_SPEC §2) -- any caller that decodes
// and re-encodes meets it. A refused encode produces nothing because it never
// completed: since generator#656 a value past its own declared bound is refused
// with sofab::Error::InvalidArgument, and that is reachable from an ordinary
// setter, not just from a caller mistake. encode() used to answer both with an
// empty vector and encodeTo() both with 0, so the only surface that could tell
// them apart was serialize() on a stream the caller built itself -- while the
// doc comment pointed at encodeTo(), which could not. CORELIB_PLAN §5.1.2 names
// that shape non-conformant: "an encoder that could not write what it was asked
// to write reports it, and a one-shot helper that ignores that report is
// non-conformant."
//
// So the verdict is the return value and the bytes are the out-parameter, and
// the property asserted here is the one the collision hid: same bytes,
// different status.
//
// Profiles. Refusing needs storage that can HOLD an over-bound value, which is
// the dynamic mode (std::string). The static profiles clamp on assignment by
// contract (FixedString<N>, decision of 2026-10-07), so there is nothing
// over-bound left to refuse by the time encode runs; run.sh passes
// -DSOFAB_ENCREF_CLAMPS for those and this check asserts the clamp instead. The
// all-default, at-bound, buffer-full and byte-agreement halves run on all four.
//
// MSG_TYPE and the message header come from run.sh via -D/-include.

#include <cstdint>
#include <cstdio>
#include <cstring>
#include <string>
#include <vector>

static std::size_t g_failures = 0;

static const char *errName(sofab::Error e)
{
    switch (e)
    {
        case sofab::Error::None:            return "None";
        case sofab::Error::BufferFull:      return "BufferFull";
        case sofab::Error::InvalidArgument: return "InvalidArgument";
        case sofab::Error::InvalidMessage:  return "InvalidMessage";
        case sofab::Error::Incomplete:      return "Incomplete";
        case sofab::Error::LimitExceeded:   return "LimitExceeded";
    }
    return "?";
}

static void mustBe(const char *what, sofab::Error got, sofab::Error want)
{
    if (got != want)
    {
        std::printf("FAIL: %s: expected sofab::Error::%s, got %s\n", what, errName(want), errName(got));
        ++g_failures;
    }
}

static void mustHold(const char *what, bool cond)
{
    if (!cond)
    {
        std::printf("FAIL: %s\n", what);
        ++g_failures;
    }
}

int main()
{
    // ---- 1. the all-default message: a success that writes no bytes ------
    MSG_TYPE dflt;
    std::vector<std::uint8_t> none;
    const sofab::Error dfltStatus = dflt.encode(none);
    mustBe("the all-default message encodes", dfltStatus, sofab::Error::None);
    mustHold("the all-default message must encode to zero bytes (MESSAGE_SPEC §2)", none.empty());

    // ---- 2. a value AT its declared bound still encodes -------------------
    MSG_TYPE atBound;
    atBound.s = "abcd";  // maxlen 4, exactly
    std::vector<std::uint8_t> bytes;
    mustBe("a value at its bound encodes", atBound.encode(bytes), sofab::Error::None);
    mustHold("a value at its bound must produce bytes", !bytes.empty());

    // ---- 3. a value PAST its declared bound ------------------------------
    MSG_TYPE over;
    over.s = "xxxxxxxxxx";  // 10 bytes, maxlen 4
#ifdef SOFAB_ENCREF_CLAMPS
    // The static profiles never see an over-bound value: the container cut it on
    // assignment, and what encode writes is the clamped value.
    mustHold("static storage must clamp an over-long assignment to its maxlen", over.s.size() == 4);
    std::vector<std::uint8_t> clamped;
    mustBe("a clamped value encodes", over.encode(clamped), sofab::Error::None);
    mustHold("a clamped value must produce bytes", !clamped.empty());
#else
    // Pre-filled, so an empty result proves the destination was CLEARED rather
    // than merely never written.
    std::vector<std::uint8_t> refused(32, 0xee);
    const sofab::Error overStatus = over.encode(refused);
    mustBe("a value past its bound is refused", overStatus, sofab::Error::InvalidArgument);
    mustHold("a refused encode must leave the destination empty, never partially written",
             refused.empty());

    // The collision itself: the bytes of a refusal and of the all-default
    // message are indistinguishable, so the status is what has to differ.
    mustHold("a refusal and the all-default message must differ in STATUS, not in bytes",
             refused.size() == none.size() && overStatus != dfltStatus);
#endif

    // ---- 4. encodeTo() reports the same verdicts -------------------------
    std::uint8_t buf[MSG_TYPE::_maxSize];
    std::size_t used = 12345;  // must be overwritten, whatever the outcome
    mustBe("the all-default message encodes into a caller buffer",
           dflt.encodeTo(buf, sizeof(buf), used), sofab::Error::None);
    mustHold("the all-default message must write 0 bytes into a caller buffer", used == 0);

    used = 12345;
    mustBe("a value at its bound encodes into a caller buffer",
           atBound.encodeTo(buf, sizeof(buf), used), sofab::Error::None);
    mustHold("encodeTo() must write exactly what encode() produced",
             used == bytes.size() && std::memcmp(buf, bytes.data(), used) == 0);

#ifndef SOFAB_ENCREF_CLAMPS
    used = 12345;
    mustBe("a value past its bound is refused by encodeTo() too",
           over.encodeTo(buf, sizeof(buf), used), sofab::Error::InvalidArgument);
    mustHold("a refused encodeTo() must report no bytes written", used == 0);
#endif

    // ---- 5. the destination is the caller's, and is emptied first --------
    // Documented contract: @p _out is emptied before the writes start and may be
    // reused across messages (which is what lets a caller keep one buffer). So a
    // vector still holding an older message must come back holding exactly the
    // new one -- and exactly nothing when the new one is all-default, the case
    // that would read as "unchanged" if the destination were only appended to.
    std::vector<std::uint8_t> reused(64, 0xee);
    mustBe("a reused destination encodes", atBound.encode(reused), sofab::Error::None);
    mustHold("a reused destination must hold exactly the new message", reused == bytes);
    mustBe("the all-default message encodes into a reused destination",
           dflt.encode(reused), sofab::Error::None);
    mustHold("the all-default message must EMPTY a reused destination", reused.empty());

    // ---- 6. a buffer too small is NOT a refusal --------------------------
    // The two failures a caller acts on differently: raise the buffer, or fix
    // the value. Folding them into one answer is what this check exists to stop,
    // so they are asserted distinct (CORELIB_PLAN §5.1: a buffer installed
    // without a sink "either holds the message or reports buffer-full").
    used = 12345;
    mustBe("a buffer too small reports BufferFull",
           atBound.encodeTo(buf, 1, used), sofab::Error::BufferFull);
    mustHold("an overflowed encodeTo() must report no bytes written", used == 0);

    // A buffer installed without a sink is subject to no minimum (CORELIB_PLAN
    // §5.1): it either holds the message or reports buffer-full, and it "stays
    // exact: a two-byte message encodes into a two-byte buffer on any port". The
    // all-default message is the zero-byte one, so a zero-length window holds it.
    used = 12345;
    mustBe("the all-default message fits a zero-length window",
           dflt.encodeTo(buf, 0, used), sofab::Error::None);
    mustHold("a zero-length window must report 0 bytes written", used == 0);

    if (g_failures != 0)
    {
        std::printf("FAIL: %zu encode-refusal check(s) failed\n", g_failures);
        return 1;
    }
    std::printf("ok: a refused encode is distinguishable from the all-default message\n");
    return 0;
}
