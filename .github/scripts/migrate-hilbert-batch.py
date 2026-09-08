# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

import argparse
import base64
import concurrent.futures
import hashlib
import hmac
import json
import os
import sys
import time
from datetime import datetime, timezone
from tempfile import NamedTemporaryFile
from urllib.parse import quote, urlencode, urlparse

import requests
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

SOURCE_WORKSPACE = 4
TARGET_WORKSPACE = 6
TARGET_BAG_PREFIX = "migration-prod-"
PART_SIZE = 64 * 1024 * 1024
MULTIPART_THRESHOLD = 128 * 1024 * 1024


def encrypted_digest(password, material):
    raw = base64.b64decode(material)
    value = hashlib.sha256(password.encode()).hexdigest().encode()
    return base64.b64encode(AESGCM(raw[:32]).encrypt(raw[32:], value, None)).decode()


def login(base, username, password):
    session = requests.Session()
    material = session.get(base + "/v1/console/nonce/generate", timeout=30).json()["data"]
    response = session.post(
        base + "/v1/console/account/login",
        json={"code": username, "nonceId": material["id"], "cipherDigest": encrypted_digest(password, material["randomKey"])},
        timeout=30,
    ).json()
    if response.get("code") != 0:
        raise RuntimeError("Hilbert login failed")
    session.headers["Authorization"] = "Bearer " + response["data"]["sessionKey"]
    return session


def api(session, base, method, path, **kwargs):
    for attempt in range(3):
        response = session.request(method, base + path, timeout=120, **kwargs)
        try:
            payload = response.json()
        except ValueError as error:
            if attempt < 2:
                time.sleep(2**attempt)
                continue
            raise RuntimeError(f"non-JSON response from {method} {path}") from error
        if response.status_code < 300 and payload.get("code") == 0:
            return payload["data"]
        if response.status_code >= 500 and attempt < 2:
            time.sleep(2**attempt)
            continue
        raise RuntimeError(f"Hilbert API failed: {method} {path} HTTP {response.status_code} body={response.text[:1000]!r}")
    raise RuntimeError(f"Hilbert API retries exhausted: {method} {path}")


def query_raw(session, base, workspace_id, raw_data_id=None, bag_name=None):
    params = {"workspaceId": workspace_id, "pageNum": 1, "pageSize": 200}
    if raw_data_id is not None:
        params["id"] = raw_data_id
    if bag_name is not None:
        params["bagName"] = bag_name
    return api(session, base, "GET", "/v1/data-collection/raw-data/query", params=params).get("records", [])


def tos_target(credentials):
    secret = credentials["credentials"]
    parsed = urlparse(credentials["endpoint"] if credentials["endpoint"].startswith("http") else "https://" + credentials["endpoint"])
    host = parsed.hostname or ""
    if host.startswith("tos-s3-") and host.endswith(".ivolces.com"):
        region = host[len("tos-s3-") : -len(".ivolces.com")]
        endpoint = f"tos-{region}.ivolces.com"
    else:
        region = credentials.get("region") or "cn-beijing"
        endpoint = host
    return secret, endpoint, region


