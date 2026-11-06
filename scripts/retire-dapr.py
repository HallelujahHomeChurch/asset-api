#!/usr/bin/env python3
"""Retire Asset's app-level sidecar using the exact existing image."""
import copy
import json
import os
import re
import signal
import subprocess
import sys
import time

APP = "asset-api"
GROUP = "alive"
HOST = "asset-api.internal.gentleriver-81abd7bc.eastasia.azurecontainerapps.io"
DAPR = {"enabled": True, "appId": APP, "appPort": 8080, "appProtocol": "http", "httpMaxRequestSize": 210,
        "logLevel": "info", "enableApiLogging": False}
PAIR_NAMES = ("LINE", "LINE_API", "EXTRACTOR", "WATERMARK", "ACCOUNT", "GATEWAY")
ERRORS = (ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError)


def az(*args):
    result = subprocess.run(["az", "containerapp", *args, "-g", GROUP, "-n", APP, "--only-show-errors", "-o", "json"],
                            capture_output=True, text=True, timeout=120)
    if result.returncode:
        raise ValueError(f"Asset {args[0]} command failed")
    return json.loads(result.stdout) if result.stdout.strip() else None


def app_state():
    return az("show", "--query", "{latest:properties.latestRevisionName,ready:properties.latestReadyRevisionName,mode:properties.configuration.activeRevisionsMode,dapr:properties.configuration.dapr,ingress:properties.configuration.ingress}")


def revision(name):
    return az("revision", "show", "--revision", name,
              "--query", "{name:name,active:properties.active,health:properties.healthState,provisioned:properties.provisioningState,running:properties.runningState,template:properties.template}")


def healthy(value, name):
    return (value["name"] == name and value["active"] is True and value["health"] == "Healthy"
            and value["provisioned"] == "Provisioned" and value["running"] in ("Running", "RunningAtMaxScale"))


def template(value):
    result = copy.deepcopy(value["template"])
    result.pop("revisionSuffix", None)
    item, = result["containers"]
    if item["name"] != APP or not re.fullmatch(r"alive\.azurecr\.io/alive/asset-api@sha256:[0-9a-f]{64}", item["image"]):
        raise ValueError("Asset revision image is not the exact immutable runtime")
    item["env"].sort(key=lambda entry: entry["name"])
    if len({entry["name"] for entry in item["env"]}) != len(item["env"]):
        raise ValueError("Asset revision environment contains duplicate bindings")
    return result


def require_prerequisites(app, source, env):
    ingress = app["ingress"]
    if (app["mode"] != "Single" or ingress["external"] is not False or ingress["allowInsecure"] is not False
            or ingress["fqdn"] != HOST or ingress["targetPort"] != 8080
            or {key: value for key, value in app["dapr"].items() if value is not None} != DAPR):
        raise ValueError("Asset does not match the reviewed sidecar and ingress settings")
    required = {"AUDIT_DISPATCH_ENABLED": "true", "AUDIT_PRIVATE_HTTPS": "true",
                "ASSET_ALLOW_DEV_CALLER_HEADER": "false", "ASSET_DAPR_ENABLED": "true",
                "ASSET_WORKLOAD_REQUIRED_ROLE": "Asset.Invoke", "ASSET_READER_CALLER_APP_ID": "api-gateway",
                "ASSET_WORKLOAD_AUDIENCE": env["ASSET_WORKLOAD_AUDIENCE"],
                "ASSET_WORKLOAD_TENANT_ID": env["AZURE_TENANT_ID"],
                "ASSET_WORKLOAD_ISSUER": f"https://sts.windows.net/{env['AZURE_TENANT_ID']}/"}
    for subject in PAIR_NAMES:
        for kind in ("CLIENT_ID", "OBJECT_ID"):
            key = f"ASSET_{subject}_WORKLOAD_{kind}"
            required[key] = env[f"LINE_ATTACHMENT_{kind}" if subject == "LINE" else key]
    actual = {entry["name"]: entry for entry in source["containers"][0]["env"]}
    if (any(not value or actual.get(key, {}).get("value") != value for key, value in required.items())
            or actual.get("AUDIT_TOKEN", {}).get("secretRef") != "audit-token"):
        raise ValueError("Asset is missing an exact private workload or Audit binding")


