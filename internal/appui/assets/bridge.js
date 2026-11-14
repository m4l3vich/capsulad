/*
 * Capsula settings bridge (version 1), injected into every settings document.
 *
 * This script is trusted host code running in an opaque-origin frame. It:
 *  - accepts exactly one init message from window.parent and locks the
 *    per-frame channel token;
 *  - exposes window.CapsulaUI with state/result listeners and a patch method
 *    that can only request setting-value changes;
 *  - reports its content height for embedding.
 *
 * It has no network, storage, cookie or parent-DOM access, and performs no
 * privileged operation itself.
 */
(() => {
  'use strict';

  const PREFIX = 'capsula.settings-ui/1:';
  const stateListeners = new Set();
  const resultListeners = new Set();
  let channel = null;
  let seq = 0;

  function post(body) {
    // Never post before init locks the channel. DOMContentLoaded/ResizeObserver
    // can fire before the host's load-event init; those early resizes are
    // re-sent explicitly after init.
    if (channel === null) return;
    // Build the envelope after the body so body.type cannot overwrite the
    // namespaced protocol type.
    const message = Object.assign({}, body, { type: PREFIX + body.type, channel });
    parent.postMessage(message, '*');
  }

  function dispatch(list, value) {
    for (const fn of list) {
      try { fn(value); } catch (err) { console.error('CapsulaUI listener failed', err); }
    }
  }

  const api = {
    apiVersion: 1,
    appId: '',
    schema: { settings: [] },
    revision: 0,
    onState(fn) {
      if (typeof fn === 'function') stateListeners.add(fn);
      return () => stateListeners.delete(fn);
    },
    onResult(fn) {
      if (typeof fn === 'function') resultListeners.add(fn);
      return () => resultListeners.delete(fn);
    },
    patch(changes) {
      if (!Array.isArray(changes) || changes.length === 0) return 0;
      seq += 1;
      post({ type: 'patch', requestId: seq, changes });
      return seq;
    },
    resize() {
      const height = Math.ceil(Math.max(
        document.documentElement.scrollHeight,
        document.body ? document.body.scrollHeight : 0
      ));
      post({ type: 'resize', height });
    }
  };

  window.addEventListener('message', (event) => {
    if (event.source !== parent) return;
    const data = event.data;
    if (!data || typeof data !== 'object' || typeof data.type !== 'string') return;

    if (data.type === PREFIX + 'init') {
      if (channel !== null) return;
      if (typeof data.channel !== 'string' || !/^[0-9a-f]{32}$/.test(data.channel)) return;
      channel = data.channel;
      api.appId = typeof data.appId === 'string' ? data.appId : '';
      api.schema = data.schema && typeof data.schema === 'object' ? data.schema : { settings: [] };
      api.revision = Number(data.revision) || 0;
      dispatch(stateListeners, data);
      post({ type: 'ready' });
      api.resize();
      return;
    }

    if (channel === null || data.channel !== channel) return;
    if (data.type === PREFIX + 'state') {
      api.revision = Number(data.revision) || api.revision;
      dispatch(stateListeners, data);
      api.resize();
    } else if (data.type === PREFIX + 'result') {
      dispatch(resultListeners, data);
    }
  });

  api.onState((state) => {
    if (state && Array.isArray(state.schema?.settings)) api.schema = state.schema;
  });

  window.addEventListener('resize', () => api.resize());
  window.addEventListener('DOMContentLoaded', () => api.resize());
  if (window.ResizeObserver) {
    new ResizeObserver(() => api.resize()).observe(document.documentElement);
  }

  window.CapsulaUI = api;
})();
