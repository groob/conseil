import XCTest

final class ConseilUITests: XCTestCase {
    @MainActor
    func testSentMessageAndReplyAppearWithoutReopeningConversation() {
        continueAfterFailure = false

        let app = XCUIApplication()
        app.launch()

        XCTAssertTrue(
            app.navigationBars["Conseil"].waitForExistence(timeout: 15),
            "Conversation list did not load. UI hierarchy:\n\(app.debugDescription)"
        )

        let newConversation = app.buttons["new-conversation-button"]
        XCTAssertTrue(newConversation.waitForExistence(timeout: 5))
        newConversation.tap()

        let messageField = app.textFields["message-field"]
        XCTAssertTrue(
            messageField.waitForExistence(timeout: 10),
            "Conversation did not open. UI hierarchy:\n\(app.debugDescription)"
        )

        let marker = "conseil-e2e-\(UUID().uuidString.prefix(8).lowercased())"
        let prompt = "Reply with exactly: \(marker)"
        messageField.tap()
        messageField.typeText(prompt)

        let send = app.buttons["send-button"]
        XCTAssertTrue(send.isEnabled)
        send.tap()

        XCTAssertTrue(
            app.staticTexts[prompt].waitForExistence(timeout: 10),
            "The persisted user message did not appear. UI hierarchy:\n\(app.debugDescription)"
        )
        XCTAssertTrue(
            app.staticTexts[marker].waitForExistence(timeout: 30),
            "The assistant reply did not appear. UI hierarchy:\n\(app.debugDescription)"
        )
    }
}
