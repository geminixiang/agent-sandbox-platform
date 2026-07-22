from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from typing import Literal

LeaseStatus = Literal["active", "released", "expired"]


@dataclass(frozen=True, slots=True)
class LeaseRecord:
    id: str
    pool: str
    status: LeaseStatus
    created_at: datetime
    expires_at: datetime
    last_used_at: datetime

    @classmethod
    def from_dict(cls, value: dict[str, object]) -> LeaseRecord:
        return cls(
            id=str(value["id"]),
            pool=str(value["pool"]),
            status=_status(value["status"]),
            created_at=_datetime(value["createdAt"]),
            expires_at=_datetime(value["expiresAt"]),
            last_used_at=_datetime(value["lastUsedAt"]),
        )


@dataclass(frozen=True, slots=True)
class CommandResult:
    stdout: str
    stderr: str
    exit_code: int

    @property
    def succeeded(self) -> bool:
        return self.exit_code == 0


def _datetime(value: object) -> datetime:
    if not isinstance(value, str):
        raise TypeError("lease timestamp must be a string")
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def _status(value: object) -> LeaseStatus:
    if value not in {"active", "released", "expired"}:
        raise ValueError(f"unknown lease status {value!r}")
    return value  # type: ignore[return-value]
