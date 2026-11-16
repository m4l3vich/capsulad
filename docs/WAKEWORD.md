# Wakeword engine and personal training

The detector combines pretrained speech embeddings with a small personal keyword head. The
architecture follows [station-wakeword](https://github.com/tangenx/station-wakeword); none of its
source code or non-commercial data is used. Model artifacts are format version 3, the training
corpus is the warm `CNEG0002` version-2 container, and any other format is refused with an explicit
error rather than reinterpreted.

## Design constraints

- **Detection is a policy over the complete word.** Runtime requires two consecutive scores at the
  threshold, then a 2.9-second cooldown; the activation window must contain the whole word and arrive
  within one second after it. Window accuracy over overlapping windows is not wakeword accuracy.
- **Pretrained speech representations are required**, because a raw-mel classifier does not transfer
  to a new room and no threshold change repairs that.
- **Labels group sustained speech symmetrically**: adjacent onsets and tails belong to the word,
  detached blips, word fragments and clean pre-word contexts are negatives, and original word streams
  keep their own background instead of zero padding, which would fabricate an acoustic floor.
- **Context needs warm-up and contiguity**: every stream carries real same-split preroll so the first
  ~2 seconds reach a 16-embedding context, every valid 80 ms phase is eligible for sampling, and
  mute, capture gaps and assistant audio clear pending audio, embeddings and score streaks.

## Architecture

- One framework-owned 16 kHz capture fan-out; no second physical microphone and no cloud recognizer.
- **Provenance.** Wakeword inference, enrollment and training subscribe to the framework's processed
  capture stream (`framework.dsp.v1`) — the same chain as `audio.input.microphone`: 80 Hz high-pass,
  energy-gated adaptive spectral suppression ≤12 dB, unity gain, −1 dBFS limiter. Untouched physical
  `speex` samples exist only at `audio.input.microphone.raw`. The runner rejects any frame whose tap
  does not match the declared processing policy, and the provenance parser/`InputPolicy` validation
  reject unknown declarations. Every recording stores a processing-provenance sidecar; a take
  without one is treated as a raw recording and converted in memory through the same chain under that
  declared policy, never relabelled. Processed and raw endpoints share the mute gate, the
  visible-indication watchdog and stream cleanup; frame subscribers are separately mute-gated and
  bounded. Detection pauses while recording and while the assistant has audio, including its
  1.2-second tail; that assistant-audio gate (with a runner reset on the transition) clears pending
  audio, embeddings and score streaks — there is no separate hidden half-duplex switch. The
  installed model is trained on the raw vendor tap, so it stays installed, selected and
  byte-identical: detection remains enabled and the daemon logs an explicit unverified-threshold
  warning instead of rewriting weights, metadata, selection or forcing a retrain.
- **Colored-noise suppression.** The noise estimator learns from aggregate frame energy against a
  fast-down/slow-up floor and a very-slow-up average; spectral flatness does not gate learning, and
  per-bin power tracks symmetrically while a frame looks like the stationary floor. A cold-start
  guard keeps a voice or word at 0 dB relative to the learned reference out of the estimate; the
  first-vowel gate is ≥0.85 of reference (worst 0.896), and room re-engagement plus a second speech
  segment after a pause are preserved. Sustained amplitude-modulated room events are deliberately
  preserved so speech is not learned as noise. Synthetic pink, brown/rumble and amplitude-modulated
  room regressions measure 4.23, 8.33 and 3.42 dB of cleanup; the one authorized 10 s background
  recording — Mac-speaker music, no local playback reference — measures 2.9 dB overall and 2.7 dB at
  80–300 Hz, which is consumed background evidence, not fan/traffic, music-immunity or zero-FAR proof.
- **Frontend.** Raw int16 magnitudes → Hann/FFT/mel → pretrained Google/openWakeWord speech
  embeddings. The incremental backbone reuses convolution/pooling state. ARM64 NEON kernels process
  complete temporal and frequency convolution positions; other architectures use a generic
  implementation.
- **Head.** One 96-dimensional embedding every 80 ms, beginning after 12,512 samples. Sixteen
  embeddings form a keyword context: shared 96→24 ReLU projection, flattened 384→24 ReLU layer,
  sigmoid output. Training and runtime run the same Go forward pass. Version-3 JSON records
  dimensions, weights, calibrated threshold, identity and training provenance/metrics.
- **Corpus container.** A separate `CNEG0002` version-2 file, raw or gzip-compressed (the loader
  detects the gzip magic, so the `.bin` extension is not significant); produced files are
  deterministically gzip-compressed (mtime 0, no stored name) at `negatives.bin`. Inside: 8-byte
  magic, little-endian 32-bit JSON-header length, JSON header, little-endian float16 embeddings. The
  header requires `version: 2`, `feature_extractor: oww-google-v1`, 96 dimensions, 80 ms hop, first
  embedding at 12,512 samples and `preroll_samples: 37120`; each stream declares `original_samples`
  (its 16 kHz body length excluding preroll) and its frame count must be
  `floor((37120 + original_samples − 12512) / 1280) + 1`. The 37,120-sample preroll is 29 complete
  80 ms steps, so the body keeps the same frontend phase as saved and runtime voice audio; 36,800
  would shift every body embedding by 60 ms.
- **Corpus accounting.** Trained-negative exposure is `sum(original_samples) / 16000` — not embedding
  count × hop and not preroll duration, since borrowed preroll is already counted where it is
  recorded. Model provenance records the required `preroll_samples`; the corpus report records
  per-split preroll totals.
- **Corpus limits.** Whole file and decompressed logical stream are each bounded at 32 MiB, the JSON
  header at 4 MiB, and one stream's frame count is bounded from the logical size before arithmetic.
  Streams decode through a bounded reader into one float32 backing; the loader never holds the whole
  compressed or uncompressed file and makes no extra float32 copy. Gzip CRC failures, truncation,
  trailing members/bytes and declared expansion past the bound fail the load instead of producing a
  partial corpus. `CNEG0001`, header `version: 1` and missing or invalid warm-up metadata are
  rejected with a regeneration instruction; embeddings are never relabelled as warm.
- **Ensemble.** Every installed word shares **one** frontend and **one** 16×96 embedding ring; each
  word adds only its weights, scratch and last score. That sharing is mandatory on this device: a
  complete frontend emits an embedding in 37.9 ms against an 80 ms hop, so N frontends cannot keep
  up, while N heads over the one embedding stream cost a fraction of a millisecond each. Per-hop
  scoring of N heads allocates nothing; only the frontend keeps its pre-existing per-embedding
  allocation, which does not grow with N.
- **Runtime policy.** Each word has its own consecutive-window streak and its own calibrated
  threshold. The first hop on which several words complete is an arbitration point, not a double
  activation: the winner is the largest normalized margin `(score − threshold) / (1 − threshold)`,
  tie-broken by higher raw score and then by model id, so the outcome is deterministic. Losers'
  streaks reset without consuming anything. One **global** 2.9-second cooldown then applies on the
  runner, so one utterance cannot fire twice — not even as two different words. Firing resets every
  context. A reset needs approximately two seconds of fresh audio before a full context exists again.
- **Runtime gates.** Detection runs only on the declared processed tap, and pauses while the
  microphone is muted, while a training take is recording, and while any app an activation could
  address (the selected assistant **or** a bound app) holds capture or playback, including its
  1.2-second tail. Activations keep the readiness, bounded pre-roll and mute semantics; manual
  talk-button activation needs no trained keyword model.
- **Storage.** Words live under `<data-dir>/wakeword/models/<id>.json`, one version-3 artifact per
  word. The file name stem is the identity used by the settings binding, the activation's
  `matched_model_id` and the Web UI, so renaming a display label never rebinds a word. Ids are
  validated against `^[a-z0-9][a-z0-9._-]{0,63}$`. A corrupt or mismatched artifact is **reported and
  skipped**, never silently replaced or deleted. A missing or invalid artifact is unavailable, not a
  functioning fallback.

## Numerical and device evidence

The embedding package has ONNX reference vectors, incremental/full-window parity checks and
ARM64/generic kernel comparisons; its race-enabled checks pass. `tools/export_oww.py` verifies the
upstream ONNX digests and `tools/gen_oww_fixtures.py` regenerates vectors.

Measured on the connected RK3308 (four Cortex-A35 cores, 1.008 GHz) over ten seconds of PCM:

| Measurement | Result |
|---|---|
| Complete frontend/embedding probe | 4.396 s CPU/wall for 10 s audio; 116 outputs |
| Mean per emitted embedding, including startup/frontend | 37.9 ms |
| Maximum steady embedding-producing feed | 39.6 ms |
| Cold first embedding-producing feed | 207.3 ms |
| Capture subscriber queue budget | 320 ms |
| Mel processing | 0.321 ms per 10 ms frame |

A full-window scalar backbone takes 425 ms per output and cannot sustain the 80 ms cadence; the
incremental probe fits both the cadence and the startup queue budget. Chunked and full feeds, and
four independent frontends sharing assets, produce bit-identical embeddings on the four non-final
word recordings, ruling out a streaming or shared-state mismatch. This is a frontend benchmark, not a
loaded-system or detection-quality measurement.

Native training completed on the device through `TrainWakewordClassifier` — six word takes, three
room takes, six unrelated phrases, seed 1, zero false-trigger takes — in 670.6 s wall time (11 min
10.6 s) inside the 30-minute RPC bound, with stages load → validate → prepare → augment → embed →
negatives → train → calibrate → final test → install and the final line `installed word.personal
(threshold 0.59)`. Daemon high-water memory: 23,564 kB before training, 96,496 kB at peak
(+72,932 kB), idle RSS 25,884 kB afterward; zero ERROR-level and zero capture-error lines.

An on-device A35 replay of the installed artifact (`/tmp/capsulad-warm-score-linux`, statically
linked ARM64 scorer, 4,212,234 bytes, SHA-256
`13409352cb0cff10c8772b3b7b62093cfba4e0899bfd56cd534b74c343348d16`) against the active model and the
15 captured files exited 0 in 81 s: 6/6 word takes had a longest run of 6, 5, 6, 5, 6, 6 windows
(all ≥2, maximum score 1.0000) and 9/9 background takes a longest run of 0 (maximum score 0.0000),
using `wk_1794372342762_background.wav` as real-room preroll and the runtime 80 ms hop,
two-consecutive-score and 2.9-second-cooldown policy. The daemon did not restart, logged zero
ERROR-level lines and kept wakeword enabled with the microphone unmuted.

## Training through Settings → Wakeword

1. Open **Settings → Wakeword**, choose the wake word and language, unmute and put the speaker in its
   normal position. The guided dialog records three independent 8-second room takes, four unrelated
   4-second phrases and **six independent 2-second word takes**. A three-second device cue precedes
   every take and the ring pulses red for the whole take; capture starts only after the cue drains, so
   it is never recorded. The browser never records or accesses the microphone. Unrelated-phrase takes
   show a random everyday phrase in the selected language, so negatives are not one fixed utterance.
   The six word prompts ask for normal speech, louder speech, a clear whisper, speaking from about two
   metres away, calling someone by name, and soft speech with the head slightly turned. Say the
   complete word once, half a second after the final cue, and never inside negative phrases.
2. Review the selected take counts; re-record or remove individual selected takes. Only IDs from this
   setup are submitted — saved recordings from other setups are never mixed in. The selection
   survives a reload in the tab; a new setup clears the selection, not the saved WAV files.
3. The browser starts training with the typed `train` command on the authenticated, same-origin
   `capsulad.web.v1` binary WebSocket and displays streamed stage/fraction/log frames, the terminal
   refusal or the installed certificate. Audio,
   embeddings, augmentation, fitting and quality checks stay on the device. Dismissing or cancelling
   sends `CancelTraining`; dismissing during a take aborts the in-flight record through
   `CancelOperation`. A lost session also cancels active recording and training on the device. After
   a disconnection, reload and inspect the installed model instead of assuming whether the final
   commit happened: a cancel that arrives while the final atomic commit is written may still install
   the new model, so a generic cancel never promises that the installed model stays unchanged.
4. Whole original recordings are assigned to train/tune/test by content, independently of optimizer
   seed and input order. Each held-out word partition gets 25% (at least two takes); the rest trains.
   Six word takes give two fitting, two tuning and two final-test recordings. Exact duplicates are
   refused. Six is both the API minimum and the guided target; extra independent takes are optional.
5. Waveform augmentation is produced lazily before embedding extraction with bounded workers and
   memory. Only complete-word windows become positive examples; word fragments and clean pre-word
   contexts are negatives. Local room/speech/false-trigger clips receive real-room preroll from their
   own partition. Every valid 80 ms negative context stays eligible as a view, without feature copies.
   Each epoch draws at least 6,000 fresh random negatives from the whole pool plus a separate hard set
   of at least 1,000, refreshed periodically by bounded mining; mining never replaces the random draw
   and addresses each distinct window once (see [Negative mining](#negative-mining-refusals-and-measured-quality)).
   Adam uses a 0.001 learning rate, 64-example batches and 40 epochs.
6. Threshold tuning replays the actual 80 ms, two-score, 2.9-second-cooldown policy. The emitted
   decision's window must contain the complete labelled word and arrive within one second after it; a
   score earlier in the streak may precede completion and is not a separate activation, while a truly
   premature activation still starts cooldown. The fixed model and threshold then face the untouched
   final recordings and corpus partition once. Both gates require at least 80% independent-word recall
   and zero false activations; no model is installed after a failing gate, and a consumed final test
   requires a fresh setup rather than threshold adjustment against its results. The calibration grid
   and the pass/fail certificate use **only** local held-out takes already processed by the daemon;
   external `CNEG` streams are precomputed off-policy examples, recorded under
   `metrics.auxiliary_offpolicy`, and never decide the certificate. A false activation on the broad
   `CNEG` corpus remains a hard veto. A refusal is a typed result, not a sentence: the stream first
   carries every distinct cause, every failing take and the measured splits, then ends with an error
   status that names them (see [Refusals](#negative-mining-refusals-and-measured-quality)).
7. On success the daemon validates the typed version-3 artifact, proves the **new ensemble** builds
   (the artifact plus every other installed word) and only then atomically writes
   `models/<id>.json`. It resubscribes one runner over one shared frontend, so detection restarts for
   **all** words — a warm-up of about two seconds. The final event and the Wakeword section carry the
   measured certificate (threshold, tune and final-test recall and false activations, auxiliary
   corpus result) read from the artifact. The dialog does not change the detection switch or any
   binding: a **new** word installs switched off, and retraining an existing word keeps that word's
   enabled state and app binding.

A setup trains either a new word or one existing word in place. The dialog's target decides: "Create
a wakeword" sends no `model_id` (the device derives a fresh `word.<hex>` id) and "Retrain" sends that
word's id. An in-place retrain replaces only that artifact: every other artifact stays
byte-identical, its gate is not re-run and its enablement and binding are untouched. A new setup
clears the take selection; switching the dialog between a new word and a retrain starts a fresh
selection too.

The Wakeword section lists every installed word with its effective state (listening, paused,
detection off, or needing attention with the reason), its own enable switch, the app it starts
("Selected assistant" or an installed app) and its measured certificate. It also lists the saved
takes on the device and recent hands-free detections newest first, capped at 20, engine-origin only
(manual talk-key activations are not detections). Each detection carries an opaque runtime ID:
marking one a false detection removes it from that list and changes nothing else — no audio, sample
or classifier state is affected, and the entry is gone from the next snapshot. Deleting a word
removes its artifact and its settings entry in one confirmed action; the saved takes are shared
training data and are kept. Training is serialized (one run at a time, also blocking deletion),
cancellable and bounded to 30 minutes by the Web UI.

A warm `CNEG0002` corpus must already exist at `<data-dir>/wakeword/negatives.bin` (or at
`-wakeword-negatives`); it is not an inference dependency, and a cold corpus is refused rather than
retrained without warm-up. The browser neither receives features nor uploads classifier JSON: there
are no feature-read or classifier-install RPCs.

## Multiple wakewords and app binding

**Installing and migrating.** The previous build kept one artifact at
`<data-dir>/wakeword/model.json`. On the first scan the daemon moves it to
`models/<its-id>.json` with a rename: no settings write, no retrain and no byte change
(the fallback copy path preserves bytes too, and is used only when the file is on another
filesystem). `-wakeword-model` is an import source for that one migration; the live store is always
the models directory. A file whose name is not a valid id, whose internal `id` disagrees with its file
name, or whose JSON does not load is left on disk, reported in the log with its reason and listed in
the Wakeword section as **needing attention** — it is never a functioning fallback and never
overwritten by a training run that happens to use the same id until the operator does so explicitly.

**Settings.** `wakewords` is the durable per-word list (`{model_id, app_id, enabled}`), replaced as a
whole by one patch, the same shape as `default_app_by_resource`. Its semantics are deliberately
backwards compatible:

- Absent (`nil`) is the legacy state: every installed word is live under the global
  `wakeword_enabled` switch and bound to the selected assistant. That is exactly the previous
  single-model behavior.
- Present is authoritative and materialized to contain every installed word. A word with no entry is
  **disabled**; a word is live only when the global switch, its own `enabled` and its availability
  all hold. At most eight entries are accepted and a duplicate `model_id` is refused.
- An explicit empty list means **no word is live**. The JSON document always carries the key, so
  `null` is the legacy state and `[]` is the explicit one; a client states the explicit state over
  the RPC by owning the `wakewords` mask with no entries, because a repeated field cannot otherwise
  distinguish "absent" from "empty". Deleting the last word leaves `[]` behind rather than falling
  back to legacy.
- `app_id` empty means the selected assistant. Deleting a word drops its entry; installing one
  appends a disabled entry.

The retired `wakeword_model_id` setting is gone, together with its settings key and its protobuf tags
(both reserved). It is not replaced by a new alias: **the old selector never gated anything.** The
engine never consulted it, so the documented "installed model, selected and enabled" gate did not
exist; this change implements the real per-word gate instead of preserving a cosmetic one.

**Selection and delivery.** A word bound to an app delivers to that app only. If the app is not ready
the activation is logged and dropped with an explicit reason, and is **not** rerouted to the selected
assistant or to another word: a wrong-app turn is worse than a dropped one. The delivery target is
always reported in `AssistantActivation.app_id`. Unbound words and physical keys target the selected
assistant; an API activation for a bound word is delivered to that same app, exactly like a detection,
and an API activation for an unbound word targets the selected assistant. The assistant-audio
detection gate and the dismissal path use the same widened set (selected assistant ∪ bound apps), so
a bound word's reply does not re-trigger detection and the circle key dismisses whichever of those
apps actually owns the turn. A bound app is part of the provider selection: it is recovered after a
crash and stops when it is both unselected and unbound.

**Cost of a change.** A per-word enable toggle or a rebinding is a settings change: it takes effect on
the next activation, with no re-subscribe and no warm-up. Installing or deleting an artifact rebuilds
the ensemble and resubscribes one runner, which costs about two seconds of warm-up for **all** words.
Memory is not the constraint: the in-memory classifier head is about 53 KB per word, while the
persisted artifact on `/data` is about 130 KB per word (JSON encoding); one 1.15 MB frontend and
1.3 MB of shared assets exist once.

**Rollback.** Restore the previous artifact file and the previous `wakewords` value (or delete the
`wakewords` key to return to the legacy state) and restart the daemon. Removing `models/` and putting
the old single file back at `wakeword/model.json` is also honored: the next scan migrates it again.
Taking the word out of the list (or clearing the global switch) suspends it immediately without
touching the artifact, which is the safe way to silence a word that misbehaves in the field.

**Honest limits.** Arbitration prevents a **double activation** on one hop; it does not prevent a
**wrong** activation, because every word's acoustic acceptance is still the per-word threshold and
gate certificate. Two words that sound alike can each pass their own threshold; only one fires per
utterance, and which one wins is the normalized margin. Per-word acoustic acceptance remains a device
gate: the host tests prove the policy, the storage and the engine parity, not recognition quality.
Warm-up, gain changes and a changed room still affect all words together through the shared frontend.
The host benchmark below measures the marginal cost of extra heads, not the on-device cadence.

| Measurement (host, Apple arm64) | N=1 | N=2 | N=3 | N=4 | N=5 |
|---|---|---|---|---|---|
| `Ensemble.Process` per 80 ms hop | 2.14 ms | 2.04 ms | 2.07 ms | 2.12 ms | 2.16 ms |
| Allocations per hop | 14 | 14 | 14 | 14 | 14 |

Re-run with `go test ./internal/wakeword -run '^$' -bench BenchmarkEnsembleProcess`. The per-hop
allocation count is constant because only the shared frontend allocates; scoring every head allocates
nothing (`TestEnsembleScoringDoesNotAllocate`). The frontend itself is unchanged, and
`TestEnsembleMatchesSingleModelEngine` proves the ensemble returns exactly the score the single-model
engine returned for the same artifact and audio, so this is not a new detector.

## Negative mining, refusals and measured quality

**Mining design.** The negative pool is a structural view, not a flattened list: the canonical
corpus windows come first, then the canonical local block (rooms, unrelated speech, false
activations, word partials and clean pre-word contexts), then that local block repeated to balance
it against the corpus (at most 1024 repetitions). Two integers describe that shape
(`Canonical`, `RepeatSpan`), so threading identity from enrollment into the optimizer adds no
per-window storage and copies no features.

Hard-negative mining addresses canonical windows only. Every mining round scores at most 32,768
distinct windows on at most four goroutines and returns at most the requested budget of distinct
canonical indices, so a duplicated view of one local window can occupy at most one hard slot.
Without this, a 1:1 repeated local block filled half of every hard set and half of the sampled
budget with copies of a few windows, and genuine corpus windows were never sampled. When the
canonical pool is larger than the cap, the sample is drawn with a seeded uniform rejection sampler
over canonical indices (one bitmap bit per canonical window), not with a fixed stride. The draw for
each round is a pure function of the pool shape, the training seed and the round, which keeps the
sample reproducible on every platform and keeps a long stream or a duplicated local block from
excluding whole sources. The additive fresh random draw, its floor, the hard budget floor, one
scoring pass per round and 30-minute RPC bound are unchanged. A seeded run remains byte-identical;
`TestFitClassifierDeterministic` and its hard-negative variant cover that contract.

**Refusals.** A refused run is a `*wakeword.TrainingFailure`: distinct machine causes (for example
`too_few_word_takes`, `duplicate_takes`, `short_context`, `no_positive_windows`,
`insufficient_heldout`, `tune_gate_failed`, `final_gate_failed`, `auxiliary_gate_failed`,
`corpus_missing`, `fit_failed`, `internal`), every failing take with its partition and 1-based
position in the submitted selection, every measured split and the threshold they were measured at.
Validation collects all problems instead of stopping at the first, and a tune or final-test refusal
lists the takes that were missed or that false-fired at the measured threshold. Distinct causes are
never collapsed: a local gate failure and an auxiliary corpus veto appear together.

The stream still ends with a gRPC error, so existing clients do not hang, but the daemon emits one
terminal `failed` event first with the structured refusal. Codes are meaningful:
`INVALID_ARGUMENT` for an unusable or insufficient selection, `FAILED_PRECONDITION` for gate,
policy, corpus or configuration failures, `INTERNAL` for a trainer or artifact fault, and the
existing `CANCELED`/`DEADLINE_EXCEEDED` for cancellation. The status text names the causes and the
takes (`... [cause: final_gate_failed]; takes to fix: word #3, room #2`). A failed run installs
nothing and leaves the previous model in place; explicit selection still means only the submitted
take IDs are used, and no retrain starts on its own. The dialog turns the causes into instructions
and lists the takes to re-record; a consumed final test still requires a fresh setup, and dismissal
still cancels.

**Measured quality.** The success event and the Wakeword section show the certificate already stored
in `Classifier.Trained`: the calibrated threshold and, for the tune split, the final test and the
auxiliary off-policy corpus, word recordings, detected recordings, recall and false activations. The
values are copied from the artifact; the browser neither recomputes a metric nor invents one. An
artifact trained before the record existed shows no table instead of zeros.

**Upstream applicability.** The ideas in `tanggenx/station-wakeword` PR #5 were reviewed without
copying its code or data (it has no source license). Adopted, reimplemented idiomatically here:
hard-negative deduplication/diversity, structured training-failure diagnostics and exposing gate
metrics to the operator. Rejected with reason: taking playback to the browser would break the
privacy contract in this document and [SPEAKER_BEHAVIOR.md](SPEAKER_BEHAVIOR.md) — the browser
receives samples and status only, never PCM or stored takes; Alice/cloud TTS augmentation sends
text to a third party with no license or terms, a hardcoded endpoint and no benefit for a
single-speaker model; its seeded 5/1 split with "recall below 0.8 is only a note" is weaker than
this project's whole-recording content split and ≥80% recall with zero false activations on both
tune and final test; and its evaluator's synthetic-silence warm-up and pre-completion hit counting
would misreport the daemon's actual two-window, cooldown and post-word policy.

## Quality evidence and limits

The candidates below are development evidence on already-consumed audio, not a certificate.

- Eight word takes replayed against the licensed corpus expose unseen speech activating at the
  calibrated threshold, so no personal model is installed from them. The evaluator judges the decision
  time (step 6), so a preceding partial score does not invalidate a correct post-word activation while
  a premature fire still consumes cooldown; correct derivatives are verified with guarded finite
  differences.
- A threshold-0.81 candidate passed the private tune split (2/2 words, zero false activations) and
  the private final regression (2/2, zero) and removed eight of nine known false activations, yet
  one legitimate non-keyword Russian phrase fired after 8.0 s.
- With Russian training broadened by the 194 auxiliary recordings that are train-only, a
  threshold-0.52 candidate removed all nine old false activations, but a new 39-minute set produced
  one Russian false activation at 1.76 s (English zero). That fire lay inside the generic cold-start
  blind spot, which the corpus fix addresses; the threshold stays unchanged. Neither candidate is
  installed.
- The warm corpus closes that blind spot: a real same-split noise prefix precedes each body, so the
  first ~2 seconds is represented in training and evaluation. The failing 1.76-second development
  clip scores zero false activations (peak 0.0124), and the nine-clip error-timing regression
  scores zero (peak 0.1269). Both are regressions on consumed audio.

The installed artifact (formerly `model.json`, now `models/word.personal.json`) is 132,984 bytes,
SHA-256
`5f92202fddc38629db4d9a6db3c965a57ffcb812ad09d41b634e27428fc267e3`: version 3, id `word.personal`,
display `Маруся`, language `ru`, threshold 0.59, context 16, projection 24, hidden 24. It is the
native compact serialization of the host candidate `64ba9f43…` (192,372 bytes pretty-printed); both
carry canonical JSON SHA-256
`fdf16ba5e396b00e98528fc06af3189dfe95b22063ab8cd7125c9393d02b472d` with identical weights, threshold
and metrics (maximum absolute weight difference 0.0).

The installed model is trained from six word takes, three room takes and six unrelated phrases with
fixed seed 1, the warm 872-stream corpus and threshold 0.59; it passed the private tune split (2/2,
zero) and the private final regression (2/2, zero). Threshold 0.590 is the normal tune-split
calibration grid result; threshold and seed are not tuned against the later evaluation sets, which
score the frozen model only. Those private final recordings have already served diagnosis and
regression, so this is not an independent positive certificate. Six word takes are a small
enrollment, and this is not a speaker-independent Russian model.

**Processed-tap usability.** Fed the processed tap, a warm classifier replay of all six backed-up
word takes — 2.32 s of its own same-session ambient audio, the word and a 1 s tail, DSP applied once
across prefix, body and tail, replayed in sequential 1280-sample windows under the real
two-consecutive-score policy and 2.9 s cooldown — fired 6/6 raw and 6/6 processed. That is
raw-trained model usability only: processing does not disable the installed detector, but the result
says nothing about calibration, accuracy or a daily false-alarm rate. It is a different algorithm
and dataset from the dumb-assistant endpointer result and must not be merged with it.

**Fourth negative set.** Manifest SHA-256
`95406ffd0a04be2db102357818bc4c6969fe32258e26a526924653d40aa3ac7b`: 237 whole clips, 2,416.67
seconds raw (about 40.3 minutes) — 133 English clips from 39 official LibriSpeech `dev-clean`
speakers and 104 Russian clips from the official FLEURS test split. The English speakers are
physically disjoint from all 32 main-corpus train voices and from the exhausted `test-clean` pool;
the Russian sentence texts are disjoint from all dev/v1/v2 text and from the 98 texts of the v3 set,
and no clip or raw hash appears in the 1,339 other inputs. The frozen candidate is evaluated once:
zero false activations in both languages over 2,409.28 scored seconds (at most 79 ms of trailing PCM
per clip is unscored), peak scores 0.0253 English and 0.4960 Russian at threshold 0.59, no preroll
false activations. The evaluation is negative-only — it measures false activations, not positive
recall — and the Russian manifest's first TSV column is a sentence ID, not a speaker ID, so
sentence-text disjointness is real while voice independence is unknown.

Zero false activations in roughly 40 minutes of held-out audio is not a daily false-alarm-rate
guarantee. After setup, evaluate new phrases, confusable words, distances, playback and long
real-room backgrounds through the actual capture path, and never report window accuracy as wakeword
accuracy. The user reports live use as "mostly works fine": informal field feedback, not a measured
accuracy or false-alarm rate.

## Licensed negative corpus

The warm corpus holds 872 whole-file source streams and 137,732 embedding vectors from approximately
160.8 minutes of original audio: about 64 minutes English speech, 65 minutes Russian speech, 16
minutes noise and 16 minutes music. Four licensed source IDs are retained: `librispeech`,
`fleurs-ru`, `ms-snsd-cc0` and `kevin-macleod`.

| Source | Terms |
|---|---|
| [LibriSpeech / OpenSLR SLR12](https://www.openslr.org/12/) | CC-BY-4.0; retain attribution |
| [Google FLEURS, Russian development audio](https://huggingface.co/datasets/google/fleurs) | CC-BY-4.0; retain attribution |
| [MS-SNSD Freesound noise subset](https://github.com/microsoft/MS-SNSD) | CC0; DEMAND CC-BY-SA files explicitly excluded |
| [Kevin MacLeod / Incompetech](https://incompetech.com/music/royalty-free/licenses/) | CC-BY-4.0; per-track attribution retained |

All 356 recordings of the FLEURS `ru_ru` development archive are included (162 original selection
plus 194 that also served auxiliary evaluation); the 194 are train-only and never described as
held-out. No official FLEURS test audio and no v3/v4 evaluation audio is in the corpus. Whole
recordings stay within one split, and LibriSpeech speaker identities also stay whole within a split;
Russian files stay whole, but speaker independence is not claimed. No Station negatives, NC/ND/SA
recordings or pretrained non-commercial keyword heads are used. The Google/openWakeWord embedding
weights are Apache-2.0 and exported independently; Station publishes no applicable source-code
license, so only its architectural approach is adopted.

Every stream carries a 37,120-sample (2.32-second) real same-split noise prefix before its original
body — reused audio, not new source material: 2,023.04 seconds of preroll across 872 streams. The
body is copied exactly once, in order, with unchanged samples and hash. The prefix uses a
deterministic ID-seeded noise choice, offset and quiet log-uniform RMS target between 40 and 400 raw
S16 units; short noise is tiled, and a missing or silent same-split noise source is a hard failure,
never a zero fallback.

The gzip `CNEG0002` file is 24,386,227 bytes on disk (26,558,985 logical): 114,429-byte JSON header
and 26,444,544-byte float16 payload, SHA-256
`fbdaf9b30a7d4e2b8a83eb1ef85b05e253272b447b259992b1735e09801f88c8`. Its sidecar
`negatives.bin.provenance.json.gz` is 514,451 bytes, SHA-256
`5041ec978799b29477eecad5f66acdd8e3c3c9c5a4a78d3276ba24147c826ced`, decompressing byte-for-byte to
the 5,120,812-byte JSON (`fe13f5af…`) that embeds one 2,243,735-byte source sidecar once. It records
source URLs, split programme, per-stream prefix noise identity/offset/gain, raw and converted body
hashes, complete licensing and prior sidecar lineage. Split totals: train 745 streams / 111,125
frames / 7,716.929 s original audio; tune 62 / 14,447 / 1,057.767; test 65 / 12,160 / 870.858.
Preroll is excluded from these exposure figures.

Build the corpus on a host (Python 3, ffmpeg/ffprobe, Go; none of these is installed on the device):

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

`plan` prints the download URLs and digests; extract to `/tmp/LibriSpeech/test-clean` and
`/tmp/fleurs-ru-dev`. Passing `test-clean` itself as the speech root makes the helper split its
speakers; passing its parent keeps the official test label. `fleurs-manifest` defaults to
`--max-seconds-per-split 600`, i.e. about 30 minutes of Russian in total; pass `0` to manifest every
source file (356 recordings, about 65 minutes). Keep the generated manifest — a different source
selection produces a different artifact checksum. The builder validates licenses, attributions,
duplicate audio, complete-file splits and extractor identity, converts audio with ffmpeg and invokes
the native `cmd/wakeword-negatives` exporter, which writes the gzip `CNEG0002` file plus sidecar;
`--embed-previous-provenance <old>.provenance.json` carries prior lineage verbatim. Ship the sidecar
next to the binary.

`/data` is 78.5 MiB and the corpus is training input only, so it need not be resident: copy it in
only for a training session, or start the daemon with `-wakeword-negatives /tmp/negatives.bin` on
tmpfs. Never leave a partial `negatives.bin` — remove the old file, stage `.tmp`, then rename. The
daemon loads the corpus through the gzip path under the same 32 MiB logical bound.

## Offline training

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
`-negatives` must be the warm `CNEG0002` file; a cold corpus is rejected. Reported negative exposure
sums only original bodies, so borrowed preroll is not counted twice.

## Resource and operational limits

The RK3308 exposes 239,840 kB RAM and no swap; exhausting memory gets the daemon OOM-killed, so keep
the default 80 MiB Go soft heap limit (`-mem-limit-mb`) and let the boot service own the device
instead of running both stacks together. On-device augmentation is bounded, negative embeddings are
decoded once into float32 storage, and feature windows are views.

The hardware watchdog cannot be assumed disarmable: stop the daemon with TERM so it hands feeding to
its detached feeder, start the replacement explicitly under `nohup`, and verify the live RPC rather
than a background shell's exit status (see [HARDWARE.md](HARDWARE.md)).

Persistent space is the other hard limit: the daemon log rotates at 512 KiB with one copy, and new
recordings or training are refused below 2 MiB free, because a full `/data` breaks logging and the
upgrade path rather than failing one write. Each installed word is one artifact of roughly 130 KB, so
the eight-word bound is about 1 MB of `/data`; the shared frontend is compiled in, not stored per
word. `-wakeword-trace` logs per-word score peaks, the arbitration winner and detection heartbeats
and stays off in production. Relevant checks live in `internal/wakeword`,
`internal/wakeword/oww`, `internal/audio`, `internal/daemon` and `internal/webui`.