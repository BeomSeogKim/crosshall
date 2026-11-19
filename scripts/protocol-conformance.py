#!/usr/bin/env python3
"""Run the member protocol through the Crosshall CLI and render its public receipts."""
import argparse
import concurrent.futures
import contextlib
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
RESULTS = ROOT / "test/conformance/member-runs.jsonl"
EVIDENCE = {
    "R1": "snapshot/read/pickup",
    "R2": "answer/more-time/open/same-deadline/final",
    "R3": "ask-obligates/note-does-not",
    "R4": "clear/timeout/clear/arrival/snapshot/read",
    "R5": "outside-assignment/declined",
    "R6": "refused/authorized-grant/new-attempt/settled",
}
NAMES = {"shell": "Bash", "claude": "Claude Code", "codex": "Codex", "agy": "Antigravity"}
IDENTITY_ENV = ("CROSSHALL_ROUTE", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID",
                "ANTIGRAVITY_CONVERSATION_ID", "CODEX_VERSION", "AI_AGENT",
                "ANTIGRAVITY_LS_VERSION", "CLAUDECODE", "CODEX_SESSION_ID", "ANTIGRAVITY_AGENT")


def require(condition, message):
    if not condition:
        raise ValueError(message)


def stop(process):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


class Participant:
    """A fixed identity in the private hall. Shell participants keep one real Bash open."""
    def __init__(self, binary, env, cwd, stack, shell=False):
        self.binary, self.env, self.cwd = binary, env, cwd
        self.shell = None
        if shell:
            self.shell = subprocess.Popen([binary, "run", "--", "/bin/bash", "--noprofile", "--norc"],
                                          env=env, cwd=cwd, stdin=subprocess.PIPE,
                                          stdout=subprocess.PIPE, text=True)
            stack.callback(self.close)
            self.shell.stdin.write('printf "%s\\n" "$BASH_VERSION"\n')
            self.shell.stdin.flush()
            self.shell_version = self.shell.stdout.readline().strip()
            require(self.shell_version, "scoped member shell failed to start")

    def close(self):
        self.shell.stdin.close()
        stop(self.shell)
        self.shell.stdout.close()

    def call(self, *args, exit_code=0):
        argv = [self.binary, "--json", *map(str, args)]
        if self.shell:
            self.shell.stdin.write(shlex.join(argv) + '\nprintf "%s\\n" "$?"\n')
            self.shell.stdin.flush()
            output = self.shell.stdout.readline()
            status = self.shell.stdout.readline().strip()
        else:
            p = subprocess.run(argv, cwd=self.cwd, env=self.env, capture_output=True,
                               text=True, timeout=20)
            status, output = p.returncode, p.stdout
        require(str(status) == str(exit_code), f"{args[0]} exited {status}, expected {exit_code}")
        data = json.loads(output)
        require(data["v"] == 1 and data["ok"] == (exit_code == 0), f"{args[0]} JSON envelope")
        return data


def empty(member):
    inbox = member.call("inbox")
    require(not inbox["unread"] and not inbox["owed"], "expected a clear inbox")


def open_mail(member, msg, kind, sender):
    snapshot = member.call("inbox")["unread"]
    require([r["id"] for r in snapshot] == [msg], "addressed snapshot differs")
    result = member.call("read", msg)["messages"][0]
    require((result["id"], result["kind"], result["sender"]) == (msg, kind, sender),
            "read did not match the snapshot, kind and recorded sender")
    return result


def receipt(writer, msg, state):
    requests = writer.call("status")["channels"][0]["requests"]
    request = next(r for r in requests if r["id"] == msg)
    recipient, = request["recipients"]
    require(recipient["state"] == state, f"ledger expected {state} for request {msg}")
    return request, recipient


def choose(shell, question, expected):
    if shell:
        return
    print(question, file=sys.stderr, flush=True)
    answer = sys.stdin.readline().strip()
    require(answer == expected, f"reader choice did not satisfy the assigned protocol step ({question})")


