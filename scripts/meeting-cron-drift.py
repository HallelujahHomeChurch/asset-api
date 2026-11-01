#!/usr/bin/env python3
"""Check that the asset-scan-worker KEDA cron rules cover every scheduled meeting.

The warm windows are hard-coded in infra/main.bicep (the asset-scan-worker
Container App scale rules). This check compares them with the public meeting
occurrences so a changed meeting time is noticed before the worker is cold
during a meeting. Adapted from hhc-line-function-bot scripts/meeting-cron-drift.py.

Exit codes: 0 covered, 1 coverage gap (report written), 2 invalid input.
"""
import argparse
import datetime as dt
import json
from pathlib import Path
import re
import sys
import urllib.request

try:
    from zoneinfo import ZoneInfo

    TAIPEI = ZoneInfo("Asia/Taipei")
except Exception:  # pragma: no cover - runners without tzdata
    # Asia/Taipei has had no DST since 1979.
    TAIPEI = dt.timezone(dt.timedelta(hours=8), "Asia/Taipei")

DEFAULT_URL = "https://www.alive.org.tw/api/meeting-occurrences"
DEFAULT_TEMPLATE = Path(__file__).resolve().parent.parent / "infra" / "main.bicep"
APP_NAME = "asset-scan-worker"
LEAD = dt.timedelta(minutes=5)
TAIL = dt.timedelta(minutes=10)
HORIZON = dt.timedelta(days=28)
ISSUE_TITLE = "Meeting cron drift: asset-scan-worker"
CRON_PATTERN = re.compile(r"^(\d{1,2}) (\d{1,2}) \* \* ([0-6](?:,[0-6])*)$")
DAY_NAMES = ["sun", "mon", "tue", "wed", "thu", "fri", "sat"]


class DriftInputError(Exception):
    pass


def parse_cron(expression):
    """Parse only the `M H * * D[,D]` shape used by the scan worker."""
    match = CRON_PATTERN.fullmatch(expression.strip())
    if not match:
        raise DriftInputError(f"unsupported cron expression: {expression!r}")
    minute, hour = int(match.group(1)), int(match.group(2))
    if minute > 59 or hour > 23:
        raise DriftInputError(f"cron time out of range: {expression!r}")
    days = [int(day) for day in match.group(3).split(",")]
    if len(set(days)) != len(days):
        raise DriftInputError(f"duplicate cron weekday: {expression!r}")
    return hour * 60 + minute, frozenset(days)


RESOURCE_PATTERN = re.compile(r"^resource \w+ '([^']+)' = .*\{$")
KEY_PATTERN = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*): (.+)$")


def _strip_comment(line):
    """Drop a trailing `//` comment that is not inside a single-quoted string."""
    quoted = False
    for index, char in enumerate(line):
        if char == "'":
            quoted = not quoted
        elif not quoted and line.startswith("//", index):
            return line[:index]
    return line


def _value(text):
    """Return a quoted Bicep string literal unquoted, or the raw expression."""
    if len(text) >= 2 and text[0] == text[-1] == "'":
        inner = text[1:-1]
        if "'" in inner or "${" in inner:
            raise DriftInputError(f"unsupported Bicep string: {text!r}")
        return inner
    if text in ("{", "[") or text.startswith(("{ ", "[ ")):
        raise DriftInputError(f"unsupported inline Bicep value: {text!r}")
    return {"expression": text}


def _parse_object(lines, index):
    """Parse a multi-line Bicep object whose opening `{` was already consumed."""
    result = {}
    while index < len(lines):
        text = lines[index]
        index += 1
        if text == "}":
            return result, index
        match = KEY_PATTERN.fullmatch(text)
        if not match:
            raise DriftInputError(f"unsupported scale rule Bicep: {text!r}")
        key, value = match.group(1), match.group(2)
        if key in result:
            raise DriftInputError(f"duplicate Bicep key: {key}")
        if value == "{":
            result[key], index = _parse_object(lines, index)
        elif value == "[":
            result[key], index = _parse_array(lines, index)
        else:
            result[key] = _value(value)
    raise DriftInputError("unterminated Bicep object")


def _parse_array(lines, index):
    """Parse a multi-line Bicep array of objects whose `[` was already consumed."""
    items = []
    while index < len(lines):
        text = lines[index]
        index += 1
        if text == "]":
            return items, index
        if text != "{":
            raise DriftInputError(f"unsupported scale rule Bicep: {text!r}")
        item, index = _parse_object(lines, index)
        items.append(item)
    raise DriftInputError("unterminated Bicep array")


