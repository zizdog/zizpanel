#!/bin/bash
# Generate small test videos for govideo. Writes only into the target directory.
set -euo pipefail

FFMPEG="${FFMPEG:-/opt/homebrew/bin/ffmpeg}"
OUT="${1:-fixtures}"

if [ ! -x "$FFMPEG" ]; then
  echo "找不到 ffmpeg: $FFMPEG" >&2
  exit 1
fi

mkdir -p "$OUT"
cd "$OUT"

echo "== 1/3 h264 + aac =="
"$FFMPEG" -hide_banner -loglevel error -y \
  -f lavfi -i "testsrc=duration=5:size=640x360:rate=30" \
  -f lavfi -i "sine=frequency=440:duration=5" \
  -c:v libx264 -pix_fmt yuv420p -profile:v baseline -level 3.1 \
  -c:a aac -b:a 96k -movflags +faststart \
  h264_aac.mp4

echo "== 2/3 h264 无音轨 =="
"$FFMPEG" -hide_banner -loglevel error -y \
  -f lavfi -i "testsrc=duration=4:size=640x360:rate=30" \
  -c:v libx264 -pix_fmt yuv420p -an -movflags +faststart \
  h264_videoonly.mp4

echo "== 3/3 hevc（可能编不出来，如实报告）=="
if "$FFMPEG" -hide_banner -loglevel error -y \
    -f lavfi -i "testsrc=duration=4:size=640x360:rate=30" \
    -f lavfi -i "sine=frequency=660:duration=4" \
    -c:v libx265 -x265-params log-level=error -pix_fmt yuv420p \
    -c:a aac -b:a 96k -tag:v hvc1 -movflags +faststart \
    hevc_aac.mp4 2>/tmp/govideo-hevc.log; then
  echo "  hevc_aac.mp4 生成成功"
else
  echo "  hevc 编码失败（本机 ffmpeg 无 libx265 可用）："
  sed 's/^/  /' /tmp/govideo-hevc.log || true
  rm -f hevc_aac.mp4
fi

echo
echo "== 结果 =="
shasum -a 256 ./*.mp4
echo
for f in ./*.mp4; do
  echo "--- $f"
  "${FFPROBE:-/opt/homebrew/bin/ffprobe}" -v error \
    -show_entries stream=codec_type,codec_name,width,height \
    -show_entries format=duration -of default=noprint_wrappers=1 "$f"
done
