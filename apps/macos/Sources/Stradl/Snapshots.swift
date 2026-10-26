import AppKit

/// With STRADL_SNAPSHOT_DIR set, the app shows each pane in turn, saves its
/// own window as <pane>.png there, and quits: screenshots for the docs and
/// for checking the layout, without screen-recording permission.
@MainActor
enum Snapshots {
    static func runIfAsked(_ model: AppModel) async {
        guard let dir = ProcessInfo.processInfo.environment["STRADL_SNAPSHOT_DIR"] else { return }
        let out = URL(fileURLWithPath: dir, isDirectory: true)
        try? FileManager.default.createDirectory(at: out, withIntermediateDirectories: true)
        try? await Task.sleep(nanoseconds: 8_000_000_000) // first data
        for pane in Pane.allCases {
            model.pane = pane
            try? await Task.sleep(nanoseconds: 2_500_000_000)
            guard let window = NSApp.windows.first(where: { $0.isVisible && $0.contentView != nil }) else { continue }
            let name = pane.rawValue.lowercased().replacingOccurrences(of: " ", with: "-")
            // The window as the window server composites it (what a person
            // sees, sidebar glass included); an app may capture its own windows.
            if let cg = CGWindowListCreateImage(.null, .optionIncludingWindow, CGWindowID(window.windowNumber), [.boundsIgnoreFraming, .bestResolution]) {
                let rep = NSBitmapImageRep(cgImage: cg)
                try? rep.representation(using: .png, properties: [:])?.write(to: out.appendingPathComponent("\(name).png"))
            } else if let view = window.contentView?.superview ?? window.contentView,
                      let rep = view.bitmapImageRepForCachingDisplay(in: view.bounds) {
                view.cacheDisplay(in: view.bounds, to: rep)
                try? rep.representation(using: .png, properties: [:])?.write(to: out.appendingPathComponent("\(name).png"))
            }
        }
        NSApp.terminate(nil)
    }
}
