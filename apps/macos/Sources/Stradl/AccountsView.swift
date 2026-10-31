import SwiftUI

/// The Claude accounts your Macs have used and which of them sync. One Mac
/// can be signed in to several accounts over time (work and personal, say);
/// an account that doesn't sync keeps its sessions, and the project folders
/// only its sessions use, on the Mac they were made on.
///
/// A plain scrolling stack rather than a List (see FoldersView).
struct AccountsView: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                Text("Choose which Claude accounts sync to all your Macs. The choice is for your whole Circle. Turning an account off keeps its sessions on the Mac they were made on and deletes nothing already synced.")
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                if !model.heldSessions.isEmpty {
                    Card(title: "\(model.heldSessions.count.plural("session")) waiting for an account", systemImage: "hourglass") {
                        Text("Stradl can't tell which account these sessions on this Mac belong to (they were written before Stradl knew this Mac's sign-ins, or while switching accounts), so they don't sync while an account is kept on each Mac.")
                            .foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                        ForEach(model.heldSessions.prefix(5)) { h in
                            Text(h.cwd.map { ($0 as NSString).abbreviatingWithTildeInPath } ?? h.id)
                                .font(.caption.monospaced())
                                .foregroundStyle(.secondary)
                                .lineLimit(1)
                                .truncationMode(.middle)
                        }
                        Menu("Assign All To…") {
                            ForEach(model.accounts) { a in
                                Button(a.name) { model.assignHeld(to: a) }
                            }
                        }
                        .fixedSize()
                        .disabled(model.busy.contains("assign"))
                    }
                }
                let undecided = model.accounts.filter(\.undecided)
                if !undecided.isEmpty {
                    Card {
                        Text(undecided.count == 1
                             ? "One account hasn't been chosen yet. It syncs until you choose."
                             : "\(undecided.count) accounts haven't been chosen yet. They sync until you choose.")
                            .foregroundStyle(.secondary)
                    }
                }
                if model.accounts.isEmpty {
                    Card {
                        Text("No Claude accounts seen yet. They're picked up at the next sync.")
                            .foregroundStyle(.secondary)
                    }
                } else {
                    VStack(spacing: 0) {
                        ForEach(Array(model.accounts.enumerated()), id: \.element.id) { i, a in
                            if i > 0 { Divider().padding(.leading, 52) }
                            AccountRow(account: a,
                                       busy: model.busy.contains("account:\(a.id)"),
                                       setSync: { on in model.setAccountSync(a, on) },
                                       setLabel: { label in model.setAccountLabel(a, label) })
                        }
                    }
                    .background(.background, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
                    .overlay(RoundedRectangle(cornerRadius: 12, style: .continuous).stroke(.separator, lineWidth: 0.5))
                }
                Text("Sign-ins never sync: each Mac signs in to each account itself.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            .padding(24)
        }
        .task { await model.refreshAccounts(fromStorage: true) }
    }
}

