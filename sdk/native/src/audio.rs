//! Audio binary fast path. PCM bytes never pass through JSON/base64.

use std::sync::Arc;

use prost::Message;

use crate::call::{start_direct, OutboundCall, CALLS};
use crate::client::ClientInner;
use crate::dynamic::{self, get_bool, get_bytes, get_message, get_u32, get_u64};
use crate::ffi::{
    self, Buf, PcmEvent, PcmFormat, SdkError, SdkResult, INVALID_ARGUMENT, INVALID_MESSAGE,
    NOT_FOUND,
};

pub const MEDIA_STARTED: i32 = 1;
pub const MEDIA_CHUNK: i32 = 2;
pub const MEDIA_NOTICE: i32 = 3;

fn bytes_per_sample(sample_format: i32) -> SdkResult<u32> {
    match sample_format {
        1 => Ok(2), // PCM_S16LE
        2 => Ok(4), // PCM_F32LE
        _ => Err(SdkError::new(
            INVALID_ARGUMENT,
            "sample_format must be PCM_S16LE (1) or PCM_F32LE (2)",
        )),
    }
}

fn validate_format(format: &PcmFormat) -> SdkResult<u32> {
    if format.reserved != 0 {
        return Err(SdkError::new(INVALID_ARGUMENT, "format.reserved must be 0"));
    }
    if format.sample_rate_hz == 0 || format.channels == 0 {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            "sample_rate_hz and channels must be non-zero",
        ));
    }
    bytes_per_sample(format.sample_format)
}

fn audio_format(format: &PcmFormat) -> SdkResult<prost_reflect::DynamicMessage> {
    let mut message = dynamic::new_msg("capsulad.framework.v1.AudioFormat")?;
    dynamic::set_enum(&mut message, "sample_format", format.sample_format);
    dynamic::set_u32(&mut message, "sample_rate_hz", format.sample_rate_hz);
    dynamic::set_u32(&mut message, "channels", format.channels);
    Ok(message)
}

fn outbound(handle: u64) -> SdkResult<Arc<OutboundCall>> {
    let inner = CALLS
        .get(handle)
        .ok_or_else(|| SdkError::new(NOT_FOUND, "unknown call handle"))?;
    inner
        .outbound()
        .ok_or_else(|| SdkError::new(INVALID_ARGUMENT, "call handle is not client-initiated"))
}

/* ------------------------------------------------------------------ *
 * Capture
 * ------------------------------------------------------------------ */

pub fn capture_open(
    client: Arc<ClientInner>,
    endpoint_id: &str,
    format: &PcmFormat,
    packet_ms: u32,
    activation_id: &str,
    conversation_id: &str,
) -> SdkResult<Arc<OutboundCall>> {
    validate_format(format)?;
    let mut request = dynamic::new_msg("capsulad.framework.v1.CaptureAudioRequest")?;
    dynamic::set_str(&mut request, "endpoint_id", endpoint_id);
    dynamic::set_message(&mut request, "preferred_format", audio_format(format)?);
    dynamic::set_u32(&mut request, "packet_ms", packet_ms);
    if !activation_id.is_empty() {
        dynamic::set_str(&mut request, "activation_id", activation_id);
    }
    if !conversation_id.is_empty() {
        dynamic::set_str(&mut request, "conversation_id", conversation_id);
    }
    let call = start_direct(
        client,
        "capsulad.framework.v1.AudioService",
        "Capture",
        1,
        None,
    )?;
    call.send_payload(request.encode_to_vec())?;
    call.half_close()?;
    Ok(call)
}

pub fn capture_recv(handle: u64, timeout_ms: u32) -> SdkResult<PcmEvent> {
    let call = outbound(handle)?;
    let bytes = call.recv(timeout_ms, false)?;
    let desc = dynamic::base_message("capsulad.framework.v1.CaptureAudioEvent")?;
    let event = prost_reflect::DynamicMessage::decode(desc, bytes.as_slice())
        .map_err(|e| SdkError::new(INVALID_MESSAGE, format!("capture event decode: {e}")))?;

    if let Some(started) = get_message(&event, "started") {
        let json = dynamic::dynamic_to_json(&started)?;
        return Ok(PcmEvent {
            kind: MEDIA_STARTED,
            json: ffi::alloc_json(json),
            ..Default::default()
        });
    }
    if let Some(notice) = get_message(&event, "notice") {
        let json = dynamic::dynamic_to_json(&notice)?;
        return Ok(PcmEvent {
            kind: MEDIA_NOTICE,
            json: ffi::alloc_json(json),
            ..Default::default()
        });
    }
    if let Some(chunk) = get_message(&event, "chunk") {
        let pcm = get_bytes(&chunk, "pcm");
        return Ok(PcmEvent {
            kind: MEDIA_CHUNK,
            discontinuity: u32::from(get_bool(&chunk, "discontinuity")),
            sequence: get_u64(&chunk, "sequence"),
            first_frame: get_u64(&chunk, "first_frame"),
            monotonic_time_ns: get_u64(&chunk, "monotonic_time_ns"),
            frame_count: get_u32(&chunk, "frame_count"),
            dropped_frames_before: get_u64(&chunk, "dropped_frames_before"),
            pcm: if pcm.is_empty() {
                Buf::empty()
            } else {
                ffi::alloc_buf(pcm)
            },
            json: Buf::empty(),
            ..Default::default()
        });
    }
    Err(SdkError::new(
        INVALID_MESSAGE,
        "CaptureAudioEvent has no started, chunk or notice payload",
    ))
}

