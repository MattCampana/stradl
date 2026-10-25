"""Convert a string to outlined SVG path data using an OFL font.

usage: text2path.py FONT.ttf TEXT --weight 600 --size 100 --tracking 0 [--x 0 --y 0]

Prints JSON: {"d": "<path data>", "width": W, "ascent": A, "capHeight": C,
"xHeight": X, "bbox": [xmin, ymin, xmax, ymax]} in output units where the
baseline sits at --y and text starts at --x. Tracking is in output units
added between glyphs. No kerning (fine for short lowercase wordmarks;
adjust pairs by hand with --pair "ad:-4,dl:2").
"""
import argparse, json
from fontTools.ttLib import TTFont
from fontTools.varLib import instancer
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.pens.boundsPen import BoundsPen

ap = argparse.ArgumentParser()
ap.add_argument("font"); ap.add_argument("text")
ap.add_argument("--weight", type=float, default=600)
ap.add_argument("--size", type=float, default=100)
ap.add_argument("--tracking", type=float, default=0)
ap.add_argument("--x", type=float, default=0); ap.add_argument("--y", type=float, default=0)
ap.add_argument("--pair", default="")
a = ap.parse_args()

font = TTFont(a.font)
if "fvar" in font:
    axes = {ax.axisTag: ax for ax in font["fvar"].axes}
    loc = {}
    if "wght" in axes:
        loc["wght"] = max(axes["wght"].minValue, min(axes["wght"].maxValue, a.weight))
    if "opsz" in axes:
        loc["opsz"] = axes["opsz"].maxValue if a.size >= 48 else axes["opsz"].defaultValue
    font = instancer.instantiateVariableFont(font, loc)

upm = font["head"].unitsPerEm
scale = a.size / upm
cmap = font.getBestCmap()
gs = font.getGlyphSet()
hmtx = font["hmtx"]
os2 = font["OS/2"]
pairs = {}
for p in filter(None, a.pair.split(",")):
    k, v = p.split(":"); pairs[k] = float(v)

pen = SVGPathPen(gs)
bpen = BoundsPen(gs)
x = a.x
prev = None
for ch in a.text:
    if prev is not None:
        x += a.tracking + pairs.get(prev + ch, 0)
    g = cmap[ord(ch)]
    t = (scale, 0, 0, -scale, x, a.y)
    gs[g].draw(TransformPen(pen, t))
    gs[g].draw(TransformPen(bpen, t))
    x += hmtx[g][0] * scale
    prev = ch

print(json.dumps({
    "d": pen.getCommands(),
    "width": x - a.x,
    "ascent": font["hhea"].ascent * scale,
    "capHeight": getattr(os2, "sCapHeight", 0) * scale,
    "xHeight": getattr(os2, "sxHeight", 0) * scale,
    "bbox": bpen.bounds,
}))
