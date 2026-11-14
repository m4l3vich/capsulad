#define _GNU_SOURCE 1

#include "capsula_host.h"

#include <dlfcn.h>
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#define HOST_QUEUE_MAX 256
#define HOST_DETAIL_MAX 512

static void *library_handle;
static capsula_api api_table;

typedef int32_t (*capsula_get_api_fn)(uint32_t abi_major, size_t table_size,
                                       capsula_api *out);

static void copy_detail(char *dst, size_t dst_len, const char *src) {
  if (dst_len == 0) {
    return;
  }
  if (src == NULL) {
    src = "";
  }
  snprintf(dst, dst_len, "%s", src);
}

size_t capsula_host_text_len(const uint8_t *data, size_t len) {
  if (data != NULL && len > 0 && data[len - 1] == 0) {
    return len - 1;
  }
  return len;
}

static size_t bounded_len(const char *s, size_t max) {
  if (s == NULL) {
    return 0;
  }
  size_t len = strlen(s);
  return len > max ? max : len;
}

int capsula_host_load(capsula_host *h, char *err, size_t errlen) {
  if (h == NULL) {
    return -1;
  }
  memset(h, 0, sizeof(*h));
  pthread_mutex_init(&h->mu, NULL);
  pthread_cond_init(&h->cv, NULL);

  const char *path = getenv("CAPSULA_LIBRARY");
  if (path == NULL || path[0] == '\0') {
    path = CAPSULA_DEFAULT_LIBRARY;
  }
  library_handle = dlopen(path, RTLD_NOW | RTLD_LOCAL);
  if (library_handle == NULL) {
    const char *why = dlerror();
    snprintf(err, errlen, "cannot load %s: %s", path,
             why != NULL ? why : "unknown loader error");
    return -1;
  }

  dlerror();
  capsula_get_api_fn get_api =
      (capsula_get_api_fn)dlsym(library_handle, "capsula_get_api");
  if (get_api == NULL) {
    snprintf(err, errlen,
             "%s does not export capsula_get_api; install the public Capsula "
             "native SDK",
             path);
    return -1;
  }

  capsula_api table;
  memset(&table, 0, sizeof(table));
  int32_t status = get_api(CAPSULA_ABI_MAJOR, sizeof(table), &table);
  if (status != CAPSULA_STATUS_OK) {
    snprintf(err, errlen, "%s rejected ABI major %u (status %d)", path,
             CAPSULA_ABI_MAJOR, status);
    return -1;
  }
  if (table.abi_major != CAPSULA_ABI_MAJOR) {
    snprintf(err, errlen, "%s reports ABI major %u, need %u", path,
             table.abi_major, CAPSULA_ABI_MAJOR);
    return -1;
  }
  if (table.struct_size < sizeof(capsula_api)) {
    snprintf(err, errlen,
             "%s provides a shorter ABI table (%zu bytes, need %zu)", path,
             table.struct_size, sizeof(capsula_api));
    return -1;
  }

  const void *required[] = {
      (const void *)table.client_connect,
      (const void *)table.client_close,
      (const void *)table.client_register_descriptors,
      (const void *)table.client_info,
      (const void *)table.client_unary,
      (const void *)table.client_unary_proto,
      (const void *)table.call_start,
      (const void *)table.call_start_routed,
      (const void *)table.call_send_json,
      (const void *)table.call_send_proto,
      (const void *)table.call_half_close,
      (const void *)table.call_recv_json,
      (const void *)table.call_recv_proto,
      (const void *)table.call_end,
      (const void *)table.call_cancel,
      (const void *)table.call_retain,
      (const void *)table.call_release,
      (const void *)table.call_info,
      (const void *)table.call_status,
      (const void *)table.bus_open,
      (const void *)table.bus_close,
      (const void *)table.bus_ready,
      (const void *)table.bus_notify_invocation,
      (const void *)table.bus_notify_action,
      (const void *)table.buf_free,
      (const void *)table.last_error,
      (const void *)table.handle_info,
  };
  for (size_t i = 0; i < sizeof(required) / sizeof(required[0]); i++) {
    if (required[i] == NULL) {
      snprintf(err, errlen, "%s has a NULL required ABI entry at index %zu",
               path, i);
      return -1;
    }
  }

  api_table = table;
  h->api = &api_table;
  return 0;
}

