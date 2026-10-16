"""Salted trace commitment, as in docs/architecture.md section 2.4 and internal/merkle.

Leaf: sha256(0x00 || salt16 || utf8(trace_id)), salt16 = HMAC-SHA256(secret, trace_id)[:16].
Node: sha256(0x01 || left || right). An odd node at the end of a level is carried up.
"""
import hashlib
import hmac


def salt(secret: bytes, trace_id: str) -> bytes:
    return hmac.new(secret, trace_id.encode("utf-8"), hashlib.sha256).digest()[:16]


def leaf_hash(salt16: bytes, trace_id: str) -> bytes:
    return hashlib.sha256(b"\x00" + salt16 + trace_id.encode("utf-8")).digest()


def node_hash(left: bytes, right: bytes) -> bytes:
    return hashlib.sha256(b"\x01" + left + right).digest()


def levels(leaves: list[bytes]) -> list[list[bytes]]:
    if not leaves:
        raise ValueError("merkle: no leaves")
    out = [list(leaves)]
    while len(out[-1]) > 1:
        level = out[-1]
        nxt = [node_hash(level[i], level[i + 1]) if i + 1 < len(level) else level[i] for i in range(0, len(level), 2)]
        out.append(nxt)
    return out


def root(leaves: list[bytes]) -> bytes:
    return levels(leaves)[-1][0]


def proof(tree: list[list[bytes]], index: int) -> list[dict]:
    if not 0 <= index < len(tree[0]):
        raise IndexError(f"merkle: leaf index {index} out of range")
    steps = []
    for level in tree[:-1]:
        sib = index ^ 1
        if sib < len(level):
            steps.append({"side": "L" if sib < index else "R", "hash": level[sib].hex()})
        index //= 2
    return steps


def verify(leaf: bytes, steps: list[dict], want_root: bytes) -> bool:
    h = leaf
    for s in steps:
        x = bytes.fromhex(s["hash"])
        if s["side"] == "L":
            h = node_hash(x, h)
        elif s["side"] == "R":
            h = node_hash(h, x)
        else:
            return False
    return h == want_root