def _app_block(template_text):
    """Return the stripped body lines of the Container App resource named APP_NAME."""
    lines = [_strip_comment(line).strip() for line in template_text.splitlines()]
    blocks = []
    index = 0
    while index < len(lines):
        match = RESOURCE_PATTERN.fullmatch(lines[index])
        index += 1
        if not match or not match.group(1).startswith("Microsoft.App/containerApps@"):
            continue
        depth, body = 1, []
        while index < len(lines) and depth:
            text = lines[index]
            index += 1
            depth += text.count("{") + text.count("[") - text.count("}") - text.count("]")
            if depth:
                body.append(text)
        if depth:
            raise DriftInputError("unterminated Container App resource")
        if f"name: '{APP_NAME}'" in body[:3]:
            blocks.append([text for text in body if text])
    if len(blocks) != 1:
        raise DriftInputError(f"template must declare exactly one {APP_NAME} Container App")
    return blocks[0]


def scale_rules(template_text):
    """Return the `properties.template.scale.rules` items of the asset-scan-worker app."""
    body = _app_block(template_text)
    starts = [i for i, text in enumerate(body) if text == "rules: ["]
    if len(starts) != 1:
        raise DriftInputError(f"{APP_NAME} must contain exactly one scale rules list")
    rules, _ = _parse_array(body, starts[0] + 1)
    return rules


def cron_rules(template_text):
    """Return validated KEDA cron windows as dictionaries."""
    windows = []
    for rule in scale_rules(template_text):
        custom = rule.get("custom")
        if not isinstance(custom, dict) or custom.get("type") != "cron":
            continue
        name = rule.get("name")
        metadata = custom.get("metadata")
        if not isinstance(name, str) or not isinstance(metadata, dict):
            raise DriftInputError(f"cron rule is malformed: {name!r}")
        if set(metadata) != {"timezone", "start", "end", "desiredReplicas"}:
            raise DriftInputError(f"cron rule {name} has unexpected metadata keys")
        if metadata["timezone"] != "Asia/Taipei":
            raise DriftInputError(f"cron rule {name} must use Asia/Taipei")
        if metadata["desiredReplicas"] != "1":
            raise DriftInputError(f"cron rule {name} must request one replica")
        start, start_days = parse_cron(metadata["start"])
        end, end_days = parse_cron(metadata["end"])
        if start_days != end_days or end <= start:
            raise DriftInputError(f"cron rule {name} must start and end on the same day")
        windows.append(
            {
                "name": name,
                "start": start,
                "end": end,
                "days": start_days,
                "startExpression": metadata["start"],
                "endExpression": metadata["end"],
            }
        )
    if not windows:
        raise DriftInputError(f"{APP_NAME} has no cron scale rules")
    return windows


def _instant(value, field):
    if not isinstance(value, str):
        raise DriftInputError(f"occurrence {field} is missing")
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as error:
        raise DriftInputError(f"occurrence {field} is not ISO-8601") from error
    if parsed.tzinfo is None:
        raise DriftInputError(f"occurrence {field} has no offset")
    return parsed


def occurrence_items(payload):
    """Accept a bare list or a single-list envelope such as {"occurrences": [...]}."""
    if isinstance(payload, dict):
        for key in ("occurrences", "items", "data"):
            if isinstance(payload.get(key), list):
                payload = payload[key]
                break
    if not isinstance(payload, list):
        raise DriftInputError("meeting occurrences response is not a list")
    for item in payload:
        if not isinstance(item, dict):
            raise DriftInputError("meeting occurrence is not an object")
    return payload


def upcoming(payload, now):
    """Scheduled occurrences that have not finished and start within the horizon."""
    selected = []
    for item in occurrence_items(payload):
        if item.get("status") != "scheduled":
            continue
        starts_at = _instant(item.get("startsAt"), "startsAt")
        ends_at = _instant(item.get("endsAt"), "endsAt")
        if ends_at <= starts_at:
            raise DriftInputError("occurrence ends before it starts")
        if ends_at + TAIL <= now or starts_at > now + HORIZON:
            continue
        selected.append({**item, "_start": starts_at, "_end": ends_at})
    return sorted(selected, key=lambda item: item["_start"])


def required_window(occurrence):
    start = (occurrence["_start"] - LEAD).astimezone(TAIPEI)
    end = (occurrence["_end"] + TAIL).astimezone(TAIPEI)
    return start, end


def covering_rule(occurrence, windows):
    start, end = required_window(occurrence)
    if start.date() != end.date():
        return None
    weekday = (start.weekday() + 1) % 7  # cron: 0 = Sunday
    start_minute = start.hour * 60 + start.minute
    end_minute = end.hour * 60 + end.minute + (1 if end.second or end.microsecond else 0)
    for window in windows:
        if weekday in window["days"] and window["start"] <= start_minute and end_minute <= window["end"]:
            return window
    return None


