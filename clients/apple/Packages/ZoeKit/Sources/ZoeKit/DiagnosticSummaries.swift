import Foundation

/// Diagnostic-only formatters never accept message contents or surface payloads.
public enum DiagnosticSummaries {
    public static func seconds(_ value: TimeInterval) -> Int {
        value.isFinite ? Int(min(max(0, value), Double(Int32.max))) : 0
    }
    public static func settings(enabled: Bool, seconds: Double) -> String {
        "\(enabled ? "on" : "off"),\(settingSeconds(seconds))"
    }
    public static func settingsSuffix(enabled: Bool, seconds: Double) -> String {
        enabled ? ";sil=on,\(settingSeconds(seconds))" : ";sil=off"
    }
    private static func settingSeconds(_ value: Double) -> String {
        guard value.isFinite else { return "0" }
        let bounded = min(max(0, value), Double(Int32.max))
        return bounded.rounded() == bounded ? String(Int(bounded)) : String(bounded)
    }
    public static func eventType(_ type: String) -> String {
        eventType(type, limit: 48)
    }
    fileprivate static func eventType(_ type: String, limit: Int) -> String {
        let safe = type.prefix(limit).filter { $0.isASCII && ($0.isLetter || $0.isNumber || "._-/".contains($0)) }
        return safe.isEmpty ? "missing" : String(safe)
    }
    public static func timeout(age: TimeInterval?, timeout: Double, enabled: Bool, interaction: Bool, elapsed: Double) -> String {
        let age = (age ?? 0).isFinite ? max(0, age ?? 0) : 0
        return String("since_last_activity=\(String(format: "%.1f", locale: Locale(identifier: "en_US_POSIX"), min(age, Double(Int32.max))));timeout=\(settingSeconds(timeout));enabled=\(enabled ? 1 : 0);interaction=\(interaction ? 1 : 0);t=\(seconds(elapsed))".prefix(200))
    }
}

public struct SilenceResetDiagnostics {
    private var count = 0
    private var reportedAt: Date?
    public init() {}
    public mutating func note(type: String, now: Date, elapsed: TimeInterval) -> String? {
        count += 1
        guard reportedAt.map({ now.timeIntervalSince($0) >= 10 }) ?? true else { return nil }
        let summary = "n=\(count);last=\(DiagnosticSummaries.eventType(type));t=\(DiagnosticSummaries.seconds(elapsed))"
        count = 0
        reportedAt = now
        return summary
    }
}

public struct EventCensusDiagnostics {
    private struct Count { var total = 0; var active = 0 }
    private var counts: [String: Count] = [:]
    private let completeTypes: Bool
    public private(set) var total = 0
    public init(completeTypes: Bool = false) { self.completeTypes = completeTypes }
    public mutating func note(type: String, activity: Bool = false, preserveSurface: Bool = false) {
        var key = DiagnosticSummaries.eventType(type, limit: completeTypes ? 64 : 48)
        if !completeTypes, counts[key] == nil, counts.keys.filter({ $0 != "other" }).count >= 40, !(preserveSurface && key == "ui.surface") { key = "other" }
        var count = counts[key] ?? Count()
        count.total += 1
        if activity { count.active += 1 }
        counts[key] = count
        total += 1
    }
    public func typeReports() -> [String] {
        let entries = counts.sorted {
            $0.value.total == $1.value.total ? $0.key < $1.key : $0.value.total > $1.value.total
        }.map { key, count in
            "\(key):\(count.total)" + (count.active == 0 ? ":ignored" : ":act=\(count.active)")
        }
        guard !entries.isEmpty else { return [] }
        // At most one entry per part: reserve enough digits for both part numbers.
        let overhead = "part=\(entries.count)/\(entries.count);types=".count
        var chunks: [String] = []
        var chunk = ""
        for entry in entries {
            let candidate = chunk.isEmpty ? entry : chunk + "|" + entry
            if overhead + candidate.count > 200, !chunk.isEmpty {
                chunks.append(chunk)
                chunk = entry
            } else {
                chunk = candidate
            }
        }
        if !chunk.isEmpty { chunks.append(chunk) }
        return chunks.enumerated().map { "part=\($0.offset + 1)/\(chunks.count);types=\($0.element)" }
    }
    public func summary(includeActivity: Bool = true, prefix: String = "types=", prioritizeSurface: Bool = false) -> String {
        let ordered = counts.sorted {
            if prioritizeSurface, ($0.key == "ui.surface") != ($1.key == "ui.surface") { return $0.key == "ui.surface" }
            return $0.value.total == $1.value.total ? $0.key < $1.key : $0.value.total > $1.value.total
        }
        let entries = ordered.map { key, count in
            "\(key):\(count.total)" + (includeActivity ? (count.active == 0 ? ":ignored" : ":act=\(count.active)") : "")
        }
        var kept: [String] = []
        for entry in entries {
            let candidate = kept + [entry]
            let omitted = entries.count - candidate.count
            let suffix = omitted > 0 ? "|+\(omitted)" : ""
            if (prefix + candidate.joined(separator: "|") + suffix).count > 200 { break }
            kept = candidate
        }
        let omitted = entries.count - kept.count
        return prefix + kept.joined(separator: "|") + (omitted > 0 ? "|+\(omitted)" : "")
    }
}

public struct EventsReceivedDiagnostics {
    private var census = EventCensusDiagnostics()
    private var reportedAt: Date
    public init(now: Date) { reportedAt = now }
    public mutating func note(type: String) { census.note(type: type, preserveSurface: true) }
    public mutating func report(now: Date, force: Bool = false) -> String? {
        guard census.total > 0, force || now.timeIntervalSince(reportedAt) >= 30 else { return nil }
        let summary = census.summary(includeActivity: false, prefix: "n=\(census.total);types=", prioritizeSurface: true)
        census = EventCensusDiagnostics()
        reportedAt = now
        return summary
    }
}
