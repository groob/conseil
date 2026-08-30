#!/usr/bin/env python3

import json
import sys


NAME = "vitrier"
TYPE = "http-proxy"
TARGET = "https://groob-tools.exe.xyz/"


def main() -> int:
    if len(sys.argv) != 2:
        raise SystemExit("usage: vitrier-integration-state.py ATTACHMENT")

    attachment = sys.argv[1]
    integrations = json.load(sys.stdin)
    if not isinstance(integrations, list):
        raise SystemExit("integration list JSON is not an array")

    matches = [
        integration
        for integration in integrations
        if isinstance(integration, dict) and integration.get("name") == NAME
    ]
    if not matches:
        print("missing")
        return 0
    if len(matches) != 1:
        raise SystemExit(f"integration list contains duplicate name {NAME!r}")

    integration = matches[0]
    if integration.get("type") != TYPE:
        raise SystemExit(f"integration {NAME!r} is not an {TYPE}")
    if integration.get("target") != TARGET:
        raise SystemExit(f"integration {NAME!r} has the wrong target")
    if integration.get("peer") is not True:
        raise SystemExit(f"integration {NAME!r} is not a peer integration")

    attachments = integration.get("attachments")
    if not isinstance(attachments, list) or not all(isinstance(value, str) for value in attachments):
        raise SystemExit(f"integration {NAME!r} has invalid attachments")
    print("attached" if attachment in attachments else "detached")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
