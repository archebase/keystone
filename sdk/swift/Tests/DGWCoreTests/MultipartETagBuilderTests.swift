import Foundation
import Testing

@testable import DGWCore

@Test func singlePartContractValueMatchesRust() throws {
    let part = Data(repeating: 0x07, count: 130)

    let etag = try MultipartETagBuilder.build(partData: [part])

    #expect(etag == "\"26E8B6462DD8A802ADBBEAF75F6CBE82-1\"")
}

@Test func threePartContractValueMatchesRust() throws {
    let parts = [
        Data("robot-part-1".utf8),
        Data("robot-part-2".utf8),
        Data("robot-part-3".utf8),
    ]

    let etag = try MultipartETagBuilder.build(partData: parts)

    #expect(etag == "\"C70C8610DA9D2CEE32C8B6194865463B-3\"")
}

@Test func changingPartBoundariesChangesMultipartETag() throws {
    let bytes = Data("robot-part-1robot-part-2robot-part-3".utf8)
    let singlePart = try MultipartETagBuilder.build(partData: [bytes])
    let splitIntoThree = try MultipartETagBuilder.build(partData: [
        Data("robot-part-1".utf8),
        Data("robot-part-2".utf8),
        Data("robot-part-3".utf8),
    ])

    #expect(singlePart != splitIntoThree)
    #expect(singlePart.hasSuffix("-1\""))
    #expect(splitIntoThree.hasSuffix("-3\""))
}

@Test func exactChunkBoundaryRemainsSinglePart() throws {
    let bytes = Data(repeating: 0x2A, count: 256)
    let etag = try MultipartETagBuilder.build(partData: [bytes])

    #expect(etag.hasSuffix("-1\""))
}

@Test func partOrderingChangesMultipartETag() throws {
    let ordered = try MultipartETagBuilder.build(partData: [
        Data("part-a".utf8),
        Data("part-b".utf8),
    ])
    let reversed = try MultipartETagBuilder.build(partData: [
        Data("part-b".utf8),
        Data("part-a".utf8),
    ])

    #expect(ordered != reversed)
}

@Test func emptyPartsAreRejected() {
    let error = #expect(throws: MultipartETagBuilderError.self) {
        try MultipartETagBuilder.build(partData: [])
    }

    #expect(error == .emptyParts)
}
