"""
Integration tests for yt-dlp subprocess spawning and progress parsing
Tests actual video downloads (may be slow, requires network)
"""

import time
import os
import pytest
from fastapi.testclient import TestClient

from server import app
from downloader import download_manager, PROGRESS_REGEX


@pytest.fixture(autouse=True)
def reset_download_manager():
    """Reset download manager state before each test"""
    download_manager.current_download = None
    yield
    download_manager.current_download = None


class TestProgressRegex:
    """Test progress regex parsing without actual downloads"""

    def test_parse_basic_progress(self):
        """Test parsing basic progress line"""
        line = "[download]  25.5% of 123.45MiB at 2.50MiB/s ETA 00:30"
        match = PROGRESS_REGEX.search(line)

        assert match is not None
        assert float(match.group(1)) == 25.5
        assert match.group(2) == "123.45"
        assert match.group(3) == "MiB"
        assert match.group(4) == "2.50MiB/s"
        assert match.group(5) == "00:30"

    def test_parse_progress_without_eta(self):
        """Test parsing progress without ETA"""
        line = "[download]  50.0% of 100MiB at 5.0MiB/s"
        match = PROGRESS_REGEX.search(line)

        assert match is not None
        assert float(match.group(1)) == 50.0
        assert match.group(4) == "5.0MiB/s"

    def test_parse_progress_percent_only(self):
        """Test parsing when only percent is available"""
        line = "[download] 75.3%"
        match = PROGRESS_REGEX.search(line)

        assert match is not None
        assert float(match.group(1)) == 75.3

    def test_parse_complete_progress(self):
        """Test parsing 100% completion"""
        line = "[download] 100% of 250.00MiB at 10.0MiB/s ETA 00:00"
        match = PROGRESS_REGEX.search(line)

        assert match is not None
        assert float(match.group(1)) == 100.0

    def test_parse_progress_with_approximate_size(self):
        """Test parsing with approximate size (tilde)"""
        line = "[download]  33.3% of ~150MiB at 3.0MiB/s ETA 01:00"
        match = PROGRESS_REGEX.search(line)

        assert match is not None
        assert float(match.group(1)) == 33.3


class TestSubprocessIntegration:
    """Integration tests that spawn actual yt-dlp subprocess"""

    @pytest.mark.slow
    def test_download_completes_successfully(self):
        """
        Test complete download flow with a very short video.
        This test requires network access and yt-dlp to be installed.
        """
        client = TestClient(app)

        # Use a very short test video (YouTube test video, 11 seconds)
        # This is a known test video that's unlikely to be removed
        test_url = "https://www.youtube.com/watch?v=jNQXAC9IVRw"

        # Start download
        response = client.post(
            "/download",
            json={"url": test_url, "format_id": "worst"},  # Use worst quality for speed
        )

        assert response.status_code == 202
        data = response.json()
        download_id = data["download_id"]
        assert data["status"] == "starting"

        # Poll progress until complete or error (timeout after 60s)
        max_attempts = 60
        attempt = 0
        last_status = None

        while attempt < max_attempts:
            time.sleep(1)
            progress_response = client.get(f"/progress/{download_id}")
            assert progress_response.status_code == 200

            progress = progress_response.json()
            status = progress["status"]
            last_status = status

            # Status should progress through states
            assert status in ["starting", "downloading", "complete", "error"]

            # If downloading, percent should be present
            if status == "downloading":
                assert "percent" in progress
                assert progress["percent"] >= 0
                assert progress["percent"] <= 100

            # Break if terminal state reached
            if status in ["complete", "error"]:
                break

            attempt += 1

        # Verify we reached completion
        assert (
            last_status == "complete"
        ), f"Download did not complete in time. Last status: {last_status}"

        # Final progress check
        final_progress = client.get(f"/progress/{download_id}").json()
        assert final_progress["status"] == "complete"
        assert final_progress["percent"] == 100.0
        assert final_progress["filename"] is not None
        assert final_progress["filepath"] is not None

        # Verify file exists
        if final_progress["filepath"]:
            assert os.path.exists(
                final_progress["filepath"]
            ), "Downloaded file not found"

    def test_invalid_format_id_causes_error(self):
        """Test that invalid format_id causes download to fail"""
        client = TestClient(app)

        # Start download with invalid format
        response = client.post(
            "/download",
            json={
                "url": "https://www.youtube.com/watch?v=jNQXAC9IVRw",
                "format_id": "nonexistent_format_999",
            },
        )

        assert response.status_code == 202
        download_id = response.json()["download_id"]

        # Wait for download to fail
        time.sleep(3)

        progress = client.get(f"/progress/{download_id}").json()
        assert progress["status"] == "error"
        assert progress["error"] is not None

    def test_concurrent_downloads_rejected(self):
        """Test that concurrent downloads are properly rejected"""
        client = TestClient(app)

        # Start first download
        response1 = client.post(
            "/download",
            json={
                "url": "https://www.youtube.com/watch?v=jNQXAC9IVRw",
                "format_id": "worst",
            },
        )
        assert response1.status_code == 202
        download_id1 = response1.json()["download_id"]

        # Wait a moment for download to start
        time.sleep(0.5)

        # Try to start second download
        response2 = client.post(
            "/download",
            json={
                "url": "https://www.youtube.com/watch?v=jNQXAC9IVRw",
                "format_id": "worst",
            },
        )

        # Should be rejected
        assert response2.status_code == 409
        data = response2.json()
        assert data["error"] == "Download already in progress"
        assert data["current_download_id"] == download_id1


class TestMockSubprocess:
    """Tests that mock subprocess behavior without actual downloads"""

    def test_ytdlp_not_found_error(self):
        """Test handling when yt-dlp is not installed"""
        # This test requires temporarily renaming yt-dlp or testing in env without it
        # For now, skip if yt-dlp exists
        import shutil

        if shutil.which("yt-dlp"):
            pytest.skip("yt-dlp is installed, cannot test not-found scenario")

    def test_progress_increases_monotonically(self):
        """Verify that progress values increase over time"""
        client = TestClient(app)

        response = client.post(
            "/download",
            json={
                "url": "https://www.youtube.com/watch?v=jNQXAC9IVRw",
                "format_id": "worst",
            },
        )

        assert response.status_code == 202
        download_id = response.json()["download_id"]

        # Track progress values
        previous_percent = -1.0

        for _ in range(10):
            time.sleep(1)
            progress = client.get(f"/progress/{download_id}").json()

            if progress["status"] == "downloading":
                current_percent = progress["percent"]
                # Progress should never decrease
                assert current_percent >= previous_percent
                previous_percent = current_percent

            if progress["status"] in ["complete", "error"]:
                break


if __name__ == "__main__":
    pytest.main([__file__, "-v", "-s"])
