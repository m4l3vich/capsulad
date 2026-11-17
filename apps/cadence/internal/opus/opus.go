// Package opus is the Cadence device link's Opus codec: it encodes captured
// 16 kHz mono PCM into the 20 ms uplink packets the server's speech recognizer
// consumes, and decodes the server's 20 ms reply packets to 48 kHz PCM for the
// framework's playback endpoint.
//
// It binds libopus at run time with dlopen, the same way libcapsulad loads its
// own engine: the app has no link-time dependency on the codec, and a device
// that does not carry libopus fails the first turn with one actionable error
// instead of a loader failure. The encoder parameters mirror
// cadence-audiod's OpusEncoder (16 kHz mono, 20 ms, 24 kbps, voip, complexity
// 5, no DTX or in-band FEC); the decoder mirrors its OpusDecoder (48 kHz).
package opus

/*
#cgo linux LDFLAGS: -ldl

#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

// Opaque libopus handles and function-pointer types. Declaring them here means
// the build needs no opus headers; the codec is resolved from the shared
// object at run time.
typedef struct OpusEncoder OpusEncoder;
typedef struct OpusDecoder OpusDecoder;

typedef OpusEncoder *(*create_encoder_fn)(int32_t rate, int channels, int application, int *error);
typedef int (*encode_fn)(OpusEncoder *st, const short *pcm, int frame_size, unsigned char *data, int max_bytes);
typedef void (*destroy_encoder_fn)(OpusEncoder *st);
// opus_encoder_ctl is variadic. Apple's ARM64 ABI passes variadic arguments on
// the stack, so the pointer must be variadic too; a non-variadic pointer would
// pass the value in a register and the callee would read garbage.
typedef int (*encoder_ctl_fn)(OpusEncoder *st, int request, ...);
typedef OpusDecoder *(*create_decoder_fn)(int32_t rate, int channels, int *error);
typedef int (*decode_fn)(OpusDecoder *st, const unsigned char *data, int len, short *pcm, int frame_size, int fec);
typedef void (*destroy_decoder_fn)(OpusDecoder *st);

static create_encoder_fn p_create_encoder;
static encode_fn p_encode;
static destroy_encoder_fn p_destroy_encoder;
static encoder_ctl_fn p_encoder_ctl;
static create_decoder_fn p_create_decoder;
static decode_fn p_decode;
static destroy_decoder_fn p_destroy_decoder;
static void *g_handle;
static char g_error[256];

// The opus constants used here, from libopus's public header. They are ABI
// constants, not implementation details.
enum {
  CAP_OPUS_OK = 0,
  CAP_OPUS_APPLICATION_VOIP = 2048,
  CAP_OPUS_SET_BITRATE = 4002,
  CAP_OPUS_SET_COMPLEXITY = 4010
};

static void cap_opus_set_error(const char *text) {
  if (text == NULL) text = "";
  strncpy(g_error, text, sizeof(g_error) - 1);
  g_error[sizeof(g_error) - 1] = 0;
}

// cap_opus_load tries each candidate soname in order. It returns 0 on success
// and -1 with cap_opus_error text otherwise. Loading is idempotent.
static int cap_opus_load(const char **candidates, int count) {
  if (g_handle != NULL) return 0;
  for (int i = 0; i < count; i++) {
    void *h = dlopen(candidates[i], RTLD_NOW | RTLD_LOCAL);
    if (h == NULL) continue;
    p_create_encoder = (create_encoder_fn)dlsym(h, "opus_encoder_create");
    p_encode = (encode_fn)dlsym(h, "opus_encode");
    p_destroy_encoder = (destroy_encoder_fn)dlsym(h, "opus_encoder_destroy");
    p_encoder_ctl = (encoder_ctl_fn)dlsym(h, "opus_encoder_ctl");
    p_create_decoder = (create_decoder_fn)dlsym(h, "opus_decoder_create");
    p_decode = (decode_fn)dlsym(h, "opus_decode");
    p_destroy_decoder = (destroy_decoder_fn)dlsym(h, "opus_decoder_destroy");
    if (!p_create_encoder || !p_encode || !p_destroy_encoder || !p_encoder_ctl ||
        !p_create_decoder || !p_decode || !p_destroy_decoder) {
      cap_opus_set_error("libopus is missing an encoder or decoder symbol");
      dlclose(h);
      p_create_encoder = NULL;
      return -1;
    }
    g_handle = h;
    return 0;
  }
  cap_opus_set_error("libopus was not found (tried the configured sonames)");
  return -1;
}

static const char *cap_opus_error(void) { return g_error; }

// cap_opus_encoder_open returns 0 on success, else a stage code: -1 no library,
// -2 create returned null, -3 create error (g_create_error), -4 bitrate, -5
// complexity. The stage is a fixed number, never library text.
static int g_create_error;
static int cap_opus_encoder_open(void **out, int32_t rate, int channels, int32_t bitrate, int complexity) {
  if (g_handle == NULL) return -1;
  int err = CAP_OPUS_OK;
  OpusEncoder *st = p_create_encoder(rate, channels, CAP_OPUS_APPLICATION_VOIP, &err);
  if (st == NULL) return -2;
  if (err != CAP_OPUS_OK) { g_create_error = err; p_destroy_encoder(st); return -3; }
  if (p_encoder_ctl(st, CAP_OPUS_SET_BITRATE, (int)bitrate) != CAP_OPUS_OK) {
    p_destroy_encoder(st);
    return -4;
  }
  if (p_encoder_ctl(st, CAP_OPUS_SET_COMPLEXITY, (int)complexity) != CAP_OPUS_OK) {
    p_destroy_encoder(st);
    return -5;
  }
  *out = (void *)st;
  return 0;
}
static int cap_opus_create_error(void) { return g_create_error; }

static int cap_opus_encode(void *st, const short *pcm, int samples, unsigned char *data, int max_bytes) {
  if (st == NULL) return -1;
  return p_encode((OpusEncoder *)st, pcm, samples, data, max_bytes);
}

static void cap_opus_encoder_close(void *st) {
  if (st != NULL) p_destroy_encoder((OpusEncoder *)st);
}

static int cap_opus_decoder_open(void **out, int32_t rate, int channels) {
  if (g_handle == NULL) return -1;
  int err = CAP_OPUS_OK;
  OpusDecoder *st = p_create_decoder(rate, channels, &err);
  if (st == NULL || err != CAP_OPUS_OK) return -1;
  *out = (void *)st;
  return 0;
}

static int cap_opus_decode(void *st, const unsigned char *data, int len, short *pcm, int frame_size) {
  if (st == NULL) return -1;
  return p_decode((OpusDecoder *)st, data, len, pcm, frame_size, 0);
}

static void cap_opus_decoder_close(void *st) {
  if (st != NULL) p_destroy_decoder((OpusDecoder *)st);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"unsafe"
)

const (
	// SampleRateHz is the uplink rate: cadence-audiod encodes 16 kHz mono
	// frames, and the server's recognizer opens at the same 16 kHz.
	SampleRateHz = 16000
	// FrameMS is the uplink packet duration: 20 ms.
	FrameMS = 20
	// FrameSamples is one mono 20 ms frame at SampleRateHz.
	FrameSamples = SampleRateHz * FrameMS / 1000
	// Bitrate is the uplink bitrate the audiod encoder uses.
	Bitrate = 24000
	// Complexity is the audiod encoder's cpu-budget setting.
	Complexity = 5
	// DecodeSampleRateHz is the reply rate the device decoder produces;
	// libopus resamples the server's packets on decode.
	DecodeSampleRateHz = 48000
	// maxDecodePerChannel is 120 ms, the longest packet Opus defines.
	maxDecodePerChannel = DecodeSampleRateHz * 120 / 1000
	// maxPacketBytes is libopus's recommended bound for one 20 ms packet.
	maxPacketBytes = 4000
)

// LibraryEnv overrides the libopus soname or path, like CAPSULAD_LIBRARY does
// for the framework engine.
const LibraryEnv = "CAPSULAD_OPUS_LIBRARY"

// ErrUnavailable means libopus could not be loaded. It is returned once per
// process; the turn that hit it fails with this error rather than pretending.
var ErrUnavailable = errors.New("opus: libopus is unavailable")

var (
	loadOnce sync.Once
	loadErr  error
)

// load resolves libopus once. The candidate list is a loader search, not a
// guess: the device's stock rootfs carries libopus.so.0 in /usr/lib, and the
// two Homebrew prefixes cover a macOS build host. A deployment can name an
// exact soname with CAPSULAD_OPUS_LIBRARY.
func load() error {
	loadOnce.Do(func() {
		candidates := []string{"libopus.so.0", "libopus.so", "libopus.dylib", "libopus.0.dylib",
			"/usr/lib/aarch64-linux-gnu/libopus.so.0", "/opt/homebrew/lib/libopus.dylib",
			"/usr/local/lib/libopus.dylib"}
		if override := os.Getenv(LibraryEnv); override != "" {
			candidates = append([]string{override}, candidates...)
		}
		cs := make([]*C.char, len(candidates))
		for i, c := range candidates {
			cs[i] = C.CString(c)
			defer C.free(unsafe.Pointer(cs[i]))
		}
		if rc := C.cap_opus_load(&cs[0], C.int(len(cs))); rc != 0 {
			loadErr = fmt.Errorf("%w: %s", ErrUnavailable, C.GoString(C.cap_opus_error()))
		}
	})
	return loadErr
}

// Encoder encodes 16 kHz mono PCM one 20 ms frame at a time. It is not safe
// for concurrent use.
type Encoder struct {
	st *C.void // OpusEncoder*
	// samples and out are reused by every Encode; see the return contract.
	samples []C.short
	out     []byte
}

// NewEncoder opens an uplink encoder with the audiod parameters.
func NewEncoder() (*Encoder, error) {
	if err := load(); err != nil {
		return nil, err
	}
	var st unsafe.Pointer
	if rc := C.cap_opus_encoder_open(&st, C.int32_t(SampleRateHz), 1, C.int32_t(Bitrate), C.int32_t(Complexity)); rc != 0 {
		return nil, fmt.Errorf("opus: encoder create failed (stage %d, libopus error %d)", int(rc), int(C.cap_opus_create_error()))
	}
	e := &Encoder{st: (*C.void)(st), samples: make([]C.short, FrameSamples), out: make([]byte, maxPacketBytes)}
	runtime.SetFinalizer(e, func(e *Encoder) { e.Close() })
	return e, nil
}

// Encode returns one Opus packet from exactly FrameSamples int16 values.
// pcm is little-endian 16-bit mono; len(pcm) must be FrameSamples*2. The
// returned slice is reused by the next Encode: consume it before calling again
// (the turn marshals it into the envelope immediately).
func (e *Encoder) Encode(pcm []byte) ([]byte, error) {
	if e == nil || e.st == nil {
		return nil, errors.New("opus: encoder is closed")
	}
	if len(pcm) != FrameSamples*2 {
		return nil, fmt.Errorf("opus: %d bytes is not one %d-sample frame", len(pcm), FrameSamples)
	}
	for i := range FrameSamples {
		e.samples[i] = C.short(int16(uint16(pcm[2*i]) | uint16(pcm[2*i+1])<<8))
	}
	n := C.cap_opus_encode(unsafe.Pointer(e.st), &e.samples[0], FrameSamples,
		(*C.uchar)(unsafe.Pointer(&e.out[0])), maxPacketBytes)
	if n < 0 {
		return nil, errors.New("opus: encode failed")
	}
	return e.out[:int(n)], nil
}

// Close releases the encoder. It is idempotent.
func (e *Encoder) Close() {
	if e == nil || e.st == nil {
		return
	}
	C.cap_opus_encoder_close(unsafe.Pointer(e.st))
	e.st = nil
	runtime.SetFinalizer(e, nil)
}

// Decoder decodes one reply stream's Opus packets to 48 kHz PCM. A new stream
// needs a new decoder: its prediction state must not continue an older reply.
// It is not safe for concurrent use.
type Decoder struct {
	st       *C.void // OpusDecoder*
	channels int
	out      []C.short
}

// NewDecoder opens a reply decoder for the stream's channel count (1 or 2).
func NewDecoder(channels int) (*Decoder, error) {
	if channels != 1 && channels != 2 {
		return nil, fmt.Errorf("opus: %d channels is not 1 or 2", channels)
	}
	if err := load(); err != nil {
		return nil, err
	}
	var st unsafe.Pointer
	if C.cap_opus_decoder_open(&st, C.int32_t(DecodeSampleRateHz), C.int(channels)) != 0 {
		return nil, errors.New("opus: decoder create failed")
	}
	d := &Decoder{st: (*C.void)(st), channels: channels, out: make([]C.short, maxDecodePerChannel*channels)}
	runtime.SetFinalizer(d, func(d *Decoder) { d.Close() })
	return d, nil
}

// Decode returns the packet's PCM as little-endian 16-bit interleaved samples.
func (d *Decoder) Decode(packet []byte) ([]byte, error) {
	if d == nil || d.st == nil {
		return nil, errors.New("opus: decoder is closed")
	}
	if len(packet) == 0 {
		return nil, errors.New("opus: empty packet")
	}
	n := C.cap_opus_decode(unsafe.Pointer(d.st), (*C.uchar)(unsafe.Pointer(&packet[0])), C.int(len(packet)),
		&d.out[0], maxDecodePerChannel)
	if n < 0 {
		return nil, errors.New("opus: decode failed")
	}
	frames := int(n) * d.channels
	pcm := make([]byte, frames*2)
	for i := range frames {
		v := uint16(d.out[i])
		pcm[2*i] = byte(v)
		pcm[2*i+1] = byte(v >> 8)
	}
	return pcm, nil
}

// Close releases the decoder. It is idempotent.
func (d *Decoder) Close() {
	if d == nil || d.st == nil {
		return
	}
	C.cap_opus_decoder_close(unsafe.Pointer(d.st))
	d.st = nil
	runtime.SetFinalizer(d, nil)
}
