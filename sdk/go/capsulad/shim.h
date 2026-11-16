/* Private dlopen shim for the Go binding. Not part of the public C ABI.
 *
 * Every wrapper that calls the native function table also captures
 * capsulad_last_error() in the same C thunk: that error is thread-local, and a
 * separate cgo call would be free to run on another OS thread. The captured
 * message is copied into the caller-provided (err, err_len) buffer.
 */
#ifndef CAPSULAD_GO_SHIM_H
#define CAPSULAD_GO_SHIM_H

#include <stddef.h>
#include <stdint.h>

#include "abi.h"

#ifdef __cplusplus
extern "C" {
#endif

/* Loads and validates the shared engine.
 * Returns 0 on success, -1 on dlopen/symbol failure, -2 on ABI mismatch.
 * On failure writes a NUL-terminated actionable message into err. */
int capsulad_shim_load(const char *path, char *err, size_t err_len);

/* Identity of the loaded engine (valid after a successful load). */
uint32_t capsulad_shim_abi_major(void);
uint32_t capsulad_shim_abi_minor(void);
const char *capsulad_shim_version(void);

int32_t capsulad_shim_client_connect(const char *socket_path, size_t socket_path_len,
                                    const char *run_id, size_t run_id_len,
                                    const char *app_id, size_t app_id_len,
                                    uint32_t timeout_ms, capsulad_handle *out_client,
                                    char *err, size_t err_len);
int32_t capsulad_shim_client_close(capsulad_handle client, char *err, size_t err_len);
int32_t capsulad_shim_client_register_descriptors(capsulad_handle client,
                                                 const uint8_t *file_descriptor_set, size_t len,
                                                 char *err, size_t err_len);
int32_t capsulad_shim_call_start(capsulad_handle client,
                                const char *service, size_t service_len,
                                const char *method, size_t method_len,
                                int32_t kind, uint32_t timeout_ms,
                                capsulad_handle *out_call, char *err, size_t err_len);
int32_t capsulad_shim_call_start_routed(capsulad_handle client,
                                       const char *target_app_id, size_t target_app_id_len,
                                       const char *service, size_t service_len,
                                       const char *method, size_t method_len,
                                       uint32_t timeout_ms, capsulad_handle *out_call,
                                       char *err, size_t err_len);
int32_t capsulad_shim_call_send_json(capsulad_handle call, const uint8_t *json, size_t len,
                                    char *err, size_t err_len);
int32_t capsulad_shim_call_send_proto(capsulad_handle call, const uint8_t *proto, size_t len,
                                     char *err, size_t err_len);
int32_t capsulad_shim_call_half_close(capsulad_handle call, char *err, size_t err_len);
int32_t capsulad_shim_call_recv_json(capsulad_handle call, uint32_t timeout_ms,
                                    capsulad_buf *out_json, char *err, size_t err_len);
int32_t capsulad_shim_call_recv_proto(capsulad_handle call, uint32_t timeout_ms,
                                     capsulad_buf *out_proto, char *err, size_t err_len);
int32_t capsulad_shim_call_end(capsulad_handle call, int32_t grpc_code,
                              const char *detail, size_t detail_len,
                              char *err, size_t err_len);
int32_t capsulad_shim_call_cancel(capsulad_handle call, char *err, size_t err_len);
int32_t capsulad_shim_call_release(capsulad_handle call, char *err, size_t err_len);
int32_t capsulad_shim_call_status(capsulad_handle call, int32_t *out_grpc_code,
                                 capsulad_buf *out_detail, char *err, size_t err_len);
int32_t capsulad_shim_bus_open(capsulad_handle client, uintptr_t user_data,
                              capsulad_handle *out_bus, char *err, size_t err_len);
int32_t capsulad_shim_bus_close(capsulad_handle bus, char *err, size_t err_len);
int32_t capsulad_shim_bus_ready(capsulad_handle bus, uint32_t timeout_ms,
                               char *err, size_t err_len);
int32_t capsulad_shim_bus_notify_invocation(capsulad_handle bus,
                                           const char *invocation_id, size_t invocation_id_len,
                                           int32_t accepted,
                                           const char *detail, size_t detail_len,
                                           char *err, size_t err_len);
int32_t capsulad_shim_bus_notify_action(capsulad_handle bus,
                                       const char *action_invocation_id,
                                       size_t action_invocation_id_len,
                                       int32_t succeeded,
                                       const char *detail, size_t detail_len,
                                       char *err, size_t err_len);
int32_t capsulad_shim_audio_capture_open(capsulad_handle client,
                                        const char *endpoint_id, size_t endpoint_id_len,
                                        const capsulad_pcm_format *format, uint32_t packet_ms,
                                        const char *activation_id, size_t activation_id_len,
                                        const char *conversation_id, size_t conversation_id_len,
                                        capsulad_handle *out_call, char *err, size_t err_len);
int32_t capsulad_shim_audio_capture_recv(capsulad_handle call, uint32_t timeout_ms,
                                        capsulad_pcm_event *out_event,
                                        char *err, size_t err_len);
int32_t capsulad_shim_audio_playback_open(capsulad_handle client,
                                         const char *endpoint_id, size_t endpoint_id_len,
                                         const capsulad_pcm_format *format,
                                         uint32_t target_latency_ms,
                                         const char *conversation_id, size_t conversation_id_len,
                                         capsulad_handle *out_call, char *err, size_t err_len);
int32_t capsulad_shim_audio_playback_write(capsulad_handle call,
                                          const uint8_t *pcm, size_t pcm_len,
                                          uint32_t frame_count, uint64_t monotonic_time_ns,
                                          int32_t discontinuity,
                                          uint64_t dropped_frames_before,
                                          char *err, size_t err_len);
int32_t capsulad_shim_audio_playback_control(capsulad_handle call, int32_t command,
                                            char *err, size_t err_len);
int32_t capsulad_shim_buf_free(capsulad_buf buf);

/* Appended v1 surface. Each wrapper captures capsulad_last_error in the same
 * C thunk as the failing call (the error is thread-local). */
int32_t capsulad_shim_http_open(const uint8_t *options_json, size_t options_len,
                                uint32_t timeout_ms, capsulad_handle *out_http,
                                char *err, size_t err_len);
int32_t capsulad_shim_http_close(capsulad_handle http, char *err, size_t err_len);
int32_t capsulad_shim_http_cancel(capsulad_handle http, uint64_t request_id,
                                  char *err, size_t err_len);
int32_t capsulad_shim_http_request(capsulad_handle http, uint64_t request_id,
                                   const char *method, size_t method_len,
                                   const char *url, size_t url_len,
                                   const uint8_t *headers_json, size_t headers_len,
                                   const uint8_t *body, size_t body_len,
                                   uint32_t timeout_ms, int32_t *out_status_code,
                                   capsulad_buf *out_headers_json,
                                   capsulad_buf *out_body, char *err, size_t err_len);
int32_t capsulad_shim_grpc_open(const char *url, size_t url_len,
                                const uint8_t *options_json, size_t options_len,
                                uint32_t timeout_ms, capsulad_handle *out_client,
                                char *err, size_t err_len);
int32_t capsulad_shim_call_start_meta(capsulad_handle client,
                                      const char *service, size_t service_len,
                                      const char *method, size_t method_len,
                                      int32_t kind, uint32_t timeout_ms,
                                      const uint8_t *headers_json, size_t headers_len,
                                      capsulad_handle *out_call,
                                      char *err, size_t err_len);
int32_t capsulad_shim_base64_encode(const uint8_t *data, size_t len, int32_t flags,
                                    capsulad_buf *out, char *err, size_t err_len);
int32_t capsulad_shim_base64_decode(const uint8_t *data, size_t len, int32_t flags,
                                    capsulad_buf *out, char *err, size_t err_len);
int32_t capsulad_shim_percent_encode(const uint8_t *data, size_t len,
                                     capsulad_buf *out, char *err, size_t err_len);
int32_t capsulad_shim_percent_decode(const uint8_t *data, size_t len,
                                     capsulad_buf *out, char *err, size_t err_len);
int32_t capsulad_shim_sha256(const uint8_t *data, size_t len, capsulad_buf *out,
                             char *err, size_t err_len);
int32_t capsulad_shim_random_bytes(uint8_t *out, size_t len,
                                   char *err, size_t err_len);
int32_t capsulad_shim_uuid_v4(capsulad_buf *out, char *err, size_t err_len);
int32_t capsulad_shim_url_parse(const char *url, size_t len, capsulad_buf *out_json,
                                char *err, size_t err_len);

/* Fill the callback table with the Go-exported dispatchers. */
void capsulad_shim_fill_callbacks(capsulad_bus_callbacks_v1 *out, uintptr_t user_data);

#ifdef __cplusplus
}
#endif

#endif /* CAPSULAD_GO_SHIM_H */
