// The one-shot encode, for the in-process C++ checks that only ever encode a
// fixture they built themselves.
//
// encode() returns a sofab::Error and takes its destination as an
// out-parameter (generator#736): "no bytes" cannot carry a verdict, because an
// all-default message encodes to nothing and that is a legal value
// (MESSAGE_SPEC §2), while a refused encode produces nothing because it never
// completed. CORELIB_PLAN §5.1.2 -- "a one-shot helper that ignores that report
// is non-conformant" -- applies to a test harness as much as to generated code:
// every fixture these checks encode is in-bounds, so a refusal is a bug in the
// check itself and must be loud rather than silently compared as an empty
// vector.
//
// The property that a refusal IS distinguishable is asserted next door, in
// encode_refusal_check.cpp; this header is only the caller side of it.

#ifndef SOFAB_CONFORMANCE_CPP_ENCODED_HPP
#define SOFAB_CONFORMANCE_CPP_ENCODED_HPP

#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <vector>

template <typename M>
inline std::vector<std::uint8_t> encoded(const M &m)
{
    std::vector<std::uint8_t> out;
    const sofab::Error e = m.encode(out);
    if (e != sofab::Error::None)
    {
        std::printf("FAIL: encode refused with sofab::Error %d -- the fixture is out of bounds\n",
                    static_cast<int>(e));
        std::exit(2);
    }
    return out;
}

#endif  // SOFAB_CONFORMANCE_CPP_ENCODED_HPP
