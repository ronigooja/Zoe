import Foundation

struct ClientMCPState: Encodable {
    let channel = "mcp"
    let type = "state"
    let enabledMCPIDs: [String]
    let tools: [String: ToolList]

    struct ToolList: Encodable {
        let tools: [JSONValue]
    }

    init(catalogs: [String: [JSONValue]]) {
        enabledMCPIDs = catalogs.keys.sorted()
        tools = catalogs.mapValues { ToolList(tools: $0) }
    }

    enum CodingKeys: String, CodingKey {
        case channel, type, tools
        case enabledMCPIDs = "enabled_mcp_ids"
    }
}

struct ClientMCPCall: Decodable {
    let channel: String
    let type: String
    let requestID: String
    let mcpID: String
    let jsonrpc: JSONValue

    var rpcID: JSONValue? {
        guard case let .object(object) = jsonrpc else { return nil }
        switch object["id"] {
        case .string, .number: return object["id"]
        default: return nil
        }
    }

    var toolName: String? {
        guard rpcID != nil, case let .object(object) = jsonrpc, object["jsonrpc"] == .string("2.0"),
              object["method"] == .string("tools/call"), case let .object(params)? = object["params"],
              case let .string(name)? = params["name"] else { return nil }
        return name
    }

    enum CodingKeys: String, CodingKey {
        case channel, type, jsonrpc
        case requestID = "request_id"
        case mcpID = "mcp_id"
    }
}

struct ClientMCPResult: Encodable {
    let channel = "mcp"
    let type = "result"
    let requestID: String
    let ok: Bool
    let result: JSONValue?
    let error: Failure?

    struct Failure: Encodable {
        let code: String
        let message: String?
    }

    enum CodingKeys: String, CodingKey {
        case channel, type, ok, result, error
        case requestID = "request_id"
    }
}
