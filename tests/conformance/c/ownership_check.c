/*!
 * @file ownership_check.c
 * @brief A decoded message OWNS its bytes (CORELIB_PLAN §6.7 / §6.7.1,
 *        generator#412).
 *
 * The rule: no value the codec delivers may outlive the callback it arrived in.
 * §6.0 fixes that for `feed` -- a chunk is borrowed only for the duration of the
 * call, so once it returns the caller may reuse, overwrite or FREE that memory
 * and the decoded message must not be affected -- and §6.7.1 gives the one-shot
 * path no exemption: `decode(buffer)` copies too.
 *
 * The oracle is DESTRUCTIVE, not comparative: encode a sample, decode it out of
 * heap storage this program controls, destroy that storage, then re-encode and
 * diff. Nothing else in this suite reaches the property -- streaming_check.c,
 * example_roundtrip.c and maxsize_fill.c all decode from a live buffer that
 * stays untouched for the rest of the run, and a destination holding a window
 * into one of those reads back perfectly.
 *
 * KNOWN REACH -- do not read a pass as "every field is copied":
 *
 *   * In C no destination CAN alias. Every field is inline storage inside the
 *     caller's struct (`char f_str[10]`, a `uint8_t len` paired with a
 *     `uint8_t buf[N]`, `items[N]` for a wrapper array), chosen by the object
 *     descriptor, and the corelib's only payload entry points --
 *     sofab_istream_read_string(ctx, char *var, size_t varlen) and its blob
 *     twin -- take a destination to copy INTO. There is no pointer-storage
 *     variant to regress into, so the mutation issue #412 asks for cannot be
 *     written here: this leg is a CORELIB regression net, not a
 *     generated-destination one. What it would catch is a corelib that started
 *     deferring the copy -- recording a pointer into the fed chunk and reading
 *     it back at payload completion.
 *   * That is also why it is built with -fsanitize=address, corelib sources
 *     included, and why every chunk is a separate heap block freed the instant
 *     `feed` returns. Without ASan a value comparison can print OWNS while the
 *     message holds a dangling pointer: freed heap usually still reads back the
 *     bytes that were in it.
 *   * The scribble byte is 0x41 ('A'), not 0xff, for the reason the family
 *     settled on: an aliased string must still RE-ENCODE, so the oracle stays a
 *     byte comparison instead of becoming an encoder error unrelated causes
 *     could produce.
 *
 * CHUNK SIZE IS THE AXIS, not the entry point -- generated `_decode` is
 * `decoder_init` plus one `decoder_feed`, so there is only one decode machine
 * here, fed two ways. A payload SPLIT across chunks is reassembled inside the
 * corelib and copied out of its own storage whether or not anything wanted to
 * alias, so small chunks alone cannot reach the whole-payload delivery path.
 * The sweep therefore ends at a size that carries the entire message.
 *
 * Runs against maxsize_fill.yaml, which carries one field of every wire shape,
 * including all four aliasing-capable kinds (string, blob, array<string>,
 * array<blob>) plus a string nested in a struct.
 *
 * SPDX-License-Identifier: MIT
 */

#include "fill.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* 'A': see the header note. An aliased string destination must still encode. */
#define SCRIBBLE 0x41

static uint8_t g_want[MESSAGE_FILL_MAX_SIZE];
static size_t  g_want_len;
static int     g_failures;

static void hexdump(const char *label, const uint8_t *b, size_t n)
{
    printf("  %s ", label);
    for (size_t i = 0; i < n; i++) { printf("%02x", b[i]); }
    printf("\n");
}

/*! Re-encode `m` and diff it against the sample. A re-encode that FAILS is a
 *  failure of this check too, not something to pass over: the encoder validates,
 *  so a destroyed string destination can come back as an error rather than as
 *  different bytes. */
static void must_match(const char *what, const message_fill_t *m)
{
    uint8_t again[MESSAGE_FILL_MAX_SIZE];
    size_t  n = 0;
    if (message_fill_encode(m, again, sizeof again, &n) != SOFAB_RET_OK) {
        printf("FAIL: %s: re-encoding the decoded message failed -- a destination "
               "aliased the storage it was decoded from\n", what);
        g_failures++;
        return;
    }
    if (n != g_want_len || memcmp(again, g_want, n) != 0) {
        printf("FAIL: %s: a decoded field aliased the storage it was decoded from\n", what);
        hexdump("want", g_want, g_want_len);
        hexdump("got ", again, n);
        printf("  f_str = \"%s\"  f_blob = ", m->f_str);
        for (unsigned i = 0; i < m->f_blob_len; i++) { printf("%02x", m->f_blob[i]); }
        printf("\n");
        for (unsigned i = 0; i < m->f_arr_str.len; i++) {
            printf("  f_arr_str.items[%u] = \"%s\"\n", i, m->f_arr_str.items[i]);
        }
        g_failures++;
    }
}

/*! Fill every aliasing-capable kind. An array kind nothing fills is an array
 *  kind nothing tests, so the wrapper arrays get real elements rather than the
 *  lengths the streaming check happens to need. */
