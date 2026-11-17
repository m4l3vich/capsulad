#!/usr/bin/env python3
"""Extract the individual ALAC frames (access units) from an MP4/M4A produced by
ffmpeg into a small length-prefixed container, so that the Go decoder can be fed
exactly one RTP-payload-sized ALAC frame at a time.

    gen_frames.py <input.m4a> <output.frames.bin> <label>

Container written (all integers big-endian):

    magic    8 bytes  "ALACFRMS"
    version  uint32   = 1
    count    uint32   number of frames
    cookielen uint32  length of the ALAC magic cookie that follows
    cookie   cookielen bytes   (the 36-byte 'alac' box payload from stsd,
                                i.e. size/tag/version + the 24-byte
                                ALACSpecificConfig)
    frames   count * (uint32 length, length bytes of ALAC frame)

The MP4 boxes are parsed with the python standard library only: the sample entry
in stsd gives the codec magic cookie, stsz gives every sample size and
stsc/stco (or co64) give the file offset of every sample, so a sample is a
contiguous run of bytes in mdat. ffmpeg's MP4 muxer stores exactly one ALAC
frame per sample.
"""

import struct
import sys


class Box:
    def __init__(self, typ, start, hdr, size):
        self.type = typ
        self.start = start          # offset of the box header
        self.payload = start + hdr  # offset of the box payload
        self.end = start + size


def parse_boxes(data, start, end):
    """Yield the boxes in data[start:end]."""
    off = start
    while off + 8 <= end:
        size = struct.unpack(">I", data[off:off + 4])[0]
        typ = data[off + 4:off + 8].decode("latin1")
        hdr = 8
        if size == 1:
            size = struct.unpack(">Q", data[off + 8:off + 16])[0]
            hdr = 16
        elif size == 0:
            size = end - off
        if size < hdr or off + size > end:
            raise ValueError("bad box %r at %d (size %d)" % (typ, off, size))
        yield Box(typ, off, hdr, size)
        off += size


def find_all(data, start, end, path):
    """Find every box matching a path of box types."""
    if not path:
        return [(start, end)]
    out = []
    for box in parse_boxes(data, start, end):
        if box.type == path[0]:
            out.extend(find_all(data, box.payload, box.end, path[1:]))
    return out


def find(data, start, end, path):
    hits = find_all(data, start, end, path)
    if not hits:
        raise ValueError("box %s not found" % "/".join(path))
    return hits[0]


def parse_stsd_alac(data, stsd_start, stsd_end):
    """Return the payload of the 'alac' box inside the first sample entry."""
    # version/flags (4) + entry count (4) come before the sample entries.
    entries = list(parse_boxes(data, stsd_start + 8, stsd_end))
    if not entries:
        raise ValueError("empty stsd")
    entry = entries[0]
    # AudioSampleEntry: 6 reserved bytes + data_reference_index (2), then
    # version(2) revision(2) vendor(4) channels(2) sampleSize(2)
    # compressionId(2) packetSize(2) sampleRate(4) = 20 bytes (version 0),
    # plus 16 (version 1) or 36 (version 2) extension bytes.
    version = struct.unpack(">H", data[entry.payload + 8:entry.payload + 10])[0]
    children = entry.payload + 8 + 20
    if version == 1:
        children += 16
    elif version == 2:
        children += 36
    for box in parse_boxes(data, children, entry.end):
        if box.type == "alac":
            return data[box.payload:box.end]
    raise ValueError("no alac box in sample entry %r" % entry.type)


def sample_offsets(data, moov):
    stbl_start, stbl_end = find(data, moov[0], moov[1], ["trak", "mdia", "minf", "stbl"])

    stsz_start, stsz_end = find(data, stbl_start, stbl_end, ["stsz"])
    count = struct.unpack(">I", data[stsz_start + 8:stsz_start + 12])[0]
    uniform = struct.unpack(">I", data[stsz_start + 4:stsz_start + 8])[0]
    if uniform:
        sizes = [uniform] * count
    else:
        sizes = list(struct.unpack(">%dI" % count, data[stsz_start + 12:stsz_start + 12 + 4 * count]))

    stsc_start, _ = find(data, stbl_start, stbl_end, ["stsc"])
    stsc_count = struct.unpack(">I", data[stsc_start + 4:stsc_start + 8])[0]
    stsc = []
    for i in range(stsc_count):
        first, per_chunk, _desc = struct.unpack(
            ">III", data[stsc_start + 8 + 12 * i:stsc_start + 20 + 12 * i])
        stsc.append((first, per_chunk))

    try:
        stco_start, _ = find(data, stbl_start, stbl_end, ["stco"])
    except ValueError:
        stco_start, _ = find(data, stbl_start, stbl_end, ["co64"])
        chunk_count = struct.unpack(">I", data[stco_start + 4:stco_start + 8])[0]
        chunks = list(struct.unpack(">%dQ" % chunk_count,
                                    data[stco_start + 8:stco_start + 8 + 8 * chunk_count]))
    else:
        chunk_count = struct.unpack(">I", data[stco_start + 4:stco_start + 8])[0]
        chunks = list(struct.unpack(">%dI" % chunk_count,
                                    data[stco_start + 8:stco_start + 8 + 4 * chunk_count]))

    offsets = []
    sample = 0
    for chunk_index, chunk_offset in enumerate(chunks):
        # samples per chunk for this chunk, from the stsc run-length table.
        per_chunk = 0
        for i, (first, n) in enumerate(stsc):
            if chunk_index + 1 >= first:
                if i + 1 == len(stsc) or chunk_index + 1 < stsc[i + 1][0]:
                    per_chunk = n
                    break
        pos = chunk_offset
        for _ in range(per_chunk):
            if sample >= count:
                break
            offsets.append((pos, sizes[sample]))
            pos += sizes[sample]
            sample += 1
    if sample != count:
        raise ValueError("located %d of %d samples" % (sample, count))
    return offsets


def main():
    if len(sys.argv) != 4:
        sys.stderr.write(__doc__)
        return 2
    src, dst, label = sys.argv[1:4]
    with open(src, "rb") as fh:
        data = fh.read()

    moov = find(data, 0, len(data), ["moov"])
    stsd = find(data, moov[0], moov[1], ["trak", "mdia", "minf", "stbl", "stsd"])
    cookie = parse_stsd_alac(data, stsd[0], stsd[1])
    offsets = sample_offsets(data, moov)

    out = bytearray()
    out += b"ALACFRMS"
    out += struct.pack(">III", 1, len(offsets), len(cookie))
    out += cookie
    for pos, size in offsets:
        out += struct.pack(">I", size)
        out += data[pos:pos + size]
    with open(dst, "wb") as fh:
        fh.write(out)

    base = {24: 0, 28: 4, 36: 12}[len(cookie)]
    frame_length = struct.unpack(">I", cookie[base:base + 4])[0]
    bit_depth = cookie[base + 5]
    channels = cookie[base + 9]
    sample_rate = struct.unpack(">I", cookie[base + 20:base + 24])[0]
    print("%-32s cookie=%d bytes %s" % ("", len(cookie), cookie.hex()))
    print("%-32s frameLength=%d bitDepth=%d channels=%d rate=%d"
          % (label, frame_length, bit_depth, channels, sample_rate))
    print("%-32s %d frames sizes=%s" % ("", len(offsets), [s for _, s in offsets]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
