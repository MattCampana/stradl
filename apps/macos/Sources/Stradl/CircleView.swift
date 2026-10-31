import SwiftUI

/// Every Mac syncing into the same storage. Each one writes an encrypted
/// heartbeat to _metadata/devices/ after it syncs.
struct CircleView: View {
    @EnvironmentObject var model: AppModel
    @State private var forgetting: Device?

    var body: some View {
        if let o = model.overview {
            ScrollView {
                VStack(alignment: .leading, spacing: 16) {
                    Text("Your Circle is every Mac syncing into \(model.config.map { "the \($0.container) container" } ?? "your storage"). Each Mac checks in after every sync.")
                        .foregroundStyle(.secondary)
                    LazyVGrid(columns: [GridItem(.adaptive(minimum: 260), spacing: 16, alignment: .top)], alignment: .leading, spacing: 16) {
                        ForEach(o.devices) { d in
                            MacCard(device: d) { forgetting = d }
                        }
                    }
                    if o.devices.isEmpty {
                        Card {
                            Text("No Mac has checked in yet. Macs appear after their first sync with Stradl 2.1 or later.")
                                .foregroundStyle(.secondary)
                        }
                    }
                }
                .padding(24)
            }
            .confirmationDialog(
                "Remove \(forgetting?.name ?? "this Mac") from your Circle?",
                isPresented: Binding(get: { forgetting != nil }, set: { if !$0 { forgetting = nil } }),
                presenting: forgetting
            ) { d in
                Button("Remove", role: .destructive) { model.forget(d) }
            } message: { _ in
                Text("Use this for a Mac you no longer use. Nothing on it changes, and if it syncs again it comes back.")
            }
        } else if model.overviewError != nil {
            AgentOffline()
        } else {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }
}

struct MacCard: View {
    var device: Device
    var forget: () -> Void

    var body: some View {
        Card {
            HStack(alignment: .top) {
                Image(systemName: "laptopcomputer")
                    .font(.system(size: 28))
                    .foregroundStyle(device.this ? Theme.violet : .secondary)
                VStack(alignment: .leading, spacing: 2) {
                    Text(device.name).font(.headline)
                    Text(device.this ? "This Mac" : "Stradl \(device.version)")
                        .font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                StatusDot(health: device.healthy ? .good : .attention)
            }
            Divider()
            row("Last sync", device.lastSyncAt.isZero ? "never" : device.lastSyncAt.ago)
            row("Sessions", "\(device.sessions)")
            row("Worked on this week", (device.recentSessionIds?.count ?? 0).plural("session"))
            if !device.this {
                row("Version", device.version)
            }
            if !device.healthy {
                Text("Hasn't synced cleanly in the last day. It may be asleep, offline or turned off.")
                    .font(.caption).foregroundStyle(Theme.warn.readable)
            }
        }
        .contextMenu {
            if !device.this {
                Button("Remove from Circle…", role: .destructive, action: forget)
            }
        }
    }

    private func row(_ k: String, _ v: String) -> some View {
        HStack {
            Text(k).foregroundStyle(.secondary)
            Spacer()
            Text(v).monospacedDigit()
        }
        .font(.callout)
    }
}
