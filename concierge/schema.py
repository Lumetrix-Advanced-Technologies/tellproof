"""The subset of JSON Schema (2020-12) that spec/pack.schema.json and spec/batch.schema.json
use: type, required, properties, additionalProperties, items, minItems, maxItems, enum, const,
pattern, minimum, maximum, oneOf, format (date, date-time). No dependencies."""
import datetime as dt
import re

_TYPES = {"object": dict, "array": list, "string": str, "integer": int, "number": (int, float), "null": type(None), "boolean": bool}


def _is(value, t: str) -> bool:
    if t in ("integer", "number") and isinstance(value, bool):
        return False
    if t == "integer" and isinstance(value, float):
        return value.is_integer()
    return isinstance(value, _TYPES[t])


def _format_ok(value, fmt: str) -> bool:
    if not isinstance(value, str):
        return True
    if fmt == "date":
        try:
            dt.date.fromisoformat(value)
            return bool(re.fullmatch(r"\d{4}-\d{2}-\d{2}", value))
        except ValueError:
            return False
    if fmt == "date-time":
        if not re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})", value):
            return False
        try:
            dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
            return True
        except ValueError:
            return False
    return True


def errors(value, schema: dict, path: str = "$") -> list[str]:
    out = []
    if "oneOf" in schema:
        matches = sum(not errors(value, s, path) for s in schema["oneOf"])
        if matches != 1:
            out.append(f"{path}: matches {matches} of the oneOf alternatives, want exactly 1")
        return out
    t = schema.get("type")
    if t is not None:
        types = t if isinstance(t, list) else [t]
        if not any(_is(value, x) for x in types):
            return [f"{path}: {value!r} is not of type {t}"]
    if "const" in schema and value != schema["const"]:
        out.append(f"{path}: must be {schema['const']!r}")
    if "enum" in schema and value not in schema["enum"]:
        out.append(f"{path}: {value!r} is not one of {schema['enum']}")
    if isinstance(value, str):
        if "pattern" in schema and not re.search(schema["pattern"], value):
            out.append(f"{path}: {value!r} does not match {schema['pattern']}")
        if "format" in schema and not _format_ok(value, schema["format"]):
            out.append(f"{path}: {value!r} is not a valid {schema['format']}")
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        if "minimum" in schema and value < schema["minimum"]:
            out.append(f"{path}: {value} is below {schema['minimum']}")
        if "maximum" in schema and value > schema["maximum"]:
            out.append(f"{path}: {value} is above {schema['maximum']}")
    if isinstance(value, dict):
        props = schema.get("properties", {})
        for r in schema.get("required", []):
            if r not in value:
                out.append(f"{path}: {r} is required")
        if schema.get("additionalProperties") is False:
            out += [f"{path}: {k} is not allowed" for k in sorted(set(value) - set(props))]
        for k, v in value.items():
            if k in props:
                out += errors(v, props[k], f"{path}.{k}")
    if isinstance(value, list):
        if "minItems" in schema and len(value) < schema["minItems"]:
            out.append(f"{path}: needs at least {schema['minItems']} items")
        if "maxItems" in schema and len(value) > schema["maxItems"]:
            out.append(f"{path}: has more than {schema['maxItems']} items")
        if "items" in schema:
            for i, v in enumerate(value):
                out += errors(v, schema["items"], f"{path}[{i}]")
    return out
