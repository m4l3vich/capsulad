package capsulad

import (
	"fmt"
	"sort"
)

// segmentCell and the frame envelopes mirror the framework's protobuf JSON
// shape. They are unexported because callers use Presentation methods.
type segmentCell struct {
	Mask uint32 `json:"mask,omitempty"`
}

type segmentFrame struct {
	Cells []segmentCell `json:"cells,omitempty"`
	Colon bool          `json:"colon,omitempty"`
}

type ringFrame struct {
	Pixels []RGB `json:"pixels,omitempty"`
}

type ringEffectFrame struct {
	EffectID  string           `json:"effectId,omitempty"`
	Arguments []PresetArgument `json:"arguments,omitempty"`
}

type segmentFaceFrame struct {
	FaceID    string           `json:"faceId,omitempty"`
	Arguments []PresetArgument `json:"arguments,omitempty"`
}

type blankFrame struct{}

type presentationFrame struct {
	Segment     *segmentFrame     `json:"segment,omitempty"`
	Ring        *ringFrame        `json:"ring,omitempty"`
	RingEffect  *ringEffectFrame  `json:"ringEffect,omitempty"`
	SegmentFace *segmentFaceFrame `json:"segmentFace,omitempty"`
	Blank       *blankFrame       `json:"blank,omitempty"`
}

// presentationRequest is the PresentationService/Open oneof envelope.
type presentationRequest struct {
	Open  *presentationOpenJSON `json:"open,omitempty"`
	Frame *presentationFrame    `json:"frame,omitempty"`
	Close *blankFrame           `json:"close,omitempty"`
}

// presetArgs converts the public parameter map into sorted framework preset
// arguments. Sorting keeps frames deterministic, matching the previous SDK.
func presetArgs(params map[string]any) ([]PresetArgument, error) {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]PresetArgument, 0, len(keys))
	for _, name := range keys {
		value, err := presetValue(params[name])
		if err != nil {
			return nil, fmt.Errorf("preset argument %q: %w", name, err)
		}
		out = append(out, PresetArgument{Name: name, Value: value})
	}
	return out, nil
}

func presetValue(v any) (*PresetValue, error) {
	switch value := v.(type) {
	case string:
		return &PresetValue{Text: &value}, nil
	case bool:
		return &PresetValue{Boolean: &value}, nil
	case int:
		n := int64(value)
		return &PresetValue{Integer: &n}, nil
	case int64:
		return &PresetValue{Integer: &value}, nil
	case float64:
		return &PresetValue{Number: &value}, nil
	case float32:
		n := float64(value)
		return &PresetValue{Number: &n}, nil
	case RGB:
		color := value
		return &PresetValue{Color: &color}, nil
	default:
		return nil, fmt.Errorf("unsupported type %T (want string, bool, int, int64, float32, float64 or RGB)", v)
	}
}
