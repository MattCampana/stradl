import SwiftUI

struct OverviewView: View {
    @EnvironmentObject var model: AppModel
    @Binding var section: Pane?

    var body: some View {
        if let o = model.overview {
            ScrollView {
                VStack(alignment: .leading, spacing: 16) {
                    hero(o)
                    if let warnings = o.warnings, !warnings.isEmpty {
                        Card(title: "Needs your attention", systemImage: "exclamationmark.triangle") {
                            ForEach(warnings, id: \.self) { w in
                                Text(w).textSelection(.enabled).foregroundStyle(.secondary)
                            }
                        }
                    }
                    stats(o)
                    HStack(alignment: .top, spacing: 16) {
                        circle(o)
                        week(o)
                    }
                }
                .padding(24)
            }
        } else if model.overviewError != nil {
            AgentOffline()
        } else {
            ProgressView("Reading your sessions…").frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }

    private func hero(_ o: Overview) -> some View {
        Card {
            HStack(alignment: .center, spacing: 16) {
                ZStack {
                    Circle().fill(heroColor.opacity(0.15)).frame(width: 56, height: 56)
                    Image(systemName: heroSymbol)
                        .font(.system(size: 24, weight: .semibold))
                        .foregroundStyle(heroColor)
                        .spinning(model.health == .working)
                }
                VStack(alignment: .leading, spacing: 4) {
                    Text(model.statusLine).font(.title2.weight(.semibold)).monospacedDigit()
                    Text("\(o.thisDevice) · Stradl \(o.agent.version) · \(o.devices.count.plural("Mac")) in your Circle")
                        .foregroundStyle(.secondary)
                    if model.progress.syncing || o.agent.syncing {
                        SyncProgressBar(progress: model.progress)
                            .padding(.top, 4)
                    }
                    if let err = o.agent.lastError, !err.isEmpty {
                        Text(err).font(.callout).foregroundStyle(.orange).textSelection(.enabled).lineLimit(3)
                    }
                }
                Spacer()
                VStack(alignment: .trailing, spacing: 8) {
                    Button { model.syncNow() } label: {
                        Label {
                            Text(model.progress.syncing ? "Syncing…" : "Sync Now")
                        } icon: {
                            Image(systemName: "arrow.triangle.2.circlepath").spinning(model.progress.syncing)
                        }
                    }
                    .buttonStyle(.borderedProminent)
                    .disabled(model.busy.contains("sync") || model.progress.syncing)
                    if o.agent.pendingDesktopRestart > 0 {
                        Button { model.restartClaude() } label: {
                            Label("Restart Claude to show \(o.agent.pendingDesktopRestart.plural("new session"))", systemImage: "arrow.clockwise")
                        }
                        .disabled(model.busy.contains("claude"))
                    }
                }
            }
        }
    }

    private var heroSymbol: String {
        switch model.health {
        case .good: return "checkmark"
        case .working: return "arrow.triangle.2.circlepath"
        case .attention: return "exclamationmark"
        case .off: return "pause"
        }
    }

    private var heroColor: Color {
        switch model.health {
        case .good: return .green
        case .working: return .blue
        case .attention: return .orange
        case .off: return .gray
        }
    }

    private func stats(_ o: Overview) -> some View {
        HStack(spacing: 12) {
            Stat(value: "\(o.totals.sessions)", label: "Sessions", symbol: "bubble.left.and.bubble.right")
            Stat(value: "\(o.totals.projects)", label: "Projects", symbol: "folder")
            Stat(value: "\(model.folders.filter { $0.files > 0 }.count)", label: "Project folders", symbol: "externaldrive.badge.icloud")
            Stat(value: "\(o.totals.syncedFiles)", label: "Claude files", symbol: "doc.on.doc")
            Stat(value: o.totals.bytes.bytes, label: "Claude data", symbol: "internaldrive")
        }
    }

    private func circle(_ o: Overview) -> some View {
        Card(title: "Your Circle", systemImage: "person.3.sequence") {
            ForEach(o.devices) { d in
                HStack {
                    StatusDot(health: d.healthy ? .good : .attention)
                    Text(d.name).fontWeight(d.this ? .semibold : .regular)
                    if d.this { Text("this Mac").font(.caption).foregroundStyle(.secondary) }
                    Spacer()
                    Text(d.lastSyncAt.isZero ? "never synced" : d.lastSyncAt.ago)
                        .foregroundStyle(.secondary)
                        .font(.callout)
                }
            }
            if o.devices.isEmpty {
                Text("Macs appear here after their first sync with Stradl 2.1 or later.")
                    .foregroundStyle(.secondary)
            }
            Button("See your Circle") { section = .circle }
                .buttonStyle(.link)
        }
        .frame(maxWidth: .infinity)
    }

    private func week(_ o: Overview) -> some View {
        Card(title: "This week", systemImage: "calendar") {
            let days = o.days.filter { !$0.sessions.isEmpty }.prefix(5)
            if days.isEmpty {
                Text("No sessions in the last 7 days.").foregroundStyle(.secondary)
            }
            ForEach(Array(days)) { day in
                VStack(alignment: .leading, spacing: 4) {
                    Text(day.label).font(.subheadline.weight(.semibold))
                    ForEach(day.sessions.prefix(4)) { s in
                        HStack(alignment: .firstTextBaseline) {
                            Text(s.title).lineLimit(1)
                            Spacer()
                            Text(s.project).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                        }
                        .font(.callout)
                    }
                    if day.sessions.count > 4 {
                        Text("and \(day.sessions.count - 4) more").font(.caption).foregroundStyle(.secondary)
                    }
                }
            }
            Button("All sessions") { section = .sessions }
                .buttonStyle(.link)
        }
        .frame(maxWidth: .infinity)
    }
}

struct Stat: View {
    var value: String
    var label: String
    var symbol: String

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Image(systemName: symbol)
                .foregroundStyle(.secondary)
                .frame(height: 18, alignment: .leading)
            Text(value).font(.title2.weight(.semibold)).monospacedDigit().lineLimit(1).minimumScaleFactor(0.7)
            Text(label).font(.caption).foregroundStyle(.secondary).lineLimit(2).fixedSize(horizontal: false, vertical: true)
        }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .topLeading)
        .background(.background, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
        .overlay(RoundedRectangle(cornerRadius: 12, style: .continuous).stroke(.separator, lineWidth: 0.5))
    }
}

