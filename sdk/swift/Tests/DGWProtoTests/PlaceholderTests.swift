// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

import Testing
@testable import DGWProto

@Test func dgwProtoModuleNameIsStable() {
    #expect(DGWProtoModule.name == "DGWProto")
}
