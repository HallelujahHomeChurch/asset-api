"""Gate one pinned Trivy 0.74.0 report; never emit finding payloads or secret matches."""
import json
import sys

SEVERITIES = {"UNKNOWN", "LOW", "MEDIUM", "HIGH", "CRITICAL"}
STATUSES = {"unknown", "not_affected", "affected", "fixed", "under_investigation",
            "will_not_fix", "fix_deferred", "end_of_life"}
HIGH = {"HIGH", "CRITICAL"}


def counts(report):
    if not (isinstance(report, dict) and report.get("SchemaVersion") == 2):
        raise ValueError("Malformed Trivy report")
    if report.get("Trivy", {}).get("Version") != "0.74.0":
        raise ValueError("Unverified Trivy version")
    if not (isinstance(report.get("ArtifactName"), str) and report["ArtifactName"]):
        raise ValueError("Malformed Trivy report")
    if report.get("ArtifactType") != "container_image":
        raise ValueError("Malformed Trivy report")
    if not (isinstance(report.get("Results"), list) and report["Results"]):
        raise ValueError("Malformed Trivy report")
    fixable, unfixed = set(), set()
    for result in report["Results"]:
        if not (isinstance(result, dict) and isinstance(result.get("Target"), str) and result["Target"]):
            raise ValueError("Malformed Trivy report")
        for field in ("Vulnerabilities", "Secrets"):
            findings = result.get(field)
            if findings is not None and not isinstance(findings, list):
                raise ValueError("Malformed Trivy report")
            for finding in findings or []:
                if not isinstance(finding, dict) or finding.get("Severity") not in SEVERITIES:
                    raise ValueError("Malformed Trivy report")
                if field == "Secrets":
                    continue
                for key in ("VulnerabilityID", "PkgName", "InstalledVersion"):
                    if not isinstance(finding.get(key), str) or not finding[key]:
                        raise ValueError("Malformed Trivy report")
                fixed = finding.get("FixedVersion", "")
                if not isinstance(fixed, str) or finding.get("Status", "unknown") not in STATUSES:
                    raise ValueError("Malformed Trivy report")
                if finding["Severity"] in HIGH:
                    key = tuple(finding[k] for k in ("VulnerabilityID", "PkgName", "InstalledVersion"))
                    (fixable if fixed else unfixed).add(key + (fixed,) if fixed else key)
    return len(fixable), len(unfixed)


def main():
    try:
        if len(sys.argv) != 3:
            raise ValueError("Expected report and candidate image")
        with open(sys.argv[1]) as source:
            report = json.load(source)
        fixable, unfixed = counts(report)
        if report["ArtifactName"] != sys.argv[2]:
            raise ValueError("Trivy artifact does not match candidate")
        # 0.74.0 --ignore-unfixed keeps Status=fixed, independently of FixedVersion.
        # Retain both that gate and the previous release FixedVersion gate.
        fixed_status = sum(v.get("Status") == "fixed" and v["Severity"] in HIGH
                           for r in report["Results"] for v in r.get("Vulnerabilities") or [])
        secrets = sum(s["Severity"] in HIGH for r in report["Results"] for s in r.get("Secrets") or [])
    except (AttributeError, KeyError, TypeError, ValueError, OSError):
        print("Invalid Trivy report; release blocked", file=sys.stderr)
        return 1
    print(f"### Trivy image summary\n- Artifact: {json.dumps(report['ArtifactName'])}"
          f"\n- Trivy: 0.74.0\n- Fixable High/Critical: {fixable}\n- Unfixed High/Critical: {unfixed}"
          f"\n- Fixed-status High/Critical: {fixed_status}\n- Secrets High/Critical: {secrets}")
    return int(bool(fixable or fixed_status or secrets))


if __name__ == "__main__":
    sys.exit(main())