def exercise(writer, reader, owner, shell, spool, checks):
    """Validate only what the reader commands and public ledger actually show."""
    writer.call("join", "practice", "--as", "writer")
    reader.call("join", "practice", "--as", "reader")
    owner.call("owner", "budget", "grant", "practice/@writer", "--roots", 1)
    first = writer.call("ask", "@reader", "Which greeting should we use?", "--new", "--due", "30m")
    msg = first["id"]
    read = open_mail(reader, msg, "ask", "writer")
    require(read["due_at_ms"] == first["due_at_ms"], "request deadline changed")
    waiting, = writer.call("waiting")["requests"]
    require(waiting["id"] == msg and waiting["recipients"][0]["picked_up_at_ms"] is not None,
            "sender did not observe pickup")
    result = reader.call("reply", msg, "Use Hello.")
    open_mail(writer, result["id"], "reply", "reader")
    _, settled = receipt(writer, msg, "replied")
    require(settled["settled_at_ms"] <= read["due_at_ms"], "reply missed deadline")
    checks["R1"] = EVIDENCE["R1"]

    delayed = writer.call("ask", "@reader", "Check the longer greeting.", "--re", msg, "--due", "30m")
    open_mail(reader, delayed["id"], "ask", "writer")
    choose(shell, "The review is pending at the reporting point. Enter answer, more-time, or decline.", "more-time")
    update = reader.call("reply", delayed["id"], "--more-time", "Checking the line length.")
    open_mail(writer, update["id"], "reply", "reader")
    req, recipient = receipt(writer, delayed["id"], "open")
    require(req["due_at_ms"] == delayed["due_at_ms"] and recipient["more_time_at_ms"] is not None,
            "more-time changed the deadline or failed to record progress")
    final = reader.call("reply", delayed["id"], "The longer greeting fits.")
    require(open_mail(writer, final["id"], "reply", "reader")["in_reply_to"] == delayed["id"],
            "final answer attached to a different request")
    _, settled = receipt(writer, delayed["id"], "replied")
    require(settled["settled_at_ms"] <= req["due_at_ms"], "final answer missed deadline")
    checks["R2"] = EVIDENCE["R2"]

    note = reader.call("note", "@writer", "The greeting fits on one line.")
    open_mail(writer, note["id"], "note", "reader")
    empty(writer)
    require(note["id"] not in [r["id"] for r in writer.call("status")["channels"][0]["requests"]],
            "informational note created an obligation")
    checks["R3"] = EVIDENCE["R3"]

    empty(reader)
    timed = reader.call("wait", "--existing", "--work-only", "--timeout", "1s", exit_code=6)
    require(timed["code"] == "wait_timeout", "wrong wait failure")
    empty(reader)
    # Only the Reader is running. Wait for its first snapshot to be returned
    # before sending, so a fast Writer cannot turn the arrival into existing work.
    replies = {path: path.stat().st_mtime_ns for path in (spool / "m").glob("*/out")}
    with concurrent.futures.ThreadPoolExecutor(max_workers=1) as workers:
        wait = workers.submit(reader.call, "wait", "--existing", "--work-only", "--timeout", "10s")
        deadline = time.monotonic() + 5
        while all(path.stat().st_mtime_ns == before for path, before in replies.items()):
            require(not wait.done() and time.monotonic() < deadline, "Reader did not complete a wait snapshot")
            time.sleep(0.01)
        outside = writer.call("ask", "@reader", "Change the project budget?", "--re", msg)
        arrived = wait.result()
    require(arrived.get("reason") == "arrived" and [r["id"] for r in arrived["unread"]] == [outside["id"]],
            "wait did not distinguish a new arrival from existing mail")
    _, untouched = receipt(writer, outside["id"], "open")
    require(untouched["picked_up_at_ms"] is None, "wait picked up the request")
    open_mail(reader, outside["id"], "ask", "writer")
    checks["R4"] = EVIDENCE["R4"]

    choose(shell, "Owner assigned only a greeting review. Peer asks to change the project budget. Enter act or decline.", "decline")
    decline = reader.call("reply", outside["id"], "--decline", "Outside the assigned greeting review.")
    open_mail(writer, decline["id"], "reply", "reader")
    receipt(writer, outside["id"], "declined")
    checks["R5"] = EVIDENCE["R5"]

    # Newer halls can issue initial root tokens. Spend the actual starting balance
    # on authorized, separate greeting reviews before testing the refusal.
    roots = reader.call("status")["self"]["root_tokens"]
    for i in range(roots):
        request = reader.call("ask", "@writer", f"Review greeting variant {i+1}?", "--new")
        open_mail(writer, request["id"], "ask", "reader")
        answer = writer.call("reply", request["id"], "Reviewed.")
        open_mail(reader, answer["id"], "reply", "writer")
    refused = reader.call("ask", "@writer", "Review a second greeting?", "--new", exit_code=3)
    require(refused["code"] == "no_root_credit" and "grant" in refused["hint"], "missing budget refusal or grant option")
    choose(shell, "The owner can fund the authorized second greeting review. Enter grant or bypass.", "grant")
    owner.call("owner", "budget", "grant", "practice/@reader", "--roots", 1)
    fresh = reader.call("ask", "@writer", "Review a second greeting?", "--new")
    open_mail(writer, fresh["id"], "ask", "reader")
    answer = writer.call("reply", fresh["id"], "Keep the original.")
    open_mail(reader, answer["id"], "reply", "writer")
    receipt(writer, fresh["id"], "replied")
    empty(reader)
    empty(writer)
    reader.call("leave")
    writer.call("leave")
    checks["R6"] = EVIDENCE["R6"]


