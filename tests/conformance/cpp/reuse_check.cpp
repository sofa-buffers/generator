// What reuse keeps, against the real corelib (generator#745).
//
// try_decode() resets the destination and decodes into it, and the reset()
// doc states exactly what that keeps: a container's OWN buffer, and not its
// elements. This leg holds the generated code to the half it promises.
//
//   KEPT. A container is cleared in place, never replaced. After the first
//   message has grown `name`, `counts` and `warnings`, every further decode
//   into the same destination finds the same buffers at the same addresses
//   with the same capacity -- so a message carrying only scalar containers
//   costs NO allocation at all. A reset() written as `out = Msg{}` would hand
//   all three back and take them again, which is what this nets.
//
//   NOT KEPT. Clearing destroys the ELEMENTS, so each `warnings` string over
//   the SSO threshold is freed and allocated again, once per message. That is
//   a corelib-side property -- StringSeq::prepare() clears the same container
//   from the collector side, so the generator cannot keep the elements on its
//   own -- and it is asserted as an UPPER BOUND, one allocation per element:
//   a corelib that learns to pool its elements makes this leg pass by more,
//   never fail.
//
// The counter is a global operator new/delete pair, armed only around the
// measured decode so neither setup nor reporting is counted.
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

namespace {
long g_alloc = 0, g_free = 0;
bool g_on = false;
int g_fail = 0;
} // namespace

void *operator new(std::size_t n) {
    if (g_on) { ++g_alloc; }
    void *p = std::malloc(n ? n : 1);
    if (!p) { std::abort(); }
    return p;
}
void operator delete(void *p) noexcept {
    if (!p) { return; }
    if (g_on) { ++g_free; }
    std::free(p);
}
void operator delete(void *p, std::size_t) noexcept { operator delete(p); }
void *operator new[](std::size_t n) { return operator new(n); }
void operator delete[](void *p) noexcept { operator delete(p); }
void operator delete[](void *p, std::size_t) noexcept { operator delete(p); }

namespace {

void check(bool ok, const char *what) {
    if (!ok) {
        std::printf("    FAIL %s\n", what);
        g_fail = 1;
    }
}

// Long enough that neither the string nor the vector can hold it inline.
const char *kLong = "a value well past the small-string threshold, 56 bytes..";

} // namespace

int main() {
    MSG_TYPE src;
    src.name = kLong;
    src.counts = {1, 2, 3, 4, 5, 6, 7, 8};
    for (int i = 0; i < 4; i++) {
        std::string w = kLong;
        w[0] = static_cast<char>('A' + i);
        src.warnings.push_back(w);
    }
    std::vector<std::uint8_t> wire;
    if (src.encode(wire) != sofab::Error::None) {
        std::printf("    FAIL encode\n");
        return 1;
    }

    // The same message without the string array: nothing in it owns element
    // storage, so a warmed destination must take no allocation at all.
    MSG_TYPE scalarSrc;
    scalarSrc.name = kLong;
    scalarSrc.counts = {1, 2, 3, 4, 5, 6, 7, 8};
    std::vector<std::uint8_t> scalarWire;
    if (scalarSrc.encode(scalarWire) != sofab::Error::None) {
        std::printf("    FAIL encode (scalar)\n");
        return 1;
    }

    // --- containers are cleared, not reallocated -----------------------------
    MSG_TYPE out;
    for (int i = 0; i < 3; i++) {
        (void)MSG_TYPE::try_decode(wire.data(), wire.size(), out);
    }
    const void *nameBuf = out.name.data();
    const std::size_t nameCap = out.name.capacity();
    const void *countsBuf = out.counts.data();
    const std::size_t countsCap = out.counts.capacity();
    const void *warnBuf = out.warnings.data();
    const std::size_t warnCap = out.warnings.capacity();

    g_alloc = 0; g_free = 0; g_on = true;
    (void)MSG_TYPE::try_decode(wire.data(), wire.size(), out);
    g_on = false;
    const long elemAllocs = g_alloc, elemFrees = g_free;

    check(out.name.data() == nameBuf && out.name.capacity() == nameCap,
          "a string field was reallocated across a reuse decode");
    check(out.counts.data() == countsBuf && out.counts.capacity() == countsCap,
          "a scalar array was reallocated across a reuse decode");
    check(out.warnings.data() == warnBuf && out.warnings.capacity() == warnCap,
          "a string array's own buffer was reallocated across a reuse decode");
    check(out.warnings.size() == 4 && out.warnings[3] == src.warnings[3],
          "the reused destination does not hold the message");

    // One pair per heap-owning element is what the doc describes; fewer is an
    // improvement, more means something other than the elements is churning.
    check(elemAllocs <= 4 && elemFrees <= 4,
          "a reuse decode allocates more than one block per heap-owning element");

    // --- a message with no heap-owning element costs nothing -----------------
    MSG_TYPE scalarOut;
    for (int i = 0; i < 3; i++) {
        (void)MSG_TYPE::try_decode(scalarWire.data(), scalarWire.size(), scalarOut);
    }
    g_alloc = 0; g_free = 0; g_on = true;
    (void)MSG_TYPE::try_decode(scalarWire.data(), scalarWire.size(), scalarOut);
    g_on = false;
    check(g_alloc == 0 && g_free == 0,
          "a reuse decode of scalar containers is not allocation-free");
    check(scalarOut.name == src.name && scalarOut.counts.size() == 8,
          "the scalar destination does not hold the message");

    std::printf("    reuse: %ld alloc / %ld free per message with 4 heap-owning"
                " elements, %ld / %ld with none\n",
                elemAllocs, elemFrees, g_alloc, g_free);
    return g_fail;
}
