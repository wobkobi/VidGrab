#!/usr/bin/env python3
"""
Quick test script to verify the backend is running
"""
import requests

try:
    response = requests.get("http://127.0.0.1:18956/health", timeout=2)
    print(f"Status Code: {response.status_code}")
    print(f"Response: {response.json()}")
    print("\n✓ Backend is running correctly!")
except requests.exceptions.ConnectionError:
    print("✗ Backend is not running. Start it with: python backend/server.py")
except Exception as e:
    print(f"✗ Error: {e}")
