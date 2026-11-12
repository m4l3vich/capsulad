//! Binary PCM fast path for audio capture and playback.
//!
//! Format and conversation metadata travel once in the open control message;
//! sample frames are raw little-endian bytes with no JSON or base64 encoding.

use std::time::Duration;

use crate::call::Call;
use crate::client::Client;
use crate::error::Result;
use crate::sys::{self, PcmEvent as RawPcmEvent};
use crate::{check_status, status_error, take_buf, wait_ms};

/// Sample encoding used by the framework audio service.
#[repr(i32)]
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SampleFormat {
    Unspecified = 0,
    PcmS16Le = 1,
    PcmF32Le = 2,
}

/// Playback control command.
#[repr(i32)]
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PlaybackCommand {
    Unspecified = 0,
    Flush = 1,
    Pause = 2,
    Resume = 3,
}

/// Audio format sent in the open control message.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct PcmFormat {
    pub sample_format: SampleFormat,
    pub sample_rate_hz: u32,
    pub channels: u32,
}

impl PcmFormat {
    fn raw(self) -> sys::PcmFormat {
        sys::PcmFormat {
            sample_format: self.sample_format as i32,
            sample_rate_hz: self.sample_rate_hz,
            channels: self.channels,
            reserved: 0,
        }
    }
}

/// Kind of one capture event.
#[repr(i32)]
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PcmKind {
    Unknown = 0,
    Started = 1,
    Chunk = 2,
    Notice = 3,
}

impl PcmKind {
    fn from_raw(value: i32) -> Self {
        match value {
            sys::MEDIA_STARTED => Self::Started,
            sys::MEDIA_CHUNK => Self::Chunk,
            sys::MEDIA_NOTICE => Self::Notice,
            _ => Self::Unknown,
        }
    }
}

/// One decoded capture event. `pcm` is empty for Started/Notice; `json` is
/// empty for Chunk.
#[derive(Clone, Debug)]
pub struct PcmEvent {
    pub kind: PcmKind,
    pub sequence: u64,
    pub first_frame: u64,
    pub monotonic_time_ns: u64,
    pub frame_count: u32,
    pub discontinuity: bool,
    pub dropped_frames_before: u64,
    pub pcm: Vec<u8>,
    pub json: Vec<u8>,
}

impl Client {
    /// Opens a raw PCM capture stream (`AudioService/Capture`). The returned
    /// call is a server-streaming handle; use [`Call::capture_recv`].
    pub fn audio_capture_open(
        &self,
        endpoint_id: &str,
        format: PcmFormat,
        packet_ms: u32,
        activation_id: &str,
        conversation_id: &str,
    ) -> Result<Call> {
        let format = format.raw();
        let mut handle = 0u64;
        let status = unsafe {
            (self.inner.api.audio_capture_open)(
                self.handle(),
                endpoint_id.as_ptr().cast(),
                endpoint_id.len(),
                &format,
                packet_ms,
                activation_id.as_ptr().cast(),
                activation_id.len(),
                conversation_id.as_ptr().cast(),
                conversation_id.len(),
                &mut handle,
            )
        };
        check_status(&self.inner, status, "audio_capture_open")?;
        if handle == 0 {
            return Err(crate::error::Error::local(
                sys::STATUS_INTERNAL,
                "audio_capture_open returned a zero handle",
            ));
        }
        Ok(Call::from_handle(self.inner.clone(), handle))
    }

    /// Opens a raw PCM playback stream (`AudioService/Playback`). Read the
    /// `PlaybackStarted` event with `call.recv_json` before streaming audio.
    pub fn audio_playback_open(
        &self,
        endpoint_id: &str,
        format: PcmFormat,
        target_latency_ms: u32,
        conversation_id: &str,
    ) -> Result<Call> {
        let format = format.raw();
        let mut handle = 0u64;
        let status = unsafe {
            (self.inner.api.audio_playback_open)(
                self.handle(),
                endpoint_id.as_ptr().cast(),
                endpoint_id.len(),
                &format,
                target_latency_ms,
                conversation_id.as_ptr().cast(),
                conversation_id.len(),
                &mut handle,
            )
        };
        check_status(&self.inner, status, "audio_playback_open")?;
        if handle == 0 {
            return Err(crate::error::Error::local(
                sys::STATUS_INTERNAL,
                "audio_playback_open returned a zero handle",
            ));
        }
        Ok(Call::from_handle(self.inner.clone(), handle))
    }
}

impl Call {
    /// Receives one capture event. `None` is a clean end of stream; `TIMEOUT`
    /// is an error while the call remains open.
    pub fn capture_recv(&self, timeout: Option<Duration>) -> Result<Option<PcmEvent>> {
        let mut raw = RawPcmEvent::default();
        let status = unsafe {
            (self.inner.api.audio_capture_recv)(self.handle(), wait_ms(timeout), &mut raw)
        };
        match status {
            sys::STATUS_OK => Ok(Some(self.decode_pcm_event(raw))),
            sys::STATUS_END_OF_STREAM => {
                unsafe { take_buf(&self.inner, raw.pcm) };
                unsafe { take_buf(&self.inner, raw.json) };
                Ok(None)
            }
            _ => {
                let err = status_error(&self.inner, status, "audio_capture_recv");
                unsafe { take_buf(&self.inner, raw.pcm) };
                unsafe { take_buf(&self.inner, raw.json) };
                Err(err)
            }
        }
    }

    fn decode_pcm_event(&self, raw: RawPcmEvent) -> PcmEvent {
        let pcm = unsafe { take_buf(&self.inner, raw.pcm) };
        let json = unsafe { take_buf(&self.inner, raw.json) };
        PcmEvent {
            kind: PcmKind::from_raw(raw.kind),
            sequence: raw.sequence,
            first_frame: raw.first_frame,
            monotonic_time_ns: raw.monotonic_time_ns,
            frame_count: raw.frame_count,
            discontinuity: raw.discontinuity != 0,
            dropped_frames_before: raw.dropped_frames_before,
            pcm,
            json,
        }
    }

    /// Writes raw PCM frames to a playback call. `frame_count == 0` lets the
    /// library infer it from `pcm.len() / (channels * bytes_per_sample)`.
    pub fn playback_write(
        &self,
        pcm: &[u8],
        frame_count: u32,
        monotonic_time_ns: u64,
        discontinuity: bool,
        dropped_frames_before: u64,
    ) -> Result<()> {
        let status = unsafe {
            (self.inner.api.audio_playback_write)(
                self.handle(),
                pcm.as_ptr(),
                pcm.len(),
                frame_count,
                monotonic_time_ns,
                discontinuity as i32,
                dropped_frames_before,
            )
        };
        check_status(&self.inner, status, "audio_playback_write")
    }

    /// Sends FLUSH/PAUSE/RESUME to a playback call.
    pub fn playback_control(&self, command: PlaybackCommand) -> Result<()> {
        let status =
            unsafe { (self.inner.api.audio_playback_control)(self.handle(), command as i32) };
        check_status(&self.inner, status, "audio_playback_control")
    }
}