int capsula_host_error(capsula_host *h, int32_t status, char *err,
                       size_t errlen) {
  const char *detail = "";
  if (h != NULL && h->api != NULL && h->api->last_error != NULL) {
    const char *last = h->api->last_error();
    if (last != NULL) {
      detail = last;
    }
  }
  if (h != NULL) {
    h->last_status = status;
    copy_detail(h->last_detail, sizeof(h->last_detail), detail);
  }
  snprintf(err, errlen, "%s (status %d)",
           detail[0] != '\0' ? detail : "capsula SDK call failed", status);
  return status;
}

static void on_ready(capsula_handle bus, void *user_data) {
  (void)bus;
  capsula_host *h = (capsula_host *)user_data;
  pthread_mutex_lock(&h->mu);
  h->bus_ready = 1;
  pthread_cond_broadcast(&h->cv);
  pthread_mutex_unlock(&h->mu);
}

static void on_disconnected(capsula_handle bus, int32_t status,
                            const char *detail, void *user_data) {
  (void)bus;
  capsula_host *h = (capsula_host *)user_data;
  if (detail == NULL) {
    detail = "";
  }
  size_t detail_len = strlen(detail);
  if (detail_len > HOST_DETAIL_MAX) {
    detail_len = HOST_DETAIL_MAX;
  }
  // The callback must not run user code. Queue one bounded terminal job so the
  // script loop can invoke its disconnect handler with the observed status and
  // detail; run() still returns the same state if the job cannot be queued.
  capsula_host_job *job = (capsula_host_job *)calloc(1, sizeof(*job));
  if (job != NULL) {
    job->kind = CAPSULA_HOST_JOB_DISCONNECTED;
    job->status = status;
    job->detail = (char *)malloc(detail_len + 1);
    if (job->detail != NULL) {
      memcpy(job->detail, detail, detail_len);
      job->detail[detail_len] = '\0';
    }
  }

  pthread_mutex_lock(&h->mu);
  int first = !h->disconnected;
  h->disconnected = 1;
  h->disconnect_status = status;
  copy_detail(h->disconnect_detail, sizeof(h->disconnect_detail), detail);
  if (!first || h->stopped || h->queued >= HOST_QUEUE_MAX) {
    free(job != NULL ? job->detail : NULL);
    free(job);
    job = NULL;
  } else if (job != NULL) {
    if (h->tail != NULL) {
      h->tail->next = job;
    } else {
      h->head = job;
    }
    h->tail = job;
    h->queued++;
  }
  pthread_cond_broadcast(&h->cv);
  pthread_mutex_unlock(&h->mu);
  fprintf(stderr, "capsula-host: app bus disconnected (status %d%s%s)\n",
          status, detail[0] != '\0' ? ": " : "", detail);
  fflush(stderr);
}

static void on_event(capsula_handle bus, int32_t event_kind,
                     const uint8_t *json, size_t json_len, void *user_data) {
  (void)bus;
  capsula_host *h = (capsula_host *)user_data;
  capsula_host_job *job = (capsula_host_job *)calloc(1, sizeof(*job));
  if (job == NULL) {
    return;
  }
  job->kind = CAPSULA_HOST_JOB_EVENT;
  job->event_kind = event_kind;
  json_len = capsula_host_text_len(json, json_len);
  if (json != NULL && json_len > 0) {
    job->json = (uint8_t *)malloc(json_len);
    if (job->json == NULL) {
      free(job);
      return;
    }
    memcpy(job->json, json, json_len);
    job->json_len = json_len;
  }

  pthread_mutex_lock(&h->mu);
  if (h->stopped || h->disconnected || h->queued >= HOST_QUEUE_MAX) {
    pthread_mutex_unlock(&h->mu);
    free(job->json);
    free(job);
    fprintf(stderr, "capsula-host: dropped event %d (queue full or closing)\n",
            event_kind);
    fflush(stderr);
    return;
  }
  if (h->tail != NULL) {
    h->tail->next = job;
  } else {
    h->head = job;
  }
  h->tail = job;
  h->queued++;
  pthread_cond_broadcast(&h->cv);
  pthread_mutex_unlock(&h->mu);
}

