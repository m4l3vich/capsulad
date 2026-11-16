/*
 * Capsulad native SDK - stable C ABI v1 (major 1, minor 0).
 *
 * The shared library is a pure Rust cdylib. It speaks the framework's gRPC
 * contract over the daemon's Unix domain socket and owns protobuf reflection
 * (JSON <-> protobuf) so every framework service is usable without generated
 * bindings. No language-owned object ever crosses this boundary: handles are
 * integers, buffers are library-allocated and freed through capsulad_buf_free.
 *
 * Threading: all entry points except the documented callbacks may be called
 * from any thread. Each handle is internally synchronized. A call handle has
 * one logical reader; concurrent recv_* calls on the same handle are
 * serialized. cancel/release/cloase may be called from another thread and
 * unblock a pending recv_*.
 *
 * Errors: every function returns an int32_t capsulad_status. On a non-OK
 * status, capsulad_last_error() returns a thread-local, NUL-terminated detail
 * string valid until the next SDK call on the same thread. GRPC failures also
 * expose the canonical code/detail through capsulad_call_status().
 *
 * Residency: first client_connect creates ONE bounded process-global Tokio
 * runtime (2 worker threads) that lives until process exit. There is no
 * shutdown/restart API; do not dlclose/unload this shared object while the
 * process lives or while any runtime thread may exist. Client/call/bus close
 * still releases their own resources; only the runtime and descriptor pool
 * are intentionally process-lifetime.
 */

#ifndef CAPSULAD_CAPSULAD_H
#define CAPSULAD_CAPSULAD_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#if defined(_WIN32)
#  define CAPSULAD_EXPORT __declspec(dllimport)
#else
#  define CAPSULAD_EXPORT __attribute__((visibility("default")))
#endif

#define CAPSULAD_ABI_MAJOR 1u
#define CAPSULAD_ABI_MINOR 0u
#define CAPSULAD_VERSION_STRING "1.0.0"
#define CAPSULAD_HANDLE_INVALID 0u
#define CAPSULAD_WAIT_FOREVER 0xFFFFFFFFu
#define CAPSULAD_DEFAULT_SOCKET "/run/capsulad/hal.sock"

/* Integer handles. 0 is always invalid. */
typedef uint64_t capsulad_handle;

typedef enum capsulad_status {
  CAPSULAD_STATUS_OK = 0,
  CAPSULAD_STATUS_INVALID_ARGUMENT = 1,
  CAPSULAD_STATUS_UNSUPPORTED_ABI = 2,
  CAPSULAD_STATUS_NOT_CONNECTED = 3,
  CAPSULAD_STATUS_CLOSED = 4,
  CAPSULAD_STATUS_DESCRIPTOR_NOT_FOUND = 5,
  CAPSULAD_STATUS_INVALID_MESSAGE = 6,
  CAPSULAD_STATUS_TRANSPORT = 7,
  CAPSULAD_STATUS_GRPC = 8,
  CAPSULAD_STATUS_END_OF_STREAM = 9,
  CAPSULAD_STATUS_TIMEOUT = 10,
  CAPSULAD_STATUS_CANCELLED = 11,
  CAPSULAD_STATUS_NOT_FOUND = 12,
  CAPSULAD_STATUS_BUSY = 13,
  CAPSULAD_STATUS_INTERNAL = 14,
  /* A bounded buffer (HTTP body, header/trailer block or chunk) exceeded its
   * cap. */
  CAPSULAD_STATUS_TOO_LARGE = 15
} capsulad_status;

/* Base64 alphabet/padding flags for capsulad_api.base64_encode/decode.
 * OR CAPSULAD_BASE64_NO_PADDING with an alphabet; STANDARD (0) is padded. */
enum {
  CAPSULAD_BASE64_STANDARD = 0,
  CAPSULAD_BASE64_URL_SAFE = 1,
  CAPSULAD_BASE64_NO_PADDING = 2
};

/* Direct framework call shape. The wire path is identical for all kinds;
 * the kind is a declaration of intent used by convenience helpers. */
typedef enum capsulad_call_kind {
  CAPSULAD_CALL_UNARY = 0,
  CAPSULAD_CALL_SERVER_STREAM = 1,
  CAPSULAD_CALL_CLIENT_STREAM = 2,
  CAPSULAD_CALL_BIDI = 3
} capsulad_call_kind;

