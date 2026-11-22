/*
 * Slash-relative path validation shared by the script hosts. It is pure,
 * allocation-free and independent of libcapsulad, so a host can reject a path
 * before touching the filesystem and the rules have one tested definition.
 */
#ifndef CAPSULAD_PATHS_H
#define CAPSULAD_PATHS_H

#include <stddef.h>
#include <string.h>

#define CAPSULAD_MAX_RELATIVE_PATH_BYTES 4096u

/* A relative path names one or more non-empty components. Dot, dot-dot,
 * backslash, NUL and a leading slash are rejected. */
static inline int capsulad_relative_path_ok(const char *path, size_t len) {
  if (len == 0 || len > CAPSULAD_MAX_RELATIVE_PATH_BYTES || path[0] == '/') {
    return 0;
  }
  size_t start = 0;
  for (size_t i = 0; i <= len; i++) {
    if (i == len || path[i] == '/') {
      size_t component = i - start;
      if (component == 0) {
        return 0;
      }
      if (component == 1 && path[start] == '.') {
        return 0;
      }
      if (component == 2 && path[start] == '.' && path[start + 1] == '.') {
        return 0;
      }
      start = i + 1;
    } else if (path[i] == '\0' || path[i] == '\\') {
      return 0;
    }
  }
  return 1;
}

/* Resolve the canonical app-visible asset path /assets/<asset-id> to the
 * store-relative asset id. A host path, a bare /assets or an escaping
 * component is refused. */
static inline int capsulad_asset_relative_path(const char *path, size_t len,
                                               const char **relative,
                                               size_t *relative_len) {
  static const char prefix[] = "/assets/";
  const size_t prefix_len = sizeof(prefix) - 1;
  if (len <= prefix_len || path == NULL ||
      memcmp(path, prefix, prefix_len) != 0) {
    return 0;
  }
  const char *rest = path + prefix_len;
  size_t rest_len = len - prefix_len;
  if (!capsulad_relative_path_ok(rest, rest_len)) {
    return 0;
  }
  if (relative != NULL) {
    *relative = rest;
  }
  if (relative_len != NULL) {
    *relative_len = rest_len;
  }
  return 1;
}

#endif
