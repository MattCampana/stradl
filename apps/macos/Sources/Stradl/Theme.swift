import SwiftUI

/// Stradl's look: the Unison icon's night ink and its three voices (pink,
/// violet, azure) on dark glass. Every view takes its colors from here.
enum Theme {
    // Ground, from deepest to raised.
    static let ink0 = Color(hex: "#07051A")!
    static let ink1 = Color(hex: "#0C0925")!
    static let ink2 = Color(hex: "#141036")!
    static let ink3 = Color(hex: "#1D1750")!

    static let line = Color.white.opacity(0.08)
    static let lineStrong = Color.white.opacity(0.14)
    static let text = Color.white.opacity(0.94)
    static let text2 = Color.white.opacity(0.64)
    static let text3 = Color.white.opacity(0.42)

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

/// The window's ground: night ink lit by the voices from the edges.
struct ThemeBackground: View {
    var body: some View {
        ZStack {
            Theme.ink0
            RadialGradient(colors: [Theme.violet.opacity(0.24), .clear], center: .topTrailing, startRadius: 0, endRadius: 720)
            RadialGradient(colors: [Theme.azure.opacity(0.10), .clear], center: .bottomLeading, startRadius: 0, endRadius: 620)
            RadialGradient(colors: [Theme.pink.opacity(0.07), .clear], center: .top, startRadius: 0, endRadius: 520)
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
            .background(Theme.ink2.opacity(0.78), in: RoundedRectangle(cornerRadius: radius, style: .continuous))
            .overlay(
                RoundedRectangle(cornerRadius: radius, style: .continuous)
                    .strokeBorder(LinearGradient(colors: [Color.white.opacity(0.13), Color.white.opacity(0.04)], startPoint: .top, endPoint: .bottom), lineWidth: 1)
            )
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
            .background(Color.white.opacity(configuration.isPressed ? 0.14 : 0.07), in: Capsule())
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
                    .fill(configuration.isOn ? AnyShapeStyle(Theme.voices) : AnyShapeStyle(Color.white.opacity(0.12)))
                    .overlay(Capsule().strokeBorder(Color.white.opacity(configuration.isOn ? 0.25 : 0.14), lineWidth: 1))
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
