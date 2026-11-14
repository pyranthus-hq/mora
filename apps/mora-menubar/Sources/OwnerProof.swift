import Foundation
import LocalAuthentication
import Security

enum OwnerProofError: LocalizedError {
    case unavailable(String)
    case invalidResponse
    case secureEnclaveUnavailable

    var errorDescription: String? {
        switch self {
        case .unavailable(let message): return message
        case .invalidResponse: return "Mora returned an invalid desktop approval response."
        case .secureEnclaveUnavailable: return "Secure Enclave approval is unavailable on this Mac."
        }
    }
}

private struct OwnerProofRequest: Encodable {
    let schema = "mora.review.desktop"
    let schemaVersion = 1
    let op: String
    var kind: String? = nil
    var id: String? = nil
    var action: String? = nil
    var canonicalRevision: String? = nil
    var displayDigest: String? = nil
    var nonce: String? = nil
    var publicKeyP256: String? = nil
    var credentialID: String? = nil
    var signatureDER: String? = nil

    enum CodingKeys: String, CodingKey {
        case schema, op, kind, id, action, nonce
        case schemaVersion = "schema_version"
        case canonicalRevision = "canonical_revision"
        case displayDigest = "display_digest"
        case publicKeyP256 = "public_key_p256_b64"
        case credentialID = "credential_id"
        case signatureDER = "signature_der_b64"
    }
}

struct OwnerProofResponse: Decodable {
    struct Failure: Decodable { let code: String; let message: String }
    let schema: String
    let schemaVersion: Int
    let enrolled: Bool?
    let readOnly: Bool?
    let installationID: String?
    let challengeBytesB64: String?
    let keyID: String?
	let nonce: String?
    let outcome: Outcome?
    let error: Failure?

    struct Outcome: Decodable { let status: String; let applied: Bool?; let indexVisibility: String
        enum CodingKeys: String, CodingKey { case status, applied; case indexVisibility = "index_visibility" }
    }
    enum CodingKeys: String, CodingKey {
        case schema, enrolled, error, outcome, nonce
        case schemaVersion = "schema_version"
        case readOnly = "read_only"
        case installationID = "installation_id"
        case challengeBytesB64 = "challenge_bytes_b64"
        case keyID = "key_id"
    }
}

struct OwnerProofStatus {
    let installationID: String
    let enrolled: Bool
    let readOnly: Bool
}

/// This client has no generic signing API: it sends only Go-issued enrollment
/// and decision bytes to the Secure Enclave after the app rendered the receipt.
struct NativeReviewClient {
    let mora: URL
    private static let frameLimit = ReviewTransport.limit

    func status() throws -> OwnerProofStatus {
        let response = try call(OwnerProofRequest(op: "status"))
        guard let installationID = response.installationID, let enrolled = response.enrolled, let readOnly = response.readOnly else { throw OwnerProofError.invalidResponse }
        return OwnerProofStatus(installationID: installationID, enrolled: enrolled, readOnly: readOnly)
    }

    func enroll(status: OwnerProofStatus) throws {
        let publicKey = try OwnerProof.publicKey(installationID: status.installationID)
        let begin = try call(OwnerProofRequest(op: "enroll.begin", publicKeyP256: publicKey.base64EncodedString()))
        guard let encoded = begin.challengeBytesB64, let bytes = Data(base64Encoded: encoded), let nonce = begin.nonce else { throw OwnerProofError.invalidResponse }
        do {
            let proof = try OwnerProof.sign(bytes, installationID: status.installationID, reason: "Enable Mora desktop approvals")
            let complete = try call(OwnerProofRequest(op: "enroll.complete", nonce: nonce, signatureDER: proof.base64EncodedString()))
            guard complete.error == nil, complete.enrolled == true, complete.readOnly == false else { throw OwnerProofError.unavailable(complete.error?.message ?? "Mora did not accept desktop enrollment.") }
        } catch {
            _ = try? call(OwnerProofRequest(op: "enroll.cancel", nonce: nonce))
            throw error
        }
    }