/* AppBus inbound lifecycle event kinds delivered to capsulad_event_fn. */
typedef enum capsulad_event_kind {
  CAPSULAD_EVENT_UNKNOWN = 0,
  CAPSULAD_EVENT_INVOCATION = 1,     /* AppLaunchInvocation */
  CAPSULAD_EVENT_SETTINGS_CHANGED = 2, /* AppSettingsChanged */
  CAPSULAD_EVENT_ACTION = 3,         /* AppActionInvoked */
  CAPSULAD_EVENT_ACTIVATION = 4,     /* AssistantActivation */
  CAPSULAD_EVENT_TOUCH_RELEASED = 5  /* AssistantInputReleased */
} capsulad_event_kind;

/* Audio fast-path event kinds. */
typedef enum capsulad_media_kind {
  CAPSULAD_MEDIA_UNKNOWN = 0,
  CAPSULAD_MEDIA_STARTED = 1, /* json = CaptureAudioStarted */
  CAPSULAD_MEDIA_CHUNK = 2,   /* pcm = raw little-endian samples */
  CAPSULAD_MEDIA_NOTICE = 3   /* json = AudioNotice */
} capsulad_media_kind;

/* Mirrors capsulad.framework.v1.PlaybackCommand. */
typedef enum capsulad_playback_command {
  CAPSULAD_PLAYBACK_COMMAND_UNSPECIFIED = 0,
  CAPSULAD_PLAYBACK_FLUSH = 1,
  CAPSULAD_PLAYBACK_PAUSE = 2,
  CAPSULAD_PLAYBACK_RESUME = 3
} capsulad_playback_command;

/* Library-owned byte buffer. Free exactly once with capsulad_buf_free.
 * len excludes a library-added trailing NUL for UTF-8/JSON buffers. Empty
 * output is {data = NULL, len = 0}. The caller never writes into the buffer. */
typedef struct capsulad_buf {
  uint8_t *data;
  size_t len;
} capsulad_buf;

/* Mirrors capsulad.framework.v1.SampleFormat. */
enum {
  CAPSULAD_SAMPLE_FORMAT_UNSPECIFIED = 0,
  CAPSULAD_SAMPLE_FORMAT_PCM_S16LE = 1,
  CAPSULAD_SAMPLE_FORMAT_PCM_F32LE = 2
};

typedef struct capsulad_pcm_format {
  int32_t sample_format; /* CAPSULAD_SAMPLE_FORMAT_* */
  uint32_t sample_rate_hz;
  uint32_t channels;
  uint32_t reserved; /* must be 0 */
} capsulad_pcm_format;

/* One raw audio capture event. pcm is the unencoded sample payload (no
 * base64); started/notice metadata is canonical protobuf JSON in json.
 * Free both buffers with capsulad_buf_free. */
typedef struct capsulad_pcm_event {
  int32_t kind; /* capsulad_media_kind */
  uint32_t discontinuity;
  uint64_t sequence;
  uint64_t first_frame;
  uint64_t monotonic_time_ns;
  uint32_t frame_count;
  uint32_t reserved; /* 0 */
  uint64_t dropped_frames_before;
  capsulad_buf pcm;
  capsulad_buf json;
} capsulad_pcm_event;

/*
 * AppBus callbacks. All callbacks run on library-owned threads and must not
 * unwind across the FFI boundary. on_ready/on_disconnected/on_event are
 * serialized per bus. on_call runs on one dedicated thread per incoming call
 * so a handler may block in capsulad_call_recv_*.
 *
 * struct_size is the caller's table length. The library reads only bytes
 * inside it and treats missing trailing callbacks as NULL.
 * user_data is borrowed for the lifetime of the bus handle.
 */
typedef void (*capsulad_log_fn)(int32_t level, const char *message,
                               void *user_data);
typedef void (*capsulad_ready_fn)(capsulad_handle bus, void *user_data);
typedef void (*capsulad_disconnected_fn)(capsulad_handle bus, int32_t status,
                                        const char *detail, void *user_data);
typedef void (*capsulad_event_fn)(capsulad_handle bus, int32_t event_kind,
                                 const uint8_t *json, size_t json_len,
                                 void *user_data);
typedef void (*capsulad_call_fn)(capsulad_handle bus, capsulad_handle call,
                                const uint8_t *info_json, size_t info_len,
                                void *user_data);

