/*!
 * @file maxsize_fill.c
 * @brief MAX_SIZE fill check: the computed worst case must be REACHED and never
 *        EXCEEDED by a real encode.
 *
 * The worst-case size constant every target emits is computed analytically from
 * the schema (internal/ir/wiresize.go). An analytic bound is only worth having
 * if a real encoder can confirm it, and there are two ways it can be wrong:
 *
 *   - too small  -> a legal message does not fit its own buffer (a real defect;
 *                   the generated encode() sizes its buffer from this constant)
 *   - too large  -> every fixed-buffer target wastes that many bytes of RAM,
 *                   silently, on the profiles that can least afford it
 *
 * So this fills every field of maxsize_fill.yaml to its declared bound, with
 * every varint at its widest value, and requires the encoded length to equal
 * MAX_SIZE exactly. Written against the C target because it is the one with no
 * dynamic storage anywhere: what is measured here is the wire, not a container.
 *
 * A 2-byte discrepancy found on the first run of this check was real — a surplus
 * framing byte charged per wrapper array, present in all seven per-backend cost
 * models that the shared walk replaced.
 *
 * SPDX-License-Identifier: MIT
 */

#include "fill.h"

#include <stdio.h>
#include <string.h>

int main(void)
{
    message_fill_t m;
    message_fill_init(&m);

    /* Every varint at its widest encoding: an unsigned field at its type's
     * maximum, a signed one at its most negative value (ZigZag maps INT_MIN to
     * UINT_MAX, the widest form). Nothing may be left on its default — a field
     * equal to its default is omitted from the wire (MESSAGE_SPEC §2) and would
     * make this "full" message shorter than the worst case. */
    m.f_bool = 1;
    m.f_u8   = UINT8_MAX;
    m.f_u16  = UINT16_MAX;
    m.f_u32  = UINT32_MAX;
    m.f_u64  = UINT64_MAX;
    m.f_i32  = INT32_MIN;
    m.f_i64  = INT64_MIN;
    m.f_fp32 = 1.0f;
    m.f_fp64 = 1.0;

    memcpy(m.f_str, "123456789", 9);   /* maxlen 9, no NUL on the wire */
    m.f_blob_len = 7;
    memset(m.f_blob, 0xAB, 7);

    /* An array's LENGTH is what reaches the wire (MESSAGE_SPEC §3/§5.1) and
     * `count` is only its capacity, so a full message has to say it is full: the
     * companion length member of a compact array, and the element count of a
     * length-carrying wrapper holder, are both set to the capacity. Leaving them
     * at 0 encodes the empty array and makes this "full" message short. */
    m.f_arr_u32_len  = 5;
    m.f_arr_u64_len  = 3;
    m.f_arr_fp32_len = 4;
    m.f_arr_fp64_len = 2;
    m.f_arr_str.len  = 3;
    for (int i = 0; i < 5; i++) { m.f_arr_u32[i]  = UINT32_MAX; }
    for (int i = 0; i < 3; i++) { m.f_arr_u64[i]  = UINT64_MAX; }
    for (int i = 0; i < 4; i++) { m.f_arr_fp32[i] = 1.0f; }
    for (int i = 0; i < 2; i++) { m.f_arr_fp64[i] = 1.0; }
    for (int i = 0; i < 3; i++) { memcpy(m.f_arr_str.items[i], "abcdef", 6); }
    /* f_arr_blob's holder carries no element count: a blob element is a sized
     * blob, and its own used-length already occupies the byte before slot 0 (see
     * docs/generator/c.md). Its value therefore occupies every slot. */
    for (int i = 0; i < 2; i++) {
        m.f_arr_blob.items[i].len = 5;
        memset(m.f_arr_blob.items[i].buf, 0xCD, 5);
    }

    m.f_nested.n_i32 = INT32_MIN;
    memcpy(m.f_nested.n_str, "wxyz", 4);

    /* Deliberately oversized so an encode that overruns MAX_SIZE still succeeds
     * and can be measured, instead of failing with BUFFER_FULL and hiding by how
     * much the bound was wrong. */
    uint8_t buf[MESSAGE_FILL_MAX_SIZE * 4];
    size_t used = 0;
    sofab_ret_t ret = message_fill_encode(&m, buf, sizeof buf, &used);

    if (ret != SOFAB_RET_OK) {
        printf("FAIL: encode returned %d\n", (int)ret);
        return 1;
    }
    printf("MESSAGE_FILL_MAX_SIZE = %d, encoded = %zu\n", MESSAGE_FILL_MAX_SIZE, (size_t)used);

    if (used > (size_t)MESSAGE_FILL_MAX_SIZE) {
        printf("FAIL: a fully filled message OVERRUNS its own MAX_SIZE by %zu bytes\n",
               used - (size_t)MESSAGE_FILL_MAX_SIZE);
        return 1;
    }
    if (used < (size_t)MESSAGE_FILL_MAX_SIZE) {
        printf("FAIL: MAX_SIZE overshoots by %zu bytes - every fixed-buffer target\n"
               "      pays that in wasted RAM, so the bound must be exact\n",
               (size_t)MESSAGE_FILL_MAX_SIZE - used);
        return 1;
    }
    printf("OK: the worst case is reached exactly and not exceeded\n");
    return 0;
}
