/*!
 * @file init_equivalence_check.c
 * @brief The generated <msg>__init equals memset + the descriptor walk, for a
 *        schema whose tree declares no default (generator#737).
 *
 * SPDX-License-Identifier: MIT
 *
 * <msg>__init used to be a bulk clear PLUS a runtime walk of the descriptor
 * tree. Where no descriptor carries a default image the walk writes nothing but
 * zeros over memory the memset just zeroed -- at ~26 ns per field visit, since
 * its per-field clear has a runtime size and compiles to a `rep stosb` each at
 * -Os -- so the generator now emits the memset alone there.
 *
 * That rests on an assumption about the CORELIB, which this check pins against
 * the corelib the suite is built with: in the NULL-image branch,
 * sofab_object_init writes only zeros -- the per-field clear, a sized buffer's
 * companion length, a holder's element count, and no union tag (_default_tag
 * reads the image, so without one it is 0). A corelib that started writing a
 * non-zero byte there would make the generated initializer silently wrong, and
 * no round-trip test would see it: both shapes produce an all-zero object today,
 * which is also what every other assertion expects.
 *
 * Asserted both ways round:
 *   1. the generated init equals memset + walk, byte for byte, over a struct
 *      pre-filled with 0xAA -- padding included;
 *   2. the walk ALONE does not cover the whole struct (it leaves the padding),
 *      which is why the memset is the half that stays.
 *
 * A message WITH a default is the control: there the walk is still emitted, and
 * memset alone must NOT be enough.
 */

#include "noinit_sofab.h"
#include "withdef_sofab.h"

#include <stdio.h>
#include <string.h>

static int failures;

static void fail(const char *what)
{
    printf("FAIL: %s\n", what);
    failures++;
}

int main(void)
{
    /* ---- 1. no default in the tree: memset alone is the whole init -------- */
    message_noinit_t a, b;
    memset(&a, 0xAA, sizeof a);
    memset(&b, 0xAA, sizeof b);

    message_noinit__init(&a);                          /* what the generator emits */
    memset(&b, 0, sizeof b);                           /* what it used to emit:    */
    sofab_object_init(&message_noinit__descr, &b);     /* memset + the walk        */

    if (memcmp(&a, &b, sizeof a) != 0)
    {
        fail("the generated init differs from memset + sofab_object_init");
    }

    /* ---- 2. the walk alone leaves the padding ---------------------------- */
    message_noinit_t c;
    memset(&c, 0xAA, sizeof c);
    sofab_object_init(&message_noinit__descr, &c);
    size_t untouched = 0;
    const unsigned char *p = (const unsigned char *)&c;
    for (size_t i = 0; i < sizeof c; i++)
    {
        if (p[i] == 0xAAu) { untouched++; }
    }
    if (untouched == 0)
    {
        fail("the walk covered every byte -- then the memset's second job "
             "(deterministic padding) is gone and this check has lost its point");
    }

    /* ---- 3. the control: a default image still needs the walk ------------- */
    message_withdef_t d, e;
    memset(&d, 0xAA, sizeof d);
    memset(&e, 0, sizeof e);
    message_withdef__init(&d);
    if (memcmp(&d, &e, sizeof d) == 0)
    {
        fail("a message with a declared default must not initialize to all-zero");
    }
    if (d.n != 4242u)
    {
        fail("the declared default did not reach the member");
    }

    if (failures != 0)
    {
        printf("FAIL: %d init-equivalence check(s) failed\n", failures);
        return 1;
    }
    printf("init equivalence: memset alone == memset + walk (%zu bytes, %zu of them "
           "padding the walk never writes); a default still walks\n",
           sizeof a, untouched);
    return 0;
}
