import AppKit
import Foundation

/// The app's state: what the agent reports, the settings, the folders, and
/// the actions people take. Views read it; it refreshes itself on a timer.
@MainActor
final class AppModel: ObservableObject {
    @Published var overview: Overview?
    @Published var overviewError: String?
    @Published var config: AppConfig?
    @Published var configError: String?
    @Published var folders: [FolderStat] = []
    @Published var accounts: [ClaudeAccount] = []
    @Published var heldSessions: [HeldSession] = []
    @Published var busy: Set<String> = []     // actions in flight, by name
    @Published var message: Banner?           // last action's outcome
    @Published var lastRefresh: Date?
    @Published var pane: Pane? = .overview      // the sidebar selection
    @Published var progress: SyncProgress = .idle // the agent's running sync

    struct Banner: Identifiable, Equatable {
        var id = UUID()
        var text: String
        var isError: Bool
    }

    private var timer: Timer?
    private var progressTimer: Timer?

    init() {
        Task { await refreshAll() }
        timer = Timer.scheduledTimer(withTimeInterval: 10, repeats: true) { [weak self] _ in
            Task { @MainActor in
                await self?.refreshOverview()
                await self?.refreshAccounts() // a new sign-in shows up within seconds
            }
        }
        // Progress is cheap to ask for: once a second, so the bar moves.
        progressTimer = Timer.scheduledTimer(withTimeInterval: 1, repeats: true) { [weak self] _ in
            Task { @MainActor in await self?.refreshProgress() }
        }
    }

    func refreshProgress() async {
        let was = progress.syncing
        progress = (try? await AgentAPI.progress()) ?? .idle
        if was != progress.syncing {
            // A sync started or finished: show its effect now, not in 10 s.
            await refreshOverview()
            if !progress.syncing { await refreshFolders() }
        }
    }

    // MARK: refresh

    func refreshAll() async {
        async let o: Void = refreshOverview()
        async let c: Void = refreshConfig()
        async let f: Void = refreshFolders()
        async let a: Void = refreshAccounts()
        _ = await (o, c, f, a)
    }

    func refreshOverview() async {
        do {
            overview = try await AgentAPI.overview()
            overviewError = nil
        } catch {
            overviewError = error.localizedDescription
            // The agent is gone (quit, stopped): drop what it last said rather
            // than keep showing "Synced" in green. A slow answer keeps it.
            let stopped = AgentAPI.endpoint() == nil ||
                [URLError.cannotConnectToHost, .networkConnectionLost, .cannotFindHost].contains((error as? URLError)?.code)
            if stopped {
                overview = nil
                await refreshConfig()
            }
        }
        lastRefresh = Date()
    }

    func refreshConfig() async {
        do {
            config = try await StradlCLI.json(AppConfig.self, ["config", "show", "--json"])
            configError = nil
        } catch {
            configError = error.localizedDescription
        }
    }

    func refreshFolders() async {
        folders = (try? await StradlCLI.json([FolderStat].self, ["folders", "--json"])) ?? folders
    }

    /// This Mac's copy of the Circle's accounts (no network); with fromStorage,
    /// read the Circle's list from storage first.
    func refreshAccounts(fromStorage: Bool = false) async {
        let args = fromStorage ? ["accounts", "--json", "--refresh"] : ["accounts", "--json"]
        accounts = (try? await StradlCLI.json([ClaudeAccount].self, args, timeout: fromStorage ? 90 : 30)) ?? accounts
        heldSessions = (try? await StradlCLI.json([HeldSession].self, ["accounts", "held", "--json"])) ?? heldSessions
    }

    /// Gives every waiting session an account.
    func assignHeld(to account: ClaudeAccount) {
        let n = heldSessions.count
        perform("assign", success: "\(n.plural("session")) now belong to \(account.name)") {
            try await StradlCLI.run(["accounts", "assign", account.id, "--held"], timeout: 90)
            try? await AgentAPI.syncNow()
        }
    }

    // MARK: derived

    var agentRunning: Bool { overview?.agent.running ?? (config?.agentRunning ?? false) }
    var paused: Bool { overview?.agent.paused ?? (config?.paused ?? false) }

    /// The one-line status at the top of the window.
    var statusLine: String {
        guard let o = overview else {
            if config?.agentInstalled == false { return "Not running: start Stradl below" }
            return overviewError == nil ? "Connecting…" : "The Stradl agent isn't running"
        }
        let a = o.agent
        if progress.syncing || a.syncing {
            if progress.total > 0 && progress.checking { return "Checking files… \(progress.done) of \(progress.total)" }
            if progress.total > 0 { return "Syncing… \(progress.done) of \(progress.total.plural("file"))" }
            return "Syncing…"
        }
        if a.paused { return "Paused" }
        if let summary = a.lastErrorSummary, !summary.isEmpty { return summary }
        if let last = a.lastSyncAt { return "Synced \(last.ago) · \(o.totals.sessions.plural("session"))" }
        return "Starting…"
    }

    enum Health { case good, working, attention, off }
    var health: Health {
        guard let a = overview?.agent else { return .off }
        if a.syncing || progress.syncing { return .working }
        if a.paused { return .off }
        if let e = a.lastError, !e.isEmpty { return .attention }
        return .good
    }

    // MARK: actions

