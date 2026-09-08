# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

import base64
import hashlib
import os
from tempfile import NamedTemporaryFile

import boto3
import requests
from botocore.config import Config
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

SOURCE_BASE = os.environ["SOURCE_HILBERT_BASE_URL"].rstrip("/")
TARGET_BASE = os.environ["TARGET_HILBERT_BASE_URL"].rstrip("/")
SOURCE_WORKSPACE = 4
TARGET_WORKSPACE = 6
TARGET_PLAN_ID = 851
SOURCE_RAW_DATA_ID = 16095
LEGACY_TARGET_RAW_DATA_ID = 197
TARGET_BAG_PREFIX = "migration-prod-16095-"


def encrypted_digest(password, material):
    raw = base64.b64decode(material)
    digest = hashlib.sha256(password.encode()).hexdigest().encode()
    encrypted = AESGCM(raw[:32]).encrypt(raw[32:], digest, None)
    return base64.b64encode(encrypted).decode()


def login(base, username, password):
    session = requests.Session()
    nonce_response = session.get(base + "/v1/console/nonce/generate", timeout=30)
    nonce_response.raise_for_status()
    material = nonce_response.json()["data"]
    response = session.post(
        base + "/v1/console/account/login",
        json={
            "code": username,
            "nonceId": material["id"],
            "cipherDigest": encrypted_digest(password, material["randomKey"]),
        },
        timeout=30,
    ).json()
    if response.get("code") != 0:
        raise RuntimeError("Hilbert login failed")
    session.headers["Authorization"] = "Bearer " + response["data"]["sessionKey"]
    return session


def api(session, base, method, path, **kwargs):
    response = session.request(method, base + path, timeout=120, **kwargs)
    try:
        payload = response.json()
    except ValueError as error:
        raise RuntimeError(f"Hilbert API returned non-JSON: {method} {path} HTTP {response.status_code}") from error
    if response.status_code >= 300 or payload.get("code") != 0:
        detail = response.text[:1000]
        raise RuntimeError(f"Hilbert API failed: {method} {path} HTTP {response.status_code} body={detail!r}")
    return payload["data"]


def query_one(session, base, raw_data_id):
    records = api(
        session,
        base,
        "GET",
        "/v1/data-collection/raw-data/query",
        params={"workspaceId": TARGET_WORKSPACE, "id": raw_data_id, "pageNum": 1, "pageSize": 1},
    )["records"]
    return records[0] if records else None


def main():
    source = login(SOURCE_BASE, os.environ["SOURCE_HILBERT_USERNAME"], os.environ["SOURCE_HILBERT_PASSWORD"])
    target = login(TARGET_BASE, os.environ["TARGET_HILBERT_USERNAME"], os.environ["TARGET_HILBERT_PASSWORD"])

    source_record = api(
        source,
        SOURCE_BASE,
        "GET",
        "/v1/data-collection/raw-data/query",
        params={"workspaceId": SOURCE_WORKSPACE, "id": SOURCE_RAW_DATA_ID, "pageNum": 1, "pageSize": 1},
    )["records"][0]
    if source_record.get("status") != "uploaded":
        raise RuntimeError(f"source raw data is not uploaded: {source_record.get('status')}")

    legacy = query_one(target, TARGET_BASE, LEGACY_TARGET_RAW_DATA_ID)
    if legacy and legacy.get("status") == "uploaded":
        raise RuntimeError("legacy target raw data 197 is already uploaded; refusing duplicate migration")

    presigned = api(
        source,
        SOURCE_BASE,
        "GET",
        "/v1/data-collection/raw-data/get-presigned-url",
        params={"workspaceId": SOURCE_WORKSPACE, "id": SOURCE_RAW_DATA_ID},
    )

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
        local_file.flush()
        actual_digest = digest.hexdigest()
        if size != source_record["bagSize"] or actual_digest != source_record["bagDigest"].lower():
            raise RuntimeError("downloaded source file failed size or digest verification")

        target_name = TARGET_BAG_PREFIX + source_record["bagName"]
        existing = api(
            target,
            TARGET_BASE,
            "GET",
            "/v1/data-collection/raw-data/query",
            params={"workspaceId": TARGET_WORKSPACE, "bagName": target_name, "pageNum": 1, "pageSize": 200},
        ).get("records", [])
        matching = [record for record in existing if record.get("bagName") == target_name]
        if matching:
            if any(record.get("bagDigest", "").lower() != actual_digest for record in matching):
                raise RuntimeError("target migration name exists with a different digest")
            target_raw_data_id = matching[0]["id"]
        else:
            target_raw_data_id = api(
                target,
                TARGET_BASE,
                "POST",
                "/v1/data-collection/raw-data/register",
                json={
                    "workspaceId": TARGET_WORKSPACE,
                    "dcPlanId": TARGET_PLAN_ID,
                    "bagName": target_name,
                    "bagStartTime": source_record["bagStartTime"],
                    "bagEndTime": source_record["bagEndTime"],
                    "bagSize": size,
                    "bagDigest": actual_digest,
                },
            )
        print(f"target raw data registered: {target_raw_data_id}")

        credentials = api(
            target,
            TARGET_BASE,
            "GET",
            "/v1/data-collection/raw-data/get-upload-credentials",
            params={"workspaceId": TARGET_WORKSPACE, "id": target_raw_data_id},
        )
        secret = credentials["credentials"]
        endpoint = credentials["endpoint"]
        if not endpoint.startswith("http"):
            endpoint = "https://" + endpoint
        endpoint_host = urlparse(endpoint).hostname or ""
        if endpoint_host.startswith("tos-s3-") and endpoint_host.endswith(".ivolces.com"):
            region = endpoint_host[len("tos-s3-") : -len(".ivolces.com")]
            endpoint = f"https://tos-{region}.ivolces.com"
        client = boto3.client(
            "s3",
            endpoint_url=endpoint,
            region_name=credentials.get("region") or "cn-beijing",
            aws_access_key_id=secret["access_key_id"],
            aws_secret_access_key=secret["secret_access_key"],
            aws_session_token=secret.get("session_token"),
            config=Config(signature_version="s3v4", s3={"addressing_style": "virtual"}),
        )
        local_file.seek(0)
        client.upload_fileobj(
            local_file,
            credentials["bucket"],
            credentials["key"],
            ExtraArgs={"ContentType": "application/octet-stream"},
        )
        head = client.head_object(Bucket=credentials["bucket"], Key=credentials["key"])
        if int(head["ContentLength"]) != size:
            raise RuntimeError("uploaded target object size mismatch")

    api(
        target,
        TARGET_BASE,
        "POST",
        "/v1/data-collection/raw-data/finish-upload",
        json={"workspaceId": TARGET_WORKSPACE, "rawDataId": target_raw_data_id},
    )
    verified = query_one(target, TARGET_BASE, target_raw_data_id)
    if not verified or verified.get("status") != "uploaded":
        raise RuntimeError(f"target raw data did not reach uploaded state: {verified}")
    if verified.get("bagDigest", "").lower() != actual_digest or verified.get("bagSize") != size:
        raise RuntimeError("target raw data identity verification failed")
    print(
        f"sample migration complete: source={SOURCE_RAW_DATA_ID} target={verified['id']} "
        f"status={verified['status']} bytes={size} sha256={actual_digest}"
    )


if __name__ == "__main__":
    main()
