'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const ui = require('../ui/assets/app.js');

function configured(overrides = {}) {
  return { ...ui.DEFAULT_CONFIG, accounts: [{ account_id: 7, name: '示例账号', email: 'owner@example.com',
    expires_at: '2026-12-31 23:59', quota: '$12.50 / $20.00', enabled: false, egress_mode: 'sub2', sticky_proxy_url: '',
    plan: 'pro', models: ['gpt-6-astra'] }], ...overrides };
}

test('empty configuration and newly imported accounts default off', () => {
  assert.deepEqual(ui.normalizeConfig({}), { ...ui.DEFAULT_CONFIG, accounts: [] });
  assert.equal(ui.normalizeConfig({ accounts: [{ account_id: 7 }] }).accounts[0].enabled, false);
  assert.equal(ui.normalizeConfig({ accounts: [{ account_id: 7 }] }).accounts[0].plan, 'pro');
  assert.equal(ui.normalizeConfig({ accounts: [{ account_id: 7 }] }).accounts[0].egress_mode, 'sub2');
  assert.equal(ui.normalizeConfig({ accounts: [{ account_id: 7, name: ' Example ' }] }).accounts[0].name, 'Example');
  assert.equal(ui.normalizeConfig({}).prefer_previous_ip, false);
  assert.equal(ui.normalizeConfig({}).allow_state_780, true);
  assert.equal(ui.normalizeConfig({}).state_780_ttl_seconds, 240);
  assert.equal(ui.normalizeConfig({}).gateway_policy, 'allow');
  assert.equal(ui.normalizeConfig({}).target_gateway, 'unified-15,unified-88,unified-180');
  assert.equal(ui.normalizeConfig({ target_gateway: 'any' }).gateway_policy, 'any');
  assert.equal(ui.normalizeConfig({ target_gateway: '' }).gateway_policy, 'any');
  assert.equal(ui.normalizeConfig({ gateway_policy: 'deny', target_gateway: '88' }).gateway_policy, 'deny');
  assert.equal(ui.normalizeConfig({}).route_cookie_reuse, true);
  assert.equal(ui.normalizeConfig({}).mint_fingerprint_convergence, true);
  assert.equal(ui.normalizeConfig({}).quality_probe_enabled, false);
  assert.equal(ui.normalizeConfig({}).quality_probe_mode, 'off');
  assert.equal(ui.normalizeConfig({}).quality_probe_prompt, '');
  assert.equal(ui.normalizeConfig({}).quality_probe_accept, '');
  assert.equal(ui.normalizeConfig({}).ticket_mode, 'cookie');
  assert.equal(ui.normalizeConfig({ ticket_mode: 'legacy', allow_state_780: false }).ticket_mode, 'cookie');
  assert.equal(ui.normalizeConfig({ ticket_mode: 'legacy', allow_state_780: false }).allow_state_780, true);
  assert.equal(ui.normalizeConfig({}).cookie_capture_mode, 'generator');
  assert.equal(ui.normalizeConfig({}).cookie_ticket_ttl_seconds, 240);
  assert.equal(ui.normalizeConfig({}).standby_lead_seconds, 90);
  assert.equal(ui.normalizeConfig({}).mint_concurrency, 3);
  assert.equal(ui.normalizeConfig({}).request_rewrite_enabled, false);
  assert.equal(ui.normalizeConfig({}).default_request_timezone, 'Asia/Singapore');
  assert.equal(ui.validateConfig(configured()).enabled, false);
});

test('request timezone rewrite validates account overrides and restricted regions', () => {
  const config = configured({ request_rewrite_enabled: true, default_request_timezone: 'Asia/Singapore' });
  assert.equal(ui.validateConfig(config).request_rewrite_enabled, true);
  config.default_request_timezone = 'Asia/Hong_Kong';
  assert.throws(() => ui.validateConfig(config), /不支持当前地区/);
  config.default_request_timezone = 'Asia/Singapore';
  config.accounts[0].request_timezone = 'America/Los_Angeles';
  assert.equal(ui.validateConfig(config).accounts[0].request_timezone, 'America/Los_Angeles');
  config.accounts[0].request_timezone = 'Asia/Taipei';
  assert.throws(() => ui.validateConfig(config), /不支持当前地区/);
});