static void on_call(capsula_handle bus, capsula_handle call,
                    const uint8_t *info_json, size_t info_len,
                    void *user_data) {
  (void)bus;
  capsula_host *h = (capsula_host *)user_data;
  capsula_host_job *job = (capsula_host_job *)calloc(1, sizeof(*job));
  int queued = 0;
  if (job != NULL) {
    job->kind = CAPSULA_HOST_JOB_CALL;
    job->call = call;
    info_len = capsula_host_text_len(info_json, info_len);
    if (info_json != NULL && info_len > 0) {
      job->info = (uint8_t *)malloc(info_len);
      if (job->info != NULL) {
        memcpy(job->info, info_json, info_len);
        job->info_len = info_len;
      }
    }
    pthread_mutex_lock(&h->mu);
    if (!h->stopped && !h->disconnected && h->queued < HOST_QUEUE_MAX) {
      if (h->tail != NULL) {
        h->tail->next = job;
      } else {
        h->head = job;
      }
      h->tail = job;
      h->queued++;
      queued = 1;
      /* The library releases the callback's reference after on_call returns.
       * Retain one reference for the script thread; end/cancel/release in the
       * SDK releases it. */
      h->api->call_retain(call);
      pthread_cond_broadcast(&h->cv);
    }
    pthread_mutex_unlock(&h->mu);
  }
  if (!queued) {
    if (job != NULL) {
      free(job->info);
      free(job);
    }
    /* An inbound call must never hang because the script loop is saturated. */
    h->api->call_end(call, 8 /* RESOURCE_EXHAUSTED */,
                     "interpreter host call queue is full", 45);
    h->api->call_release(call);
    fprintf(stderr, "capsula-host: rejected inbound call (queue full)\n");
    fflush(stderr);
  }
}

static void on_log(int32_t level, const char *message, void *user_data) {
  (void)user_data;
  fprintf(stderr, "capsula-host[%d]: %s\n", level,
          message != NULL ? message : "");
  fflush(stderr);
}

int capsula_host_connect(capsula_host *h, uint32_t timeout_ms, char *err,
                         size_t errlen) {
  if (h == NULL || h->api == NULL) {
    snprintf(err, errlen, "interpreter host is not loaded");
    return -1;
  }
  if (h->client_connected) {
    return 0;
  }
  const char *socket_path = getenv("CAPSULA_SOCKET");
  if (socket_path == NULL || socket_path[0] == '\0') {
    socket_path = CAPSULA_DEFAULT_SOCKET;
  }
  const char *run_id = getenv("CAPSULA_RUN_ID");
  const char *app_id = getenv("CAPSULA_APP_ID");
  if (run_id == NULL || run_id[0] == '\0') {
    snprintf(err, errlen,
             "CAPSULA_RUN_ID is required; interpreter hosts run as a "
             "supervised app, not as a standalone client");
    return -1;
  }
  if (app_id == NULL) {
    app_id = "";
  }

  capsula_handle client = CAPSULA_HANDLE_INVALID;
  int32_t status = h->api->client_connect(
      socket_path, bounded_len(socket_path, 4096), run_id,
      bounded_len(run_id, 512), app_id, bounded_len(app_id, 512), timeout_ms,
      &client);
  if (status != CAPSULA_STATUS_OK) {
    capsula_host_error(h, status, err, errlen);
    return -1;
  }
  h->client = client;
  h->client_connected = 1;
  return 0;
}

int capsula_host_start(capsula_host *h, uint32_t timeout_ms, char *err,
                       size_t errlen) {
  if (h == NULL || h->api == NULL) {
    snprintf(err, errlen, "interpreter host is not loaded");
    return -1;
  }
  if (h->bus != CAPSULA_HANDLE_INVALID && h->bus_ready) {
    return 0;
  }
  if (!h->client_connected) {
    if (capsula_host_connect(h, timeout_ms, err, errlen) != 0) {
      return -1;
    }
  }

  capsula_bus_callbacks_v1 callbacks;
  memset(&callbacks, 0, sizeof(callbacks));
  callbacks.struct_size = sizeof(callbacks);
  callbacks.user_data = h;
  callbacks.on_ready = on_ready;
  callbacks.on_disconnected = on_disconnected;
  callbacks.on_event = on_event;
  callbacks.on_call = on_call;
  callbacks.on_log = on_log;

  capsula_handle bus = CAPSULA_HANDLE_INVALID;
  int32_t status = h->api->bus_open(h->client, &callbacks, &bus);
  if (status != CAPSULA_STATUS_OK) {
    capsula_host_error(h, status, err, errlen);
    return -1;
  }
  h->bus = bus;

  status = h->api->bus_ready(bus, timeout_ms);
  if (status != CAPSULA_STATUS_OK) {
    capsula_host_error(h, status, err, errlen);
    return -1;
  }
  pthread_mutex_lock(&h->mu);
  h->bus_ready = 1;
  pthread_mutex_unlock(&h->mu);
  return 0;
}

