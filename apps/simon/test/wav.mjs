// Test-only RIFF/WAVE reader for the packaged Simon cues.
//
// The app validates its own container; this parser intentionally lives in the
// contract tests so a broken file is caught independently of that code.

import { readFileSync } from "node:fs";

export const TONE_FILES = [
  { name: "red-long.wav", path: "audio/red-long.wav", frequency: 392, frames: 7680 },
  { name: "red-short.wav", path: "audio/red-short.wav", frequency: 392, frames: 4320 },
  { name: "yellow-long.wav", path: "audio/yellow-long.wav", frequency: 523, frames: 7680 },
  { name: "yellow-short.wav", path: "audio/yellow-short.wav", frequency: 523, frames: 4320 },
  { name: "green-long.wav", path: "audio/green-long.wav", frequency: 659, frames: 7680 },
  { name: "green-short.wav", path: "audio/green-short.wav", frequency: 659, frames: 4320 },
  { name: "blue-long.wav", path: "audio/blue-long.wav", frequency: 784, frames: 7680 },
  { name: "blue-short.wav", path: "audio/blue-short.wav", frequency: 784, frames: 4320 },
  { name: "error.wav", path: "audio/error.wav", frequency: null, frames: 14400 },
];

const ASCII = (bytes, offset, length) =>
  String.fromCharCode(...bytes.subarray(offset, offset + length));

export function parseWav(bytes) {
  if (bytes.length < 12 || ASCII(bytes, 0, 4) !== "RIFF" || ASCII(bytes, 8, 4) !== "WAVE") {
    throw new Error("not a RIFF/WAVE file");
  }
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  let format = null;
  let data = null;
  let offset = 12;
  while (offset + 8 <= bytes.length) {
    const id = ASCII(bytes, offset, 4);
    const size = view.getUint32(offset + 4, true);
    const body = offset + 8;
    if (body + size > bytes.length) throw new Error("truncated " + id + " chunk");
    if (id === "fmt ") {
      format = {
        encoding: view.getUint16(body, true),
        channels: view.getUint16(body + 2, true),
        rate: view.getUint32(body + 4, true),
        byteRate: view.getUint32(body + 8, true),
        blockAlign: view.getUint16(body + 12, true),
        bits: view.getUint16(body + 14, true),
      };
    } else if (id === "data") {
      data = bytes.subarray(body, body + size);
    }
    offset = body + size + (size & 1);
  }
  if (!format) throw new Error("no fmt chunk");
  if (!data) throw new Error("no data chunk");
  if (format.encoding !== 1 || format.channels !== 1 || format.rate !== 24000 ||
      format.bits !== 16 || format.blockAlign !== 2 || format.byteRate !== 48000) {
    throw new Error("unexpected WAV format " + JSON.stringify(format));
  }
  if (data.length % (format.channels * 2) !== 0) {
    throw new Error("data chunk is not frame aligned");
  }
  return { format, pcm: data, frames: data.length / (format.channels * 2) };
}

export function readWav(path) {
  return parseWav(new Uint8Array(readFileSync(path)));
}
