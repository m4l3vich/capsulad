import test from 'node:test'
import assert from 'node:assert/strict'
import { MAX_PACKAGE_BYTES, packageFileError, packageURLError } from '../src/lib/apps.js'

// The browser refuses input the device would refuse, before opening a request.
// Package URLs are limited to absolute http(s); the daemon repeats the check
// and owns the download.

test('only absolute http and https package URLs pass', () => {
  assert.equal(packageURLError('https://example.com/app-1.0.0.tar.gz'), '')
  assert.equal(packageURLError('http://192.168.1.2:8000/app.tar.gz'), '')
  assert.equal(packageURLError('  https://example.com/app.tar.gz  '), '')
})

test('other schemes, relative paths and credential URLs are refused', () => {
  for (const value of [
    '',
    'file:///etc/passwd',
    'data:application/gzip;base64,H4sI',
    'ftp://example.com/app.tar.gz',
    '/app.tar.gz',
    'https:///app.tar.gz',
    'https://user:secret@example.com/app.tar.gz',
    'https://example.com/app.tar.gz#fragment',
    'not a url'
  ]) {
    assert.notEqual(packageURLError(value), '', `expected ${JSON.stringify(value)} to be refused`)
  }
})

test('the package archive bound matches the transport limit', () => {
  assert.equal(MAX_PACKAGE_BYTES, 65 * 1024 * 1024)
  assert.equal(packageFileError({ size: 0 }), 'The archive is empty.')
  assert.equal(packageFileError({ size: MAX_PACKAGE_BYTES }), '')
  assert.match(packageFileError({ size: MAX_PACKAGE_BYTES + 1 }), /at most 65 MiB/)
})
