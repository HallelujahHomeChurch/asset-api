#!/usr/bin/env python3
import copy
import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("retire_dapr", Path(__file__).with_name("retire-dapr.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

PREVIOUS = "asset-api--ready"
CANDIDATE = "asset-api--dapr-off-123-1"
ROLLBACK = "asset-api--dapr-rollback-123-1"
IMAGE = "alive.azurecr.io/alive/asset-api@sha256:" + "a" * 64


class RetireDaprTest(unittest.TestCase):
    def fixture(self):
        env = dict(GITHUB_REPOSITORY="HallelujahHomeChurch/asset-api", GITHUB_REF="refs/heads/main",
                   RELEASE_CONFIRMATION="deploy-asset-api-production", RETIRE_DAPR_REVISION=PREVIOUS,
                   GITHUB_RUN_ID="123", GITHUB_RUN_ATTEMPT="1")
        values = dict(ASSET_DAPR_ENABLED="true", AUDIT_DISPATCH_ENABLED="true", AUDIT_PRIVATE_HTTPS="true",
                      ASSET_ALLOW_DEV_CALLER_HEADER="false", ASSET_READER_CALLER_APP_ID="api-gateway")
        bindings = [{"name": name, "value": value} for name, value in values.items()]
        bindings.append({"name": "AUDIT_TOKEN", "secretRef": "audit-token"})
        app = dict(latest=PREVIOUS, ready=PREVIOUS, mode="Single", dapr=copy.deepcopy(module.DAPR),
                   ingress=dict(external=False, allowInsecure=False, targetPort=8080, fqdn=module.HOST))
        source = dict(name=PREVIOUS, active=True, health="Healthy", provisioned="Provisioned", running="Running",
                      template=dict(containers=[dict(name="asset-api", image=IMAGE, env=bindings)], scale={"minReplicas": 1}))
        return env, app, source

    def run_fixture(self, failure=None):
        env, app, source = self.fixture()
        current = copy.deepcopy(source)
        mutations = []
        if failure == "stale":
            app["latest"] = "asset-api--wrong"
        if failure == "caller":
            current["template"]["containers"][0]["env"] = [entry for entry in current["template"]["containers"][0]["env"] if entry["name"] != "ASSET_READER_CALLER_APP_ID"]

        def az(*args):
            nonlocal current
            if args[:2] == ("revision", "list"):
                return [PREVIOUS, CANDIDATE] if failure == "draining" else [CANDIDATE]
            mutations.append(args)
            if args[:2] == ("revision", "copy"):
                self.assertEqual(args[args.index("--from-revision") + 1], PREVIOUS)
                name = "asset-api--" + args[args.index("--revision-suffix") + 1]
                app.update(latest=name, ready=name)
                current = copy.deepcopy(source)
                current["name"] = name
                if name == CANDIDATE:
                    self.assertEqual(args[-2:], ("--set-env-vars", "ASSET_DAPR_ENABLED=false"))
                    entry = next(item for item in current["template"]["containers"][0]["env"] if item["name"] == "ASSET_DAPR_ENABLED")
                    entry["value"] = "false"
                    if failure == "drift":
                        current["template"]["scale"]["minReplicas"] = 2
            elif args == ("dapr", "disable"):
                self.assertEqual(app["ready"], CANDIDATE)
                app["dapr"] = {"enabled": False}
                if failure == "readback":
                    raise ValueError("response lost after sidecar disabled")
            elif args[:2] == ("dapr", "enable"):
                self.assertEqual(args[args.index("--dapr-http-max-request-size") + 1], "210")
                app["dapr"] = copy.deepcopy(module.DAPR)

        with (patch.object(module, "app_state", side_effect=lambda: copy.deepcopy(app)),
              patch.object(module, "revision", side_effect=lambda _: copy.deepcopy(current)),
              patch.object(module, "az", side_effect=az), patch.object(module.time, "sleep")):
            if failure:
                with self.assertRaises(ValueError):
                    module.retire(env)
            else:
                module.retire(env)
        return app, mutations

    def test_retirement_mutates_only_app_revision_and_sidecar(self):
        app, mutations = self.run_fixture()
        self.assertEqual([args[:2] for args in mutations], [("revision", "copy"), ("dapr", "disable")])
        self.assertEqual(app["ready"], CANDIDATE)
        self.assertIs(app["dapr"]["enabled"], False)

    def test_retirement_rejects_changed_inputs_before_mutation(self):
        for failure in ("stale", "caller"):
            with self.subTest(failure=failure):
                _, mutations = self.run_fixture(failure)
                self.assertEqual(mutations, [])

    def test_retirement_rolls_back_exact_previous_revision(self):
        for failure, actions in (
            ("drift", [("revision", "copy"), ("revision", "copy")]),
            ("draining", [("revision", "copy"), ("revision", "copy")]),
            ("readback", [("revision", "copy"), ("dapr", "disable"), ("dapr", "enable"), ("revision", "copy")]),
        ):
            with self.subTest(failure=failure):
                app, mutations = self.run_fixture(failure)
                self.assertEqual([args[:2] for args in mutations], actions)
                self.assertEqual(app["ready"], ROLLBACK)
                self.assertEqual(app["dapr"], module.DAPR)


if __name__ == "__main__":
    unittest.main()
