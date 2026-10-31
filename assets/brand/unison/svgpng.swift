// svgpng <in.svg> <out.png> <canvas px> [inset px]
// Renders an SVG with WebKit (full SVG: filters, blur) onto a transparent
// canvas and writes a PNG with alpha.
import Cocoa
import WebKit

let a = CommandLine.arguments
guard a.count >= 4 else { fatalError("usage: svgpng in.svg out.png size [inset]") }
let size = Int(a[3])!
let inset = a.count > 4 ? Int(a[4]) ?? 0 : 0
let svg = try! String(contentsOfFile: a[1], encoding: .utf8)
let out = a[2]
let inner = size - 2 * inset
let b64 = Data(svg.utf8).base64EncodedString()
let html = """
<html><head><style>html,body{margin:0;padding:0;background:transparent;overflow:hidden}
img{position:absolute;left:\(inset)px;top:\(inset)px;width:\(inner)px;height:\(inner)px}</style></head>
<body><img src="data:image/svg+xml;base64,\(b64)"></body></html>
"""

final class R: NSObject, WKNavigationDelegate {
    let view: WKWebView
    init(_ v: WKWebView) { view = v }
    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.4) {
            let cfg = WKSnapshotConfiguration()
            cfg.rect = CGRect(x: 0, y: 0, width: size, height: size)
            cfg.snapshotWidth = NSNumber(value: size)
            webView.takeSnapshot(with: cfg) { img, err in
                guard let img, let tiff = img.tiffRepresentation, let rep = NSBitmapImageRep(data: tiff) else {
                    FileHandle.standardError.write("snapshot failed: \(String(describing: err))\n".data(using: .utf8)!); exit(1)
                }
                // Exactly size x size pixels.
                let px = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: size, pixelsHigh: size, bitsPerSample: 8,
                                          samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
                NSGraphicsContext.saveGraphicsState()
                NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: px)
                rep.draw(in: NSRect(x: 0, y: 0, width: size, height: size))
                NSGraphicsContext.restoreGraphicsState()
                try! px.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: out))
                exit(0)
            }
        }
    }
}

let app = NSApplication.shared
app.setActivationPolicy(.prohibited)
let v = WKWebView(frame: NSRect(x: 0, y: 0, width: size, height: size))
v.setValue(false, forKey: "drawsBackground")
let r = R(v)
v.navigationDelegate = r
v.loadHTMLString(html, baseURL: nil)
DispatchQueue.main.asyncAfter(deadline: .now() + 20) { FileHandle.standardError.write("timeout\n".data(using: .utf8)!); exit(2) }
app.run()