test('quality probe modes are strict, strict fallback or off and normalize accepted answers', () => {
  assert.doesNotThrow(() => ui.validateConfig(configured()));
  const config = configured({ quality_probe_enabled: true, quality_probe_mode: 'strict_fallback' });
  assert.throws(() => ui.validateConfig(config), /质量探针/);
  config.quality_probe_prompt = 'probe';
  config.quality_probe_accept = ' 17, iPhone 17 ,17 ';
  assert.equal(ui.validateConfig(config).quality_probe_accept, '17,iphone 17');
  assert.equal(ui.validateConfig(config).quality_probe_mode, 'strict_fallback');
  assert.equal(ui.validateConfig(config).quality_probe_enabled, true);
  config.quality_probe_accept = '1,2,3,4,5,6,7,8,9';
  assert.throws(() => ui.validateConfig(config), /最多填写 8 个/);
  config.quality_probe_accept = '17';
  config.quality_probe_mode = 'invalid';
  assert.throws(() => ui.validateConfig(config), /模式格式/);
});

test('generator capture uses the first layer API and always blocks unknown or Hong Kong egress', () => {
  const config = configured({ enabled: true, cookie_capture_mode: 'generator' });
  config.accounts[0].enabled = true;
  assert.throws(() => ui.validateConfig(config), /填写代理生成器地址/);
  config.proxy_generator_url = 'https://generator.example/gen?zone=custom&sessType=sticky';
  config.proxy_generator_blocked_countries = ['us', 'hk', 'US'];
  assert.equal(ui.validateConfig(config), config);
  assert.deepEqual(config.proxy_generator_blocked_countries, ['HK', 'US']);
  config.proxy_generator_url = 'https://user:pass@generator.example/gen';
  assert.throws(() => ui.validateConfig(config), /不含认证信息/);
  config.proxy_generator_url = 'https://generator.example/gen';
  config.proxy_generator_blocked_countries = ['HKG'];
  assert.throws(() => ui.validateConfig(config), /ISO 两位代码/);
  config.proxy_generator_blocked_countries = ['HK'];
  config.prefer_previous_ip = 'yes';
  assert.throws(() => ui.validateConfig(config), /复用开关/);
});

test('cookie mode validates capture and business egress independently', () => {
  const config = configured({ enabled: true, ticket_mode: 'cookie', cookie_capture_mode: 'socks5' });
  config.accounts[0].enabled = true;
  assert.throws(() => ui.validateConfig(config), /请填写采集代理/);
  config.cookie_capture_proxy_url = 'socks5h://capture-user:capture-pass@capture.example:1080';
  config.cookie_business_proxy_url = 'http://business-user:business-pass@business.example:8080';
  config.cookie_ticket_ttl_seconds = 300;
  config.state_780_ttl_seconds = 300;
  config.standby_ticket_enabled = true;
  config.standby_lead_seconds = 90;
  assert.equal(ui.validateConfig(config), config);
  config.standby_lead_seconds = 300;
  assert.throws(() => ui.validateConfig(config), /备用票提前时间必须小于/);
  config.standby_lead_seconds = 90;
  config.cookie_capture_mode = 'generator';
  config.proxy_generator_url = '';
  assert.throws(() => ui.validateConfig(config), /请填写代理生成器地址/);
  config.proxy_generator_url = 'https://generator.example/gen?zone=custom';
  assert.equal(ui.validateConfig(config).ticket_mode, 'cookie');
});

test('account labels omit empty fields instead of showing placeholder text', () => {
  assert.equal(ui.accountOptionLabel(40, {}), '#40');
  assert.equal(ui.accountOptionLabel(40, { name: 'account' }), '#40 · account');
  assert.equal(ui.accountOptionLabel(40, { email: 'account@example.com' }), '#40 · account@example.com');
  assert.equal(ui.accountOptionLabel(40, { name: 'account', email: 'account@example.com' }), '#40 · account · account@example.com');
});

