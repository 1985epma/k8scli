@echo off
rem Opens a prompt where k8scli works, even before the PATH change reaches already running programs.
set "PATH=%~dp0;%PATH%"
title k8scli
k8scli help
echo.
echo Type k8scli ^<command^>. Run k8scli-ui to open the web console.
cmd /k
