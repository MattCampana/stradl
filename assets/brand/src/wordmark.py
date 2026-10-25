"""Outlined 'stradl' wordmark from Manrope (OFL) with stradl-specific edits:
squared (horizontal) s terminals and hand-set pair spacing.
usage: wordmark.py FONT.ttf WEIGHT TRACKING PAIRS SQUARE(0/1)  -> JSON"""
import sys, json
from fontTools.ttLib import TTFont
from fontTools.varLib import instancer
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.pens.boundsPen import BoundsPen
from fontTools.pens.recordingPen import RecordingPen

fontp, weight, tracking, pairs_s, square = sys.argv[1], float(sys.argv[2]), float(sys.argv[3]), sys.argv[4], sys.argv[5] == "1"
font = instancer.instantiateVariableFont(TTFont(fontp), {"wght": weight})
upm = font["head"].unitsPerEm
glyf = font["glyf"]; cmap = font.getBestCmap(); hmtx = font["hmtx"]
pairs = {}
for p in filter(None, pairs_s.split(",")):
    k, v = p.split(":"); pairs[k] = float(v)

def nearest(coords, x, y):
    return min(range(len(coords)), key=lambda i: (coords[i][0]-x)**2 + (coords[i][1]-y)**2)

if square:
    g = glyf[cmap[ord("s")]]
    c = g.coordinates
    # locate terminal cut points by proximity to their default positions
    def flat(pa, pb, ha, hb):
        ia, ib = nearest(c, *pa), nearest(c, *pb)
        y = (c[ia][1] + c[ib][1]) / 2
        da, db = y - c[ia][1], y - c[ib][1]
        iha, ihb = nearest(c, *ha), nearest(c, *hb)
        c[ia] = (c[ia][0], y); c[iha] = (c[iha][0], c[iha][1] + da)
        c[ib] = (c[ib][0], y); c[ihb] = (c[ihb][0], c[ihb][1] + db)
    flat((80, 304), (300, 339), (107, 146), (320, 255))     # lower-left terminal
    flat((987, 792), (766, 752), (974, 888), (756, 828))    # upper-right terminal
    g.coordinates = c
    g.recalcBounds(glyf)

text = "stradl"; size = 100.0; scale = size / upm
gs = font.getGlyphSet()
pen = SVGPathPen(None); bpen = BoundsPen(None)
x = 0.0; prev = None; adv = []
for ch in text:
    if prev is not None:
        x += tracking + pairs.get(prev + ch, 0)
    gn = cmap[ord(ch)]
    t = (scale, 0, 0, -scale, x, 0)
    rec = RecordingPen(); glyf[gn].draw(rec, glyf)
    rec.replay(TransformPen(pen, t)); rec.replay(TransformPen(bpen, t))
    x += hmtx[gn][0] * scale; prev = ch
os2 = font["OS/2"]
print(json.dumps({"d": pen.getCommands(), "width": x, "bbox": bpen.bounds,
                  "xHeight": os2.sxHeight * scale, "capHeight": os2.sCapHeight * scale}))