static void sample(message_fill_t *m)
{
    message_fill_init(m);

    m->f_bool = 1;
    m->f_u64  = UINT64_MAX;
    m->f_i64  = INT64_MIN;
    m->f_fp64 = 1.0;

    memcpy(m->f_str, "123456789", 9);
    m->f_blob_len = 7;
    memset(m->f_blob, 0xAB, 7);

    m->f_arr_u32_len = 5;
    for (int i = 0; i < 5; i++) { m->f_arr_u32[i] = UINT32_MAX; }
    m->f_arr_u64_len = 3;
    for (int i = 0; i < 3; i++) { m->f_arr_u64[i] = UINT64_MAX - (uint64_t)i; }
    m->f_arr_fp32_len = 4;
    for (int i = 0; i < 4; i++) { m->f_arr_fp32[i] = 1.5f * (float)(i + 1); }
    m->f_arr_fp64_len = 2;
    for (int i = 0; i < 2; i++) { m->f_arr_fp64[i] = -2.25 * (double)(i + 1); }

    m->f_arr_str.len = 3;
    for (int i = 0; i < 3; i++) { memcpy(m->f_arr_str.items[i], "abcdef", 6); }

    m->f_arr_blob.len = 2;
    m->f_arr_blob.items[0].len = 5;
    memset(m->f_arr_blob.items[0].buf, 0xCD, 5);
    m->f_arr_blob.items[1].len = 3;
    memset(m->f_arr_blob.items[1].buf, 0xEF, 3);

    m->f_nested.n_i32 = INT32_MIN;
    memcpy(m->f_nested.n_str, "wxyz", 4);
}

int main(void)
{
    message_fill_t m;
    sample(&m);

    if (message_fill_encode(&m, g_want, sizeof g_want, &g_want_len) != SOFAB_RET_OK) {
        printf("FAIL: encoding the sample failed\n");
        return 2;
    }

    /* ---- 1. one-shot, out of heap storage that is then destroyed --------
     * §6.7.1: `buf` may be reused, overwritten or freed the moment _decode
     * returns, so doing exactly that is a legitimate caller. */
    {
        uint8_t *buf = malloc(g_want_len);
        if (buf == NULL) { printf("FAIL: out of memory\n"); return 2; }
        memcpy(buf, g_want, g_want_len);

        message_fill_t got;
        message_fill_init(&got);
        /* Unchecked, a failed decode leaves an all-default message that
         * re-encodes identically twice and the whole check passes vacuously. */
        if (message_fill_decode(&got, buf, g_want_len) != SOFAB_RET_OK) {
            printf("FAIL: one-shot decode failed\n");
            free(buf);
            return 1;
        }
        memset(buf, SCRIBBLE, g_want_len);
        free(buf);
        must_match("one-shot decode", &got);
    }

    /* ---- 2. streaming, one heap block per chunk, freed on return --------
     * §6.0: the borrow ends when feed returns. The sweep ends at a size that
     * carries the whole message, which is the only one guaranteed to deliver
     * every payload inside a single chunk. */
    {
        const size_t sizes[] = {1, 2, 3, 7, 16, MESSAGE_FILL_MAX_SIZE};
        for (unsigned s = 0; s < sizeof sizes / sizeof sizes[0]; s++) {
            const size_t size = sizes[s];
            message_fill_t got;
            message_fill_init(&got);
            message_fill_decoder_t d;
            message_fill_decoder_init(&d, &got);

            sofab_ret_t r = SOFAB_RET_OK;
            for (size_t off = 0; off < g_want_len; off += size) {
                size_t n = g_want_len - off;
                if (n > size) { n = size; }
                /* sofab_istream_feed asserts datalen > 0; n is never 0 here. */
                uint8_t *chunk = malloc(n);
                if (chunk == NULL) { printf("FAIL: out of memory\n"); return 2; }
                memcpy(chunk, g_want + off, n);
                r = message_fill_decoder_feed(&d, chunk, n);
                memset(chunk, SCRIBBLE, n);
                free(chunk);
                if (r != SOFAB_RET_OK && r != SOFAB_RET_INCOMPLETE) {
                    printf("FAIL: chunk size %zu: feed at %zu reported %d\n",
                           size, off, (int)r);
                    return 1;
                }
            }
            /* The last verdict is the one that says whether the stream ended
             * half-read; there is no way to ask afterwards. */
            if (r != SOFAB_RET_OK) {
                printf("FAIL: chunk size %zu: stream did not end at a field boundary (%d)\n",
                       size, (int)r);
                g_failures++;
                continue;
            }

            char what[64];
            snprintf(what, sizeof what, "streaming feed(chunk=%zu)", size);
            must_match(what, &got);
        }
    }

    if (g_failures > 0) { return 1; }
    printf("ownership: %zu bytes, decoded message owns them after the input was "
           "scribbled and freed -- one-shot + 6 chunk sizes\n", g_want_len);
    return 0;
}
