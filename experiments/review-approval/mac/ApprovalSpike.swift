import AppKit
import CryptoKit
import Foundation
import LocalAuthentication
import Security

private struct Preview: Encodable, Sendable {
    let id: String
    let title: String
    let body: String
    let scope: String
    let type = "Fact"
    let source = "Synthetic fixture"
    let provenance = "Agent-authored"
    let kind: String
    let state: String

    var display: String {
        """
        Item: \(id)
        Title: \(title)
        Text: \(body)
        Scope: \(scope)
        Type: \(type)
        Source: \(source)
        Provenance: \(provenance)
        Queue: \(kind)
        State: \(state)
        """
    }

    func revision() throws -> String {
        try sha256(canonicalBytes(self))
    }
}

private func canonicalBytes(_ value: some Encodable) throws -> Data {
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.sortedKeys]
    return try encoder.encode(value)
}

private func sha256(_ data: Data) -> String {
    SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
}

private struct Challenge: Encodable, Equatable {
    let session: UUID
    let nonce: UUID
    let revision: String
    let scope: String
    let action: String
}

private struct Verifier {
    let publicKey: P256.Signing.PublicKey
    private let session = UUID()
    private var pending: (challenge: Challenge, expires: ContinuousClock.Instant)?
    private(set) var committed: Challenge?

    init(publicKey: P256.Signing.PublicKey) {
        self.publicKey = publicKey
    }

    mutating func issue(
        for preview: Preview, now: ContinuousClock.Instant = .now
    ) throws -> Challenge {
        let challenge = Challenge(
            session: session, nonce: UUID(), revision: try preview.revision(),
            scope: preview.scope, action: "Keep"
        )
        pending = (challenge, now.advanced(by: .seconds(90)))
        return challenge
    }

    mutating func cancel() {
        pending = nil
    }

    mutating func approve(
        _ challenge: Challenge, signature: Data?, current: Preview,
        now: ContinuousClock.Instant = .now
    ) throws -> Bool {
        guard committed == nil, let pending, pending.challenge == challenge,
              now < pending.expires, challenge.scope == current.scope,
              challenge.revision == (try current.revision()),
              let signature,
              let proof = try? P256.Signing.ECDSASignature(derRepresentation: signature),
              publicKey.isValidSignature(proof, for: try canonicalBytes(challenge))
        else { return false }
        self.pending = nil
        committed = challenge
        return true
    }
}

private let saved = Preview(
    id: "synthetic-saved", title: "Use the compass",
    body: "The fixture places a red compass next to a paper map.",
    scope: "project:synthetic", kind: "Saved", state: "Tentative"
)

private let held = Preview(
    id: "synthetic-held", title: "Use the paper map",
    body: "The fixture places a paper map on the top shelf.",
    scope: "project:synthetic", kind: "Held", state: "Pending review"
)

private struct FixtureFailure: Error {
    let name: String
}

private func check(_ name: String, _ condition: @autoclosure () throws -> Bool) throws {
    guard try condition() else { throw FixtureFailure(name: name) }
    print("PASS \(name)")
}

private func selfTest() throws {
    // Software keys exercise the verifier. This path never invokes OS authentication.
    let key = P256.Signing.PrivateKey()
    let wrongKey = P256.Signing.PrivateKey()
    let now = ContinuousClock.now
    for preview in [saved, held] {
        let tag = preview.id
        var verifier = Verifier(publicKey: key.publicKey)
        let challenge = try verifier.issue(for: preview, now: now)
        let bytes = try canonicalBytes(challenge)
        let valid = try key.signature(for: bytes).derRepresentation
        let foreign = try wrongKey.signature(for: bytes).derRepresentation
        let stale = Preview(
            id: preview.id, title: preview.title, body: "Changed fixture.",
            scope: preview.scope, kind: preview.kind, state: preview.state
        )
        let moved = Preview(
            id: preview.id, title: preview.title, body: preview.body,
            scope: "project:other", kind: preview.kind, state: preview.state
        )
        let changedAction = Challenge(
            session: challenge.session, nonce: challenge.nonce, revision: challenge.revision,
            scope: challenge.scope, action: "Discard"
        )
        let actionSignature = try key.signature(for: canonicalBytes(changedAction)).derRepresentation
        try check("\(tag) missing proof", !verifier.approve(challenge, signature: nil, current: preview))
        try check("\(tag) malformed proof", !verifier.approve(challenge, signature: Data([0]), current: preview))
        try check("\(tag) wrong signer", !verifier.approve(challenge, signature: foreign, current: preview))
        try check("\(tag) stale preview", !verifier.approve(challenge, signature: valid, current: stale))
        try check("\(tag) changed scope", !verifier.approve(challenge, signature: valid, current: moved))
        try check("\(tag) changed action", !verifier.approve(changedAction, signature: actionSignature, current: preview))
        try check("\(tag) expired", !verifier.approve(challenge, signature: valid, current: preview, now: now.advanced(by: .seconds(90))))
        try check("\(tag) no decision after refusal", verifier.committed == nil)
        try check("\(tag) valid fixture signature", verifier.approve(challenge, signature: valid, current: preview))
        try check("\(tag) repeated submission", !verifier.approve(challenge, signature: valid, current: preview))
        var restarted = Verifier(publicKey: key.publicKey)
        _ = try restarted.issue(for: preview, now: now)
        try check("\(tag) prior-session proof", !restarted.approve(challenge, signature: valid, current: preview))
        var cancelled = Verifier(publicKey: key.publicKey)
        let cancelledChallenge = try cancelled.issue(for: preview, now: now)
        let cancelledProof = try key.signature(for: canonicalBytes(cancelledChallenge)).derRepresentation
        cancelled.cancel()
        try check("\(tag) Cancel", !cancelled.approve(cancelledChallenge, signature: cancelledProof, current: preview) && cancelled.committed == nil)
    }
}

