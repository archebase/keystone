# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

import argparse
import base64
import hashlib
import hmac
import json
import os
import sys
import time
from datetime import datetime, timezone
from tempfile import NamedTemporaryFile
from urllib.parse import quote, urlparse

import requests
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

SOURCE_WORKSPACE = 4
TARGET_WORKSPACE = 6
LEGACY_TARGET_RAW_DATA_ID = 197
TARGET_BAG_PREFIX = "migration-prod-"


def encrypted_digest(password, material):
    raw = base64.b64decode(material)
    digest = hashlib.sha256(password.encode()).hexdigest().encode()
    return base64.b64encode(AESGCM(raw[:32]).encrypt(raw[32:], digest, None)).decode()


def login(base, username, password):
    session = requests.Session()
    nonce = session.get(base + "/v1/console/nonce/generate", timeout=30)
    nonce.raise_for_status()
    material = nonce.json()["data"]
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
                time.sleep(2 ** attempt)
                continue
            raise RuntimeError(f"Hilbert API returned non-JSON: {method} {path} HTTP {response.status_code}") from error
        if response.status_code < 300 and payload.get("code") == 0:
            return payload["data"]
        if response.status_code >= 500 and attempt < 2:
            time.sleep(2 ** attempt)
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


def sign_tos_request(bucket, key, secret, payload_hash, endpoint, region):
    now = datetime.now(timezone.utc)
    timestamp = now.strftime("%Y%m%dT%H%M%SZ")
    short_date = timestamp[:8]
    host = f"{bucket}.{endpoint}"
    path = "/" + quote(key.lstrip("/"), safe="/-_.~")
    headers = {"host": host, "x-tos-content-sha256": payload_hash, "x-tos-date": timestamp}
    if secret.get("session_token"):
        headers["x-tos-security-token"] = secret["session_token"]
    names = sorted(headers)
    canonical = "".join(f"{name}:{headers[name].strip()}\n" for name in names)
    signed = ";".join(names)
    request = "\n".join(("PUT", path, "", canonical, signed, payload_hash))
    scope = f"{short_date}/{region}/tos/request"
    string = "\n".join(("TOS4-HMAC-SHA256", timestamp, scope, hashlib.sha256(request.encode()).hexdigest()))
    def mac(key_bytes, value):
        return hmac.new(key_bytes, value.encode(), hashlib.sha256).digest()
    date_key = mac(secret["secret_access_key"].encode(), short_date)
    region_key = mac(date_key, region)
    service_key = mac(region_key, "tos")
    signing_key = mac(service_key, "request")
    signature = hmac.new(signing_key, string.encode(), hashlib.sha256).hexdigest()
    headers["Authorization"] = f"TOS4-HMAC-SHA256 Credential={secret['access_key_id']}/{scope}, SignedHeaders={signed}, Signature={signature}"
    headers["Host"] = headers.pop("host")
    return f"https://{host}{path}", headers


def upload(local_file, credentials, size, digest):
    secret = credentials["credentials"]
    endpoint_value = credentials["endpoint"]
    parsed = urlparse(endpoint_value if endpoint_value.startswith("http") else "https://" + endpoint_value)
    host = parsed.hostname or ""
    if host.startswith("tos-s3-") and host.endswith(".ivolces.com"):
        region = host[len("tos-s3-") : -len(".ivolces.com")]
        endpoint = f"tos-{region}.ivolces.com"
    else:
        region = credentials.get("region") or "cn-beijing"
        endpoint = host
    url, headers = sign_tos_request(credentials["bucket"], credentials["key"], secret, digest, endpoint, region)
    headers.update({"Content-Type": "application/octet-stream", "Content-Length": str(size)})
    local_file.seek(0)
    response = requests.put(url, headers=headers, data=local_file, timeout=(30, 900))
    if response.status_code != 200:
        raise RuntimeError(f"TOS PUT failed: HTTP {response.status_code} body={response.text[:1000]!r}")


def migrate_one(source, target, source_base, target_base, item, dry_run=False):
    source_id = item["source_raw_data_id"]
    source_record = query_raw(source, source_base, SOURCE_WORKSPACE, raw_data_id=source_id)[0]
    target_name = TARGET_BAG_PREFIX + str(source_id) + "-" + source_record["bagName"]
    existing = query_raw(target, target_base, TARGET_WORKSPACE, bag_name=target_name)
    matching = [x for x in existing if x.get("bagName") == target_name]
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
        with requests.get(presigned["url"], stream=True, timeout=180) as response:
            response.raise_for_status()
            for chunk in response.iter_content(8 * 1024 * 1024):
                if chunk:
                    local_file.write(chunk)
                    digest.update(chunk)
                    size += len(chunk)
        actual = digest.hexdigest()
        if size != source_record["bagSize"] or actual != source_record["bagDigest"].lower():
            raise RuntimeError("source file identity mismatch")
        if matching:
            target_id = matching[0]["id"]
        else:
            target_id = api(target, target_base, "POST", "/v1/data-collection/raw-data/register", json={"workspaceId": TARGET_WORKSPACE, "dcPlanId": item["target_plan_id"], "bagName": target_name, "bagStartTime": source_record["bagStartTime"], "bagEndTime": source_record["bagEndTime"], "bagSize": size, "bagDigest": actual})
        creds = api(target, target_base, "GET", "/v1/data-collection/raw-data/get-upload-credentials", params={"workspaceId": TARGET_WORKSPACE, "id": target_id})
        upload(local_file, creds, size, actual)
    api(target, target_base, "POST", "/v1/data-collection/raw-data/finish-upload", json={"workspaceId": TARGET_WORKSPACE, "rawDataId": target_id})
    verified = query_raw(target, target_base, TARGET_WORKSPACE, raw_data_id=target_id)[0]
    if verified.get("status") != "uploaded" or verified.get("bagDigest", "").lower() != actual:
        raise RuntimeError("target verification failed")
    return {"source_raw_data_id": source_id, "target_raw_data_id": target_id, "status": "uploaded", "bytes": size, "sha256": actual}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--limit", type=int, default=0)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    source = login(os.environ["SOURCE_HILBERT_BASE_URL"].rstrip("/"), os.environ["SOURCE_HILBERT_USERNAME"], os.environ["SOURCE_HILBERT_PASSWORD"])
    target = login(os.environ["TARGET_HILBERT_BASE_URL"].rstrip("/"), os.environ["TARGET_HILBERT_USERNAME"], os.environ["TARGET_HILBERT_PASSWORD"])
    source_base = os.environ["SOURCE_HILBERT_BASE_URL"].rstrip("/")
    target_base = os.environ["TARGET_HILBERT_BASE_URL"].rstrip("/")
    with open(args.manifest, encoding="utf-8") as stream:
        items = json.load(stream)
    if args.limit:
        items = items[:args.limit]
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
