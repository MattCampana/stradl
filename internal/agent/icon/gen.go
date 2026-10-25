//go:build ignore

// gen draws the placeholder menu bar glyph: two chairs facing each other
// under a two-headed arc (sessions trading seats between Macs). It writes
// black-on-transparent template images at 1x and 2x.
//
//	go generate ./internal/agent/icon
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
)

// The design lives on a 22-unit grid, the menu bar's point size.
const grid = 22.0

type shape func(x, y float64) bool

func roundRect(x0, y0, x1, y1, r float64) shape {
	return func(x, y float64) bool {
		dx := math.Max(math.Max(x0+r-x, x-(x1-r)), 0)
		dy := math.Max(math.Max(y0+r-y, y-(y1-r)), 0)
		return x >= x0 && x <= x1 && y >= y0 && y <= y1 && dx*dx+dy*dy <= r*r
	}
}

// arc is a stroke of width w along the circle (cx, cy, R) between angles a0
// and a1 (degrees, counterclockwise from +x, y up).
func arc(cx, cy, R, w, a0, a1 float64) shape {
	return func(x, y float64) bool {
		dx, dy := x-cx, cy-y
		d := math.Hypot(dx, dy)
		if math.Abs(d-R) > w/2 {
			return false
		}
		a := math.Atan2(dy, dx) * 180 / math.Pi
		return a >= a0 && a <= a1
	}
}

func triangle(ax, ay, bx, by, cx, cy float64) shape {
	side := func(px, py, qx, qy, x, y float64) float64 { return (qx-px)*(y-py) - (qy-py)*(x-px) }
	return func(x, y float64) bool {
		d1 := side(ax, ay, bx, by, x, y)
		d2 := side(bx, by, cx, cy, x, y)
		d3 := side(cx, cy, ax, ay, x, y)
		neg := d1 < 0 || d2 < 0 || d3 < 0
		pos := d1 > 0 || d2 > 0 || d3 > 0
		return !(neg && pos)
	}
}

// arrowHead returns a triangle whose base is centered at the arc point for
// angle a (degrees), pointing along the direction of travel (dx, dy).
func arrowHead(cx, cy, R, a, dirSign, length, half float64) shape {
	t := a * math.Pi / 180
	px, py := cx+R*math.Cos(t), cy-R*math.Sin(t)
	// Tangent in image coordinates for increasing angle is (-sin, -cos).
	dx, dy := -math.Sin(t)*dirSign, -math.Cos(t)*dirSign
	nx, ny := -dy, dx
	bx, by := px-dx*length*0.25, py-dy*length*0.25
	return triangle(
		bx+dx*length, by+dy*length,
		bx+nx*half, by+ny*half,
		bx-nx*half, by-ny*half,
	)
}

func glyph() []shape {
	const (
		cx, cy, R = 11.0, 9.9, 5.9
		stroke    = 1.7
		a0, a1    = 40.0, 140.0
		bar       = 2.1 // chair bar thickness
		rad       = 1.05
		top, seat = 9.2, 13.6 // chair back top and seat top
		bottom    = 19.2
	)
	var s []shape
	// Left chair faces right: backrest, seat, front leg.
	s = append(s,
		roundRect(2.2, top, 2.2+bar, bottom, rad),
		roundRect(2.2, seat, 9.2, seat+bar, rad),
		roundRect(9.2-bar, seat, 9.2, bottom, rad),
	)
	// Right chair mirrors it.
	s = append(s,
		roundRect(grid-2.2-bar, top, grid-2.2, bottom, rad),
		roundRect(grid-9.2, seat, grid-2.2, seat+bar, rad),
		roundRect(grid-9.2, seat, grid-9.2+bar, bottom, rad),
	)
	// Arc over the top with a head at each end: the seats swap.
	s = append(s,
		arc(cx, cy, R, stroke, a0+4, a1-4),
		arrowHead(cx, cy, R, a0+2, -1, 3.1, 2.1),
		arrowHead(cx, cy, R, a1-2, +1, 3.1, 2.1),
	)
	return s
}

func render(size int) *image.NRGBA {
	const ss = 8 // supersamples per axis
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	shapes := glyph()
	scale := grid / float64(size)
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			hit := 0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) * scale
					y := (float64(py) + (float64(sy)+0.5)/ss) * scale
					for _, f := range shapes {
						if f(x, y) {
							hit++
							break
						}
					}
				}
			}
			a := uint8(math.Round(float64(hit) * 255 / (ss * ss)))
			img.SetNRGBA(px, py, color.NRGBA{0, 0, 0, a})
		}
	}
	return img
}

func write(name string, img image.Image) {
	f, err := os.Create(name)
	if err != nil {
		log.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
}

func main() {
	write("menubar.png", render(22))
	write("menubar@2x.png", render(44))
	if len(os.Args) > 1 && os.Args[1] == "-preview" {
		write("preview.png", render(352))
	}
}