test('validates bounds, renewal horizon, duplicate accounts and model allowlist', () => {
  assert.throws(() => ui.validateConfig(configured({ max_attempts: 33 })), /1–32/);
  assert.throws(() => ui.validateConfig(configured({ mint_concurrency: 4 })), /1–3/);
  assert.throws(() => ui.validateConfig(configured({ ttl_minutes: 181 })), /1–180/);
  assert.throws(() => ui.validateConfig(configured({ proxy_generator_ttl_minutes: 181 })), /1–180/);
  assert.doesNotThrow(() => ui.validateConfig(configured({ ttl_minutes: 180, refresh_before_seconds: 120, proxy_generator_ttl_minutes: 180 })));
  assert.throws(() => ui.validateConfig(configured({ cooldown_seconds: 29 })), /30–3600/);
  assert.throws(() => ui.validateConfig(configured({ target_gateway: 'unified-abc' })), /目标网关/);
  assert.throws(() => ui.validateConfig(configured({ target_gateway: 'any,unified-88' })), /不能同时/);
  assert.equal(ui.validateConfig(configured({ target_gateway: '88, 180,15,unified_88' })).target_gateway,
    'unified-15,unified-88,unified-180');
  assert.equal(ui.validateConfig(configured({ gateway_policy: 'any', target_gateway: 'unified-88' })).target_gateway, '');
  assert.equal(ui.validateConfig(configured({ gateway_policy: 'deny', target_gateway: '88,180' })).gateway_policy, 'deny');
  assert.throws(() => ui.validateConfig(configured({ gateway_policy: 'blocked' })), /网关策略/);
  assert.throws(() => ui.validateConfig(configured({ gateway_policy: 'deny', target_gateway: '' })), /至少填写一个/);
  assert.throws(() => ui.validateConfig(configured({ ttl_minutes: 10, refresh_before_seconds: 600 })), /必须小于/);
  assert.doesNotThrow(() => ui.validateConfig(configured({ ttl_minutes: 10, refresh_before_seconds: 30 })));
  const config = configured(); config.accounts.push({ ...config.accounts[0] });
  assert.throws(() => ui.validateConfig(config), /重复/);
  config.accounts.pop(); config.accounts[0].models = ['gpt-6-astra', 'gpt-6-astra'];
  assert.throws(() => ui.validateConfig(config), /不能重复/);
  config.accounts[0].models = ['<img src=x onerror=alert(1)>'];
  assert.throws(() => ui.validateConfig(config), /模型须以/);
  config.accounts[0].models = ['gpt-6-astra']; config.accounts[0].plan = 'team';
  assert.doesNotThrow(() => ui.validateConfig(config));
  config.accounts[0].email = 'not-an-email';
  assert.throws(() => ui.validateConfig(config), /邮箱/);
  config.accounts[0].email = 'owner@example.com';
  config.accounts[0].name = 'bad\nname';
  assert.throws(() => ui.validateConfig(config), /控制字符/);
});

test('legacy renewal horizon still loads without exposing it in the new UI', () => {
  assert.equal(ui.normalizeConfig({ refresh_before_minutes: 1 }).refresh_before_seconds, 60);
  assert.equal(ui.normalizeConfig({ refresh_before_seconds: 30, refresh_before_minutes: 1 }).refresh_before_seconds, 30);
});

test('status tolerates pre-initialization, de-duplicates safe IDs, never labels unknown state as raw text', () => {
  assert.deepEqual(ui.parseStatus({ healthy: true }), { host_ready: false, account_ids: [], account_catalog: [], tickets: [],
    diagnostics_enabled: false, diagnostics_listening: false, diagnostics_retained: 0, diagnostics: [], message: '' });
  const status = ui.parseStatus({ status_json: JSON.stringify({ host_ready: true, account_ids: [8, 2, 8, null, -1, '9', '9007199254740992'],
    account_catalog: [{ account_id: 8, name: ' Eight ', email: 'eight@example.com', expires_at: '2026-10-01', quota: '10/20' }], tickets: [] }) });
  assert.deepEqual(status.account_ids, [2, 8, 9]);
  assert.equal(status.account_catalog[0].name, 'Eight');
  assert.deepEqual(ui.stateLabel('raw-sensitive-ticket-content'), ['未知状态', 'warning']);
  assert.deepEqual(ui.stateLabel('ready'), ['可用', 'success']);
  assert.deepEqual(ui.stateLabel('ready_provisional'), ['可用 · 未通过质量', 'warning']);
  assert.deepEqual(ui.stateLabel('renewing'), ['可用 · 续期中', 'success']);
  assert.deepEqual(ui.stateLabel('renewing_provisional'), ['可用 · 未通过质量 · 校验中', 'warning']);
  assert.equal(ui.errorLabel('unknown-raw-ticket'), '操作未完成，请检查账号与插件设置。');
  assert.equal(ui.errorLabel('upstream_rate_limited'), '上游限流（429）');
  assert.throws(() => ui.parseStatus({ status_json: 'broken{' }), /格式不正确/);
});

