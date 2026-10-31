import SwiftUI

@main
struct StradlApp: App {
    @StateObject private var model = AppModel()

    init() {
        // Always open the window at launch: macOS's window restoration can
        // replay a saved "no windows" state (after a crash, a forced quit or
        // quitting with the window closed), which leaves the app running
        // with nothing on screen.
        UserDefaults.standard.set(true, forKey: "ApplePersistenceIgnoreState")
    }

    var body: some Scene {
        // One window, always opened at launch. (A WindowGroup restores its
        // windows by the content's type, and silently opens none after an
        // update changes that type.)
        Window("Stradl", id: "main") {
            ThemedRoot {
                ContentView()
                    .environmentObject(model)
            }
            .frame(minWidth: 900, minHeight: 600)
            .task { await Snapshots.runIfAsked(model) }
        }
        .windowToolbarStyle(.unifiedCompact)
        .commands {
            CommandGroup(after: .sidebar) {
                Picker("Appearance", selection: Binding(
                    get: { UserDefaults.standard.string(forKey: "appearance") ?? Appearance.dark.rawValue },
                    set: { UserDefaults.standard.set($0, forKey: "appearance") })) {
                    ForEach(Appearance.allCases) { a in Text(a.title).tag(a.rawValue) }
                }
            }
            CommandMenu("Sync") {
                Button("Sync Now") { model.syncNow() }
                    .keyboardShortcut("r", modifiers: [.command])
                Button(model.paused ? "Resume Syncing" : "Pause Syncing") { model.togglePause() }
                    .keyboardShortcut("p", modifiers: [.command, .shift])
                Divider()
                Button("Restart Claude") { model.restartClaude() }
                Button("Open Dashboard in Browser") { model.openDashboard() }
                Divider()
                Button("Copy Diagnostics") { model.copyDiagnostics() }
            }
        }

        Settings {
            ThemedRoot {
                SettingsView()
                    .environmentObject(model)
            }
            .frame(width: 560, height: 620)
        }
    }
}

/// The sections in the sidebar.
enum Pane: String, CaseIterable, Identifiable {
    case overview = "Overview"
    case circle = "Circle"
    case sessions = "Sessions"
    case accounts = "Accounts"
    case folders = "Folders"
    case activity = "Next Sync"
    case settings = "Settings"
    case logs = "Logs"

    var id: String { rawValue }

    var symbol: String {
        switch self {
        case .overview: return "gauge.with.dots.needle.33percent"
        case .circle: return "person.3.sequence"
        case .sessions: return "bubble.left.and.text.bubble.right"
        case .accounts: return "person.crop.circle.badge.checkmark"
        case .folders: return "folder"
        case .activity: return "arrow.triangle.2.circlepath"
        case .settings: return "gearshape"
        case .logs: return "doc.text.magnifyingglass"
        }
    }
}

struct ContentView: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        NavigationSplitView {
            Sidebar()
                .navigationSplitViewColumnWidth(min: 210, ideal: 228, max: 280)
        } detail: {
            ZStack {
                ThemeBackground()
                VStack(spacing: 0) {
                    if let banner = model.message {
                        BannerView(banner: banner) { model.message = nil }
                            .padding(.horizontal, 24)
                            .padding(.top, 14)
                    }
                    PaneHeader(pane: model.pane ?? .overview)
                    Group {
                        switch model.pane ?? .overview {
                        case .overview: OverviewView(section: $model.pane)
                        case .circle: CircleView()
                        case .sessions: SessionsView()
                        case .accounts: AccountsView()
                        case .folders: FoldersView()
                        case .activity: ActivityView()
                        case .settings: ScrollView { SettingsForm().padding(24) }
                        case .logs: LogsView()
                        }
                    }
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                    .scrollContentBackground(.hidden)
                    // Every plain button and switch in the panes, themed;
                    // a button that names its own style keeps it.
                    .buttonStyle(.ghost)
                    .toggleStyle(.themeSwitch)
                }
            }
            .foregroundStyle(Theme.text)
            .navigationTitle("")
            .toolbarBackground(Theme.ink0, for: .windowToolbar)
            .toolbarBackground(.visible, for: .windowToolbar)
            .toolbar {
                ToolbarItemGroup(placement: .primaryAction) {
                    Button { model.syncNow() } label: {
                        Label {
                            Text("Sync Now")
                        } icon: {
                            Image(systemName: "arrow.triangle.2.circlepath").spinning(model.progress.syncing)
                        }
                    }
                    .help(model.progress.syncing ? "Syncing now" : "Sync with your other Macs now (⌘R)")
                    .disabled(model.busy.contains("sync") || model.progress.syncing || !model.agentRunning)
                    Button { model.togglePause() } label: {
                        Label(model.paused ? "Resume" : "Pause", systemImage: model.paused ? "play.fill" : "pause.fill")
                    }
                    .help(model.paused ? "Resume syncing" : "Pause syncing until you resume")
                    .disabled(!model.agentRunning)
                }
            }
        }
        // A Claude account new to the Circle (just signed in to): name it
        // and choose whether it syncs before anything else.
        .sheet(item: Binding(get: { model.newAccount }, set: { _ in })) { account in
            // One sheet per account: when the next new account takes this
            // one's place, its nickname field starts empty, not with the
            // name typed for the last one.
            NewAccountSheet(account: account).environmentObject(model).id(account.id)
        }
    }
}

