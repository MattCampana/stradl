"""stradl / Perch geometry. Single source for every deliverable.

Two equal continuous-corner seats on a rising diagonal, touching at one
corner (the seam). One disc (the session) sits exactly on the seam; a
knockout ring keeps it clear of both seats. All three outer corners of a
seat use identical smoothed-corner parameters; the seam corner is a
sharp, tangent-free concave arc (reads as occlusion, not a bite)."""
import math

def f(v):
    s = f"{v:.3f}".rstrip("0").rstrip(".")
    return "0" if s in ("-0", "") else s

def _smooth_params(R, sm):
    # Figma-style smoothed corner (same maths as the app-icon body)
    p = (1 + sm) * R
    arc = 90 * (1 - sm)
    L = math.sin(math.radians(arc / 2)) * R * math.sqrt(2)
    alpha = (90 - arc) / 2
    p34 = R * math.tan(math.radians(alpha / 2))
    beta = 45 * sm
    c = p34 * math.cos(math.radians(beta))
    d = c * math.tan(math.radians(beta))
    b = (p - L - c - d) / 3
    a = 2 * b
    return p, a, b, c, d, L

DIRS = {"tr": ((1, 0), (0, 1)), "br": ((0, 1), (-1, 0)),
        "bl": ((-1, 0), (0, -1)), "tl": ((0, -1), (1, 0))}
ORDER = ["tl", "tr", "br", "bl"]

def tile(x0, y0, w, h, R, sm, cut=None):
    """Rounded rect with smoothed corners, clockwise. Every non-cut corner
    gets the SAME (R, sm); raises if they don't fit. cut=(corner, rk)
    replaces that corner with a concave arc of radius rk centred on it."""
    corners = {"tl": (x0, y0), "tr": (x0 + w, y0), "br": (x0 + w, y0 + h), "bl": (x0, y0 + h)}
    p = (1 + sm) * R
    if cut:
        assert p <= min(w, h) - cut[1] + 1e-9, ("corner does not fit", p, min(w, h) - cut[1])
    assert p <= min(w, h) / 2 + 1e-9
    P, a, b, c, d, L = _smooth_params(R, sm)
    out = []
    def pt(q):
        return f"{f(q[0])} {f(q[1])}"
    for k, name in enumerate(ORDER):
        (ix, iy), (ox, oy) = DIRS[name]
        cx, cy = corners[name]
        if cut and cut[0] == name:
            rk = cut[1]
            P0 = (cx - rk * ix, cy - rk * iy)
            P1 = (cx + rk * ox, cy + rk * oy)
            out.append(("M" if k == 0 else "L") + pt(P0))
            out.append(f"A{f(rk)} {f(rk)} 0 0 0 {pt(P1)}")
            continue
        S = (cx - P * ix, cy - P * iy)
        def at(u, v):
            return (S[0] + u * ix + v * ox, S[1] + u * iy + v * oy)
        out.append(("M" if k == 0 else "L") + pt(S))
        out.append("C" + " ".join(pt(q) for q in (at(a, 0), at(a + b, 0), at(a + b + c, d))))
        out.append(f"A{f(R)} {f(R)} 0 0 1 {pt(at(a + b + c + L, d + L))}")
        out.append("C" + " ".join(pt(q) for q in (at(a + b + c + L + d, d + L + c),
                                                  at(a + b + c + L + d, d + L + b + c), at(P, P))))
    return "".join(out) + "Z"

def plain_rrect(x0, y0, w, h, r, cut=None):
    """Circular-corner version for tiny grids (pixel-fitted masters)."""
    return tile(x0, y0, w, h, r, 0.0, cut)

def perch(cx, cy, s, R, sm, rd, ring, smooth=True):
    rk = rd + ring
    fn = tile if smooth else (lambda x, y, w, h, R_, sm_, cut: plain_rrect(x, y, w, h, R_, cut))
    a = fn(cx - s, cy, s, s, R, sm, ("tr", rk))   # lower-left seat
    b = fn(cx, cy - s, s, s, R, sm, ("bl", rk))   # upper-right seat
    return a, b, (cx, cy, rd)

def squircle(x0, y0, w, h, R, sm):
    return tile(x0, y0, w, h, R, sm)
