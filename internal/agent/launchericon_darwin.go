//go:build darwin && cgo

package agent

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>
#include <stdlib.h>
#include <string.h>

// stradlBadgeIcon draws base (a PNG) at 1024 px with label on a rounded band
// of color r,g,b across its lower part, and returns the result as a PNG
// (malloc'd; NULL on failure).
static void *stradlBadgeIcon(const void *base, int baseLen, const char *label,
                             double r, double g, double b, int *outLen) {
	@autoreleasepool {
		NSImage *img = [[NSImage alloc] initWithData:[NSData dataWithBytes:base length:baseLen]];
		if (img == nil) return NULL;
		const NSInteger S = 1024;
		NSBitmapImageRep *rep = [[NSBitmapImageRep alloc]
			initWithBitmapDataPlanes:NULL pixelsWide:S pixelsHigh:S bitsPerSample:8 samplesPerPixel:4
			hasAlpha:YES isPlanar:NO colorSpaceName:NSDeviceRGBColorSpace bytesPerRow:0 bitsPerPixel:0];
		if (rep == nil) return NULL;
		rep.size = NSMakeSize(S, S);
		NSGraphicsContext *ctx = [NSGraphicsContext graphicsContextWithBitmapImageRep:rep];
		if (ctx == nil) return NULL;
		[NSGraphicsContext saveGraphicsState];
		[NSGraphicsContext setCurrentContext:ctx];
		[img drawInRect:NSMakeRect(0, 0, S, S) fromRect:NSZeroRect
		      operation:NSCompositingOperationSourceOver fraction:1.0];

		// The band sits inside the icon body's lower edge (the body spans
		// 100-924; its corners curve in below about 740 from the top).
		NSRect band = NSMakeRect(150, 172, 724, 184);
		NSBezierPath *pill = [NSBezierPath bezierPathWithRoundedRect:band xRadius:92 yRadius:92];
		NSColor *fill = [NSColor colorWithSRGBRed:r green:g blue:b alpha:1];
		[NSGraphicsContext saveGraphicsState];
		NSShadow *shadow = [[NSShadow alloc] init];
		shadow.shadowColor = [NSColor colorWithWhite:0 alpha:0.5];
		shadow.shadowBlurRadius = 28;
		shadow.shadowOffset = NSMakeSize(0, -10);
		[shadow set];
		[fill setFill];
		[pill fill];
		[NSGraphicsContext restoreGraphicsState];
		NSGradient *sheen = [[NSGradient alloc]
			initWithStartingColor:[NSColor colorWithWhite:1 alpha:0.30]
			          endingColor:[NSColor colorWithWhite:1 alpha:0.0]];
		[sheen drawInBezierPath:pill angle:-90];
		[[NSColor colorWithWhite:1 alpha:0.35] setStroke];
		pill.lineWidth = 3;
		[pill stroke];

		// White text, or ink on a light band.
		double lum = 0.2126 * r + 0.7152 * g + 0.0722 * b;
		NSColor *ink = lum > 0.62 ? [NSColor colorWithSRGBRed:0.07 green:0.05 blue:0.21 alpha:1]
		                          : [NSColor whiteColor];
		NSString *text = [NSString stringWithUTF8String:label];
		if (text == nil) text = @"";
		CGFloat maxWidth = band.size.width - 96;
		CGFloat size = 116;
		NSDictionary *attrs = nil;
		NSSize ts = NSZeroSize;
		for (;;) {
			NSFont *font = [NSFont systemFontOfSize:size weight:NSFontWeightHeavy];
			attrs = @{NSFontAttributeName: font, NSForegroundColorAttributeName: ink};
			ts = [text sizeWithAttributes:attrs];
			if (ts.width <= maxWidth || size <= 60) break;
			size -= 4;
		}
		if (ts.width > maxWidth) {
			// Still too long at the smallest size: cut it short, whole
			// characters at a time, and end it with an ellipsis.
			NSString *cut = text;
			while (cut.length > 1) {
				cut = [cut substringToIndex:[cut rangeOfComposedCharacterSequenceAtIndex:cut.length - 1].location];
				NSString *shown = [[cut stringByTrimmingCharactersInSet:NSCharacterSet.whitespaceCharacterSet]
				                   stringByAppendingString:@"…"];
				ts = [shown sizeWithAttributes:attrs];
				if (ts.width <= maxWidth) {
					text = shown;
					break;
				}
			}
		}
		NSFont *font = attrs[NSFontAttributeName];
		CGFloat baseline = NSMidY(band) - (font.ascender + font.descender) / 2;
		[text drawAtPoint:NSMakePoint(NSMidX(band) - ts.width / 2, baseline + font.descender)
		   withAttributes:attrs];
		[NSGraphicsContext restoreGraphicsState];

		NSData *png = [rep representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
		if (png == nil) return NULL;
		void *out = malloc(png.length);
		if (out == NULL) return NULL;
		memcpy(out, png.bytes, png.length);
		*outLen = (int)png.length;
		return out;
	}
}
*/
import "C"

import (
	"errors"
	"strconv"
	"unsafe"
)

func init() { BadgeIcon = badgeIcon }

func badgeIcon(base []byte, label, color string) ([]byte, error) {
	if len(base) == 0 {
		return nil, errors.New("no base icon")
	}
	r, g, b, err := rgb(color)
	if err != nil {
		return nil, err
	}
	cLabel := C.CString(label)
	defer C.free(unsafe.Pointer(cLabel))
	var n C.int
	out := C.stradlBadgeIcon(unsafe.Pointer(&base[0]), C.int(len(base)), cLabel, C.double(r), C.double(g), C.double(b), &n)
	if out == nil {
		return nil, errors.New("couldn't draw the launcher icon")
	}
	defer C.free(out)
	return C.GoBytes(out, n), nil
}

// rgb reads #RRGGBB as three 0-1 components.
func rgb(hex string) (r, g, b float64, err error) {
	if len(hex) != 7 || hex[0] != '#' {
		return 0, 0, 0, errors.New("color must be #RRGGBB")
	}
	v, err := strconv.ParseUint(hex[1:], 16, 32)
	if err != nil {
		return 0, 0, 0, err
	}
	return float64(v>>16&0xff) / 255, float64(v>>8&0xff) / 255, float64(v&0xff) / 255, nil
}
