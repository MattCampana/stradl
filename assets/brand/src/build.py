"""Builds the final stradl identity set from spec.json (single source)."""
import os, sys, json, subprocess
SRC = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, SRC)
from geom import f, perch, squircle

# Regenerate with: python3 -m pip install fonttools && python3 assets/brand/src/build.py
# (needs rsvg-convert on PATH: brew install librsvg). Fonts are OFL, in ../fonts.
import shutil
OUT = os.path.dirname(SRC)
WORK = os.path.join(OUT, "work")
os.makedirs(WORK, exist_ok=True)
BR = OUT
PY = sys.executable
RSVG = shutil.which("rsvg-convert") or "rsvg-convert"
SPEC = json.load(open(os.path.join(SRC, "spec.json")))
C = {k: v["hex"] for k, v in SPEC["palette"].items()}
MK = SPEC["mark"]

# ---------------------------------------------------------------- helpers
def wrap(vbw, vbh, inner, title, w=None, h=None):
    size = f' width="{f(w)}" height="{f(h)}"' if w else ""
    return (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {f(vbw)} {f(vbh)}"{size}>'
            f'<title>{title}</title>{inner}</svg>\n')

def nest(x, y, w, h, vbw, vbh, inner, vbx=0, vby=0):
    return (f'<svg x="{f(x)}" y="{f(y)}" width="{f(w)}" height="{f(h)}" '
            f'viewBox="{f(vbx)} {f(vby)} {f(vbw)} {f(vbh)}">{inner}</svg>')

def render(svg, png, w, h=None):
    args = [RSVG, "-w", str(w)] + (["-h", str(h)] if h else []) + ["-o", png, svg]
    subprocess.run(args, check=True)

def write(name, svg):
    p = os.path.join(OUT, name)
    open(p, "w").write(svg)
    return p

# ---------------------------------------------------------------- mark (master)
def master_geom(cx, cy, s):
    return perch(cx, cy, s, MK["cornerR"] * s, MK["smoothing"], MK["discR"] * s, MK["ring"] * s)

def mark_inner(cx=256, cy=256, s=None, seat=C["midnight"], disc=C["ultra"]):
    s = s or MK["master512"]["s"]
    a, b, (x, y, r) = master_geom(cx, cy, s)
    return (f'<path d="{a}" fill="{seat}"/><path d="{b}" fill="{seat}"/>'
            f'<circle cx="{f(x)}" cy="{f(y)}" r="{f(r)}" fill="{disc}"/>')

def grid_inner(cfg, seat, disc, seat_op=1.0, disc_op=1.0, disc_hollow=None):
    g = cfg["grid"]; c = g / 2; s = cfg["s"]
    a, b, (x, y, r) = perch(c, c, s, cfg["cornerR"], cfg["smoothing"], cfg["discR"], cfg["ring"])
    so = f' fill-opacity="{f(seat_op)}"' if seat_op < 1 else ""
    do = f' fill-opacity="{f(disc_op)}"' if disc_op < 1 else ""
    if disc_hollow:
        sw = disc_hollow
        dsc = (f'<circle cx="{f(x)}" cy="{f(y)}" r="{f(r - sw / 2)}" fill="none" stroke="{disc}" '
               f'stroke-width="{f(sw)}"/>')
    else:
        dsc = f'<circle cx="{f(x)}" cy="{f(y)}" r="{f(r)}" fill="{disc}"{do}/>'
    return f'<path d="{a}" fill="{seat}"{so}/><path d="{b}" fill="{seat}"{so}/>{dsc}'