typedef struct capsulad_bus_callbacks_v1 {
  size_t struct_size;
  void *user_data;
  capsulad_ready_fn on_ready;
  capsulad_disconnected_fn on_disconnected;
  capsulad_event_fn on_event;
  capsulad_call_fn on_call;
  capsulad_log_fn on_log;
} capsulad_bus_callbacks_v1;

/* --- function pointer types (frozen order in capsulad_api) ------------------ */

typedef int32_t (*capsulad_fn_client_connect)(
    const char *socket_path, size_t socket_path_len,
    const char *run_id, size_t run_id_len,
    const char *app_id, size_t app_id_len,
    uint32_t timeout_ms, capsulad_handle *out_client);
typedef int32_t (*capsulad_fn_client_close)(capsulad_handle client);
typedef int32_t (*capsulad_fn_client_register_descriptors)(
    capsulad_handle client, const uint8_t *file_descriptor_set, size_t len);
typedef int32_t (*capsulad_fn_client_info)(capsulad_handle client,
                                          capsulad_buf *out_json);
typedef int32_t (*capsulad_fn_client_unary)(
    capsulad_handle client,
    const char *service, size_t service_len,
    const char *method, size_t method_len,
    const uint8_t *request_json, size_t request_len,
    uint32_t timeout_ms, capsulad_buf *out_json);
typedef int32_t (*capsulad_fn_client_unary_proto)(
    capsulad_handle client,
    const char *service, size_t service_len,
    const char *method, size_t method_len,
    const uint8_t *request_proto, size_t request_len,
    uint32_t timeout_ms, capsulad_buf *out_proto);
typedef int32_t (*capsulad_fn_call_start)(
    capsulad_handle client,
    const char *service, size_t service_len,
    const char *method, size_t method_len,
    int32_t kind, uint32_t timeout_ms, capsulad_handle *out_call);
typedef int32_t (*capsulad_fn_call_start_routed)(
    capsulad_handle client,
    const char *target_app_id, size_t target_app_id_len,
    const char *service, size_t service_len,
    const char *method, size_t method_len,
    uint32_t timeout_ms, capsulad_handle *out_call);
typedef int32_t (*capsulad_fn_call_send_json)(capsulad_handle call,
                                             const uint8_t *json, size_t len);
typedef int32_t (*capsulad_fn_call_send_proto)(capsulad_handle call,
                                              const uint8_t *proto, size_t len);
typedef int32_t (*capsulad_fn_call_half_close)(capsulad_handle call);
typedef int32_t (*capsulad_fn_call_recv_json)(capsulad_handle call,
                                             uint32_t timeout_ms,
                                             capsulad_buf *out_json);
typedef int32_t (*capsulad_fn_call_recv_proto)(capsulad_handle call,
                                              uint32_t timeout_ms,
                                              capsulad_buf *out_proto);
typedef int32_t (*capsulad_fn_call_end)(capsulad_handle call, int32_t grpc_code,
                                       const char *detail, size_t detail_len);
typedef int32_t (*capsulad_fn_call_cancel)(capsulad_handle call);
typedef int32_t (*capsulad_fn_call_retain)(capsulad_handle call);
typedef int32_t (*capsulad_fn_call_release)(capsulad_handle call);
typedef int32_t (*capsulad_fn_call_info)(capsulad_handle call,
                                        capsulad_buf *out_json);
typedef int32_t (*capsulad_fn_call_status)(capsulad_handle call,
                                          int32_t *out_grpc_code,
                                          capsulad_buf *out_detail);
typedef int32_t (*capsulad_fn_bus_open)(
    capsulad_handle client, const capsulad_bus_callbacks_v1 *callbacks,
    capsulad_handle *out_bus);
typedef int32_t (*capsulad_fn_bus_close)(capsulad_handle bus);
typedef int32_t (*capsulad_fn_bus_ready)(capsulad_handle bus,
                                        uint32_t timeout_ms);
typedef int32_t (*capsulad_fn_bus_notify_invocation)(
    capsulad_handle bus,
    const char *invocation_id, size_t invocation_id_len,
    int32_t accepted, const char *detail, size_t detail_len);
typedef int32_t (*capsulad_fn_bus_notify_action)(
    capsulad_handle bus,
    const char *action_invocation_id, size_t action_invocation_id_len,
    int32_t succeeded, const char *detail, size_t detail_len);
