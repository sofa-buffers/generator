"""How a conformance harness spells JSON, and how a driver reads it back.

The harnesses in tests/conformance/<lang> speak slightly different JSON for the
same message, and none of those differences is a wire fact. The shared drivers
import this module so the tolerance is written once:

  * a 64-bit integer goes IN as a bare number (C, C++, Zig, Go, Rust, ... read a
    quoted one as 0 or reject it) or as a decimal string (TypeScript and Dart
    read a double at their JSON front door) -- `int64_in()`;
  * a 64-bit integer comes OUT in either spelling -- `as_int()`;
  * a `u8` array comes out as a JSON array or as a base64 string (Go `[]byte`)
    -- `bytes_of()`.

Nothing here knows about any one backend; a harness that needs a spelling this
module does not cover gets it added here, not in a driver.
"""

import base64
import binascii

DIALECTS = ("number", "string")


def check_dialect(dialect: str) -> str:
    """Return `dialect` or raise ValueError naming the accepted spellings."""
    if dialect not in DIALECTS:
        raise ValueError(f"--int64-json must be number or string, got {dialect!r}")
    return dialect


def int64_in(value: int, dialect: str):
    """A 64-bit integer as the ENCODE input spells it in `dialect`."""
    return str(value) if dialect == "string" else value


def as_int(v):
    """An integer value from either spelling, or None if it is not one."""
    if isinstance(v, bool):
        return None
    if isinstance(v, int):
        return v
    if isinstance(v, str):
        try:
            return int(v)
        except ValueError:
            return None
    return None


def as_float(v):
    """A float value from either spelling, or None if it is not one."""
    if isinstance(v, bool):
        return None
    if isinstance(v, (int, float)):
        return float(v)
    if isinstance(v, str):
        try:
            return float(v)
        except ValueError:
            return None
    return None


def bytes_of(v):
    """The bytes a `u8`/blob rendering stands for, or None if it is neither.

    Accepts a JSON array of byte values, a base64 string, and null/"" as empty.
    """
    if v is None:
        return b""
    if isinstance(v, list):
        if all(isinstance(x, int) and not isinstance(x, bool) and 0 <= x < 256 for x in v):
            return bytes(v)
        return None
    if isinstance(v, str):
        try:
            return base64.b64decode(v, validate=True)
        except (binascii.Error, ValueError):
            return None
    return None
