"""Judge one test of a `go test -json` stream for scripts/release-gate.sh.

Usage: release_gate_events.py <test> <subtest-prefix>... < events

Exit 0 when the test passed and, for every prefix, a subtest named with it
passed; 2 when nothing failed but a skip kept a required case from passing;
1 otherwise: a failure, a build error or a case that never reported. The
first output line names the reason.
"""

import json
import sys


def main():
    test, prefixes = sys.argv[1], sys.argv[2:]
    outcomes = {}
    output = {}
    for line in sys.stdin:
        try:
            event = json.loads(line)
        except ValueError:
            continue
        name = event.get("Test") or ""
        if name != test and not name.startswith(test + "/"):
            continue
        action = event.get("Action")
        if action == "output":
            output.setdefault(name, []).append(event.get("Output", "").strip())
        elif action in ("pass", "fail", "skip"):
            outcomes[name] = action

    def names(action):
        return sorted(n for n, a in outcomes.items() if a == action)

    def passed(prefix):
        return any(n.startswith(test + "/" + prefix) for n in names("pass"))

    if names("fail"):
        print("failed: " + ", ".join(names("fail")))
        return 1
    missing = [p for p in prefixes if not passed(p)]
    if outcomes.get(test) == "pass" and not missing:
        print("passed: " + ", ".join(n for n in names("pass") if n != test))
        return 0
    if names("skip"):
        reasons = []
        for name in names("skip"):
            said = [o for o in output.get(name, []) if o and not o.startswith(("=== ", "--- "))]
            reasons.append(name + (": " + said[0] if said else ""))
        print("skipped: " + "; ".join(reasons))
        return 2
    print("did not pass: " + (", ".join(missing) if missing else test))
    return 1


if __name__ == "__main__":
    sys.exit(main())