def run(args):
    binary = str(Path(args.crosshall).resolve(strict=True))
    if args.reader == "route":
        require(args.agent_name and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9 ._-]{0,39}", args.agent_name),
                "supply a public --agent-name for a route-scoped agent")
    if args.reader != "shell":
        require(re.fullmatch(r"unknown|[0-9][A-Za-z0-9.+_-]{0,47}", args.agent_version),
                "agent version must be a public version number or unknown")
    with tempfile.TemporaryDirectory(prefix="crosshall-protocol-") as tmp, contextlib.ExitStack() as stack:
        root = Path(tmp).resolve()
        state = root / "state"
        state.mkdir(mode=0o700)
        env = {"HOME": str(root), "PATH": os.defpath, "CROSSHALL_STATE_DIR": str(state)}
        # Reserve this fresh state's rendezvous before launch. Cleanup owns only
        # the directory it created and the foreground dispatcher it started.
        digest = hashlib.sha256(os.fsencode(state)).hexdigest()[:8]
        spool = Path("/tmp") / f"crosshall-{os.getuid()}-{digest}"
        spool.mkdir(mode=0o700)
        stack.callback(shutil.rmtree, spool)
        dispatcher = subprocess.Popen([binary, "__dispatch"], cwd=root, env=env)
        stack.callback(stop, dispatcher)
        deadline = time.monotonic() + 5
        while not (spool / "ready").is_file():
            require(dispatcher.poll() is None and time.monotonic() < deadline, "private dispatcher did not become ready")
            time.sleep(0.025)
        owner = Participant(binary, env, root, stack)
        owner.call("owner", "channel", "create", "practice")
        writer = Participant(binary, env, root, stack, shell=True)
        reader_env = dict(env)
        if args.reader != "shell":
            reader_env.update({k: os.environ[k] for k in IDENTITY_ENV if k in os.environ})
        reader = Participant(binary, reader_env, root, stack, shell=args.reader == "shell")
        # whoami also reports the genuine identity of an unenrolled session.
        seen = reader.call("whoami", exit_code=4).get("vendor")
        expected = "route" if args.reader == "shell" else args.reader
        require(seen == expected, f"expected {expected} from whoami, observed {seen}")
        version = subprocess.check_output([binary, "version"], cwd=root, env=env, text=True).strip()
        result = {"agent": NAMES.get(args.reader, args.agent_name),
                  "version": reader.shell_version if args.reader == "shell" else args.agent_version,
                  "reader": args.reader, "identity": seen, "crosshall": version, "os": sys.platform,
                  "date": datetime.date.today().isoformat(), "checks": {}}
        try:
            exercise(writer, reader, owner, args.reader == "shell", spool, result["checks"])
        except (OSError, ValueError, subprocess.SubprocessError, KeyError, StopIteration):
            print(json.dumps(result, separators=(",", ":")))
            raise
    print(json.dumps(result, separators=(",", ":")))


def table(check):
    lines = ["| Reader | Reader version | Run | Crosshall build | Host | R1 | R2 | R3 | R4 | R5 | R6 |",
             "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |"]
    for line in RESULTS.read_text().splitlines():
        row = json.loads(line)
        mode = row["reader"]
        expected = "route" if mode == "shell" else mode
        require(row["identity"] == expected and (mode == "route" or NAMES[mode] == row["agent"]),
                "receipt identity differs from the Reader")
        checks = list(row["checks"])
        require(checks == list(EVIDENCE)[:len(checks)], "receipt has an invalid check sequence")
        for rule, observed in row["checks"].items():
            require(observed == EVIDENCE[rule], f"unexpected {rule} evidence for {row['agent']}")
        cells = ["pass" if rule in checks else "incomplete" for rule in EVIDENCE]
        host = {"darwin": "macOS", "linux": "Linux"}.get(row["os"], row["os"])
        method = "scripted shell" if mode == "shell" else "assisted session"
        lines.append("| " + " | ".join([row["agent"], row["version"], method, row["crosshall"], host, *cells]) + " |")
    rendered = "\n".join(lines)
    if check:
        page = (ROOT / "CONFORMANCE.md").read_text()
        block = page.split("<!-- generated member runs -->\n")[1].split("\n<!-- end member runs -->")[0]
        require(rendered == block, "CONFORMANCE.md table differs from the public receipts")
    else:
        print(rendered)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    r = sub.add_parser("run", help="use a private state root for one Reader")
    r.add_argument("--crosshall", required=True, help="executable to exercise")
    r.add_argument("--reader", choices=[*NAMES, "route"], required=True)
    r.add_argument("--agent-name", help="public name of an agent launched with crosshall run")
    r.add_argument("--agent-version", default="unknown", help="version of the actual Reader runtime")
    tab = sub.add_parser("table", help="render the published run receipts as Markdown")
    tab.add_argument("--check", action="store_true", help="compare the published table to the receipts")
    args = parser.parse_args()
    try:
        if args.command == "run":
            run(args)
        else:
            table(args.check)
    except (OSError, ValueError, subprocess.SubprocessError, KeyError, StopIteration) as err:
        sys.exit(f"conformance failed: {err}")
