/*
 * Focused regression for the script-host asset reader: a regular file below
 * the store root opens, but an in-root symlink to that same file (and a
 * symlinked directory component) is refused.
 */
/* Darwin needs _DARWIN_C_SOURCE for O_NOFOLLOW and mkdtemp under -std=c11;
 * Linux exposes them through _XOPEN_SOURCE 700. */
#if defined(__APPLE__)
#ifndef _DARWIN_C_SOURCE
#define _DARWIN_C_SOURCE 1
#endif
#elif !defined(_XOPEN_SOURCE) && !defined(_GNU_SOURCE)
#define _XOPEN_SOURCE 700
#endif

#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#include "capsulad_files.h"

#define TEST_PATH_BYTES 4096

static int write_all(const char *path, const char *text) {
  int fd = open(path, O_WRONLY | O_CREAT | O_TRUNC, 0600);
  if (fd < 0) {
    return -1;
  }
  size_t len = strlen(text);
  size_t off = 0;
  while (off < len) {
    ssize_t n = write(fd, text + off, len - off);
    if (n <= 0) {
      close(fd);
      return -1;
    }
    off += (size_t)n;
  }
  return close(fd);
}

static int read_ok(const char *root, const char *relative, const char *want) {
  int fd = -1;
  if (capsulad_open_regular_beneath(root, relative, strlen(relative), &fd) != 0) {
    return 0;
  }
  struct stat st;
  if (fstat(fd, &st) != 0 || !S_ISREG(st.st_mode)) {
    close(fd);
    return 0;
  }
  char buf[64] = {0};
  ssize_t n = read(fd, buf, sizeof(buf) - 1);
  close(fd);
  return n == (ssize_t)strlen(want) && memcmp(buf, want, (size_t)n) == 0;
}

static int refused(const char *root, const char *relative) {
  int fd = -1;
  if (capsulad_open_regular_beneath(root, relative, strlen(relative), &fd) == 0) {
    close(fd);
    return 0;
  }
  return 1;
}

int main(void) {
  char template[] = "/tmp/capsulad-files-XXXXXX";
  char *root = mkdtemp(template);
  if (root == NULL) {
    perror("mkdtemp");
    return 1;
  }

  char audio[TEST_PATH_BYTES];
  char target[TEST_PATH_BYTES];
  char link[TEST_PATH_BYTES];
  char alias[TEST_PATH_BYTES];
  snprintf(audio, sizeof(audio), "%s/audio", root);
  snprintf(target, sizeof(target), "%s/beep.wav", audio);
  snprintf(link, sizeof(link), "%s/link.wav", root);
  snprintf(alias, sizeof(alias), "%s/alias", root);

  int failed = 0;
  if (mkdir(audio, 0700) != 0 || write_all(target, "pcm") != 0) {
    perror("fixture");
    failed = 1;
    goto cleanup;
  }
  if (!read_ok(root, "audio/beep.wav", "pcm")) {
    fprintf(stderr, "regular nested file did not open\n");
    failed = 1;
    goto cleanup;
  }
  if (symlink(target, link) != 0) {
    perror("symlink file");
    failed = 1;
    goto cleanup;
  }
  if (!refused(root, "link.wav")) {
    fprintf(stderr, "in-root symlink to a regular file was followed\n");
    failed = 1;
    goto cleanup;
  }
  if (symlink(audio, alias) != 0) {
    perror("symlink directory");
    failed = 1;
    goto cleanup;
  }
  if (!refused(root, "alias/beep.wav")) {
    fprintf(stderr, "symlinked directory component was followed\n");
    failed = 1;
    goto cleanup;
  }

cleanup:
  unlink(link);
  unlink(alias);
  unlink(target);
  rmdir(audio);
  rmdir(root);
  return failed;
}
