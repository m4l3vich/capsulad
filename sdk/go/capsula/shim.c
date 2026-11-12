/* Private dlopen shim for the Go binding. Not part of the public C ABI. */
#include "shim.h"

#include <dlfcn.h>
#include <stddef.h>
#include <stdio.h>
#include <string.h>

/* Exported by the Go package (cgo //export). Declared here so the shim can
 * fill the callback table without the Go side touching C function pointers. */
extern void capsulaGoOnReady(capsula_handle bus, void *user_data);
extern void capsulaGoOnDisconnected(capsula_handle bus, int32_t status,
                                    char *detail, void *user_data);
extern void capsulaGoOnEvent(capsula_handle bus, int32_t kind,
                             uint8_t *json, size_t len, void *user_data);
extern void capsulaGoOnCall(capsula_handle bus, capsula_handle call,
                            uint8_t *info_json, size_t len, void *user_data);
extern void capsulaGoOnLog(int32_t level, const char *message, void *user_data);

/* Retained for the process lifetime. The engine owns a process-wide worker
 * runtime, so a successful load is never dlclose'd; failed pre-init loads are. */
static void *g_library;
static capsula_api g_api;
static int g_loaded;
static uint32_t g_major;
static uint32_t g_minor;
static const char *g_version = "unloaded";

static const capsula_api *api(void) { return &g_api; }

/* Captures the thread-local native error in the same C thunk as the failing
 * call, then writes it to caller-owned memory. */
static int32_t capture(int32_t status, char *err, size_t err_len) {
  if (err != NULL && err_len > 0) {
    err[0] = '\0';
    if (status != CAPSULA_STATUS_OK && g_loaded && api()->last_error != NULL) {
      const char *message = api()->last_error();
      if (message != NULL) {
        snprintf(err, err_len, "%s", message);
      }
    }
  }
  return status;
}

int capsula_shim_load(const char *path, char *err, size_t err_len) {
  if (err != NULL && err_len > 0) {
    err[0] = '\0';
  }
  if (g_loaded) {
    return 0;
  }
  dlerror();
  void *lib = dlopen(path, RTLD_NOW | RTLD_LOCAL);
  if (lib == NULL) {
    const char *reason = dlerror();
    if (err != NULL && err_len > 0) {
      snprintf(err, err_len, "%s", reason != NULL ? reason : "dlopen failed");
    }
    return -1;
  }

  uint32_t (*major_fn)(void) = (uint32_t (*)(void))dlsym(lib, "capsula_abi_major");
  uint32_t (*minor_fn)(void) = (uint32_t (*)(void))dlsym(lib, "capsula_abi_minor");
  const char *(*version_fn)(void) = (const char *(*)(void))dlsym(lib, "capsula_version");
  int32_t (*get_api)(uint32_t, size_t, capsula_api *) =
      (int32_t (*)(uint32_t, size_t, capsula_api *))dlsym(lib, "capsula_get_api");
  if (major_fn == NULL || minor_fn == NULL || get_api == NULL) {
    dlclose(lib);
    if (err != NULL && err_len > 0) {
      snprintf(err, err_len,
               "missing capsula_abi_major/capsula_abi_minor/capsula_get_api exports");
    }
    return -1;
  }

  g_major = major_fn();
  g_minor = minor_fn();
  g_version = version_fn != NULL ? version_fn() : "unknown";
  if (g_major != CAPSULA_ABI_MAJOR) {
    if (err != NULL && err_len > 0) {
      snprintf(err, err_len,
               "native library reports ABI %u.%u; this SDK requires major %u",
               g_major, g_minor, CAPSULA_ABI_MAJOR);
    }
    dlclose(lib);
    return -2;
  }

  capsula_api table;
  memset(&table, 0, sizeof(table));
  int32_t status = get_api(CAPSULA_ABI_MAJOR, sizeof(table), &table);
  if (status != CAPSULA_STATUS_OK) {
    if (err != NULL && err_len > 0) {
      snprintf(err, err_len, "capsula_get_api(1, %zu) failed with status %d",
               sizeof(table), status);
    }
    dlclose(lib);
    return -1;
  }

  /* Check the prefix this binding actually calls. A future minor may append
   * fields; an older minor may expose a shorter prefix, which is accepted as
   * long as every required field is inside struct_size. */
  size_t required = offsetof(capsula_api, last_error) + sizeof(table.last_error);
  if (table.abi_major != CAPSULA_ABI_MAJOR || table.struct_size < required ||
      table.client_connect == NULL || table.client_close == NULL ||
      table.client_register_descriptors == NULL ||
      table.call_start == NULL || table.call_start_routed == NULL ||
      table.call_send_json == NULL || table.call_send_proto == NULL ||
      table.call_half_close == NULL || table.call_recv_json == NULL ||
      table.call_recv_proto == NULL || table.call_end == NULL ||
      table.call_cancel == NULL || table.call_status == NULL ||
      table.bus_open == NULL || table.bus_close == NULL || table.bus_ready == NULL ||
      table.bus_notify_invocation == NULL || table.bus_notify_action == NULL ||
      table.audio_capture_open == NULL || table.audio_capture_recv == NULL ||
      table.audio_playback_open == NULL || table.audio_playback_write == NULL ||
      table.audio_playback_control == NULL ||
      table.buf_free == NULL || table.last_error == NULL) {
    if (err != NULL && err_len > 0) {
      snprintf(err, err_len, "native library exposes an incomplete ABI v1 table "
               "(major %u, minor %u, struct_size %zu, required %zu)",
               table.abi_major, table.abi_minor, table.struct_size, required);
    }
    dlclose(lib);
    return -1;
  }

  g_api = table;
  g_library = lib;
  g_loaded = 1;
  return 0;
}

