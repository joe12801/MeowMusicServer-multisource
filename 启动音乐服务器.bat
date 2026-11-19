@echo off
chcp 65001 >nul
title 小智AI 音乐服务器 (MeowMusicServer)
set PORT=2233
set PATH=D:\ffmpeg-master-latest-win64-gpl\bin;%PATH%
cd /d E:\dev\meow
echo ============================================
echo  音乐服务器启动中...  端口 2233
echo  设备访问地址: http://192.168.2.200:2233
echo  关闭本窗口即停止服务
echo ============================================
MeowMusicServer.exe
pause
