import ZoeUI
import ZoeKit
import SwiftUI

@main
struct ZoeMacApp: App {
    @StateObject private var model: ZoeAppModel
    @StateObject private var coordinator: ScreenCoordinator
    @NSApplicationDelegateAdaptor(ZoePushNotificationAppDelegate.self) private var pushDelegate

    init() {
        let model = ZoeAppModel()
        _model = StateObject(wrappedValue: model)
        _coordinator = StateObject(wrappedValue: ScreenCoordinator(model: model))
    }

    var body: some Scene {
        WindowGroup {
            MacSurfaceRoot(model: model, coordinator: coordinator)
                .onOpenURL { model.receiveInstallationLink($0) }
                .task { await model.start() }
        }
        .windowResizability(.contentSize)
        .defaultSize(width: 460, height: 380)
        Settings { MacSettingsView(model: model, coordinator: coordinator) }
    }
}
