import AppKit
import SwiftUI

struct SessionsView: View {
    @EnvironmentObject var model: AppModel
    @State private var query = ""
    @State private var state = "all"
    @State private var selection: Session.ID?
    @State private var order = [KeyPathComparator(\Session.lastActivityAt, order: .reverse)]

    var body: some View {
        if let o = model.overview {
            let rows = filtered(o.sessions).sorted(using: order)
            HSplitView {
                Table(rows, selection: $selection, sortOrder: $order) {
                    TableColumn("Session", value: \.title) { s in
                        HStack(spacing: 6) {
                            if s.live { Image(systemName: "circle.fill").font(.system(size: 6)).foregroundStyle(Theme.ok).help("Open in Claude now") }
                            Text(s.title).lineLimit(1)
                        }
                    }
                    .width(min: 220, ideal: 320)
                    TableColumn("Project", value: \.project) { s in Text(s.project).foregroundStyle(Theme.text2) }
                        .width(min: 90, ideal: 140)
                    TableColumn("Last activity", value: \.lastActivityAt) { s in Text(s.lastActivityAt.ago) }
                        .width(min: 90, ideal: 110)
                    TableColumn("Turns", value: \.userTurns) { s in Text("\(s.turns)").monospacedDigit() }
                        .width(50)
                    TableColumn("Sync", value: \.syncState) { s in SyncBadge(state: s.syncState) }
                        .width(min: 90, ideal: 120)
                }
                .tableStyle(.inset(alternatesRowBackgrounds: false))
                .scrollContentBackground(.hidden)
                .frame(minWidth: 520)
                if let id = selection, let s = o.sessions.first(where: { $0.id == id }) {
                    SessionDetail(session: s)
                        .frame(minWidth: 260, idealWidth: 320, maxWidth: 420)
                }
            }
            .searchable(text: $query, placement: .toolbar, prompt: "Search sessions")
            .toolbar {
                ToolbarItem {
                    Picker("Sync state", selection: $state) {
                        Text("All").tag("all")
                        Text("Synced").tag("synced")
                        Text("Waiting").tag("pending")
                        Text("Open in Claude").tag("held")
                        Text("Conflicts").tag("conflict")
                    }
                    .pickerStyle(.menu)
                    .help("Filter by sync state")
                }
            }
        } else if model.overviewError != nil {
            AgentOffline()
        } else {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }

    private func filtered(_ sessions: [Session]) -> [Session] {
        sessions.filter { s in
            let matchesState: Bool = {
                switch state {
                case "all": return true
                case "pending": return s.syncState.hasPrefix("pending") || s.syncState == "merging"
                default: return s.syncState == state
                }
            }()
            guard matchesState else { return false }
            guard !query.isEmpty else { return true }
            return [s.title, s.project, s.firstPrompt, s.lastPrompt].contains { $0.localizedCaseInsensitiveContains(query) }
        }
    }
}

struct SyncBadge: View {
    var state: String

    var body: some View {
        Text(label)
            .font(.caption.weight(.medium))
            .padding(.horizontal, 7)
            .padding(.vertical, 2)
            .background(color.opacity(0.15), in: Capsule())
            .foregroundStyle(color.readable)
    }

    private var label: String {
        switch state {
        case "synced": return "Synced"
        case "pending-upload": return "To upload"
        case "pending-download": return "To download"
        case "merging": return "Merging"
        case "held": return "Open in Claude"
        case "conflict": return "Conflict"
        default: return "Not synced"
        }
    }

    private var color: Color {
        switch state {
        case "synced": return Theme.ok
        case "pending-upload", "pending-download", "merging": return Theme.azure
        case "held": return Theme.violet
        case "conflict": return Theme.warn
        default: return .secondary
        }
    }
}

struct SessionDetail: View {
    var session: Session

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                Text(session.title).font(.title3.weight(.semibold)).textSelection(.enabled)
                HStack { SyncBadge(state: session.syncState); if session.archived { Text("Archived").font(.caption).foregroundStyle(.secondary) } }
                group("Project") {
                    Text(session.projectPath).textSelection(.enabled).font(.callout)
                    if let b = session.branch, !b.isEmpty { Text("branch \(b)").font(.caption).foregroundStyle(.secondary) }
                }
                group("Activity") {
                    Text("Started \(session.startedAt.formatted(date: .abbreviated, time: .shortened))").font(.callout)
                    Text("Last \(session.lastActivityAt.ago) · \(session.turns) turns · \(session.sizeBytes.bytes)").font(.callout).foregroundStyle(.secondary)
                    if let d = session.devices, !d.isEmpty { Text("Worked on from \(d.joined(separator: ", "))").font(.callout).foregroundStyle(.secondary) }
                }
                if !session.firstPrompt.isEmpty {
                    group("First prompt") { Text(session.firstPrompt).font(.callout).textSelection(.enabled) }
                }
                if !session.lastPrompt.isEmpty && session.lastPrompt != session.firstPrompt {
                    group("Latest prompt") { Text(session.lastPrompt).font(.callout).textSelection(.enabled) }
                }
                if let files = session.filesTouched, !files.isEmpty {
                    group("Files touched") {
                        ForEach(files.prefix(12), id: \.self) { f in
                            Text(f).font(.caption.monospaced()).lineLimit(1).truncationMode(.middle)
                        }
                    }
                }
                HStack {
                    Button("Copy resume command") {
                        NSPasteboard.general.clearContents()
                        NSPasteboard.general.setString("cd \(shellQuote(session.projectPath)) && claude --resume \(session.id)", forType: .string)
                    }
                    Button("Show folder") {
                        NSWorkspace.shared.open(URL(fileURLWithPath: session.projectPath))
                    }
                }
            }
            .padding(16)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private func group<C: View>(_ title: String, @ViewBuilder _ c: () -> C) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(title.uppercased()).font(.caption2.weight(.semibold)).foregroundStyle(.secondary)
            c()
        }
    }
}

func shellQuote(_ s: String) -> String {
    "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'"
}
