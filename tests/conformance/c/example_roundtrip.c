#include "myfirstmessage.h"
#include <stdio.h>
#include <string.h>
#include <assert.h>

int main(void) {
    message_myfirstmessage_t m;
    message_myfirstmessage_init(&m);

    /* set representative non-default values across every field kind */
    m.somei8 = -42;
    m.somebool = 1;
    strcpy(m.somestring, "hello sofa");
    /* `count` is a capacity; the array's LENGTH is what goes on the wire
     * (MESSAGE_SPEC §3/§5.1), so a full array says so through its companion
     * length member (compact array) / element count (wrapper holder). */
    m.someintarray_len = 5;
    for (int i = 0; i < 5; i++) m.someintarray[i] = (int32_t)(i*1000 - 2000);
    m.someenum = 33;              /* YELLOW */
    m.somebitfield = 0x2;         /* flagB */
    m.somestruct.nestedint = 200;
    strcpy(m.somestruct.nestedstring, "nested!");
    m.somestruct.nestedstruct.deepint = -123456;
    m.someunion.option1 = 4242;   /* one option set */
    m.somefp32 = 3.5f;
    memcpy(m.someblob, (uint8_t[]){1,2,3,4,5}, 5);
    m.someblob_len = 5;   /* sized blob: set the used length (issue #128) */
    /* the holder's element count and each element's own used-length are separate
     * members: the count leads the holder, the element length leads its slot */
    m.someblobarray.len = 3;
    for (int i = 0; i < 3; i++) { memset(m.someblobarray.items[i].buf, i+1, 8); m.someblobarray.items[i].len = 8; }  /* sized blob elements (issue #130) */
    m.someu64 = 18446744073709551615ULL;
    m.somestringarray.len = 5;
    strcpy(m.somestringarray.items[0], "one");
    strcpy(m.somestringarray.items[1], "two");
    strcpy(m.somestringarray.items[2], "three");
    strcpy(m.somestringarray.items[3], "four");
    strcpy(m.somestringarray.items[4], "five");

    uint8_t buf[MESSAGE_MYFIRSTMESSAGE_MAX_SIZE];
    size_t used = 0;
    sofab_ret_t r = message_myfirstmessage_encode(&m, buf, sizeof(buf), &used);
    assert(r == SOFAB_RET_OK);
    printf("encoded %zu bytes (max %d)\n", used, MESSAGE_MYFIRSTMESSAGE_MAX_SIZE);
    assert(used <= MESSAGE_MYFIRSTMESSAGE_MAX_SIZE);

    message_myfirstmessage_t d;
    message_myfirstmessage_init(&d);
    r = message_myfirstmessage_decode(&d, buf, used);
    assert(r == SOFAB_RET_OK);

    /* verify round-trip */
    assert(d.somei8 == -42);
    assert(d.somebool == 1);
    assert(strcmp(d.somestring, "hello sofa") == 0);
    for (int i = 0; i < 5; i++) assert(d.someintarray[i] == (int32_t)(i*1000 - 2000));
    assert(d.someenum == 33);
    assert(d.somebitfield == 0x2);
    assert(d.somestruct.nestedint == 200);
    assert(strcmp(d.somestruct.nestedstring, "nested!") == 0);
    assert(d.somestruct.nestedstruct.deepint == -123456);
    assert(d.someunion.option1 == 4242);
    assert(d.somefp32 == 3.5f);
    assert(d.someblob_len == 5);   /* sub-maxlen blob length preserved (issue #128) */
    assert(memcmp(d.someblob, m.someblob, d.someblob_len) == 0);
    assert(d.someblobarray.len == 3);
    for (int i = 0; i < 3; i++) {
        assert(d.someblobarray.items[i].len == m.someblobarray.items[i].len);
        assert(memcmp(d.someblobarray.items[i].buf, m.someblobarray.items[i].buf, d.someblobarray.items[i].len) == 0);
    }
    assert(d.someu64 == 18446744073709551615ULL);
    assert(strcmp(d.somestringarray.items[0], "one") == 0);
    assert(strcmp(d.somestringarray.items[4], "five") == 0);
    /* The length is part of the value and comes back with it (MESSAGE_SPEC §3/§5.1). */
    assert(d.someintarray_len == 5);
    assert(d.somestringarray.len == 5);

    /* An array SHORTER than its capacity must survive the round trip as itself:
     * `count` bounds the storage, it never adds elements the wire did not carry.
     * Both forms are covered — a compact array whose tail elements equal the
     * element default (which must NOT be trimmed away, since [−2000,0] and
     * [−2000] are different values) and a wrapper array of 2 of a possible 5.
     * The BLOB wrapper is in there too: its slots carry their own used-length, so
     * until the holder count moved to offset 0 it had no count of its own and a
     * 1-of-3 blob array came back as 3. */
    message_myfirstmessage_t s;
    message_myfirstmessage_init(&s);
    s.someintarray_len = 2;
    s.someintarray[0] = -2000;
    s.someintarray[1] = 0;
    s.somestringarray.len = 2;
    strcpy(s.somestringarray.items[0], "one");
    strcpy(s.somestringarray.items[1], "two");
    s.someblobarray.len = 1;
    memset(s.someblobarray.items[0].buf, 0xAB, 3);
    s.someblobarray.items[0].len = 3;

    size_t sused = 0;
    r = message_myfirstmessage_encode(&s, buf, sizeof(buf), &sused);
    assert(r == SOFAB_RET_OK);

    message_myfirstmessage_t sd;
    message_myfirstmessage_init(&sd);
    r = message_myfirstmessage_decode(&sd, buf, sused);
    assert(r == SOFAB_RET_OK);
    assert(sd.someintarray_len == 2);
    assert(sd.someintarray[0] == -2000 && sd.someintarray[1] == 0);
    assert(sd.somestringarray.len == 2);
    assert(strcmp(sd.somestringarray.items[0], "one") == 0);
    assert(strcmp(sd.somestringarray.items[1], "two") == 0);
    assert(sd.someblobarray.len == 1);
    assert(sd.someblobarray.items[0].len == 3);
    assert(memcmp(sd.someblobarray.items[0].buf, s.someblobarray.items[0].buf, 3) == 0);
    printf("short arrays round-trip at their own length (%u / %u / %u)\n",
           (unsigned)sd.someintarray_len, (unsigned)sd.somestringarray.len,
           (unsigned)sd.someblobarray.len);

    printf("ALL ROUND-TRIP CHECKS OK\n");
    return 0;
}
