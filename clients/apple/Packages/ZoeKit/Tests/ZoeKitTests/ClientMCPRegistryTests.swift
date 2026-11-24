import XCTest
@testable import ZoeKit

final class ClientMCPRegistryTests: XCTestCase {
    func testDefaultsOptionsAndDataDrivenSecondEntryPersist() throws {
        let store = InMemoryMCPRegistryStore()
        var registry = try ClientMCPRegistry(store: store)
        XCTAssertEqual(registry.entries, [.agentDesktop])
        XCTAssertEqual(registry.entries[0].idleTimeoutSeconds, 1800)
        XCTAssertFalse(registry.entries[0].enabled)
        var first = registry.entries[0]
        first.launchOptions["headed"] = true
        let second = ClientMCPEntry(id: "other", command: "other", arguments: ["serve"], launchOptions: ["visible": true], optionArguments: ["visible": "--visible"])
        try registry.replace([first, second])
        let loaded = try ClientMCPRegistry(store: store)
        XCTAssertEqual(loaded.entries[0].effectiveArguments, ["mcp", "--headed"])
        XCTAssertEqual(loaded.entries[1].effectiveArguments, ["serve", "--visible"])
    }

    func testResolverUsesPathAndRejectsMissingCommand() {
        let resolver = PATHMCPExecutableResolver(path: "/usr/bin:/bin")
        XCTAssertNotNil(resolver.resolve("sh"))
        XCTAssertNil(resolver.resolve("zoe-nonexistent-mcp-command"))
    }
}
