import SwiftUI

/// The three looks: night ink, graphite, and soft white. Each keeps the
/// Unison voices as its accents.
enum Appearance: String, CaseIterable, Identifiable {
    case dark, grey, light
    var id: String { rawValue }
    var title: String {
        switch self {
        case .dark: return "Dark"
        case .grey: return "Grey"
        case .light: return "Light"
        }
    }
}

/// One look's neutrals.
struct Palette {
    var ink0, ink1, ink2, ink3: Color // ground, sidebar, panels, raised
    var text, text2, text3: Color
    var line, lineStrong: Color
    var fill, fillStrong, hover: Color // quiet fills: ghost buttons, off switches, hovered rows
    var edgeTop, edgeBottom: Color     // a panel's border, lit from above
    var panelOpacity: Double
    var glow: Double                   // how strongly the voices light the ground
    var scheme: ColorScheme
}

/// Stradl's look: the Unison icon's three voices (pink, violet, azure) over
/// the chosen ground. Every view takes its colors from here.
enum Theme {
    /// The look in use; ThemedRoot sets it from the stored choice and rebuilds
    /// the views when it changes.
    static var appearance: Appearance = .dark

    static let palettes: [Appearance: Palette] = [
        .dark: Palette(
            ink0: Color(hex: "#07051A")!, ink1: Color(hex: "#0C0925")!, ink2: Color(hex: "#141036")!, ink3: Color(hex: "#1D1750")!,
            text: .white.opacity(0.94), text2: .white.opacity(0.64), text3: .white.opacity(0.42),
            line: .white.opacity(0.08), lineStrong: .white.opacity(0.14),
            fill: .white.opacity(0.07), fillStrong: .white.opacity(0.12), hover: .white.opacity(0.05),
            edgeTop: .white.opacity(0.13), edgeBottom: .white.opacity(0.04),
            panelOpacity: 0.78, glow: 1, scheme: .dark),
        .grey: Palette(
            ink0: Color(hex: "#202227")!, ink1: Color(hex: "#26292F")!, ink2: Color(hex: "#2E3138")!, ink3: Color(hex: "#383C45")!,
            text: .white.opacity(0.94), text2: .white.opacity(0.66), text3: .white.opacity(0.44),
            line: .white.opacity(0.09), lineStrong: .white.opacity(0.16),
            fill: .white.opacity(0.08), fillStrong: .white.opacity(0.14), hover: .white.opacity(0.06),
            edgeTop: .white.opacity(0.14), edgeBottom: .white.opacity(0.05),
            panelOpacity: 0.86, glow: 0.55, scheme: .dark),
        .light: Palette(
            ink0: Color(hex: "#F4F3FA")!, ink1: Color(hex: "#FBFAFF")!, ink2: Color(hex: "#FFFFFF")!, ink3: Color(hex: "#ECEAF6")!,
            text: Color(hex: "#15122B")!.opacity(0.94), text2: Color(hex: "#15122B")!.opacity(0.62), text3: Color(hex: "#15122B")!.opacity(0.42),
            line: Color(hex: "#15122B")!.opacity(0.08), lineStrong: Color(hex: "#15122B")!.opacity(0.14),
            fill: Color(hex: "#15122B")!.opacity(0.05), fillStrong: Color(hex: "#15122B")!.opacity(0.12), hover: Color(hex: "#15122B")!.opacity(0.05),
            edgeTop: Color(hex: "#15122B")!.opacity(0.10), edgeBottom: Color(hex: "#15122B")!.opacity(0.06),
            panelOpacity: 0.86, glow: 0.45, scheme: .light),
    ]

    static var p: Palette { palettes[appearance] ?? palettes[.dark]! }

    // Ground, from deepest to raised.
    static var ink0: Color { p.ink0 }
    static var ink1: Color { p.ink1 }
    static var ink2: Color { p.ink2 }
    static var ink3: Color { p.ink3 }

    static var line: Color { p.line }
    static var lineStrong: Color { p.lineStrong }
    static var text: Color { p.text }
    static var text2: Color { p.text2 }
    static var text3: Color { p.text3 }
    static var fill: Color { p.fill }
    static var fillStrong: Color { p.fillStrong }
    static var hover: Color { p.hover }

    // The three voices, and the rest of the palette.
    static let pink = Color(hex: "#FF3D9A")!
    static let violet = Color(hex: "#8A5CFF")!
    static let azure = Color(hex: "#1EC8FF")!
    static let teal = Color(hex: "#19C39C")!
    static let amber = Color(hex: "#FFB020")!
    static let coral = Color(hex: "#FF6B4A")!
    static let magenta = Color(hex: "#E040FB")!
    static let slate = Color(hex: "#8FA3D9")!