/* ------------------------------------------------------------------ *
 * Playback
 * ------------------------------------------------------------------ */

pub fn playback_open(
    client: Arc<ClientInner>,
    endpoint_id: &str,
    format: &PcmFormat,
    target_latency_ms: u32,
    conversation_id: &str,
) -> SdkResult<Arc<OutboundCall>> {
    let bps = validate_format(format)?;
    let mut open = dynamic::new_msg("capsulad.framework.v1.OpenPlayback")?;
    dynamic::set_str(&mut open, "endpoint_id", endpoint_id);
    dynamic::set_message(&mut open, "format", audio_format(format)?);
    dynamic::set_u32(&mut open, "target_latency_ms", target_latency_ms);
    if !conversation_id.is_empty() {
        dynamic::set_str(&mut open, "conversation_id", conversation_id);
    }
    let mut request = dynamic::new_msg("capsulad.framework.v1.PlaybackRequest")?;
    dynamic::set_message(&mut request, "open", open);

    let call = start_direct(
        client,
        "capsulad.framework.v1.AudioService",
        "Playback",
        3,
        None,
    )?;
    call.set_playback_format(format.channels, bps);
    call.send_payload(request.encode_to_vec())?;
    Ok(call)
}

pub fn playback_write(
    handle: u64,
    pcm: &[u8],
    frame_count: u32,
    monotonic_time_ns: u64,
    discontinuity: bool,
    dropped_frames_before: u64,
) -> SdkResult<()> {
    let call = outbound(handle)?;
    let (channels, bps) = call.playback_format().ok_or_else(|| {
        SdkError::new(
            INVALID_ARGUMENT,
            "audio_playback_open must succeed before writing PCM",
        )
    })?;
    let bytes_per_frame = channels
        .checked_mul(bps)
        .ok_or_else(|| SdkError::new(INVALID_ARGUMENT, "format overflow"))?
        as usize;
    if bytes_per_frame == 0 || pcm.len() % bytes_per_frame != 0 {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            "pcm length is not a whole number of interleaved frames",
        ));
    }
    let derived = (pcm.len() / bytes_per_frame) as u32;
    if frame_count != 0 && frame_count != derived {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            format!("frame_count {frame_count} does not match {derived} frames in pcm"),
        ));
    }

    let mut chunk = dynamic::new_msg("capsulad.framework.v1.AudioChunk")?;
    dynamic::set_u64(&mut chunk, "sequence", call.next_sequence());
    dynamic::set_u64(
        &mut chunk,
        "monotonic_time_ns",
        if monotonic_time_ns == 0 {
            crate::runtime_monotonic_ns()
        } else {
            monotonic_time_ns
        },
    );
    dynamic::set_u32(&mut chunk, "frame_count", derived);
    dynamic::set_bytes(&mut chunk, "pcm", pcm.to_vec());
    dynamic::set_bool(&mut chunk, "discontinuity", discontinuity);
    dynamic::set_u64(&mut chunk, "dropped_frames_before", dropped_frames_before);

    let mut request = dynamic::new_msg("capsulad.framework.v1.PlaybackRequest")?;
    dynamic::set_message(&mut request, "chunk", chunk);
    call.send_payload(request.encode_to_vec())
}

pub fn playback_control(handle: u64, command: i32) -> SdkResult<()> {
    if !(1..=3).contains(&command) {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            "playback command must be FLUSH (1), PAUSE (2) or RESUME (3)",
        ));
    }
    let call = outbound(handle)?;
    let mut request = dynamic::new_msg("capsulad.framework.v1.PlaybackRequest")?;
    dynamic::set_enum(&mut request, "control", command);
    call.send_payload(request.encode_to_vec())
}

/* ------------------------------------------------------------------ *
 * Playback response helpers used by the API's generic recv_json path.
 * ------------------------------------------------------------------ */
