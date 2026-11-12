/* Code generated from sdk/native/include/capsula/capsula.h by sdk/go/regenerate-abi.sh. DO NOT EDIT. */
/*
 * Capsula native SDK - stable C ABI v1 (major 1, minor 0).
 *
 * The shared library is a pure Rust cdylib. It speaks the framework's gRPC
 * contract over the daemon's Unix domain socket and owns protobuf reflection
 * (JSON <-> protobuf) so every framework service is usable without generated
 * bindings. No language-owned object ever crosses this boundary: handles are
 * integers, buffers are library-allocated and freed through capsula_buf_free.
 *
 * Threading: all entry points except the documented callbacks may be called
 * from any thread. Each handle is internally synchronized. A call handle has
 * one logical reader; concurrent recv_* calls on the same handle are
 * serialized. cancel/release/cloase may be called from another thread and
 * unblock a pending recv_*.
 *
 * Errors: every function returns an int32_t capsula_status. On a non-OK
 * status, capsula_last_error() returns a thread-local, NUL-terminated detail
 * string valid until the next SDK call on the same thread. GRPC failures also
 * expose the canonical code/detail through capsula_call_status().
 *
 * Residency: first client_connect creates ONE bounded process-global Tokio
 * runtime (2 worker threads) that lives until process exit. There is no
 * shutdown/restart API; do not dlclose/unload this shared object while the
 * process lives or while any runtime thread may exist. Client/call/bus close
 * still releases their own resources; only the runtime and descriptor pool
 * are intentionally process-lifetime.
 */

#ifndef CAPSULA_CAPSULA_H
#define CAPSULA_CAPSULA_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#if defined(_WIN32)
#  define CAPSULA_EXPORT __declspec(dllimport)
#else
#  define CAPSULA_EXPORT __attribute__((visibility("default")))
#endif

#define CAPSULA_ABI_MAJOR 1u
#define CAPSULA_ABI_MINOR 0u
#define CAPSULA_VERSION_STRING "1.0.0"
#define CAPSULA_HANDLE_INVALID 0u
#define CAPSULA_WAIT_FOREVER 0xFFFFFFFFu
#define CAPSULA_DEFAULT_SOCKET "/run/capsulad/hal.sock"

/* Integer handles. 0 is always invalid. */
typedef uint64_t capsula_handle;

typedef enum capsula_status {
  CAPSULA_STATUS_OK = 0,
  CAPSULA_STATUS_INVALID_ARGUMENT = 1,
  CAPSULA_STATUS_UNSUPPORTED_ABI = 2,
  CAPSULA_STATUS_NOT_CONNECTED = 3,
  CAPSULA_STATUS_CLOSED = 4,
  CAPSULA_STATUS_DESCRIPTOR_NOT_FOUND = 5,
  CAPSULA_STATUS_INVALID_MESSAGE = 6,
  CAPSULA_STATUS_TRANSPORT = 7,
  CAPSULA_STATUS_GRPC = 8,
  CAPSULA_STATUS_END_OF_STREAM = 9,
  CAPSULA_STATUS_TIMEOUT = 10,
  CAPSULA_STATUS_CANCELLED = 11,
  CAPSULA_STATUS_NOT_FOUND = 12,
  CAPSULA_STATUS_BUSY = 13,
  CAPSULA_STATUS_INTERNAL = 14
} capsula_status;

/* Direct framework call shape. The wire path is identical for all kinds;
 * the kind is a declaration of intent used by convenience helpers. */
typedef enum capsula_call_kind {
  CAPSULA_CALL_UNARY = 0,
  CAPSULA_CALL_SERVER_STREAM = 1,
  CAPSULA_CALL_CLIENT_STREAM = 2,
  CAPSULA_CALL_BIDI = 3
} capsula_call_kind;

/* AppBus inbound lifecycle event kinds delivered to capsula_event_fn. */
typedef enum capsula_event_kind {
  CAPSULA_EVENT_UNKNOWN = 0,
  CAPSULA_EVENT_INVOCATION = 1,     /* AppLaunchInvocation */
  CAPSULA_EVENT_SETTINGS_CHANGED = 2, /* AppSettingsChanged */
  CAPSULA_EVENT_ACTION = 3,         /* AppActionInvoked */
  CAPSULA_EVENT_ACTIVATION = 4,     /* AssistantActivation */
  CAPSULA_EVENT_TOUCH_RELEASED = 5  /* AssistantInputReleased */
} capsula_event_kind;