test('redacts credentials and common ticket fields from error display', () => {
  const redacted = ui.redactError('proxy socks5h://my-user:my-secret@proxy.example:1080 x-codex-turn-state=opaque-state authorization=Bearer-token');
  assert.equal(redacted.includes('my-secret'), false);
  assert.equal(redacted.includes('opaque-state'), false);
  assert.equal(redacted.includes('Bearer-token'), false);
  assert.equal(ui.remainingText(127), '2 分 7 秒');
  assert.equal(ui.remainingText(-1), '—');
});

test('diagnostic events expose state classes and egress evidence without credentials or raw state', () => {
  const parsed = ui.parseStatus({ status_json: JSON.stringify({
    host_ready: true, tickets: [], diagnostics_enabled: true,
    diagnostics: [{ seq: 9, time: '2026-09-21T12:00:00Z', account_id: 19, model: 'gpt-6-astra', stage: 'capture',
      upstream_proxy: 'socks5://user:secret@first.example:1081', target_proxy: 'socks5://user:secret@second.example:10000',
      capture_egress: '203.0.113.18', fixed_egress: '198.51.100.24', egress_match: false, state_length: 292,
      state_class: '292', response_model: 'gpt-6-astra', quality: 'matched', quality_fingerprint: 'abc123',
      http_status: 200, outcome: 'accepted', duration_ms: 123,
      raw_state: 'gAAAAA-secret-state', token: 'secret-token' }]
  }) });
  assert.equal(parsed.diagnostics.length, 1);
  assert.equal(parsed.diagnostics[0].state_class, '292');
  assert.equal(parsed.diagnostics[0].capture_egress, '203.0.113.18');
  assert.equal(parsed.diagnostics[0].egress_match, false);
  assert.equal(parsed.diagnostics[0].quality, 'matched');
  assert.equal(parsed.diagnostics_retained, 1);
  assert.equal(JSON.stringify(parsed).includes('secret'), false);
  assert.equal(JSON.stringify(parsed).includes('gAAAAA-secret-state'), false);
  assert.equal(ui.diagnosticStageLabel('capture'), '采集票据');
  assert.equal(ui.diagnosticStageLabel('quality'), '质量探针');
  assert.equal(ui.diagnosticOutcomeLabel('state_312'), '收到 312');
  assert.equal(ui.diagnosticQualityLabel('matched'), '质量通过');
  assert.equal(ui.diagnosticQualityLabel('failed'), '质量未通过');
  assert.deepEqual(ui.diagnosticGatewayStats([
    { gateway: 'unified-15', stage: 'capture', outcome: 'accepted' },
    { gateway: 'unified-15', stage: 'fixed_validation', outcome: 'model_match' },
    { gateway: 'unified-15', stage: 'quality', outcome: 'accepted', quality: 'matched' },
    { gateway: 'unified-180', stage: 'quality', outcome: 'quality_mismatch' },
    { gateway: 'unified-12', stage: 'fixed_validation', outcome: 'gateway_mismatch' },
    { gateway: 'not-a-gateway', outcome: 'accepted' }
  ]), [
    { gateway: 'unified-15', total: 3, captureSuccess: 1, validationPassed: 1, qualityPassed: 1, qualityFailed: 0, otherFailed: 0 },
    { gateway: 'unified-12', total: 1, captureSuccess: 0, validationPassed: 0, qualityPassed: 0, qualityFailed: 0, otherFailed: 1 },
    { gateway: 'unified-180', total: 1, captureSuccess: 0, validationPassed: 0, qualityPassed: 0, qualityFailed: 1, otherFailed: 0 }
  ]);
});

test('quality probe fields follow the three-state mode selector', async () => {
  const h = uiHarness(); await settle();
  assert.equal(h.get('quality-probe-fields').hidden, true);
  assert.equal(h.get('quality-probe-prompt').disabled, true);
  h.get('quality-probe-mode').value = 'strict_fallback';
  await h.get('quality-probe-mode').fire('change');
  assert.equal(h.get('quality-probe-fields').hidden, false);
  assert.equal(h.get('quality-probe-prompt').disabled, false);
  assert.equal(h.get('quality-probe-accept').disabled, false);
  h.get('quality-probe-mode').value = 'off';
  await h.get('quality-probe-mode').fire('change');
  assert.equal(h.get('quality-probe-fields').hidden, true);
  h.runtime.stop();
});