# ---------------------------------------------------------------- app icon
AI = SPEC["appicon"]
def appicon_inner(p="ai"):
    bx, by, bw, bh = AI["body"]
    body = squircle(bx, by, bw, bh, AI["bodyR"], AI["bodySmoothing"])
    s = AI["markWidth"] * bw / 2
    cx = cy = AI["canvas"] / 2
    a, b, (x, y, r) = master_geom(cx, cy, s)
    sh = AI["shadow"]
    defs = (
        f'<filter id="{p}-bs" x="-20%" y="-20%" width="140%" height="140%">'
        f'<feGaussianBlur stdDeviation="{sh["blur"]}"/></filter>'
        f'<filter id="{p}-ms" x="-30%" y="-30%" width="160%" height="160%">'
        f'<feGaussianBlur stdDeviation="14"/></filter>'
        f'<linearGradient id="{p}-bg" x1="0" y1="{by}" x2="0" y2="{by + bh}" gradientUnits="userSpaceOnUse">'
        f'<stop offset="0" stop-color="#2A2C66"/><stop offset=".55" stop-color="#15173A"/>'
        f'<stop offset="1" stop-color="#0A0B1C"/></linearGradient>'
        f'<radialGradient id="{p}-glow" cx="{f(x)}" cy="{f(y)}" r="{f(s * 1.25)}" gradientUnits="userSpaceOnUse">'
        f'<stop offset="0" stop-color="#5B4DFF" stop-opacity=".72"/>'
        f'<stop offset=".4" stop-color="#3B2DFF" stop-opacity=".26"/>'
        f'<stop offset="1" stop-color="#3B2DFF" stop-opacity="0"/></radialGradient>'
        # one gradient, objectBoundingBox: each seat gets the identical top-light ramp
        f'<linearGradient id="{p}-seat" x1="0" y1="0" x2="0" y2="1">'
        f'<stop offset="0" stop-color="#F2F3F9"/><stop offset="1" stop-color="#CACEE0"/></linearGradient>'
        # disc: colour travels along the diagonal (lower-left seat -> upper-right seat)
        f'<linearGradient id="{p}-disc" x1="{f(x - r * .75)}" y1="{f(y + r * .75)}" x2="{f(x + r * .75)}" '
        f'y2="{f(y - r * .75)}" gradientUnits="userSpaceOnUse">'
        f'<stop offset="0" stop-color="#3424FF"/><stop offset=".55" stop-color="#5A4CFF"/>'
        f'<stop offset="1" stop-color="#A89EFF"/></linearGradient>'
        f'<linearGradient id="{p}-rim" x1="0" y1="{by}" x2="0" y2="{by + bh}" gradientUnits="userSpaceOnUse">'
        f'<stop offset="0" stop-color="#FFFFFF" stop-opacity=".20"/>'
        f'<stop offset=".3" stop-color="#FFFFFF" stop-opacity="0"/></linearGradient>'
        f'<clipPath id="{p}-clip"><path d="{body}"/></clipPath>'
    )
    return (
        f'<defs>{defs}</defs>'
        f'<path d="{body}" fill="#000000" fill-opacity="{sh["opacity"]}" transform="translate(0 {sh["dy"]})" filter="url(#{p}-bs)"/>'
        f'<path d="{body}" fill="url(#{p}-bg)"/>'
        f'<g clip-path="url(#{p}-clip)">'
        f'<circle cx="{f(x)}" cy="{f(y)}" r="{f(s * 1.25)}" fill="url(#{p}-glow)"/>'
        f'<g transform="translate(0 12)" filter="url(#{p}-ms)" fill="#05061A" fill-opacity=".55">'
        f'<path d="{a}"/><path d="{b}"/></g>'
        f'<path d="{body}" fill="none" stroke="url(#{p}-rim)" stroke-width="6"/></g>'
        f'<path d="{a}" fill="url(#{p}-seat)"/><path d="{b}" fill="url(#{p}-seat)"/>'
        f'<circle cx="{f(x)}" cy="{f(y)}" r="{f(r)}" fill="url(#{p}-disc)"/>'
    )

# ---------------------------------------------------------------- wordmark + lockups
WMS = SPEC["wordmark"]
WORD = json.loads(subprocess.run(
    [PY, "-I", os.path.join(SRC, "wordmark.py"), os.path.join(BR, "fonts/Manrope.ttf"),
     str(WMS["weight"]), str(WMS["tracking"]), WMS["pairs"], "1"],
    capture_output=True, text=True, check=True, cwd=SRC).stdout)
