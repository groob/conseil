import SwiftUI

struct SettingsView: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss

    let required: Bool

    @State private var serverURL = ""
    @State private var token = ""
    @State private var errorMessage: String?

    var body: some View {
        Form {
            Section {
                TextField("https://name.exe.xyz", text: $serverURL)
                    .textInputAutocapitalization(.never)
                    .keyboardType(.URL)
                    .autocorrectionDisabled()
                SecureField("VM-scoped token", text: $token)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
            } header: {
                Text("Server")
            } footer: {
                Text("Generate a VM token with `ssh exe.dev ssh-key generate-api-key --vm=VM_NAME --label=conseil-ios`, then paste it here. The token stays in Keychain.")
                    .textSelection(.enabled)
            }

            if let errorMessage {
                Section {
                    Text(errorMessage)
                        .foregroundStyle(.red)
                }
            }
        }
        .navigationTitle("Conseil settings")
        .navigationBarTitleDisplayMode(.inline)
        .onAppear {
            serverURL = model.serverURL
            token = model.token
        }
        .toolbar {
            if !required {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
            }
            ToolbarItem(placement: .confirmationAction) {
                Button("Save") { save() }
            }
        }
    }

    private func save() {
        do {
            try model.saveSettings(serverURL: serverURL, token: token)
            errorMessage = nil
            if !required {
                dismiss()
            }
            Task { await model.loadConversations() }
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}