class Node {
  constructor(tag) { this.tagName = tag; this.children = []; this.listeners = {}; this.attributes = {}; this._value = ''; this.textContent = ''; this.hidden = false; this.disabled = false; this.checked = false; this.open = false; }
  get value() { return this._value; }
  set value(value) { this._value = String(value); }
  appendChild(child) { this.children.push(child); return child; }
  replaceChildren(...children) { this.children = children; }
  addEventListener(type, fn) { this.listeners[type] = fn; }
  setAttribute(key, value) { this.attributes[key] = value; }
  getAttribute(key) { return this.attributes[key]; }
  async fire(type, event = {}) { if (this.listeners[type]) return this.listeners[type]({ preventDefault() {}, ...event }); }
  click() { return this.fire('click'); }
  showModal() { this.open = true; }
  close() { this.open = false; if (this.listeners.close) return this.listeners.close(); }
}

function uiHarness(options = {}) {
  const elements = new Map();
  const calls = { load: 0, save: [], test: 0, status: 0, dispose: 0 };
  const timers = new Map();
  const document = { getElementById: id => { if (!elements.has(id)) elements.set(id, new Node('div')); return elements.get(id); },
    createElement: tag => new Node(tag), documentElement: { scrollHeight: 900 }, body: new Node('body'), visibilityState: 'visible' };
  const config = configured();
  let status = { host_ready: true, account_ids: [7, 12], tickets: [{ account_id: 7, plan: 'pro', model: 'gpt-6-astra', state: 'ready', remaining_seconds: 600, attempts: 1 }] };
  const bridge = { ready() {}, resize() {}, dispose() { calls.dispose++; },
    async load() { calls.load++; return { config }; },
    async save(value) { calls.save.push(value); return { config: value }; },
    async test() { calls.test++; return { result: { message: '检查通过' } }; },
    async status() {
      calls.status++;
      const current = options.status ? await options.status(calls.status, status) : status;
      return { result: { status_json: JSON.stringify(current) } };
    } };
  const global = { document, Sub2APIPluginBridge: bridge, setInterval: fn => { timers.set(1, fn); return 1; }, clearInterval: id => timers.delete(id), addEventListener() {}, removeEventListener() {} };
  const runtime = ui.start(global);
  return { elements, get: document.getElementById, calls, timers, runtime, setStatus: value => { status = value; } };
}
const settle = () => new Promise(resolve => setImmediate(resolve));

test('passive status refresh preserves unsaved form and never invokes test or save', async () => {
  const h = uiHarness(); await settle();
  h.get('proxy-generator-url').value = 'https://unsaved.example/gen';
  await h.get('config-form').fire('input');
  h.setStatus({ host_ready: true, account_ids: [7, 12, 13], tickets: [] });
  await h.runtime.refreshStatus();
  assert.equal(h.get('proxy-generator-url').value, 'https://unsaved.example/gen');
  assert.equal(h.get('save-state').textContent, '有未保存修改');
  assert.equal(h.calls.load, 1); assert.equal(h.calls.save.length, 0); assert.equal(h.calls.test, 0);
  assert.equal(h.get('new-account-id').children.length, 4);
  h.runtime.stop(); assert.equal(h.timers.size, 0);
});

test('adding account defaults off, saved-config check does not save or overwrite edits', async () => {
  const h = uiHarness(); await settle();
  h.get('new-account-id').value = '12'; await h.get('add-account').click();
  const row = h.get('accounts-body').children[1];
  assert.equal(row.children[2].children[0].checked, false);
  assert.equal(row.children[3].children[0].value, 'pro');
  assert.equal(row.children[4].children[0].getAttribute('list'), 'model-options');
  await h.get('test-config').click();
  assert.equal(h.calls.test, 1); assert.equal(h.calls.save.length, 0);
  assert.equal(h.get('accounts-body').children.length, 2);
  assert.match(h.get('notice').textContent, /未保存修改/);
  h.runtime.stop();
});

test('manual first-layer proxy address saves without unsupported host bridge calls', async () => {
  const h = uiHarness(); await settle();
  const firstLayer = 'socks5://proxy-user:test-only@203.0.113.17:1081';
  h.get('upstream-proxy-url').value = firstLayer;
  await h.get('config-form').fire('change');
  await h.get('save-config').click();
  assert.equal(h.calls.save.length, 1);
  assert.equal(h.calls.save[0].upstream_proxy_id, 0);
  assert.equal(h.calls.save[0].upstream_proxy_url, firstLayer);
  h.runtime.stop();
});