/// One account. It gets everything it shows as values (see FolderRow).
struct AccountRow: View {
    var account: ClaudeAccount
    var busy: Bool
    var setSync: (Bool) -> Void
    var setLabel: (String) -> Void
    @State private var naming = false
    @State private var draft = ""

    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: account.active ? "person.crop.circle.fill" : "person.crop.circle")
                .font(.system(size: 20))
                .foregroundStyle(account.syncs ? Color.accentColor : .secondary)
                .frame(width: 28)
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 6) {
                    Text(account.name).lineLimit(1).truncationMode(.middle)
                    if account.active {
                        HStack(spacing: 4) {
                            Circle().fill(.green).frame(width: 7, height: 7)
                            Text("Signed in to Claude")
                        }
                        .font(.caption2)
                        .padding(.horizontal, 6).padding(.vertical, 1)
                        .background(Color.green.opacity(0.12), in: Capsule())
                        .help("The Claude app on this Mac is signed in to this account")
                    }
                    if account.terminal == true && !account.active {
                        Text("Terminal")
                            .font(.caption2)
                            .padding(.horizontal, 6).padding(.vertical, 1)
                            .background(Color.secondary.opacity(0.15), in: Capsule())
                            .help("Claude Code in the terminal is signed in to this account")
                    }
                }
                Text(details)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                    .truncationMode(.middle)
            }
            Spacer(minLength: 8)
            if account.undecided {
                Button("Keep on each Mac") { setSync(false) }
                    .controlSize(.small)
                    .disabled(busy)
                Button("Sync") { setSync(true) }
                    .controlSize(.small)
                    .buttonStyle(.borderedProminent)
                    .disabled(busy)
            } else {
                Toggle("Sync", isOn: Binding(get: { account.syncs }, set: { setSync($0) }))
                    .toggleStyle(.switch)
                    .labelsHidden()
                    .disabled(busy)
                    .help(account.syncs ? "Its sessions sync to all your Macs" : "Its sessions stay on the Mac they were made on")
            }
            Button {
                draft = account.label ?? ""
                naming = true
            } label: {
                Image(systemName: "pencil")
            }
            .buttonStyle(.borderless)
            .help("Name this account")
            .disabled(busy)
            .popover(isPresented: $naming) {
                VStack(alignment: .leading, spacing: 8) {
                    Text("Nickname").font(.headline)
                    TextField("Work, Personal… (empty: its email)", text: $draft)
                        .frame(width: 220)
                        .onSubmit { save() }
                    HStack {
                        Spacer()
                        Button("Cancel") { naming = false }
                        Button("Save") { save() }.keyboardShortcut(.defaultAction)
                    }
                }
                .padding(14)
            }
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
    }

    private func save() {
        naming = false
        setLabel(draft)
    }

    private var details: String {
        var parts: [String] = ["ID \(account.shortID)"]
        if let e = account.email, e != account.name { parts.append(e) }
        let orgs = (account.orgs ?? []).compactMap { $0.name }.filter { !$0.isEmpty }
        if !orgs.isEmpty { parts.append(orgs.joined(separator: ", ")) }
        parts.append(account.sessions.plural("session"))
        switch account.sync {
        case "off": parts.append("stays on each Mac")
        case "ask": parts.append("syncs until you choose")
        default: parts.append("syncs to all your Macs")
        }
        return parts.joined(separator: " · ")
    }
}

/// Asks about an account new to the Circle (just signed in to, say): its
/// name and whether its sessions sync. Shown until it has both.
struct NewAccountSheet: View {
    @EnvironmentObject var model: AppModel
    var account: ClaudeAccount
    @State private var name = ""
    @State private var syncs = true

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack(spacing: 10) {
                Image(systemName: "person.crop.circle.badge.plus")
                    .font(.system(size: 28))
                    .foregroundStyle(Color.accentColor)
                VStack(alignment: .leading, spacing: 2) {
                    Text("New Claude account").font(.title3.weight(.semibold))
                    Text(account.active ? "The Claude app on this Mac just signed in to it." : "A Mac in your Circle has used it.")
                        .foregroundStyle(.secondary)
                }
            }
            VStack(alignment: .leading, spacing: 4) {
                Text(account.email ?? "Account \(account.shortID)").font(.headline)
                Text(account.email == nil
                     ? "ID \(account.id). Its email shows here once Stradl learns it."
                     : "ID \(account.id)")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
                    .fixedSize(horizontal: false, vertical: true)
            }
            VStack(alignment: .leading, spacing: 6) {
                Text("Nickname (optional)").font(.headline)
                TextField("Work, Personal, a client…", text: $name)
                    .textFieldStyle(.roundedBorder)
            }
            VStack(alignment: .leading, spacing: 6) {
                Text("Its sessions").font(.headline)
                Picker("", selection: $syncs) {
                    Text("Sync to all my Macs").tag(true)
                    Text("Stay on the Mac they were made on").tag(false)
                }
                .pickerStyle(.radioGroup)
                .labelsHidden()
            }
            Text("You can rename it or change this later in Accounts.")
                .font(.caption)
                .foregroundStyle(.secondary)
            HStack {
                Spacer()
                Button("Save") { model.setUpAccount(account, name: name, syncs: syncs) }
                    .keyboardShortcut(.defaultAction)
                    .disabled(model.busy.contains("account:\(account.id)"))
            }
        }
        .padding(22)
        .frame(width: 420)
        .interactiveDismissDisabled(true)
        .onAppear {
            name = account.label ?? ""
        }
    }
}
