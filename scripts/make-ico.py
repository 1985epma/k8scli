#!/usr/bin/env python3
"""Pack PNG files into a Windows .ico (PNG-compressed entries, standard library only).

usage: make-ico.py OUTPUT.ico 16.png 24.png ... 256.png
"""
import struct
import sys


def png_size(data):
    if data[:8] != b"\x89PNG\r\n\x1a\n":
        raise SystemExit("not a PNG file")
    width, height = struct.unpack(">II", data[16:24])
    return width, height


def main():
    if len(sys.argv) < 3:
        raise SystemExit(__doc__)
    output, sources = sys.argv[1], sys.argv[2:]

    images = []
    for source in sources:
        with open(source, "rb") as handle:
            data = handle.read()
        width, height = png_size(data)
        if width > 256 or height > 256:
            raise SystemExit(f"{source}: an ICO entry is at most 256x256")
        images.append((width, height, data))
    images.sort(key=lambda item: item[0])

    header = struct.pack("<HHH", 0, 1, len(images))
    offset = 6 + 16 * len(images)
    entries, payload = b"", b""
    for width, height, data in images:
        # 0 stands for 256 in the width and height bytes.
        entries += struct.pack("<BBBBHHII", width % 256, height % 256, 0, 0, 1, 32, len(data), offset)
        payload += data
        offset += len(data)

    with open(output, "wb") as handle:
        handle.write(header + entries + payload)


if __name__ == "__main__":
    main()