typedef int32_t (*capsulad_fn_audio_capture_open)(
    capsulad_handle client,
    const char *endpoint_id, size_t endpoint_id_len,
    const capsulad_pcm_format *format, uint32_t packet_ms,
    const char *activation_id, size_t activation_id_len,
    const char *conversation_id, size_t conversation_id_len,
    capsulad_handle *out_call);
typedef int32_t (*capsulad_fn_audio_capture_recv)(
    capsulad_handle call, uint32_t timeout_ms, capsulad_pcm_event *out_event);
typedef int32_t (*capsulad_fn_audio_playback_open)(
    capsulad_handle client,
    const char *endpoint_id, size_t endpoint_id_len,
    const capsulad_pcm_format *format, uint32_t target_latency_ms,
    const char *conversation_id, size_t conversation_id_len,
    capsulad_handle *out_call);
typedef int32_t (*capsulad_fn_audio_playback_write)(
    capsulad_handle call, const uint8_t *pcm, size_t pcm_len,
    uint32_t frame_count, uint64_t monotonic_time_ns,
    int32_t discontinuity, uint64_t dropped_frames_before);
typedef int32_t (*capsulad_fn_audio_playback_control)(capsulad_handle call,
                                                     int32_t command);
typedef int32_t (*capsulad_fn_buf_free)(capsulad_buf buf);
typedef const char *(*capsulad_fn_last_error)(void);
typedef int32_t (*capsulad_fn_handle_info)(capsulad_handle handle,
                                          capsulad_buf *out_json);

/* --- appended v1 surface: HTTP, remote gRPC, primitives ------------------- */

/* Opens an HTTP/1.1 client. options_json is snake_case; unknown keys are
 * rejected. Known keys: roots ("system" default | "webpki" | "pem"),
 * ca_pem, user_agent, max_body (default 32 MiB, hard cap 64 MiB),
 * follow_redirects (default 4, 0 disables), connect_timeout_ms (default
 * 15000), read_timeout_ms (default 30000, applied to every individual read and
 * write), total_timeout_ms (default 120000, bounds the whole exchange including
 * every redirect hop; 0 selects the default, never "unlimited"). TLS always
 * verifies; there is no insecure option. "pem" trusts only ca_pem: no system
 * bundle and no public roots are loaded, and ca_pem must contain at least one
 * CERTIFICATE or the open fails. timeout_ms is accepted for symmetry and
 * ignored (opening only reads CA files). */
typedef int32_t (*capsulad_fn_http_open)(
    const uint8_t *options_json, size_t options_len, uint32_t timeout_ms,
    capsulad_handle *out_http);
/* Closes the client: cancels every in-flight request, then releases the handle.
 * The call waits until no request can still be using the TLS/root state, unless
 * it is made on a Tokio runtime thread, where it returns without blocking
 * (in-flight requests hold Arc snapshots, so that state stays valid). Either
 * way the handle is unusable when the call returns. */
typedef int32_t (*capsulad_fn_http_close)(capsulad_handle http);

/* One HTTP/1.1 request. Absolute http(s) URLs only; no userinfo or fragment.
 * headers_json is a JSON object of string values; names are lowercased and
 * host/content-length/transfer-encoding/connection are rejected. Returns the
 * status code, a lowercase-name JSON header map (last value wins) and the body
 * exactly as sent (no automatic decompression). The response header block must
 * be ASCII: names must be RFC 7230 tokens and values may contain visible ASCII
 * (0x20..0x7e) plus HTAB; any other byte (obs-text, C1 controls, CR/LF/NUL)
 * fails with TRANSPORT.
 *
 * Redirects are bounded by follow_redirects. 301/302/303 keep HEAD as HEAD and
 * turn every other method into a bodiless GET; 307/308 preserve method and
 * body. An https -> http redirect is refused with TRANSPORT (no cleartext
 * downgrade of a verified channel). At most 8 interim 1xx responses are
 * tolerated, and chunked trailers are bounded by count and total bytes.
 *
 * timeout_ms bounds the whole exchange, redirects included. 0 uses the
 * client's total_timeout_ms; it never means "no deadline".
 *
 * request_id must be non-zero and unique for the lifetime of the client: an id
 * is never reused, not even after its request returns. Zero, or an id that is
 * already in flight, is INVALID_ARGUMENT. The same id passed to http_cancel
 * aborts just this exchange. */
