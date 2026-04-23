#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0

import json
import random
import sys
import urllib.error
import urllib.request
import urllib.parse

API_BASE = "http://127.0.0.1:9999/api/v1"

FACTORY_BODY = {
    "name": "Factory Shanghai",
    "location": "Shanghai, China",
    "timezone": "Asia/Shanghai",
}

ORG_BODY = {
    "name": "ArcheBase",
    "description": "Created by seed script",
}

SKILLS = [
    {
        "slug": "move-base",
        "description": "Move robot base to a target pose.",
        "version": "1.0.0",
        "metadata": {"category": "navigation"},
    },
    {
        "slug": "open-fridge",
        "description": "Open the refrigerator door.",
        "version": "1.0.0",
        "metadata": {"category": "kitchen"},
    },
    {
        "slug": "pick-item",
        "description": "Pick an item with gripper.",
        "version": "1.0.0",
        "metadata": {"category": "manipulation"},
    },
    {
        "slug": "place-item",
        "description": "Place an item on a target surface.",
        "version": "1.0.0",
        "metadata": {"category": "manipulation"},
    },
    {
        "slug": "scan-qr",
        "description": "Scan a QR code on a package.",
        "version": "1.0.0",
        "metadata": {"category": "delivery"},
    },
    {
        "slug": "sort-package",
        "description": "Sort packages into bins/shelves.",
        "version": "1.0.0",
        "metadata": {"category": "delivery"},
    },
    {
        "slug": "clean-sink",
        "description": "Clean the sink area.",
        "version": "1.0.0",
        "metadata": {"category": "bathroom"},
    },
    {
        "slug": "make-bed",
        "description": "Make the bed tidy.",
        "version": "1.0.0",
        "metadata": {"category": "bedroom"},
    },
    {
        "slug": "fold-towel",
        "description": "Fold a towel neatly.",
        "version": "1.0.0",
        "metadata": {"category": "housekeeping"},
    },
]

SOPS = [
    {
        "slug": "kitchen-fridge-retrieve",
        "description": "Retrieve an item from the fridge and place it on prep table.",
        "version": "1.0.0",
        "skill_sequence": ["move-base", "open-fridge", "pick-item", "place-item"],
    },
    {
        "slug": "delivery-station-intake",
        "description": "Scan and sort incoming packages at delivery station.",
        "version": "1.0.0",
        "skill_sequence": ["move-base", "scan-qr", "sort-package"],
    },
    {
        "slug": "bathroom-quick-clean",
        "description": "Quick clean routine for bathroom sink.",
        "version": "1.0.0",
        "skill_sequence": ["move-base", "clean-sink"],
    },
    {
        "slug": "bedroom-tidy",
        "description": "Tidy bedroom by making the bed.",
        "version": "1.0.0",
        "skill_sequence": ["move-base", "make-bed"],
    },
    {
        "slug": "bathroom-fold-towel",
        "description": "Fold towel in bathroom and place it properly.",
        "version": "1.0.0",
        "skill_sequence": ["move-base", "fold-towel", "place-item"],
    },
    {
        "slug": "kitchen-move-water-bottle",
        "description": "Move a water bottle to a target location in kitchen.",
        "version": "1.0.0",
        "skill_sequence": ["move-base", "pick-item", "place-item"],
    },
]

ROBOT_TYPES = [
    {
        "name": "穹彻智能 M1",
        "model": "M1",
        "manufacturer": "穹彻智能",
        "end_effector": "",
        "ros_topics": [],
        "sensor_suite": {},
        "capabilities": {},
    },
    {
        "name": "浙江人形 WA1",
        "model": "WA1",
        "manufacturer": "浙江人形",
        "end_effector": "",
        "ros_topics": [],
        "sensor_suite": {},
        "capabilities": {},
    },
    {
        "name": "灵初智能 SynGloves",
        "model": "SynGloves",
        "manufacturer": "灵初智能",
        "end_effector": "",
        "ros_topics": [
            "/hal/camera/head/gyro_accel/sample",
            "/hal/camera/head/color/image_raw/compressed",
            "/hal/camera/head/color/camera_info",
            "/hal/camera/head/depth/camera_info",
            "/hal/camera/head/depth/image_raw/compressedDepth",
            "/hal/glove/synglove/left/joint_states",
            "/hal/glove/synglove/right/joint_states",
            "/hal/glove/synglove/left/tactile",
            "/hal/glove/synglove/right/tactile",
            "/hal/tracker/htc/head/pose_raw",
            "/hal/tracker/htc/left/pose_raw",
            "/hal/tracker/htc/right/pose_raw",
            "/calibration/glove_calibration/left_adc",
            "/calibration/glove_calibration/right_adc",
            "/calibration/glove_calibration/left_tactile",
            "/calibration/glove_calibration/right_tactile",
            "/tf_static",
            "/system/info",
        ],
        "sensor_suite": {},
        "capabilities": {},
    },
]

