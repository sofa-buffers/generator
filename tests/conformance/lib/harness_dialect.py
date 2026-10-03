"""How a conformance harness spells JSON, and how a driver reads it back.

The harnesses in tests/conformance/<lang> speak slightly different JSON for the
same message, and none of those differences is a wire fact. The shared drivers
import this module so the tolerance is written once:

  * a 64-bit integer goes IN as a bare number (C, C++, Zig, Go, Rust, ... read a
    quoted one as 0 or reject it) or as a decimal string (TypeScript and Dart
    read a double at their JSON front door) -- `int64_in()`;
  * an infinite float goes IN as the string "inf", the string "Infinity", the
    bare token `Infinity` or the literal `1e999` -- `float_in()`, `dumps_in()`;
  * a 64-bit integer comes OUT in either spelling -- `as_int()`;
  * a `u8` array comes out as a JSON array or as a base64 string (Go `[]byte`)
    -- `bytes_of()`;
  * a non-finite float comes OUT as a bare `inf` / `-inf` / `nan` (C), as
    `Infinity` / `NaN` (Python, Dart, Java), or as the strings "inf" / "-inf" /
    "nan" (Go, TypeScript) -- `loads_out()` reads all of them, and `-0` as -0.0.

Nothing here knows about any one backend; a harness that needs a spelling this
module does not cover gets it added here, not in a driver.
"""

import base64
import binascii
import json
import re

DIALECTS = ("number", "string")

# How an infinite float goes IN: the string "inf" / "-inf" (the vector file's own
# spelling), the string "Infinity" / "-Infinity" (what Java's Float.parseFloat and
# .NET's float parsing read), or the bare token JSON-with-extensions readers take,
# `Infinity`, or a number literal too large for a double (`1e999`), which Dart's
# and JavaScript's own JSON readers turn into an infinity.
INF_DIALECTS = ("inf", "infinity", "literal", "overflow")


def check_dialect(dialect: str) -> str:
    """Return `dialect` or raise ValueError naming the accepted spellings."""
    if dialect not in DIALECTS:
        raise ValueError(f"--int64-json must be number or string, got {dialect!r}")
    return dialect


def check_inf_dialect(dialect: str) -> str:
    """Return `dialect` or raise ValueError naming the accepted spellings."""
    if dialect not in INF_DIALECTS:
        raise ValueError(f"--inf-json must be one of inf, infinity, literal, overflow, got {dialect!r}")
    return dialect


def float_in(value, dialect: str):
    """A float as the ENCODE input spells it. `value` is a JSON number or the
    vector file's string "inf" / "-inf"; only the latter depends on `dialect`."""
    if isinstance(value, str):
        if dialect == "literal":
            return float(value)  # json.dumps writes it as Infinity / -Infinity
        if dialect == "infinity":
            return value.replace("inf", "Infinity")
        if dialect == "overflow":
            return OVERFLOW_MARK + value  # spelled by dumps_in()
    return value


OVERFLOW_MARK = "\0overflow:"


def dumps_in(value, dialect: str) -> str:
    """json.dumps for an ENCODE input. The `overflow` spelling is a number literal,
    which json.dumps cannot write, so float_in() leaves a marker and it is
    replaced by the literal here."""
    text = json.dumps(value)
    if dialect == "overflow":
        for sign in ("", "-"):
            text = text.replace('"\\u0000overflow:%sinf"' % sign, sign + "1e999")
    return text


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


_BARE_NONFINITE = re.compile(r'("(?:\\.|[^"\\])*")|(?<![\w.])(-?)(inf|nan)(?![\w.])')


def loads_out(text: str):
    """json.loads for a harness's DECODE output. A bare `inf` / `-inf` / `nan`
    token is read as the matching string (as_float turns it into the float), and a
    `-0` literal is read as negative zero, a different float from 0 that
    json.loads would otherwise turn into the int 0."""
    text = _BARE_NONFINITE.sub(
        lambda m: m.group(1) or '"%s%s"' % (m.group(2), m.group(3)), text)
    return json.loads(text, parse_int=lambda s: -0.0 if s == "-0" else int(s))
