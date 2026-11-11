#!/usr/bin/env python3
"""Export the Apache-2.0 openWakeWord frontend/embedding weights for the Go engine.

The engine does not vendor any model weights from projects whose licences forbid
redistribution. This script takes the two Apache-2.0 ONNX assets published by
openWakeWord (melspectrogram and the Google speech embedding), verifies their
digests, and writes a flat float32 blob plus a JSON manifest that the Go loader
consumes. Run it from the repository root:

    python3 tools/export_oww.py --models /tmp/oww --out internal/wakeword/oww

Requires: numpy, onnx (pip install numpy onnx).
"""
import argparse
import hashlib
import json
import os
import sys

import numpy as np
import onnx
from onnx import numpy_helper

# Digests of the upstream assets this exporter was validated against.
EXPECTED = {
    "melspectrogram.onnx": "ba2b0e0f8b7b875369a2c89cb13360ff53bac436f2895cced9f479fa65eb176f",
    "embedding_model.onnx": "70d164290c1d095d1d4ee149bc5e00543250a7316b59f31d056cff7bd3075c1f",
}


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def tensor_index(doc):
    return {i.name: numpy_helper.to_array(i) for i in doc.graph.initializer}


def conv_layers(doc):
    """Return the conv/pool/activation sequence with the tensors it needs."""
    inits = tensor_index(doc)
    layers = []
    tensors = []
    seen_bias = {}

    def add_tensor(name, array):
        tensors.append({"name": name, "shape": list(array.shape), "data": array.astype(np.float32)})
        return name

    for node in doc.graph.node:
        if node.op_type == "Conv":
            weights = inits[node.input[1]]
            out_ch, in_ch, kh, kw = weights.shape
            pads = [0, 0, 0, 0]
            for attr in node.attribute:
                if attr.name == "pads":
                    pads = list(attr.ints)
            bias = None
            if len(node.input) > 2:
                bias = inits[node.input[2]]
            name = f"conv{len(layers)}"
            add_tensor(name + ".w", weights)
            entry = {
                "op": "conv", "w": name + ".w",
                "kh": kh, "kw": kw, "in": in_ch, "out": out_ch,
                "pt": pads[0], "pl": pads[1], "pb": pads[2], "pr": pads[3],
            }
            if bias is not None:
                add_tensor(name + ".b", bias)
                entry["b"] = name + ".b"
            layers.append(entry)
        elif node.op_type == "LeakyRelu":
            alpha = 0.2
            for attr in node.attribute:
                if attr.name == "alpha":
                    alpha = float(attr.f)
            layers.append({"op": "leakyrelu", "alpha": alpha})
        elif node.op_type == "Max":
            const = inits[node.input[1]].reshape(-1)[0]
            layers.append({"op": "max", "value": float(const)})
        elif node.op_type == "MaxPool":
            kernel = [1, 1]
            strides = [1, 1]
            for attr in node.attribute:
                if attr.name == "kernel_shape":
                    kernel = list(attr.ints)
                if attr.name == "strides":
                    strides = list(attr.ints)
            layers.append({"op": "maxpool", "kh": kernel[0], "kw": kernel[1],
                           "sh": strides[0], "sw": strides[1]})
        elif node.op_type in ("Reshape",):
            continue
        else:
            raise SystemExit(f"unsupported embedding op {node.op_type}")
    return layers, tensors


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--models", required=True, help="directory with the two ONNX files")
    ap.add_argument("--out", required=True, help="output directory for oww.bin/oww.json")
    args = ap.parse_args()

    paths = {name: os.path.join(args.models, name) for name in EXPECTED}
    for name, path in paths.items():
        if not os.path.exists(path):
            sys.exit(f"missing {path}")
        got = sha256(path)
        if got != EXPECTED[name]:
            sys.exit(f"{name}: digest {got} does not match the validated {EXPECTED[name]}")

    mel_doc = onnx.load(paths["melspectrogram.onnx"])
    mel_inits = tensor_index(mel_doc)
    mel_w = mel_inits["1.melW"]
    if mel_w.shape != (257, 32):
        sys.exit(f"unexpected mel filterbank shape {mel_w.shape}")

    emb_doc = onnx.load(paths["embedding_model.onnx"])
    layers, tensors = conv_layers(emb_doc)

    tensors.insert(0, {"name": "melW", "shape": list(mel_w.shape), "data": mel_w.astype(np.float32)})

    blob = bytearray()
    manifest = {"version": 1, "source": {n: sha256(p) for n, p in paths.items()}, "tensors": [], "layers": layers}
    for t in tensors:
        data = t["data"].reshape(-1).astype(np.float32)
        manifest["tensors"].append({
            "name": t["name"], "shape": t["shape"],
            "offset": len(blob) // 4, "count": int(data.size),
        })
        blob += data.tobytes()

    os.makedirs(args.out, exist_ok=True)
    with open(os.path.join(args.out, "oww.bin"), "wb") as f:
        f.write(blob)
    with open(os.path.join(args.out, "oww.json"), "w") as f:
        json.dump(manifest, f, indent=1, sort_keys=True)
    print(f"wrote {len(blob)} bytes of weights, {len(layers)} layers, {len(tensors)} tensors")


if __name__ == "__main__":
    main()