XH = WORD["xHeight"]
WB = WORD["bbox"]                 # xmin, ymin(neg = up), xmax, ymax
ASC = -WB[1]                      # ascender top (d / l) above baseline
LK = SPEC["lockup"]

def lockup(seat, disc, word):
    mh = LK["markXH"] * XH        # mark height
    over = (mh - ASC) / 2         # equal optical overshoot above ascender / below baseline
    gap, pad = LK["gapXH"] * XH, LK["padXH"] * XH
    top = -ASC - over; bot = max(WB[3], over)
    ox = pad; oy = pad - top      # baseline y
    s = mh / 2
    mcx, mcy = ox + s, oy - ASC / 2
    tx = ox + mh + gap - WB[0]
    W = tx + WB[2] + pad
    H = (bot - top) + 2 * pad
    inner = (mark_inner(mcx, mcy, s, seat, disc)
             + f'<path transform="translate({f(tx)} {f(oy)})" d="{WORD["d"]}" fill="{word}"/>')
    return W, H, inner

# ---------------------------------------------------------------- write deliverables
MB, MB16, SM = SPEC["menubar"], SPEC["menubar16"], SPEC["small16"]
MARK_L = mark_inner()
MARK_D = mark_inner(seat=C["paper"], disc=C["ultraLight"])
SMALL_L = grid_inner(SM, C["midnight"], C["ultra"])
SMALL_D = grid_inner(SM, C["paper"], C["ultraLight"])
LW, LH, LI = lockup(C["midnight"], C["ultra"], C["midnight"])
DW, DH, DI = lockup(C["paper"], C["ultraLight"], C["paper"])
K = "#000000"
MB_STATES = {
    "menubar.svg":         grid_inner(MB, K, K),
    "menubar-syncing.svg": grid_inner(MB, K, K, disc_op=0.4),
    "menubar-offline.svg": grid_inner(MB, K, K, seat_op=0.35),
}

files = {}
files["mark"] = write("mark.svg", wrap(512, 512, MARK_L, "stradl mark"))
files["mark-dark"] = write("mark-dark.svg", wrap(512, 512, MARK_D, "stradl mark (dark surfaces)"))
files["mark-small"] = write("mark-small.svg", wrap(16, 16, SMALL_L, "stradl mark, 16/32 px master"))
files["mark-small-dark"] = write("mark-small-dark.svg", wrap(16, 16, SMALL_D, "stradl mark, 16/32 px master (dark)"))
files["appicon"] = write("appicon.svg", wrap(1024, 1024, appicon_inner(), "stradl app icon"))
files["lockup-light"] = write("lockup-light.svg", wrap(LW, LH, LI, "stradl"))
files["lockup-dark"] = write("lockup-dark.svg", wrap(DW, DH, DI, "stradl"))
for n, inner in MB_STATES.items():
    files[n[:-4]] = write(n, wrap(44, 44, inner, "stradl menu bar template"))
files["menubar-16"] = write("menubar-16.svg", wrap(32, 32, grid_inner(MB16, K, K), "stradl menu bar template, 16 pt"))

# ---------------------------------------------------------------- contact sheet
from textpath import text_d
def label(txt, x, y, size=12, color="#8A8D99", weight=560, track=0.6, font="Inter"):
    d, w = text_d(os.path.join(BR, f"fonts/{font}.ttf"), txt, weight, size, track)
    return f'<path transform="translate({f(x)} {f(y)})" d="{d}" fill="{color}"/>', w

def L(*a, **k):
    return label(*a, **k)[0]

def card(x, y, w, h, fill="#FFFFFF"):
    return f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="18" fill="{fill}"/>'

