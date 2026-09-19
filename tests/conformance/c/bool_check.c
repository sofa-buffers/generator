/*!
 * @file bool_check.c
 * @brief CORELIB_PLAN §4.4 on the decode side of a generated C message
 *        (generator#581).
 *
 * A boolean is canonical on encode and tolerant on decode: every value other
 * than 0 reads as true, is normalized away (the member holds 1, and a re-encode
 * emits 1), and carries no width bound -- 256 and 2^64-1 are true, not INVALID.
 * The C member is a uint8_t, so the harness JSON cannot see the rule. This check
 * inspects the stored bytes and the re-encoded wire instead.
 *
 * The defect it pins: described as UNSIGNED / ARRAY_UNSIGNED, a boolean was
 * read as a one-byte integer, so 2 was stored raw and re-encoded as 02, and 256
 * was rejected as INVALID by the width check. Only SOFAB_OBJECT_FIELDTYPE_BOOLEAN
 * and _ARRAY_BOOLEAN (corelib-c-cpp#172) apply the boolean rule.
 *
 * Every case runs twice: one-shot, and fed one byte at a time, so each element
 * varint is split across feeds. Positions: a scalar, an array, a nested-array
 * row, an array inside a struct, a union arm and a depth-3 row.
 *
 * SPDX-License-Identifier: MIT
 */

#include "boolchk.h"

#include <stdio.h>
#include <string.h>

/* The varints under test: 2 (non-zero, fits a byte), 256 (does not fit a byte)
 * and 2^64-1 (the widest varint there is). */
#define V2 0x02
#define V256 0x80, 0x02
#define VMAX 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01

/* Header bytes: (id << 3) | wire type, with 0 = unsigned, 3 = unsigned array,
 * 6 = sequence start and 7 = sequence end. */

/* Only values that fit a byte: a one-byte integer read accepts these, so this
 * case isolates the normalization from the width bound. */
static const uint8_t k_small[] = {
    0x00, V2,                                   /* flag = 2 */
    0x0b, 0x03, V2, 0x00, 0x05,                 /* flags = [2, 0, 5] */
};

static const uint8_t k_wide[] = {
    0x00, VMAX,                                 /* flag = 2^64-1 */
    0x0b, 0x05, V2, 0x01, 0x00, V256, VMAX,     /* flags = [2, 1, 0, 256, max] */
    0x16,                                       /* rows: sequence start */
    0x03, 0x02, V2, V256,                       /*   row 0 = [2, 256] */
    0x0b, 0x03, VMAX, 0x00, 0x05,               /*   row 1 = [max, 0, 5] */
    0x07,                                       /* rows: sequence end */
    0x1e,                                       /* nested: sequence start */
    0x03, 0x03, 0x00, V256, 0x05,               /*   inner = [0, 256, 5] */
    0x07,                                       /* nested: sequence end */
    0x26,                                       /* choice: sequence start */
    0x03, 0x03, V256, 0x00, V2,                 /*   bits = [256, 0, 2] */
    0x07,                                       /* choice: sequence end */
    0x2e,                                       /* cube: sequence start */
    0x06,                                       /*   row 0: sequence start */
    0x03, 0x02, V2, VMAX,                       /*     [2, max] */
    0x0b, 0x01, 0x00,                           /*     [0] */
    0x07,                                       /*   row 0: sequence end */
    0x0e,                                       /*   row 1: sequence start */
    0x03, 0x01, V256,                           /*     [256] */
    0x07,                                       /*   row 1: sequence end */
    0x07,                                       /* cube: sequence end */
};

static int g_failures;

static void fail(const char *surface, const char *what)
{
    printf("FAIL: [%s] %s\n", surface, what);
    g_failures++;
}

static int holds(const uint8_t *v, size_t len, const uint8_t *want, size_t wantlen)
{
    return len == wantlen && memcmp(v, want, len) == 0;
}

static int decode(message_boolchk_t *m, const uint8_t *wire, size_t len, int drip)
{
    message_boolchk_init(m);
    if (!drip) {
        return message_boolchk_decode(m, wire, len) == SOFAB_RET_OK;
    }
    message_boolchk_decoder_t d;
    message_boolchk_decoder_init(&d, m);
    sofab_ret_t r = SOFAB_RET_OK;
    for (size_t i = 0; i < len; i++) {
        r = message_boolchk_decoder_feed(&d, wire + i, 1);
        if (r != SOFAB_RET_OK && r != SOFAB_RET_INCOMPLETE) {
            return 0;
        }
    }
    return r == SOFAB_RET_OK;
}

static int same_wire(const message_boolchk_t *a, const message_boolchk_t *b)
{
    uint8_t wa[MESSAGE_BOOLCHK_MAX_SIZE], wb[MESSAGE_BOOLCHK_MAX_SIZE];
    size_t na = 0, nb = 0;
    if (message_boolchk_encode(a, wa, sizeof wa, &na) != SOFAB_RET_OK) return 0;
    if (message_boolchk_encode(b, wb, sizeof wb, &nb) != SOFAB_RET_OK) return 0;
    return na == nb && memcmp(wa, wb, na) == 0;
}