def signed_request(credentials, method, payload, payload_hash, query=None, content_type="", content_length=None):
    secret, endpoint, region = tos_target(credentials)
    now = datetime.now(timezone.utc)
    timestamp = now.strftime("%Y%m%dT%H%M%SZ")
    short_date = timestamp[:8]
    host = f"{credentials['bucket']}.{endpoint}"
    path = "/" + quote(credentials["key"].lstrip("/"), safe="/-_.~")
    query_string = urlencode(sorted((query or {}).items()))
    headers = {"host": host, "x-tos-content-sha256": payload_hash, "x-tos-date": timestamp}
    if secret.get("session_token"):
        headers["x-tos-security-token"] = secret["session_token"]
    names = sorted(headers)
    canonical_headers = "".join(f"{name}:{headers[name].strip()}\n" for name in names)
    signed_headers = ";".join(names)
    canonical = "\n".join((method, path, query_string, canonical_headers, signed_headers, payload_hash))
    scope = f"{short_date}/{region}/tos/request"
    string_to_sign = "\n".join(("TOS4-HMAC-SHA256", timestamp, scope, hashlib.sha256(canonical.encode()).hexdigest()))

    def mac(key, value):
        return hmac.new(key, value.encode(), hashlib.sha256).digest()

    signing_key = mac(mac(mac(secret["secret_access_key"].encode(), short_date), region), "tos")
    signing_key = mac(signing_key, "request")
    signature = hmac.new(signing_key, string_to_sign.encode(), hashlib.sha256).hexdigest()
    headers["Authorization"] = f"TOS4-HMAC-SHA256 Credential={secret['access_key_id']}/{scope}, SignedHeaders={signed_headers}, Signature={signature}"
    headers["Host"] = headers.pop("host")
    if content_type:
        headers["Content-Type"] = content_type
    if content_length is not None:
        headers["Content-Length"] = str(content_length)
    elif isinstance(payload, (bytes, bytearray)):
        headers["Content-Length"] = str(len(payload))
    else:
        raise RuntimeError("TOS request content length is required")
    url = f"https://{host}{path}" + (f"?{query_string}" if query_string else "")
    return requests.request(method, url, headers=headers, data=payload, timeout=(30, 300))


def upload_object(local_file, credentials, size, digest):
    if size < MULTIPART_THRESHOLD:
        local_file.seek(0)
        response = signed_request(credentials, "PUT", local_file, digest, content_type="application/octet-stream", content_length=size)
        if response.status_code != 200:
            raise RuntimeError(f"TOS PUT failed: HTTP {response.status_code} body={response.text[:1000]!r}")
        return

    empty_hash = hashlib.sha256(b"").hexdigest()
    response = signed_request(credentials, "POST", b"", empty_hash, {"uploads": ""})
    if response.status_code != 200:
        raise RuntimeError(f"TOS multipart create failed: HTTP {response.status_code} body={response.text[:1000]!r}")
    try:
        upload_id = response.json().get("UploadId", "")
    except ValueError as error:
        raise RuntimeError("TOS multipart create returned invalid JSON") from error
    if not upload_id:
        raise RuntimeError("TOS multipart create returned no upload id")

    try:
        part_count = (size + PART_SIZE - 1) // PART_SIZE
        def read_part(number):
            local_file.seek((number - 1) * PART_SIZE)
            return number, local_file.read(min(PART_SIZE, size - (number - 1) * PART_SIZE))
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as executor:
            parts = list(executor.map(lambda number: read_part(number), range(1, part_count + 1)))
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as executor:
            uploaded = list(executor.map(lambda item: upload_part(credentials, upload_id, item[0], item[1]), parts))
        uploaded.sort()
        payload = json.dumps({"Parts": [{"PartNumber": number, "ETag": etag} for number, etag in uploaded]}, separators=(",", ":")).encode()
        response = signed_request(credentials, "POST", payload, hashlib.sha256(payload).hexdigest(), {"uploadId": upload_id}, "application/json")
        if response.status_code != 200:
            raise RuntimeError(f"TOS multipart complete failed: HTTP {response.status_code} body={response.text[:1000]!r}")
    except Exception:
        signed_request(credentials, "DELETE", b"", empty_hash, {"uploadId": upload_id})
        raise


def upload_part(credentials, upload_id, number, data):
    digest = hashlib.sha256(data).hexdigest()
    for attempt in range(3):
        response = signed_request(credentials, "PUT", data, digest, {"partNumber": str(number), "uploadId": upload_id}, "application/octet-stream")
        if response.status_code == 200:
            etag = response.headers.get("ETag", "").strip()
            if not etag:
                raise RuntimeError(f"TOS part {number} returned no ETag")
            return number, etag
        if response.status_code < 500 and response.status_code != 429:
            raise RuntimeError(f"TOS part {number} failed: HTTP {response.status_code} body={response.text[:1000]!r}")
        time.sleep(2**attempt)
    raise RuntimeError(f"TOS part {number} failed after retries")