/// The bar under the status while a sync runs: how many files of how many,
/// or an indeterminate bar while it is still comparing.
struct SyncProgressBar: View {
    var progress: SyncProgress

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            if let f = progress.fraction {
                ProgressView(value: f)
                    .progressViewStyle(.linear)
                    .animation(.easeOut(duration: 0.4), value: f)
                Text("\(progress.done.formatted()) of \(progress.total.formatted()) files · \(Int((f * 100).rounded()))%")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .monospacedDigit()
            } else {
                ProgressView()
                    .progressViewStyle(.linear)
                Text("Comparing this Mac with your storage…")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .frame(maxWidth: 520, alignment: .leading)
    }
}

extension View {
    /// Turns the view continuously while active (the sync arrows).
    func spinning(_ active: Bool) -> some View {
        modifier(Spin(active: active))
    }
}

private struct Spin: ViewModifier {
    var active: Bool

    func body(content: Content) -> some View {
        if active {
            TimelineView(.animation) { ctx in
                content.rotationEffect(.degrees(ctx.date.timeIntervalSinceReferenceDate.truncatingRemainder(dividingBy: 1.2) / 1.2 * 360))
            }
        } else {
            content
        }
    }
}

extension View {
    /// A gentle pulse while syncing, on macOS versions that have symbol effects.
    @ViewBuilder
    func symbolEffectIfAvailable(_ active: Bool) -> some View {
        if #available(macOS 14.0, *) {
            self.symbolEffect(.pulse, isActive: active)
        } else {
            self
        }
    }
}
