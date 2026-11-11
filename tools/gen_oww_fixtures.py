#!/usr/bin/env python3
"""Generate reference vectors for the Go wakeword frontend tests.

Uses ONNX Runtime with the Apache-2.0 openWakeWord assets to produce, for one
deterministic signal, the mel frames (dB/10+2, as the reference pipeline applies)
and one 96-dim speech embedding. The Go implementation must match these.
"""
import json
import os
import sys

import numpy as np
import onnxruntime as ort

OUT = sys.argv[1] if len(sys.argv) > 1 else "internal/wakeword/oww/testdata"
MODELS = sys.argv[2] if len(sys.argv) > 2 else "/tmp/oww"

rng = np.random.default_rng(20261110)
n = 16000
t = np.arange(n) / 16000.0
signal = (0.25 * np.sin(2 * np.pi * 440 * t) + 0.12 * np.sin(2 * np.pi * 1330 * t)
          + 0.05 * rng.standard_normal(n)).astype(np.float32)

mel_sess = ort.InferenceSession(os.path.join(MODELS, "melspectrogram.onnx"), providers=["CPUExecutionProvider"])
mel = mel_sess.run(None, {"input": signal[None, :]})[0]   # (1,1,T,32) dB
mel = np.squeeze(mel, axis=1).squeeze(axis=0)             # (T,32)
transformed = mel / 10.0 + 2.0                            # reference melspec_transform

emb_sess = ort.InferenceSession(os.path.join(MODELS, "embedding_model.onnx"), providers=["CPUExecutionProvider"])
window = transformed[8:8 + 76].astype(np.float32)
emb = emb_sess.run(None, {"input_1": window[None, :, :, None].astype(np.float32)})[0]
emb = np.squeeze(emb)

os.makedirs(OUT, exist_ok=True)
signal.astype(np.float32).tofile(os.path.join(OUT, "signal.bin"))
# Second fixture: the device path. 2 s of 16-bit PCM at a realistic level, fed to
# the same models as raw int16 values (the reference casts PCM to float without
# dividing by 32768).
n2 = 32000
t2 = np.arange(n2) / 16000.0
pcm = (6000 * np.sin(2 * np.pi * 300 * t2) + 2000 * np.sin(2 * np.pi * 1900 * t2)
       + 400 * rng.standard_normal(n2)).astype(np.int16)
mel2 = mel_sess.run(None, {"input": pcm.astype(np.float32)[None, :]})[0]
mel2 = np.squeeze(mel2, axis=1).squeeze(0) / 10.0 + 2.0
windows2 = [mel2[i:i + 76] for i in range(0, len(mel2), 8) if mel2[i:i + 76].shape[0] == 76]
batch = np.stack(windows2).astype(np.float32)[:, :, :, None]
embs2 = emb_sess.run(None, {"input_1": batch})[0].reshape(len(windows2), -1)
pcm.tofile(os.path.join(OUT, "pcm16.bin"))

json.dump({
    "signal_samples": int(n),
    "mel_frames": int(transformed.shape[0]),
    "mel": [round(float(x), 6) for x in transformed[:32].reshape(-1)],
    "mel_shape": [32, 32],
    "window_start": 8,
    "embedding": [round(float(x), 6) for x in emb.reshape(-1)],
    "pcm16_samples": int(n2),
    "pcm16_embeddings": int(len(windows2)),
    "pcm16_embedding0": [round(float(x), 6) for x in embs2[0]],
}, open(os.path.join(OUT, "reference.json"), "w"), indent=1)
print("mel frames", transformed.shape, "embedding", emb.shape)
