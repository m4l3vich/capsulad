package linuxdev

import (
	"math"

	"capsulad/internal/backend"
)

// WS2812ChannelMap maps a logical WS2812B channel (R,G,B per pixel) to the
// physical chain channel. Both supported stock firmwares embed this permutation
// in their voice-agent: the RK3308 rev 4 image and the Amlogic A113X Mini image
// use the same 36-channel wiring, so the table is board data that happens to be
// shared. It reverses the ring order and swaps R and G within each pixel,
// matching WS2812B GRB wire order.
var WS2812ChannelMap = [...]int{
	1, 0, 2, 34, 33, 35, 31, 30, 32, 28, 27, 29,
	25, 24, 26, 22, 21, 23, 19, 18, 20, 16, 15, 17,
	13, 12, 14, 10, 9, 11, 7, 6, 8, 4, 3, 5,
}

// BuildGammaTable precomputes the perceptual curve applied to ring output.
func BuildGammaTable(gamma float64) (table [256]uint8) {
	for i := range table {
		table[i] = uint8(math.Round(255 * math.Pow(float64(i)/255, gamma)))
	}
	return table
}

// ClampBrightness bounds a resolved frame brightness to [0,1]. A NaN or a
// negative level blanks the frame.
func ClampBrightness(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ScaleChannel applies a linear brightness multiplier to one 8-bit channel.
func ScaleChannel(v uint8, brightness float64) uint8 {
	return uint8(math.Round(float64(v) * brightness))
}

// EncodeWS2812Frame expands RGB pixels into the controller's channel buffer,
// applying brightness in linear light and then the perceptual curve once per
// channel, and permutes the result through channelMap. Missing pixels are
// black. A controller whose channel count does not match the map is driven in
// logical order rather than with the wrong permutation.
func EncodeWS2812Frame(pixels []backend.RGB, brightness float64, channels int, channelMap []int, gamma [256]uint8) []byte {
	brightness = ClampBrightness(brightness)
	logical := make([]byte, channels)
	pixels = pixels[:min(len(pixels), channels/3)]
	for pixel := range pixels {
		px := pixels[pixel]
		logical[pixel*3] = gamma[ScaleChannel(px.R, brightness)]
		logical[pixel*3+1] = gamma[ScaleChannel(px.G, brightness)]
		logical[pixel*3+2] = gamma[ScaleChannel(px.B, brightness)]
	}
	out := make([]byte, channels)
	if channels == len(channelMap) {
		for i, target := range channelMap {
			out[target] = logical[i]
		}
		return out
	}
	copy(out, logical)
	return out
}
