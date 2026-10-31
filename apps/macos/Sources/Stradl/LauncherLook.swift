import SwiftUI
import AppKit

/// The per-account Claude launchers Stradl puts in ~/Applications
/// (`stradl profile launchers`): each icon is the app icon with the
/// account's name on a band of its color.
enum LauncherApps {
    /// Whether any launcher Stradl made is in ~/Applications (its
    /// Info.plist carries StradlLauncher).
    static func deployed() -> Bool {
        let dir = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Applications")
        guard let apps = try? FileManager.default.contentsOfDirectory(at: dir, includingPropertiesForKeys: nil) else { return false }
        for app in apps where app.pathExtension == "app" {
            let plist = app.appendingPathComponent("Contents/Info.plist")
            if let d = NSDictionary(contentsOf: plist), d["StradlLauncher"] != nil { return true }
        }
        return false
    }
}

/// The band colors offered (the same palette `stradl profile color` names).
let launcherPalette: [(name: String, hex: String)] = [
    ("Pink", "#FF3D9A"), ("Violet", "#8A5CFF"), ("Azure", "#1EC8FF"), ("Teal", "#19C39C"),
    ("Amber", "#FFB020"), ("Coral", "#FF6B4A"), ("Lime", "#7ED957"), ("Magenta", "#E040FB"),
]

extension Color {
    /// #RRGGBB; nil for anything else.
    init?(hex: String) {
        var s = hex.trimmingCharacters(in: .whitespaces)
        if s.hasPrefix("#") { s.removeFirst() }
        guard s.count == 6, let v = UInt32(s, radix: 16) else { return nil }
        self.init(.sRGB, red: Double(v >> 16 & 0xff) / 255, green: Double(v >> 8 & 0xff) / 255, blue: Double(v & 0xff) / 255)
    }

    /// #RRGGBB in sRGB.
    var hexString: String {
        let ns = NSColor(self).usingColorSpace(.sRGB) ?? .black
        return String(format: "#%02X%02X%02X", Int((ns.redComponent * 255).rounded()),
                      Int((ns.greenComponent * 255).rounded()), Int((ns.blueComponent * 255).rounded()))
    }
}

/// A small picture of an account's launcher icon: the app icon with a band
/// of its color.
struct LauncherPreview: View {
    var color: Color
    var size: CGFloat = 30

    var body: some View {
        ZStack(alignment: .bottom) {
            Image(nsImage: NSApp.applicationIconImage)
                .resizable()
                .interpolation(.high)
                .frame(width: size, height: size)
            Capsule()
                .fill(color)
                .overlay(Capsule().stroke(.white.opacity(0.4), lineWidth: 0.5))
                .frame(width: size * 0.7, height: size * 0.18)
                .padding(.bottom, size * 0.17)
        }
        .frame(width: size, height: size)
        .accessibilityHidden(true)
    }
}

/// Picks a launcher color: the palette, or any color.
struct LauncherColorPicker: View {
    var current: String?
    var choose: (String) -> Void
    @State private var custom: Color = .pink

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Launcher color").font(.headline)
            LazyVGrid(columns: Array(repeating: GridItem(.fixed(28), spacing: 8), count: 4), spacing: 8) {
                ForEach(launcherPalette, id: \.hex) { c in
                    Button { choose(c.hex) } label: {
                        Circle()
                            .fill(Color(hex: c.hex) ?? .gray)
                            .frame(width: 24, height: 24)
                            .overlay(Circle().stroke(Color.primary, lineWidth: current?.uppercased() == c.hex ? 2 : 0).padding(-3))
                    }
                    .buttonStyle(.plain)
                    .help(c.name)
                }
            }
            HStack {
                ColorPicker("Other…", selection: $custom, supportsOpacity: false)
                Button("Use") { choose(custom.hexString) }
            }
            .controlSize(.small)
            Text("Its Claude launcher in Applications shows the account's name on this color.")
                .font(.caption)
                .foregroundStyle(.secondary)
                .frame(width: 200, alignment: .leading)
                .fixedSize(horizontal: false, vertical: true)
        }
        .padding(14)
        .onAppear { if let c = current.flatMap(Color.init(hex:)) { custom = c } }
    }
}