typedef int32_t (*capsulad_fn_http_request)(
    capsulad_handle http, uint64_t request_id,
    const char *method, size_t method_len,
    const char *url, size_t url_len,
    const uint8_t *headers_json, size_t headers_len,
    const uint8_t *body, size_t body_len, uint32_t timeout_ms,
    int32_t *out_status_code, capsulad_buf *out_headers_json,
    capsulad_buf *out_body);

/* Opens a remote gRPC client handle. url is an origin only
 * (https://host[:port] | http://host[:port]); https uses TLS with ALPN h2 and
 * normal verification, http uses h2c prior knowledge. Options: roots, ca_pem,
 * server_name, connect_timeout_ms (default 10000). The returned handle is a
 * normal client handle: client_close/info/register_descriptors/unary and
 * call_start* work; bus_open and call_start_routed fail with
 * CAPSULAD_STATUS_INVALID_ARGUMENT because a remote channel has no app bus. */
typedef int32_t (*capsulad_fn_grpc_open)(
    const char *url, size_t url_len, const uint8_t *options_json,
    size_t options_len, uint32_t timeout_ms, capsulad_handle *out_client);

/* Starts a call with caller metadata: headers_json is a JSON string map whose
 * keys are lowercased and validated as ASCII gRPC metadata (no CR/LF, no
 * -bin). Names owned by HTTP/2 or gRPC (content-type, te, trailer,
 * grpc-timeout, grpc-encoding, grpc-accept-encoding, user-agent, host) and the
 * SDK's own capsulad-run-id are rejected with INVALID_ARGUMENT. The metadata is
 * applied to the call's request head. */
typedef int32_t (*capsulad_fn_call_start_meta)(
    capsulad_handle client, const char *service, size_t service_len,
    const char *method, size_t method_len, int32_t kind, uint32_t timeout_ms,
    const uint8_t *headers_json, size_t headers_len, capsulad_handle *out_call);

/* Strict base64 (standard or URL-safe, padded or unpadded). Decode rejects
 * non-canonical input. flags are CAPSULAD_BASE64_*. */
typedef int32_t (*capsulad_fn_base64_encode)(const uint8_t *data, size_t len,
                                             int32_t flags, capsulad_buf *out);
typedef int32_t (*capsulad_fn_base64_decode)(const uint8_t *data, size_t len,
                                             int32_t flags, capsulad_buf *out);
/* RFC 3986: encode leaves A-Z a-z 0-9 - . _ ~; decode rejects malformed %. */
typedef int32_t (*capsulad_fn_percent_encode)(const uint8_t *data, size_t len,
                                              capsulad_buf *out);
typedef int32_t (*capsulad_fn_percent_decode)(const uint8_t *data, size_t len,
                                              capsulad_buf *out);
/* Raw 32-byte digest, getrandom bytes, lowercase RFC 4122 v4 UUID. */
typedef int32_t (*capsulad_fn_sha256)(const uint8_t *data, size_t len,
                                      capsulad_buf *out);
typedef int32_t (*capsulad_fn_random_bytes)(uint8_t *out, size_t len);
typedef int32_t (*capsulad_fn_uuid_v4)(capsulad_buf *out);
/* Absolute http(s) URL -> {"scheme","host","port","path","query"}, where port
 * is the effective TCP port (explicit or 80/443). */
typedef int32_t (*capsulad_fn_url_parse)(const char *url, size_t len,
                                         capsulad_buf *out_json);
/* Aborts the in-flight request with this id. The interrupted http_request
 * returns CAPSULAD_STATUS_CANCELLED; other requests are unaffected.
 * Cancellation is sticky across the registration race: an id with no request
 * yet is remembered (bounded to 64 ids, oldest evicted) and the matching
 * http_request then fails with CAPSULAD_STATUS_CANCELLED before any network
 * I/O. An id whose request already completed is a successful no-op: it neither
 * arms a later cancellation nor affects any other request. Ids are unique for
 * the client lifetime, so a completed id is never used again. Id 0 is a no-op. */
typedef int32_t (*capsulad_fn_http_cancel)(capsulad_handle http,
                                           uint64_t request_id);

/*
 * v1 function table. Frozen layout: fields are only appended in a future
 * minor version, never reordered or removed. struct_size lets bindings
 * detect appended fields.
 */
