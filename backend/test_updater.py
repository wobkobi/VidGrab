"""Updater tests with mocked PyPI and pip."""

from __future__ import annotations

import subprocess
from pathlib import Path

import pytest

from updater import CHECK_INTERVAL, UpdateBusy, UpdateError, Updater, version_key


class Fakes:
    """Mutable fake PyPI, pip and clock."""

    def __init__(self) -> None:
        self.latest: str | None = "2026.11.01"
        self.installed = "2026.03.03"
        self.now = 1_000_000.0
        self.pypi_calls = 0
        self.pip_calls = 0
        self.pip_code = 0

    def fetch(self) -> str | None:
        self.pypi_calls += 1
        return self.latest

    def pip(self) -> subprocess.CompletedProcess:
        self.pip_calls += 1
        if self.pip_code == 0:
            self.installed = self.latest or self.installed
        return subprocess.CompletedProcess([], self.pip_code, stdout="", stderr="pip error text")

    def updater(self, path: Path) -> Updater:
        return Updater(
            path,
            latest_fetcher=self.fetch,
            pip_runner=self.pip,
            version_reader=lambda: self.installed,
            clock=lambda: self.now,
        )


@pytest.fixture
def fakes() -> Fakes:
    return Fakes()


def test_version_key() -> None:
    assert version_key("2026.11.01") > version_key("2026.03.03")
    assert version_key("2026.03.03.1") > version_key("2026.03.03")
    assert version_key(None) < version_key("2020.01.01")


def test_check_at_most_once_a_day(tmp_path: Path, fakes: Fakes) -> None:
    updater = fakes.updater(tmp_path / "state.json")
    assert updater.check() == "2026.11.01"
    updater.check()
    assert fakes.pypi_calls == 1
    # State survives a restart
    assert fakes.updater(tmp_path / "state.json").check() == "2026.11.01"
    assert fakes.pypi_calls == 1
    fakes.now += CHECK_INTERVAL
    updater.check()
    assert fakes.pypi_calls == 2
    updater.check(force=True)
    assert fakes.pypi_calls == 3


def test_offline_check_keeps_previous_latest(tmp_path: Path, fakes: Fakes) -> None:
    updater = fakes.updater(tmp_path / "state.json")
    updater.check()
    fakes.latest = None
    assert updater.check(force=True) == "2026.11.01"


def test_update_available(tmp_path: Path, fakes: Fakes) -> None:
    updater = fakes.updater(tmp_path / "state.json")
    assert not updater.update_available()
    updater.check()
    assert updater.update_available()
    fakes.installed = "2026.11.01"
    assert not updater.update_available()


def test_update_success_and_failure(tmp_path: Path, fakes: Fakes) -> None:
    updater = fakes.updater(tmp_path / "state.json")
    updater.check()
    assert updater.update() == {"updated": True, "version": "2026.11.01"}
    assert updater.update() == {"updated": False, "version": "2026.11.01"}
    fakes.pip_code = 1
    with pytest.raises(UpdateError) as info:
        updater.update()
    assert "pip error text" in (info.value.detail or "")


@pytest.mark.parametrize(("enabled", "expect_pip"), [(True, 1), (False, 0)])
def test_auto_update(tmp_path: Path, fakes: Fakes, enabled: bool, expect_pip: int) -> None:
    updater = fakes.updater(tmp_path / "state.json")
    updater.auto_update(lambda: enabled)
    assert fakes.pip_calls == expect_pip


def test_auto_update_postponed_while_busy(tmp_path: Path, fakes: Fakes) -> None:
    def busy() -> dict[str, object]:
        raise UpdateBusy("downloads running")

    updater = fakes.updater(tmp_path / "state.json")
    updater.auto_update(lambda: True, busy)
    assert fakes.pip_calls == 0
    # Not marked as attempted, so the next startup tries again
    updater.auto_update(lambda: True)
    assert fakes.pip_calls == 1


def test_auto_update_tries_each_release_once(tmp_path: Path, fakes: Fakes) -> None:
    """pip may settle on an older release (e.g. the newest needs a newer Python)."""

    def partial_pip() -> subprocess.CompletedProcess:
        fakes.pip_calls += 1
        fakes.installed = "2026.10.21"
        return subprocess.CompletedProcess([], 0, stdout="", stderr="")

    updater = fakes.updater(tmp_path / "state.json")
    updater._run_pip = partial_pip
    updater.auto_update(lambda: True)
    assert updater.update_available()
    fakes.updater(tmp_path / "state.json").auto_update(lambda: True)
    assert fakes.pip_calls == 1
    fakes.latest = "2026.12.01"
    fakes.now += CHECK_INTERVAL
    later = fakes.updater(tmp_path / "state.json")
    later._run_pip = partial_pip
    later.auto_update(lambda: True)
    assert fakes.pip_calls == 2


def test_auto_update_swallows_pip_failure(tmp_path: Path, fakes: Fakes) -> None:
    fakes.pip_code = 1
    updater = fakes.updater(tmp_path / "state.json")
    updater.auto_update(lambda: True)
    assert fakes.pip_calls == 1


def test_corrupt_state_file_ignored(tmp_path: Path, fakes: Fakes) -> None:
    path = tmp_path / "state.json"
    path.write_text("{not json")
    assert fakes.updater(path).latest is None
