@echo off
rem Launcher for the Firefox native messaging host.
rem install_native_host.py rewrites this file with the absolute path of the Python that has the
rem dependencies installed; this fallback uses whatever python is first on PATH.
python "%~dp0native_messaging.py" %*
