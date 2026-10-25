// Package icon holds the menu bar glyph. The PNGs are template images
// (black on transparent; macOS recolors them for light and dark menu bars).
// They are placeholders drawn by gen.go until the final logo replaces them
// at the same paths.
package icon

import _ "embed"

//go:generate go run gen.go

// MenuBar is the 22x22 (1x) glyph.
//
//go:embed menubar.png
var MenuBar []byte

// MenuBar2x is the 44x44 (2x) glyph.
//
//go:embed menubar@2x.png
var MenuBar2x []byte