    func decide(_ action: String, preview: ReviewPreview, status: OwnerProofStatus) throws -> OwnerProofResponse.Outcome {
        let begin = try call(OwnerProofRequest(op: "decision.begin", kind: preview.kind, id: preview.id, action: action, canonicalRevision: preview.canonicalRevision, displayDigest: preview.displayDigest))
        guard let encoded = begin.challengeBytesB64, let bytes = Data(base64Encoded: encoded) else { throw OwnerProofError.invalidResponse }
        let label = action == "keep" ? "Keep" : "Discard"
        guard let nonce = begin.nonce else { throw OwnerProofError.invalidResponse }
        let complete: OwnerProofResponse
        do {
            let proof = try OwnerProof.sign(bytes, installationID: status.installationID, reason: "\(label) this Mora review after checking its displayed preview")
            complete = try call(OwnerProofRequest(op: "decision.complete", nonce: nonce, signatureDER: proof.base64EncodedString()))
        } catch {
            guard let outcome = try? call(OwnerProofRequest(op: "cancel", nonce: nonce)).outcome else {
                throw OwnerProofError.unavailable("Outcome unknown. Refresh before retrying.")
            }
            return outcome
        }
        // The nonce is returned inside the opaque Go challenge, never reconstructed by Swift.
        guard complete.error == nil, let outcome = complete.outcome else { throw OwnerProofError.unavailable(complete.error?.message ?? "Mora did not accept the desktop decision.") }
        return outcome
    }

    private func call(_ request: OwnerProofRequest) throws -> OwnerProofResponse {
        guard FileManager.default.isExecutableFile(atPath: mora.path) else { throw OwnerProofError.unavailable("The bundled Mora binary was not found.") }
        let raw = try JSONEncoder().encode(request)
        guard raw.count <= Self.frameLimit else { throw OwnerProofError.invalidResponse }
        let body: Data
        do { body = try ReviewTransport.run(executable: mora, arguments: ["review", "desktop", "--stdio"], request: raw) } catch ReviewTransportError.timedOut { throw OwnerProofError.unavailable("Mora desktop approval timed out.") } catch { throw OwnerProofError.invalidResponse }
        let decoded = try JSONDecoder().decode(OwnerProofResponse.self, from: body)
        guard decoded.schema == "mora.review.desktop", decoded.schemaVersion == 1 else { throw OwnerProofError.invalidResponse }
        if let error = decoded.error { throw OwnerProofError.unavailable(error.message) }
        return decoded
    }
}

enum OwnerProof {
    private static func tag(_ installationID: String) -> Data { Data("com.pyranthus.mora-status.owner-proof.\(installationID)".utf8) }

    static func publicKey(installationID: String) throws -> Data {
        let key = try key(installationID: installationID, context: nil)
        guard let publicKey = SecKeyCopyPublicKey(key), let value = SecKeyCopyExternalRepresentation(publicKey, nil) as Data?, value.count == 65, value.first == 4 else { throw OwnerProofError.secureEnclaveUnavailable }
        return value
    }

    static func sign(_ bytes: Data, installationID: String, reason: String) throws -> Data {
        let context = LAContext(); context.touchIDAuthenticationAllowableReuseDuration = 0; context.localizedReason = reason
        defer { context.invalidate() }
        let key = try key(installationID: installationID, context: context)
        var error: Unmanaged<CFError>?
        guard let signature = SecKeyCreateSignature(key, .ecdsaSignatureMessageX962SHA256, bytes as CFData, &error) as Data? else { throw (error?.takeRetainedValue() as Error?) ?? OwnerProofError.secureEnclaveUnavailable }
        return signature
    }

    private static func key(installationID: String, context: LAContext?) throws -> SecKey {
        let keyTag = tag(installationID)
        var query: [String: Any] = [kSecClass as String: kSecClassKey, kSecAttrApplicationTag as String: keyTag, kSecAttrKeyType as String: kSecAttrKeyTypeECSECPrimeRandom, kSecAttrTokenID as String: kSecAttrTokenIDSecureEnclave, kSecReturnRef as String: true]
        if let context { query[kSecUseAuthenticationContext as String] = context }
        var result: CFTypeRef?; let found = SecItemCopyMatching(query as CFDictionary, &result)
        if found == errSecSuccess, let result { return result as! SecKey }
        guard found == errSecItemNotFound else { throw OwnerProofError.secureEnclaveUnavailable }
        var failure: Unmanaged<CFError>?
        guard let access = SecAccessControlCreateWithFlags(nil, kSecAttrAccessibleWhenUnlockedThisDeviceOnly, [.privateKeyUsage, .userPresence], &failure) else { throw (failure?.takeRetainedValue() as Error?) ?? OwnerProofError.secureEnclaveUnavailable }
        let attributes: [String: Any] = [kSecAttrKeyType as String: kSecAttrKeyTypeECSECPrimeRandom, kSecAttrKeySizeInBits as String: 256, kSecAttrTokenID as String: kSecAttrTokenIDSecureEnclave, kSecPrivateKeyAttrs as String: [kSecAttrIsPermanent as String: true, kSecAttrApplicationTag as String: keyTag, kSecAttrAccessControl as String: access]]
        guard let key = SecKeyCreateRandomKey(attributes as CFDictionary, &failure) else { throw (failure?.takeRetainedValue() as Error?) ?? OwnerProofError.secureEnclaveUnavailable }
        return key
    }
}