/* Audio fast-path event kinds. */
typedef enum capsula_media_kind {
  CAPSULA_MEDIA_UNKNOWN = 0,
  CAPSULA_MEDIA_STARTED = 1, /* json = CaptureAudioStarted */
  CAPSULA_MEDIA_CHUNK = 2,   /* pcm = raw little-endian samples */
  CAPSULA_MEDIA_NOTICE = 3   /* json = AudioNotice */
} capsula_media_kind;

/* Mirrors capsula.framework.v1.PlaybackCommand. */
typedef enum capsula_playback_command {
  CAPSULA_PLAYBACK_COMMAND_UNSPECIFIED = 0,
  CAPSULA_PLAYBACK_FLUSH = 1,
  CAPSULA_PLAYBACK_PAUSE = 2,
  CAPSULA_PLAYBACK_RESUME = 3
} capsula_playback_command;

/* Library-owned byte buffer. Free exactly once with capsula_buf_free.
 * len excludes a library-added trailing NUL for UTF-8/JSON buffers. Empty
 * output is {data = NULL, len = 0}. The caller never writes into the buffer. */
typedef struct capsula_buf {
  uint8_t *data;
  size_t len;
} capsula_buf;

/* Mirrors capsula.framework.v1.SampleFormat. */
enum {
  CAPSULA_SAMPLE_FORMAT_UNSPECIFIED = 0,
  CAPSULA_SAMPLE_FORMAT_PCM_S16LE = 1,
  CAPSULA_SAMPLE_FORMAT_PCM_F32LE = 2
};

typedef struct capsula_pcm_format {
  int32_t sample_format; /* CAPSULA_SAMPLE_FORMAT_* */
  uint32_t sample_rate_hz;
  uint32_t channels;
  uint32_t reserved; /* must be 0 */
} capsula_pcm_format;

/* One raw audio capture event. pcm is the unencoded sample payload (no
 * base64); started/notice metadata is canonical protobuf JSON in json.
 * Free both buffers with capsula_buf_free. */
typedef struct capsula_pcm_event {
  int32_t kind; /* capsula_media_kind */
  uint32_t discontinuity;
  uint64_t sequence;
  uint64_t first_frame;
  uint64_t monotonic_time_ns;
  uint32_t frame_count;
  uint32_t reserved; /* 0 */
  uint64_t dropped_frames_before;
  capsula_buf pcm;
  capsula_buf json;
} capsula_pcm_event;

/*
 * AppBus callbacks. All callbacks run on library-owned threads and must not
 * unwind across the FFI boundary. on_ready/on_disconnected/on_event are
 * serialized per bus. on_call runs on one dedicated thread per incoming call
 * so a handler may block in capsula_call_recv_*.
 *
 * struct_size is the caller's table length. The library reads only bytes
 * inside it and treats missing trailing callbacks as NULL.
 * user_data is borrowed for the lifetime of the bus handle.
 */
typedef void (*capsula_log_fn)(int32_t level, const char *message,
                               void *user_data);
typedef void (*capsula_ready_fn)(capsula_handle bus, void *user_data);
typedef void (*capsula_disconnected_fn)(capsula_handle bus, int32_t status,
                                        const char *detail, void *user_data);
typedef void (*capsula_event_fn)(capsula_handle bus, int32_t event_kind,
                                 const uint8_t *json, size_t json_len,
                                 void *user_data);
typedef void (*capsula_call_fn)(capsula_handle bus, capsula_handle call,
                                const uint8_t *info_json, size_t info_len,
                                void *user_data);

typedef struct capsula_bus_callbacks_v1 {
  size_t struct_size;
  void *user_data;
  capsula_ready_fn on_ready;
  capsula_disconnected_fn on_disconnected;
  capsula_event_fn on_event;
  capsula_call_fn on_call;
  capsula_log_fn on_log;
} capsula_bus_callbacks_v1;

/* --- function pointer types (frozen order in capsula_api) ------------------ */

typedef int32_t (*capsula_fn_client_connect)(
    const char *socket_path, size_t socket_path_len,
    const char *run_id, size_t run_id_len,
    const char *app_id, size_t app_id_len,
    uint32_t timeout_ms, capsula_handle *out_client);
