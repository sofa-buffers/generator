#!/usr/bin/env python3
"""Unit tests for sum_syms.py — run with
`python3 -m unittest discover -s tests/bench/lib`.

Hermetic like the other tests here: no toolchain, no nm. The listings below are
written in the exact shape `nm --defined-only -S -C` prints for the footprint
images, and fed to the same parse/summarize code bench_size calls.

What they guard is the rule in sum_syms.py's docstring: an aliased body is
counted ONCE (generator#611 — GCC's C1/C2 constructor aliases were counted once
per name, so an unrelated inlining flip between the aliases and a local .isra
clone moved a row by a whole function), without merging things that only look
alike, and the driver root is counted net of its skeleton rather than dropped.
"""

import importlib.util
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
_spec = importlib.util.spec_from_file_location("bench_sum_syms", HERE / "sum_syms.py")
ss = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(ss)

RAM = 0x20000000

# One synthetic linked image. Sizes are hex, as nm prints them.
LISTING = """\
00000000 00000018 W sofab::IStreamMessage::field_callback_(sofab_istream*)
00000078 00000024 W sofab::OStreamImpl::~OStreamImpl()
00000078 00000024 W sofab::OStreamImpl::~OStreamImpl()
0000009c 0000000c W sofab::OStreamImpl::~OStreamImpl()
00000100 00000040 t message::a_encode(int)
00000100 00000040 t message::b_encode(int)
00000200 00000010 T memcpy
00000200 00000010 T message::copy_bytes(void*, void const*, unsigned int)
00000300 00000000 t $t
00000300 00000020 T sofab_ostream_init
00000400 00000030 T reset
00000500 0000008a T __gedf2
00000500 0000008a T __gtdf2
00000508 00000082 T __ledf2
00000600 00000010 T memset
         00000004 a no_address_absolute
20000000 000003f9 b reset::buf
20000400 00000038 V message::VehicleTelemetry::deserialize()::_r0
20000440 00000038 V message::VehicleTelemetry::deserialize()::_r0
20000480 00000008 D message::table
"""

EXCLUDE = {"memcpy", "memset", "reset", "reset::buf"}
LIBGCC = {"__gedf2", "__gtdf2", "__ledf2"}


def syms(text=LISTING):
    return list(ss.parse_nm(text))


class ParseNm(unittest.TestCase):
    def test_skips_lines_without_an_address(self):
        names = [n for _, _, _, n in syms()]
        self.assertNotIn("no_address_absolute", names)
        # Demangled names keep their spaces.
        self.assertIn("message::copy_bytes(void*, void const*, unsigned int)", names)


class Summarize(unittest.TestCase):
    def test_full_image(self):
        text, data, bss = ss.summarize(syms(), RAM, EXCLUDE, LIBGCC)
        expected_text = (
            0x18            # field_callback_
            + 0x24          # ~OStreamImpl D1/D2: one body, two names -> once
            + 0x0C          # ~OStreamImpl D0: a different body (size), counted
            + 0x40          # a_encode/b_encode folded by -fipa-icf -> once
            + 0x10          # copy_bytes folded with the memcpy stub: still
                            #   SofaBuffers cost, since one name is not glue
            + 0x00 + 0x20   # zero-size label at the same address: 0 bytes, and
                            #   sofab_ostream_init is not merged away with it
        )                   # reset, memset: excluded; libgcc group: excluded
        self.assertEqual(text, expected_text)
        # Two distinct statics with one demangled name live at two addresses.
        self.assertEqual(bss, 2 * 0x38)
        self.assertEqual(data, 0x08)

    def test_aliases_do_not_depend_on_how_many_names(self):
        one = "00000078 00000024 W sofab::OStreamImpl::~OStreamImpl()\n"
        base = ss.summarize(syms(one), RAM)
        self.assertEqual(ss.summarize(syms(one * 2), RAM), base)
        self.assertEqual(ss.summarize(syms(one * 3), RAM), base)

    def test_alias_vs_isra_clone_is_the_same_cost(self):
        # The #611 flip: GCC emits basic_string(char const*) either as C1/C2
        # aliases of one body or as a single local .isra clone. Same bytes.
        aliases = (
            "000007fc 00000056 W std::string::basic_string(char const*)\n"
            "000007fc 00000056 W std::string::basic_string(char const*)\n"
        )
        clone = "000007fc 00000056 t std::string::basic_string(char const*) [clone .isra.0]\n"
        self.assertEqual(ss.summarize(syms(aliases), RAM),
                         ss.summarize(syms(clone), RAM))

    def test_different_sizes_at_one_address_are_not_aliases(self):
        listing = (
            "00000500 0000008a T entry_a\n"
            "00000508 00000082 T entry_b\n"
            "00000500 00000010 T entry_c\n"
        )
        self.assertEqual(ss.summarize(syms(listing), RAM)[0], 0x8A + 0x82 + 0x10)

    def test_group_excluded_only_when_every_name_is(self):
        both = "00000200 00000010 T memcpy\n00000200 00000010 T memcpy_alias\n"
        self.assertEqual(ss.summarize(syms(both), RAM, {"memcpy", "memcpy_alias"})[0], 0)
        self.assertEqual(ss.summarize(syms(both), RAM, {"memcpy"})[0], 0x10)

    def test_local_static_suffix_is_excluded_with_its_base(self):
        listing = "20000000 00000010 b buf.0\n20000010 00000004 b other\n"
        self.assertEqual(ss.summarize(syms(listing), RAM, {"buf"}), (0, 0, 4))

    def test_root_counted_net_of_skeleton(self):
        # reset is 0x30 in this image; its skeleton (the harness's own I/O) is
        # 0x0c, so 0x24 bytes of inlined SofaBuffers code are counted.
        excl = EXCLUDE - {"reset"}
        base = ss.summarize(syms(), RAM, EXCLUDE, LIBGCC)
        net = ss.summarize(syms(), RAM, excl, LIBGCC, root="reset", root_baseline=0x0C)
        self.assertEqual(net[0] - base[0], 0x30 - 0x0C)
        self.assertEqual(net[1:], base[1:])

    def test_symbol_size_requires_exactly_one(self):
        self.assertEqual(ss.symbol_size(syms(), "reset"), 0x30)
        with self.assertRaises(SystemExit):
            ss.symbol_size(syms(), "nope")


if __name__ == "__main__":
    unittest.main()