def migrate_one(source, target, source_base, target_base, item, dry_run=False):
    source_id = item["source_raw_data_id"]
    source_record = query_raw(source, source_base, SOURCE_WORKSPACE, raw_data_id=source_id)[0]
    target_name = TARGET_BAG_PREFIX + str(source_id) + "-" + source_record["bagName"]
    existing = query_raw(target, target_base, TARGET_WORKSPACE, bag_name=target_name)
    matching = [record for record in existing if record.get("bagName") == target_name]
    if matching and matching[0].get("status") == "uploaded":
        if matching[0].get("bagDigest", "").lower() != source_record["bagDigest"].lower():
            raise RuntimeError("target name exists with a different digest")
        return {"source_raw_data_id": source_id, "target_raw_data_id": matching[0]["id"], "status": "skipped_existing"}
    if dry_run:
        return {"source_raw_data_id": source_id, "target_bag_name": target_name, "status": "dry_run"}

    presigned = api(source, source_base, "GET", "/v1/data-collection/raw-data/get-presigned-url", params={"workspaceId": SOURCE_WORKSPACE, "id": source_id})
    with NamedTemporaryFile(suffix=".mcap") as local_file:
        digest = hashlib.sha256()
        size = 0
        with requests.get(presigned["url"], stream=True, timeout=(30, 300)) as response:
            response.raise_for_status()
            for chunk in response.iter_content(8 * 1024 * 1024):
                if chunk:
                    local_file.write(chunk)
                    digest.update(chunk)
                    size += len(chunk)
        actual = digest.hexdigest()
        if size != source_record["bagSize"] or actual != source_record["bagDigest"].lower():
            raise RuntimeError("source file identity mismatch")
        target_id = matching[0]["id"] if matching else api(target, target_base, "POST", "/v1/data-collection/raw-data/register", json={"workspaceId": TARGET_WORKSPACE, "dcPlanId": item["target_plan_id"], "bagName": target_name, "bagStartTime": source_record["bagStartTime"], "bagEndTime": source_record["bagEndTime"], "bagSize": size, "bagDigest": actual})
        credentials = api(target, target_base, "GET", "/v1/data-collection/raw-data/get-upload-credentials", params={"workspaceId": TARGET_WORKSPACE, "id": target_id})
        upload_object(local_file, credentials, size, actual)
    api(target, target_base, "POST", "/v1/data-collection/raw-data/finish-upload", json={"workspaceId": TARGET_WORKSPACE, "rawDataId": target_id})
    verified = query_raw(target, target_base, TARGET_WORKSPACE, raw_data_id=target_id)[0]
    if verified.get("status") != "uploaded" or verified.get("bagDigest", "").lower() != actual or verified.get("bagSize") != size:
        raise RuntimeError("target verification failed")
    return {"source_raw_data_id": source_id, "target_raw_data_id": target_id, "status": "uploaded", "bytes": size, "sha256": actual}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--limit", type=int, default=0)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    source_base = os.environ["SOURCE_HILBERT_BASE_URL"].rstrip("/")
    target_base = os.environ["TARGET_HILBERT_BASE_URL"].rstrip("/")
    source = login(source_base, os.environ["SOURCE_HILBERT_USERNAME"], os.environ["SOURCE_HILBERT_PASSWORD"])
    target = login(target_base, os.environ["TARGET_HILBERT_USERNAME"], os.environ["TARGET_HILBERT_PASSWORD"])
    with open(args.manifest, encoding="utf-8") as stream:
        items = json.load(stream)
    if args.limit:
        items = items[: args.limit]
    expected = len(items)
    results = []
    for index, item in enumerate(items, 1):
        try:
            result = migrate_one(source, target, source_base, target_base, item, args.dry_run)
        except Exception as error:
            result = {"source_raw_data_id": item["source_raw_data_id"], "status": "failed", "error": str(error)}
        results.append(result)
        print(json.dumps({"index": index, "expected": expected, **result}, ensure_ascii=False), flush=True)
    assert len(results) == expected
    failed = sum(result["status"] == "failed" for result in results)
    print(json.dumps({"summary": {"expected": expected, "completed": expected - failed, "failed": failed}}, ensure_ascii=False), flush=True)
    if failed:
        sys.exit(1)


if __name__ == "__main__":
    main()