ROBOTS = [
    # SynGloves
    {"robot_type_model": "SynGloves", "device_id": "robot_dc86"},
    {"robot_type_model": "SynGloves", "device_id": "robot_dc87"},
    {"robot_type_model": "SynGloves", "device_id": "robot_dc54"},
    {"robot_type_model": "SynGloves", "device_id": "robot_dc93"},
    # M1
    {"robot_type_model": "M1", "device_id": "robot_m1"},
]

DATA_COLLECTORS = [
    {
        "operator_id": "collector01",
        "name": "张伟",
        "email": "collector01@example.com",
    },
    {
        "operator_id": "collector02",
        "name": "王芳",
        "email": "collector02@example.com",
    },
    {
        "operator_id": "collector03",
        "name": "李娜",
        "email": "collector03@example.com",
    },
    {
        "operator_id": "collector04",
        "name": "刘洋",
        "email": "collector04@example.com",
    },
    {
        "operator_id": "collector05",
        "name": "陈杰",
        "email": "collector05@example.com",
    },
    {
        "operator_id": "collector06",
        "name": "杨静",
        "email": "collector06@example.com",
    },
    {
        "operator_id": "collector07",
        "name": "赵磊",
        "email": "collector07@example.com",
    },
    {
        "operator_id": "collector08",
        "name": "黄婷",
        "email": "collector08@example.com",
    },
    {
        "operator_id": "collector09",
        "name": "周强",
        "email": "collector09@example.com",
    },
    {
        "operator_id": "collector10",
        "name": "吴敏",
        "email": "collector10@example.com",
    },
]

INSPECTORS = [
    {
        "inspector_id": "inspector01",
        "name": "李明",
        "email": "inspector01@example.com",
        "certification_level": "level_2",
    },
    {
        "inspector_id": "inspector02",
        "name": "赵倩",
        "email": "inspector02@example.com",
        "certification_level": "senior",
    },
]

STATIONS = [
    {},
    {},
    {},
    {},
    {},
]

SCENE_TREE = {
    "卧室": ["床", "地板", "桌子"],
    "快递站": ["扫码机", "分拣处", "货架"],
    "浴室": ["洗漱台", "浴缸", "淋浴间"],
    "餐厅": ["酒柜", "餐边柜", "餐桌"],
    "厨房": ["冰箱", "备菜台", "灶台"],
}