    // State.
    static let ok = Color(hex: "#2EE6A6")!
    static let warn = Color(hex: "#FFB020")!
    static let bad = Color(hex: "#FF5C7A")!

    /// The voices side by side: primary buttons, the selected section.
    static let voices = LinearGradient(colors: [pink, violet, azure], startPoint: .leading, endPoint: .trailing)

    static let radius: CGFloat = 18
}

/// Applies the chosen look (stored as "appearance") to everything under it,
/// rebuilding the views when the choice changes.
struct ThemedRoot<Content: View>: View {
    @AppStorage("appearance") private var appearance = Appearance.dark.rawValue
    @ViewBuilder var content: Content

    var body: some View {
        let look = Appearance(rawValue: appearance) ?? .dark
        Theme.appearance = look
        return content
            .id(look)
            .preferredColorScheme(Theme.p.scheme)
            .tint(Theme.violet)
    }
}

/// The window's ground: night ink lit by the voices from the edges.
struct ThemeBackground: View {
    var body: some View {
        ZStack {
            Theme.ink0
            RadialGradient(colors: [Theme.violet.opacity(0.24 * Theme.p.glow), .clear], center: .topTrailing, startRadius: 0, endRadius: 720)
            RadialGradient(colors: [Theme.azure.opacity(0.10 * Theme.p.glow), .clear], center: .bottomLeading, startRadius: 0, endRadius: 620)
            RadialGradient(colors: [Theme.pink.opacity(0.07 * Theme.p.glow), .clear], center: .top, startRadius: 0, endRadius: 520)
        }
        .ignoresSafeArea()
    }
}

/// A rounded tile holding a symbol in a color: section and card icons.
struct IconTile: View {
    var symbol: String
    var color: Color
    var size: CGFloat = 28
    var filled = false

    var body: some View {
        ZStack {
            RoundedRectangle(cornerRadius: size * 0.3, style: .continuous)
                .fill(filled ? AnyShapeStyle(LinearGradient(colors: [color, color.opacity(0.72)], startPoint: .top, endPoint: .bottom))
                             : AnyShapeStyle(color.opacity(0.16)))
            RoundedRectangle(cornerRadius: size * 0.3, style: .continuous)
                .strokeBorder(color.opacity(filled ? 0 : 0.34), lineWidth: 1)
            Image(systemName: symbol)
                .font(.system(size: size * 0.48, weight: .semibold))
                .foregroundStyle(filled ? Color.white : color)
        }
        .frame(width: size, height: size)
        .shadow(color: filled ? color.opacity(0.45) : .clear, radius: 8, y: 2)
        .accessibilityHidden(true)
    }
}

extension View {
    /// A glass panel on the night ground.
    func themePanel(radius: CGFloat = Theme.radius, padding: CGFloat = 0) -> some View {
        self
            .padding(padding)
            .background(Theme.ink2.opacity(Theme.p.panelOpacity), in: RoundedRectangle(cornerRadius: radius, style: .continuous))
            .overlay(
                RoundedRectangle(cornerRadius: radius, style: .continuous)
                    .strokeBorder(LinearGradient(colors: [Theme.p.edgeTop, Theme.p.edgeBottom], startPoint: .top, endPoint: .bottom), lineWidth: 1)
            )
            .shadow(color: .black.opacity(Theme.p.scheme == .light ? 0.06 : 0), radius: 10, y: 3)
    }
}

/// The main action: a capsule in the voices' gradient.
struct PrimaryButtonStyle: ButtonStyle {
    @Environment(\.isEnabled) private var enabled
    @Environment(\.controlSize) private var controlSize

    func makeBody(configuration: Configuration) -> some View {
        let small = controlSize == .small || controlSize == .mini
        configuration.label
            .font(.system(size: small ? 12 : 13, weight: .semibold, design: .rounded))
            .foregroundStyle(.white)
            .padding(.horizontal, small ? 12 : 16)
            .padding(.vertical, small ? 5 : 8)
            .background(Theme.voices, in: Capsule())
            .overlay(Capsule().strokeBorder(Color.white.opacity(0.22), lineWidth: 1))
            .shadow(color: Theme.violet.opacity(enabled ? 0.45 : 0), radius: 10, y: 3)
            .opacity(enabled ? (configuration.isPressed ? 0.85 : 1) : 0.4)
            .scaleEffect(configuration.isPressed ? 0.97 : 1)
            .animation(.easeOut(duration: 0.12), value: configuration.isPressed)
    }
}

