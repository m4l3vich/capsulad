//go:build ffmpeg

// Package aac decodes AAC access units with the FFmpeg libavcodec decoder.
//
// This file is compiled only with the `ffmpeg` build tag; the app's build
// script passes it and refuses to package a receiver without AAC support. The
// decoder is the same one the upstream AirPlay receiver links for its AAC-LC
// buffered streams, and it is used only for decoding: the network transport,
// timing and buffering all stay in this app, so there is no second receiver or
// unmanaged daemon.
package aac

/*
#cgo pkg-config: libavcodec libavutil
#cgo LDFLAGS: -lm
#include <libavcodec/avcodec.h>
#include <libavutil/channel_layout.h>
#include <libavutil/error.h>
#include <libavutil/frame.h>
#include <errno.h>
#include <math.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include "asc.h"

typedef struct {
  AVCodecContext *ctx;
  AVPacket *pkt;
  AVFrame *frame;
  int channels;
} aac_ctx;

static void aac_set_error(char *buf, size_t len, const char *what, int code) {
  if (len == 0) return;
  char detail[128];
  av_strerror(code, detail, sizeof(detail));
  snprintf(buf, len, "%s: %s", what, detail);
}

static int aac_open(aac_ctx *h, int sample_rate, int channels, int eld,
                    char *err, size_t errlen) {
  memset(h, 0, sizeof(*h));
  const AVCodec *codec = avcodec_find_decoder(AV_CODEC_ID_AAC);
  if (codec == NULL) {
    snprintf(err, errlen, "this libavcodec has no AAC decoder");
    return -1;
  }
  h->ctx = avcodec_alloc_context3(codec);
  if (h->ctx == NULL) {
    snprintf(err, errlen, "out of memory allocating the AAC decoder");
    return -1;
  }
  uint8_t asc[8];
  int asc_len = alac_asc_build(asc, sizeof(asc), sample_rate, channels, eld);
  if (asc_len <= 0) {
    snprintf(err, errlen, "unsupported AAC configuration %d Hz %d ch", sample_rate, channels);
    return -1;
  }
  h->ctx->extradata = av_mallocz((size_t)asc_len + AV_INPUT_BUFFER_PADDING_SIZE);
  if (h->ctx->extradata == NULL) {
    snprintf(err, errlen, "out of memory allocating the AAC extradata");
    return -1;
  }
  memcpy(h->ctx->extradata, asc, (size_t)asc_len);
  h->ctx->extradata_size = asc_len;
  h->ctx->pkt_timebase = (AVRational){1, sample_rate};
#if LIBAVCODEC_VERSION_MAJOR >= 60
  av_channel_layout_default(&h->ctx->ch_layout, channels);
#else
  h->ctx->channels = channels;
  h->ctx->channel_layout = av_get_default_channel_layout(channels);
#endif
  int ret = avcodec_open2(h->ctx, codec, NULL);
  if (ret < 0) {
    aac_set_error(err, errlen, "opening the AAC decoder", ret);
    return -1;
  }
  h->pkt = av_packet_alloc();
  h->frame = av_frame_alloc();
  if (h->pkt == NULL || h->frame == NULL) {
    snprintf(err, errlen, "out of memory allocating the AAC packet/frame");
    return -1;
  }
  h->channels = channels;
  return 0;
}

static void aac_close(aac_ctx *h) {
  if (h->ctx != NULL) avcodec_free_context(&h->ctx);
  if (h->pkt != NULL) av_packet_free(&h->pkt);
  if (h->frame != NULL) av_frame_free(&h->frame);
  memset(h, 0, sizeof(*h));
}

static double aac_sample_at(const AVFrame *frame, int channel, int index) {
#if LIBAVCODEC_VERSION_MAJOR >= 60
  enum AVSampleFormat format = (enum AVSampleFormat)frame->format;
#endif
  switch (frame->format) {
    case AV_SAMPLE_FMT_FLTP: return ((const float *)frame->extended_data[channel])[index];
    case AV_SAMPLE_FMT_FLT: return ((const float *)frame->extended_data[0])[index * frame->ch_layout.nb_channels + channel];
    case AV_SAMPLE_FMT_S16P: return ((const int16_t *)frame->extended_data[channel])[index] / 32768.0;
    case AV_SAMPLE_FMT_S16: return ((const int16_t *)frame->extended_data[0])[index * frame->ch_layout.nb_channels + channel] / 32768.0;
    case AV_SAMPLE_FMT_DBLP: return ((const double *)frame->extended_data[channel])[index];
    case AV_SAMPLE_FMT_DBL: return ((const double *)frame->extended_data[0])[index * frame->ch_layout.nb_channels + channel];
    default: return 0.0;
  }
}

// aac_decode decodes one access unit and writes interleaved S16LE into out.
// It returns the number of frames written, or a negative FFmpeg error code.
static int aac_decode(aac_ctx *h, const uint8_t *data, int len,
                      int16_t *out, int out_frames) {
  if (av_new_packet(h->pkt, len) < 0) return AVERROR(ENOMEM);
  memcpy(h->pkt->data, data, (size_t)len);
  int ret = avcodec_send_packet(h->ctx, h->pkt);
  av_packet_unref(h->pkt);
  if (ret < 0) return ret;
  ret = avcodec_receive_frame(h->ctx, h->frame);
  if (ret == AVERROR(EAGAIN) || ret == AVERROR_EOF) return 0;
  if (ret < 0) return ret;
  int channels = h->channels;
  int frames = h->frame->nb_samples;
  if (frames > out_frames) frames = out_frames;
  for (int i = 0; i < frames; i++) {
    for (int ch = 0; ch < channels; ch++) {
      // Match ffmpeg's PCM quantisation (round-half-to-even in float), so a
      // fixture decodes sample for sample.
      float value = (float)aac_sample_at(h->frame, ch, i) * 32768.0f;
      long rounded = lrintf(value);
      if (rounded > 32767) rounded = 32767;
      if (rounded < -32768) rounded = -32768;
      out[i * channels + ch] = (int16_t)rounded;
    }
  }
  av_frame_unref(h->frame);
  return frames;
}

static int aac_frame_samples(aac_ctx *h) { return h->ctx->frame_size > 0 ? h->ctx->frame_size : 1024; }
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

// Profile is the AAC object type carried by the stream.
type Profile string

const (
	// ProfileLC is AAC-LC, the profile the upstream AirPlay receiver advertises.
	ProfileLC Profile = "lc"
	// ProfileELD is AAC-ELD. FFmpeg decodes it, but this receiver does not
	// advertise it, matching upstream's advertised format list.
	ProfileELD Profile = "eld"
)

// ErrUnavailable is returned when this build has no AAC decoder.
var ErrUnavailable = errors.New("aac: no AAC decoder in this build (build with -tags ffmpeg)")

// Config selects the decoder configuration for one stream.
type Config struct {
	SampleRate int
	Channels   int
	Profile    Profile
}

// Decoder decodes raw AAC access units (no ADTS header), one per call.
type Decoder struct {
	mu     sync.Mutex
	ctx    C.aac_ctx
	out    []int16
	closed bool
	errBuf [256]C.char
}

// Available reports whether this build can decode AAC.
func Available() bool { return true }

// New opens one AAC decoder.
func New(cfg Config) (*Decoder, error) {
	if cfg.SampleRate <= 0 || cfg.Channels <= 0 || cfg.Channels > 2 {
		return nil, fmt.Errorf("aac: unsupported configuration %d Hz %d ch", cfg.SampleRate, cfg.Channels)
	}
	profile := cfg.Profile
	if profile == "" {
		profile = ProfileLC
	}
	if profile != ProfileLC && profile != ProfileELD {
		return nil, fmt.Errorf("aac: unsupported profile %q", profile)
	}
	eld := C.int(0)
	if profile == ProfileELD {
		eld = 1
	}
	decoder := &Decoder{}
	if code := C.aac_open(&decoder.ctx, C.int(cfg.SampleRate), C.int(cfg.Channels), eld,
		&decoder.errBuf[0], C.size_t(len(decoder.errBuf))); code != 0 {
		return nil, fmt.Errorf("aac: %s", C.GoString(&decoder.errBuf[0]))
	}
	runtime.SetFinalizer(decoder, func(d *Decoder) { _ = d.Close() })
	return decoder, nil
}

// FrameSamples is the nominal frame size of the configured profile.
func (d *Decoder) FrameSamples() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return int(C.aac_frame_samples(&d.ctx))
}

// Decode decodes one access unit and appends interleaved S16LE PCM to dst.
func (d *Decoder) Decode(frame []byte, dst []byte) ([]byte, error) {
	if len(frame) == 0 {
		return dst, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return dst, errors.New("aac: decoder closed")
	}
	channels := int(d.ctx.channels)
	if channels <= 0 {
		return dst, errors.New("aac: decoder has no channel configuration")
	}
	need := 2048 * channels
	if cap(d.out) < need {
		d.out = make([]int16, need)
	}
	d.out = d.out[:need]
	written := int(C.aac_decode(&d.ctx, (*C.uint8_t)(unsafe.Pointer(&frame[0])), C.int(len(frame)),
		(*C.int16_t)(unsafe.Pointer(&d.out[0])), C.int(2048)))
	if written < 0 {
		return dst, fmt.Errorf("aac: decode failed with libavcodec error %d", written)
	}
	if written == 0 {
		return dst, nil
	}
	pcm := d.out[:written*channels]
	raw := unsafe.Slice((*byte)(unsafe.Pointer(&pcm[0])), len(pcm)*2)
	return append(dst, raw...), nil
}

// Close releases the libavcodec context.
func (d *Decoder) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	C.aac_close(&d.ctx)
	runtime.SetFinalizer(d, nil)
	return nil
}
