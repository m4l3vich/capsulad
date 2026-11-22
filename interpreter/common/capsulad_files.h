/*
 * Secure read-only file opening below a configured store root.
 *
 * The asset store contract publishes regular, non-symlink files only. Resolving
 * a path and then stat/open-ing it would follow a symlink planted inside the
 * store. This helper instead walks the validated relative path one component at
 * a time with openat(O_NOFOLLOW) from a directory fd, so no path component can
 * be a symlink and the final fstat/read use the same descriptor that was
 * opened. A replacement after open cannot redirect the read.
 *
 * It is pure POSIX, independent of libcapsulad/QuickJS, and shared by the
 * script host and its focused test.
 */
#ifndef CAPSULAD_FILES_H
#define CAPSULAD_FILES_H

#include <errno.h>
#include <fcntl.h>
#include <stddef.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "capsulad_paths.h"

#ifndef O_CLOEXEC
#define O_CLOEXEC 0
#endif

#ifndef O_NOFOLLOW
#error "capsulad_files.h requires O_NOFOLLOW"
#endif

/* Open root/relative as a regular file, refusing symlinks in every component.
 * The caller owns the returned descriptor. Returns 0 on success, -1 with errno
 * set otherwise. O_NONBLOCK keeps a FIFO/socket from blocking before fstat can
 * reject its type. */
static inline int capsulad_open_regular_beneath(const char *root,
                                                const char *relative,
                                                size_t relative_len,
                                                int *out_fd) {
  if (out_fd == NULL) {
    errno = EINVAL;
    return -1;
  }
  *out_fd = -1;
  if (root == NULL || relative == NULL ||
      !capsulad_relative_path_ok(relative, relative_len)) {
    errno = EINVAL;
    return -1;
  }
  int dirfd = open(root, O_RDONLY | O_DIRECTORY | O_CLOEXEC);
  if (dirfd < 0) {
    return -1;
  }
  char *copy = malloc(relative_len + 1);
  if (copy == NULL) {
    int saved = errno;
    close(dirfd);
    errno = saved == 0 ? ENOMEM : saved;
    return -1;
  }
  memcpy(copy, relative, relative_len);
  copy[relative_len] = '\0';

  size_t start = 0;
  for (size_t i = 0; i <= relative_len; i++) {
    if (i != relative_len && copy[i] != '/') {
      continue;
    }
    copy[i] = '\0';
    int last = i == relative_len;
    int flags = O_RDONLY | O_NOFOLLOW | O_CLOEXEC;
    if (last) {
      flags |= O_NONBLOCK;
    } else {
      flags |= O_DIRECTORY;
    }
    int fd = openat(dirfd, copy + start, flags);
    if (fd < 0) {
      int saved = errno;
      close(dirfd);
      free(copy);
      errno = saved;
      return -1;
    }
    close(dirfd);
    dirfd = fd;
    if (last) {
      *out_fd = fd;
      free(copy);
      return 0;
    }
    start = i + 1;
  }

  /* capsulad_relative_path_ok rejects an empty path, so the loop always
   * returns for a valid input. Keep the cleanup explicit for the compiler. */
  close(dirfd);
  free(copy);
  errno = EINVAL;
  return -1;
}

#endif
