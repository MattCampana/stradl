import Foundation

// Mirrors of the JSON the agent's dashboard API (internal/dashboard/model.go)
// and the stradl command's --json output (cmd/claude-sync/app_cmd.go) return.
// Fields the app doesn't show are left out; unknown fields are ignored.

struct Overview: Decodable {
    var generatedAt: Date
    var thisDevice: String
    var agent: AgentInfo
    var devices: [Device]
    var totals: Totals
    var days: [Day]
    var sessions: [Session]
    var pending: Pending
    var warnings: [String]?
}

struct AgentInfo: Decodable {
    var running: Bool
    var paused: Bool
    var syncing: Bool
    var lastSyncAt: Date?
    var lastAttemptAt: Date?
    var lastError: String?
    var lastErrorSummary: String?
    var version: String
    var pendingDesktopRestart: Int
}

struct Device: Decodable, Identifiable, Hashable {
    var id: String
    var name: String
    var this: Bool
    var version: String
    var lastSyncAt: Date
    var healthy: Bool
    var sessions: Int
    var recentSessionIds: [String]?
}

struct Totals: Decodable {
    var sessions: Int
    var projects: Int
    var messages: Int
    var files: Int
    var syncedFiles: Int
    var bytes: Int64
}

struct Pending: Decodable {
    var upload: Int
    var download: Int
    var merge: Int
    var deferred: Int
    var delete: Int?
    var trash: Int?
    var conflicts: Int
    var files: [PendingFile]

    var total: Int { upload + download + merge + deferred + (delete ?? 0) + (trash ?? 0) }
}

struct PendingFile: Decodable, Identifiable, Hashable {
    var path: String
    var action: String
    var id: String { action + "\u{0}" + path }
}

struct Session: Decodable, Identifiable, Hashable {
    var id: String
    var title: String
    var project: String
    var projectPath: String
    var branch: String?
    var surface: String
    var startedAt: Date
    var lastActivityAt: Date
    var userTurns: Int
    var assistantTurns: Int
    var firstPrompt: String
    var lastPrompt: String
    var filesTouched: [String]?
    var sizeBytes: Int64
    var syncState: String
    var live: Bool
    var archived: Bool
    var inSidebar: Bool
    var devices: [String]?

    var turns: Int { userTurns + assistantTurns }
}

struct Day: Decodable, Identifiable, Hashable {
    var date: String
    var label: String
    var sessions: [DaySession]
    var id: String { date }
}

struct DaySession: Decodable, Identifiable, Hashable {
    var id: String
    var title: String
    var project: String
    var turns: Int
    var prompts: [String]?
    var filesTouched: [String]?
    var devices: [String]?
    var firstAt: Date
    var lastAt: Date
}

/// `stradl config show --json`
struct AppConfig: Decodable, Equatable {
    var version: String
    var configPath: String
    var stateDir: String
    var provider: String
    var account: String?
    var container: String
    var endpoint: String?
    var auth: String
    var scope: String
    var folderSync: Bool
    var desktopSync: Bool
    var mcpSync: Bool
    var syncBuildArtifacts: Bool
    var syncIntervalSeconds: Int
    var extraFolders: [String]
    var folderExclude: [String]
    var exclude: [String]
    var buildArtifacts: [String]
    var deviceName: String
    var deviceId: String
    var agentInstalled: Bool
    var agentRunning: Bool
    var paused: Bool
    var logPath: String
    var trashDir: String
    var conflictsDir: String
}

/// `stradl folders --json`
struct FolderStat: Decodable, Identifiable, Hashable {
    var path: String
    var abs: String
    var exists: Bool
    var git: Bool
    var files: Int
    var bytes: Int64
    var lastChange: Date?
    var id: String { path }
}

extension JSONDecoder {
    /// Decodes the RFC 3339 timestamps Go writes, with or without fractions.
    static let stradl: JSONDecoder = {
        let d = JSONDecoder()
        let plain = ISO8601DateFormatter()
        let fractional = ISO8601DateFormatter()
        fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        d.dateDecodingStrategy = .custom { decoder in
            let s = try decoder.singleValueContainer().decode(String.self)
            if let date = fractional.date(from: s) ?? plain.date(from: s) {
                return date
            }
            throw DecodingError.dataCorrupted(.init(codingPath: decoder.codingPath, debugDescription: "bad date \(s)"))
        }
        return d
    }()
}