int main(void)
{
    /* The same messages written from normalized values: what a re-encode of the
     * tolerant input must reproduce byte for byte. */
    message_boolchk_t want_small, want_wide;
    static const uint8_t small_flags[] = {1, 0, 1};
    static const uint8_t wide_flags[] = {1, 1, 0, 1, 1};
    static const uint8_t row0[] = {1, 1}, row1[] = {1, 0, 1}, inner[] = {0, 1, 1};
    static const uint8_t bits[] = {1, 0, 1}, cube00[] = {1, 1}, cube01[] = {0}, cube10[] = {1};

    message_boolchk_init(&want_small);
    want_small.flag = 1;
    want_small.flags_len = 3;
    memcpy(want_small.flags, small_flags, 3);

    message_boolchk_init(&want_wide);
    want_wide.flag = 1;
    want_wide.flags_len = 5;
    memcpy(want_wide.flags, wide_flags, 5);
    want_wide.rows.len = 2;
    want_wide.rows.items[0].len = 2;
    memcpy(want_wide.rows.items[0].vals, row0, 2);
    want_wide.rows.items[1].len = 3;
    memcpy(want_wide.rows.items[1].vals, row1, 3);
    want_wide.nested.inner_len = 3;
    memcpy(want_wide.nested.inner, inner, 3);
    want_wide.choice.bits_len = 3;
    memcpy(want_wide.choice.bits, bits, 3);
    want_wide.cube.len = 2;
    want_wide.cube.items[0].len = 2;
    want_wide.cube.items[0].items[0].len = 2;
    memcpy(want_wide.cube.items[0].items[0].vals, cube00, 2);
    want_wide.cube.items[0].items[1].len = 1;
    memcpy(want_wide.cube.items[0].items[1].vals, cube01, 1);
    want_wide.cube.items[1].len = 1;
    want_wide.cube.items[1].items[0].len = 1;
    memcpy(want_wide.cube.items[1].items[0].vals, cube10, 1);

    for (int drip = 0; drip <= 1; drip++) {
        const char *surface = drip ? "one byte per feed" : "one-shot";
        message_boolchk_t m;

        if (!decode(&m, k_small, sizeof k_small, drip)) {
            fail(surface, "flag = 2, flags [2, 0, 5] must decode");
        } else {
            if (m.flag != 1) fail(surface, "a scalar boolean of 2 must be stored as 1");
            if (!holds(m.flags, m.flags_len, small_flags, 3))
                fail(surface, "flags [2, 0, 5] must be stored as [1, 0, 1]");
            if (!same_wire(&m, &want_small))
                fail(surface, "flag = 2, flags [2, 0, 5] must re-encode as 1 / [1, 0, 1]");
        }

        if (!decode(&m, k_wide, sizeof k_wide, drip)) {
            fail(surface, "booleans holding 256 and 2^64-1 must decode (true, not INVALID)");
            continue;
        }
        if (m.flag != 1) fail(surface, "a scalar boolean of 2^64-1 must be stored as 1");
        if (!holds(m.flags, m.flags_len, wide_flags, 5))
            fail(surface, "flags [2, 1, 0, 256, max] must be stored as [1, 1, 0, 1, 1]");
        if (m.rows.len != 2 ||
            !holds(m.rows.items[0].vals, m.rows.items[0].len, row0, 2) ||
            !holds(m.rows.items[1].vals, m.rows.items[1].len, row1, 3))
            fail(surface, "rows [[2, 256], [max, 0, 5]] must be stored as [[1, 1], [1, 0, 1]]");
        if (!holds(m.nested.inner, m.nested.inner_len, inner, 3))
            fail(surface, "nested.inner [0, 256, 5] must be stored as [0, 1, 1]");
        if (!holds(m.choice.bits, m.choice.bits_len, bits, 3))
            fail(surface, "choice.bits [256, 0, 2] must be stored as [1, 0, 1]");
        if (m.cube.len != 2 || m.cube.items[0].len != 2 || m.cube.items[1].len != 1 ||
            !holds(m.cube.items[0].items[0].vals, m.cube.items[0].items[0].len, cube00, 2) ||
            !holds(m.cube.items[0].items[1].vals, m.cube.items[0].items[1].len, cube01, 1) ||
            !holds(m.cube.items[1].items[0].vals, m.cube.items[1].items[0].len, cube10, 1))
            fail(surface, "cube [[[2, max], [0]], [[256]]] must be stored as [[[1, 1], [0]], [[1]]]");
        if (!same_wire(&m, &want_wide))
            fail(surface, "the decoded booleans must re-encode with every true value as 1");
    }

    if (g_failures) return 1;
    printf("booleans (§4.4): 2 cases, each one-shot and one byte per feed -- every "
           "non-zero value stored and re-encoded as 1\n");
    return 0;
}
