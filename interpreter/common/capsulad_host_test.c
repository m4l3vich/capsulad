/*
 * Host queue test: a bus disconnect must become exactly one bounded terminal
 * job that the script loop can dispatch, while capsulad_host_disconnect_status
 * keeps the same terminal state for run()'s return value.
 *
 * This includes the implementation directly so the static callback is covered
 * without loading libcapsulad or running a daemon.
 */
#include <assert.h>
#include <string.h>

#include "capsulad_host.c"

int main(void) {
  capsulad_host h;
  memset(&h, 0, sizeof(h));
  assert(pthread_mutex_init(&h.mu, NULL) == 0);
  assert(pthread_cond_init(&h.cv, NULL) == 0);

  on_disconnected(CAPSULAD_HANDLE_INVALID, 13, "daemon gone", &h);

  capsulad_host_job job;
  int got = capsulad_host_poll(&h, 0, &job);
  assert(got == 1);
  assert(job.kind == CAPSULAD_HOST_JOB_DISCONNECTED);
  assert(job.status == 13);
  assert(job.detail != NULL && strcmp(job.detail, "daemon gone") == 0);
  capsulad_host_free_job(&job);

  /* A repeated callback must not queue a second terminal event. */
  on_disconnected(CAPSULAD_HANDLE_INVALID, 13, "daemon gone", &h);
  assert(capsulad_host_poll(&h, 0, &job) == 0);

  int32_t status = 0;
  char detail[64] = {0};
  assert(capsulad_host_disconnect_status(&h, &status, detail, sizeof(detail)) == 1);
  assert(status == 13);
  assert(strcmp(detail, "daemon gone") == 0);
  assert(capsulad_host_disconnected(&h) == 1);

  pthread_cond_destroy(&h.cv);
  pthread_mutex_destroy(&h.mu);
  return 0;
}
