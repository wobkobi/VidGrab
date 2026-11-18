"""
Unit tests for validation.py
Tests URL and format_id validation with security focus
"""

import pytest
from fastapi import HTTPException
from validation import validate_youtube_url, validate_format_id


class TestValidateYoutubeUrl:
    """Test YouTube URL validation"""

    def test_valid_youtube_watch_url(self):
        """Valid youtube.com/watch?v=... URL"""
        video_id = validate_youtube_url("https://www.youtube.com/watch?v=dQw4w9WgXcQ")
        assert video_id == "dQw4w9WgXcQ"

    def test_valid_youtube_watch_url_no_www(self):
        """Valid youtube.com URL without www"""
        video_id = validate_youtube_url("https://youtube.com/watch?v=dQw4w9WgXcQ")
        assert video_id == "dQw4w9WgXcQ"

    def test_valid_youtu_be_short_url(self):
        """Valid youtu.be/... short URL"""
        video_id = validate_youtube_url("https://youtu.be/dQw4w9WgXcQ")
        assert video_id == "dQw4w9WgXcQ"

    def test_valid_youtube_with_query_params(self):
        """Valid URL with additional query parameters"""
        video_id = validate_youtube_url(
            "https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=42s"
        )
        assert video_id == "dQw4w9WgXcQ"

    def test_valid_http_protocol(self):
        """Valid URL with http (not https)"""
        video_id = validate_youtube_url("http://www.youtube.com/watch?v=dQw4w9WgXcQ")
        assert video_id == "dQw4w9WgXcQ"

    def test_invalid_domain(self):
        """Reject non-YouTube domain"""
        with pytest.raises(HTTPException) as exc_info:
            validate_youtube_url("https://example.com/watch?v=dQw4w9WgXcQ")
        assert exc_info.value.status_code == 400
        assert "Invalid YouTube URL" in exc_info.value.detail

    def test_invalid_video_id_too_short(self):
        """Reject video ID that's not 11 characters"""
        with pytest.raises(HTTPException) as exc_info:
            validate_youtube_url("https://www.youtube.com/watch?v=short")
        assert exc_info.value.status_code == 400

    def test_invalid_video_id_too_long(self):
        """Reject video ID that's too long"""
        with pytest.raises(HTTPException) as exc_info:
            validate_youtube_url("https://www.youtube.com/watch?v=dQw4w9WgXcQextra")
        assert exc_info.value.status_code == 400

    def test_invalid_video_id_special_chars(self):
        """Reject video ID with invalid characters"""
        with pytest.raises(HTTPException) as exc_info:
            validate_youtube_url("https://www.youtube.com/watch?v=../etc/pass")
        assert exc_info.value.status_code == 400

    def test_empty_url(self):
        """Reject empty URL"""
        with pytest.raises(HTTPException) as exc_info:
            validate_youtube_url("")
        assert exc_info.value.status_code == 400

    def test_none_url(self):
        """Reject None URL"""
        with pytest.raises(HTTPException) as exc_info:
            validate_youtube_url(None)
        assert exc_info.value.status_code == 400

    def test_command_injection_attempt(self):
        """Reject command injection attempts"""
        malicious_urls = [
            "https://www.youtube.com/watch?v=dQw4w9WgXcQ; rm -rf /",
            "https://www.youtube.com/watch?v=dQw4w9WgXcQ`whoami`",
            "https://www.youtube.com/watch?v=dQw4w9WgXcQ$(ls)",
        ]
        for url in malicious_urls:
            with pytest.raises(HTTPException):
                validate_youtube_url(url)


class TestValidateFormatId:
    """Test format_id validation"""

    def test_valid_numeric_format_id(self):
        """Valid numeric format ID"""
        assert validate_format_id("137") == "137"

    def test_valid_special_format_id(self):
        """Valid special format ID"""
        assert validate_format_id("best_video") == "best_video"

    def test_valid_with_hyphen(self):
        """Valid format ID with hyphen"""
        assert validate_format_id("bestvideo-mp4") == "bestvideo-mp4"

    def test_valid_mp3(self):
        """Valid mp3 format ID"""
        assert validate_format_id("mp3") == "mp3"

    def test_invalid_path_traversal(self):
        """Reject path traversal attempt"""
        with pytest.raises(HTTPException) as exc_info:
            validate_format_id("../../etc/passwd")
        assert exc_info.value.status_code == 400
        assert "Invalid format_id" in exc_info.value.detail

    def test_invalid_shell_metacharacters(self):
        """Reject shell metacharacters (non-whitespace)"""
        # These should be rejected even after strip()
        invalid_chars = [";", "|", "&", "$", "`", "(", ")", "<", ">"]
        for char in invalid_chars:
            with pytest.raises(HTTPException) as exc_info:
                validate_format_id(f"137{char}")
            assert exc_info.value.status_code == 400

    def test_strips_whitespace(self):
        """Whitespace is stripped and remaining string validated"""
        # Trailing whitespace is stripped
        assert validate_format_id("137\n") == "137"
        assert validate_format_id("137 ") == "137"
        assert validate_format_id(" 137") == "137"
        assert validate_format_id(" 137 ") == "137"

        # But whitespace in the middle is rejected
        with pytest.raises(HTTPException):
            validate_format_id("137 456")

    def test_empty_format_id(self):
        """Reject empty format_id"""
        with pytest.raises(HTTPException) as exc_info:
            validate_format_id("")
        assert exc_info.value.status_code == 400

    def test_none_format_id(self):
        """Reject None format_id"""
        with pytest.raises(HTTPException) as exc_info:
            validate_format_id(None)
        assert exc_info.value.status_code == 400

    def test_command_injection_attempt(self):
        """Reject command injection attempts"""
        malicious_ids = [
            "137; rm -rf /",
            "137`whoami`",
            "137$(ls)",
            "137 && cat /etc/passwd",
        ]
        for format_id in malicious_ids:
            with pytest.raises(HTTPException):
                validate_format_id(format_id)
