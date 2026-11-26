"""Judge one test of a `go test -json` stream for scripts/release-gate.sh.

Usage: release_gate_events.py [--strict] <test> <subtest>... < events

Exit 0 when the test passed and, for every prefix, every direct subtest named
with it passed, and at least one did; a nested subtest never stands for its
parent; 2 when nothing failed but a skip kept a required case from passing;
1 otherwise: a failure, a build error or a case that never reported. The
first output line names the reason.

With --strict, require exact direct subtest names and treat any skip of
the named test or its descendants as a failure.
"""

import json
import sys


def main():
    args = sys.argv[1:]
    strict = args[0] == "--strict"
    test, *cases = args[1:] if strict else args
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

    def passed(case):
        if strict:
            return outcomes.get(test + "/" + case) == "pass"
        direct = [n for n in outcomes if n.startswith(test + "/" + case) and n.count("/") == 1]
        return bool(direct) and all(outcomes[n] == "pass" for n in direct)

    if names("fail"):
        print("failed: " + ", ".join(names("fail")))
        return 1
    missing = [case for case in cases if not passed(case)]
    skips = names("skip")
    if outcomes.get(test) == "pass" and not missing and not (strict and skips):
        print("passed: " + ", ".join(n for n in names("pass") if n != test))
        return 0
    if skips:
        reasons = []
        for name in skips:
            said = [o for o in output.get(name, []) if o and not o.startswith(("=== ", "--- "))]
            reasons.append(name + (": " + said[0] if said else ""))
        print("skipped: " + "; ".join(reasons))
        return 1 if strict else 2
    print("did not pass: " + (", ".join(missing) if missing else test))
    return 1


if __name__ == "__main__":
    sys.exit(main())
