#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0
# Register/upload/ready the 5 pure-direct CMD calibrations as Hilbert
# CalibrationSnapshots in hilbert-staging (ws6) and bind every uploaded raw
# of the mapped device. Runs inside the staging cluster where TOS is reachable.
import base64, hashlib, hmac, json, os, time
from datetime import datetime, timezone
from urllib.parse import quote, urlencode, urlparse
import requests
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

requests.packages.urllib3.disable_warnings()
BASE = os.environ["TARGET_HILBERT_BASE_URL"].rstrip("/")
WORKSPACE = 6
CALIB_DIR = os.environ.get("CALIB_DIR", "/etc/hilbert-calibration")
CMDS = ["CMD-000004", "CMD-000011", "CMD-000198", "CMD-000123", "CMD-000143"]

def log(msg):
    print(time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), msg, flush=True)

def encrypted_digest(password, material):
    raw = base64.b64decode(material)
    value = hashlib.sha256(password.encode()).hexdigest().encode()
    return base64.b64encode(AESGCM(raw[:32]).encrypt(raw[32:], value, None)).decode()

def login(base, username, password):
    session = requests.Session()
    material = session.get(base + "/v1/console/nonce/generate", timeout=40).json()["data"]
    response = session.post(
        base + "/v1/console/account/login",
        json={"code": username, "nonceId": material["id"],
              "cipherDigest": encrypted_digest(password, material["randomKey"])}, timeout=40).json()
    if response.get("code") != 0:
        raise RuntimeError("hilbert login failed")
    session.headers["Authorization"] = "Bearer " + response["data"]["sessionKey"]
    return session

def tos_put(credentials, payload):
    secret = credentials["credentials"]
    parsed = urlparse(credentials["endpoint"] if credentials["endpoint"].startswith("http") else "https://" + credentials["endpoint"])
    hostname = parsed.hostname or ""
    if hostname.startswith("tos-s3-") and hostname.endswith(".ivolces.com"):
        region = hostname[len("tos-s3-"):-len(".ivolces.com")]
        endpoint = f"tos-{region}.ivolces.com"
    else:
        region = credentials.get("region") or "cn-beijing"
        endpoint = hostname
    now = datetime.now(timezone.utc)
    timestamp = now.strftime("%Y%m%dT%H%M%SZ")
    short_date = timestamp[:8]
    host = f"{credentials['bucket']}.{endpoint}"
    path = "/" + quote(credentials["key"].lstrip("/"), safe="/-_.~")
    payload_hash = hashlib.sha256(payload).hexdigest()
    headers = {"host": host, "x-tos-content-sha256": payload_hash, "x-tos-date": timestamp}
    if secret.get("session_token"):
        headers["x-tos-security-token"] = secret["session_token"]
    names = sorted(headers)
    canonical_headers = "".join(f"{name}:{headers[name].strip()}\n" for name in names)
    signed_headers = ";".join(names)
    canonical = "\n".join(("PUT", path, "", canonical_headers, signed_headers, payload_hash))
    scope = f"{short_date}/{region}/tos/request"
    string_to_sign = "\n".join(("TOS4-HMAC-SHA256", timestamp, scope, hashlib.sha256(canonical.encode()).hexdigest()))
    def mac(key, value):
        return hmac.new(key, value.encode(), hashlib.sha256).digest()
    signing_key = mac(mac(mac(secret["secret_access_key"].encode(), short_date), region), "tos")
    signing_key = mac(signing_key, "request")
    signature = hmac.new(signing_key, string_to_sign.encode(), hashlib.sha256).hexdigest()
    headers["Authorization"] = f"TOS4-HMAC-SHA256 Credential={secret['access_key_id']}/{scope}, SignedHeaders={signed_headers}, Signature={signature}"
    headers["Host"] = headers.pop("host")
    headers["Content-Type"] = "application/octet-stream"
    headers["Content-Length"] = str(len(payload))
    url = f"https://{host}{path}"
    return requests.put(url, headers=headers, data=payload, timeout=(30, 300))

def main():
    s = login(BASE, os.environ["TARGET_HILBERT_USERNAME"], os.environ["TARGET_HILBERT_PASSWORD"])
    # device name -> id and plan -> device
    devs = s.get(BASE + "/v1/data-collection/dc-device/query",
                 params={"workspaceId": WORKSPACE, "pageNum": 1, "pageSize": 200}, timeout=60).json()["data"]["records"]
    name2id = {d["name"]: d["id"] for d in devs}
    plans = s.get(BASE + "/v1/data-collection/dc-plan/query",
                  params={"workspaceId": WORKSPACE, "pageNum": 1, "pageSize": 200}, timeout=60).json()["data"]["records"]
    plan2dev = {p["id"]: p["dcDeviceId"] for p in plans}
    dev2name = {d["id"]: d["name"] for d in devs}
    cmd2ep = {
        "CMD-000004": "EP-000045", "CMD-000011": "EP-000042", "CMD-000198": "EP-000108",
        "CMD-000123": "EP-000123", "CMD-000143": "EP-000143",
    }
    raws = []; page = 1
    while True:
        r = s.get(BASE + "/v1/data-collection/raw-data/query",
                  params={"workspaceId": WORKSPACE, "pageNum": page, "pageSize": 200}, timeout=90).json()["data"]
        raws.extend(r["records"])
        if len(raws) >= r["total"]:
            break
        page += 1
    for cmd in CMDS:
        data = open(os.path.join(CALIB_DIR, f"{cmd}-calibration.json"), "rb").read()
        sha = hashlib.sha256(data).hexdigest()
        reg = s.post(BASE + "/v1/data-collection/raw-data/register-param-file",
                     json={"workspaceId": WORKSPACE, "contentSha256": sha, "sizeBytes": len(data)}, timeout=60).json()
        if reg.get("code") != 0:
            log(f"{cmd} register failed: {reg}"); continue
        pid = reg["data"]["paramFileMotionStoreId"]; state = reg["data"]["state"]
        if state != "READY":
            cred = s.get(BASE + "/v1/data-collection/raw-data/get-param-file-upload-credentials",
                         params={"workspaceId": WORKSPACE, "paramFileMotionStoreId": pid}, timeout=60).json()["data"]
            resp = tos_put(cred, data)
            if resp.status_code != 200:
                log(f"{cmd} TOS put failed: {resp.status_code} {resp.text[:300]}"); continue
            fin = s.post(BASE + "/v1/data-collection/raw-data/finish-param-file-upload",
                         json={"workspaceId": WORKSPACE, "paramFileMotionStoreId": pid}, timeout=60).json()
            if fin.get("code") != 0:
                log(f"{cmd} finish failed: {fin}"); continue
        log(f"{cmd} snapshot ready: {pid}")
        ep = cmd2ep[cmd]; did = name2id[ep]
        target = [x for x in raws if x["dcPlanId"] in [p for p, d in plan2dev.items() if d == did]
                  and x.get("status") == "uploaded" and not x.get("paramFileMotionStoreId")]
        bound = 0
        for row in target:
            r = s.post(BASE + "/v1/data-collection/raw-data/update-param-file",
                       json={"workspaceId": WORKSPACE, "rawDataId": row["id"], "paramFileMotionStoreId": pid}, timeout=60).json()
            if r.get("code") == 0:
                bound += 1
            else:
                log(f"  bind raw {row['id']} failed: {r}")
        log(f"{cmd}: bound {bound}/{len(target)} raws ({ep})")

if __name__ == "__main__":
    main()
