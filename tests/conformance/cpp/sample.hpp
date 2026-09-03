// The sample message the C++ in-process checks share, plus the one profile
// switch they both need.
//
// streaming_check.cpp and ownership_check.cpp assert different properties over
// the SAME message, and the fill is the part that must not drift between them:
// an array kind nothing fills is an array kind nothing tests, and that lesson
// has been learned twice in this suite already (someenumarray/someboolarray,
// then somematrix). One fill, included by both.
//
// Everything here is profile-independent by construction: only operations that
// mean the same thing whether a field is a std::string or a sofab::FixedString<N>,
// a std::vector or a sofab::InlineVector. The message header and MSG_TYPE are
// supplied by run.sh via -include/-D, which knows the profile.

#ifndef SOFAB_CONFORMANCE_CPP_SAMPLE_HPP
#define SOFAB_CONFORMANCE_CPP_SAMPLE_HPP

#include <cstddef>
#include <cstdint>
#include <string_view>
#include <type_traits>

// corelib-cpp's IStreamObject takes a sofab::Limits -- the byte budget for one
// top-level field -- and offers no constructor that leaves it out (corelib-cpp#128;
// CORELIB_PLAN §6.2.1: a codec supplies no default for a number it was not given).
// The corelib-c-cpp one takes none, its containers being statically bounded, and
// does not declare the type at all, so the choice cannot be a `requires` test and
// run.sh passes it as a -D. Neither check is about caps, so the pure leg states
// the platform ceiling -- a number stated, not a mode.
#ifdef SOFAB_STREAM_LIMITS
#define SOFAB_STREAM_ARGS {sofab::Limits{SIZE_MAX}}
#else
#define SOFAB_STREAM_ARGS
#endif

// Populate the shapes that matter for chunked feeding and for ownership:
// scalars at their widest varint, strings (which is what the reassembly buffer
// exists for), a native array (index assignment works in every storage mode), a
// nested sequence, and both wrapper-array kinds.
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

    // The SCALAR blob. It carries a schema default, so leaving it alone omits it
    // from the wire entirely (MESSAGE_SPEC §2 sparse omission) and the whole
    // scalar-blob destination goes untested -- which is what happened until
    // generator#412: blob delivery was reached only through someblobarray.
    // Same spelling on std::vector<std::uint8_t> and on sofab::FixedBytes<N>.
    m.someblob.assign({std::uint8_t(0x01), 0x7f, 0x00, 0xff, 0x2a});

    // The remaining payload arms that are their own generated destination
    // rather than another use of the same helper: a string inside a union
    // inside a wrapper array, a struct-with-array's own label, and the string
    // key of a dynamic map row. resize + index works on std::vector and on
    // sofab::InlineVector<T, N> alike; both start EMPTY here, so a loop over
    // size() would fill nothing.
    m.someunionarray.resize(1);                                          // count 2
    m.someunionarray[0].asstring.assign(std::string_view{"union-row-str"});  // maxlen 16
    m.somestructwitharray.label.assign(std::string_view{"struct-label"});    // maxlen 16
    m.somemap.resize(2);                                                 // dynamic
    m.somemap[0].key.assign(std::string_view{"first-key-straddles"});    // maxlen 32
    m.somemap[0].value = 1;
    m.somemap[1].key.assign(std::string_view{"second-key-straddles"});
    m.somemap[1].value = 2;
}

#endif // SOFAB_CONFORMANCE_CPP_SAMPLE_HPP
