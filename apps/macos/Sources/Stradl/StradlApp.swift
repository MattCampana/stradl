import SwiftUI

@main
struct StradlApp: App {
    @StateObject private var model = AppModel()

    var body: some Scene {
        WindowGroup("Stradl") {
            ContentView()
                .environmentObject(model)
                .frame(minWidth: 860, minHeight: 560)
        }
        .windowToolbarStyle(.unified)
        .commands {
            CommandGroup(replacing: .newItem) {}
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
        case .folders: return "folder"
        case .activity: return "arrow.triangle.2.circlepath"
        case .settings: return "gearshape"
        case .logs: return "doc.text.magnifyingglass"
        }
    }
}

struct ContentView: View {
    @EnvironmentObject var model: AppModel
    @State private var section: Pane? = .overview

    var body: some View {
        NavigationSplitView {
            List(Pane.allCases, selection: $section) { s in
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
                    switch section ?? .overview {
                    case .overview: OverviewView(section: $section)
                    case .circle: CircleView()
                    case .sessions: SessionsView()
                    case .folders: FoldersView()
                    case .activity: ActivityView()
                    case .settings: ScrollView { SettingsForm().padding(24) }
                    case .logs: LogsView()
                    }
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
            .navigationTitle(section?.rawValue ?? "Stradl")
            .toolbar {
                ToolbarItemGroup(placement: .primaryAction) {
                    Button { model.syncNow() } label: {
                        Label("Sync Now", systemImage: "arrow.triangle.2.circlepath")
                    }
                    .help("Sync with your other Macs now (⌘R)")
                    .disabled(model.busy.contains("sync") || !model.agentRunning)
                    Button { model.togglePause() } label: {
                        Label(model.paused ? "Resume" : "Pause", systemImage: model.paused ? "play.fill" : "pause.fill")
                    }
                    .help(model.paused ? "Resume syncing" : "Pause syncing until you resume")
                    .disabled(!model.agentRunning)
                }
            }
        }
    }

    private func badge(for s: Pane) -> Int {
        switch s {
        case .circle: return model.overview?.devices.count ?? 0
        case .activity: return model.overview?.pending.total ?? 0
        default: return 0
        }
    }
}

struct SidebarFooter: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        HStack(spacing: 8) {
            StatusDot(health: model.health)
            Text(model.statusLine)
                .font(.caption)
                .foregroundStyle(.secondary)
                .lineLimit(2)
            Spacer(minLength: 0)
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
        VStack(spacing: 14) {
            Image(systemName: "moon.zzz")
                .font(.system(size: 40))
                .foregroundStyle(.secondary)
            Text("Stradl isn't running")
                .font(.title2.weight(.semibold))
            Text(model.overviewError ?? "Start the background agent to sync and see your Macs.")
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
                .frame(maxWidth: 420)
            Button("Start Stradl") { model.startAgent() }
                .buttonStyle(.borderedProminent)
                .disabled(model.busy.contains("agent"))
        }
        .padding(40)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}
