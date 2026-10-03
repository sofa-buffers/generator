/*!
 * @file blob_default_check.c
 * @brief MESSAGE_SPEC section 2 for a sized blob with a non-empty default
 *        (generator#680).
 *
 * A blob left at its default is not written; an explicit EMPTY blob over a
 * non-empty default is a value and IS written, and decodes back as empty. Both
 * halves depend on the const default image carrying the blob's length beside its
 * bytes: sofab_object_init seeds the companion length from it, and the corelib's
 * default test compares length and bytes against the same image. An image with
 * no length describes the empty blob, so the default decodes as empty and the
 * explicit empty blob is omitted.
 *
 * Positions: the message, a struct, and a struct-array element; each with an
 * ordinary default ("Hello"/"Hi") and an all-zero one ("AAAA" = 3 zero bytes,
 * whose byte image is elided but whose length is not).
 *
 * SPDX-License-Identifier: MIT
 */

#include "blobdef_sofab.h"

#include <stdio.h>
#include <string.h>

static int g_failures;
static int g_cases;

static void fail(const char *what)
{
    printf("FAIL: %s\n", what);
    g_failures++;
}

static const uint8_t k_hello[] = {'H', 'e', 'l', 'l', 'o'};
static const uint8_t k_hi[] = {'H', 'i'};
static const uint8_t k_zero[] = {0, 0, 0};

/* A message at its defaults: every blob holds its declared default. */
static void expect_defaults(const message_blobdef_t *m, const char *ctx)
{
    g_cases++;
    if (m->bl__len != 5 || memcmp(m->bl, k_hello, 5) != 0) fail(ctx), puts("  bl is not \"Hello\"");
    if (m->bz__len != 3 || memcmp(m->bz, k_zero, 3) != 0) fail(ctx), puts("  bz is not 3 zero bytes");
    if (m->s.sb__len != 2 || memcmp(m->s.sb, k_hi, 2) != 0) fail(ctx), puts("  s.sb is not \"Hi\"");
    if (m->s.sz__len != 3 || memcmp(m->s.sz, k_zero, 3) != 0) fail(ctx), puts("  s.sz is not 3 zero bytes");
}

/* Encode `m`, decode into a fresh message and return the wire length. */
static size_t roundtrip(const message_blobdef_t *m, message_blobdef_t *out, const char *ctx)
{
    uint8_t wire[MESSAGE_BLOBDEF__MAX_SIZE];
    size_t n = 0;
    if (message_blobdef__encode(m, wire, sizeof wire, &n) != SOFAB_RET_OK) {
        fail(ctx);
        return 0;
    }
    message_blobdef__init(out);
    if (message_blobdef__decode(out, wire, n) != SOFAB_RET_OK) fail(ctx);
    return n;
}

int main(void)
{
    message_blobdef_t m, back;

    /* 1. A fresh message holds the defaults and encodes to nothing. */
    message_blobdef__init(&m);
    expect_defaults(&m, "fresh message");
    if (roundtrip(&m, &back, "default round-trip") != 0) fail("a message at its defaults must encode to no bytes");
    expect_defaults(&back, "empty wire decodes to the defaults");

    /* 2. An explicit empty blob over a non-empty default is written, and comes
     * back empty with every other blob still at its default. */
    for (int which = 0; which < 4; which++) {
        static const char *const names[] = {"bl", "bz", "s.sb", "s.sz"};
        char ctx[64];
        snprintf(ctx, sizeof ctx, "explicit empty %s", names[which]);
        message_blobdef__init(&m);
        switch (which) {
        case 0: m.bl__len = 0; break;
        case 1: m.bz__len = 0; break;
        case 2: m.s.sb__len = 0; break;
        default: m.s.sz__len = 0; break;
        }
        if (roundtrip(&m, &back, ctx) == 0) {
            fail(ctx);
            puts("  an empty blob over a non-empty default must be written");
            continue;
        }
        g_cases++;
        uint8_t lens[4] = {back.bl__len, back.bz__len, back.s.sb__len, back.s.sz__len};
        uint8_t want[4] = {5, 3, 2, 3};
        want[which] = 0;
        if (memcmp(lens, want, 4) != 0) fail(ctx), puts("  decoded lengths differ from expected");
    }

    /* 3. The same inside a struct-array element. */
    message_blobdef__init(&m);
    m.sa.len = 1;
    if (m.sa.items[0].ab__len != 2 || memcmp(m.sa.items[0].ab, k_hi, 2) != 0)
        fail("a struct-array element's blob must start at its default"), puts("  sa[0].ab is not \"Hi\"");
    m.sa.items[0].ab__len = 0;
    if (roundtrip(&m, &back, "explicit empty sa[0].ab") == 0)
        fail("an empty element blob over a non-empty default must be written");
    g_cases++;
    if (back.sa.len != 1 || back.sa.items[0].ab__len != 0)
        fail("sa[0].ab must decode back as empty");

    if (g_failures) return 1;
    printf("blob defaults (generator#680): %d cases -- the default image carries each blob's length\n", g_cases);
    return 0;
}