private func platformAvailable() -> Bool {
    SecureEnclave.isAvailable && LAContext().canEvaluatePolicy(.deviceOwnerAuthentication, error: nil)
}

@MainActor
private func review(_ preview: Preview) throws {
    let revision = try preview.revision()
    let available = platformAvailable()
    let app = NSApplication.shared
    app.setActivationPolicy(.regular)
    app.activate()

    let alert = NSAlert()
    alert.messageText = available ? "Review synthetic Mora note" : "Synthetic Mora note is read-only"
    alert.informativeText = """
    \(preview.display)
    Revision: \(revision)
    Requested action: Keep
    """
    let cancel = alert.addButton(withTitle: available ? "Cancel" : "Close")
    cancel.keyEquivalent = "\r"
    if available {
        alert.addButton(withTitle: "Keep with macOS approval").keyEquivalent = ""
    }
    let selection = alert.runModal()
    guard available else {
        print("READ_ONLY platform verification unavailable; decisions=0")
        return
    }
    guard selection == .alertSecondButtonReturn else {
        print("CANCELLED decisions=0")
        return
    }

    let context = LAContext()
    context.touchIDAuthenticationAllowableReuseDuration = 0
    context.localizedReason = "Keep the synthetic Mora note in \(preview.scope), revision \(revision.prefix(12))"
    defer { context.invalidate() }
    do {
        guard let access = SecAccessControlCreateWithFlags(
            nil, kSecAttrAccessibleWhenUnlockedThisDeviceOnly, [.privateKeyUsage, .userPresence], nil
        ) else {
            print("READ_ONLY key access policy unavailable; decisions=0")
            return
        }
        // The demo enrolls one ephemeral test key inside this run and exposes no API to replace it.
        let enrolled = try SecureEnclave.P256.Signing.PrivateKey(accessControl: access)
        var verifier = Verifier(publicKey: enrolled.publicKey)
        let challenge = try verifier.issue(for: preview)
        let signingKey = try SecureEnclave.P256.Signing.PrivateKey(
            dataRepresentation: enrolled.dataRepresentation, authenticationContext: context
        )
        let signature = try signingKey.signature(for: canonicalBytes(challenge)).derRepresentation
        guard try verifier.approve(challenge, signature: signature, current: preview) else {
            print("READ_ONLY verification refused; decisions=0")
            return
        }
        print("VERIFIED_SYNTHETIC_DECISION action=Keep item=\(preview.id) scope=\(preview.scope) revision=\(revision) decisions=1")
    } catch {
        let failure = error as NSError
        print("READ_ONLY authentication declined or unavailable (\(failure.domain):\(failure.code)); decisions=0")
    }
}

@main
private enum ApprovalSpike {
    @MainActor
    static func main() {
        do {
            switch Array(CommandLine.arguments.dropFirst()) {
            case ["--self-test"]:
                try selfTest()
            case ["--preflight"]:
                print("secure_enclave=\(SecureEnclave.isAvailable) owner_auth_available=\(platformAvailable()) decisions=0")
            case ["--saved"]:
                try review(saved)
            case ["--held"]:
                try review(held)
            default:
                print("Usage: mora-review-mac-spike --self-test | --preflight | --saved | --held")
                exit(2)
            }
        } catch let failure as FixtureFailure {
            print("FAIL \(failure.name)")
            exit(1)
        } catch {
            print("FAIL \(error)")
            exit(1)
        }
    }
}
