//! Daemon control channel used for listener allocation.
//!
//! The launcher passes one `AF_UNIX` `SOCK_SEQPACKET` descriptor per run in
//! `CAPSULAD_CONTROL_FD`. One request is one JSON message; the daemon answers
//! with one JSON message and, for `listen`, exactly one fd via `SCM_RIGHTS` in
//! the same `sendmsg`. Requests are serialized process-wide: the descriptor is
//! a single request/response stream and interleaving two requests would swap
//! their replies. `SOCK_DGRAM` is also accepted: platforms without AF_UNIX
//! `SEQPACKET` (macOS) still provide message boundaries and `SCM_RIGHTS`.
//!
//! The channel is policy, not transport: the daemon validates protocol, bind
//! address, port and backlog and is the only side that binds. A missing or
//! unusable channel is `UNSUPPORTED` (16), never a silent fallback.
//!
//! Every request carries a monotonically increasing numeric `id` that the
//! daemon must echo verbatim, including in error replies. A reply with a
//! different id belongs to an earlier request whose caller timed out: every
//! descriptor it carried is closed and the SDK keeps reading until the
//! matching reply or the deadline. A timeout therefore leaves one reply
//! queued, but the next request drains it instead of adopting its descriptor,
//! so the channel is recoverable by validation rather than by discarding the
//! connection. A reply that is not valid JSON is a protocol violation and
//! fails immediately (after closing received descriptors). A daemon that does
//! not echo `id` can never match, so requests against it time out instead of
//! returning another request's descriptor.

#[cfg(unix)]
mod unix {
    use std::mem::size_of;
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::sync::{LazyLock, Mutex};
    use std::time::{Duration, Instant};

    use serde_json::Value;

    use crate::ffi::{SdkError, SdkResult, INTERNAL, INVALID_ARGUMENT, UNSUPPORTED};

    pub const CONTROL_FD_ENV: &str = "CAPSULAD_CONTROL_FD";
    /// Bounded request/response payloads. A listener request is a few hundred
    /// bytes; this is generous while keeping one message small enough for any
    /// reasonable `SOCK_SEQPACKET` buffer.
    const MAX_REQUEST: usize = 16 * 1024;
    const MAX_RESPONSE: usize = 64 * 1024;
    /// Control budget when the caller passes 0 (the ABI's "default").
    pub const DEFAULT_TIMEOUT: Duration = Duration::from_secs(15);

    static CONTROL_LOCK: LazyLock<Mutex<()>> = LazyLock::new(|| Mutex::new(()));

    /// Monotonic request id; the daemon echoes it so a late reply from a
    /// timed-out request can never be mistaken for the current one.
    static NEXT_REQUEST_ID: AtomicU64 = AtomicU64::new(1);

    #[derive(Debug)]
    pub struct ControlReply {
        pub json: Value,
        /// The single received descriptor, if the reply carried one.
        pub fd: Option<std::os::fd::RawFd>,
    }

