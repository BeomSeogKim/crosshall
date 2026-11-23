"""Regression tests for the terminal used by the setup and installer fixtures."""

import itertools
import subprocess
import sys
import unittest
from unittest.mock import patch

import terminal


class TerminalTest(unittest.TestCase):
    def test_timeout_survives_killpg_denial_for_an_exited_child(self):
        with patch.object(terminal.subprocess, "Popen") as popen, \
             patch.object(terminal.os, "killpg", side_effect=PermissionError):
            child = popen.return_value
            child.poll.return_value = 0
            with self.assertRaises(subprocess.TimeoutExpired):
                terminal.run(["child"], timeout=0)
            child.wait.assert_called_once_with()

    def test_child_exits_before_parent_handles_io(self):
        popen = subprocess.Popen

        def finished_child(*args, **kwargs):
            process = popen(*args, **kwargs)
            process.wait(timeout=10)
            return process

        # A fast child may finish while the parent is descheduled after spawn.
        with patch.object(terminal.subprocess, "Popen", finished_child):
            for tty_in, tty_out, code in itertools.product((False, True), (False, True), (0, 17)):
                with self.subTest(tty_in=tty_in, tty_out=tty_out, code=code):
                    child = f"import os; os.write(1, b'marker\\n'); os._exit({code})"
                    got = terminal.run([sys.executable, "-c", child], tty_in=tty_in, tty_out=tty_out)
                    transcript = b"marker\r\n" if tty_out else b"marker\n"
                    self.assertEqual(got, (code, transcript))


if __name__ == "__main__":
    unittest.main()
