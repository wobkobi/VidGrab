"""
Integration tests for download endpoints
Tests /download and /progress API endpoints
"""

import pytest
from fastapi.testclient import TestClient
from server import app
from downloader import download_manager


@pytest.fixture
def client():
    """Test client for FastAPI app"""
    return TestClient(app)


@pytest.fixture(autouse=True)
def reset_download_manager():
    """Reset download manager state before each test"""
    download_manager.current_download = None
    yield
    download_manager.current_download = None


class TestDownloadEndpoint:
    """Test POST /download endpoint"""

    def test_start_download_valid_request(self, client):
        """Valid download request returns 202 with download_id"""
        response = client.post(
            "/download",
            json={
                "url": "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
                "format_id": "best_video",
            },
        )
        assert response.status_code == 202
        data = response.json()
        assert "download_id" in data
        assert data["status"] == "starting"
        assert len(data["download_id"]) == 36  # UUID length

    def test_start_download_invalid_url(self, client):
        """Invalid URL returns 400"""
        response = client.post(
            "/download",
            json={"url": "https://example.com/watch?v=abc", "format_id": "best_video"},
        )
        assert response.status_code == 400
        assert "Invalid YouTube URL" in response.json()["detail"]

    def test_start_download_invalid_format_id(self, client):
        """Invalid format_id returns 400"""
        response = client.post(
            "/download",
            json={
                "url": "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
                "format_id": "../../etc/passwd",
            },
        )
        assert response.status_code == 400
        assert "Invalid format_id" in response.json()["detail"]

    def test_concurrent_download_rejected(self, client):
        """Second download while one is in progress returns 409"""
        # Start first download
        response1 = client.post(
            "/download",
            json={
                "url": "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
                "format_id": "best_video",
            },
        )
        assert response1.status_code == 202
        download_id1 = response1.json()["download_id"]

        # Try to start second download
        response2 = client.post(
            "/download",
            json={
                "url": "https://www.youtube.com/watch?v=jNQXAC9IVRw",
                "format_id": "137",
            },
        )
        assert response2.status_code == 409
        data = response2.json()
        assert "already in progress" in data["error"]
        assert data["current_download_id"] == download_id1

    def test_missing_url_field(self, client):
        """Missing URL field returns 422"""
        response = client.post("/download", json={"format_id": "best_video"})
        assert response.status_code == 422  # Pydantic validation error

    def test_missing_format_id_field(self, client):
        """Missing format_id field returns 422"""
        response = client.post(
            "/download", json={"url": "https://www.youtube.com/watch?v=dQw4w9WgXcQ"}
        )
        assert response.status_code == 422  # Pydantic validation error


class TestProgressEndpoint:
    """Test GET /progress/{download_id} endpoint"""

    def test_get_progress_valid_id(self, client):
        """Progress for valid download_id returns 200"""
        # Start a download first
        response = client.post(
            "/download",
            json={
                "url": "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
                "format_id": "best_video",
            },
        )
        download_id = response.json()["download_id"]

        # Get progress
        progress_response = client.get(f"/progress/{download_id}")
        assert progress_response.status_code == 200
        data = progress_response.json()
        assert data["download_id"] == download_id
        assert data["status"] == "starting"
        assert data["percent"] == 0.0

    def test_get_progress_invalid_id(self, client):
        """Progress for non-existent download_id returns 404"""
        response = client.get("/progress/invalid-uuid-12345")
        assert response.status_code == 404
        # Either "not found" or "no download" message is acceptable
        detail = response.json()["detail"].lower()
        assert "not found" in detail or "no download" in detail

    def test_get_progress_no_download_active(self, client):
        """Progress query when no download active returns 404"""
        response = client.get("/progress/00000000-0000-0000-0000-000000000000")
        assert response.status_code == 404


class TestDownloadStateManagement:
    """Test download state transitions"""

    def test_download_state_initialized_correctly(self, client):
        """Download state contains all required fields"""
        response = client.post(
            "/download",
            json={
                "url": "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
                "format_id": "mp3",
            },
        )
        download_id = response.json()["download_id"]

        progress = client.get(f"/progress/{download_id}").json()

        # Check all required fields are present
        assert "download_id" in progress
        assert "status" in progress
        assert "percent" in progress
        assert "speed" in progress
        assert "eta" in progress
        assert "filename" in progress
        assert "filepath" in progress
        assert "downloaded_bytes" in progress
        assert "total_bytes" in progress
        assert "error" in progress

        # Check initial values
        # Status might be "starting" or "downloading" depending on timing
        assert progress["status"] in ["starting", "downloading", "error"]
        assert progress["percent"] == 0.0
        assert progress["error"] is None
