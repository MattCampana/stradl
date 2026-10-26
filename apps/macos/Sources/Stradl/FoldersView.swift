import AppKit
import SwiftUI

/// The project folders that sync: every folder a session works in, plus
/// extra folders, with git repositories carried whole.
struct FoldersView: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            if let c = model.config, !c.folderSync {
                Card {
                    Text("Project folders don't sync on this Mac. Turn on Project folders in Settings to sync the folders your sessions work in.")
                        .foregroundStyle(.secondary)
                }
                .padding(24)
            }
            List {
                Section {
                    ForEach(model.folders) { f in
                        FolderRow(folder: f)
                    }
                    if model.folders.isEmpty {
                        Text("No project folders yet. They're picked up from your sessions at the next sync.")
                            .foregroundStyle(.secondary)
                    }
                } header: {
                    Text("Every folder a session works in syncs to all your Macs at the same place under your home folder. Build output such as node_modules stays on each Mac.")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                        .textCase(nil)
                        .padding(.bottom, 6)
                }
            }
            .listStyle(.inset(alternatesRowBackgrounds: true))
        }
        .toolbar {
            ToolbarItem {
                Button { addFolder() } label: { Label("Add Folder", systemImage: "folder.badge.plus") }
                    .help("Sync another folder to all your Macs")
            }
        }
        .task { await model.refreshFolders() }
    }

    private func addFolder() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.directoryURL = StradlPaths.home
        panel.prompt = "Sync This Folder"
        panel.message = "Choose a folder in your home folder to keep on all your Macs."
        if panel.runModal() == .OK, let url = panel.url {
            model.editList("extra_folders", add: true, url.path)
        }
    }
}

struct FolderRow: View {
    @EnvironmentObject var model: AppModel
    var folder: FolderStat

    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: folder.git ? "point.3.connected.trianglepath.dotted" : "folder")
                .font(.system(size: 18))
                .foregroundStyle(folder.exists ? Color.accentColor : .secondary)
                .frame(width: 24)
                .help(folder.git ? "A git repository: history and branches sync too" : "A folder")
            VStack(alignment: .leading, spacing: 2) {
                Text("~/" + folder.path).font(.body.monospaced())
                HStack(spacing: 6) {
                    Text(folder.git ? "Git repository" : "Folder")
                    Text("·")
                    Text(folder.files.plural("file"))
                    Text("·")
                    Text(folder.bytes.bytes)
                    if let t = folder.lastChange, !t.isZero {
                        Text("·")
                        Text("changed \(t.ago)")
                    }
                }
                .font(.caption)
                .foregroundStyle(.secondary)
            }
            Spacer()
            if !folder.exists {
                Text("Not on this Mac").font(.caption).foregroundStyle(.orange)
            }
            Button { NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: folder.abs)]) } label: {
                Image(systemName: "magnifyingglass")
            }
            .buttonStyle(.borderless)
            .help("Show in Finder")
            .disabled(!folder.exists)
        }
        .padding(.vertical, 4)
        .contextMenu {
            if let c = model.config, c.extraFolders.contains(where: { expand($0) == folder.abs }) {
                Button("Stop Syncing This Extra Folder") {
                    if let entry = c.extraFolders.first(where: { expand($0) == folder.abs }) {
                        model.editList("extra_folders", add: false, entry)
                    }
                }
            }
            Button("Show in Finder") { NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: folder.abs)]) }
        }
    }

    private func expand(_ p: String) -> String {
        p.hasPrefix("~/") ? StradlPaths.home.appendingPathComponent(String(p.dropFirst(2))).path : p
    }
}
