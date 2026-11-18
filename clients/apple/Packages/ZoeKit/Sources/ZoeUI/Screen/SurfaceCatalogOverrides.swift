#if os(macOS)
import AppKit
import A2UISwiftCore
import A2UISwiftUI
import SwiftUI

/// Catalog overrides shared by live surfaces and the read-only history view.
/// Every override wraps the library's default view, so the library still renders and dispatches the component.
enum SurfaceCatalogOverrides {
    /// `tracker` nil means read-only: every Button is disabled and no input state is tracked.
    @MainActor static func items(tracker: SurfaceActionTracker?) -> [CatalogItem] {
        [
            CatalogItem(name: .button) { context in
                if tracker == nil {
                    return AnyView(context.buildDefaultView().disabled(true))
                }
                guard let props = try? context.node.typedProperties(ButtonProperties.self),
                      case let .event(_, eventContext) = props.action else { return nil }
                let paths = uiBoundPaths(in: eventContext)
                guard !paths.isEmpty else { return nil }
                return AnyView(BoundInputButton(content: context.buildDefaultView(), componentID: context.node.baseComponentId,
                                                context: context.dataContext, paths: paths, tracker: tracker))
            },
            CatalogItem(name: .image) { context in
                guard let props = try? context.node.typedProperties(ImageProperties.self) else { return nil }
                return saveAsWrapped(context, url: context.dataContext.resolve(props.url))
            },
            CatalogItem(name: .video) { context in
                guard let props = try? context.node.typedProperties(VideoProperties.self) else { return nil }
                return saveAsWrapped(context, url: context.dataContext.resolve(props.url))
            },
            CatalogItem(name: .audioPlayer) { context in
                guard let props = try? context.node.typedProperties(AudioPlayerProperties.self) else { return nil }
                return saveAsWrapped(context, url: context.dataContext.resolve(props.url))
            }
        ]
    }

    @MainActor private static func saveAsWrapped(_ context: CatalogItemContext, url: String) -> AnyView {
        let view = context.buildDefaultView()
        guard let source = URL(string: url), source.scheme != nil else { return view }
        return AnyView(view.contextMenu {
            SwiftUI.Button(uiText("Save As…")) { SurfaceMediaSaver.chooseDestination(for: source) }
        })
    }
}

/// Mechanism: the library's Button reads `a2uiActionHandler` from the environment and calls it after dispatch.
/// This wrapper overrides that environment value for its own subtree, marks the component pending on tap,
/// then forwards to the handler it received. Modal triggers receive their handler from the same environment,
/// so trigger activation still reaches Modal. Only event actions with bound context get this wrapper.
private struct BoundInputButton: View {
    let content: AnyView
    let componentID: String
    let context: DataContext
    let paths: [String]
    let tracker: SurfaceActionTracker?

    @Environment(\.a2uiActionHandler) private var parentHandler

    var body: some View {
        let pending = tracker?.isPending(componentID) == true
        let failed = tracker?.didFail(componentID) == true
        HStack(spacing: 6) {
            content
                .disabled(pending)
                .environment(\.a2uiActionHandler) { action in
                    MainActor.assumeIsolated {
                        tracker?.begin(component: componentID, context: context, paths: paths)
                    }
                    parentHandler?(action)
                }
            if pending {
                ProgressView().controlSize(.small)
            }
            if failed {
                Text(uiText("Not sent")).font(.caption).foregroundStyle(.red)
            }
        }
    }
}

/// Asks for a destination first. Only after the user picks one is the URL downloaded and written.
@MainActor enum SurfaceMediaSaver {
    static func chooseDestination(for source: URL) {
        let panel = NSSavePanel()
        panel.nameFieldStringValue = suggestedName(for: source)
        panel.canCreateDirectories = true
        panel.begin { response in
            guard response == .OK, let destination = panel.url else { return }
            Task { @MainActor in await download(source, to: destination) }
        }
    }

    static func suggestedName(for source: URL) -> String {
        let name = source.lastPathComponent.removingPercentEncoding ?? source.lastPathComponent
        return name.isEmpty || name == "/" ? "media" : name
    }

    static func download(_ source: URL, to destination: URL) async {
        do {
            let (data, response) = try await URLSession.shared.data(from: source)
            if let http = response as? HTTPURLResponse, !(200..<300).contains(http.statusCode) {
                throw URLError(.badServerResponse)
            }
            try data.write(to: destination, options: .atomic)
        } catch {
            let alert = NSAlert()
            alert.messageText = uiText("The file could not be saved.")
            alert.informativeText = error.localizedDescription
            alert.runModal()
        }
    }
}
#endif
