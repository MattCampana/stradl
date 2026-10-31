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
            ContentView()
                .environmentObject(model)
                .frame(minWidth: 860, minHeight: 560)
                .task { await Snapshots.runIfAsked(model) }
        }
        .windowToolbarStyle(.unified)
        .commands {
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
            SettingsView()
                .environmentObject(model)
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
            List(Pane.allCases, selection: $model.pane) { s in
                Label(s.rawValue, systemImage: s.symbol)
                    .badge(badge(for: s))
                    .tag(s)
            }
            .navigationSplitViewColumnWidth(min: 170, ideal: 190)
            .safeAreaInset(edge: .bottom) { SidebarFooter() }
        } detail: {
            VStack(spacing: 0) {
                if let banner = model.message {
                    BannerView(banner: banner) { model.message = nil }
                }
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
            }
            .navigationTitle(model.pane?.rawValue ?? "Stradl")
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
            NewAccountSheet(account: account).environmentObject(model)
        }
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

struct SidebarFooter: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 8) {
                if model.progress.syncing {
                    Image(systemName: "arrow.triangle.2.circlepath")
                        .font(.caption.weight(.semibold))
                        .foregroundStyle(.blue)
                        .spinning(true)
                } else {
                    StatusDot(health: model.health)
                }
                Text(model.statusLine)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(2)
                    .monospacedDigit()
                Spacer(minLength: 0)
            }
            if model.progress.syncing, let f = model.progress.fraction {
                ProgressView(value: f).progressViewStyle(.linear).controlSize(.small)
            }
        }
        .padding(12)
    }
}

struct StatusDot: View {
    var health: AppModel.Health

    var body: some View {
        Circle()
            .fill(color)
            .frame(width: 9, height: 9)
            .overlay(Circle().stroke(.black.opacity(0.12), lineWidth: 0.5))
            .accessibilityLabel(label)
    }

    private var color: Color {
        switch health {
        case .good: return .green
        case .working: return .blue
        case .attention: return .orange
        case .off: return .gray
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
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: banner.isError ? "exclamationmark.triangle.fill" : "checkmark.circle.fill")
                .foregroundStyle(banner.isError ? .orange : .green)
            Text(banner.text)
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
            Button(action: dismiss) { Image(systemName: "xmark") }
                .buttonStyle(.borderless)
                .help("Dismiss")
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 10)
        .background(banner.isError ? Color.orange.opacity(0.12) : Color.green.opacity(0.10))
        .task(id: banner.id) {
            guard !banner.isError else { return }
            try? await Task.sleep(nanoseconds: 6_000_000_000)
            dismiss()
        }
    }
}

/// A rounded card used across the app.
struct Card<Content: View>: View {
    var title: String?
    var systemImage: String?
    @ViewBuilder var content: Content

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            if let title {
                Label(title, systemImage: systemImage ?? "")
                    .labelStyle(.titleAndIcon)
                    .font(.headline)
            }
            content
        }
        .padding(16)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(.background, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
        .overlay(RoundedRectangle(cornerRadius: 12, style: .continuous).stroke(.separator, lineWidth: 0.5))
    }
}

/// Shown where a view needs the agent and it isn't running.
struct AgentOffline: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        // The agent can be running but slow to answer (its first overview
        // reads every transcript): that is not the same as stopped.
        let running = model.config?.agentRunning ?? false
        VStack(spacing: 14) {
            Image(systemName: running ? "hourglass" : "moon.zzz")
                .font(.system(size: 40))
                .foregroundStyle(.secondary)
            Text(running ? "Stradl is busy" : "Stradl isn't running")
                .font(.title2.weight(.semibold))
            Text(running
                 ? "It didn't answer in time, usually because it's in the middle of a big sync. \(model.overviewError ?? "")"
                 : "Start the background agent to sync and see your Macs.")
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
                .frame(maxWidth: 440)
            if running {
                Button("Try Again") { Task { await model.refreshAll() } }
            } else {
                Button("Start Stradl") { model.startAgent() }
                    .buttonStyle(.borderedProminent)
                    .disabled(model.busy.contains("agent"))
            }
        }
        .padding(40)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}
