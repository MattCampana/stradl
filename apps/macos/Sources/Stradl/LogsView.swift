import AppKit
import SwiftUI

/// The agent's log, live, with search and troubleshooting tools.
struct LogsView: View {
    @EnvironmentObject var model: AppModel
    @StateObject private var tail = LogTail(url: StradlPaths.log)
    @State private var query = ""
    @State private var problemsOnly = false
    @State private var follow = true

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 12) {
                Toggle("Problems only", isOn: $problemsOnly).toggleStyle(.themeSwitch)
                Toggle("Follow", isOn: $follow).toggleStyle(.themeSwitch)
                    .help("Keep the newest lines in view")
                Spacer()
                Button { model.copyDiagnostics() } label: { Label("Copy Diagnostics", systemImage: "stethoscope") }
                    .help("Copy settings, status and the recent log, with secrets removed, for a bug report")
                Button { model.reveal(StradlPaths.log) } label: { Label("Show in Finder", systemImage: "folder") }
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 10)
            Divider()
            let lines = visible
            // A lazy stack, not a List: scrolling a table-backed List while
            // it updates re-enters AppKit's table view.
            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 2) {
                        ForEach(lines) { line in
                            LogLineView(line: line).id(line.id)
                        }
                    }
                    .padding(.horizontal, 14)
                    .padding(.vertical, 8)
                }
                .font(.system(.callout, design: .monospaced))
                .onChange(of: tail.lines.last?.id) { _ in
                    // visible, not lines: lines is the list from before the change.
                    if follow, let last = visible.last { proxy.scrollTo(last.id, anchor: .bottom) }
                }
                .onChange(of: problemsOnly) { _ in
                    if let last = visible.last { proxy.scrollTo(last.id, anchor: .bottom) }
                }
            }
            if tail.lines.isEmpty {
                Text("No log yet: it starts when the background agent first runs.")
                    .foregroundStyle(.secondary)
                    .padding()
            }
        }
        .searchable(text: $query, placement: .toolbar, prompt: "Search the log")
        .onAppear { tail.start() }
        .onDisappear { tail.stop() }
    }

    private var visible: [LogLine] {
        tail.lines.filter { l in
            (!problemsOnly || l.level != .info) && (query.isEmpty || l.text.localizedCaseInsensitiveContains(query))
        }
    }
}

struct LogLine: Identifiable, Hashable {
    enum Level { case info, warning, error }
    var id: Int
    var text: String
    var level: Level
}

struct LogLineView: View {
    var line: LogLine

    var body: some View {
        Text(line.text)
            .foregroundStyle(color)
            .textSelection(.enabled)
            .lineLimit(nil)
            .frame(maxWidth: .infinity, alignment: .leading)
    }

    private var color: Color {
        switch line.level {
        case .info: return .primary
        case .warning: return Theme.warn.readable
        case .error: return Theme.bad.readable
        }
    }
}

/// Follows the end of a log file: reads the last part at start, then new
/// bytes every second, and starts over when the file is rotated.
@MainActor
final class LogTail: ObservableObject {
    @Published private(set) var lines: [LogLine] = []
    private let url: URL
    private var offset: UInt64 = 0
    private var inode: UInt64 = 0
    private var nextID = 0
    private var timer: Timer?
    private var partial = ""
    private let keep = 3000

    init(url: URL) { self.url = url }

    func start() {
        guard timer == nil else { return }
        read()
        timer = Timer.scheduledTimer(withTimeInterval: 1, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.read() }
        }
    }

    func stop() {
        timer?.invalidate()
        timer = nil
    }

    private func read() {
        guard let attrs = try? FileManager.default.attributesOfItem(atPath: url.path),
              let size = (attrs[.size] as? NSNumber)?.uint64Value,
              let ino = (attrs[.systemFileNumber] as? NSNumber)?.uint64Value,
              let h = try? FileHandle(forReadingFrom: url) else { return }
        defer { try? h.close() }
        if ino != inode || size < offset {
            // New or rotated file: start from its last 512 KB.
            inode = ino
            offset = size > 512 * 1024 ? size - 512 * 1024 : 0
            lines.removeAll()
            partial = ""
        }
        guard size > offset else { return }
        try? h.seek(toOffset: offset)
        let data = h.readDataToEndOfFile()
        offset += UInt64(data.count)
        let chunk = partial + String(decoding: data, as: UTF8.self)
        var parts = chunk.components(separatedBy: "\n")
        partial = parts.removeLast()
        var added: [LogLine] = []
        for p in parts where !p.isEmpty {
            added.append(LogLine(id: nextID, text: p, level: level(p)))
            nextID += 1
        }
        lines.append(contentsOf: added)
        if lines.count > keep { lines.removeFirst(lines.count - keep) }
    }

    private func level(_ s: String) -> LogLine.Level {
        let l = s.lowercased()
        if l.contains("sync failed") || l.contains("  error:") || l.contains("fatal") || l.contains("panic:")
            || l.range(of: #"errors=[1-9]"#, options: .regularExpression) != nil {
            return .error
        }
        if l.contains("warning") || l.contains("  conflict:") || l.contains("trashed:") || l.contains("couldn't")
            || l.range(of: #"conflicts=[1-9]"#, options: .regularExpression) != nil {
            return .warning
        }
        return .info
    }
}
