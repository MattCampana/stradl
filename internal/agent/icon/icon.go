// Package icon holds the menu bar glyph: the Unison mark (three voices, one
// per Mac, falling into one wave) as macOS template images, black on
// transparent so macOS recolors them for light and dark menu bars. They are
// rendered from assets/brand/unison/glyph.svg by assets/brand/unison/render.sh.
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