def faux_items(x, y, color):
    # rough stand-ins for neighbouring SF Symbols at true menu-bar scale (22 px row), full strength
    out = []
    out.append(f'<rect x="{x + .75}" y="{y + 6.75}" width="20" height="9.5" rx="2.6" fill="none" stroke="{color}" stroke-opacity=".45" stroke-width="1.2"/>')
    out.append(f'<rect x="{x + 2.5}" y="{y + 8.5}" width="13" height="6" rx="1.3" fill="{color}"/>')
    out.append(f'<rect x="{x + 22}" y="{y + 9.5}" width="1.4" height="3.5" rx=".7" fill="{color}" fill-opacity=".45"/>')
    wx, wy = x + 40, y + 16
    out.append(f'<path d="M{wx} {wy}l-1.9-1.9a2.7 2.7 0 0 1 3.8 0Z" fill="{color}"/>')
    out.append(f'<path d="M{wx - 4.3} {wy - 4.3}a6.1 6.1 0 0 1 8.6 0M{wx - 6.6} {wy - 6.6}a9.4 9.4 0 0 1 13.2 0" fill="none" stroke="{color}" stroke-width="1.7" stroke-linecap="round"/>')
    cx_, cy_ = x + 62, y + 11
    out.append(f'<rect x="{cx_ - 7}" y="{cy_ - 6}" width="14" height="5" rx="2.5" fill="none" stroke="{color}" stroke-width="1.4"/>'
               f'<circle cx="{cx_ - 4.5}" cy="{cy_ - 3.5}" r="1.3" fill="{color}"/>'
               f'<rect x="{cx_ - 7}" y="{cy_ + 1}" width="14" height="5" rx="2.5" fill="none" stroke="{color}" stroke-width="1.4"/>'
               f'<circle cx="{cx_ + 4.5}" cy="{cy_ + 3.5}" r="1.3" fill="{color}"/>')
    return "".join(out)

P = ['<rect width="1600" height="1000" fill="#EEEEF2"/>']

# A. app icon card --------------------------------------------------------
P.append(card(40, 40, 560, 920))
P.append(L("APP ICON · 512 / 128 / 64 / 32", 68, 74))
P.append(nest(64, 88, 512, 512, 1024, 1024, appicon_inner("i512")))
P.append(nest(64, 610, 128, 128, 1024, 1024, appicon_inner("i128")))
P.append(nest(214, 642, 64, 64, 1024, 1024, appicon_inner("i64")))
P.append(nest(300, 658, 32, 32, 1024, 1024, appicon_inner("i32")))
for t, xx in (("128", 116), ("64", 238), ("32", 309)):
    P.append(L(t, xx, 756, size=10, color="#A3A6B1"))
# Dock-context strip: icon at 64 among neutral neighbours
P.append(f'<rect x="372" y="626" width="204" height="96" rx="16" fill="#E9E9EE"/>')
for i in range(3):
    xx = 386 + i * 64
    if i == 1:
        P.append(nest(xx - 8, 642, 64, 64, 1024, 1024, appicon_inner("idock")))
    else:
        P.append(f'<rect x="{xx + 4}" y="652" width="44" height="44" rx="10" fill="#FFFFFF" stroke="#000000" stroke-opacity=".06"/>')
P.append(L("dock", 458, 756, size=10, color="#A3A6B1"))
P.append(f'<line x1="68" y1="782" x2="572" y2="782" stroke="#E6E7EC" stroke-width="1"/>')
# tagline lockup (primes the two-taken / two-empty reading)
tl_w = 240; tl_h = tl_w * LH / LW
P.append(nest(60, 796, tl_w, tl_h, LW, LH, LI))
P.append(L("Musical chairs for your Claude sessions.", 68 + 6, 796 + tl_h + 14, size=15, color="#4A4D5C", weight=500, track=0, font="Manrope"))
for i, (key, nm) in enumerate([("midnight", "Midnight"), ("paper", "Paper"), ("ultra", "Ultra"),
                               ("ultraLight", "Ultra Light"), ("mint", "Mint · status")]):
    xx = 68 + i * 102
    P.append(f'<rect x="{xx}" y="910" width="22" height="22" rx="6" fill="{C[key]}" stroke="#000000" stroke-opacity=".08"/>')
    P.append(L(nm, xx + 28, 920, size=9.5, color="#6B6E7B", weight=600, track=0.1))
    P.append(L(C[key], xx + 28, 932, size=9, color="#A3A6B1", weight=500, track=0.1))