test('explicit save button works without native form submission in sandbox', async () => {
  const h = uiHarness(); await settle();
  h.get('new-account-id').value = '12'; await h.get('add-account').click();
  await h.get('save-config').click();
  assert.equal(h.calls.save.length, 1);
  assert.equal(h.calls.save[0].enabled, false);
  assert.equal(h.calls.save[0].accounts[1].enabled, false);
  assert.equal(h.calls.save[0].accounts[1].name, '');
  assert.equal(h.get('save-state').textContent, '已保存');
  assert.match(h.get('notice').textContent, /STATE Kit 已关闭/);
  h.runtime.stop();
});

test('status rendering uses text nodes and never displays unrecognized raw state or model', async () => {
  const h = uiHarness(); await settle();
  h.setStatus({ host_ready: true, account_ids: [7, 8], account_catalog: [{ account_id: 7, name: '示例账号', email: 'owner@example.com' }], tickets: [
    { account_id: 7, plan: 'pro', model: 'gpt-6-astra', state: 'ready_provisional', quality_status: 'provisional', remaining_seconds: 9 },
    { account_id: 8, plan: 'pro', model: '<img src=x onerror=alert(1)>', state: 'SECRET-STATE-VALUE', last_error: 'x-codex-turn-state=SECRET-STATE-VALUE', remaining_seconds: 9 }
  ] });
  await h.runtime.refreshStatus();
  function text(node) { return String(node.textContent) + node.children.map(text).join(''); }
  const rendered = text(h.get('tickets-body'));
  assert.equal(rendered.includes('SECRET-STATE-VALUE'), false);
  assert.equal(rendered.includes('<img'), false);
  assert.match(rendered, /未知状态/);
  assert.match(rendered, /可用 · 未通过质量/);
  assert.match(rendered, /临时票 · 未通过质量/);
  assert.match(rendered, /账号：7 · 示例账号/);
  assert.match(rendered, /模型：未知模型/);
  h.runtime.stop();
});

