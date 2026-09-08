# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

import base64
import hashlib
import os
import tempfile
from urllib.parse import urlparse

import boto3
import requests
from botocore.config import Config
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

SOURCE_BASE = os.environ["SOURCE_HILBERT_BASE_URL"].rstrip("/")
TARGET_BASE = os.environ["TARGET_HILBERT_BASE_URL"].rstrip("/")
SOURCE_WORKSPACE = 4
TARGET_WORKSPACE = 6
SOURCE_RAW_DATA_ID = 16095
TARGET_RAW_DATA_ID = 197


def encrypted_digest(password, material):
    raw = base64.b64decode(material)
    digest = hashlib.sha256(password.encode()).hexdigest().encode()
    encrypted = AESGCM(raw[:32]).encrypt(raw[32:], digest, None)
    return base64.b64encode(encrypted).decode()


def login(base, username, password):
    session = requests.Session()
    nonce = session.get(base + "/v1/console/nonce/generate", timeout=30).json()
    material = nonce["data"]
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
    payload = response.json()
    if response.status_code >= 300 or payload.get("code") != 0:
        raise RuntimeError(f"Hilbert API failed: {method} {path} HTTP {response.status_code}")
    return payload["data"]


def main():
    source = login(SOURCE_BASE, os.environ["SOURCE_HILBERT_USERNAME"], os.environ["SOURCE_HILBERT_PASSWORD"])
    target = login(TARGET_BASE, os.environ["TARGET_HILBERT_USERNAME"], os.environ["TARGET_HILBERT_PASSWORD"])
    source_record = api(
        source, SOURCE_BASE, "GET", "/v1/data-collection/raw-data/query",
        params={"workspaceId": SOURCE_WORKSPACE, "id": SOURCE_RAW_DATA_ID, "pageNum": 1, "pageSize": 1},
    )["records"][0]
    presigned = api(
        source, SOURCE_BASE, "GET", "/v1/data-collection/raw-data/get-presigned-url",
        params={"workspaceId": SOURCE_WORKSPACE, "id": SOURCE_RAW_DATA_ID},
    )
    target_record = api(
        target, TARGET_BASE, "GET", "/v1/data-collection/raw-data/query",
        params={"workspaceId": TARGET_WORKSPACE, "id": TARGET_RAW_DATA_ID, "pageNum": 1, "pageSize": 1},
    )["records"][0]
    if target_record["bagDigest"].lower() != source_record["bagDigest"].lower():
        raise RuntimeError("registered target record does not match source digest")

    with tempfile.NamedTemporaryFile(suffix=".mcap") as local_file:
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
        if size != source_record["bagSize"] or digest.hexdigest() != source_record["bagDigest"]:
            raise RuntimeError("downloaded source file failed size or digest verification")

        credentials = api(
            target, TARGET_BASE, "GET", "/v1/data-collection/raw-data/get-upload-credentials",
            params={"workspaceId": TARGET_WORKSPACE, "id": TARGET_RAW_DATA_ID},
        )
        secret = credentials["credentials"]
        endpoint = credentials["endpoint"]
        if not endpoint.startswith("http"):
            endpoint = "https://" + endpoint
        client = boto3.client(
            "s3",
            endpoint_url=endpoint,
            region_name=credentials.get("region") or "cn-beijing",
            aws_access_key_id=secret["access_key_id"],
            aws_secret_access_key=secret["secret_access_key"],
            aws_session_token=secret.get("session_token"),
            config=Config(signature_version="s3v4", s3={"addressing_style": "path"}),
        )
        local_file.seek(0)
        client.upload_fileobj(local_file, credentials["bucket"], credentials["key"], ExtraArgs={"ContentType": "application/octet-stream"})
        head = client.head_object(Bucket=credentials["bucket"], Key=credentials["key"])
        if int(head["ContentLength"]) != size:
            raise RuntimeError("uploaded target object size mismatch")

    api(target, TARGET_BASE, "POST", "/v1/data-collection/raw-data/finish-upload", json={"workspaceId": TARGET_WORKSPACE, "rawDataId": TARGET_RAW_DATA_ID})
    verified = api(
        target, TARGET_BASE, "GET", "/v1/data-collection/raw-data/query",
        params={"workspaceId": TARGET_WORKSPACE, "id": TARGET_RAW_DATA_ID, "pageNum": 1, "pageSize": 1},
    )["records"][0]
    print(f"sample migration complete: source={SOURCE_RAW_DATA_ID} target={verified['id']} status={verified['status']} bytes={size} sha256={verified['bagDigest']}")


if __name__ == "__main__":
    main()
