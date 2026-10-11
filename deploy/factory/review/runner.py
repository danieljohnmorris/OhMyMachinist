from __future__ import annotations

from dataclasses import dataclass
import os
from pathlib import Path
import shlex
import shutil
import subprocess


DEFAULT_TIMEOUT_SECONDS = 15 * 60


@dataclass(frozen=True)
class CommandResult:
    args: tuple[str, ...]
    returncode: int
    stdout: str
    stderr: str
    timed_out: bool = False

    @property
    def output(self) -> str:
        return "\n".join(part for part in (self.stdout, self.stderr) if part).strip()

    @property
    def ok(self) -> bool:
        return self.returncode == 0 and not self.timed_out


def timeout_from_env(environ: dict[str, str] | None = None) -> int:
    environ = os.environ if environ is None else environ
    try:
        return max(1, int(environ.get("MACHINIST_REVIEW_TIMEOUT_SECONDS", DEFAULT_TIMEOUT_SECONDS)))
    except (TypeError, ValueError):
        return DEFAULT_TIMEOUT_SECONDS


def run_command(
    args: list[str] | tuple[str, ...],
    *,
    cwd: str | Path,
    timeout: int = DEFAULT_TIMEOUT_SECONDS,
    stdin: str | None = None,
) -> CommandResult:
    try:
        completed = subprocess.run(
            list(args),
            cwd=str(cwd),
            input=stdin,
            text=True,
            capture_output=True,
            timeout=timeout,
            check=False,
        )
        return CommandResult(tuple(args), completed.returncode, completed.stdout, completed.stderr)
    except subprocess.TimeoutExpired as error:
        stdout = error.stdout.decode(errors="replace") if isinstance(error.stdout, bytes) else (error.stdout or "")
        stderr = error.stderr.decode(errors="replace") if isinstance(error.stderr, bytes) else (error.stderr or "")
        stderr = f"{stderr}\nCommand timed out after {timeout} seconds.".strip()
        return CommandResult(tuple(args), 124, stdout, stderr, timed_out=True)
    except OSError as error:
        return CommandResult(tuple(args), 127, "", str(error))


def executor_command(executor_cmd: str | None, environ: dict[str, str] | None = None) -> list[str] | None:
    value = executor_cmd or (environ or os.environ).get("MACHINIST_REVIEW_EXECUTOR")
    if not value or not value.strip():
        return None
    try:
        return shlex.split(value)
    except ValueError as error:
        raise ValueError(f"invalid executor command: {error}") from error


def run_executor(
    args: list[str],
    *,
    prompt: str,
    timeout: int = DEFAULT_TIMEOUT_SECONDS,
) -> CommandResult:
    return run_command(args, cwd=Path.cwd(), timeout=timeout, stdin=prompt)


def available(tool: str) -> bool:
    return shutil.which(tool) is not None
