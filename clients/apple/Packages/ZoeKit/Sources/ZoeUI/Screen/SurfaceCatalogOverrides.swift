#if os(macOS)
import AppKit
import A2UISwiftCore
import A2UISwiftUI
import SwiftUI

/// Catalog overrides shared by live surfaces and the read-only history view.
/// Every override wraps the library's default view, so the library still renders the component.
enum SurfaceCatalogOverrides {
    @MainActor static func items() -> [CatalogItem] {
        [
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
