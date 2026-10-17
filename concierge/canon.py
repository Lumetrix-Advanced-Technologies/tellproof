"""RFC 8785 canonical JSON (JCS) in Python, matching internal/jcs and web/verify/verify.js.

jcs(value) -> UTF-8 bytes. Keys sorted by UTF-16 code units; strings escape only
'"', '\\' and U+0000-U+001F; numbers in ECMAScript Number#toString form. Integers
must be safe JavaScript integers (D-018). loads() parses strictly (duplicate keys,
NaN and Infinity rejected).
"""
import json
import math

MAX_SAFE_INTEGER = 2**53 - 1


def _number(x) -> str:
    if isinstance(x, bool):
        raise TypeError("bool is not a number")
    if isinstance(x, int):
        if abs(x) > MAX_SAFE_INTEGER:
            raise ValueError(f"integer {x} is outside the safe integer range")
        return str(x)
    if not math.isfinite(x):
        raise ValueError("number is NaN or infinite")
    if x == 0:
        return "0"
    sign = "-" if x < 0 else ""
    # repr gives the shortest round-trip digits; lay them out as ECMAScript does.
    # value = 0.d1d2...dk * 10^n
    mantissa, _, exp = repr(abs(x)).partition("e")
    whole, _, frac = mantissa.partition(".")
    all_digits = whole + frac
    leading = len(all_digits) - len(all_digits.lstrip("0"))
    n = len(whole) + int(exp or 0) - leading
    digits = all_digits.lstrip("0").rstrip("0")
    k = len(digits)
    if k <= n <= 21:
        out = digits + "0" * (n - k)
    elif 0 < n <= 21:
        out = digits[:n] + "." + digits[n:]
    elif -6 < n <= 0:
        out = "0." + "0" * (-n) + digits
    else:
        e = n - 1
        out = digits[0] + ("." + digits[1:] if k > 1 else "") + "e" + ("+" if e >= 0 else "-") + str(abs(e))
    return sign + out


def _string(s: str) -> str:
    s.encode("utf-8")  # raises on unpaired surrogates
    return json.dumps(s, ensure_ascii=False)


def _encode(v) -> str:
    if v is None:
        return "null"
    if v is True:
        return "true"
    if v is False:
        return "false"
    if isinstance(v, (int, float)):
        return _number(v)
    if isinstance(v, str):
        return _string(v)
    if isinstance(v, (list, tuple)):
        return "[" + ",".join(_encode(x) for x in v) + "]"
    if isinstance(v, dict):
        for k in v:
            if not isinstance(k, str):
                raise TypeError("object keys must be strings")
        keys = sorted(v, key=lambda k: k.encode("utf-16-be"))
        return "{" + ",".join(_string(k) + ":" + _encode(v[k]) for k in keys) + "}"
    raise TypeError(f"cannot canonicalize {type(v).__name__}")


def jcs(v) -> bytes:
    return _encode(v).encode("utf-8")


def _no_duplicates(pairs):
    out = {}
    for k, v in pairs:
        if k in out:
            raise ValueError(f"duplicate object key {k!r}")
        out[k] = v
    return out


def _reject_constant(name):
    raise ValueError(f"{name} is not JSON")


def _parse_int(s: str):
    # JSON numbers are IEEE doubles (RFC 8785); keep exact ints only where a double is exact.
    i = int(s)
    return i if abs(i) <= MAX_SAFE_INTEGER else float(s)


def loads(text):
    return json.loads(text, object_pairs_hook=_no_duplicates, parse_constant=_reject_constant, parse_int=_parse_int)