/// The section list: the mark and name, a colored tile per section with room
/// around it, the selected one lit in the voices' gradient, and the status at
/// the bottom.
struct Sidebar: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 10) {
                Image(nsImage: NSApp.applicationIconImage)
                    .resizable()
                    .interpolation(.high)
                    .frame(width: 34, height: 34)
                VStack(alignment: .leading, spacing: 0) {
                    Text("stradl")
                        .font(.system(size: 20, weight: .bold, design: .rounded))
                        .foregroundStyle(Theme.text)
                    Text("Musical chairs for Claude")
                        .font(.system(size: 10.5, weight: .medium, design: .rounded))
                        .foregroundStyle(Theme.text3)
                }
            }
            .padding(.horizontal, 18)
            .padding(.top, 14)
            .padding(.bottom, 18)

            ScrollView {
                VStack(spacing: 4) {
                    ForEach(Pane.allCases) { pane in
                        SidebarRow(pane: pane, selected: (model.pane ?? .overview) == pane, badge: badge(for: pane)) {
                            model.pane = pane
                        }
                    }
                }
                .padding(.horizontal, 10)
            }
            Spacer(minLength: 0)
            SidebarFooter()
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        .background(
            LinearGradient(colors: [Theme.ink1, Theme.ink0], startPoint: .top, endPoint: .bottom)
                .overlay(alignment: .trailing) { Rectangle().fill(Theme.line).frame(width: 1) }
                .ignoresSafeArea()
        )
    }

    private func badge(for s: Pane) -> Int {
        switch s {
        case .circle: return model.overview?.devices.count ?? 0
        case .activity: return model.overview?.pending.total ?? 0
        case .accounts: return model.accounts.filter(\.undecided).count
        default: return 0
        }
    }
}

struct SidebarRow: View {
    var pane: Pane
    var selected: Bool
    var badge: Int
    var action: () -> Void
    @State private var hovering = false

    var body: some View {
        Button(action: action) {
            HStack(spacing: 12) {
                IconTile(symbol: pane.symbol, color: pane.color, size: 28, filled: selected)
                Text(pane.rawValue)
                    .font(.system(size: 13.5, weight: selected ? .semibold : .medium, design: .rounded))
                    .foregroundStyle(selected ? Theme.text : Theme.text2)
                Spacer(minLength: 4)
                if badge > 0 {
                    Text("\(badge)")
                        .font(.system(size: 11, weight: .bold, design: .rounded))
                        .monospacedDigit()
                        .foregroundStyle(selected ? Color.white : pane.color)
                        .padding(.horizontal, 7)
                        .padding(.vertical, 2)
                        .background((selected ? Color.white.opacity(0.18) : pane.color.opacity(0.16)), in: Capsule())
                }
            }
            .padding(.horizontal, 10)
            .padding(.vertical, 7)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background {
                RoundedRectangle(cornerRadius: 12, style: .continuous)
                    .fill(selected ? AnyShapeStyle(LinearGradient(colors: [pane.color.opacity(0.30), pane.color.opacity(0.08)], startPoint: .leading, endPoint: .trailing))
                                   : AnyShapeStyle(hovering ? Theme.hover : Color.clear))
            }
            .overlay {
                if selected {
                    RoundedRectangle(cornerRadius: 12, style: .continuous)
                        .strokeBorder(pane.color.opacity(0.35), lineWidth: 1)
                }
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .onHover { hovering = $0 }
        .accessibilityAddTraits(selected ? .isSelected : [])
    }
}

/// The section's title, big, with its tile and one line about it.
struct PaneHeader: View {
    var pane: Pane

    var body: some View {
        HStack(spacing: 14) {
            IconTile(symbol: pane.symbol, color: pane.color, size: 40, filled: true)
            VStack(alignment: .leading, spacing: 2) {
                Text(pane.rawValue)
                    .font(.system(size: 26, weight: .bold, design: .rounded))
                    .foregroundStyle(Theme.text)
                Text(pane.subtitle)
                    .font(.system(size: 13, weight: .medium, design: .rounded))
                    .foregroundStyle(Theme.text2)
            }
            Spacer()
        }
        .padding(.horizontal, 24)
        .padding(.top, 18)
        .padding(.bottom, 6)
    }
}

struct SidebarFooter: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 10) {
                if model.progress.syncing {
                    Image(systemName: "arrow.triangle.2.circlepath")
                        .font(.caption.weight(.bold))
                        .foregroundStyle(Theme.azure)
                        .spinning(true)
                } else {
                    StatusDot(health: model.health)
                }
                Text(model.statusLine)
                    .font(.system(size: 11.5, weight: .medium, design: .rounded))
                    .foregroundStyle(Theme.text2)
                    .lineLimit(2)
                    .monospacedDigit()
                Spacer(minLength: 0)
            }
            if model.progress.syncing, let f = model.progress.fraction {
                GradientBar(value: f)
            }
        }
        .padding(12)
        .themePanel(radius: 14)
        .padding(12)
    }
}