def suggested_rule(occurrence):
    start, end = required_window(occurrence)
    weekday = (start.weekday() + 1) % 7
    end_ceiling = end + dt.timedelta(minutes=1) if end.second or end.microsecond else end
    return {
        "name": f"{DAY_NAMES[weekday]}-{start.hour:02d}{start.minute:02d}",
        "start": f"{start.minute} {start.hour} * * {weekday}",
        "end": f"{end_ceiling.minute} {end_ceiling.hour} * * {weekday}",
    }


def check(payload, template_text, now):
    windows = cron_rules(template_text)
    occurrences = upcoming(payload, now)
    gaps = []
    for occurrence in occurrences:
        if covering_rule(occurrence, windows) is None:
            gaps.append(occurrence)
    return {"windows": windows, "occurrences": occurrences, "gaps": gaps}


def render_report(result, now):
    lines = [
        "The asset-scan-worker KEDA cron rules in `infra/main.bicep` do not cover",
        "every scheduled meeting occurrence (5 minutes before start to 10 minutes after end,",
        "Asia/Taipei). Edit the cron rules and release, or confirm the meeting schedule.",
        "",
        f"Checked at {now.astimezone(TAIPEI).isoformat(timespec='minutes')} for the next 28 days.",
        "",
        "| meetingKey | startsAt | endsAt | suggested rule |",
        "|---|---|---|---|",
    ]
    for gap in result["gaps"]:
        rule = suggested_rule(gap)
        lines.append(
            "| {key} | {start} | {end} | `{name}`: start `{rule_start}`, end `{rule_end}` |".format(
                key=str(gap.get("meetingKey") or "unknown"),
                start=gap["_start"].astimezone(TAIPEI).isoformat(timespec="minutes"),
                end=gap["_end"].astimezone(TAIPEI).isoformat(timespec="minutes"),
                name=rule["name"],
                rule_start=rule["start"],
                rule_end=rule["end"],
            )
        )
    lines += ["", "Suggested Bicep scale rules (merge with existing rules as appropriate):", "", "```bicep"]
    seen = set()
    for gap in result["gaps"]:
        rule = suggested_rule(gap)
        if rule["name"] in seen:
            continue
        seen.add(rule["name"])
        lines += [
            "{",
            f"  name: '{rule['name']}'",
            "  custom: {",
            "    type: 'cron'",
            "    metadata: {",
            "      timezone: 'Asia/Taipei'",
            f"      start: '{rule['start']}'",
            f"      end: '{rule['end']}'",
            "      desiredReplicas: '1'",
            "    }",
            "  }",
            "}",
        ]
    lines.append("```")
    return "\n".join(lines) + "\n"


def fetch(url):
    request = urllib.request.Request(url, headers={"Accept": "application/json"})
    with urllib.request.urlopen(request, timeout=30) as response:
        if response.status != 200:
            raise DriftInputError(f"meeting occurrences returned HTTP {response.status}")
        return json.load(response)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default=DEFAULT_URL)
    parser.add_argument("--input", type=Path, help="read occurrences JSON from a file instead")
    parser.add_argument("--template", type=Path, default=DEFAULT_TEMPLATE)
    parser.add_argument("--report", type=Path, help="write a Markdown gap report here")
    parser.add_argument("--now", help="ISO-8601 instant (testing)")
    args = parser.parse_args(argv)
    try:
        now = _instant(args.now, "now") if args.now else dt.datetime.now(dt.timezone.utc)
        payload = json.loads(args.input.read_text(encoding="utf-8")) if args.input else fetch(args.url)
        result = check(payload, args.template.read_text(encoding="utf-8"), now)
    except (DriftInputError, OSError, ValueError) as error:
        print(f"meeting cron drift check failed: {error}", file=sys.stderr)
        return 2
    for window in result["windows"]:
        print(f"rule {window['name']}: {window['startExpression']} -> {window['endExpression']}")
    for occurrence in result["occurrences"]:
        status = "GAP" if occurrence in result["gaps"] else "covered"
        start = occurrence["_start"].astimezone(TAIPEI).isoformat(timespec="minutes")
        print(f"{status}: {occurrence.get('meetingKey')} {start}")
    print(f"{len(result['occurrences'])} scheduled occurrences, {len(result['gaps'])} gaps")
    if not result["gaps"]:
        return 0
    if args.report:
        args.report.write_text(render_report(result, now), encoding="utf-8")
    return 1


if __name__ == "__main__":
    sys.exit(main())
