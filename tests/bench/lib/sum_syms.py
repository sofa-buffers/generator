#!/usr/bin/env python3
"""sum_syms.py [--root NAME --skeleton ELF] <nm> <elf> <libgcc.a> <ram_origin_hex> [name...]
    -> "<text> <data> <bss>"

Sums a linked, --gc-sections'd ELF's .text/.rodata/.data/.bss BY SYMBOL,
excluding:
  * every symbol named on the command line (or matching its GCC local-static
    disambiguation, "name.N") -- the footprint driver's own harness code, not
    what it is measuring;
  * every symbol libgcc.a can define -- resolved from the actual toolchain
    invocation (`$cc ... -print-libgcc-file-name`), not a hardcoded list, so a
    GCC upgrade that renames a helper cannot silently start counting it.

Used by tests/bench/lang/c.sh and cpp.sh: linking is what makes --gc-sections
correctly decide which SofaBuffers code is reachable, but the linked image
also carries the freestanding glue (libc stubs, a bump allocator, the ABI
helpers -lgcc supplies) that a real target's own environment would already
provide -- none of which is SofaBuffers code, so none of it belongs in the row.

BYTES, NOT NAMES. nm lists a symbol per NAME, and one piece of code can carry
several: GCC emits a C++ constructor/destructor as one body under its C1/C2
(D1/D2) aliases, -fipa-icf folds identical functions into aliases of one body,
and libgcc exports several entry names for one comparison routine. Summing per
name counted such a body once per alias -- and whether GCC emits the aliases or
a single local `.isra` clone depends on inlining decisions elsewhere in the
image, so an unrelated change could move a row by the full size of a function
the image carries once (generator#611). The rule, therefore:

  * symbols are grouped by (address, size); a group is ONE body and is counted
    once, whatever number of names it has;
  * a group is excluded only when EVERY name in it is excluded. A body that is
    also reachable under a SofaBuffers name is SofaBuffers cost even when it
    happens to be folded with a glue function -- the image needs those bytes
    for SofaBuffers either way;
  * symbols that merely share an ADDRESS but not a size are not aliases and are
    all counted: a zero-size label contributes 0 bytes regardless, and two
    entry points into one routine with different extents (libgcc's __gedf2/
    __ledf2 pattern) are distinct nm records. None occurs in SofaBuffers code
    today; the size key keeps such a pair from being merged by accident.

The (address, size) key is also what keeps apart the things that must NOT be
merged: distinct local statics sharing a demangled name (a function-local
`_r0` in several template instances) live at different addresses.

THE DRIVER ROOT. The footprint drivers call SofaBuffers from one root function
(`reset`) that is harness code, but GCC may inline generated code into it --
the C++ driver's `reset` holds the message constructor, encodeTo and try_decode
on the -dyn row. Excluding the root by name would then drop real SofaBuffers
bytes from the row, and whether they sit in the root or in their own symbol is
again an inlining decision. `--root NAME --skeleton ELF` counts the root as
size(NAME in <elf>) - size(NAME in <skeleton>), where <skeleton> is the same
driver with every SofaBuffers call taken out: what remains of the root is the
harness's own volatile I/O, and only that is subtracted.

A symbol's OWN nm type letter is not enough to sort it into text/data/bss: a
C++ vtable and a same-TU guard variable both come back as weak ('V'/'v'),
one flash-resident and the other RAM, because nm's letter conflates "weak"
with "category" for objects. <ram_origin_hex> is the RAM ORIGIN both
footprint.ld files declare (0x20000000): everything below it is flash
(text/rodata, regardless of letter), everything at or above it is RAM, sorted
into data/bss by letter there.

Unit tests: tests/bench/lib/test_sum_syms.py (hermetic, synthetic nm listings).
"""
import argparse
import subprocess
import sys


def parse_nm(text):
    """Yield (addr, size, type, name) from `nm --defined-only -S` output.

    Lines without a size (absolute symbols nm prints with no extent) are
    skipped: they carry no bytes to count."""
    for line in text.splitlines():
        parts = line.split(None, 3)
        if len(parts) < 4:
            continue
        addr, size, typ, name = parts
        try:
            yield int(addr, 16), int(size, 16), typ, name
        except ValueError:
            continue


def defined_symbols(nm, path, demangle=False):
    args = [nm, "--defined-only", "-S"]
    if demangle:
        args.append("-C")
    args.append(path)
    out = subprocess.run(args, capture_output=True, text=True, check=True).stdout
    return list(parse_nm(out))


def is_excluded(name, exclude, libgcc_names):
    base = name.split(".", 1)[0]
    return name in exclude or base in exclude or name in libgcc_names


def summarize(symbols, ram_origin, exclude=(), libgcc_names=(), root=None,
              root_baseline=0):
    """Return (text, data, bss) for the parsed symbols of one linked image.

    `root`, if given, is counted as its size minus `root_baseline` (the size of
    the same symbol in the skeleton image); every other rule is in the module
    docstring."""
    exclude = set(exclude)
    libgcc_names = set(libgcc_names)

    groups = {}  # (addr, size) -> [(typ, name), ...], first-seen order
    for addr, size, typ, name in symbols:
        groups.setdefault((addr, size), []).append((typ, name))

    text = data = bss = 0
    for (addr, size), members in groups.items():
        names = [name for _, name in members]
        if root is not None and root in names:
            size -= root_baseline
        elif all(is_excluded(n, exclude, libgcc_names) for n in names):
            continue
        typ = members[0][0]
        if addr < ram_origin:
            text += size
        elif typ.upper() == "D":
            data += size
        else:
            bss += size
    return text, data, bss


def symbol_size(symbols, name):
    sizes = {size for _, size, _, n in symbols if n == name}
    if len(sizes) != 1:
        raise SystemExit(f"sum_syms: expected exactly one '{name}', found sizes {sorted(sizes)}")
    return sizes.pop()


def main(argv):
    ap = argparse.ArgumentParser(add_help=True)
    ap.add_argument("--root", help="driver root counted net of --skeleton")
    ap.add_argument("--skeleton", help="the driver linked with no SofaBuffers call")
    ap.add_argument("nm")
    ap.add_argument("elf")
    ap.add_argument("libgcc")
    ap.add_argument("ram_origin")
    ap.add_argument("exclude", nargs="*")
    a = ap.parse_args(argv[1:])
    if bool(a.root) != bool(a.skeleton):
        ap.error("--root and --skeleton go together")

    libgcc_names = {name for _, _, _, name in defined_symbols(a.nm, a.libgcc)}
    symbols = defined_symbols(a.nm, a.elf, demangle=True)
    baseline = 0
    if a.root:
        baseline = symbol_size(defined_symbols(a.nm, a.skeleton, demangle=True), a.root)
        symbol_size(symbols, a.root)  # present exactly once in the full image too

    text, data, bss = summarize(symbols, int(a.ram_origin, 16), a.exclude,
                                libgcc_names, a.root, baseline)
    print(text, data, bss)


if __name__ == "__main__":
    main(sys.argv)