/// Everything else: a quiet glass capsule.
struct GhostButtonStyle: ButtonStyle {
    @Environment(\.isEnabled) private var enabled
    @Environment(\.controlSize) private var controlSize

    func makeBody(configuration: Configuration) -> some View {
        let small = controlSize == .small || controlSize == .mini
        configuration.label
            .font(.system(size: small ? 12 : 13, weight: .medium, design: .rounded))
            .foregroundStyle(Theme.text)
            .padding(.horizontal, small ? 11 : 14)
            .padding(.vertical, small ? 5 : 7)
            .background(configuration.isPressed ? Theme.fillStrong : Theme.fill, in: Capsule())
            .overlay(Capsule().strokeBorder(Theme.lineStrong, lineWidth: 1))
            .opacity(enabled ? 1 : 0.4)
    }
}

extension ButtonStyle where Self == PrimaryButtonStyle {
    static var primary: PrimaryButtonStyle { PrimaryButtonStyle() }
}

extension ButtonStyle where Self == GhostButtonStyle {
    static var ghost: GhostButtonStyle { GhostButtonStyle() }
}

/// A small status capsule: a glowing dot and words.
struct StatusPill: View {
    var color: Color
    var text: String

    var body: some View {
        HStack(spacing: 6) {
            Circle().fill(color).frame(width: 7, height: 7).shadow(color: color.opacity(0.8), radius: 4)
            Text(text)
        }
        .font(.system(size: 11, weight: .semibold, design: .rounded))
        .foregroundStyle(color)
        .padding(.horizontal, 8)
        .padding(.vertical, 3)
        .background(color.opacity(0.14), in: Capsule())
        .overlay(Capsule().strokeBorder(color.opacity(0.3), lineWidth: 1))
    }
}

extension Pane {
    /// Each section's own color, from the palette.
    var color: Color {
        switch self {
        case .overview: return Theme.azure
        case .circle: return Theme.violet
        case .sessions: return Theme.pink
        case .accounts: return Theme.magenta
        case .folders: return Theme.teal
        case .activity: return Theme.amber
        case .settings: return Theme.slate
        case .logs: return Theme.coral
        }
    }

    /// One line under the section's title.
    var subtitle: String {
        switch self {
        case .overview: return "Your sync at a glance"
        case .circle: return "Every Mac syncing with you"
        case .sessions: return "Every Claude session on this Mac"
        case .accounts: return "Your Claude accounts and their launchers"
        case .folders: return "Project folders that sync"
        case .activity: return "What the next sync will do"
        case .settings: return "What syncs, and how"
        case .logs: return "What the agent has been doing"
        }
    }
}

/// A switch in the voices' gradient when on.
struct ThemeSwitchStyle: ToggleStyle {
    @Environment(\.isEnabled) private var enabled

    func makeBody(configuration: Configuration) -> some View {
        HStack(spacing: 10) {
            ZStack(alignment: configuration.isOn ? .trailing : .leading) {
                Capsule()
                    .fill(configuration.isOn ? AnyShapeStyle(Theme.voices) : AnyShapeStyle(Theme.fillStrong))
                    .overlay(Capsule().strokeBorder(configuration.isOn ? Color.white.opacity(0.25) : Theme.lineStrong, lineWidth: 1))
                    .shadow(color: configuration.isOn ? Theme.violet.opacity(0.45) : .clear, radius: 6)
                Circle()
                    .fill(.white)
                    .shadow(color: .black.opacity(0.3), radius: 2, y: 1)
                    .padding(3)
            }
            .frame(width: 40, height: 23)
            .animation(.spring(response: 0.25, dampingFraction: 0.8), value: configuration.isOn)
            configuration.label
                .font(.system(size: 13, weight: .medium, design: .rounded))
                .foregroundStyle(Theme.text)
        }
        .contentShape(Rectangle())
        .onTapGesture { if enabled { configuration.isOn.toggle() } }
        .opacity(enabled ? 1 : 0.4)
        .accessibilityElement(children: .combine)
        .accessibilityAddTraits(.isButton)
        .accessibilityValue(configuration.isOn ? "On" : "Off")
    }
}

extension ToggleStyle where Self == ThemeSwitchStyle {
    static var themeSwitch: ThemeSwitchStyle { ThemeSwitchStyle() }
}

/// A text link in azure.
struct ThemeLinkStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.system(size: 13, weight: .semibold, design: .rounded))
            .foregroundStyle(Theme.azure.opacity(configuration.isPressed ? 0.7 : 1))
    }
}

extension ButtonStyle where Self == ThemeLinkStyle {
    static var themeLink: ThemeLinkStyle { ThemeLinkStyle() }
}
