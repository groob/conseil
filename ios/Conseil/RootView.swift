import SwiftUI

struct RootView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        Group {
            if model.isConfigured {
                ConversationListView()
            } else {
                NavigationStack {
                    SettingsView(required: true)
                }
            }
        }
    }
}