uint32_t capsula_shim_abi_major(void) { return g_major; }
uint32_t capsula_shim_abi_minor(void) { return g_minor; }
const char *capsula_shim_version(void) { return g_version; }

int32_t capsula_shim_client_connect(const char *socket_path, size_t socket_path_len,
                                    const char *run_id, size_t run_id_len,
                                    const char *app_id, size_t app_id_len,
                                    uint32_t timeout_ms, capsula_handle *out_client,
                                    char *err, size_t err_len) {
  int32_t status = api()->client_connect(socket_path, socket_path_len, run_id, run_id_len,
                                         app_id, app_id_len, timeout_ms, out_client);
  return capture(status, err, err_len);
}

int32_t capsula_shim_client_close(capsula_handle client, char *err, size_t err_len) {
  return capture(api()->client_close(client), err, err_len);
}

int32_t capsula_shim_client_register_descriptors(capsula_handle client,
                                                 const uint8_t *file_descriptor_set,
                                                 size_t len, char *err, size_t err_len) {
  return capture(api()->client_register_descriptors(client, file_descriptor_set, len),
                 err, err_len);
}




int32_t capsula_shim_call_start(capsula_handle client,
                                const char *service, size_t service_len,
                                const char *method, size_t method_len,
                                int32_t kind, uint32_t timeout_ms,
                                capsula_handle *out_call, char *err, size_t err_len) {
  int32_t status = api()->call_start(client, service, service_len, method, method_len,
                                     kind, timeout_ms, out_call);
  return capture(status, err, err_len);
}

int32_t capsula_shim_call_start_routed(capsula_handle client,
                                       const char *target_app_id, size_t target_app_id_len,
                                       const char *service, size_t service_len,
                                       const char *method, size_t method_len,
                                       uint32_t timeout_ms, capsula_handle *out_call,
                                       char *err, size_t err_len) {
  int32_t status = api()->call_start_routed(client, target_app_id, target_app_id_len,
                                            service, service_len, method, method_len,
                                            timeout_ms, out_call);
  return capture(status, err, err_len);
}

int32_t capsula_shim_call_send_json(capsula_handle call, const uint8_t *json, size_t len,
                                    char *err, size_t err_len) {
  return capture(api()->call_send_json(call, json, len), err, err_len);
}

int32_t capsula_shim_call_send_proto(capsula_handle call, const uint8_t *proto, size_t len,
                                     char *err, size_t err_len) {
  return capture(api()->call_send_proto(call, proto, len), err, err_len);
}

int32_t capsula_shim_call_half_close(capsula_handle call, char *err, size_t err_len) {
  return capture(api()->call_half_close(call), err, err_len);
}

int32_t capsula_shim_call_recv_json(capsula_handle call, uint32_t timeout_ms,
                                    capsula_buf *out_json, char *err, size_t err_len) {
  return capture(api()->call_recv_json(call, timeout_ms, out_json), err, err_len);
}

int32_t capsula_shim_call_recv_proto(capsula_handle call, uint32_t timeout_ms,
                                     capsula_buf *out_proto, char *err, size_t err_len) {
  return capture(api()->call_recv_proto(call, timeout_ms, out_proto), err, err_len);
}

int32_t capsula_shim_call_end(capsula_handle call, int32_t grpc_code,
                              const char *detail, size_t detail_len,
                              char *err, size_t err_len) {
  return capture(api()->call_end(call, grpc_code, detail, detail_len), err, err_len);
}

int32_t capsula_shim_call_cancel(capsula_handle call, char *err, size_t err_len) {
  return capture(api()->call_cancel(call), err, err_len);
}

int32_t capsula_shim_call_release(capsula_handle call, char *err, size_t err_len) {
  return capture(api()->call_release(call), err, err_len);
}

int32_t capsula_shim_call_status(capsula_handle call, int32_t *out_grpc_code,
                                 capsula_buf *out_detail, char *err, size_t err_len) {
  int32_t status = api()->call_status(call, out_grpc_code, out_detail);
  return capture(status, err, err_len);
}

