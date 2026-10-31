import SwiftUI

/// What the next sync would do: the agent's dry run.
struct ActivityView: View {
    @EnvironmentObject var model: AppModel
    @State private var query = ""

    var body: some View {
        if let p = model.overview?.pending {
            VStack(alignment: .leading, spacing: 16) {
                HStack(spacing: 12) {
                    Stat(value: "\(p.upload)", label: "To upload", symbol: "arrow.up", color: Theme.azure)
                    Stat(value: "\(p.download)", label: "To download", symbol: "arrow.down", color: Theme.teal)
                    Stat(value: "\(p.merge)", label: "To merge", symbol: "arrow.triangle.merge", color: Theme.violet)
                    Stat(value: "\(p.deferred)", label: "Waiting for Claude", symbol: "clock", color: Theme.amber)
                    Stat(value: "\((p.delete ?? 0) + (p.trash ?? 0))", label: "Deletes (to trash)", symbol: "trash", color: Theme.coral)
                    Stat(value: "\(p.conflicts)", label: "Conflict copies", symbol: "exclamationmark.triangle", color: Theme.pink)
                }
                if p.known == false {
                    Card {
                        Label("Storage didn't answer, so what's waiting to download isn't known yet. It updates on its own.", systemImage: "clock")
                            .foregroundStyle(.secondary)
                        ForEach(model.overview?.warnings ?? [], id: \.self) { w in
                            Text(w).font(.callout).foregroundStyle(.secondary).textSelection(.enabled)
                        }
                    }
                }
                if p.total == 0 && p.known != false {
                    Card {
                        Label("Nothing waiting. This Mac and your storage match.", systemImage: "checkmark.circle")
                            .foregroundStyle(Theme.ok)
                    }
                } else if p.total > 0 {
                    let rows = p.files.filter { query.isEmpty || $0.path.localizedCaseInsensitiveContains(query) }
                    Table(rows) {
                        TableColumn("Action") { f in ActionBadge(action: f.action) }.width(110)
                        TableColumn("File") { f in
                            Text(f.path).font(.callout.monospaced()).lineLimit(1).truncationMode(.middle).help(f.path)
                        }
                    }
                    .tableStyle(.inset(alternatesRowBackgrounds: false))
                    .scrollContentBackground(.hidden)
                    .themePanel(radius: 16)
                    if p.total > p.files.count {
                        Text("Showing the first \(p.files.count) of \(p.total).").font(.caption).foregroundStyle(.secondary)
                    }
                }
                if p.deferred > 0 {
                    Text("Files waiting for Claude belong to sessions open on this Mac. They update when those sessions close.")
                        .font(.callout).foregroundStyle(.secondary)
                }
                if p.conflicts > 0 {
                    Text("Conflict copies (.conflict files) are the other side of a change made on two Macs at once. Review them with 'stradl conflicts' in Terminal.")
                        .font(.callout).foregroundStyle(.secondary)
                }
            }
            .padding(24)
            .searchable(text: $query, placement: .toolbar, prompt: "Filter files")
        } else if model.overviewError != nil {
            AgentOffline()
        } else {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }
}

struct ActionBadge: View {
    var action: String

    var body: some View {
        Label(label, systemImage: symbol)
            .font(.caption.weight(.medium))
            .foregroundStyle(color)
    }

    private var label: String {
        switch action {
        case "upload": return "Upload"
        case "download": return "Download"
        case "merge": return "Merge"
        case "restore": return "Restore"
        case "deferred": return "Waiting"
        case "delete": return "Delete"
        case "trash": return "To trash"
        default: return action.capitalized
        }
    }

    private var symbol: String {
        switch action {
        case "upload", "restore": return "arrow.up"
        case "download": return "arrow.down"
        case "merge": return "arrow.triangle.merge"
        case "deferred": return "clock"
        case "delete", "trash": return "trash"
        default: return "circle"
        }
    }

    private var color: Color {
        switch action {
        case "deferred": return Theme.violet
        case "delete", "trash": return Theme.warn
        default: return Theme.azure
        }
    }
}
