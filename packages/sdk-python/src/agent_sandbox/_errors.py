from __future__ import annotations


class SandboxError(Exception):
    def __init__(self, message: str, *, code: str | None = None, status: int | None = None) -> None:
        super().__init__(message)
        self.code = code
        self.status = status


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


_ERROR_TYPES: dict[str, type[SandboxError]] = {
    "LEASE_NOT_FOUND": SandboxNotFoundError,
    "LEASE_NOT_ACTIVE": SandboxNotActiveError,
    "LEASE_QUOTA_EXCEEDED": SandboxQuotaExceededError,
    "ABORTED": SandboxAbortedError,
}


def error_from_response(*, status: int, code: str | None, message: str) -> SandboxError:
    error_type = _ERROR_TYPES.get(code or "", SandboxError)
    return error_type(message, status=status, code=code)