    /// Sends one request and returns the matching response JSON plus an
    /// optional fd.
    ///
    /// The request gains a numeric `id` the daemon must echo; replies with
    /// another id are stale, their descriptors are closed and the read
    /// continues until the matching reply or `timeout`. The fd is owned by
    /// the caller (CLOEXEC already set) and must be closed on every error path
    /// after this returns.
    pub fn request(value: &Value, timeout: Duration) -> SdkResult<ControlReply> {
        let fd = control_fd()?;
        let Some(object) = value.as_object() else {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                "control request must be a JSON object",
            ));
        };
        let id = NEXT_REQUEST_ID.fetch_add(1, Ordering::Relaxed);
        let mut request = object.clone();
        request.insert("id".to_string(), Value::from(id));
        let body = serde_json::to_vec(&Value::Object(request))
            .map_err(|err| SdkError::new(INVALID_ARGUMENT, format!("control JSON: {err}")))?;
        if body.len() > MAX_REQUEST {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                "control request exceeds the message bound",
            ));
        }
        let _serial = CONTROL_LOCK.lock().unwrap_or_else(|err| err.into_inner());
        // One budget covers send plus every drained stale reply.
        let deadline = Instant::now() + timeout;
        send_message(fd, &body, remaining(deadline)?)?;
        loop {
            let (response, received) = recv_message(fd, remaining(deadline)?)?;
            let json: Value = match serde_json::from_slice(&response) {
                Ok(json) => json,
                Err(err) => {
                    // Not a reply at all; never leak a descriptor that came
                    // with it.
                    for extra in received {
                        close_fd(extra);
                    }
                    return Err(SdkError::new(
                        INTERNAL,
                        format!("control response is not valid JSON: {err}"),
                    ));
                }
            };
            if json.get("id").and_then(Value::as_u64) != Some(id) {
                // Reply to an earlier request (its caller timed out). Its
                // descriptor must never reach this request.
                for extra in received {
                    close_fd(extra);
                }
                continue;
            }
            // An error reply carries no fd. Anything else must carry exactly
            // one; extra descriptors are closed immediately instead of being
            // leaked.
            let ok = json.get("ok").and_then(Value::as_bool).unwrap_or(true);
            if !ok {
                for extra in received {
                    close_fd(extra);
                }
                return Err(reply_error(&json));
            }
            if received.len() > 1 {
                for fd in received {
                    close_fd(fd);
                }
                return Err(SdkError::new(
                    INTERNAL,
                    "control response carried more than one file descriptor",
                ));
            }
            let Some(fd) = received.into_iter().next() else {
                return Err(SdkError::new(
                    INTERNAL,
                    "control response carried no file descriptor",
                ));
            };
            if let Err(err) = set_cloexec(fd) {
                close_fd(fd);
                return Err(err);
            }
            return Ok(ControlReply { json, fd: Some(fd) });
        }
    }

    fn remaining(deadline: Instant) -> SdkResult<Duration> {
        let left = deadline.saturating_duration_since(Instant::now());
        if left.is_zero() {
            return Err(SdkError::new(
                crate::ffi::TIMEOUT,
                "control channel timed out",
            ));
        }
        Ok(left)
    }

    fn reply_error(json: &Value) -> SdkError {
        let status = json
            .get("status")
            .or_else(|| json.get("code"))
            .and_then(Value::as_i64)
            .filter(|status| (0..=64).contains(status))
            .map(|status| status as i32)
            // A refusal without an explicit code is the daemon rejecting the
            // request (address/port/backlog policy or malformed options), so
            // INVALID_ARGUMENT is the honest classification; internal daemon
            // failures are expected to include their own status.
            .unwrap_or(INVALID_ARGUMENT);
        let message = json
            .get("message")
            .or_else(|| json.get("error"))
            .and_then(Value::as_str)
            .unwrap_or("control channel refused the request");
        SdkError::new(status, message)
    }

    /// Reads `CAPSULAD_CONTROL_FD` and proves it is a connected AF_UNIX
    /// SOCK_SEQPACKET descriptor. Every failure is UNSUPPORTED: the runtime
    /// simply did not provide a control channel.
    fn control_fd() -> SdkResult<libc::c_int> {
        let text = std::env::var(CONTROL_FD_ENV).map_err(|_| unsupported())?;
        let fd: libc::c_int = text.trim().parse().map_err(|_| unsupported())?;
        if fd < 0 {
            return Err(unsupported());
        }
        if unsafe { libc::fcntl(fd, libc::F_GETFD) } < 0 {
            return Err(unsupported());
        }
        let mut socket_type: libc::c_int = 0;
        let mut len = size_of::<libc::c_int>() as libc::socklen_t;
        let rc = unsafe {
            libc::getsockopt(
                fd,
                libc::SOL_SOCKET,
                libc::SO_TYPE,
                &mut socket_type as *mut _ as *mut libc::c_void,
                &mut len,
            )
        };
        if rc != 0 || (socket_type != libc::SOCK_SEQPACKET && socket_type != libc::SOCK_DGRAM) {
            return Err(unsupported());
        }
        let mut storage: libc::sockaddr_storage = unsafe { std::mem::zeroed() };
        let mut addr_len = size_of::<libc::sockaddr_storage>() as libc::socklen_t;
        let rc = unsafe {
            libc::getsockname(
                fd,
                &mut storage as *mut _ as *mut libc::sockaddr,
                &mut addr_len,
            )
        };
        if rc != 0 || storage.ss_family as libc::c_int != libc::AF_UNIX {
            return Err(unsupported());
        }
        // An inherited fd must already be connected to the daemon; a plain
        // bound socket would fail later with ENOTCONN. Classify that here as
        // "no usable channel" instead of a transport failure.
        let mut peer: libc::sockaddr_storage = unsafe { std::mem::zeroed() };
        let mut peer_len = size_of::<libc::sockaddr_storage>() as libc::socklen_t;
        let rc = unsafe {
            libc::getpeername(
                fd,
                &mut peer as *mut _ as *mut libc::sockaddr,
                &mut peer_len,
            )
        };
        if rc != 0 {
            return Err(unsupported());
        }
        Ok(fd)
    }

    fn unsupported() -> SdkError {
        SdkError::new(
            UNSUPPORTED,
            "no usable CAPSULAD_CONTROL_FD control channel; the runtime does not support listeners",
        )
    }

    fn poll_fd(fd: libc::c_int, events: libc::c_short, timeout: Duration) -> SdkResult<()> {
        let mut pfd = libc::pollfd {
            fd,
            events,
            revents: 0,
        };
        let ms = timeout.as_millis().min(i32::MAX as u128) as libc::c_int;
        loop {
            let rc = unsafe { libc::poll(&mut pfd, 1, ms) };
            if rc < 0 {
                if std::io::Error::last_os_error().kind() == std::io::ErrorKind::Interrupted {
                    continue;
                }
                return Err(SdkError::new(
                    crate::ffi::TRANSPORT,
                    format!("control poll: {}", std::io::Error::last_os_error()),
                ));
            }
            if rc == 0 {
                return Err(SdkError::new(
                    crate::ffi::TIMEOUT,
                    "control channel timed out",
                ));
            }
            return Ok(());
        }
    }

    fn send_message(fd: libc::c_int, body: &[u8], timeout: Duration) -> SdkResult<()> {
        poll_fd(fd, libc::POLLOUT, timeout)?;
        let mut iov = libc::iovec {
            iov_base: body.as_ptr() as *mut libc::c_void,
            iov_len: body.len(),
        };
        let msg = libc::msghdr {
            msg_name: std::ptr::null_mut(),
            msg_namelen: 0,
            msg_iov: &mut iov,
            msg_iovlen: 1,
            msg_control: std::ptr::null_mut(),
            msg_controllen: 0,
            msg_flags: 0,
        };
        loop {
            let sent = unsafe { libc::sendmsg(fd, &msg, libc::MSG_NOSIGNAL) };
            if sent >= 0 {
                if sent as usize != body.len() {
                    return Err(SdkError::new(
                        crate::ffi::TRANSPORT,
                        "control channel accepted a truncated request",
                    ));
                }
                return Ok(());
            }
            let err = std::io::Error::last_os_error();
            if err.kind() == std::io::ErrorKind::Interrupted {
                continue;
            }
            return Err(SdkError::new(
                crate::ffi::TRANSPORT,
                format!("control sendmsg: {err}"),
            ));
        }
    }

    fn recv_message(fd: libc::c_int, timeout: Duration) -> SdkResult<(Vec<u8>, Vec<libc::c_int>)> {
        poll_fd(fd, libc::POLLIN, timeout)?;
        let mut buf = vec![0u8; MAX_RESPONSE];
        // Room for one descriptor plus alignment; SCM_RIGHTS is the only
        // ancillary type accepted.
        let mut cell = [0u8; 128];
        let mut iov = libc::iovec {
            iov_base: buf.as_mut_ptr() as *mut libc::c_void,
            iov_len: buf.len(),
        };
        let mut msg: libc::msghdr = unsafe { std::mem::zeroed() };
        msg.msg_iov = &mut iov;
        msg.msg_iovlen = 1;
        msg.msg_control = cell.as_mut_ptr() as *mut libc::c_void;
        msg.msg_controllen = cell.len() as _;
        loop {
            let received = unsafe { libc::recvmsg(fd, &mut msg, 0) };
            if received >= 0 {
                if received as usize > MAX_RESPONSE {
                    return Err(SdkError::new(
                        INTERNAL,
                        "control response exceeds the message bound",
                    ));
                }
                buf.truncate(received as usize);
                if msg.msg_flags & libc::MSG_CTRUNC != 0 {
                    // The kernel closes SCM_RIGHTS descriptors that did not fit;
                    // parsing a truncated control buffer is unsafe.
                    return Err(SdkError::new(
                        INTERNAL,
                        "control response ancillary data was truncated",
                    ));
                }
                let mut fds = Vec::new();
                let mut cmsg = unsafe { libc::CMSG_FIRSTHDR(&msg) };
                while !cmsg.is_null() {
                    let header = unsafe { &*cmsg };
                    if header.cmsg_level == libc::SOL_SOCKET && header.cmsg_type == libc::SCM_RIGHTS
                    {
                        let payload = (header.cmsg_len as usize)
                            .saturating_sub(unsafe { libc::CMSG_LEN(0) } as usize);
                        let count = payload / size_of::<libc::c_int>();
                        let data = unsafe { libc::CMSG_DATA(cmsg) } as *const libc::c_int;
                        for index in 0..count {
                            fds.push(unsafe { *data.add(index) });
                        }
                    }
                    cmsg = unsafe { libc::CMSG_NXTHDR(&msg, cmsg) };
                }
                if msg.msg_flags & libc::MSG_TRUNC != 0 {
                    // The body was truncated, so this reply cannot be
                    // validated; never leak its descriptors.
                    for fd in fds {
                        close_fd(fd);
                    }
                    return Err(SdkError::new(
                        crate::ffi::TOO_LARGE,
                        "control response exceeds the message bound",
                    ));
                }
                return Ok((buf, fds));
            }
            let err = std::io::Error::last_os_error();
            if err.kind() == std::io::ErrorKind::Interrupted {
                continue;
            }
            return Err(SdkError::new(
                crate::ffi::TRANSPORT,
                format!("control recvmsg: {err}"),
            ));
        }
    }

    fn set_cloexec(fd: libc::c_int) -> SdkResult<()> {
        if unsafe { libc::fcntl(fd, libc::F_SETFD, libc::FD_CLOEXEC) } < 0 {
            return Err(SdkError::new(
                INTERNAL,
                format!(
                    "set control fd CLOEXEC: {}",
                    std::io::Error::last_os_error()
                ),
            ));
        }
        Ok(())
    }

    fn close_fd(fd: libc::c_int) {
        unsafe {
            libc::close(fd);
        }
    }

    /// Closes a descriptor returned to the listener layer after a later
    /// failure. Kept here so the unsafe block lives with the other fd code.
    pub fn close_received(fd: std::os::fd::RawFd) {
        close_fd(fd);
    }
}

#[cfg(unix)]
pub use unix::*;

#[cfg(not(unix))]
pub mod unix {
    use crate::ffi::{SdkError, SdkResult, UNSUPPORTED};
    use serde_json::Value;
    use std::time::Duration;

    pub const CONTROL_FD_ENV: &str = "CAPSULAD_CONTROL_FD";
    pub const DEFAULT_TIMEOUT: Duration = Duration::from_secs(15);

    #[derive(Debug)]
    pub struct ControlReply {
        pub json: Value,
        pub fd: Option<std::os::fd::RawFd>,
    }

    pub fn request(_value: &Value, _timeout: Duration) -> SdkResult<ControlReply> {
        Err(SdkError::new(
            UNSUPPORTED,
            "listeners require a Unix control channel",
        ))
    }
}
