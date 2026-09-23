#!/usr/bin/env python3
"""sum_syms.py <nm> <elf> <libgcc.a> <ram_origin_hex> [name...]
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

A symbol's OWN nm type letter is not enough to sort it into text/data/bss: a
C++ vtable and a same-TU guard variable both come back as weak ('V'/'v'),
one flash-resident and the other RAM, because nm's letter conflates "weak"
with "category" for objects. <ram_origin_hex> is the RAM ORIGIN both
footprint.ld files declare (0x20000000): everything below it is flash
(text/rodata, regardless of letter), everything at or above it is RAM, sorted
into data/bss by letter there.
"""
import subprocess
import sys


def defined_symbols(nm, path, demangle=False):
    args = [nm, "--defined-only", "-S"]
    if demangle:
        args.append("-C")
    args.append(path)
    out = subprocess.run(args, capture_output=True, text=True, check=True).stdout
    for line in out.splitlines():
        parts = line.split(None, 3)
        if len(parts) < 4:
            continue
        addr, size, typ, name = parts
        try:
            yield int(addr, 16), int(size, 16), typ, name
        except ValueError:
            continue


def main(argv):
    nm, elf, libgcc, ram_origin_s, *exclude = argv[1:]
    ram_origin = int(ram_origin_s, 16)
    exclude = set(exclude)
    libgcc_names = {name for _, _, _, name in defined_symbols(nm, libgcc)}

    text = data = bss = 0
    for addr, size, typ, name in defined_symbols(nm, elf, demangle=True):
        base = name.split(".", 1)[0]
        if name in exclude or base in exclude or name in libgcc_names:
            continue
        if addr < ram_origin:
            text += size
        elif typ.upper() == "D":
            data += size
        else:
            bss += size

    print(text, data, bss)


if __name__ == "__main__":
    main(sys.argv)
