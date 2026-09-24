'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const path = require('node:path');
const source = fs.readFileSync(path.join(__dirname, '../ui/assets/bridge-v1.js'), 'utf8');

function harness(overrides = {}) {
  const listeners = new Map();
  const timers = new Map();
  const posted = [];
  let nextTimer = 0;
  const parent = { postMessage: (data, origin) => posted.push({ data, origin }) };
  const window = {
    location: { hash: '#bridge_token=private-bridge-token', href: 'https://sub2.example/api/v1/plugin-ui/asset/index.html' },
    document: { referrer: 'https://sub2.example/admin/plugins' }, parent,
    crypto: { getRandomValues: bytes => bytes.fill(127) },
    setTimeout: fn => { timers.set(++nextTimer, fn); return nextTimer; },
    clearTimeout: id => timers.delete(id),
    addEventListener: (type, fn) => listeners.set(type, fn),
    removeEventListener: (type, fn) => { if (listeners.get(type) === fn) listeners.delete(type); },
    ...overrides,
  };
  vm.runInNewContext(source, { window, URL, URLSearchParams, Uint32Array });
  function respond(index = posted.length - 1, overrides = {}, eventOverrides = {}) {
    const request = posted[index].data;
    const data = { source: 'sub2api-plugin-host', bridge_token: request.bridge_token,
      request_id: request.request_id, type: request.type + '.result', ok: true, config: { enabled: false }, ...overrides };
    const fn = listeners.get('message');
    if (fn) fn({ source: parent, origin: 'https://sub2.example', data, ...eventOverrides });
  }
  return { window, bridge: window.Sub2APIPluginBridge, posted, timers, listeners, respond };
}

test('bridge accepts only matching parent, origin, token, type and pending request', async () => {
  const h = harness();
  const promise = h.bridge.load();
  assert.equal(h.posted[0].origin, 'https://sub2.example');
  assert.equal(h.posted[0].data.type, 'config.load');
  h.respond(0, {}, { source: {} });
  h.respond(0, {}, { origin: 'https://attacker.example' });
  h.respond(0, { source: 'wrong-source' });
  h.respond(0, { bridge_token: 'wrong-token' });
  h.respond(0, { request_id: 'unknown-request' });
  h.respond(0, { type: 'config.save.result' });
  assert.equal(h.timers.size, 1);
  h.respond(0);
  const result = await promise;
  assert.equal(result.config.enabled, false);
  assert.equal(h.timers.size, 0);
  h.respond(0, { config: { enabled: true } });
  assert.equal(result.config.enabled, false);
  h.bridge.dispose();
});

test('bridge requests time out and remove pending requests', async () => {
  const h = harness();
  const promise = h.bridge.status();
  const rejected = assert.rejects(promise, /响应超时/);
  for (const [id, callback] of h.timers) { h.timers.delete(id); callback(); }
  await rejected;
  h.respond(0, { result: { healthy: true } });
  h.bridge.dispose();
});

test('pagehide rejects pending work and removes event listeners and timers', async () => {
  const h = harness();
  const promise = h.bridge.save({ enabled: false });
  const rejected = assert.rejects(promise, /已关闭/);
  h.listeners.get('pagehide')();
  await rejected;
  assert.equal(h.timers.size, 0);
  assert.equal(h.listeners.size, 0);
  await assert.rejects(h.bridge.load(), /已关闭/);
});

test('test uses saved config without transmitting unsaved form data', async () => {
  const h = harness();
  const promise = h.bridge.test();
  assert.equal(h.posted[0].data.type, 'config.test');
  assert.equal(Object.hasOwn(h.posted[0].data, 'config'), false);
  h.respond(0, { result: { success: true, message: 'ok' } });
  assert.equal((await promise).result.success, true);
  h.bridge.dispose();
});

test('bridge does not call host methods absent from Sub2API 0.2.8', () => {
  const h = harness();
  assert.equal(typeof h.bridge.proxies, 'undefined');
  assert.equal(typeof h.bridge.accounts, 'undefined');
  assert.equal(h.posted.length, 0);
  h.bridge.dispose();
});

test('bridge rejects missing token and refuses non-web parent origin', async () => {
  const absent = harness({ location: { hash: '', href: 'https://sub2.example/index.html' } });
  await assert.rejects(absent.bridge.load(), /配置页打开/);
  assert.equal(absent.posted.length, 0);
  absent.bridge.dispose();
  const nonweb = harness({ document: { referrer: 'file:///tmp/host.html' } });
  await assert.rejects(nonweb.bridge.load(), /配置页打开/);
  assert.equal(nonweb.posted.length, 0);
  nonweb.bridge.dispose();
});

test('failed host response is surfaced without treating it as successful config', async () => {
  const h = harness();
  const promise = h.bridge.test();
  const rejected = assert.rejects(promise, /未初始化/);
  h.respond(0, { ok: false, result: { message: '宿主未初始化' } });
  await rejected;
  h.bridge.dispose();
});