typedef struct capsulad_api_v1 {
  uint32_t abi_major;
  uint32_t abi_minor;
  size_t struct_size;

  capsulad_fn_client_connect client_connect;
  capsulad_fn_client_close client_close;
  capsulad_fn_client_register_descriptors client_register_descriptors;
  capsulad_fn_client_info client_info;
  capsulad_fn_client_unary client_unary;
  capsulad_fn_client_unary_proto client_unary_proto;

  capsulad_fn_call_start call_start;
  capsulad_fn_call_start_routed call_start_routed;
  capsulad_fn_call_send_json call_send_json;
  capsulad_fn_call_send_proto call_send_proto;
  capsulad_fn_call_half_close call_half_close;
  capsulad_fn_call_recv_json call_recv_json;
  capsulad_fn_call_recv_proto call_recv_proto;
  capsulad_fn_call_end call_end;
  capsulad_fn_call_cancel call_cancel;
  capsulad_fn_call_retain call_retain;
  capsulad_fn_call_release call_release;
  capsulad_fn_call_info call_info;
  capsulad_fn_call_status call_status;

  capsulad_fn_bus_open bus_open;
  capsulad_fn_bus_close bus_close;
  capsulad_fn_bus_ready bus_ready;
  capsulad_fn_bus_notify_invocation bus_notify_invocation;
  capsulad_fn_bus_notify_action bus_notify_action;

  capsulad_fn_audio_capture_open audio_capture_open;
  capsulad_fn_audio_capture_recv audio_capture_recv;
  capsulad_fn_audio_playback_open audio_playback_open;
  capsulad_fn_audio_playback_write audio_playback_write;
  capsulad_fn_audio_playback_control audio_playback_control;

  capsulad_fn_buf_free buf_free;
  capsulad_fn_last_error last_error;
  capsulad_fn_handle_info handle_info;

  /* Appended in v1: HTTP, remote gRPC and primitives. */
  capsulad_fn_http_open http_open;
  capsulad_fn_http_close http_close;
  capsulad_fn_http_request http_request;
  capsulad_fn_grpc_open grpc_open;
  capsulad_fn_call_start_meta call_start_meta;
  capsulad_fn_base64_encode base64_encode;
  capsulad_fn_base64_decode base64_decode;
  capsulad_fn_percent_encode percent_encode;
  capsulad_fn_percent_decode percent_decode;
  capsulad_fn_sha256 sha256;
  capsulad_fn_random_bytes random_bytes;
  capsulad_fn_uuid_v4 uuid_v4;
  capsulad_fn_url_parse url_parse;
  capsulad_fn_http_cancel http_cancel;
} capsulad_api;

/* Stable v1 prefix: abi_major, abi_minor, struct_size. Any v1 table at least
 * this large is accepted by capsulad_get_api, including tables built against an
 * older minor with fewer function pointers. */
#define CAPSULAD_API_V1_MIN_SIZE ((size_t)offsetof(capsulad_api, client_connect))

/* --- exported entry points ------------------------------------------------ */

/* Returns sizeof(capsulad_api) for the calling ABI. */
CAPSULAD_EXPORT size_t capsulad_api_size(void);

/* Compile-time ABI identity of the library. */
CAPSULAD_EXPORT uint32_t capsulad_abi_major(void);
CAPSULAD_EXPORT uint32_t capsulad_abi_minor(void);
CAPSULAD_EXPORT const char *capsulad_version(void);

/* Read-only process-wide table. Equivalent to a fully populated
 * capsulad_get_api() result; do not free. */
CAPSULAD_EXPORT const capsulad_api *capsulad_api_v1(void);

/*
 * Copy the v1 table into caller memory.
 * - abi_major must be 1, otherwise CAPSULAD_STATUS_UNSUPPORTED_ABI with no write.
 * - out must be non-NULL and table_size >= CAPSULAD_API_V1_MIN_SIZE (the stable
 *   prefix through struct_size), otherwise CAPSULAD_STATUS_INVALID_ARGUMENT
 *   with no write. Tables built against a smaller v1 minor remain valid.
 * - Copies exactly min(table_size, capsulad_api_size()) bytes; never writes past
 *   table_size. `out->struct_size` reports the library's current full table
 *   size; keep the caller's table zero-initialized and require every field you
 *   call to be within that size and non-NULL.
 */
CAPSULAD_EXPORT int32_t capsulad_get_api(uint32_t abi_major, size_t table_size,
                                       capsulad_api *out);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* CAPSULAD_CAPSULAD_H */
