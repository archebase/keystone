#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

import argparse
import json
import sys


def load_json(path):
    with open(path, encoding="utf-8") as stream:
        return json.load(stream)


def require(condition, message):
    if not condition:
        raise ValueError(message)


def validate_target(context, cluster):
    require(context, "staging kubeconfig must have a current context")
    require(cluster, "staging kubeconfig current context must reference a cluster")
    require("staging" in context, f"kubeconfig context must identify staging; got {context}")
    require("staging" in cluster, f"kubeconfig cluster must identify staging; got {cluster}")


def validate_ingress_class(ingress_class, expected_name):
    metadata = ingress_class.get("metadata", {})
    require(
        metadata.get("name") == expected_name,
        "staging Keystone IngressClass name does not match STAGING_KEYSTONE_INGRESS_CLASS",
    )

    spec = ingress_class.get("spec", {})
    require(
        spec.get("controller") == "ingress.vke.volcengine.com/alb",
        "staging Keystone IngressClass must use the Volcengine ALB controller",
    )
    parameters = spec.get("parameters", {})
    require(
        parameters.get("apiGroup") == "loadbalancer.vke.volcengine.com"
        and parameters.get("kind") == "ALBInstance"
        and parameters.get("scope") == "Cluster"
        and parameters.get("name"),
        "staging Keystone IngressClass must reference an ALBInstance",
    )
    return parameters["name"]


def validate_resources(args):
    namespace = load_json(args.namespace_json)
    service_account = load_json(args.service_account_json)
    ingress_class = load_json(args.ingress_class_json)
    alb = load_json(args.alb_instance_json)

    namespace_metadata = namespace.get("metadata", {})
    require(
        namespace_metadata.get("name") == args.namespace,
        "staging namespace metadata does not match the deployment namespace",
    )
    namespace_labels = namespace_metadata.get("labels", {})
    require(
        namespace_labels.get("vke.volcengine.com/pod-identity-injection-enabled") == "true",
        "staging namespace must enable VKE pod identity injection",
    )

    service_account_metadata = service_account.get("metadata", {})
    require(
        service_account_metadata.get("name") == args.service_account
        and service_account_metadata.get("namespace") == args.namespace,
        "staging Keystone ServiceAccount metadata does not match the deployment target",
    )
    annotations = service_account_metadata.get("annotations", {})
    role_trn = annotations.get("vke.volcengine.com/role-trn", "")
    require(
        role_trn == args.expected_irsa_role_trn,
        "staging Keystone ServiceAccount IRSA role does not match STAGING_KEYSTONE_IRSA_ROLE_TRN",
    )
    labels = service_account_metadata.get("labels", {})
    require(
        labels.get("archebase.io/environment") == "staging",
        "staging Keystone ServiceAccount must be labeled archebase.io/environment=staging",
    )

    alb_instance_name = validate_ingress_class(ingress_class, args.ingress_class)
    require(
        alb.get("metadata", {}).get("name") == alb_instance_name,
        "staging Keystone ALBInstance does not match its IngressClass reference",
    )

    status = alb.get("status", {})
    require(status.get("phase") == "Running", "staging Keystone ALBInstance must be Running")
    require(
        status.get("edition") == "Standard",
        "staging Keystone ALBInstance must use the Standard edition for gRPC",
    )
    require(
        status.get("ingress", {}).get("hostname") == args.alb_dns_name,
        "STAGING_KEYSTONE_ALB_DNS_NAME does not match the staging ALBInstance status",
    )

    listeners = {
        (listener.get("port"), listener.get("protocol"), listener.get("enableHTTP2"))
        for listener in alb.get("spec", {}).get("listeners", [])
    }
    require(
        (443, "HTTPS", True) in listeners,
        "staging Keystone ALBInstance requires an HTTP/2 HTTPS listener on port 443",
    )
    require(
        (args.grpc_port, "HTTPS", True) in listeners,
        f"staging Keystone ALBInstance requires an HTTP/2 HTTPS listener on port {args.grpc_port} for gRPC",
    )


def parse_args():
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="command", required=True)

    target = subparsers.add_parser("target")
    target.add_argument("--context", required=True)
    target.add_argument("--cluster", required=True)

    ingress_name = subparsers.add_parser("ingress-alb-name")
    ingress_name.add_argument("--ingress-class-json", required=True)
    ingress_name.add_argument("--expected-name", required=True)

    resources = subparsers.add_parser("resources")
    resources.add_argument("--namespace-json", required=True)
    resources.add_argument("--service-account-json", required=True)
    resources.add_argument("--ingress-class-json", required=True)
    resources.add_argument("--alb-instance-json", required=True)
    resources.add_argument("--namespace", required=True)
    resources.add_argument("--service-account", required=True)
    resources.add_argument("--ingress-class", required=True)
    resources.add_argument("--alb-dns-name", required=True)
    resources.add_argument("--grpc-port", required=True, type=int)
    resources.add_argument("--expected-irsa-role-trn", required=True)

    return parser.parse_args()


def main():
    args = parse_args()
    try:
        if args.command == "target":
            validate_target(args.context, args.cluster)
        elif args.command == "ingress-alb-name":
            ingress_class = load_json(args.ingress_class_json)
            print(validate_ingress_class(ingress_class, args.expected_name))
        else:
            validate_resources(args)
    except (OSError, ValueError, json.JSONDecodeError) as error:
        print(error, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
