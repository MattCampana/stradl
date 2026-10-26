import Foundation

/// Paths of the Stradl agent's files in ~/.stradl.
enum StradlPaths {
    static let home = FileManager.default.homeDirectoryForCurrentUser
    static let stateDir = home.appendingPathComponent(".stradl", isDirectory: true)
    static let log = stateDir.appendingPathComponent("agent.log")
    static let dashboard = stateDir.appendingPathComponent("dashboard.json")
    static let trash = stateDir.appendingPathComponent("trash", isDirectory: true)
    static let conflicts = stateDir.appendingPathComponent("conflicts", isDirectory: true)
}

/// Runs the stradl command. The app never touches storage or settings files
/// itself: everything goes through the same command people use in Terminal.
enum StradlCLI {
    struct Failure: LocalizedError {
        var message: String
        var errorDescription: String? { message }
    }

    /// The stradl executable: the installed one, else one bundled in the app.
    static var executable: URL? {
        var candidates = [
            StradlPaths.home.appendingPathComponent(".local/bin/stradl"),
            URL(fileURLWithPath: "/opt/homebrew/bin/stradl"),
            URL(fileURLWithPath: "/usr/local/bin/stradl"),
        ]
        if let bundled = Bundle.main.url(forResource: "stradl", withExtension: nil) {
            candidates.append(bundled)
        }
        return candidates.first { FileManager.default.isExecutableFile(atPath: $0.path) }
    }

    /// Runs stradl with args and returns its standard output. A non-zero exit
    /// throws with the command's own error message.
    @discardableResult
    static func run(_ args: [String], timeout: TimeInterval = 120) async throws -> Data {
        guard let exe = executable else {
            throw Failure(message: "The stradl command isn't installed. Install Stradl with the one-line installer from github.com/MattCampana/stradl.")
        }
        return try await withCheckedThrowingContinuation { cont in
            DispatchQueue.global(qos: .userInitiated).async {
                let p = Process()
                p.executableURL = exe
                p.arguments = args
                var env = ProcessInfo.processInfo.environment
                env["NO_COLOR"] = "1"
                p.environment = env
                let out = Pipe(), err = Pipe()
                p.standardOutput = out
                p.standardError = err
                do {
                    try p.run()
                } catch {
                    cont.resume(throwing: error)
                    return
                }
                let timedOut = TimeoutFlag()
                let timer = DispatchWorkItem { if p.isRunning { timedOut.set(); p.terminate() } }
                DispatchQueue.global().asyncAfter(deadline: .now() + timeout, execute: timer)
                // Drain both pipes at once before waiting: a full pipe would
                // block the command, and reading one to the end first could
                // leave it stuck writing the other.
                var errData = Data()
                let errDone = DispatchSemaphore(value: 0)
                DispatchQueue.global(qos: .userInitiated).async {
                    errData = err.fileHandleForReading.readDataToEndOfFile()
                    errDone.signal()
                }
                let outData = out.fileHandleForReading.readDataToEndOfFile()
                errDone.wait()
                p.waitUntilExit()
                timer.cancel()
                if timedOut.isSet {
                    cont.resume(throwing: Failure(message: "stradl \(args.joined(separator: " ")) didn't finish within \(Int(timeout)) seconds and was stopped."))
                } else if p.terminationStatus == 0 {
                    cont.resume(returning: outData)
                } else {
                    let msg = String(decoding: errData.isEmpty ? outData : errData, as: UTF8.self)
                        .replacingOccurrences(of: "Error: ", with: "")
                        .trimmingCharacters(in: .whitespacesAndNewlines)
                    cont.resume(throwing: Failure(message: msg.isEmpty ? "stradl \(args.joined(separator: " ")) failed" : msg))
                }
            }
        }
    }

    static func json<T: Decodable>(_ type: T.Type, _ args: [String]) async throws -> T {
        try JSONDecoder.stradl.decode(T.self, from: try await run(args))
    }
}

/// Set once from the timeout timer, read after the process exits.
private final class TimeoutFlag: @unchecked Sendable {
    private let lock = NSLock()
    private var value = false
    func set() { lock.lock(); value = true; lock.unlock() }
    var isSet: Bool { lock.lock(); defer { lock.unlock() }; return value }
}

/// Talks to the running agent's local dashboard API (127.0.0.1, random port,
/// random key in ~/.stradl/dashboard.json).
enum AgentAPI {
    struct Endpoint {
        var base: URL   // http://127.0.0.1:port
        var token: String
    }

    static func endpoint() -> Endpoint? {
        guard let data = try? Data(contentsOf: StradlPaths.dashboard),
              let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let raw = obj["url"] as? String,
              let comps = URLComponents(string: raw),
              let host = comps.host, host == "127.0.0.1" || host == "::1" || host == "localhost",
              let port = comps.port,
              let token = comps.queryItems?.first(where: { $0.name == "t" })?.value
        else { return nil }
        return Endpoint(base: URL(string: "http://\(host):\(port)")!, token: token)
    }

    private static let session: URLSession = {
        let c = URLSessionConfiguration.ephemeral
        c.timeoutIntervalForRequest = 70 // the first overview scans every transcript
        c.httpCookieAcceptPolicy = .never
        return URLSession(configuration: c)
    }()

    private static func url(_ ep: Endpoint, _ path: String) -> URL {
        var c = URLComponents(url: ep.base.appendingPathComponent(path), resolvingAgainstBaseURL: false)!
        c.queryItems = [URLQueryItem(name: "t", value: ep.token)]
        return c.url!
    }

    static func overview() async throws -> Overview {
        guard let ep = endpoint() else { throw StradlCLI.Failure(message: "The Stradl agent isn't running.") }
        let (data, resp) = try await session.data(from: url(ep, "api/overview"))
        guard (resp as? HTTPURLResponse)?.statusCode == 200 else {
            throw StradlCLI.Failure(message: String(decoding: data, as: UTF8.self))
        }
        return try JSONDecoder.stradl.decode(Overview.self, from: data)
    }

    static func progress() async throws -> SyncProgress {
        guard let ep = endpoint() else { throw StradlCLI.Failure(message: "The Stradl agent isn't running.") }
        var req = URLRequest(url: url(ep, "api/progress"))
        req.timeoutInterval = 3
        let (data, resp) = try await session.data(for: req)
        guard (resp as? HTTPURLResponse)?.statusCode == 200 else {
            throw StradlCLI.Failure(message: String(decoding: data, as: UTF8.self))
        }
        return try JSONDecoder.stradl.decode(SyncProgress.self, from: data)
    }

    /// Asks the agent for an immediate sync. The API only takes requests from
    /// its own origin, which this request names.
    static func syncNow() async throws {
        guard let ep = endpoint() else { throw StradlCLI.Failure(message: "The Stradl agent isn't running.") }
        var req = URLRequest(url: url(ep, "api/sync"))
        req.httpMethod = "POST"
        req.setValue(ep.base.absoluteString, forHTTPHeaderField: "Origin")
        let (data, resp) = try await session.data(for: req)
        guard let code = (resp as? HTTPURLResponse)?.statusCode, (200..<300).contains(code) else {
            throw StradlCLI.Failure(message: String(decoding: data, as: UTF8.self))
        }
    }
}
