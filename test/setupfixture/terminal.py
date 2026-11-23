"""Run a fixture command with independently selectable terminal input/output."""

import argparse
import errno
import os
import selectors
import signal
import subprocess
import sys
import termios
import time


def run(command, *, env=None, cwd=None, answers=b"", tty_in=True, tty_out=True,
        timeout=30):
    """Return (exit code, combined output). The caller supplies the fixture env."""
    master, slave = os.openpty()
    try:
        attrs = termios.tcgetattr(slave)
        attrs[3] &= ~termios.ECHO
        termios.tcsetattr(slave, termios.TCSANOW, attrs)
        process = subprocess.Popen(
            command, cwd=cwd, env=env, start_new_session=True,
            stdin=slave if tty_in else subprocess.PIPE,
            stdout=slave if tty_out else subprocess.PIPE, stderr=subprocess.STDOUT,
        )
    except BaseException:
        os.close(slave)
        os.close(master)
        raise

    output = bytearray()
    deadline = time.monotonic() + timeout
    try:
        if tty_in:
            os.write(master, answers + b"\x04")
        else:
            try:
                process.stdin.write(answers)
            except BrokenPipeError:
                pass
            finally:
                try:
                    process.stdin.close()
                except BrokenPipeError:
                    pass
        if not tty_out:
            os.close(slave)
            slave = None
        with selectors.DefaultSelector() as poll:
            poll.register(master if tty_out else process.stdout, selectors.EVENT_READ)
            while poll.get_map():
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise subprocess.TimeoutExpired(command, timeout, bytes(output))
                # Darwin discards unread output on the last slave close. Keep
                # our copy until the child exits and the master is drained.
                exited = slave is not None and process.poll() is not None
                wait = remaining
                if slave is not None:
                    wait = 0 if exited else min(remaining, 0.05)
                events = poll.select(wait)
                if not events and exited:
                    os.close(slave)
                    slave = None
                for key, _ in events:
                    try:
                        chunk = os.read(key.fd, 65536)
                    except OSError as error:
                        if error.errno != errno.EIO or key.fd != master:
                            raise
                        chunk = b""  # Linux PTY after slave close.
                    if chunk:
                        output.extend(chunk)
                    else:
                        poll.unregister(key.fileobj)
        code = process.wait(timeout=max(0, deadline - time.monotonic()))
        return code, bytes(output)
    except BaseException:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        except PermissionError:
            # macOS can reject killpg for an exited, unreaped child.
            if process.poll() is None:
                raise
        process.wait()
        raise
    finally:
        if slave is not None:
            os.close(slave)
        if process.stdout is not None:
            process.stdout.close()
        os.close(master)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pipe-in", action="store_true")
    parser.add_argument("--pipe-out", action="store_true")
    parser.add_argument("--answers", default="")
    parser.add_argument("--timeout", type=float, default=30)
    parser.add_argument("command", nargs="+")
    args = parser.parse_args()
    code, output = run(args.command, answers=args.answers.encode(),
                       tty_in=not args.pipe_in, tty_out=not args.pipe_out,
                       timeout=args.timeout)
    sys.stdout.buffer.write(output)
    sys.exit(code if code >= 0 else 128 - code)