# B/C. lockups ------------------------------------------------------------
P.append(card(620, 40, 460, 290))
P.append(L("LOCKUP · LIGHT", 648, 74))
lw = 360; lh = lw * LH / LW
P.append(nest(620 + (460 - lw) / 2, 98, lw, lh, LW, LH, LI))
sw = 160; shh = sw * LH / LW
P.append(nest(648, 262, sw, shh, LW, LH, LI))
P.append(L("160 px", 822, 262 + shh / 2 + 4, size=10, color="#A3A6B1"))
P.append(card(1100, 40, 460, 290, "#0B0B0F"))
P.append(L("LOCKUP · DARK", 1128, 74, color="#6E7180"))
P.append(nest(1100 + (460 - lw) / 2, 98, lw, lh, DW, DH, DI))
P.append(nest(1128, 262, sw, shh, DW, DH, DI))
P.append(L("160 px", 1302, 262 + shh / 2 + 4, size=10, color="#5E6170"))

# D. mark card -----------------------------------------------------------
P.append(card(620, 350, 940, 270))
P.append(L("MARK · 128 / 64 / 32", 648, 384))
P.append(L("SMALL MASTER · 24 / 16", 990, 384))
P.append(L("CONSTRUCTION", 1236, 384))
P.append(nest(648, 404, 128, 128, 512, 512, MARK_L))
P.append(nest(800, 436, 64, 64, 512, 512, MARK_L))
P.append(nest(888, 452, 32, 32, 512, 512, MARK_L))
P.append(nest(990, 456, 24, 24, 16, 16, SMALL_L))
P.append(nest(1030, 460, 16, 16, 16, 16, SMALL_L))
P.append(f'<rect x="648" y="548" width="884" height="52" rx="10" fill="#0B0B0F"/>')
P.append(nest(668, 558, 32, 32, 512, 512, MARK_D))
P.append(nest(714, 566, 16, 16, 16, 16, SMALL_D))
P.append(L("mark-dark", 742, 578, size=10, color="#6E7180"))
P.append(nest(990, 562, 24, 24, 16, 16, SMALL_D))
P.append(nest(1030, 566, 16, 16, 16, 16, SMALL_D))
P.append(L("mark-small-dark", 1058, 578, size=10, color="#6E7180"))
P.append(f'<rect x="1236" y="562" width="34" height="24" rx="6" fill="#FFFFFF" fill-opacity=".1"/>')
P.append(nest(1241, 562, 24, 24, 16, 16, SMALL_D))
P.append(L("favicon, dark tab", 1280, 578, size=10, color="#6E7180"))
# construction
cx0, cy0, cs = 1236, 400, 136
_k = cs / 512
P.append(nest(cx0, cy0, cs, cs, 512, 512, mark_inner(seat="#DEE0E8", disc="#C9C3FF")))
_ms = MK["master512"]["s"]; _rk = (MK["discR"] + MK["ring"]) * _ms
X_ = lambda v: f(cx0 + v * _k); Y_ = lambda v: f(cy0 + v * _k)
P.append(f'<g fill="none" stroke="{C["ultra"]}" stroke-width="1">'
         f'<line x1="{X_(48)}" y1="{Y_(256)}" x2="{X_(464)}" y2="{Y_(256)}" stroke-dasharray="3 3"/>'
         f'<line x1="{X_(256)}" y1="{Y_(48)}" x2="{X_(256)}" y2="{Y_(464)}" stroke-dasharray="3 3"/>'
         f'<circle cx="{X_(256)}" cy="{Y_(256)}" r="{f(_rk * _k)}" stroke-dasharray="2 2"/>'
         f'<circle cx="{X_(256)}" cy="{Y_(256)}" r="2.5" fill="{C["ultra"]}" stroke="none"/></g>')
