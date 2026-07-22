from __future__ import annotations

from ._models import CommandResult


class SandboxError(Exception):
    def __init__(self, message: str, *, code: str | None = None, status: int | None = None) -> None:
        super().__init__(message)
        self.code = code
        self.status = status


class CommandFailedError(SandboxError):
    def __init__(self, command: str, result: CommandResult) -> None:
        super().__init__(
            f"command exited with status {result.exit_code}",
            code="COMMAND_FAILED",
        )
        self.command = command
        self.result = result


class SandboxNotFoundError(SandboxError):
    pass


class SandboxNotActiveError(SandboxError):
    pass


class SandboxExpiredError(SandboxNotActiveError):
    pass


class SandboxQuotaExceededError(SandboxError):
    pass


class SandboxAbortedError(SandboxError):
    pass


class SandboxFileNotFoundError(SandboxError):
    pass


class SandboxTransferTooLargeError(SandboxError):
    pass


class SandboxTransferLimitError(SandboxError):
    pass


class SandboxIntegrityError(SandboxError):
    pass


class SandboxStreamingNotSupportedError(SandboxError):
    pass


class SandboxInvalidCursorError(SandboxError):
    pass


class SandboxCursorExpiredError(SandboxError):
    pass


class SandboxUnknownPoolError(SandboxError):
    pass


_ERROR_TYPES: dict[str, type[SandboxError]] = {
    "LEASE_NOT_FOUND": SandboxNotFoundError,
    "LEASE_NOT_ACTIVE": SandboxNotActiveError,
    "LEASE_QUOTA_EXCEEDED": SandboxQuotaExceededError,
    "ABORTED": SandboxAbortedError,
    "FILE_NOT_FOUND": SandboxFileNotFoundError,
    "TRANSFER_TOO_LARGE": SandboxTransferTooLargeError,
    "TRANSFER_LIMIT_REACHED": SandboxTransferLimitError,
    "CONTENT_LENGTH_MISMATCH": SandboxIntegrityError,
    "CONTENT_DIGEST_MISMATCH": SandboxIntegrityError,
    "INVALID_CONTENT_DIGEST": SandboxIntegrityError,
    "STREAMING_NOT_SUPPORTED": SandboxStreamingNotSupportedError,
    "INVALID_CURSOR": SandboxInvalidCursorError,
    "CURSOR_EXPIRED": SandboxCursorExpiredError,
    "UNKNOWN_POOL": SandboxUnknownPoolError,
}


def error_from_response(*, status: int, code: str | None, message: str) -> SandboxError:
    error_type = _ERROR_TYPES.get(code or "", SandboxError)
    return error_type(message, status=status, code=code)
