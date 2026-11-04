import AVFAudio
import Foundation
import WebRTC

/// Owns the local media peer connection. Signaling and durable voice turn handling remain server-backed.
public protocol WebRTCTransport: Sendable {
    func createOffer() async throws -> String
    func applyAnswer(_ answerSDP: String) async throws
    func sendData(_ data: Data) throws
    func close() async
}

public enum WebRTCTransportError: Error, LocalizedError, Sendable {
    case peerConnectionUnavailable
    case offerFailed(String)
    case descriptionFailed(String)
    case dataChannelUnavailable
    case iceGatheringTimedOut
    case connectionTimedOut

    public var errorDescription: String? {
        switch self {
        case .peerConnectionUnavailable: "WebRTC could not create a media peer connection."
        case let .offerFailed(message): "WebRTC could not create an SDP offer: \(message)"
        case let .descriptionFailed(message): "WebRTC could not apply the SDP description: \(message)"
        case .dataChannelUnavailable: "The voice data channel is not ready."
        case .iceGatheringTimedOut: "WebRTC did not finish gathering network candidates in time."
        case .connectionTimedOut: "WebRTC did not connect its audio peer and voice data channel in time."
        }
    }
}

/// A concrete libwebrtc peer connection with microphone capture, remote audio reception,
/// an ordered voice data channel, and non-trickle SDP for the selected gateway contract.
public final class LibWebRTCTransport: NSObject, WebRTCTransport, @unchecked Sendable {
    private let factory: RTCPeerConnectionFactory
    private let peerConnection: RTCPeerConnection
    private let localAudioTrack: RTCAudioTrack
    private let localAudioSender: RTCRtpSender?
    private let dataChannel: RTCDataChannel
    private let lock = NSLock()
    private var isClosed = false
#if DEBUG
    private var dataChannelStateOverrideForTesting: RTCDataChannelState?
#endif

    public var onDataMessage: (@Sendable (Data) -> Void)?
    public var onConnectionState: (@Sendable (RTCPeerConnectionState) -> Void)?
    public var onDataChannelState: (@Sendable (RTCDataChannelState) -> Void)?

    public init(iceServers: [RTCIceServer] = [], dataChannelLabel: String = "oai-events") throws {
        RTCInitializeSSL()
        let encoder = RTCDefaultVideoEncoderFactory()
        let decoder = RTCDefaultVideoDecoderFactory()
        factory = RTCPeerConnectionFactory(encoderFactory: encoder, decoderFactory: decoder)

        let configuration = RTCConfiguration()
        configuration.iceServers = iceServers
        configuration.sdpSemantics = .unifiedPlan
        configuration.bundlePolicy = .maxBundle
        configuration.rtcpMuxPolicy = .require

        let constraints = RTCMediaConstraints(mandatoryConstraints: nil, optionalConstraints: ["DtlsSrtpKeyAgreement": "true"])
        guard let connection = factory.peerConnection(with: configuration, constraints: constraints, delegate: nil) else {
            throw WebRTCTransportError.peerConnectionUnavailable
        }
        peerConnection = connection

        let audioSource = factory.audioSource(with: RTCMediaConstraints(mandatoryConstraints: nil, optionalConstraints: nil))
        localAudioTrack = factory.audioTrack(with: audioSource, trackId: "zoe-microphone")
        localAudioTrack.isEnabled = true
        localAudioSender = connection.add(localAudioTrack, streamIds: ["zoe-audio"])

        let channelConfig = RTCDataChannelConfiguration()
        channelConfig.isOrdered = true
        channelConfig.isNegotiated = true
        channelConfig.channelId = 0
        guard let channel = connection.dataChannel(forLabel: dataChannelLabel, configuration: channelConfig) else {
            connection.close()
            throw WebRTCTransportError.dataChannelUnavailable
        }
        dataChannel = channel
        super.init()
        connection.delegate = self
        channel.delegate = self
    }

    public func createOffer() async throws -> String {
        let constraints = RTCMediaConstraints(
            mandatoryConstraints: ["OfferToReceiveAudio": "true", "OfferToReceiveVideo": "false"],
            optionalConstraints: nil
        )
        let offer: RTCSessionDescription = try await withCheckedThrowingContinuation { continuation in
            peerConnection.offer(for: constraints) { description, error in
                if let error { continuation.resume(throwing: WebRTCTransportError.offerFailed(error.localizedDescription)) }
                else if let description { continuation.resume(returning: description) }
                else { continuation.resume(throwing: WebRTCTransportError.offerFailed("The SDK returned no description.")) }
            }
        }
        try await setLocalDescription(offer)
        try await waitForICEGathering()
        guard let sdp = peerConnection.localDescription?.sdp, !sdp.isEmpty else {
            throw WebRTCTransportError.offerFailed("The local description is empty.")
        }
        return sdp
    }

