import AppKit
import SwiftUI

/// ⌘, opens the same settings the sidebar shows.
struct SettingsView: View {
    var body: some View {
        ScrollView { SettingsForm().padding(24) }
    }
}

/// Every setting the stradl command can change, saved as it changes. The
/// agent picks changes up within a minute.
struct SettingsForm: View {
    @EnvironmentObject var model: AppModel
    @State private var deviceName = ""
    @State private var newExclude = ""
    @State private var confirmBuildOutput = false

    var body: some View {
        if let c = model.config {
            VStack(alignment: .leading, spacing: 20) {
                Card(title: "What syncs", systemImage: "square.stack.3d.up") {
                    Picker("Scope", selection: binding(c.scope) { model.setConfig("scope", $0) }) {
                        Text("Everything (recommended)").tag("everything")
                        Text("Sessions only").tag("sessions")
                        Text("Sessions and Claude settings").tag("full")
                    }
                    Text(scopeHelp(c.scope)).font(.caption).foregroundStyle(.secondary)
                    Toggle("Project folders, including git repositories", isOn: binding(c.folderSync) { model.setConfig("folder_sync", $0 ? "on" : "off") })
                        .disabled(c.scope != "everything")
                    Toggle("Claude app sidebar (sessions, groups and pins)", isOn: binding(c.desktopSync) { model.setConfig("desktop_sync", $0 ? "on" : "off") })
                    Toggle("MCP servers", isOn: binding(c.mcpSync) { model.setConfig("mcp_sync", $0 ? "on" : "off") })
                    Toggle("Build output too (node_modules, .venv, …)", isOn: binding(c.syncBuildArtifacts) { on in
                        // Turning it on is rarely right: ask first.
                        if on { confirmBuildOutput = true } else { model.setConfig("sync_build_artifacts", "off") }
                    })
                    .confirmationDialog("Sync build output too?", isPresented: $confirmBuildOutput) {
                        Button("Sync Build Output") { model.setConfig("sync_build_artifacts", "on") }
                        Button("Cancel", role: .cancel) {}
                    } message: {
                        Text("Folders like node_modules can hold hundreds of thousands of files. Every Mac rebuilds its own, and syncing them is slow, fills your storage and, on an iCloud Desktop, makes iCloud download all of them.")
                    }
                    Text("Left on each Mac by default: \(c.buildArtifacts.joined(separator: ", ")). Each Mac rebuilds its own.")
                        .font(.caption).foregroundStyle(.secondary)
                }

                Card(title: "Extra folders", systemImage: "folder.badge.plus") {
                    Text("Folders your sessions work in sync on their own. Add any other folder in your home folder to keep it on every Mac.")
                        .font(.callout).foregroundStyle(.secondary)
                    ForEach(c.extraFolders, id: \.self) { f in
                        HStack {
                            Text(f).font(.callout.monospaced())
                            Spacer()
                            Button(role: .destructive) { model.editList("extra_folders", add: false, f) } label: { Image(systemName: "minus.circle") }
                                .buttonStyle(.borderless).help("Stop syncing this folder")
                        }
                    }
                    Button("Add Folder…") { addFolder() }
                }

                Card(title: "Leave out", systemImage: "line.3.horizontal.decrease.circle") {
                    Text("Patterns for files in project folders that stay on each Mac, such as *.mov or dist/**.")
                        .font(.callout).foregroundStyle(.secondary)
                    ForEach(c.folderExclude, id: \.self) { p in
                        HStack {
                            Text(p).font(.callout.monospaced())
                            Spacer()
                            Button(role: .destructive) { model.editList("folder_exclude", add: false, p) } label: { Image(systemName: "minus.circle") }
                                .buttonStyle(.borderless).help("Remove this pattern")
                        }
                    }
                    HStack {
                        TextField("Pattern, e.g. *.mov", text: $newExclude)
                            .onSubmit(addExclude)
                        Button("Add", action: addExclude).disabled(newExclude.trimmingCharacters(in: .whitespaces).isEmpty)
                    }
                }

                Card(title: "This Mac", systemImage: "laptopcomputer") {
                    HStack {
                        TextField("Name in your Circle", text: $deviceName)
                            .onSubmit { model.setConfig("device_name", deviceName) }
                        Button("Rename") { model.setConfig("device_name", deviceName) }
                            .disabled(deviceName == c.deviceName || deviceName.trimmingCharacters(in: .whitespaces).isEmpty)
                    }
                    Picker("Full check every", selection: binding(c.syncIntervalSeconds) { model.setConfig("sync_interval_seconds", String($0)) }) {
                        Text("1 minute").tag(60)
                        Text("2 minutes").tag(120)
                        Text("5 minutes").tag(300)
                        Text("15 minutes").tag(900)
                        Text("1 hour").tag(3600)
                        if ![60, 120, 300, 900, 3600].contains(c.syncIntervalSeconds) {
                            Text("\(c.syncIntervalSeconds) seconds").tag(c.syncIntervalSeconds)
                        }
                    }
                    Text("Stradl also syncs within seconds of a change here and within a minute of one on another Mac.")
                        .font(.caption).foregroundStyle(.secondary)
                }

                Card(title: "Background", systemImage: "menubar.rectangle") {
                    Toggle("Start Stradl at login and keep syncing", isOn: Binding(
                        get: { c.agentInstalled },
                        set: { $0 ? model.startAgent() : model.stopAgent() }))
                        .disabled(model.busy.contains("agent"))
                    Toggle("Pause syncing", isOn: Binding(get: { model.paused }, set: { _ in model.togglePause() }))
                        .disabled(!c.agentInstalled)
                }

                Card(title: "Storage", systemImage: "externaldrive.connected.to.line.below") {
                    LabeledContent("Provider", value: c.provider.capitalized)
                    if let a = c.account, !a.isEmpty { LabeledContent("Account", value: a) }
                    LabeledContent("Container", value: c.container)
                    LabeledContent("Signed in with", value: c.auth)
                    Text("Files are encrypted on this Mac before upload. To change storage or credentials, run 'stradl init' in Terminal.")
                        .font(.caption).foregroundStyle(.secondary)
                }

                Card(title: "Files", systemImage: "folder") {
                    HStack {
                        Button("Open Stradl Folder") { NSWorkspace.shared.open(URL(fileURLWithPath: c.stateDir)) }
                        Button("Open Trash") { model.reveal(URL(fileURLWithPath: c.trashDir)) }
                            .help("Files your other Macs deleted, kept here for 30 days")
                        Button("Open Conflicts") { model.reveal(URL(fileURLWithPath: c.conflictsDir)) }
                        Button("Edit config.yaml") { NSWorkspace.shared.open(URL(fileURLWithPath: c.configPath)) }
                    }
                    Text("Stradl \(c.version) · device \(c.deviceId)").font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
                }
            }
            .onAppear { deviceName = c.deviceName }
            .onChange(of: c.deviceName) { deviceName = $0 }
        } else if let err = model.configError {
            let outdated = err.contains("unknown command")
            VStack(spacing: 12) {
                Image(systemName: outdated ? "arrow.down.circle" : "gearshape.2").font(.system(size: 36)).foregroundStyle(.secondary)
                Text(outdated ? "Update Stradl to change settings here" : "Settings aren't available").font(.title3.weight(.semibold))
                Text(outdated ? "The stradl command on this Mac is older than this app." : err)
                    .foregroundStyle(.secondary).multilineTextAlignment(.center).textSelection(.enabled)
                if outdated {
                    Button("Update Stradl") { model.update() }
                        .buttonStyle(.borderedProminent)
                        .disabled(model.busy.contains("update"))
                } else {
                    Button("Try Again") { Task { await model.refreshConfig() } }
                }
            }
            .frame(maxWidth: .infinity)
            .padding(40)
        } else {
            ProgressView().frame(maxWidth: .infinity)
        }
    }

    /// A binding that shows the saved value and saves changes through set.
    private func binding<T>(_ value: T, set: @escaping (T) -> Void) -> Binding<T> {
        Binding(get: { value }, set: set)
    }

    private func scopeHelp(_ scope: String) -> String {
        switch scope {
        case "everything": return "All of ~/.claude (logins stay on each Mac), your MCP servers and every project folder your sessions use."
        case "sessions": return "Conversations, prompt history, plans and the Claude app sidebar only."
        default: return "Conversations plus Claude settings, skills, agents and commands, without project folders."
        }
    }

    private func addExclude() {
        let p = newExclude.trimmingCharacters(in: .whitespaces)
        guard !p.isEmpty else { return }
        model.editList("folder_exclude", add: true, p)
        newExclude = ""
    }

    private func addFolder() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.directoryURL = StradlPaths.home
        panel.prompt = "Sync This Folder"
        if panel.runModal() == .OK, let url = panel.url {
            model.editList("extra_folders", add: true, url.path)
        }
    }
}
