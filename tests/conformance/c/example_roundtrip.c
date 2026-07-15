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
    for (int i = 0; i < 3; i++) { memset(m.someblobarray.items[i].buf, i+1, 8); m.someblobarray.items[i].len = 8; }  /* sized blob elements (issue #130) */
    m.someu64 = 18446744073709551615ULL;
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
    for (int i = 0; i < 3; i++) {
        assert(d.someblobarray.items[i].len == m.someblobarray.items[i].len);
        assert(memcmp(d.someblobarray.items[i].buf, m.someblobarray.items[i].buf, d.someblobarray.items[i].len) == 0);
    }
    assert(d.someu64 == 18446744073709551615ULL);
    assert(strcmp(d.somestringarray.items[0], "one") == 0);
    assert(strcmp(d.somestringarray.items[4], "five") == 0);

    printf("ALL ROUND-TRIP CHECKS OK\n");
    return 0;
}