    public func applyAnswer(_ answerSDP: String) async throws {
        guard !answerSDP.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
            throw WebRTCTransportError.descriptionFailed("The answer is empty.")
        }
        let answer = RTCSessionDescription(type: .answer, sdp: answerSDP)
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            peerConnection.setRemoteDescription(answer) { error in
                if let error { continuation.resume(throwing: WebRTCTransportError.descriptionFailed(error.localizedDescription)) }
                else { continuation.resume() }
            }
        }
    }

    public func sendData(_ data: Data) throws {
        guard dataChannel.readyState == .open else { throw WebRTCTransportError.dataChannelUnavailable }
        guard dataChannel.sendData(RTCDataBuffer(data: data, isBinary: false)) else {
            throw WebRTCTransportError.dataChannelUnavailable
        }
    }

    public func waitUntilConnected() async throws {
        let deadline = ContinuousClock.now + .seconds(30)
        while (peerConnection.iceConnectionState != .connected && peerConnection.iceConnectionState != .completed) || dataChannel.readyState != .open {
            if peerConnection.iceConnectionState == .disconnected || peerConnection.iceConnectionState == .failed || peerConnection.iceConnectionState == .closed || dataChannel.readyState == .closed {
                throw WebRTCTransportError.dataChannelUnavailable
            }
            guard ContinuousClock.now < deadline else { throw WebRTCTransportError.connectionTimedOut }
            try await Task.sleep(for: .milliseconds(100))
        }
    }

    public func close() async {
        closeImmediately()
    }

    private func closeImmediately() {
        lock.lock()
        guard !isClosed else { lock.unlock(); return }
        isClosed = true
        localAudioTrack.isEnabled = false
        dataChannel.delegate = nil
        dataChannel.close()
        peerConnection.delegate = nil
        if let localAudioSender { _ = peerConnection.removeTrack(localAudioSender) }
        peerConnection.close()
        lock.unlock()
    }

    private func setLocalDescription(_ description: RTCSessionDescription) async throws {
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            peerConnection.setLocalDescription(description) { error in
                if let error { continuation.resume(throwing: WebRTCTransportError.descriptionFailed(error.localizedDescription)) }
                else { continuation.resume() }
            }
        }
    }

    private func waitForICEGathering() async throws {
        let deadline = ContinuousClock.now + .seconds(20)
        while peerConnection.iceGatheringState != .complete {
            guard ContinuousClock.now < deadline else {
                // Non-trickle signaling normally waits for `complete`, but a
                // STUN server can remain in `gathering` indefinitely while
                // host candidates are already usable. Continue with the
                // candidates that have been gathered instead of discarding a
                // valid offer solely because the state never settled.
                let hasCandidate = peerConnection.localDescription?.sdp.contains("a=candidate:") == true
                if hasCandidate { return }
                throw WebRTCTransportError.iceGatheringTimedOut
            }
            try await Task.sleep(for: .milliseconds(100))
        }
    }
}

extension LibWebRTCTransport: RTCPeerConnectionDelegate {
    public func peerConnection(_ peerConnection: RTCPeerConnection, didChange stateChanged: RTCSignalingState) {}
    public func peerConnection(_ peerConnection: RTCPeerConnection, didAdd stream: RTCMediaStream) {}
    public func peerConnection(_ peerConnection: RTCPeerConnection, didRemove stream: RTCMediaStream) {}
    public func peerConnectionShouldNegotiate(_ peerConnection: RTCPeerConnection) {}
    public func peerConnection(_ peerConnection: RTCPeerConnection, didChange newState: RTCIceConnectionState) {}
    public func peerConnection(_ peerConnection: RTCPeerConnection, didChange newState: RTCIceGatheringState) {}
    public func peerConnection(_ peerConnection: RTCPeerConnection, didGenerate candidate: RTCIceCandidate) {}
    public func peerConnection(_ peerConnection: RTCPeerConnection, didRemove candidates: [RTCIceCandidate]) {}
    public func peerConnection(_ peerConnection: RTCPeerConnection, didOpen dataChannel: RTCDataChannel) {}
    public func peerConnection(_ peerConnection: RTCPeerConnection, didChange stateChanged: RTCPeerConnectionState) {
        onConnectionState?(stateChanged)
    }
    public func peerConnection(_ peerConnection: RTCPeerConnection, didStartReceivingOn transceiver: RTCRtpTransceiver) {}
}

extension LibWebRTCTransport: RTCDataChannelDelegate {
    public func dataChannelDidChangeState(_ dataChannel: RTCDataChannel) {
        #if DEBUG
        onDataChannelState?(dataChannelStateOverrideForTesting ?? dataChannel.readyState)
        #else
        onDataChannelState?(dataChannel.readyState)
        #endif
    }

#if DEBUG
    func driveDataChannelStateForTesting(_ state: RTCDataChannelState) {
        dataChannelStateOverrideForTesting = state
        dataChannelDidChangeState(dataChannel)
        dataChannelStateOverrideForTesting = nil
    }

