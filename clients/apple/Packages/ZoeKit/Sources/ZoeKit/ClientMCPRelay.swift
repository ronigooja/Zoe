import Foundation

private actor MCPRelayBridge: ClientWebSocketChannel, ClientMCPTransport {
    weak var transport: ClientWebSocketTransport?
    weak var controller: ClientMCPController?
    // Each relay holds its own demand, so releasing an old relay cannot drop a replacement's demand on a shared socket.
    let demand = "\(ClientWebSocketTransport.mcpChannel)-\(UUID().uuidString)"

    func install(controller: ClientMCPController, transport: ClientWebSocketTransport) {
        self.controller = controller
        self.transport = transport
    }
    func setNeeded(_ needed: Bool) async {
        await transport?.setNeeded(needed, demand: demand)
    }
    func send(_ data: Data) async throws {
        guard let transport else { throw ClientWebSocketError.disconnected }
        try await transport.send(data)
    }
    func opened() async { await controller?.connectionOpened() }
    func closed() async { await controller?.connectionClosed() }
    func received(_ data: Data) async { await controller?.receive(data) }
}

public actor ClientMCPRelay {
    public let controller: ClientMCPController
    private let transport: ClientWebSocketTransport
    private let bridge: MCPRelayBridge

    private init(controller: ClientMCPController, transport: ClientWebSocketTransport, bridge: MCPRelayBridge) {
        self.controller = controller
        self.transport = transport
        self.bridge = bridge
    }

    deinit {
        let bridge = bridge
        Task { await bridge.setNeeded(false) }
    }

    public static func make(api: APIClient, store: any ClientMCPRegistryStore = UserDefaultsMCPRegistryStore(), permissions: any MCPPermissionChecking = MCPMacPermissions(), resolver: any MCPExecutableResolving = PATHMCPExecutableResolver(), diagnostic: @escaping @Sendable (String) -> Void = { _ in }) async throws -> ClientMCPRelay {
        try await make(transport: ClientWebSocketTransport(api: api, diagnostic: diagnostic), store: store, permissions: permissions, resolver: resolver)
    }

    /// Relay on a socket the caller owns. The socket may be shared with other channels.
    public static func make(transport: ClientWebSocketTransport, store: any ClientMCPRegistryStore = UserDefaultsMCPRegistryStore(), permissions: any MCPPermissionChecking = MCPMacPermissions(), resolver: any MCPExecutableResolving = PATHMCPExecutableResolver()) async throws -> ClientMCPRelay {
        try await make(transport: transport, store: store, permissions: permissions, childFactory: ClientMCPController.processFactory(resolver: resolver))
    }

    static func make(api: APIClient, store: any ClientMCPRegistryStore, permissions: any MCPPermissionChecking,
                     childFactory: @escaping ClientMCPController.ChildFactory, socketFactory: ClientWebSocketTransport.SocketFactory? = nil,
                     diagnostic: @escaping @Sendable (String) -> Void = { _ in }) async throws -> ClientMCPRelay {
        try await make(transport: ClientWebSocketTransport(api: api, factory: socketFactory, diagnostic: diagnostic), store: store, permissions: permissions, childFactory: childFactory)
    }

    static func make(transport: ClientWebSocketTransport, store: any ClientMCPRegistryStore, permissions: any MCPPermissionChecking,
                     childFactory: @escaping ClientMCPController.ChildFactory) async throws -> ClientMCPRelay {
        let bridge = MCPRelayBridge()
        let registry = try ClientMCPRegistry(store: store)
        let controller = ClientMCPController(registry: registry, permissions: permissions, transport: bridge, factory: childFactory)
        // Install before registering: registering into an open socket calls `opened()` immediately.
        await bridge.install(controller: controller, transport: transport)
        await transport.register(channel: ClientWebSocketTransport.mcpChannel, handler: bridge)
        return ClientMCPRelay(controller: controller, transport: transport, bridge: bridge)
    }

    public func setEnabled(_ enabled: Bool, id: String) async throws {
        if enabled { try await controller.enable(id) } else { await controller.disable(id) }
    }

    public func setIdleTimeout(_ seconds: TimeInterval, id: String) async throws { try await controller.setIdleTimeout(seconds, for: id) }
    public func setLaunchOption(_ key: String, value: Bool, id: String) async throws { try await controller.setLaunchOption(key, value: value, for: id) }
    public func credentialsChanged() async { await transport.credentialsChanged() }
    /// Permanently stop this relay before dropping or replacing its authenticated connection.
    public func shutdown() async { await controller.shutdown() }
    public func entries() async -> [ClientMCPEntry] { await controller.entries() }
    public func statuses() async -> [String: ClientMCPStatus] { await controller.statuses }
    public func statusUpdates() async -> AsyncStream<[String: ClientMCPStatus]> { await controller.statusUpdates() }
}
