"""In-process outlined text for sheet labels (cached font instances)."""
from functools import lru_cache
from fontTools.ttLib import TTFont
from fontTools.varLib import instancer
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen

@lru_cache(maxsize=None)
def _font(path, weight):
    f = TTFont(path)
    if "fvar" in f:
        axes = {a.axisTag: a for a in f["fvar"].axes}
        loc = {}
        if "wght" in axes:
            loc["wght"] = max(axes["wght"].minValue, min(axes["wght"].maxValue, weight))
        if "opsz" in axes:
            loc["opsz"] = axes["opsz"].defaultValue
        f = instancer.instantiateVariableFont(f, loc)
    return f

def text_d(path, text, weight, size, tracking=0.0):
    f = _font(path, weight)
    scale = size / f["head"].unitsPerEm
    cmap = f.getBestCmap(); gs = f.getGlyphSet(); hmtx = f["hmtx"]
    pen = SVGPathPen(gs); x = 0.0
    for i, ch in enumerate(text):
        if i: x += tracking
        g = cmap.get(ord(ch), cmap[ord("?")])
        gs[g].draw(TransformPen(pen, (scale, 0, 0, -scale, x, 0)))
        x += hmtx[g][0] * scale
    return pen.getCommands(), x
