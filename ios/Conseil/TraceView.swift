import SwiftUI

struct TraceView: View {
    @Environment(\.dismiss) private var dismiss

    let events: [TraceEvent]

    var body: some View {
        List(events.sorted(by: { $0.id < $1.id })) { event in
            DisclosureGroup {
                VStack(alignment: .leading, spacing: 8) {
                    if let runID = event.runID {
                        LabeledContent("Run", value: runID)
                            .font(.caption)
                    }
                    LabeledContent("Time", value: event.createdAt)
                        .font(.caption)
                    Text(event.payload.formatted)
                        .font(.system(.caption, design: .monospaced))
                        .textSelection(.enabled)
                }
                .padding(.vertical, 4)
            } label: {
                VStack(alignment: .leading, spacing: 2) {
                    Text(event.type)
                    Text("Event \(event.id)")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .navigationTitle("Full trace")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .confirmationAction) {
                Button("Done") { dismiss() }
            }
        }
    }
}
