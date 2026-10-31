import copy
import importlib.util
import json
import os
import subprocess
import tempfile
from pathlib import Path

spec = importlib.util.spec_from_file_location("report", "scripts/trivy-report.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
clean = {"SchemaVersion": 2, "Trivy": {"Version": "0.74.0"}, "ArtifactName": "fixture",
         "ArtifactType": "container_image", "Results": [{"Target": "alpine"}]}
vuln = {"VulnerabilityID": "CVE-test", "PkgName": "test", "InstalledVersion": "1", "Severity": "HIGH"}


def run(report, expected=0, image="fixture"):
    with tempfile.TemporaryDirectory() as directory:
        path = Path(directory) / "report.json"
        path.write_text(json.dumps(report))
        result = subprocess.run(["python3", "scripts/trivy-report.py", str(path), image], capture_output=True)
        assert result.returncode == expected, (report, result.stderr)
        assert b"secret-match-must-not-appear" not in result.stdout + result.stderr


assert module.counts(clean) == (0, 0)
run(clean)
# Pinned 0.74.0 vulnerability_flags.go retains only fixed status; the legacy
# JSON gate additionally blocks any populated FixedVersion. Preserve their union.
for status in sorted(module.STATUSES):
    for fixed in ("", "2"):
        report = copy.deepcopy(clean)
        report["Results"][0]["Vulnerabilities"] = [dict(vuln, Status=status, FixedVersion=fixed)] * 2
        assert module.counts(report) == ((1, 0) if fixed else (0, 1))
        run(report, int(status == "fixed" or bool(fixed)))
for severity in sorted(module.SEVERITIES):
    report = copy.deepcopy(clean)
    report["Results"][0]["Secrets"] = [{"RuleID": "fixture", "Severity": severity, "Match": "secret-match-must-not-appear"}]
    run(report, int(severity in module.HIGH))
# Shape from v0.74.0 pkg/types/{report,finding}.go, after OpenVEX fixed suppression.
report = copy.deepcopy(clean)
report["Results"][0]["ExperimentalModifiedFindings"] = [{
    "Type": "vulnerability", "Status": "fixed", "Statement": "fixture", "Source": "fixture.openvex.json",
    "Finding": dict(vuln, Status="fixed", FixedVersion="2"),
}]
run(report)
report["Results"][0]["Vulnerabilities"] = [dict(vuln, Status="fixed", FixedVersion="2")]
run(report, 1)  # A separate real finding is never exempted by suppressed evidence.
for bad in [{}, {**clean, "Trivy": {"Version": "other"}}, {**clean, "Results": []},
            {**clean, "Results": [{}]}, {**clean, "Results": [{"Target": "x", "Vulnerabilities": [{}]}]},
            {**clean, "Results": [{"Target": "x", "Secrets": [{}]}]},
            {**clean, "Results": [{"Target": "x", "Vulnerabilities": [dict(vuln, Status="future")]}]}]:
    run(bad, 1)
run(clean, 1, "other-image")
# The real wrapper must retain scanner errors, reject empty/stale output, and
# scan secrets as well as vulnerabilities without printing either payload.
with tempfile.TemporaryDirectory() as directory:
    docker = Path(directory) / "docker"
    docker.write_text('#!/bin/sh\n[ "$SCAN_STATUS" = 0 ] || exit "$SCAN_STATUS"\nprintf "%s\\n" "$*" >> "$SCAN_ARGS"\ncat "$SCAN_REPORT"\n')
    docker.chmod(0o755)
    report = Path(directory) / "report.json"
    args = Path(directory) / "args"
    summary = Path(directory) / "summary"
    secret = {**clean, "Results": [{"Target": "x", "Secrets": [{"Severity": "HIGH", "Match": "secret-match-must-not-appear"}]}]}
    for status, content, expected in [(42, clean, 42), (0, {}, 1), (0, clean, 0), (0, secret, 1), (0, None, 1)]:
        report.write_text("" if content is None else json.dumps(content))
        result = subprocess.run(["sh", "scripts/scan-image.sh", "fixture"], env={**os.environ,
            "PATH": directory + ":" + os.environ["PATH"], "SCAN_STATUS": str(status), "SCAN_REPORT": str(report),
            "SCAN_ARGS": str(args), "RUNNER_TEMP": directory, "GITHUB_STEP_SUMMARY": str(summary)}, capture_output=True)
        assert result.returncode == expected, result.stderr
        assert b"secret-match-must-not-appear" not in result.stdout + result.stderr
    assert "--scanners vuln,secret" in args.read_text()
    assert "secret-match-must-not-appear" not in summary.read_text()
print("Trivy 0.74.0 report gate and scanner-failure checks passed")