for i, t in enumerate(["seat = s (app-icon corner)", "disc = 0.72 s", "ring = 0.075 s", "one seam, one disc"]):
    P.append(L(t, cx0 + cs + 12, cy0 + 34 + i * 22, size=10.5, color="#6B6E7B", weight=520, track=0.2))

# E. menu bar card ----------------------------------------------------------
P.append(card(620, 640, 940, 320))
P.append(L("MENU BAR TEMPLATE · 44 / 22 / 16", 648, 674))
P.append(L("STATES @22", 1150, 674))
for row, (bg, fg, lc) in enumerate([("#ECECEC", K, "#8A8D99"), ("#1E1E1E", "#FFFFFF", "#6E7180")]):
    y0 = 694 + row * 128
    P.append(f'<rect x="648" y="{y0}" width="884" height="112" rx="10" fill="{bg}"/>')
    P.append(nest(672, y0 + 30, 44, 44, 44, 44, grid_inner(MB, fg, fg)))
    P.append(L("@2x", 680, y0 + 92, size=10, color=lc))
    # true-size menu bar strip
    P.append(f'<rect x="740" y="{y0 + 41}" width="380" height="22" fill="{fg}" fill-opacity=".05"/>')
    P.append(faux_items(760, y0 + 41, fg))
    P.append(nest(850, y0 + 41, 22, 22, 44, 44, grid_inner(MB, fg, fg)))
    P.append(L("22", 855, y0 + 92, size=10, color=lc))
    P.append(nest(894, y0 + 44, 16, 16, 32, 32, grid_inner(MB16, fg, fg)))
    P.append(L("16", 896, y0 + 92, size=10, color=lc))
    P.append(L("next to system items", 930, y0 + 56, size=10, color=lc))
    # states
    for i, (inner, nm) in enumerate([(grid_inner(MB, fg, fg), "synced"),
                                     (grid_inner(MB, fg, fg, disc_op=0.4), "syncing"),
                                     (grid_inner(MB, fg, fg, seat_op=0.35), "offline")]):
        P.append(nest(1156 + i * 64, y0 + 41, 22, 22, 44, 44, inner))
        P.append(L(nm, 1150 + i * 64, y0 + 92, size=10, color=lc))
    P.append(f'<rect x="1340" y="{y0 + 39}" width="34" height="26" rx="6" fill="{fg}" fill-opacity=".16"/>')
    P.append(nest(1346, y0 + 41, 22, 22, 44, 44, grid_inner(MB, fg, fg)))
    P.append(L("selected", 1335, y0 + 92, size=10, color=lc))
    P.append(f'<circle cx="1420" cy="{y0 + 52}" r="5" fill="{C["mint"] if row == 0 else C["mintLight"]}"/>')
    P.append(L("in sync (app UI)", 1432, y0 + 56, size=10, color=lc))

write("sheet.svg", wrap(1600, 1000, "".join(P), "stradl identity sheet", 1600, 1000))

# ---------------------------------------------------------------- renders
O = lambda n: os.path.join(OUT, n)
render(O("sheet.svg"), O("preview.png"), 1600)
render(O("appicon.svg"), O("appicon-1024.png"), 1024)
render(O("appicon.svg"), O("appicon-512.png"), 512)
render(O("mark.svg"), O("mark-512.png"), 512)
render(O("mark-dark.svg"), O("mark-dark-512.png"), 512)
render(O("menubar.svg"), O("menubar-44.png"), 44, 44)
render(O("menubar.svg"), O("menubar-22.png"), 22, 22)
render(O("menubar-16.svg"), O("menubar-16.png"), 16, 16)
render(O("mark-small.svg"), O("mark-16.png"), 16, 16)
render(O("mark.svg"), O("mark-32.png"), 32, 32)          # 512 master lands on whole pixels at 32
# inspection-only renders
render(O("menubar.svg"), os.path.join(WORK, "menubar44svg-at16.png"), 16, 16)
print(json.dumps({"lockup": [round(LW, 1), round(LH, 1)], "xh": round(XH, 2), "asc": round(ASC, 2),
                  "mark_over_xh": LK["markXH"]}))