    /// Runs an action, marks it busy meanwhile, reports failure in the banner.
    func perform(_ name: String, success: String? = nil, refresh: Bool = true, _ body: @escaping () async throws -> Void) {
        guard !busy.contains(name) else { return }
        busy.insert(name)
        Task {
            defer { busy.remove(name) }
            do {
                try await body()
                if let success { message = Banner(text: success, isError: false) }
            } catch {
                message = Banner(text: error.localizedDescription, isError: true)
            }
            if refresh { await refreshAll() }
        }
    }

    func syncNow() {
        perform("sync", success: "Syncing with your other Macs") {
            do {
                try await AgentAPI.syncNow()
            } catch {
                try await StradlCLI.run(["sync"], timeout: 600)
            }
        }
    }

    func togglePause() {
        let resume = paused
        perform("pause", success: resume ? "Syncing resumed" : "Syncing paused") {
            try await StradlCLI.run(["agent", resume ? "resume" : "pause"])
        }
    }

    func startAgent() {
        perform("agent", success: "Stradl starts at login and is syncing now") {
            try await StradlCLI.run(["agent", "install"], timeout: 180)
        }
    }

    func stopAgent() {
        perform("agent", success: "Stradl stopped and won't start at login") {
            try await StradlCLI.run(["agent", "uninstall"], timeout: 60)
        }
    }

    func update() {
        perform("update", success: "Stradl is up to date") {
            try await StradlCLI.run(["update"], timeout: 300)
        }
    }

    func restartClaude() {
        perform("claude", success: "Claude restarted with everything from your other Macs") {
            try await StradlCLI.run(["claude", "restart"], timeout: 240)
        }
    }

    func setConfig(_ key: String, _ value: String) {
        perform("config:\(key)") {
            try await StradlCLI.run(["config", "set", key, value])
        }
    }

    func editList(_ key: String, add: Bool, _ value: String) {
        perform("config:\(key)") {
            try await StradlCLI.run(["config", add ? "add" : "remove", key, value])
        }
    }

    /// Chooses whether an account's sessions sync, for every Mac in the Circle.
    func setAccountSync(_ account: ClaudeAccount, _ on: Bool) {
        let done = on ? "\(account.name) syncs to all your Macs"
                      : "\(account.name) stays on each Mac now. Nothing already synced was deleted."
        perform("account:\(account.id)", success: done) {
            try await StradlCLI.run(["accounts", "sync", account.id, on ? "on" : "off"], timeout: 90)
            try? await AgentAPI.syncNow()
        }
    }

    /// Names an account new to the Circle and records whether it syncs.
    func setUpAccount(_ account: ClaudeAccount, name: String, syncs: Bool) {
        let label = name.trimmingCharacters(in: .whitespaces)
        perform("account:\(account.id)", success: syncs ? "\(label) syncs to all your Macs" : "\(label) stays on each Mac") {
            try await StradlCLI.run(["accounts", "label", account.id, label], timeout: 90)
            try await StradlCLI.run(["accounts", "sync", account.id, syncs ? "on" : "off"], timeout: 90)
            try? await AgentAPI.syncNow()
        }
    }

    /// The first account new to the Circle, the one the Claude app is
    /// signed in to first: the app asks about it.
    var newAccount: ClaudeAccount? {
        accounts.filter(\.isNew).sorted { $0.active && !$1.active }.first
    }

    func setAccountLabel(_ account: ClaudeAccount, _ label: String) {
        perform("account:\(account.id)") {
            try await StradlCLI.run(["accounts", "label", account.id, label], timeout: 90)
        }
    }

    func forget(_ device: Device) {
        perform("forget:\(device.id)", success: "\(device.name) removed from your Circle") {
            try await StradlCLI.run(["circle", "forget", device.id])
        }
    }

    func copyDiagnostics() {
        perform("diagnostics", success: "Diagnostics copied: paste them into a message or an issue", refresh: false) {
            let data = try await StradlCLI.run(["diagnostics"])
            await MainActor.run {
                NSPasteboard.general.clearContents()
                NSPasteboard.general.setString(String(decoding: data, as: UTF8.self), forType: .string)
            }
        }
    }

    func reveal(_ url: URL) {
        if FileManager.default.fileExists(atPath: url.path) {
            NSWorkspace.shared.activateFileViewerSelecting([url])
        } else {
            NSWorkspace.shared.open(url.deletingLastPathComponent())
        }
    }

    func openDashboard() {
        guard let ep = AgentAPI.endpoint(),
              var c = URLComponents(url: ep.base, resolvingAgainstBaseURL: false) else { return }
        c.queryItems = [URLQueryItem(name: "t", value: ep.token)]
        if let u = c.url { NSWorkspace.shared.open(u) }
    }
}

// MARK: formatting

extension Date {
    var ago: String {
        let s = -timeIntervalSinceNow
        switch s {
        case ..<60: return "just now"
        case ..<3600: return "\(Int(s / 60)) min ago"
        case ..<86400: return "\(Int(s / 3600)) hr ago"
        default:
            let d = Int(s / 86400)
            return d == 1 ? "yesterday" : "\(d) days ago"
        }
    }

    /// Go's zero time, which a Mac that never synced reports.
    var isZero: Bool { timeIntervalSince1970 < 86400 }
}

extension Int {
    func plural(_ unit: String) -> String { "\(self) \(unit)\(self == 1 ? "" : "s")" }
}

extension Int64 {
    var bytes: String { ByteCountFormatter.string(fromByteCount: self, countStyle: .file) }
}
