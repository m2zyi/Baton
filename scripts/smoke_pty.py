#!/usr/bin/env python3
"""Exercise the native terminal handoff with an isolated, synthetic Claude CLI."""

import errno
import json
import os
from pathlib import Path
import pty
import select
import signal
import sys
import tempfile
import time


FAKE_CLAUDE = r'''
import json
import os
from pathlib import Path
import sys
import termios
import tty

assert all(os.isatty(fd) for fd in (0, 1, 2)), "native streams are not TTYs"
assert sys.argv[1] == "--session-id"
session_id, prompt = sys.argv[2:4]
before = termios.tcgetattr(0)
try:
    tty.setraw(0)
    os.write(1, b"RAW_READY\n")
    assert os.read(0, 1) == b"x", "raw input did not arrive"
finally:
    termios.tcsetattr(0, termios.TCSANOW, before)

records = [
    {"type": "user", "uuid": "smoke-user", "sessionId": session_id,
     "cwd": os.getcwd(), "message": {"role": "user", "content": prompt}},
    {"type": "assistant", "uuid": "smoke-assistant", "parentUuid": "smoke-user",
     "sessionId": session_id, "cwd": os.getcwd(),
     "message": {"role": "assistant", "content": "PTY smoke passed"}},
]
path = Path(os.environ["CLAUDE_CONFIG_DIR"]) / "projects" / "synthetic" / (session_id + ".jsonl")
path.parent.mkdir(parents=True, exist_ok=True)
path.write_text("".join(json.dumps(record) + "\n" for record in records))
print("NATIVE_DONE", flush=True)
'''


def main():
    binary = Path(sys.argv[1] if len(sys.argv) > 1 else "bin/terminus").resolve()
    if not binary.is_file():
        raise SystemExit("Build first: go build -o bin/terminus ./cmd/terminus")
    with tempfile.TemporaryDirectory(prefix="terminus-pty-") as temporary:
        root = Path(temporary)
        fake = root / "fake-claude"
        fake.write_text("#!" + sys.executable + "\n" + FAKE_CLAUDE)
        fake.chmod(0o700)
        environment = dict(os.environ, TERMINUS_HOME=str(root / "data"),
                           CLAUDE_CONFIG_DIR=str(root / "claude"),
                           CODEX_HOME=str(root / "codex"),
                           TERMINUS_CLAUDE_BIN=str(fake),
                           TERMINUS_CODEX_BIN=str(root / "unused-codex"))
        pid, terminal = pty.fork()
        if pid == 0:
            os.chdir(root)
            os.execve(str(binary), [str(binary), "start", "--agent", "claude",
                                   "--prompt", "synthetic PTY request"], environment)
        transcript = bytearray()
        reaped = False
        try:
            raw_sent = quit_sent = False
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                if select.select([terminal], [], [], 0.1)[0]:
                    try:
                        chunk = os.read(terminal, 65536)
                    except OSError as error:
                        if error.errno != errno.EIO:
                            raise
                        chunk = b""
                    if not chunk:
                        break
                    transcript.extend(chunk)
                    if not raw_sent and b"RAW_READY" in transcript:
                        os.write(terminal, b"x")
                        raw_sent = True
                    if not quit_sent and b"NATIVE_DONE" in transcript and b"Select:" in transcript:
                        os.write(terminal, b"q\n")
                        quit_sent = True
                found, status = os.waitpid(pid, os.WNOHANG)
                if found:
                    reaped = True
                    assert os.waitstatus_to_exitcode(status) == 0, transcript.decode(errors="replace")
                    break
            if not reaped:
                # Closing the final PTY descriptor can precede waitpid's exit
                # notification. Allow that short kernel scheduling gap.
                while time.monotonic() < deadline:
                    found, status = os.waitpid(pid, os.WNOHANG)
                    if found:
                        reaped = True
                        assert os.waitstatus_to_exitcode(status) == 0, transcript.decode(errors="replace")
                        break
                    select.select([], [], [], 0.01)
            assert reaped and raw_sent and quit_sent, transcript.decode(errors="replace")
            directories = list((root / "data").iterdir())
            assert len(directories) == 1
            manifest = json.loads((directories[0] / "conversation.json").read_text())
            assert manifest["active_agent"] == "claude" and not manifest.get("pending")
            events = [json.loads(line)["event"] for line in
                      (directories[0] / "events.jsonl").read_text().splitlines()]
            assert [event["text"] for event in events] == ["synthetic PTY request", "PTY smoke passed"]
            print("PASS: native stdin/stdout/stderr are TTYs; raw input, receipt binding, journal, menu return, and exit verified.")
        finally:
            if not reaped:
                os.kill(pid, signal.SIGKILL)
                os.waitpid(pid, 0)
            os.close(terminal)


if __name__ == "__main__":
    main()
