import Testing
@testable import DGWProto

@Test func dgwProtoModuleNameIsStable() {
    #expect(DGWProtoModule.name == "DGWProto")
}