/// A thin progress bar in the voices' gradient.
struct GradientBar: View {
    var value: Double

    var body: some View {
        GeometryReader { geo in
            ZStack(alignment: .leading) {
                Capsule().fill(Theme.fillStrong)
                Capsule().fill(Theme.voices)
                    .frame(width: max(6, geo.size.width * min(max(value, 0), 1)))
                    .shadow(color: Theme.violet.opacity(0.6), radius: 4)
            }
        }
        .frame(height: 5)
        .animation(.easeOut(duration: 0.4), value: value)
    }
}

struct StatusDot: View {
    var health: AppModel.Health

    var body: some View {
        Circle()
            .fill(color)
            .frame(width: 9, height: 9)
            .shadow(color: color.opacity(0.8), radius: 4)
            .accessibilityLabel(label)
    }

    private var color: Color {
        switch health {
        case .good: return Theme.ok
        case .working: return Theme.azure
        case .attention: return Theme.warn
        case .off: return Theme.text3
        }
    }

    private var label: String {
        switch health {
        case .good: return "In sync"
        case .working: return "Syncing"
        case .attention: return "Needs attention"
        case .off: return "Not syncing"
        }
    }
}

struct BannerView: View {
    var banner: AppModel.Banner
    var dismiss: () -> Void

    var body: some View {
        let color = banner.isError ? Theme.warn : Theme.ok
        HStack(alignment: .top, spacing: 12) {
            IconTile(symbol: banner.isError ? "exclamationmark.triangle.fill" : "checkmark", color: color, size: 26)
            Text(banner.text)
                .font(.system(size: 13, weight: .medium, design: .rounded))
                .foregroundStyle(Theme.text)
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.top, 4)
            Button(action: dismiss) { Image(systemName: "xmark").foregroundStyle(Theme.text2) }
                .buttonStyle(.plain)
                .padding(.top, 5)
                .help("Dismiss")
        }
        .padding(12)
        .background(color.opacity(0.10), in: RoundedRectangle(cornerRadius: 14, style: .continuous))
        .overlay(RoundedRectangle(cornerRadius: 14, style: .continuous).strokeBorder(color.opacity(0.35), lineWidth: 1))
        .task(id: banner.id) {
            guard !banner.isError else { return }
            try? await Task.sleep(nanoseconds: 6_000_000_000)
            dismiss()
        }
    }
}

/// A glass card used across the app, with an optional title and icon tile.
struct Card<Content: View>: View {
    var title: String?
    var systemImage: String?
    var color: Color = Theme.violet
    @ViewBuilder var content: Content

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            if let title {
                HStack(spacing: 10) {
                    if let systemImage {
                        IconTile(symbol: systemImage, color: color, size: 26)
                    }
                    Text(title)
                        .font(.system(size: 15, weight: .semibold, design: .rounded))
                        .foregroundStyle(Theme.text)
                }
            }
            content
        }
        .padding(18)
        .frame(maxWidth: .infinity, alignment: .leading)
        .themePanel()
    }
}

/// Shown where a view needs the agent and it isn't running.
struct AgentOffline: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        // The agent can be running but slow to answer (its first overview
        // reads every transcript): that is not the same as stopped.
        let running = model.config?.agentRunning ?? false
        VStack(spacing: 16) {
            IconTile(symbol: running ? "hourglass" : "moon.zzz.fill", color: running ? Theme.azure : Theme.violet, size: 64, filled: true)
            Text(running ? "Stradl is busy" : "Stradl isn't running")
                .font(.system(size: 22, weight: .bold, design: .rounded))
            Text(running
                 ? "It didn't answer in time, usually because it's in the middle of a big sync. \(model.overviewError ?? "")"
                 : "Start the background agent to sync and see your Macs.")
                .foregroundStyle(Theme.text2)
                .multilineTextAlignment(.center)
                .frame(maxWidth: 440)
            if running {
                Button("Try Again") { Task { await model.refreshAll() } }
                    .buttonStyle(.ghost)
            } else {
                Button("Start Stradl") { model.startAgent() }
                    .buttonStyle(.primary)
                    .disabled(model.busy.contains("agent"))
            }
        }
        .padding(40)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}
