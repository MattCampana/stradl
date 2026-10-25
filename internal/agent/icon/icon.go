// Package icon holds the menu bar glyph: the Stradl "perch" mark (two seats,
// one session disc on the seam) as macOS template images, black on
// transparent so macOS recolors them for light and dark menu bars. They are
// rendered from assets/brand/menubar.svg by assets/brand/src/build.py.
package icon

import _ "embed"

// MenuBar is the 22x22 (1x) glyph.
//
//go:embed menubar.png
var MenuBar []byte

// MenuBar2x is the 44x44 (2x) glyph.
//
//go:embed menubar@2x.png
var MenuBar2x []byte