def wait_ready(name, previous, expected):
    for _ in range(60):
        app = app_state()
        if app["latest"] not in (previous, name):
            raise ValueError("Asset latest revision changed unexpectedly")
        if app["latest"] == app["ready"] == name:
            value = revision(name)
            if healthy(value, name):
                if template(value) != expected:
                    raise ValueError("Asset revision template changed outside the approved setting")
                return app
        time.sleep(5)
    raise ValueError("Asset revision did not become ready before timeout")


def wait_drained(candidate):
    for _ in range(60):
        active = az("revision", "list", "--all", "--query", "[?properties.active || properties.replicas > `0`].name")
        if active == [candidate]:
            return
        time.sleep(5)
    raise ValueError("prior Asset replicas have not drained")


def retire(env):
    if (env["GITHUB_REPOSITORY"] != "HallelujahHomeChurch/asset-api" or env["GITHUB_REF"] != "refs/heads/main"
            or env["RELEASE_CONFIRMATION"] != "deploy-asset-api-production"):
        raise ValueError("retirement requires the reviewed Asset owner workflow")
    previous = env["RETIRE_DAPR_REVISION"]
    run_id, attempt = env["GITHUB_RUN_ID"], env["GITHUB_RUN_ATTEMPT"]
    if not re.fullmatch(r"asset-api--[a-z0-9-]+", previous) or not (run_id.isascii() and run_id.isdecimal() and attempt.isascii() and attempt.isdecimal()):
        raise ValueError("invalid Asset retirement identity")
    suffix = f"dapr-off-{run_id}-{attempt}"
    candidate = APP + "--" + suffix
    app = app_state()
    if app["latest"] != previous or app["ready"] != previous:
        raise ValueError("the reviewed Asset revision is no longer current")
    value = revision(previous)
    if not healthy(value, previous):
        raise ValueError("the reviewed Asset revision is not healthy")
    source = template(value)
    require_prerequisites(app, source, env)
    expected = copy.deepcopy(source)
    for entry in expected["containers"][0]["env"]:
        if entry["name"] == "ASSET_DAPR_ENABLED":
            entry["value"] = "false"

    try:
        if app_state() != app:
            raise ValueError("Asset app state changed before retirement")
        az("revision", "copy", "--from-revision", previous, "--revision-suffix", suffix,
           "--set-env-vars", "ASSET_DAPR_ENABLED=false")
        current = wait_ready(candidate, previous, expected)
        if current["dapr"] != app["dapr"] or current["ingress"] != app["ingress"]:
            raise ValueError("Asset app configuration drifted during revision copy")
        wait_drained(candidate)
        if app_state() != current:
            raise ValueError("Asset app state changed before disabling Dapr")
        az("dapr", "disable")
        current = wait_ready(candidate, candidate, expected)
        if current["dapr"]["enabled"] is not False or current["ingress"] != app["ingress"]:
            raise ValueError("Asset sidecar retirement did not preserve ingress")
    except ERRORS:
        current = app_state()
        if current["latest"] == candidate:
            if current["dapr"]["enabled"] is False:
                az("dapr", "enable", "--dapr-app-id", APP, "--dapr-app-port", "8080", "--dapr-app-protocol", "http",
                   "--dapr-http-max-request-size", "210", "--dapr-log-level", "info")
            rollback = f"dapr-rollback-{run_id}-{attempt}"
            az("revision", "copy", "--from-revision", previous, "--revision-suffix", rollback)
            restored = wait_ready(APP + "--" + rollback, candidate, source)
            require_prerequisites(restored, source, env)
            print("Restored the previous Asset runtime and Dapr settings", file=sys.stderr)
        raise
    print(f"Asset sidecar disabled on {candidate}, retaining {expected['containers'][0]['image']}")


def interrupted(*_):
    raise InterruptedError("Asset retirement interrupted")


if __name__ == "__main__":
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    try:
        retire(os.environ)
    except ERRORS:
        print("Asset retirement failed exact configuration or readiness verification", file=sys.stderr)
        sys.exit(1)