    func driveConnectionStateForTesting(_ state: RTCPeerConnectionState) {
        peerConnection(self.peerConnection, didChange: state)
    }
#endif
    public func dataChannel(_ dataChannel: RTCDataChannel, didReceiveMessageWith buffer: RTCDataBuffer) {
        onDataMessage?(buffer.data)
    }
}

public protocol VoiceAudioSessionAdapter: AnyObject {
    var onLifecycleEvent: (@Sendable (VoiceAudioLifecycleEvent) -> Void)? { get set }
    func activate() throws
    func deactivate() throws
}

public enum VoiceAudioLifecycleEvent: Sendable {
    case interrupted
    case routeUnavailable
}

#if os(iOS)
public final class AppleMobileAudioSessionAdapter: VoiceAudioSessionAdapter {
    private let session = AVAudioSession.sharedInstance()
    public var onLifecycleEvent: (@Sendable (VoiceAudioLifecycleEvent) -> Void)?
    private var observers: [NSObjectProtocol] = []

    public init() {}

    public func activate() throws {
        observeLifecycleEvents()
        try session.setCategory(.playAndRecord, mode: .voiceChat, options: [.allowBluetooth, .defaultToSpeaker])
        try session.setActive(true)
    }

    public func deactivate() throws {
        removeLifecycleObservers()
        try session.setActive(false, options: .notifyOthersOnDeactivation)
    }

    private func observeLifecycleEvents() {
        guard observers.isEmpty else { return }
        let center = NotificationCenter.default
        observers.append(center.addObserver(forName: AVAudioSession.interruptionNotification, object: session, queue: .main) { [weak self] notification in
            guard let raw = notification.userInfo?[AVAudioSessionInterruptionTypeKey] as? UInt,
                  AVAudioSession.InterruptionType(rawValue: raw) == .began else { return }
            self?.onLifecycleEvent?(.interrupted)
        })
        observers.append(center.addObserver(forName: AVAudioSession.routeChangeNotification, object: session, queue: .main) { [weak self] notification in
            guard let raw = notification.userInfo?[AVAudioSessionRouteChangeReasonKey] as? UInt,
                  AVAudioSession.RouteChangeReason(rawValue: raw) == .oldDeviceUnavailable else { return }
            self?.onLifecycleEvent?(.routeUnavailable)
        })
    }

    private func removeLifecycleObservers() {
        for observer in observers { NotificationCenter.default.removeObserver(observer) }
        observers.removeAll()
    }

    deinit { removeLifecycleObservers() }
}
#elseif os(macOS)
public final class AppleDesktopAudioSessionAdapter: VoiceAudioSessionAdapter {
    public var onLifecycleEvent: (@Sendable (VoiceAudioLifecycleEvent) -> Void)?
    public init() {}

    public func activate() throws {}

    public func deactivate() throws {}
}
#endif

public enum VoiceIntegrationStatus: Equatable, Sendable {
    case unavailable(reason: String)
    case idle
    case connecting
    case active(sessionID: String, mediaGeneration: Int64)
    case interrupted(reason: String)
    case reconnecting(reason: String)
    case stopped
}

enum VoiceMediaLoss {
    case connection(RTCPeerConnectionState)
    case audio(VoiceAudioLifecycleEvent)
    case dataChannel(RTCDataChannelState)
    case leaseRenewalFailed(String)

    var message: String? {
        switch self {
        case let .connection(state):
            switch state {
            case .disconnected: "The WebRTC connection was lost."
            case .failed: "The WebRTC connection failed."
            case .closed: "The WebRTC connection closed."
            default: nil
            }
        case .audio(.interrupted): "The system interrupted the voice audio session."
        case .audio(.routeUnavailable): "The previous voice audio route became unavailable."
        case .dataChannel(.closing), .dataChannel(.closed): "The voice data channel was lost."
        case .dataChannel: nil
        case let .leaseRenewalFailed(message): "The voice lease heartbeat failed: \(message)"
        }
    }
}

struct VoiceMediaEpochFence {
    private(set) var current: UInt64 = 0

    mutating func beginAttempt() -> UInt64 {
        current &+= 1
        return current
    }

    func accepts(_ epoch: UInt64) -> Bool {
        epoch == current
    }

    mutating func stop(_ epoch: UInt64) -> Bool {
        guard accepts(epoch) else { return false }
        current &+= 1
        return true
    }
}

@MainActor
final class VoiceHeartbeatLease {
    private var task: Task<Void, Never>?

    var isActive: Bool { task != nil && task?.isCancelled == false }

    func install(_ task: Task<Void, Never>) {
        self.task?.cancel()
        self.task = task
    }

    func cancel() {
        task?.cancel()
        task = nil
    }
}