typedef int32_t (*capsula_fn_client_close)(capsula_handle client);
typedef int32_t (*capsula_fn_client_register_descriptors)(
    capsula_handle client, const uint8_t *file_descriptor_set, size_t len);
typedef int32_t (*capsula_fn_client_info)(capsula_handle client,
                                          capsula_buf *out_json);
typedef int32_t (*capsula_fn_client_unary)(
    capsula_handle client,
    const char *service, size_t service_len,
    const char *method, size_t method_len,
    const uint8_t *request_json, size_t request_len,
    uint32_t timeout_ms, capsula_buf *out_json);
typedef int32_t (*capsula_fn_client_unary_proto)(
    capsula_handle client,
    const char *service, size_t service_len,
    const char *method, size_t method_len,
    const uint8_t *request_proto, size_t request_len,
    uint32_t timeout_ms, capsula_buf *out_proto);
typedef int32_t (*capsula_fn_call_start)(
    capsula_handle client,
    const char *service, size_t service_len,
    const char *method, size_t method_len,
    int32_t kind, uint32_t timeout_ms, capsula_handle *out_call);
typedef int32_t (*capsula_fn_call_start_routed)(
    capsula_handle client,
    const char *target_app_id, size_t target_app_id_len,
    const char *service, size_t service_len,
    const char *method, size_t method_len,
    uint32_t timeout_ms, capsula_handle *out_call);
typedef int32_t (*capsula_fn_call_send_json)(capsula_handle call,
                                             const uint8_t *json, size_t len);
typedef int32_t (*capsula_fn_call_send_proto)(capsula_handle call,
                                              const uint8_t *proto, size_t len);
typedef int32_t (*capsula_fn_call_half_close)(capsula_handle call);
typedef int32_t (*capsula_fn_call_recv_json)(capsula_handle call,
                                             uint32_t timeout_ms,
                                             capsula_buf *out_json);
typedef int32_t (*capsula_fn_call_recv_proto)(capsula_handle call,
                                              uint32_t timeout_ms,
                                              capsula_buf *out_proto);
typedef int32_t (*capsula_fn_call_end)(capsula_handle call, int32_t grpc_code,
                                       const char *detail, size_t detail_len);
typedef int32_t (*capsula_fn_call_cancel)(capsula_handle call);
typedef int32_t (*capsula_fn_call_retain)(capsula_handle call);
typedef int32_t (*capsula_fn_call_release)(capsula_handle call);
typedef int32_t (*capsula_fn_call_info)(capsula_handle call,
                                        capsula_buf *out_json);
typedef int32_t (*capsula_fn_call_status)(capsula_handle call,
                                          int32_t *out_grpc_code,
                                          capsula_buf *out_detail);
typedef int32_t (*capsula_fn_bus_open)(
    capsula_handle client, const capsula_bus_callbacks_v1 *callbacks,
    capsula_handle *out_bus);
typedef int32_t (*capsula_fn_bus_close)(capsula_handle bus);
typedef int32_t (*capsula_fn_bus_ready)(capsula_handle bus,
                                        uint32_t timeout_ms);
typedef int32_t (*capsula_fn_bus_notify_invocation)(
    capsula_handle bus,
    const char *invocation_id, size_t invocation_id_len,
    int32_t accepted, const char *detail, size_t detail_len);
typedef int32_t (*capsula_fn_bus_notify_action)(
    capsula_handle bus,
    const char *action_invocation_id, size_t action_invocation_id_len,
    int32_t succeeded, const char *detail, size_t detail_len);
typedef int32_t (*capsula_fn_audio_capture_open)(
    capsula_handle client,
    const char *endpoint_id, size_t endpoint_id_len,
    const capsula_pcm_format *format, uint32_t packet_ms,
    const char *activation_id, size_t activation_id_len,
    const char *conversation_id, size_t conversation_id_len,
    capsula_handle *out_call);
typedef int32_t (*capsula_fn_audio_capture_recv)(
    capsula_handle call, uint32_t timeout_ms, capsula_pcm_event *out_event);
typedef int32_t (*capsula_fn_audio_playback_open)(
    capsula_handle client,
    const char *endpoint_id, size_t endpoint_id_len,
    const capsula_pcm_format *format, uint32_t target_latency_ms,
    const char *conversation_id, size_t conversation_id_len,
    capsula_handle *out_call);