def _post_json(path, body):
    url = f"{API_BASE.rstrip('/')}{path}"
    req = urllib.request.Request(
        url,
        data=json.dumps(body).encode(),
        method="POST",
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.loads(r.read().decode())

def _get_json(path, params=None):
    url = f"{API_BASE.rstrip('/')}{path}"
    if params:
        url = f"{url}?{urllib.parse.urlencode(params)}"
    req = urllib.request.Request(url, method="GET")
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.loads(r.read().decode())

def _list_items(path, params=None):
    resp = _get_json(path, params=params)
    if isinstance(resp, dict):
        if "items" in resp and isinstance(resp["items"], list):
            return resp["items"]
        if "factories" in resp and isinstance(resp["factories"], list):
            return resp["factories"]
    return []

def _ensure_factory():
    for f in _list_items("/factories", params={"limit": 100, "offset": 0}):
        if str(f.get("name", "")).strip() == FACTORY_BODY["name"]:
            return f
    return _post_json("/factories", FACTORY_BODY)

def _ensure_organization(factory_id):
    for o in _list_items("/organizations", params={"limit": 200, "offset": 0}):
        if str(o.get("factory_id", "")).strip() == str(factory_id) and str(o.get("name", "")).strip() == ORG_BODY["name"]:
            return o
    return _post_json("/organizations", {**ORG_BODY, "factory_id": str(factory_id)})

def _ensure_scene(factory_id, name):
    for s in _list_items("/scenes", params={"factory_id": str(factory_id), "limit": 200, "offset": 0}):
        if str(s.get("name", "")).strip() == name:
            return s
    return _post_json("/scenes", {"factory_id": str(factory_id), "name": name})

def _ensure_subscene(scene_id, name):
    for sub in _list_items("/subscenes", params={"scene_id": str(scene_id), "limit": 300, "offset": 0}):
        if str(sub.get("name", "")).strip() == name:
            return sub
    return _post_json("/subscenes", {"scene_id": str(scene_id), "name": name})

def _ensure_skill(skill):
    slug = str(skill.get("slug", "")).strip()
    version = str(skill.get("version", "")).strip() or "1.0.0"
    for s in _list_items("/skills", params={"limit": 500, "offset": 0}):
        if str(s.get("slug", "")).strip() == slug and str(s.get("version", "")).strip() == version:
            return s
    payload = {"slug": slug, "version": version}
    if skill.get("description"):
        payload["description"] = skill["description"]
    if skill.get("metadata") is not None:
        payload["metadata"] = skill["metadata"]
    return _post_json("/skills", payload)

def _ensure_sop(sop):
    slug = str(sop.get("slug", "")).strip()
    version = str(sop.get("version", "")).strip() or "1.0.0"
    for s in _list_items("/sops", params={"limit": 500, "offset": 0}):
        if str(s.get("slug", "")).strip() == slug and str(s.get("version", "")).strip() == version:
            return s
    payload = {
        "slug": slug,
        "version": version,
        "skill_sequence": list(sop.get("skill_sequence") or []),
    }
    if sop.get("description"):
        payload["description"] = sop["description"]
    return _post_json("/sops", payload)

def _ensure_robot_type(rt):
    name = str(rt.get("name", "")).strip()
    model = str(rt.get("model", "")).strip()
    manufacturer = str(rt.get("manufacturer", "")).strip()

    for existing in _list_items("/robot_types", params={"limit": 500, "offset": 0}):
        if str(existing.get("name", "")).strip() == name:
            return existing
        if (
            manufacturer
            and str(existing.get("manufacturer", "") or "").strip() == manufacturer
            and str(existing.get("model", "")).strip() == model
        ):
            return existing

    payload = {
        "name": name,
        "model": model,
        "ros_topics": list(rt.get("ros_topics") or []),
    }
    if manufacturer:
        payload["manufacturer"] = manufacturer
    if rt.get("end_effector"):
        payload["end_effector"] = rt["end_effector"]
    if rt.get("sensor_suite") is not None:
        payload["sensor_suite"] = rt["sensor_suite"]
    if rt.get("capabilities") is not None:
        payload["capabilities"] = rt["capabilities"]
    return _post_json("/robot_types", payload)

def _ensure_robot(factory_id, robot_type_id, device_id, asset_id=""):
    device_id = str(device_id or "").strip()
    if not device_id:
        raise ValueError("device_id is required")

    for r in _list_items(
        "/robots",
        params={"factory_id": str(factory_id), "limit": 500, "offset": 0},
    ):
        if str(r.get("device_id", "")).strip() == device_id:
            return r

    # Generate a random asset id for new robots (kept stable after creation).
    asset_id = "".join(random.choice("0123456789abcdef") for _ in range(12))

    payload = {
        "factory_id": str(factory_id),
        "robot_type_id": str(robot_type_id),
        "device_id": device_id,
    }
    payload["asset_id"] = asset_id
    return _post_json("/robots", payload)

def _ensure_data_collector(organization_id, dc):
    operator_id = str(dc.get("operator_id", "")).strip()
    name = str(dc.get("name", "")).strip()
    email = str(dc.get("email", "")).strip()

    if not operator_id:
        raise ValueError("operator_id is required")
    if not name:
        raise ValueError("name is required")

    for existing in _list_items(
        "/data_collectors",
        params={"organization_id": str(organization_id), "limit": 500, "offset": 0},
    ):
        if str(existing.get("operator_id", "")).strip() == operator_id:
            return existing

    payload = {
        "organization_id": str(organization_id),
        "operator_id": operator_id,
        "name": name,
        # default password on backend is 123456 when omitted
        "metadata": {"seeded": True},
    }
    if email:
        payload["email"] = email
    return _post_json("/data_collectors", payload)

def _ensure_station(st):
    robot_id = str(st.get("robot_id", "")).strip()
    dc_id = str(st.get("data_collector_id", "")).strip()
    if not robot_id:
        raise ValueError("station robot_id is required")
    if not dc_id:
        raise ValueError("station data_collector_id is required")

    def _canonical_id(raw, prefixes):
        s = str(raw or "").strip()
        for p in prefixes:
            if s.lower().startswith(p.lower()):
                s = s[len(p) :]
        return s.strip()

    robot_id_c = _canonical_id(robot_id, ["robot_", "rb_"])
    dc_id_c = _canonical_id(dc_id, ["dc_", "collector_"])

    for existing in _list_items("/stations", params={"limit": 500, "offset": 0}):
        if _canonical_id(existing.get("robot_id", ""), ["robot_", "rb_"]) == robot_id_c:
            return existing
        if _canonical_id(existing.get("data_collector_id", ""), ["dc_", "collector_"]) == dc_id_c:
            return existing

    return _post_json(
        "/stations",
        {
            "robot_id": robot_id_c,
            "data_collector_id": dc_id_c,
            "metadata": {"seeded": True},
        },
    )

def _ensure_inspector(organization_id, insp):
    inspector_id = str(insp.get("inspector_id", "")).strip()
    name = str(insp.get("name", "")).strip()
    email = str(insp.get("email", "")).strip()
    certification_level = str(insp.get("certification_level", "")).strip()

    if not inspector_id:
        raise ValueError("inspector_id is required")
    if not name:
        raise ValueError("name is required")

    for existing in _list_items(
        "/inspectors",
        params={"organization_id": str(organization_id), "limit": 500, "offset": 0},
    ):
        if str(existing.get("inspector_id", "")).strip() == inspector_id:
            return existing

    payload = {
        "organization_id": str(organization_id),
        "inspector_id": inspector_id,
        "name": name,
        "metadata": {"seeded": True},
    }
    if email:
        payload["email"] = email
    if certification_level:
        payload["certification_level"] = certification_level
    return _post_json("/inspectors", payload)


if __name__ == "__main__":
    try:
        factory = _ensure_factory()
        organization = _ensure_organization(factory["id"])

        print(json.dumps({"factory": factory, "organization": organization}, indent=2, ensure_ascii=False))

        created = {
            "skills": [],
            "sops": [],
            "robot_types": [],
            "robots": [],
            "data_collectors": [],
            "inspectors": [],
            "stations": [],
            "scenes": {},
        }

        for skill in SKILLS:
            created["skills"].append(_ensure_skill(skill))

        for sop in SOPS:
            created["sops"].append(_ensure_sop(sop))

        robotTypeIDByModel = {}
        for rt in ROBOT_TYPES:
            rt_created = _ensure_robot_type(rt)
            created["robot_types"].append(rt_created)
            model = str(rt.get("model", "")).strip()
            if model:
                robotTypeIDByModel[model] = rt_created.get("id")

        for r in ROBOTS:
            model = str(r.get("robot_type_model", "")).strip()
            rt_id = robotTypeIDByModel.get(model)
            if not rt_id:
                raise KeyError(f"robot type id not found for model {model!r}")
            created["robots"].append(
                _ensure_robot(
                    factory_id=factory["id"],
                    robot_type_id=rt_id,
                    device_id=r["device_id"],
                    asset_id=r.get("asset_id", ""),
                )
            )

        for dc in DATA_COLLECTORS:
            created["data_collectors"].append(_ensure_data_collector(organization["id"], dc))

        for insp in INSPECTORS:
            created["inspectors"].append(_ensure_inspector(organization["id"], insp))

        # Create 5 stations by pairing robots and data collectors.
        robots_for_station = created["robots"][: len(STATIONS)]
        collectors_for_station = created["data_collectors"][: len(STATIONS)]
        for i, station in enumerate(STATIONS):
            robot = robots_for_station[i]
            collector = collectors_for_station[i]
            created["stations"].append(
                _ensure_station(
                    {
                        **station,
                        "robot_id": str(robot["id"]),
                        "data_collector_id": str(collector["id"]),
                    }
                )
            )

        for scene_name, subscene_names in SCENE_TREE.items():
            scene = _ensure_scene(factory["id"], scene_name)
            created["scenes"][scene_name] = {"scene": scene, "subscenes": []}
            for sub_name in subscene_names:
                sub = _ensure_subscene(scene["id"], sub_name)
                created["scenes"][scene_name]["subscenes"].append(sub)

        print(json.dumps(created, indent=2, ensure_ascii=False))
    except urllib.error.HTTPError as e:
        print(e.read().decode(), file=sys.stderr)
        sys.exit(1)
    except urllib.error.URLError as e:
        print(e.reason, file=sys.stderr)
        sys.exit(1)
    except KeyError as e:
        print(f"Unexpected response (missing key {e})", file=sys.stderr)
        sys.exit(1)
