// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import (
	"testing"
)

func TestBuildMultipartETag_SinglePart(t *testing.T) {
	// This test case mirrors the Rust test: build_local_multipart_etag_matches_expected_single_part_value
	part := make([]byte, 130)
	for i := range part {
		part[i] = 7
	}
	partDigests := [][16]byte{MD5DigestBytes(part)}

	got := BuildMultipartETag(partDigests)
	want := "\"26E8B6462DD8A802ADBBEAF75F6CBE82-1\""
	if got != want {
		t.Errorf("BuildMultipartETag single part = %q, want %q", got, want)
	}
}

func TestBuildMultipartETag_MultiPart(t *testing.T) {
	// This test case mirrors the Rust test: build_local_multipart_etag_matches_expected_multi_part_value
	parts := [][]byte{
		[]byte("robot-part-1"),
		[]byte("robot-part-2"),
		[]byte("robot-part-3"),
	}
	var partDigests [][16]byte
	for _, p := range parts {
		partDigests = append(partDigests, MD5DigestBytes(p))
	}

	got := BuildMultipartETag(partDigests)
	want := "\"C70C8610DA9D2CEE32C8B6194865463B-3\""
	if got != want {
		t.Errorf("BuildMultipartETag multi part = %q, want %q", got, want)
	}
}

func TestCanonicalizedResource_SortsSubresources(t *testing.T) {
	// Mirrors the Rust test: canonicalized_resource_sorts_subresources
	resource := canonicalizedResource(
		"bucket",
		"uploads/file.bin",
		[]queryParam{
			{Key: "uploadId", Value: "upload-1"},
			{Key: "partNumber", Value: "3"},
		},
	)
	want := "/bucket/uploads/file.bin?partNumber=3&uploadId=upload-1"
	if resource != want {
		t.Errorf("canonicalizedResource = %q, want %q", resource, want)
	}
}

func TestCanonicalizedResource_NoQuery(t *testing.T) {
	resource := canonicalizedResource("bucket", "file.bin", nil)
	want := "/bucket/file.bin"
	if resource != want {
		t.Errorf("canonicalizedResource empty = %q, want %q", resource, want)
	}
}

func TestEncodeObjectKeyForPath_PreservesSlashes(t *testing.T) {
	// Mirrors the Rust test: object_key_path_encoding_preserves_slashes
	got := encodeObjectKeyForPath("dir 1/file name.bin")
	want := "dir%201/file%20name.bin"
	if got != want {
		t.Errorf("encodeObjectKeyForPath = %q, want %q", got, want)
	}
}

func TestEncodeQueryString_FlagStyle(t *testing.T) {
	// Mirrors the Rust test: query_encoding_supports_flag_style_parameters
	got := encodeQueryString([]queryParam{{Key: "uploads"}})
	want := "uploads"
	if got != want {
		t.Errorf("encodeQueryString flag = %q, want %q", got, want)
	}
}

func TestBuildCompleteMultipartUploadXML_OrdersParts(t *testing.T) {
	// Mirrors the Rust test: complete_xml_orders_parts_by_part_number
	xml := buildCompleteMultipartUploadXML([]UploadedPart{
		{PartNumber: 2, ETag: "\"etag-2\""},
		{PartNumber: 1, ETag: "\"etag-1\""},
	})
	want := "<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>\"etag-1\"</ETag></Part><Part><PartNumber>2</PartNumber><ETag>\"etag-2\"</ETag></Part></CompleteMultipartUpload>"
	if xml != want {
		t.Errorf("buildCompleteMultipartUploadXML =\n%s\nwant:\n%s", xml, want)
	}
}

func TestBuildRequestURL_VirtualHostedStyle(t *testing.T) {
	got := buildRequestURL("https://oss-cn-shanghai.aliyuncs.com", "my-bucket", "path/to/file.mcap", nil)
	want := "https://my-bucket.oss-cn-shanghai.aliyuncs.com/path/to/file.mcap"
	if got != want {
		t.Errorf("buildRequestURL virtual = %q, want %q", got, want)
	}
}

func TestBuildRequestURL_VirtualHostedStyleWithPort(t *testing.T) {
	got := buildRequestURL("https://oss-cn-shanghai.aliyuncs.com:443", "my-bucket", "path/to/file.mcap", nil)
	want := "https://my-bucket.oss-cn-shanghai.aliyuncs.com:443/path/to/file.mcap"
	if got != want {
		t.Errorf("buildRequestURL virtual with port = %q, want %q", got, want)
	}
}

func TestBuildRequestURL_PathStyle(t *testing.T) {
	got := buildRequestURL("http://127.0.0.1:9000", "my-bucket", "file.mcap", nil)
	want := "http://127.0.0.1:9000/my-bucket/file.mcap"
	if got != want {
		t.Errorf("buildRequestURL path = %q, want %q", got, want)
	}
}

func TestBuildRequestURL_AddsHTTPS(t *testing.T) {
	got := buildRequestURL("oss-cn-shanghai.aliyuncs.com", "bucket", "key", nil)
	if got[:8] != "https://" {
		t.Errorf("buildRequestURL should add https://, got %q", got)
	}
}

func TestSignOSSV1_NotEmpty(t *testing.T) {
	sig := signOSSV1("GET\n\n\nMon, 01 Jan 2024 00:00:00 GMT\n/bucket/key", "secret")
	if sig == "" {
		t.Error("signOSSV1 returned empty string")
	}
}

func TestCanonicalizedOSSHeaders(t *testing.T) {
	headers := map[string]string{
		"x-oss-security-token": "token123",
	}
	got := canonicalizedOSSHeaders(headers)
	want := "x-oss-security-token:token123\n"
	if got != want {
		t.Errorf("canonicalizedOSSHeaders = %q, want %q", got, want)
	}
}

func TestCanonicalizedOSSHeaders_Empty(t *testing.T) {
	got := canonicalizedOSSHeaders(nil)
	if got != "" {
		t.Errorf("canonicalizedOSSHeaders empty = %q, want empty", got)
	}
}
