/*
 * MicroPython capsulad module.
 *
 * The module exposes thin primitives over the public libcapsulad C ABI. The
 * Python prelude installed by the host adds the ergonomic object API
 * (capsulad.Call, stream helpers, event dispatch). Keeping protocol calls in C
 * prevents a second, divergent client implementation in Python.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "py/obj.h"
#include "py/objexcept.h"
#include "py/objmodule.h"
#include "py/runtime.h"
#include "py/stackctrl.h"

#include "capsulad_host.h"

static capsulad_host *module_host;

void capsulad_module_set_host(capsulad_host *host) { module_host = host; }

static capsulad_host *require_host(void) {
  if (module_host == NULL) {
    mp_raise_msg(&mp_type_RuntimeError,
                 MP_ERROR_TEXT("capsulad host is not initialised"));
  }
  return module_host;
}

static void raise_status(int32_t status) {
  char err[512];
  capsulad_host_error(module_host, status, err, sizeof(err));
  mp_obj_t args[2];
  args[0] = mp_obj_new_int(status);
  args[1] = mp_obj_new_str(err, strlen(err));
  nlr_raise(mp_obj_new_exception_args(&mp_type_OSError, 2, args));
}

static uint32_t arg_timeout(size_t n_args, const mp_obj_t *args, size_t index,
                            uint32_t fallback) {
  if (n_args <= index || args[index] == mp_const_none) {
    return fallback;
  }
  mp_int_t value = mp_obj_get_int(args[index]);
  if (value < 0) {
    return CAPSULAD_WAIT_FOREVER;
  }
  return (uint32_t)value;
}

static uint64_t arg_handle(mp_obj_t value) {
  mp_int_t handle = mp_obj_get_int(value);
  if (handle <= 0) {
    return CAPSULAD_HANDLE_INVALID;
  }
  return (uint64_t)handle;
}

static const char *arg_str(mp_obj_t value, size_t *len) {
  return mp_obj_str_get_data(value, len);
}

static const uint8_t *arg_bytes(mp_obj_t value, size_t *len) {
  if (!mp_obj_is_type(value, &mp_type_bytes)) {
    mp_raise_TypeError(MP_ERROR_TEXT("expected bytes"));
  }
  return (const uint8_t *)mp_obj_str_get_data(value, len);
}

static mp_obj_t mp_capsulad_start(size_t n_args, const mp_obj_t *args) {
  capsulad_host *h = require_host();
  char err[512];
  uint32_t timeout = arg_timeout(n_args, args, 0, 10000);
  if (capsulad_host_start(h, timeout, err, sizeof(err)) != 0) {
    mp_raise_msg(&mp_type_OSError, err);
  }
  return mp_const_none;
}
MP_DEFINE_CONST_FUN_OBJ_VAR_BETWEEN(mp_capsulad_start_obj, 0, 1,
                                     mp_capsulad_start);

static mp_obj_t mp_capsulad_poll(size_t n_args, const mp_obj_t *args) {
  capsulad_host *h = require_host();
  int timeout = 0;
  if (n_args > 0 && args[0] != mp_const_none) {
    timeout = (int)mp_obj_get_int(args[0]);
  }
  capsulad_host_job job;
  if (!capsulad_host_poll(h, timeout, &job)) {
    return mp_const_none;
  }
  mp_obj_t items[5] = {mp_const_none, mp_const_none, mp_const_none,
                       mp_const_none, mp_const_none};
  switch (job.kind) {
    case CAPSULAD_HOST_JOB_EVENT:
      items[0] = mp_obj_new_str("event", 5);
      items[1] = mp_obj_new_int(job.event_kind);
      if (job.json != NULL && job.json_len > 0) {
        items[2] = mp_obj_new_bytes(job.json, job.json_len);
      }
      break;
    case CAPSULAD_HOST_JOB_CALL:
      items[0] = mp_obj_new_str("call", 4);
      items[3] = mp_obj_new_int_from_uint((mp_uint_t)job.call);
      if (job.info != NULL && job.info_len > 0) {
        items[4] = mp_obj_new_bytes(job.info, job.info_len);
      }
      break;
    case CAPSULAD_HOST_JOB_DISCONNECTED:
      items[0] = mp_obj_new_str("disconnect", 10);
      items[1] = mp_obj_new_int(job.status);
      if (job.detail != NULL) {
        items[2] = mp_obj_new_str(job.detail, strlen(job.detail));
      }
      break;
    default:
      break;
  }
  capsulad_host_free_job(&job);
  return mp_obj_new_tuple(5, items);
}
MP_DEFINE_CONST_FUN_OBJ_VAR_BETWEEN(mp_capsulad_poll_obj, 0, 1,
                                     mp_capsulad_poll);

static mp_obj_t mp_capsulad_stopped(void) {
  return mp_obj_new_bool(capsulad_host_stopped(require_host()));
}
MP_DEFINE_CONST_FUN_OBJ_0(mp_capsulad_stopped_obj, mp_capsulad_stopped);

static mp_obj_t mp_capsulad_disconnected(void) {
  return mp_obj_new_bool(capsulad_host_disconnected(require_host()));
}
MP_DEFINE_CONST_FUN_OBJ_0(mp_capsulad_disconnected_obj,
                          mp_capsulad_disconnected);

static mp_obj_t mp_capsulad_connected(void) {
  return mp_obj_new_bool(capsulad_host_connected(require_host()));
}
MP_DEFINE_CONST_FUN_OBJ_0(mp_capsulad_connected_obj, mp_capsulad_connected);

static mp_obj_t mp_capsulad_ready(void) {
  return mp_obj_new_bool(capsulad_host_bus_ready(require_host()));
}
MP_DEFINE_CONST_FUN_OBJ_0(mp_capsulad_ready_obj, mp_capsulad_ready);

static mp_obj_t mp_capsulad_disconnect_status(void) {
  int32_t status = 0;
  capsulad_host_disconnect_status(require_host(), &status, NULL, 0);
  return mp_obj_new_int(status);
}
MP_DEFINE_CONST_FUN_OBJ_0(mp_capsulad_disconnect_status_obj,
                          mp_capsulad_disconnect_status);

static mp_obj_t mp_capsulad_disconnect_detail(void) {
  int32_t status = 0;
  char detail[512] = {0};
  if (!capsulad_host_disconnect_status(require_host(), &status, detail,
                                      sizeof(detail))) {
    return mp_const_none;
  }
  return mp_obj_new_str(detail, strlen(detail));
}
MP_DEFINE_CONST_FUN_OBJ_0(mp_capsulad_disconnect_detail_obj,
                          mp_capsulad_disconnect_detail);

static mp_obj_t mp_capsulad_last_status(void) {
  return mp_obj_new_int(require_host()->last_status);
}
MP_DEFINE_CONST_FUN_OBJ_0(mp_capsulad_last_status_obj, mp_capsulad_last_status);

static mp_obj_t mp_capsulad_last_error(void) {
  return mp_obj_new_str(require_host()->last_detail,
                        strlen(require_host()->last_detail));
}
MP_DEFINE_CONST_FUN_OBJ_0(mp_capsulad_last_error_obj, mp_capsulad_last_error);

static mp_obj_t mp_capsulad_stop(void) {
  capsulad_host *h = require_host();
  pthread_mutex_lock(&h->mu);
  h->stopped = 1;
  pthread_cond_broadcast(&h->cv);
  pthread_mutex_unlock(&h->mu);
  return mp_const_none;
}
MP_DEFINE_CONST_FUN_OBJ_0(mp_capsulad_stop_obj, mp_capsulad_stop);

static mp_obj_t mp_capsulad_log(mp_obj_t message) {
  size_t len = 0;
  const char *text = arg_str(message, &len);
  fprintf(stderr, "capsulad-script: %.*s\n", (int)len, text);
  fflush(stderr);
  return mp_const_none;
}
MP_DEFINE_CONST_FUN_OBJ_1(mp_capsulad_log_obj, mp_capsulad_log);

static mp_obj_t mp_capsulad_unary(size_t n_args, const mp_obj_t *args) {
  if (n_args < 3) {
    mp_raise_TypeError(MP_ERROR_TEXT("unary(service, method, request, timeout=...)"));
  }
  capsulad_host *h = require_host();
  size_t service_len = 0, method_len = 0, request_len = 0;
  const char *service = arg_str(args[0], &service_len);
  const char *method = arg_str(args[1], &method_len);
  const char *request = arg_str(args[2], &request_len);
  capsulad_buf out = {0};
  uint32_t timeout = arg_timeout(n_args, args, 3, 30000);
  int32_t status = h->api->client_unary(
      h->client, service, service_len, method, method_len,
      (const uint8_t *)request, request_len, timeout, &out);
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  mp_obj_t result = mp_obj_new_str((const char *)out.data, capsulad_host_text_len(out.data, out.len));
  h->api->buf_free(out);
  return result;
}
MP_DEFINE_CONST_FUN_OBJ_VAR_BETWEEN(mp_capsulad_unary_obj, 3, 4,
                                     mp_capsulad_unary);

static mp_obj_t mp_capsulad_unary_proto(size_t n_args, const mp_obj_t *args) {
  if (n_args < 3) {
    mp_raise_TypeError(MP_ERROR_TEXT("unary_proto(service, method, request, timeout=...)"));
  }
  capsulad_host *h = require_host();
  size_t service_len = 0, method_len = 0, request_len = 0;
  const char *service = arg_str(args[0], &service_len);
  const char *method = arg_str(args[1], &method_len);
  const uint8_t *request = arg_bytes(args[2], &request_len);
  uint32_t timeout = arg_timeout(n_args, args, 3, 30000);
  capsulad_buf out = {0};
  int32_t status = h->api->client_unary_proto(h->client, service, service_len,
                                              method, method_len, request,
                                              request_len, timeout, &out);
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  mp_obj_t result = mp_obj_new_bytes(out.data, out.len);
  h->api->buf_free(out);
  return result;
}
MP_DEFINE_CONST_FUN_OBJ_VAR_BETWEEN(mp_capsulad_unary_proto_obj, 3, 4,
                                     mp_capsulad_unary_proto);

static mp_obj_t mp_capsulad_call_start(size_t n_args, const mp_obj_t *args) {
  if (n_args < 3) {
    mp_raise_TypeError(MP_ERROR_TEXT("call_start(service, method, kind, timeout=...)"));
  }
  capsulad_host *h = require_host();
  size_t service_len = 0, method_len = 0;
  const char *service = arg_str(args[0], &service_len);
  const char *method = arg_str(args[1], &method_len);
  uint32_t timeout = arg_timeout(n_args, args, 3, 30000);
  capsulad_handle call = CAPSULAD_HANDLE_INVALID;
  int32_t status = h->api->call_start(
      h->client, service, service_len, method, method_len,
      (int32_t)mp_obj_get_int(args[2]), timeout, &call);
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  return mp_obj_new_int_from_uint((mp_uint_t)call);
}
MP_DEFINE_CONST_FUN_OBJ_VAR_BETWEEN(mp_capsulad_call_start_obj, 3, 4,
                                     mp_capsulad_call_start);

static mp_obj_t mp_capsulad_call_start_routed(size_t n_args,
                                             const mp_obj_t *args) {
  if (n_args < 3) {
    mp_raise_TypeError(MP_ERROR_TEXT("call_start_routed(app_id, service, method, timeout=...)"));
  }
  capsulad_host *h = require_host();
  size_t app_len = 0, service_len = 0, method_len = 0;
  const char *app = arg_str(args[0], &app_len);
  const char *service = arg_str(args[1], &service_len);
  const char *method = arg_str(args[2], &method_len);
  uint32_t timeout = arg_timeout(n_args, args, 3, 30000);
  capsulad_handle call = CAPSULAD_HANDLE_INVALID;
  int32_t status = h->api->call_start_routed(
      h->client, app, app_len, service, service_len, method, method_len,
      timeout, &call);
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  return mp_obj_new_int_from_uint((mp_uint_t)call);
}
MP_DEFINE_CONST_FUN_OBJ_VAR_BETWEEN(mp_capsulad_call_start_routed_obj, 3, 4,
                                     mp_capsulad_call_start_routed);

static mp_obj_t mp_capsulad_send_json(mp_obj_t handle_obj, mp_obj_t json_obj) {
  capsulad_host *h = require_host();
  size_t len = 0;
  const char *json = arg_str(json_obj, &len);
  int32_t status =
      h->api->call_send_json(arg_handle(handle_obj), (const uint8_t *)json, len);
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  return mp_const_none;
}
MP_DEFINE_CONST_FUN_OBJ_2(mp_capsulad_send_json_obj, mp_capsulad_send_json);

static mp_obj_t mp_capsulad_send_proto(mp_obj_t handle_obj, mp_obj_t bytes_obj) {
  capsulad_host *h = require_host();
  size_t len = 0;
  const uint8_t *data = arg_bytes(bytes_obj, &len);
  int32_t status = h->api->call_send_proto(arg_handle(handle_obj), data, len);
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  return mp_const_none;
}
MP_DEFINE_CONST_FUN_OBJ_2(mp_capsulad_send_proto_obj, mp_capsulad_send_proto);

static mp_obj_t mp_capsulad_half_close(mp_obj_t handle_obj) {
  capsulad_host *h = require_host();
  int32_t status = h->api->call_half_close(arg_handle(handle_obj));
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  return mp_const_none;
}
MP_DEFINE_CONST_FUN_OBJ_1(mp_capsulad_half_close_obj, mp_capsulad_half_close);

static mp_obj_t mp_capsulad_recv_json(size_t n_args, const mp_obj_t *args) {
  capsulad_host *h = require_host();
  if (n_args < 1) {
    mp_raise_TypeError(MP_ERROR_TEXT("recv_json(call, timeout=...)"));
  }
  uint32_t timeout =
      n_args > 1 ? (mp_obj_get_int(args[1]) < 0
                        ? CAPSULAD_WAIT_FOREVER
                        : (uint32_t)mp_obj_get_int(args[1]))
                 : 30000;
  capsulad_buf out = {0};
  int32_t status = h->api->call_recv_json(arg_handle(args[0]), timeout, &out);
  if (status == CAPSULAD_STATUS_END_OF_STREAM) {
    return mp_const_none;
  }
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  mp_obj_t result = mp_obj_new_str((const char *)out.data, capsulad_host_text_len(out.data, out.len));
  h->api->buf_free(out);
  return result;
}
MP_DEFINE_CONST_FUN_OBJ_VAR_BETWEEN(mp_capsulad_recv_json_obj, 1, 2,
                                     mp_capsulad_recv_json);

static mp_obj_t mp_capsulad_recv_proto(size_t n_args, const mp_obj_t *args) {
  capsulad_host *h = require_host();
  if (n_args < 1) {
    mp_raise_TypeError(MP_ERROR_TEXT("recv_proto(call, timeout=...)"));
  }
  uint32_t timeout =
      n_args > 1 ? (mp_obj_get_int(args[1]) < 0
                        ? CAPSULAD_WAIT_FOREVER
                        : (uint32_t)mp_obj_get_int(args[1]))
                 : 30000;
  capsulad_buf out = {0};
  int32_t status = h->api->call_recv_proto(arg_handle(args[0]), timeout, &out);
  if (status == CAPSULAD_STATUS_END_OF_STREAM) {
    return mp_const_none;
  }
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  mp_obj_t result = mp_obj_new_bytes(out.data, out.len);
  h->api->buf_free(out);
  return result;
}
MP_DEFINE_CONST_FUN_OBJ_VAR_BETWEEN(mp_capsulad_recv_proto_obj, 1, 2,
                                     mp_capsulad_recv_proto);

static mp_obj_t mp_capsulad_end(size_t n_args, const mp_obj_t *args) {
  capsulad_host *h = require_host();
  if (n_args < 1) {
    mp_raise_TypeError(MP_ERROR_TEXT("end(call, code=0, detail='')"));
  }
  int32_t code = n_args > 1 ? (int32_t)mp_obj_get_int(args[1]) : 0;
  size_t detail_len = 0;
  const char *detail = "";
  if (n_args > 2 && args[2] != mp_const_none) {
    detail = arg_str(args[2], &detail_len);
  }
  int32_t status =
      h->api->call_end(arg_handle(args[0]), code, detail, detail_len);
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  return mp_const_none;
}
MP_DEFINE_CONST_FUN_OBJ_VAR_BETWEEN(mp_capsulad_end_obj, 1, 3, mp_capsulad_end);

static mp_obj_t mp_capsulad_cancel(mp_obj_t handle_obj) {
  capsulad_host *h = require_host();
  int32_t status = h->api->call_cancel(arg_handle(handle_obj));
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  return mp_const_none;
}
MP_DEFINE_CONST_FUN_OBJ_1(mp_capsulad_cancel_obj, mp_capsulad_cancel);

static mp_obj_t mp_capsulad_release(mp_obj_t handle_obj) {
  capsulad_host *h = require_host();
  int32_t status = h->api->call_release(arg_handle(handle_obj));
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  return mp_const_none;
}
MP_DEFINE_CONST_FUN_OBJ_1(mp_capsulad_release_obj, mp_capsulad_release);

static mp_obj_t mp_capsulad_call_info(mp_obj_t handle_obj) {
  capsulad_host *h = require_host();
  capsulad_buf out = {0};
  int32_t status = h->api->call_info(arg_handle(handle_obj), &out);
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  mp_obj_t result = mp_obj_new_str((const char *)out.data, capsulad_host_text_len(out.data, out.len));
  h->api->buf_free(out);
  return result;
}
MP_DEFINE_CONST_FUN_OBJ_1(mp_capsulad_call_info_obj, mp_capsulad_call_info);

static mp_obj_t mp_capsulad_call_status(mp_obj_t handle_obj) {
  capsulad_host *h = require_host();
  int32_t code = 0;
  capsulad_buf detail = {0};
  int32_t status =
      h->api->call_status(arg_handle(handle_obj), &code, &detail);
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  mp_obj_t items[2];
  items[0] = mp_obj_new_int(code);
  items[1] = mp_obj_new_str((const char *)detail.data,
                             capsulad_host_text_len(detail.data, detail.len));
  h->api->buf_free(detail);
  return mp_obj_new_tuple(2, items);
}
MP_DEFINE_CONST_FUN_OBJ_1(mp_capsulad_call_status_obj, mp_capsulad_call_status);

static mp_obj_t mp_capsulad_notify_invocation(size_t n_args,
                                             const mp_obj_t *args) {
  capsulad_host *h = require_host();
  if (n_args < 2) {
    mp_raise_TypeError(MP_ERROR_TEXT("notify_invocation(id, accepted, detail='')"));
  }
  size_t id_len = 0, detail_len = 0;
  const char *id = arg_str(args[0], &id_len);
  int32_t accepted = (int32_t)mp_obj_get_int(args[1]);
  const char *detail = "";
  if (n_args > 2 && args[2] != mp_const_none) {
    detail = arg_str(args[2], &detail_len);
  }
  int32_t status = h->api->bus_notify_invocation(h->bus, id, id_len, accepted,
                                                 detail, detail_len);
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  return mp_const_none;
}
MP_DEFINE_CONST_FUN_OBJ_VAR_BETWEEN(mp_capsulad_notify_invocation_obj, 2, 3,
                                     mp_capsulad_notify_invocation);

static mp_obj_t mp_capsulad_notify_action(size_t n_args,
                                         const mp_obj_t *args) {
  capsulad_host *h = require_host();
  if (n_args < 2) {
    mp_raise_TypeError(MP_ERROR_TEXT("notify_action(id, succeeded, detail='')"));
  }
  size_t id_len = 0, detail_len = 0;
  const char *id = arg_str(args[0], &id_len);
  int32_t succeeded = (int32_t)mp_obj_get_int(args[1]);
  const char *detail = "";
  if (n_args > 2 && args[2] != mp_const_none) {
    detail = arg_str(args[2], &detail_len);
  }
  int32_t status = h->api->bus_notify_action(h->bus, id, id_len, succeeded,
                                             detail, detail_len);
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  return mp_const_none;
}
MP_DEFINE_CONST_FUN_OBJ_VAR_BETWEEN(mp_capsulad_notify_action_obj, 2, 3,
                                     mp_capsulad_notify_action);

static mp_obj_t mp_capsulad_client_info(void) {
  capsulad_host *h = require_host();
  capsulad_buf out = {0};
  int32_t status = h->api->client_info(h->client, &out);
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  mp_obj_t result = mp_obj_new_str((const char *)out.data, capsulad_host_text_len(out.data, out.len));
  h->api->buf_free(out);
  return result;
}
MP_DEFINE_CONST_FUN_OBJ_0(mp_capsulad_client_info_obj, mp_capsulad_client_info);

static mp_obj_t mp_capsulad_handle_info(mp_obj_t handle_obj) {
  capsulad_host *h = require_host();
  capsulad_buf out = {0};
  int32_t status = h->api->handle_info(arg_handle(handle_obj), &out);
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  mp_obj_t result = mp_obj_new_str((const char *)out.data, capsulad_host_text_len(out.data, out.len));
  h->api->buf_free(out);
  return result;
}
MP_DEFINE_CONST_FUN_OBJ_1(mp_capsulad_handle_info_obj, mp_capsulad_handle_info);

static mp_obj_t mp_capsulad_register_descriptors(mp_obj_t bytes_obj) {
  capsulad_host *h = require_host();
  size_t len = 0;
  const uint8_t *data = arg_bytes(bytes_obj, &len);
  int32_t status = h->api->client_register_descriptors(h->client, data, len);
  if (status != CAPSULAD_STATUS_OK) {
    raise_status(status);
  }
  return mp_const_none;
}
MP_DEFINE_CONST_FUN_OBJ_1(mp_capsulad_register_descriptors_obj,
                          mp_capsulad_register_descriptors);

static void dict_set_str(mp_obj_t dict, const char *key, const char *value) {
  if (value == NULL) {
    mp_obj_dict_store(dict, mp_obj_new_str(key, strlen(key)), mp_const_none);
    return;
  }
  mp_obj_dict_store(dict, mp_obj_new_str(key, strlen(key)),
                    mp_obj_new_str(value, strlen(value)));
}

static mp_obj_t mp_capsulad_env(void) {
  mp_obj_t dict = mp_obj_new_dict(6);
  dict_set_str(dict, "socket", getenv("CAPSULAD_SOCKET"));
  dict_set_str(dict, "runId", getenv("CAPSULAD_RUN_ID"));
  dict_set_str(dict, "appId", getenv("CAPSULAD_APP_ID"));
  dict_set_str(dict, "sdk", getenv("CAPSULAD_SDK"));
  dict_set_str(dict, "library", getenv("CAPSULAD_LIBRARY"));
  dict_set_str(dict, "appDir", getenv("CAPSULAD_APP_DIR"));
  return dict;
}
MP_DEFINE_CONST_FUN_OBJ_0(mp_capsulad_env_obj, mp_capsulad_env);

static const struct {
  const char *name;
  const void *function;
} capsulad_module_functions[] = {
    {"_start", &mp_capsulad_start_obj},
    {"_poll", &mp_capsulad_poll_obj},
    {"_stopped", &mp_capsulad_stopped_obj},
    {"_disconnected", &mp_capsulad_disconnected_obj},
    {"_connected", &mp_capsulad_connected_obj},
    {"_ready", &mp_capsulad_ready_obj},
    {"_disconnect_status", &mp_capsulad_disconnect_status_obj},
    {"_disconnect_detail", &mp_capsulad_disconnect_detail_obj},
    {"_last_status", &mp_capsulad_last_status_obj},
    {"_last_error", &mp_capsulad_last_error_obj},
    {"_stop", &mp_capsulad_stop_obj},
    {"_log", &mp_capsulad_log_obj},
    {"_unary", &mp_capsulad_unary_obj},
    {"_unary_proto", &mp_capsulad_unary_proto_obj},
    {"_call_start", &mp_capsulad_call_start_obj},
    {"_call_start_routed", &mp_capsulad_call_start_routed_obj},
    {"_send_json", &mp_capsulad_send_json_obj},
    {"_send_proto", &mp_capsulad_send_proto_obj},
    {"_half_close", &mp_capsulad_half_close_obj},
    {"_recv_json", &mp_capsulad_recv_json_obj},
    {"_recv_proto", &mp_capsulad_recv_proto_obj},
    {"_end", &mp_capsulad_end_obj},
    {"_cancel", &mp_capsulad_cancel_obj},
    {"_release", &mp_capsulad_release_obj},
    {"_call_info", &mp_capsulad_call_info_obj},
    {"_call_status", &mp_capsulad_call_status_obj},
    {"_notify_invocation", &mp_capsulad_notify_invocation_obj},
    {"_notify_action", &mp_capsulad_notify_action_obj},
    {"_client_info", &mp_capsulad_client_info_obj},
    {"_handle_info", &mp_capsulad_handle_info_obj},
    {"_register_descriptors", &mp_capsulad_register_descriptors_obj},
    {"_env", &mp_capsulad_env_obj},
};

void capsulad_module_init(void) {
  mp_obj_t module = mp_obj_new_module(qstr_from_str("capsulad"));
  for (size_t i = 0;
       i < sizeof(capsulad_module_functions) / sizeof(capsulad_module_functions[0]);
       i++) {
    mp_store_attr(module, qstr_from_str(capsulad_module_functions[i].name),
                  MP_OBJ_FROM_PTR(capsulad_module_functions[i].function));
  }
  mp_obj_dict_store(MP_OBJ_FROM_PTR(&MP_STATE_VM(mp_loaded_modules_dict)),
                    MP_OBJ_NEW_QSTR(qstr_from_str("capsulad")), module);
}
