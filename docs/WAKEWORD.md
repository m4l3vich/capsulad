# Wakeword engine and personal training

**Updated:** 2026-11-12. The detector now uses pretrained speech embeddings and a small personal
keyword head, following the architecture of
[station-wakeword](https://github.com/tangenx/station-wakeword), not its unlicensed source code or
non-commercial dataset. The raw-mel classifier and browser numerical trainer have been removed.
Legacy personal models and cold negative corpora are refused, not silently reinterpreted: the
classifier artefact remains format version 3, while the training corpus is the strict, warm
`CNEG0002` version-2 container described below. A cold `CNEG0001` corpus must be regenerated.

## Why the previous detector was unreliable

The live Rockchip capture path, installed model and 20 saved takes were inspected:

| Area | Finding |
|---|---|
| Hardware | Unmuted `speex`, S16LE mono 16 kHz; GPIO 21 high. The saved takes and a fresh 10-second recording were unclipped. No capture-read errors appeared in the inspected daemon log. This does not certify microphone sensitivity or echo cancellation. |
| Training | Overlapping windows from the same recording leaked into validation. Silence and word fragments were labelled positive. A reported 97.3% window accuracy did not measure independent wakeword detection. |
| Model | The raw-mel MLP had no pretrained speech representation and failed on new room audio. Earlier weights had also diverged under momentum SGD. Raising a threshold does not repair either problem. |
| Streaming | Dropped capture chunks and queued audio across assistant transitions could leave noncontiguous detector state. Sequence/discontinuity tracking and state resets correct this independently of the model. |

Recordings now fail on mute or capture gaps. Detection pauses during recording so the assistant
cannot contaminate its own training takes. Half-duplex detection also remains off while the assistant
uses audio and for its 1.2-second tail.

### Six-take training failure

The later six-take failure exposed additional training defects, not evidence of a microphone fault:

- Blip and onset/tail labels were wrong: isolated clicks stretched speech labels into trailing
  noise, while short adjacent onsets could be discarded. Symmetric sustained-speech grouping now
  retains adjacent onsets and tails and rejects detached blips.
- Zero-filled lead-in and trailing padding created an artificial acoustic floor. Original word
  streams now use their own captured background on both sides.
- Adam's previous 0.01 step size was unstable in the controlled tune comparison; 0.001 learned the
  same complete-word/partial-word objective without removing necessary fragment negatives.
- A single pre-selected random negative subset, later mostly replaced by mined hard examples,
  starved broad speech coverage. Every epoch now draws at least 6,000 fresh random negatives from
  the whole pool and adds a separate hard set (at least 1,000, bounded by the mining pool) that is
  refreshed periodically; mining never replaces the random draw.
- Two cold-start blind spots dropped context phases. Cold local negatives omitted their first
  approximately two seconds, and every generic corpus stream also began at its own first sample, so
  the first approximately two seconds of each utterance never reached a 16-embedding training or
  evaluation context. Local negatives now take 2.3 seconds of real-room preroll from their own
  partition, and every corpus stream carries 2.32 seconds (37,120 samples, 29 embedding steps) of
  real same-split noise before its original body. Every valid 80 ms phase is eligible for sampling
  rather than a fixed 8,000-window corpus subset.

The diagnostic used the six available word takes, three room takes and all six available unrelated
phrases with fixed seed 1. The failed browser request's exact phrase selection and random seed were
not recoverable, so this is not claimed as a byte-identical reproduction of that request.
The recorded audio was unclipped. Chunked/full feeds and four independent frontends sharing assets
produced bit-identical embeddings on the four non-final word recordings; this did not implicate a
streaming inference or shared-state mismatch.

## Current architecture

- One framework-owned 16 kHz capture fan-out; no second physical microphone or cloud recognizer.
- Raw int16 magnitudes → Hann/FFT/mel frontend → pretrained Google/openWakeWord speech embeddings.
- The incremental backbone reuses convolution/pooling state. ARM64 NEON kernels process complete
  temporal/frequency convolution positions; other architectures have a generic implementation.
- One 96-dimensional embedding every 80 ms, beginning after 12,512 samples. Sixteen embeddings form
  a keyword context: shared 96→24 ReLU projection, flattened 384→24 ReLU layer, sigmoid output.
- The head's training and runtime forward pass are the same Go implementation. Version-3 JSON records
  the dimensions, weights, calibrated threshold, identity and training provenance/metrics.
- The negative corpus is a separate `CNEG0002` version-2 container. It may be raw or gzip-compressed;
  the loader detects the gzip magic, so the `.bin` extension is not significant. Produced files are
  deterministically gzip-compressed (mtime 0, no stored name) at the same `negatives.bin` path.
  Inside is the 8-byte magic, a little-endian 32-bit JSON-header length, the JSON header and
  little-endian float16 embeddings.
- The header requires `version: 2`, `feature_extractor: oww-google-v1`, 96 dimensions, 80 ms hop,
  first embedding at 12,512 samples and `preroll_samples: 37120`. Each stream requires
  `original_samples` (the actual 16 kHz body length, excluding preroll), and its frame count must be
  `floor((37120 + original_samples − 12512) / 1280) + 1`. The 37,120-sample preroll is 29 complete
  80 ms steps, so the original body keeps the same frontend phase as the saved and runtime voice
  audio; 36,800 would shift every body embedding by 60 ms.
- Trained-negative exposure is `sum(original_samples) / 16000`, not embedding count × hop and not
  preroll duration: borrowed preroll was already counted where it was recorded. Model provenance
  records the required `preroll_samples`; the corpus report records per-split preroll totals.
- The whole file and the decompressed logical stream are each bounded at 32 MiB, the JSON header at
  4 MiB, and one stream's frame count is bounded from the logical size before arithmetic. Streams are
  decoded through a bounded reader directly into one float32 backing; the loader never holds the
  entire compressed or uncompressed file and makes no extra float32 copy. Gzip CRC failures,
  truncation, trailing members/bytes and declared expansion past the bound fail the load rather than
  producing a silent partial corpus.
- Legacy `CNEG0001`, header `version: 1`, and missing or invalid warm-up metadata are rejected with
  an explicit regeneration instruction. Old embeddings are never relabelled as warm.
- Runtime requires two consecutive scores at the threshold, then applies a 2.9-second cooldown.
  Mute, capture gaps and half-duplex transitions clear pending audio, embeddings and score streaks.
  A reset needs approximately two seconds of fresh audio before a full context is available.
- Activations retain the existing selected-assistant, readiness, bounded pre-roll and microphone-mute
  semantics. Manual talk-button activation does not require a trained keyword model.

The model is loaded from `<data-dir>/wakeword/model.json` (or `-wakeword-model`). A missing, invalid or
legacy model is unavailable, not a functioning fallback. Detection only begins after a valid model
has been installed and selected/enabled in Settings.

### Numerical and device evidence

The embedding package has ONNX reference vectors, incremental/full-window parity checks and
ARM64/generic kernel comparisons. Its race-enabled checks passed after the incremental/NEON cutover.
`tools/export_oww.py` verifies the upstream ONNX digests; `tools/gen_oww_fixtures.py` regenerates vectors.

Measured on the connected RK3308 (four Cortex-A35 cores, 1.008 GHz), over ten seconds of PCM:

| Measurement | Result |
|---|---|
| Complete frontend/embedding probe | 4.396 s CPU/wall execution for 10 s audio; 116 outputs |
| Mean per emitted embedding, including startup/frontend | 37.9 ms |
| Maximum steady embedding-producing feed | 39.6 ms |
| Cold first embedding-producing feed | 207.3 ms |
| Capture subscriber queue budget | 320 ms |
| Mel processing | 0.321 ms per 10 ms frame |

The former full-window scalar backbone took 425 ms per output and could not sustain the 80 ms
cadence. The new probe fits the cadence and startup queue budget. This is a frontend benchmark,
not a sustained loaded-system certification or a detection-quality measurement.

## Training through `/wakeword`

1. Choose the word, unmute and put Capsula in its normal position. The guided flow records three
   independent 8-second room takes, four unrelated 4-second phrases and **six independent 2-second
   word takes**. A cancellable three-second countdown precedes each recording, and the ring pulses
   red for the whole take. Word and unrelated-phrase takes play a three-second tone countdown (two short tones
   and one longer one); capture starts only after it drains, so the cue is never recorded.
   Unrelated-phrase takes show a random everyday Russian phrase, so the negatives are not one
   fixed utterance. The six word prompts ask for normal speech, louder speech, a clear whisper,
   speaking from about two metres away, calling someone by name, and soft speech with the head
   slightly turned. Say the complete word once, half a second after the final tone. Do not say it
   in negative phrases.
2. Review the selected take counts. Only IDs from this setup are submitted; saved recordings from
   earlier setups are not silently mixed in. The selection survives a reload in this tab. Starting
   a new setup clears the selection, not the saved WAV files.
3. The browser starts `AssistantService.TrainWakewordClassifier` through authenticated, CSRF-checked
   `POST /wakeword/train` and displays its streamed progress. Audio, embeddings, augmentation,
   fitting and quality checks stay on the device. Closing/cancelling aborts the request; reload to
   check installation if cancellation/disconnection occurred near the final commit.
4. Whole original recordings are assigned to train/tune/test by content, independently of optimizer
   seed and input order. Each held-out word partition gets 25% (at least two takes); the rest trains.
   Six word takes give two fitting, two tuning and two final-test recordings. Exact duplicates are
   refused. Six is both the API minimum and guided target; extra independent takes are optional.
5. Waveform augmentation is produced lazily before embedding extraction, with bounded workers and
   memory. Only complete-word windows become positive examples; word fragments and clean pre-word
   contexts are negatives. Local room/speech/false-trigger clips receive real-room preroll from their
   own partition. Every valid 80 ms negative context remains eligible as a view, without feature copies.
   Each epoch draws at least 6,000 fresh random negatives from the whole pool, plus a separate hard
   set of at least 1,000 refreshed periodically by bounded mining; mining never replaces the random
   draw. Adam uses a 0.001 learning rate, 64-example batches and 40 epochs. This broad-coverage
   recipe, not a threshold change, is what removed the earlier false activations.
6. Threshold tuning replays the actual 80 ms, two-score, 2.9-second-cooldown policy. The emitted
   decision's window must contain the complete labelled word and arrive within one second after it.
   An earlier score in the streak may precede completion: it is not a separate activation. Truly
   premature activation still starts cooldown and is not ignored.
   The fixed model/threshold then faces the untouched final recordings and corpus partition once.
   Both gates require at least 80% independent-word recall and zero false activations. No model is
   installed after a failing gate. A consumed final test requires a fresh setup, not threshold
   adjustment against the test results.
7. On success the daemon validates the typed version-3 artifact, constructs its engine, atomically
   replaces `model.json`, stops the previous runner and starts the replacement. Select `word.personal`
   and enable detection in Settings if it is not already selected.

Training is serialized, cancellable and bounded to 30 minutes by the Web UI. A warm `CNEG0002`
negative corpus must already exist at `<data-dir>/wakeword/negatives.bin` (or at the path passed to
`-wakeword-negatives`); it is not an inference dependency, and the loader refuses a cold corpus
rather than retraining without warm-up.
The browser no longer receives features or uploads arbitrary classifier JSON. The old
`GetWakewordFeatures` and `InstallWakewordClassifier` RPCs have been removed.

### Current quality evidence and limits

The original eight word takes were replayed against the licensed corpus. Diagnostic runs exposed
unseen speech activating at the calibrated threshold; no resulting personal model was installed.
Validation also caught an over-strict evaluator that required both streak windows to contain the
whole word. It now judges the actual decision time, so a preceding partial score does not invalidate
a correct post-word activation. Truly premature fires still consume cooldown. Correct derivatives
were verified with guarded finite differences; this was not evidence of a hardware fault or a
forward/backward inference mismatch. Those eight recordings are now consumed for diagnosis and
cannot certify a later fit.

The later six-take candidates show why a passing private split is not a positive certificate. A
threshold-0.81 candidate passed the private tune split (2/2 words, zero false activations) and the
private final regression (2/2, zero), and removed eight of nine earlier false activations, but one
legitimate non-keyword Russian phrase still fired after 8.0 seconds. After Russian training was
broadened with the 194 retired auxiliary recordings, a threshold-0.52 candidate removed all nine
old false activations, yet a genuinely new 39-minute set produced one Russian false activation at
1.76 seconds (English zero). That early fire lay inside the generic cold-start blind spot, so the
general corpus fix was applied instead of changing the threshold. Neither candidate was installed.

The warm corpus addresses that blind spot: a real same-split noise prefix precedes each original
body, so the first approximately two seconds is represented in training and evaluation. The
previously failing 1.76-second development clip now scores zero false activations (peak 0.0124),
and the old nine-clip error-timing regression also scores zero (peak 0.1269). Both are
development regressions on already-consumed audio, not fresh evidence.

The installed model was trained from six word takes, three room takes and six unrelated phrases with
fixed seed 1, the warm 872-stream corpus and threshold 0.59. It passed the private tune split with
2/2 words and zero false activations, and the private final regression with 2/2 and zero. Threshold
0.590 is the normal tune-split calibration grid result; neither the threshold nor seed 1 was tuned
against v3 or v4, and v3/v4 were used only for scoring after the model was frozen. Those private
final recordings have already been used for diagnosis and regression, so this repeat is not an
independent positive certificate. Six word takes are also a small enrollment, and this is not a
speaker-independent Russian model.

A fourth negative set was prepared before the candidate was scored (manifest SHA-256
`95406ffd0a04be2db102357818bc4c6969fe32258e26a526924653d40aa3ac7b`): 237 whole clips, 2,416.67
seconds raw (about 40.3 minutes), split into 133 English clips from 39 official LibriSpeech
`dev-clean` speakers and 104 Russian clips from the official FLEURS test split. The English speakers
are physically disjoint from all 32 main-corpus train voices and from the exhausted `test-clean`
pool. The Russian sentence texts are disjoint from all dev/v1/v2 text and from the 98 texts used by
the previous v3 set, and no clip or raw hash appears in the 1,339 earlier inputs. The frozen
candidate was evaluated once on this set: zero false activations in both languages over 2,409.28
scored seconds (at most 79 ms of trailing PCM per clip is not scored), with peak scores 0.0253
English and 0.4960 Russian at threshold 0.59 and no preroll false activations. The evaluation is
negative-only: it measures false activations, not positive recall. The Russian manifest's first TSV
column is a sentence ID, not a speaker ID; sentence-text disjointness is real, voice independence is
unknown.

Zero false activations in roughly 40 minutes of held-out audio is not a daily false-alarm-rate
guarantee. After setup, evaluate new phrases, confusable words, distances, playback and long
real-room backgrounds through the actual capture path. Do not report window accuracy as wakeword
accuracy.

Live use after this installation is reported by the user as "mostly works fine". That is informal
field feedback, not a measured accuracy or daily false-alarm-rate result, and it does not claim
perfect detection.

## Deployment state and cutover verification

At the warm cutover (2026-11-11T11:51:05Z), PID 21406 ran `/data/capsdemo/capsulad` with SHA-256
`b3ec47eff84b662ab0efea2004cec74f3ffa04a923443379c0a31654d71d3098` (21,954,712 bytes), replacing
`2035279e…`. At that point the installed corpus was the gzip `CNEG0002` file
`fbdaf9b30a7d4e2b8a83eb1ef85b05e253272b447b259992b1735e09801f88c8` (24,386,227 bytes; logical
26,558,985; 872 streams; 137,732 frames; strict 37,120-sample preroll). Its sidecar was
`negatives.bin.provenance.json.gz`, 514,451 bytes, SHA-256
`5041ec978799b29477eecad5f66acdd8e3c3c9c5a4a78d3276ba24147c826ced`; it decompresses to 5,120,812
bytes (`fe13f5af…`) and the old 2.24 MB uncompressed JSON was removed only after that verification.
The previous 21,681,117-byte cold corpus and legacy model are gone.

The installed `model.json` is 132,984 bytes, SHA-256
`5f92202fddc38629db4d9a6db3c965a57ffcb812ad09d41b634e27428fc267e3`: version 3, id `word.personal`,
display `Маруся`, language `ru`, threshold 0.59, context 16, projection 24, hidden 24. It is the
native compact serialization of the host candidate `64ba9f43…` (192,372 bytes pretty-printed). Both
have canonical JSON SHA-256 `fdf16ba5e396b00e98528fc06af3189dfe95b22063ab8cd7125c9393d02b472d`;
every weight, threshold and trained-metric value is identical, with maximum absolute weight
difference 0.0. The difference is serialization only.

Native training ran to completion on the device through `TrainWakewordClassifier`: six word takes,
three room takes, six unrelated phrases, seed 1, zero false-trigger takes, via
`/tmp/capsulad-native-enrollment-warm` (`c8414859…`). Wall time was 670.6 seconds (11 min 10.6 s),
well inside the 30-minute RPC bound. Stages ran load → validate → prepare → augment → embed →
negatives → train → calibrate → final test → install, ending with
`installed word.personal (threshold 0.59)`. Daemon high-water memory was 23,564 kB before training
and 96,496 kB at peak (+72,932 kB); idle RSS afterward was 25,884 kB. The run logged zero
ERROR-level and zero capture-error lines. This is the full-training measurement the earlier partial
probe did not provide.

Actual on-device A35 replay of the installed artifact used the statically linked ARM64 scorer
`/tmp/capsulad-warm-score-linux` (SHA-256
`13409352cb0cff10c8772b3b7b62093cfba4e0899bfd56cd534b74c343348d16`, 4,212,234 bytes) against the
active model `5f92202f…` and the 15 captured files. It exited 0 in 81 seconds on the device: 6/6
word takes had a longest run of 6, 5, 6, 5, 6, 6 windows (all ≥2; maximum score 1.0000), and 9/9
background takes had a longest run of 0 (all <2; maximum score 0.0000), using
`wk_1794372342762_background.wav` as real-room preroll. The replayed policy is the runtime 80 ms
hop, two consecutive scores and 2.9-second cooldown. The daemon stayed at PID 21406 (VmRSS
27,260 kB, VmHWM 96,496 kB), did not restart, logged zero ERROR-level lines, and kept wakeword
enabled with the microphone unmuted. The scorer was removed from device tmpfs after the run.

Immediately after installation, RPC reported `personal_available=true`, `selected="word.personal"`,
`wakeword_enabled=true`, `microphone_muted=false`, `hardware_mute=true` and `saved_samples=15`. The
settings revision 1476 change was the single CAS migration `vendor.voice-agent` → `word.personal`;
assistant app, master volume, timezone and brightness settings were preserved. At that point all 15
user WAVs matched the protected snapshot, clock and assistant were ready, the Web UI returned 303,
and the watchdog was armed with a 21-second timeout. `/data` then had 3,788 KiB free, `MemAvailable`
was 119,448 kB and tmpfs had 85,128 KiB free.

### Post-cutover operations and storage

The daemon was replaced in place through the boot service on the same unit
(`capsula daemon install`): `/data/capsdemo/capsulad` is now 21,889,176 bytes, SHA-256
`a01c0c0a398623e71b6ac5f8d7cc199c7345fa5a36089839ee7eeb185e6a78c7`, with the cutover binary kept
as `capsulad.prev` (b3ec47ef…). The installed model is unchanged (`5f92202f…`), loads on boot and
detects (`classifier word.personal installed … threshold=0.59`); the assistant, clock and the new
media provider start after a power cycle.

The 24.4 MB corpus, its gzip sidecar and the 15 recordings were then moved to the host backup
(`_capsula-mini-data-backup/`, git-excluded) to make room for the daemon upgrade and the media app.
`/data` has about 4.3 MB free, the log is capped at 512 KiB plus one rotated copy, and new takes or
training are refused below 2 MiB free. Training therefore needs the corpus restored first, or the
daemon started with `-wakeword-negatives /tmp/negatives.bin`:

```sh
adb push _capsula-mini-data-backup/wakeword/negatives.bin /data/capsdemo/wakeword/negatives.bin
adb push _capsula-mini-data-backup/wakeword/negatives.bin.provenance.json.gz \
  /data/capsdemo/wakeword/negatives.bin.provenance.json
adb push _capsula-mini-data-backup/wakeword/samples/. /data/capsdemo/wakeword/samples/
```

The corpus remains training input only; it is not consulted by detection.

Earlier implementation-path evidence remains valid: the focused wakeword race checks passed
(`internal/wakeword` 20.514 s), the production loader accepted the actual gzip corpus, a real browser
exercised streamed progress/cancellation and the six-take guide, and the RPC rejects five word takes
while accepting six through augmented embedding extraction. This is a personal model in a small
enrollment, not a field certification; long-duration false-activation and miss rates remain
unmeasured.

The previous verbose trace log had grown to 30 MB and blocked binary staging. It was preserved
losslessly as `/data/capsdemo/capsulad.pre-embedding-cutover.log.gz`; the new daemon runs without
`-wakeword-trace`.

## Licensed negative corpus

The warm corpus contains 872 whole-file source streams and 137,732 embedding vectors from
approximately 160.8 minutes of original audio: about 64 minutes of English speech, 65 minutes of
Russian speech, 16 minutes of noise and 16 minutes of music. Four licensed source IDs are retained:
`librispeech`, `fleurs-ru`, `ms-snsd-cc0` and `kevin-macleod`. All 356 recordings in the FLEURS
`ru_ru` development archive are now included: the original 162-file selection plus 194 recordings
that had previously formed auxiliary evaluation sets. Those 194 are permanently train-only and are
never described as held-out again. No official FLEURS test audio, or any v3/v4 evaluation audio, is
in the training corpus. Whole recordings are confined to one split; LibriSpeech speaker identities
also stay whole within a split. Russian files remain whole, but speaker independence is not claimed.

Every stream carries a 37,120-sample (2.32-second) real same-split noise prefix before its original
body. The prefix is borrowed from an existing noise recording in the same partition, so it is reused
audio, not new source material: the corpus adds 2,023.04 seconds of preroll across 872 streams. The
original body is copied exactly once, in order, and its samples and hash are unchanged. The prefix
uses a deterministic ID-seeded noise choice, offset and quiet log-uniform RMS target between 40 and
400 raw S16 units; short noise is tiled, and a missing or silent same-split noise source is a hard
failure, never a zero fallback.

The gzip-compressed `CNEG0002` file is 24,386,227 bytes on disk (26,558,985 bytes logical), with a
114,429-byte JSON header and 26,444,544-byte float16 payload. Its SHA-256 is
`fbdaf9b30a7d4e2b8a83eb1ef85b05e253272b447b259992b1735e09801f88c8`. The full source URLs, split
programme, per-stream prefix noise identity/offset/gain, raw and converted body hashes, complete
licensing and the previous sidecar lineage are retained in the deterministic gzip JSON sidecar
`negatives.bin.provenance.json.gz`: 514,451 bytes, SHA-256
`5041ec978799b29477eecad5f66acdd8e3c3c9c5a4a78d3276ba24147c826ced`. It decompresses byte-for-byte
to the 5,120,812-byte JSON (SHA-256 `fe13f5af5a33cf005e5477bdb6a81e582b9c27a8e4c57bf7258f7a9efcd016ea`)
and embeds the 2,243,735-byte previous sidecar once. Split totals are: train 745 streams / 111,125
frames / 7,716.929 seconds of original audio; tune 62 / 14,447 / 1,057.767; test 65 / 12,160 /
870.858. Preroll is excluded from these exposure figures.

| Source | Terms used |
|---|---|
| [LibriSpeech / OpenSLR SLR12](https://www.openslr.org/12/) | CC-BY-4.0; retain attribution |
| [Google FLEURS, Russian development audio](https://huggingface.co/datasets/google/fleurs) | CC-BY-4.0; retain attribution |
| [MS-SNSD Freesound noise subset](https://github.com/microsoft/MS-SNSD) | CC0; DEMAND CC-BY-SA files explicitly excluded |
| [Kevin MacLeod / Incompetech](https://incompetech.com/music/royalty-free/licenses/) | CC-BY-4.0; per-track attribution retained |

No Station negatives, NC/ND/SA recordings or pretrained non-commercial keyword heads are used.
The Google/openWakeWord embedding weights are Apache-2.0 and exported independently. No applicable
source-code license was visible in Station at inspection, so only its architectural approach was
adopted; repository visibility is not source-code permission.

Host preparation needs Python 3, ffmpeg/ffprobe and Go; none is installed on the device for training:

Download/check the LibriSpeech `test-clean` and Russian FLEURS development archives using the URLs
and digests printed by `plan`. Extract them to `/tmp/LibriSpeech/test-clean` and
`/tmp/fleurs-ru-dev`, respectively. Passing `test-clean` itself as the speech root makes the helper
split its speakers for this custom corpus; passing its parent retains the official test label.
Keep the generated manifest: a different source selection produces a different artifact checksum.

The `fleurs-manifest` generator defaults to `--max-seconds-per-split 600`, which selects at most
10 minutes per synthetic split, about 30 minutes of Russian total. That default is how the earlier
corpus under-covered Russian. Pass `0` to disable the cap and manifest every source file; the
`ru_ru` development archive currently yields 356 recordings, about 65 minutes. The shipped artifact
was built from the preserved merged manifest at
`/tmp/capsulad-negative-corpus-ru-expanded/manifest.merged.json`, which also fixes the historical
splits and marks the 194 auxiliary Russian recordings as train-only. Re-running the generator below
selects the same complete source pool but writes a new manifest; retain it for reproduction.

```sh
python3 tools/fetch_wakeword_corpus.py plan
python3 tools/fetch_wakeword_corpus.py fetch --out /tmp/capsulad-negative-corpus
python3 tools/fetch_wakeword_corpus.py speech-manifest \
  --root /tmp/LibriSpeech/test-clean --seconds-per-speaker 90 \
  --out /tmp/capsulad-negative-corpus/manifest-speech.json
python3 tools/fetch_wakeword_corpus.py fleurs-manifest \
  --root /tmp/fleurs-ru-dev --max-seconds-per-split 0 \
  --out /tmp/capsulad-negative-corpus/manifest-russian.json
python3 tools/fetch_wakeword_corpus.py merge \
  --input /tmp/capsulad-negative-corpus/manifest-noise-music.json \
  --input /tmp/capsulad-negative-corpus/manifest-speech.json \
  --input /tmp/capsulad-negative-corpus/manifest-russian.json \
  --out /tmp/capsulad-negative-corpus/manifest.json
python3 tools/build_wakeword_negatives.py \
  --manifest /tmp/capsulad-negative-corpus/manifest.json \
  --out /tmp/capsulad-negative-corpus/negatives.bin
python3 tools/build_wakeword_negatives.py --verify \
  /tmp/capsulad-negative-corpus/negatives.bin
```

The builder validates licenses, attributions, duplicate audio, complete-file splits and the
embedding extractor identity, converts audio with ffmpeg, then invokes the native
`cmd/wakeword-negatives` exporter. It writes a gzip `CNEG0002` file plus the sidecar. On the expanded
rebuild, `--embed-previous-provenance <old>.provenance.json` carries the earlier lineage verbatim.
Ship the sidecar alongside the binary. The current artifact is 24,386,227 bytes with SHA-256
`fbdaf9b30a7d4e2b8a83eb1ef85b05e253272b447b259992b1735e09801f88c8`; its gzip sidecar is
514,451 bytes with SHA-256 `5041ec978799b29477eecad5f66acdd8e3c3c9c5a4a78d3276ba24147c826ced`.

On the connected unit `/data` is a 78.5 MB partition. The built corpus, sidecar and recordings are
**not resident** after the 2026-11-11 boot/upgrade deployment: they live in
`_capsula-mini-data-backup/` on the host, because the daemon and previous-binary rollback copy fill
the partition. Copy them back only for a training session, or start the daemon with
`-wakeword-negatives /tmp/negatives.bin` and stage the file on tmpfs instead. Whichever path is
used, never leave a partial `negatives.bin`: remove the old file, stage `.tmp`, then rename. The
procedure below is the in-place variant for a unit with enough free space.

```sh
adb push /tmp/capsulad-negative-corpus-warm/negatives.bin /tmp/negatives-warm.bin
adb push /tmp/capsulad-negative-corpus-warm/negatives.bin.provenance.json.gz /tmp/negatives-warm.provenance.json.gz
adb shell 'cp /data/capsdemo/wakeword/negatives.bin /tmp/negatives-old.bin'
adb shell 'sha256sum /tmp/negatives-warm.bin'   # must match fbdaf9b3…
adb shell 'rm -f /data/capsdemo/wakeword/negatives.bin'
adb shell 'cp /tmp/negatives-warm.bin /data/capsdemo/wakeword/negatives.bin.tmp'
adb shell 'mv /data/capsdemo/wakeword/negatives.bin.tmp /data/capsdemo/wakeword/negatives.bin'
adb shell 'cp /tmp/negatives-warm.provenance.json.gz /data/capsdemo/wakeword/negatives.bin.provenance.json.gz.tmp'
adb shell 'mv /data/capsdemo/wakeword/negatives.bin.provenance.json.gz.tmp /data/capsdemo/wakeword/negatives.bin.provenance.json.gz'
```

The compressed corpus and sidecar are not optional on this unit: they are what make the warm
metadata fit. The daemon loads the corpus through the gzip path with the same 32 MiB logical bound.

### Offline training

`cmd/waketrain` uses the same augmentation, embedding, fitting and gates as the daemon:

```sh
go run ./cmd/waketrain \
  -samples /path/to/word-takes -ambient /path/to/room-takes \
  -speech /path/to/unrelated-phrases \
  -negatives /path/to/negatives.bin -out /path/to/model.json \
  -name word.personal -display Маруся -language ru -seed 1
```

Word files end in `_word.wav`; `_background.wav` inside `-samples` is treated as room audio.
`-false` accepts a directory of false-trigger negatives. Failed quality checks leave no new model.
The `-negatives` corpus must be the warm `CNEG0002` file; a cold corpus is rejected. Reported negative
exposure sums only the original bodies, so borrowed preroll is not counted twice.

## Resource and operational limits

The RK3308 exposes 239,840 kB RAM and no swap. A previous workflow exhausted memory and the kernel
OOM-killed the daemon. Keep the default 80 MiB Go soft heap limit (`-mem-limit-mb`) and let the boot
service own the device instead of running both stacks together. On-device augmentation is bounded;
negative embeddings are decoded once into float32 storage and feature windows are views.

The hardware watchdog cannot be assumed disarmable. Use graceful TERM so the daemon hands feeding
to its detached feeder; start the replacement explicitly with `nohup` and verify the live RPC rather
than trusting a background shell's exit status. See [HARDWARE.md](HARDWARE.md).

Persistent space is the other hard limit: the daemon log rotates at 512 KiB and keeps one copy, and
new recordings or training are refused below 2 MiB free, because a full `/data` breaks logging and
the next in-place upgrade rather than failing one write. The corpus is not an inference dependency,
so it stays on the host outside training sessions.

`-wakeword-trace` logs score peaks and detection heartbeats; leave it off for normal production use.
Relevant checks are in `internal/wakeword`, `internal/wakeword/oww`, `internal/audio`,
`internal/daemon` and `internal/webui`. The removed JavaScript numerical trainer has no remaining
Node test command.