void capsula_shim_fill_callbacks(capsula_bus_callbacks_v1 *out, uintptr_t user_data) {
  memset(out, 0, sizeof(*out));
  out->struct_size = sizeof(*out);
  /* The Go side passes a cgo.Handle integer; the library treats user_data as
   * borrowed opaque memory and never dereferences it. */
  out->user_data = (void *)user_data;
  out->on_ready = capsulaGoOnReady;
  /* The Go exports use cgo's unqualified pointer parameters; the ABI table
   * expects const-qualified inputs. The representation is identical. */
  out->on_disconnected = (capsula_disconnected_fn)capsulaGoOnDisconnected;
  out->on_event = (capsula_event_fn)capsulaGoOnEvent;
  out->on_call = (capsula_call_fn)capsulaGoOnCall;
  out->on_log = capsulaGoOnLog;
}

int32_t capsula_shim_bus_open(capsula_handle client, uintptr_t user_data,
                              capsula_handle *out_bus, char *err, size_t err_len) {
  capsula_bus_callbacks_v1 callbacks;
  capsula_shim_fill_callbacks(&callbacks, user_data);
  return capture(api()->bus_open(client, &callbacks, out_bus), err, err_len);
}

int32_t capsula_shim_bus_close(capsula_handle bus, char *err, size_t err_len) {
  return capture(api()->bus_close(bus), err, err_len);
}

int32_t capsula_shim_bus_ready(capsula_handle bus, uint32_t timeout_ms,
                               char *err, size_t err_len) {
  return capture(api()->bus_ready(bus, timeout_ms), err, err_len);
}

int32_t capsula_shim_bus_notify_invocation(capsula_handle bus,
                                           const char *invocation_id, size_t invocation_id_len,
                                           int32_t accepted,
                                           const char *detail, size_t detail_len,
                                           char *err, size_t err_len) {
  int32_t status = api()->bus_notify_invocation(bus, invocation_id, invocation_id_len,
                                                accepted, detail, detail_len);
  return capture(status, err, err_len);
}

int32_t capsula_shim_bus_notify_action(capsula_handle bus,
                                       const char *action_invocation_id,
                                       size_t action_invocation_id_len,
                                       int32_t succeeded,
                                       const char *detail, size_t detail_len,
                                       char *err, size_t err_len) {
  int32_t status = api()->bus_notify_action(bus, action_invocation_id,
                                            action_invocation_id_len, succeeded,
                                            detail, detail_len);
  return capture(status, err, err_len);
}

int32_t capsula_shim_audio_capture_open(capsula_handle client,
                                        const char *endpoint_id, size_t endpoint_id_len,
                                        const capsula_pcm_format *format, uint32_t packet_ms,
                                        const char *activation_id, size_t activation_id_len,
                                        const char *conversation_id, size_t conversation_id_len,
                                        capsula_handle *out_call, char *err, size_t err_len) {
  int32_t status = api()->audio_capture_open(client, endpoint_id, endpoint_id_len, format,
                                             packet_ms, activation_id, activation_id_len,
                                             conversation_id, conversation_id_len, out_call);
  return capture(status, err, err_len);
}

int32_t capsula_shim_audio_capture_recv(capsula_handle call, uint32_t timeout_ms,
                                        capsula_pcm_event *out_event,
                                        char *err, size_t err_len) {
  return capture(api()->audio_capture_recv(call, timeout_ms, out_event), err, err_len);
}

int32_t capsula_shim_audio_playback_open(capsula_handle client,
                                         const char *endpoint_id, size_t endpoint_id_len,
                                         const capsula_pcm_format *format,
                                         uint32_t target_latency_ms,
                                         const char *conversation_id, size_t conversation_id_len,
                                         capsula_handle *out_call, char *err, size_t err_len) {
  int32_t status = api()->audio_playback_open(client, endpoint_id, endpoint_id_len, format,
                                              target_latency_ms, conversation_id,
                                              conversation_id_len, out_call);
  return capture(status, err, err_len);
}

int32_t capsula_shim_audio_playback_write(capsula_handle call,
                                          const uint8_t *pcm, size_t pcm_len,
                                          uint32_t frame_count, uint64_t monotonic_time_ns,
                                          int32_t discontinuity,
                                          uint64_t dropped_frames_before,
                                          char *err, size_t err_len) {
  int32_t status = api()->audio_playback_write(call, pcm, pcm_len, frame_count,
                                               monotonic_time_ns, discontinuity,
                                               dropped_frames_before);
  return capture(status, err, err_len);
}

int32_t capsula_shim_audio_playback_control(capsula_handle call, int32_t command,
                                            char *err, size_t err_len) {
  return capture(api()->audio_playback_control(call, command), err, err_len);
}

int32_t capsula_shim_buf_free(capsula_buf buf) { return api()->buf_free(buf); }