static void drain_queue(capsula_host *h, int notify_calls) {
  capsula_host_job *job = NULL;
  pthread_mutex_lock(&h->mu);
  job = h->head;
  h->head = NULL;
  h->tail = NULL;
  h->queued = 0;
  pthread_mutex_unlock(&h->mu);

  while (job != NULL) {
    capsula_host_job *next = job->next;
    if (job->kind == CAPSULA_HOST_JOB_CALL && notify_calls &&
        h->api != NULL) {
      h->api->call_end(job->call, 14 /* UNAVAILABLE */,
                       "interpreter host is stopping", 29);
      h->api->call_release(job->call);
    }
    capsula_host_free_job(job);
    free(job);
    job = next;
  }
}

void capsula_host_free_job(capsula_host_job *job) {
  if (job == NULL) {
    return;
  }
  free(job->json);
  free(job->info);
  free(job->detail);
  job->json = NULL;
  job->info = NULL;
  job->detail = NULL;
}

int capsula_host_poll(capsula_host *h, int timeout_ms, capsula_host_job *out) {
  if (h == NULL || out == NULL) {
    return 0;
  }
  memset(out, 0, sizeof(*out));

  struct timespec deadline;
  int have_deadline = 0;
  if (timeout_ms > 0) {
    clock_gettime(CLOCK_REALTIME, &deadline);
    deadline.tv_sec += timeout_ms / 1000;
    deadline.tv_nsec += (long)(timeout_ms % 1000) * 1000000L;
    if (deadline.tv_nsec >= 1000000000L) {
      deadline.tv_sec++;
      deadline.tv_nsec -= 1000000000L;
    }
    have_deadline = 1;
  }

  pthread_mutex_lock(&h->mu);
  while (h->head == NULL && !h->stopped && !h->disconnected) {
    if (timeout_ms == 0) {
      break;
    }
    if (have_deadline) {
      int rc = pthread_cond_timedwait(&h->cv, &h->mu, &deadline);
      if (rc == ETIMEDOUT) {
        break;
      }
    } else {
      pthread_cond_wait(&h->cv, &h->mu);
    }
  }
  if (h->head == NULL) {
    pthread_mutex_unlock(&h->mu);
    return 0;
  }
  capsula_host_job *job = h->head;
  h->head = job->next;
  if (h->head == NULL) {
    h->tail = NULL;
  }
  h->queued--;
  pthread_mutex_unlock(&h->mu);

  *out = *job;
  out->next = NULL;
  free(job);
  return 1;
}

int capsula_host_connected(capsula_host *h) {
  pthread_mutex_lock(&h->mu);
  int connected = h->client_connected && !h->disconnected && !h->stopped;
  pthread_mutex_unlock(&h->mu);
  return connected;
}

int capsula_host_disconnected(capsula_host *h) {
  pthread_mutex_lock(&h->mu);
  int disconnected = h->disconnected;
  pthread_mutex_unlock(&h->mu);
  return disconnected;
}

int capsula_host_stopped(capsula_host *h) {
  pthread_mutex_lock(&h->mu);
  int stopped = h->stopped;
  pthread_mutex_unlock(&h->mu);
  return stopped;
}

int capsula_host_bus_ready(capsula_host *h) {
  pthread_mutex_lock(&h->mu);
  int ready = h->bus_ready;
  pthread_mutex_unlock(&h->mu);
  return ready;
}

int capsula_host_disconnect_status(capsula_host *h, int32_t *status,
                                   char *detail, size_t detail_len) {
  pthread_mutex_lock(&h->mu);
  int disconnected = h->disconnected;
  if (status != NULL) {
    *status = h->disconnect_status;
  }
  if (disconnected) {
    copy_detail(detail, detail_len, h->disconnect_detail);
  }
  pthread_mutex_unlock(&h->mu);
  return disconnected;
}

void capsula_host_close(capsula_host *h) {
  if (h == NULL) {
    return;
  }
  pthread_mutex_lock(&h->mu);
  h->stopped = 1;
  pthread_cond_broadcast(&h->cv);
  pthread_mutex_unlock(&h->mu);

  /* End queued inbound calls while the bus/session is still alive. */
  drain_queue(h, 1);

  if (h->api != NULL) {
    if (h->bus != CAPSULA_HANDLE_INVALID) {
      h->api->bus_close(h->bus);
      h->bus = CAPSULA_HANDLE_INVALID;
    }
    if (h->client != CAPSULA_HANDLE_INVALID) {
      h->api->client_close(h->client);
      h->client = CAPSULA_HANDLE_INVALID;
    }
  }
  h->client_connected = 0;
  drain_queue(h, 0);
}
