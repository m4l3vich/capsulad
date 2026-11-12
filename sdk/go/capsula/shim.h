/* Private dlopen shim for the Go binding. Not part of the public C ABI.
 *
 * Every wrapper that calls the native function table also captures
 * capsula_last_error() in the same C thunk: that error is thread-local, and a
 * separate cgo call would be free to run on another OS thread. The captured
 * message is copied into the caller-provided (err, err_len) buffer.
 */
#ifndef CAPSULA_GO_SHIM_H
#define CAPSULA_GO_SHIM_H

#include <stddef.h>
#include <stdint.h>

#include "abi.h"

#ifdef __cplusplus
extern "C" {
#endif

/* Loads and validates the shared engine.
 * Returns 0 on success, -1 on dlopen/symbol failure, -2 on ABI mismatch.
 * On failure writes a NUL-terminated actionable message into err. */
int capsula_shim_load(const char *path, char *err, size_t err_len);

/* Identity of the loaded engine (valid after a successful load). */
uint32_t capsula_shim_abi_major(void);
uint32_t capsula_shim_abi_minor(void);
const char *capsula_shim_version(void);

int32_t capsula_shim_client_connect(const char *socket_path, size_t socket_path_len,
                                    const char *run_id, size_t run_id_len,
                                    const char *app_id, size_t app_id_len,
                                    uint32_t timeout_ms, capsula_handle *out_client,
                                    char *err, size_t err_len);
int32_t capsula_shim_client_close(capsula_handle client, char *err, size_t err_len);
int32_t capsula_shim_client_register_descriptors(capsula_handle client,
                                                 const uint8_t *file_descriptor_set, size_t len,
                                                 char *err, size_t err_len);
int32_t capsula_shim_call_start(capsula_handle client,
                                const char *service, size_t service_len,
                                const char *method, size_t method_len,
                                int32_t kind, uint32_t timeout_ms,
                                capsula_handle *out_call, char *err, size_t err_len);
int32_t capsula_shim_call_start_routed(capsula_handle client,
                                       const char *target_app_id, size_t target_app_id_len,
                                       const char *service, size_t service_len,
                                       const char *method, size_t method_len,
                                       uint32_t timeout_ms, capsula_handle *out_call,
                                       char *err, size_t err_len);
int32_t capsula_shim_call_send_json(capsula_handle call, const uint8_t *json, size_t len,
                                    char *err, size_t err_len);
int32_t capsula_shim_call_send_proto(capsula_handle call, const uint8_t *proto, size_t len,
                                     char *err, size_t err_len);
int32_t capsula_shim_call_half_close(capsula_handle call, char *err, size_t err_len);
int32_t capsula_shim_call_recv_json(capsula_handle call, uint32_t timeout_ms,
                                    capsula_buf *out_json, char *err, size_t err_len);
int32_t capsula_shim_call_recv_proto(capsula_handle call, uint32_t timeout_ms,
                                     capsula_buf *out_proto, char *err, size_t err_len);
int32_t capsula_shim_call_end(capsula_handle call, int32_t grpc_code,
                              const char *detail, size_t detail_len,
                              char *err, size_t err_len);
int32_t capsula_shim_call_cancel(capsula_handle call, char *err, size_t err_len);
int32_t capsula_shim_call_release(capsula_handle call, char *err, size_t err_len);
int32_t capsula_shim_call_status(capsula_handle call, int32_t *out_grpc_code,
                                 capsula_buf *out_detail, char *err, size_t err_len);
int32_t capsula_shim_bus_open(capsula_handle client, uintptr_t user_data,
                              capsula_handle *out_bus, char *err, size_t err_len);
int32_t capsula_shim_bus_close(capsula_handle bus, char *err, size_t err_len);
int32_t capsula_shim_bus_ready(capsula_handle bus, uint32_t timeout_ms,
                               char *err, size_t err_len);
int32_t capsula_shim_bus_notify_invocation(capsula_handle bus,
                                           const char *invocation_id, size_t invocation_id_len,
                                           int32_t accepted,
                                           const char *detail, size_t detail_len,
                                           char *err, size_t err_len);
int32_t capsula_shim_bus_notify_action(capsula_handle bus,
                                       const char *action_invocation_id,
                                       size_t action_invocation_id_len,
                                       int32_t succeeded,
                                       const char *detail, size_t detail_len,
                                       char *err, size_t err_len);
int32_t capsula_shim_audio_capture_open(capsula_handle client,
                                        const char *endpoint_id, size_t endpoint_id_len,
                                        const capsula_pcm_format *format, uint32_t packet_ms,
                                        const char *activation_id, size_t activation_id_len,
                                        const char *conversation_id, size_t conversation_id_len,
                                        capsula_handle *out_call, char *err, size_t err_len);
int32_t capsula_shim_audio_capture_recv(capsula_handle call, uint32_t timeout_ms,
                                        capsula_pcm_event *out_event,
                                        char *err, size_t err_len);
int32_t capsula_shim_audio_playback_open(capsula_handle client,
                                         const char *endpoint_id, size_t endpoint_id_len,
                                         const capsula_pcm_format *format,
                                         uint32_t target_latency_ms,
                                         const char *conversation_id, size_t conversation_id_len,
                                         capsula_handle *out_call, char *err, size_t err_len);
int32_t capsula_shim_audio_playback_write(capsula_handle call,
                                          const uint8_t *pcm, size_t pcm_len,
                                          uint32_t frame_count, uint64_t monotonic_time_ns,
                                          int32_t discontinuity,
                                          uint64_t dropped_frames_before,
                                          char *err, size_t err_len);
int32_t capsula_shim_audio_playback_control(capsula_handle call, int32_t command,
                                            char *err, size_t err_len);
int32_t capsula_shim_buf_free(capsula_buf buf);

/* Fill the callback table with the Go-exported dispatchers. */
void capsula_shim_fill_callbacks(capsula_bus_callbacks_v1 *out, uintptr_t user_data);

#ifdef __cplusplus
}
#endif

#endif /* CAPSULA_GO_SHIM_H */
