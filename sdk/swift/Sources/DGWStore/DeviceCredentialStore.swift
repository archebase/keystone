// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

import Foundation
import Security

package protocol DeviceCredentialStoring: Sendable {
    func load(account: String) throws -> String
    func save(_ credential: String, account: String) throws
    func delete(account: String) throws
    func loadMigratableCredential(excludingAccounts: Set<String>) throws -> StoredDeviceCredential
}

package struct StoredDeviceCredential: Sendable, Equatable {
    package var account: String
    package var credential: String

    package init(account: String, credential: String) {
        self.account = account
        self.credential = credential
    }
}

package extension DeviceCredentialStoring {
    func loadMigratableCredential(excludingAccounts: Set<String>) throws -> StoredDeviceCredential {
        throw DeviceCredentialStoreError.notFound
    }
}

package enum DeviceCredentialStoreError: Error, Sendable, Equatable {
    case notFound
    case invalidCredential
    case keychainFailure(OSStatus)
}

package struct KeychainDeviceCredentialStore: DeviceCredentialStoring {
    private let service: String

    package init(service: String = "com.archebase.data-gateway.device-api-key") {
        self.service = service
    }

    package func load(account: String) throws -> String {
        var query = self.baseQuery(account: account)
        query[kSecReturnData as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne

        var result: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &result)
        if status == errSecItemNotFound {
            throw DeviceCredentialStoreError.notFound
        }
        guard status == errSecSuccess else {
            throw DeviceCredentialStoreError.keychainFailure(status)
        }
        guard let data = result as? Data,
              let credential = String(data: data, encoding: .utf8),
              !credential.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
        else {
            throw DeviceCredentialStoreError.invalidCredential
        }
        return credential
    }

    package func save(_ credential: String, account: String) throws {
        let trimmed = credential.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty, let data = trimmed.data(using: .utf8) else {
            throw DeviceCredentialStoreError.invalidCredential
        }

        let query = self.baseQuery(account: account)
        let updateStatus = SecItemUpdate(
            query as CFDictionary,
            [kSecValueData as String: data] as CFDictionary
        )
        if updateStatus == errSecSuccess {
            return
        }
        guard updateStatus == errSecItemNotFound else {
            throw DeviceCredentialStoreError.keychainFailure(updateStatus)
        }

        var attributes = query
        attributes[kSecValueData as String] = data
        attributes[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
        let addStatus = SecItemAdd(attributes as CFDictionary, nil)
        guard addStatus == errSecSuccess else {
            throw DeviceCredentialStoreError.keychainFailure(addStatus)
        }
    }

    package func delete(account: String) throws {
        let status = SecItemDelete(self.baseQuery(account: account) as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else {
            throw DeviceCredentialStoreError.keychainFailure(status)
        }
    }

    package func loadMigratableCredential(excludingAccounts: Set<String>) throws -> StoredDeviceCredential {
        var query = self.serviceQuery()
        query[kSecReturnAttributes as String] = true
        query[kSecReturnData as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitAll

        var result: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &result)
        if status == errSecItemNotFound {
            throw DeviceCredentialStoreError.notFound
        }
        guard status == errSecSuccess else {
            throw DeviceCredentialStoreError.keychainFailure(status)
        }

        let items: [[String: Any]]
        if let resultItems = result as? [[String: Any]] {
            items = resultItems
        } else if let resultItem = result as? [String: Any] {
            items = [resultItem]
        } else {
            throw DeviceCredentialStoreError.notFound
        }

        let candidates = items.compactMap { item -> CredentialCandidate? in
            guard let account = item[kSecAttrAccount as String] as? String,
                  !excludingAccounts.contains(account),
                  let data = item[kSecValueData as String] as? Data,
                  let credential = String(data: data, encoding: .utf8),
                  !credential.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
                return nil
            }
            return CredentialCandidate(
                credential: StoredDeviceCredential(account: account, credential: credential),
                modifiedAt: item[kSecAttrModificationDate as String] as? Date
            )
        }
        guard let candidate = candidates.sorted(by: { left, right in
            switch (left.modifiedAt, right.modifiedAt) {
            case let (left?, right?):
                return left > right
            case (_?, nil):
                return true
            case (nil, _?):
                return false
            case (nil, nil):
                return left.credential.account > right.credential.account
            }
        }).first else {
            throw DeviceCredentialStoreError.notFound
        }
        return candidate.credential
    }

    private func baseQuery(account: String) -> [String: Any] {
        var query = self.serviceQuery()
        query[kSecAttrAccount as String] = account
        return query
    }

    private func serviceQuery() -> [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: self.service,
        ]
    }

    private struct CredentialCandidate {
        var credential: StoredDeviceCredential
        var modifiedAt: Date?
    }
}
