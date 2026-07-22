from __future__ import annotations

from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Protocol, runtime_checkable


@runtime_checkable
class TokenProvider(Protocol):
    def __call__(self) -> str | Awaitable[str]: ...


AsyncTokenProvider = Callable[[], Awaitable[str]]


@dataclass(frozen=True, slots=True)
class StaticToken:
    token: str

    def __post_init__(self) -> None:
        if not self.token.strip():
            raise ValueError("token must be non-empty")

    def __call__(self) -> str:
        return self.token
