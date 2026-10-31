import AppKit
import SwiftUI

/// The project folders that sync: every folder a session works in, plus
/// extra folders, with git repositories carried whole.
///
/// A plain scrolling stack rather than a List: an AppKit-backed List here
/// updated itself from inside its own table-view callbacks ("reentrant
/// operation in its NSTableView delegate"), which stopped the whole window
/// from drawing.
struct FoldersView: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        let folders = model.folders.sorted {
            // Real projects first, the Claude app's scratch folders last.
            ($0.isScratch ? 1 : 0, -$0.files, $0.path) < ($1.isScratch ? 1 : 0, -$1.files, $1.path)
        }
        let extras = model.config?.extraFolders ?? []
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                Text("Every folder a session works in syncs to all your Macs, at the same place under your home folder. Build output such as node_modules stays on each Mac.")
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                if let c = model.config, !c.folderSync {
                    Card {
                        Text("Project folders don't sync on this Mac. Turn on Project folders in Settings to sync the folders your sessions work in.")
                            .foregroundStyle(.secondary)
                    }
                }
                if folders.isEmpty {
                    Card {
                        Text("No project folders yet. They're picked up from your sessions at the next sync.")
                            .foregroundStyle(.secondary)
                    }
                } else {
                    VStack(spacing: 0) {
                        ForEach(Array(folders.enumerated()), id: \.element.id) { i, f in
                            if i > 0 { Divider().padding(.leading, 52) }
                            FolderRow(folder: f, extraEntry: extraEntry(for: f, in: extras)) { entry in
                                model.editList("extra_folders", add: false, entry)
                            }
                        }
                    }
                    .themePanel()
                }
            }
            .padding(24)
        }
        .toolbar {
            ToolbarItem {
                Button { addFolder() } label: { Label("Add Folder", systemImage: "folder.badge.plus") }
                    .help("Sync another folder to all your Macs")
            }
        }
        .task { await model.refreshFolders() }
    }

    /// The extra_folders entry that adds this folder, if one does.
    private func extraEntry(for f: FolderStat, in extras: [String]) -> String? {
        extras.first { entry in
            let expanded = entry.hasPrefix("~/") ? StradlPaths.home.appendingPathComponent(String(entry.dropFirst(2))).path : entry
            return expanded == f.abs
        }
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

/// One folder. It gets everything it shows as values, so a refresh of the
/// app's state never re-enters a row while it draws.
struct FolderRow: View {
    var folder: FolderStat
    var extraEntry: String?
    var stopSyncing: (String) -> Void

    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: folder.git ? "point.3.connected.trianglepath.dotted" : "folder")
                .font(.system(size: 18))
                .foregroundStyle(folder.exists ? Theme.violet : .secondary)
                .frame(width: 28)
                .help(folder.git ? "A git repository: history and branches sync too" : "A folder")
            VStack(alignment: .leading, spacing: 2) {
                Text(folder.isScratch ? (folder.path as NSString).lastPathComponent : "~/" + folder.path)
                    .font(.body.monospaced())
                    .lineLimit(1)
                    .truncationMode(.middle)
                    .help("~/" + folder.path)
                Text(details)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Spacer(minLength: 8)
            if !folder.exists {
                Text("Not on this Mac").font(.caption).foregroundStyle(Theme.warn.readable)
            }
            if let entry = extraEntry {
                Button("Remove from Extra Folders") { stopSyncing(entry) }
                    .controlSize(.small)
                    .help("Stop adding this folder by hand. It keeps syncing while a session works in it; nothing is deleted.")
            }
            Button {
                NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: folder.abs)])
            } label: {
                Image(systemName: "magnifyingglass")
            }
            .buttonStyle(.borderless)
            .help("Show in Finder")
            .disabled(!folder.exists)
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
    }

    private var details: String {
        var parts = [folder.git ? "Git repository" : folder.isScratch ? "Claude app scratch folder" : "Folder",
                     folder.files.plural("file"), folder.bytes.bytes]
        if let t = folder.lastChange, !t.isZero {
            parts.append("changed \(t.ago)")
        }
        return parts.joined(separator: " · ")
    }
}
