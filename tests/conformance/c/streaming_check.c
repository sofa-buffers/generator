/*!
 * @file streaming_check.c
 * @brief Behavioural check for the C streaming API.
 *
 * The corelib streams in both directions and always has: sofab_ostream_init
 * takes a flush callback that drains the buffer as it fills, and
 * sofab_istream_feed is incremental. Neither was reachable from generated code
 * -- _encode passed NULL for the callback, and _decode kept its istream and
 * decoder stack in locals -- so the whole message had to be one contiguous
 * buffer in each direction. The generator exposes both now; this runs them.
 *
 * The property is that streaming is indistinguishable from the one-shot path:
 * byte-identical on encode, value-identical on decode. Feeding one byte at a
 * time is the sharp end -- every varint, string and array element is then split
 * across feeds, so any parse state the decoder fails to carry between calls
 * shows up immediately.
 *
 * Runs against maxsize_fill.yaml, which carries one field of every wire shape.
 *
 * SPDX-License-Identifier: MIT
 */

#include "fill.h"

#include <stdio.h>
#include <string.h>

static uint8_t g_wire[MESSAGE_FILL_MAX_SIZE * 2];
static size_t  g_len;

static void sink(sofab_ostream_t *ctx, const uint8_t *data, size_t len, void *usr)
{
    (void)ctx; (void)usr;
    memcpy(g_wire + g_len, data, len);
    g_len += len;
}

int main(void)
{
    message_fill_t m;
    message_fill_init(&m);

    m.f_bool = 1;
    m.f_u64  = UINT64_MAX;          /* widest varint */
    m.f_i64  = INT64_MIN;
    m.f_fp64 = 1.0;
    memcpy(m.f_str, "123456789", 9);
    m.f_blob_len = 7;
    memset(m.f_blob, 0xAB, 7);
    /* The wire carries an array's length, so set it (MESSAGE_SPEC §3/§5.1). */
    m.f_arr_u32_len = 5;
    m.f_arr_str.len = 3;
    for (int i = 0; i < 5; i++) { m.f_arr_u32[i] = UINT32_MAX; }
    for (int i = 0; i < 3; i++) { memcpy(m.f_arr_str.items[i], "abcdef", 6); }
    m.f_nested.n_i32 = INT32_MIN;
    memcpy(m.f_nested.n_str, "wxyz", 4);

    /* Send through an 8-byte scratch buffer -- far smaller than the message, so
     * the sink fires repeatedly mid-encode rather than once at the end. */
    uint8_t scratch[8];
    sofab_ostream_t os;
    sofab_ostream_init(&os, scratch, sizeof scratch, 0, sink, NULL);
    if (message_fill_encode_to(&os, &m) != SOFAB_RET_OK) {
        printf("FAIL: streaming encode failed\n");
        return 1;
    }
    /* flush() invokes the callback for the tail and returns the count; the sink
     * is what accumulates, so the return value must not be added again. */
    (void)sofab_ostream_flush(&os);

    /* Must equal what the one-shot path produces, byte for byte. */
    uint8_t one[MESSAGE_FILL_MAX_SIZE];
    size_t used = 0;
    if (message_fill_encode(&m, one, sizeof one, &used) != SOFAB_RET_OK) {
        printf("FAIL: one-shot encode failed\n");
        return 1;
    }
    if (used != g_len || memcmp(one, g_wire, used) != 0) {
        printf("FAIL: streaming encode differs from encode(): %zu vs %zu bytes\n", g_len, used);
        return 1;
    }

    /* Receive one byte at a time. */
    message_fill_t back;
    message_fill_init(&back);
    message_fill_decoder_t d;
    message_fill_decoder_init(&d, &back);

    sofab_ret_t r = SOFAB_RET_OK;
    for (size_t i = 0; i < g_len; i++) {
        r = message_fill_decoder_feed(&d, g_wire + i, 1);
        if (r != SOFAB_RET_OK && r != SOFAB_RET_INCOMPLETE) {
            printf("FAIL: feed %zu reported %d\n", i, (int)r);
            return 1;
        }
    }
    /* The last verdict is the one that says whether the stream ended half-read.
     * There is no way to ask afterwards: sofab_istream_feed asserts datalen > 0,
     * so the zero-length end-of-input probe corelib-rs documents has no
     * counterpart here. */
    if (r != SOFAB_RET_OK) {
        printf("FAIL: stream did not end at a field boundary (%d)\n", (int)r);
        return 1;
    }

    /* Value-identical: re-encoding the chunk-fed message must give the same bytes. */
    uint8_t again[MESSAGE_FILL_MAX_SIZE];
    size_t againlen = 0;
    message_fill_encode(&back, again, sizeof again, &againlen);
    if (againlen != used || memcmp(again, one, used) != 0) {
        printf("FAIL: byte-wise decode differs from the one-shot decode\n");
        return 1;
    }

    printf("streaming: %zu bytes byte-identical through an %zu-byte buffer; "
           "value-identical after %zu single-byte feeds\n",
           g_len, sizeof scratch, g_len);
    return 0;
}