typedef int32_t (*capsula_fn_audio_playback_write)(
    capsula_handle call, const uint8_t *pcm, size_t pcm_len,
    uint32_t frame_count, uint64_t monotonic_time_ns,
    int32_t discontinuity, uint64_t dropped_frames_before);
typedef int32_t (*capsula_fn_audio_playback_control)(capsula_handle call,
                                                     int32_t command);
typedef int32_t (*capsula_fn_buf_free)(capsula_buf buf);
typedef const char *(*capsula_fn_last_error)(void);
typedef int32_t (*capsula_fn_handle_info)(capsula_handle handle,
                                          capsula_buf *out_json);

/*
 * v1 function table. Frozen layout: fields are only appended in a future
 * minor version, never reordered or removed. struct_size lets bindings
 * detect appended fields.
 */
typedef struct capsula_api_v1 {
  uint32_t abi_major;
  uint32_t abi_minor;
  size_t struct_size;

  capsula_fn_client_connect client_connect;
  capsula_fn_client_close client_close;
  capsula_fn_client_register_descriptors client_register_descriptors;
  capsula_fn_client_info client_info;
  capsula_fn_client_unary client_unary;
  capsula_fn_client_unary_proto client_unary_proto;

  capsula_fn_call_start call_start;
  capsula_fn_call_start_routed call_start_routed;
  capsula_fn_call_send_json call_send_json;
  capsula_fn_call_send_proto call_send_proto;
  capsula_fn_call_half_close call_half_close;
  capsula_fn_call_recv_json call_recv_json;
  capsula_fn_call_recv_proto call_recv_proto;
  capsula_fn_call_end call_end;
  capsula_fn_call_cancel call_cancel;
  capsula_fn_call_retain call_retain;
  capsula_fn_call_release call_release;
  capsula_fn_call_info call_info;
  capsula_fn_call_status call_status;

  capsula_fn_bus_open bus_open;
  capsula_fn_bus_close bus_close;
  capsula_fn_bus_ready bus_ready;
  capsula_fn_bus_notify_invocation bus_notify_invocation;
  capsula_fn_bus_notify_action bus_notify_action;

  capsula_fn_audio_capture_open audio_capture_open;
  capsula_fn_audio_capture_recv audio_capture_recv;
  capsula_fn_audio_playback_open audio_playback_open;
  capsula_fn_audio_playback_write audio_playback_write;
  capsula_fn_audio_playback_control audio_playback_control;

  capsula_fn_buf_free buf_free;
  capsula_fn_last_error last_error;
  capsula_fn_handle_info handle_info;
} capsula_api;

/* Stable v1 prefix: abi_major, abi_minor, struct_size. Any v1 table at least
 * this large is accepted by capsula_get_api, including tables built against an
 * older minor with fewer function pointers. */
#define CAPSULA_API_V1_MIN_SIZE ((size_t)offsetof(capsula_api, client_connect))

/* --- exported entry points ------------------------------------------------ */

/* Returns sizeof(capsula_api) for the calling ABI. */
CAPSULA_EXPORT size_t capsula_api_size(void);

/* Compile-time ABI identity of the library. */
CAPSULA_EXPORT uint32_t capsula_abi_major(void);
CAPSULA_EXPORT uint32_t capsula_abi_minor(void);
CAPSULA_EXPORT const char *capsula_version(void);

/* Read-only process-wide table. Equivalent to a fully populated
 * capsula_get_api() result; do not free. */
CAPSULA_EXPORT const capsula_api *capsula_api_v1(void);

/*
 * Copy the v1 table into caller memory.
 * - abi_major must be 1, otherwise CAPSULA_STATUS_UNSUPPORTED_ABI with no write.
 * - out must be non-NULL and table_size >= CAPSULA_API_V1_MIN_SIZE (the stable
 *   prefix through struct_size), otherwise CAPSULA_STATUS_INVALID_ARGUMENT
 *   with no write. Tables built against a smaller v1 minor remain valid.
 * - Copies exactly min(table_size, capsula_api_size()) bytes; never writes past
 *   table_size. `out->struct_size` reports the library's current full table
 *   size; keep the caller's table zero-initialized and require every field you
 *   call to be within that size and non-NULL.
 */
CAPSULA_EXPORT int32_t capsula_get_api(uint32_t abi_major, size_t table_size,
                                       capsula_api *out);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* CAPSULA_CAPSULA_H */
