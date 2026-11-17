#ifndef CAPSULAD_AIRPLAY_ASC_H
#define CAPSULAD_AIRPLAY_ASC_H

#include <stdint.h>
#include <string.h>

/* Minimal MSB-first bit writer for the two-byte AudioSpecificConfig this
 * receiver needs. AOT 2 (AAC-LC) and AOT 39 (AAC-ELD) only. */
typedef struct {
  uint8_t *buf;
  int capacity;
  int bits;
} asc_writer;

static int asc_put(asc_writer *w, uint32_t value, int count) {
  for (int i = count - 1; i >= 0; i--) {
    int bit = (value >> i) & 1;
    int byte = w->bits / 8;
    if (byte >= w->capacity) return -1;
    if ((w->bits % 8) == 0) w->buf[byte] = 0;
    w->buf[byte] |= (uint8_t)(bit << (7 - (w->bits % 8)));
    w->bits++;
  }
  return 0;
}

static int asc_frequency_index(int sample_rate) {
  switch (sample_rate) {
    case 96000: return 0;
    case 88200: return 1;
    case 64000: return 2;
    case 48000: return 3;
    case 44100: return 4;
    case 32000: return 5;
    case 24000: return 6;
    case 22050: return 7;
    case 16000: return 8;
    case 12000: return 9;
    case 11025: return 10;
    case 8000: return 11;
    case 7350: return 12;
    default: return -1;
  }
}

/* alac_asc_build writes the AudioSpecificConfig for AAC-LC or AAC-ELD and
 * returns its length in bytes, or -1 for an unsupported configuration. */
static int alac_asc_build(uint8_t *out, int capacity, int sample_rate, int channels, int eld) {
  asc_writer w = {out, capacity, 0};
  int index = asc_frequency_index(sample_rate);
  if (index < 0 || channels < 1 || channels > 2) return -1;
  if (!eld) {
    /* AOT 2, frequency index, channel configuration, GASpecificConfig. */
    if (asc_put(&w, 2, 5) < 0) return -1;
    if (asc_put(&w, (uint32_t)index, 4) < 0) return -1;
    if (asc_put(&w, (uint32_t)channels, 4) < 0) return -1;
    if (asc_put(&w, 0, 3) < 0) return -1; /* frameLengthFlag, dependsOnCoreCoder, extensionFlag */
  } else {
    /* AOT 39 is escape-coded: 31 then 39 - 32. */
    if (asc_put(&w, 31, 5) < 0) return -1;
    if (asc_put(&w, 39 - 32, 6) < 0) return -1;
    if (asc_put(&w, (uint32_t)index, 4) < 0) return -1;
    if (asc_put(&w, (uint32_t)channels, 4) < 0) return -1;
    /* ELDSpecificConfig: 512-sample frames, no resilience flags, no LD-SBR. */
    if (asc_put(&w, 0, 1) < 0) return -1;
    if (asc_put(&w, 0, 3) < 0) return -1;
    if (asc_put(&w, 0, 1) < 0) return -1;
    /* eldExtType = ELDEXT_TERM, then epConfig. */
    if (asc_put(&w, 0, 4) < 0) return -1;
    if (asc_put(&w, 0, 2) < 0) return -1;
  }
  return (w.bits + 7) / 8;
}

#endif
