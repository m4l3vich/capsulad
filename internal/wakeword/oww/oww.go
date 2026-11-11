// Package oww implements the openWakeWord feature frontend and speech-embedding
// network in pure Go.
//
// The weights are derived from the Apache-2.0 assets published by openWakeWord
// (the melspectrogram model and Google's speech-embedding network, itself
// Apache-2.0) and are exported by tools/export_oww.py. No CC BY-NC-SA asset is
// bundled: the pre-trained wake-word models and the negative-features dataset are
// not redistributable here.
//
// Pipeline: 16 kHz mono → 512-point STFT (Hann-400 centred, hop 160) → 32-bin mel
// → 10·log10 → /10+2 → 76-frame windows stepped by 8 frames → 96-dim embeddings.
// Frontend executes the network incrementally: every layer keeps a sliding run
// of activations and only evaluates the positions a new window adds, while
// Embedder retains the from-scratch path for parity tests and offline export.
// A keyword classifier consumes the embedding stream; it is not part of this
// package.
package oww

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed oww.bin
var weightsBlob []byte

//go:embed oww.json
var manifestJSON []byte

// Manifest describes the exported tensors and the layer sequence.
type Manifest struct {
	Version int               `json:"version"`
	Source  map[string]string `json:"source"`
	Tensors []TensorInfo      `json:"tensors"`
	Layers  []map[string]any  `json:"layers"`
}

type TensorInfo struct {
	Name   string `json:"name"`
	Shape  []int  `json:"shape"`
	Offset int    `json:"offset"` // in float32 units
	Count  int    `json:"count"`
}

// Assets is the loaded weight set.
type Assets struct {
	Manifest Manifest
	tensors  map[string][]float32
}

var loaded *Assets

// Load decodes the embedded weights once.
func Load() (*Assets, error) {
	if loaded != nil {
		return loaded, nil
	}
	var m Manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return nil, fmt.Errorf("oww: manifest: %w", err)
	}
	if m.Version != 1 {
		return nil, fmt.Errorf("oww: unsupported asset version %d", m.Version)
	}
	a := &Assets{Manifest: m, tensors: make(map[string][]float32, len(m.Tensors))}
	for _, t := range m.Tensors {
		end := (t.Offset + t.Count) * 4
		if t.Offset < 0 || end > len(weightsBlob) || end < 0 {
			return nil, fmt.Errorf("oww: tensor %s out of range", t.Name)
		}
		raw := weightsBlob[t.Offset*4 : end]
		values := make([]float32, t.Count)
		for i := 0; i < t.Count; i++ {
			bits := uint32(raw[i*4]) | uint32(raw[i*4+1])<<8 | uint32(raw[i*4+2])<<16 | uint32(raw[i*4+3])<<24
			values[i] = mathFromBits(bits)
		}
		a.tensors[t.Name] = values
	}
	if _, ok := a.tensors["melW"]; !ok {
		return nil, fmt.Errorf("oww: mel filterbank missing")
	}
	loaded = a
	return a, nil
}

// Tensor returns a named float32 tensor.
func (a *Assets) Tensor(name string) ([]float32, bool) {
	v, ok := a.tensors[name]
	return v, ok
}