test('detected account dropdown shows ID, name and email while model accepts presets or custom values', async () => {
  const h = uiHarness(); await settle();
  const choices = h.get('new-account-id').children;
  assert.match(String(choices[1].textContent), /#7 · 示例账号 · owner@example\.com/);
  assert.match(String(choices[2].textContent), /^#12$/);
  h.get('new-account-id').value = '7';
  await h.get('add-account').click();
  assert.match(h.get('notice').textContent, /已在列表中/);
  const model = h.get('accounts-body').children[0].children[4].children[0];
  model.value = 'gpt-5.6-sol, gpt-custom-model';
  await model.fire('input');
  await h.get('save-config').click();
  assert.deepEqual(h.calls.save[0].accounts[0].models, ['gpt-5.6-sol', 'gpt-custom-model']);
  h.runtime.stop();
});

test('new mode hides legacy per-account egress controls and saves neutral host-compatible fields', async () => {
  const h = uiHarness(); await settle();
  assert.equal(h.get('accounts-body').children[0].children.length, 6);
  await h.get('save-config').click();
  assert.equal(h.calls.save[0].accounts[0].egress_mode, 'sub2');
  assert.equal(h.calls.save[0].accounts[0].sticky_proxy_url, '');
  h.runtime.stop();
});

test('generator URL, blocked countries, concurrency and TTL are saved', async () => {
  const h = uiHarness(); await settle();
  h.get('proxy-generator-url').value = 'https://generator.example/gen?zone=custom&sessType=sticky';
  h.get('proxy-generator-blocked-countries').value = 'us, hk, vn';
  h.get('prefer-previous-ip').checked = true;
  h.get('proxy-generator-ttl-minutes').value = '6';
  h.get('cookie-ticket-ttl-seconds').value = '300';
  h.get('mint-concurrency').value = '2';
  await h.get('config-form').fire('input');
  await h.get('save-config').click();
  assert.equal(h.calls.save[0].proxy_generator_url, 'https://generator.example/gen?zone=custom&sessType=sticky');
  assert.deepEqual(h.calls.save[0].proxy_generator_blocked_countries, ['HK', 'US', 'VN']);
  assert.equal(h.calls.save[0].prefer_previous_ip, true);
  assert.equal(h.calls.save[0].proxy_generator_ttl_minutes, 6);
  assert.equal(h.calls.save[0].cookie_ticket_ttl_seconds, 300);
  assert.equal(h.calls.save[0].state_780_ttl_seconds, 300);
  assert.equal(h.calls.save[0].mint_concurrency, 2);
  h.runtime.stop();
});

test('cookie and 780 are fixed while advanced gateway and retry policy remain configurable', async () => {
  const h = uiHarness(); await settle();
  h.get('route-cookie-reuse').checked = true;
  h.get('mint-fingerprint-convergence').checked = true;
  h.get('cookie-ticket-ttl-seconds').value = '240';
  h.get('gateway-policy').value = 'any';
  await h.get('gateway-policy').fire('change');
  assert.equal(h.get('target-gateway').disabled, true);
  h.get('gateway-policy').value = 'deny';
  await h.get('gateway-policy').fire('change');
  assert.equal(h.get('target-gateway').disabled, false);
  h.get('target-gateway').value = '88, 180,15';
  await h.get('config-form').fire('change');
  await h.get('save-config').click();
  assert.equal(h.calls.save.length, 1);
  assert.equal(h.calls.save[0].ticket_mode, 'cookie');
  assert.equal(h.calls.save[0].allow_state_780, true);
  assert.equal(h.calls.save[0].state_780_ttl_seconds, 240);
  assert.equal(h.calls.save[0].cookie_ticket_ttl_seconds, 240);
  assert.equal(h.calls.save[0].gateway_policy, 'deny');
  assert.equal(h.calls.save[0].target_gateway, 'unified-15,unified-88,unified-180');
  assert.equal(h.calls.save[0].route_cookie_reuse, true);
  assert.equal(h.calls.save[0].mint_fingerprint_convergence, true);
  h.runtime.stop();
});

test('request rewrite switch, default timezone and account timezone are persisted', async () => {
  const h = uiHarness(); await settle();
  h.get('request-rewrite-enabled').checked = true;
  h.get('default-request-timezone').value = 'Asia/Singapore';
  const timezone = h.get('accounts-body').children[0].children[1].children[0].children[4].children[1];
  timezone.value = 'Asia/Tokyo';
  await timezone.fire('input');
  await h.get('save-config').click();
  assert.equal(h.calls.save[0].request_rewrite_enabled, true);
  assert.equal(h.calls.save[0].default_request_timezone, 'Asia/Singapore');
  assert.equal(h.calls.save[0].accounts[0].request_timezone, 'Asia/Tokyo');
  h.runtime.stop();
});

test('cookie split mode saves capture, business, TTL and standby controls', async () => {
  const h = uiHarness(); await settle();
  assert.equal(h.get('cookie-mode-fields').hidden, false);
  h.get('cookie-capture-mode').value = 'socks5';
  await h.get('cookie-capture-mode').fire('change');
  h.get('cookie-capture-proxy-url').value = 'socks5h://capture-user:capture-pass@capture.example:1080';
  h.get('cookie-business-proxy-url').value = 'http://business-user:business-pass@business.example:8080';
  h.get('cookie-ticket-ttl-seconds').value = '300';
  h.get('standby-ticket-enabled').checked = true;
  await h.get('standby-ticket-enabled').fire('change');
  h.get('standby-lead-seconds').value = '90';
  await h.get('save-config').click();
  assert.equal(h.calls.save.length, 1);
  assert.equal(h.calls.save[0].ticket_mode, 'cookie');
  assert.equal(h.calls.save[0].cookie_capture_mode, 'socks5');
  assert.equal(h.calls.save[0].cookie_capture_proxy_url, 'socks5h://capture-user:capture-pass@capture.example:1080');
  assert.equal(h.calls.save[0].cookie_business_proxy_url, 'http://business-user:business-pass@business.example:8080');
  assert.equal(h.calls.save[0].cookie_ticket_ttl_seconds, 300);
  assert.equal(h.calls.save[0].state_780_ttl_seconds, 300);
  assert.equal(h.calls.save[0].standby_ticket_enabled, true);
  assert.equal(h.calls.save[0].standby_lead_seconds, 90);
  h.runtime.stop();
});

test('diagnostic panel listens while open, places newest first and retains history on close', async () => {
  const h = uiHarness(); await settle();
  h.setStatus({ host_ready: true, tickets: [], diagnostics_enabled: true, diagnostics_listening: true, diagnostics: [{
    seq: 1, time: '2026-09-21T12:00:00Z', account_id: 19, model: 'gpt-6-astra', stage: 'fixed_validation',
    upstream_proxy: 'socks5://user:secret@first.example:1081', target_proxy: 'socks5://user:secret@business.example:1081',
    capture_egress: '203.0.113.18', fixed_egress: '198.51.100.24', egress_match: false, state_length: 312,
    state_class: '312', response_model: 'gpt-5.6-luna', http_status: 200, outcome: 'state_312', duration_ms: 88
  }, {
    seq: 2, time: '2026-09-21T12:00:01Z', account_id: 20, model: 'gpt-6-astra', stage: 'capture',
    target_proxy: 'socks5://user:secret@capture.example:1081', capture_egress: '203.0.113.19',
    state_length: 780, state_class: '780', gateway: 'unified-15', response_model: 'gpt-6-astra', http_status: 200,
    outcome: 'accepted', duration_ms: 55
  }] });
  await h.get('open-diagnostics').click();
  assert.equal(h.calls.status >= 3, true);
  assert.equal(h.calls.save.length, 0);
  h.timers.get(1)();
  await settle();
  function text(node) { return String(node.textContent) + node.children.map(text).join(''); }
  const rendered = text(h.get('diagnostics-body'));
  assert.equal(h.get('diagnostics-dialog').open, true);
  assert.match(text(h.get('diagnostics-body').children[0]), /#20/);
  assert.match(rendered, /采集：203\.0\.113\.18/);
  assert.match(rendered, /固定：198\.51\.100\.24/);
  assert.match(rendered, /出口不一致/);
  assert.match(rendered, /票据状态[\s\S]*312/);
  assert.match(rendered, /gpt-5\.6-luna/);
  assert.match(text(h.get('diagnostics-gateways')), /unified-15[\s\S]*采集成功 1/);
  assert.equal(text(h.get('diagnostics-gateways')).includes('通过率'), false);
  assert.equal(rendered.includes('secret'), false);
  h.get('diagnostics-dialog').close();
  assert.equal(h.get('diagnostics-body').children.length, 2);
  assert.match(h.get('diagnostics-summary').textContent, /最近 5 小时/);
  assert.equal(h.timers.size, 0);
  h.runtime.stop();
});

test('diagnostic panel opens the listener with parallel status signals and displays 780', async () => {
  let slowFirst = true;
  const h = uiHarness({
    status: async function () {
      if (slowFirst) {
        slowFirst = false;
        await new Promise(resolve => setTimeout(resolve, 30));
      }
      return { host_ready: true, tickets: [], diagnostics_enabled: true, diagnostics_listening: true, diagnostics_retained: 2200, diagnostics: [{
        seq: 3, time: '2026-09-24T08:00:00Z', account_id: 42, model: 'gpt-6-astra',
        stage: 'fixed_validation', state_length: 780, state_class: '780',
        response_model: 'gpt-6-astra', http_status: 200, outcome: 'accepted'
      }] };
    }
  });
  await settle();
  const initialStatusCalls = h.calls.status;
  const opening = h.get('open-diagnostics').click();
  await new Promise(resolve => setTimeout(resolve, 5));
  assert.equal(h.calls.status, initialStatusCalls + 3);
  await opening;
  function text(node) { return String(node.textContent) + node.children.map(text).join(''); }
  assert.match(text(h.get('diagnostics-body').children[0]), /780/);
  assert.match(h.get('diagnostics-summary').textContent, /内存共 2200 条/);
  h.runtime.stop();
});

test('diagnostic window uses a full-screen single-scroll event layout', () => {
  const html = fs.readFileSync(path.join(__dirname, '..', 'ui', 'index.html'), 'utf8');
  const css = fs.readFileSync(path.join(__dirname, '..', 'ui', 'assets', 'diagnostics.css'), 'utf8');
  assert.match(html, /id="gateway-policy"/);
  assert.match(html, /id="diagnostics-scroll"/);
  assert.match(html, /最近 5 小时/);
  assert.match(html, /各阶段计数不能相加/);
  assert.match(html, /返回新的 <code>780<\/code>/);
  assert.doesNotMatch(html, /diagnostics-drag-handle/);
  assert.match(css, /\.diagnostics-dialog[\s\S]*width:\s*100vw/);
  assert.match(css, /\.diagnostics-dialog\[open\][\s\S]*display:\s*flex/);
  assert.match(css, /\.diagnostics-scroll[\s\S]*overflow-y:\s*auto/);
  assert.match(css, /\.diagnostics-events[\s\S]*display:\s*grid/);
  assert.doesNotMatch(css, /\.diagnostics-table/);
});
