(function (global, factory) {
  'use strict';
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else api.start(global);
})(typeof window === 'object' ? window : null, function () {
  'use strict';
  const DEFAULT_CONFIG = Object.freeze({ enabled: false, upstream_proxy_id: 0, upstream_proxy_url: '', dynamic_proxy_url: '',
    proxy_generator_url: '', proxy_generator_blocked_countries: ['HK'], proxy_generator_ttl_minutes: 180, prefer_previous_ip: false,
    allow_state_780: false,
    ticket_mode: 'legacy', cookie_capture_mode: 'generator', cookie_capture_proxy_url: '', cookie_business_proxy_url: '',
    cookie_ticket_ttl_seconds: 300, standby_ticket_enabled: false, standby_lead_seconds: 90,
    request_rewrite_enabled: false, default_request_timezone: 'Asia/Singapore',
    ttl_minutes: 180, refresh_before_seconds: 120, max_attempts: 8, attempt_interval_seconds: 10, cooldown_seconds: 300 });
  const NUMBERS = Object.freeze({ ttl_minutes: [1, 180, '旧模式票据有效期'], refresh_before_seconds: [0, 3599, '旧模式提前续期'],
    proxy_generator_ttl_minutes: [1, 180, '生成器出口有效期'], max_attempts: [1, 32, '每轮最多尝试'],
    cookie_ticket_ttl_seconds: [30, 1800, 'Cookie 票据持续期'], standby_lead_seconds: [10, 600, '备用票提前时间'],
    attempt_interval_seconds: [1, 300, '常规重试间隔'], cooldown_seconds: [30, 3600, '失败后冷却'] });
  const STATES = Object.freeze({ disabled: ['已关闭', ''], waiting_host: ['等待宿主', 'warning'],
    waiting_account: ['等待账号', 'warning'], queued: ['等待获取', ''], harvesting: ['正在获取', ''],
    ready: ['可用', 'success'], renewing: ['可用 · 续期中', 'success'], cooldown: ['冷却中', 'warning'],
    expired: ['已过期', 'warning'], error: ['获取失败', 'error'] });
  const MODEL_PATTERN = /^gpt-[A-Za-z0-9][A-Za-z0-9._-]{0,94}$/;
  const ACCOUNT_TEXT_LIMITS = Object.freeze({ name: 120, email: 254, expires_at: 64, quota: 80, request_timezone: 64 });
  const RESTRICTED_TIMEZONES = new Set(['PRC', 'ROC', 'Hongkong', 'Asia/Chongqing', 'Asia/Chungking',
    'Asia/Harbin', 'Asia/Kashgar', 'Asia/Macao', 'Asia/Taipei', 'Asia/Shanghai', 'Asia/Urumqi',
    'Asia/Hong_Kong', 'Asia/Macau']);
  const ERRORS = Object.freeze({ attempts_exhausted: '本轮尝试已用完', identity_unavailable: '暂时无法取得账号授权或业务代理',
    invalid_dynamic_proxy: '动态代理配置无效', harvest_failed: '动态代理获取票据未成功', unexpected_state_length: '票据长度与所选套餐不符',
    identity_changed: '账号授权信息发生变化', account_egress_invalid: '账号出口代理配置无效',
    fixed_proxy_validation_failed: '票据未通过账号业务代理验证', sticky_egress_changed: '账号粘性代理出口发生变化',
    generator_unavailable: '代理生成器暂时不可用', generator_egress_unavailable: '生成器出口暂时不可用',
    generator_region_blocked: '生成器出口位于阻止地区，正在重试',
    business_egress_unavailable: 'Cookie 业务出口暂时不可用', business_region_blocked: 'Cookie 业务出口位于阻止地区',
    cookie_session_incomplete: 'Cookie 会话不完整，正在重新获取',
    ticket_persistence_failed: '票据保存失败', upstream_unauthorized: '上游拒绝授权（401）', upstream_forbidden: '上游拒绝访问（403）',
    upstream_rate_limited: '上游限流（429）', upstream_rejected: '上游拒绝请求', model_mismatch: '返回模型不匹配，正在重新获取票据',
    state_312: '收到 312 状态，正在重新获取票据', model_mismatch_persistence_failed: '返回模型不匹配，票据失效记录保存失败',
    state_312_persistence_failed: '收到 312 状态，票据失效记录保存失败' });
  const MESSAGES = Object.freeze({ 'STATE disabled; requests use the account business proxy': 'STATE 已关闭，请求使用账号原有业务代理。',
    'STATE active only for explicitly enabled account/model pairs': 'STATE 仅对手动开启的账号与模型生效。',
    'waiting for host services': '正在等待宿主服务初始化。' });
  function accountID(value) {
    if (typeof value !== 'number' && typeof value !== 'string') return null;
    if (!/^[1-9]\d*$/.test(String(value).trim())) return null;
    const number = Number(value);
    return Number.isSafeInteger(number) && number > 0 ? number : null;
  }
  function accountText(value, key) {
    if (typeof value !== 'string') return '';
    return value.trim();
  }
  function safeAccountText(value, key) {
    return accountText(value, key).slice(0, ACCOUNT_TEXT_LIMITS[key]);
  }
  function validateAccountText(value, label, key) {
    if (typeof value !== 'string') throw new Error(label + '格式不正确。');
    const normalized = value.trim();
    if (Array.from(normalized).length > ACCOUNT_TEXT_LIMITS[key] || /[\u0000-\u001f\u007f]/.test(normalized)) {
      throw new Error(label + '过长或包含不支持的控制字符。');
    }
    if (key === 'email' && normalized && !normalized.includes('@')) throw new Error('账号邮箱须包含 @。');
    return normalized;
  }
  function accountName(account) {
    return accountText(account && account.name, 'name');
  }
  function accountEmail(account) {
    return accountText(account && account.email, 'email');
  }
  function accountOptionLabel(id, account) {
    const parts = ['#' + id];
    const name = accountName(account);
    const email = accountEmail(account);
    if (name) parts.push(name);
    if (email) parts.push(email);
    return parts.join(' · ');
  }
  function normalizeAccountCatalog(response) {
    const source = response && Array.isArray(response.accounts) ? response.accounts : [];
    return source.map(function (account) {
      const id = accountID(account && account.account_id);
      if (id === null) return null;
      return { account_id: id, name: safeAccountText(account.name, 'name'), email: safeAccountText(account.email, 'email'),
        expires_at: safeAccountText(account.expires_at, 'expires_at'), quota: safeAccountText(account.quota, 'quota') };
    }).filter(Boolean).slice(0, 1000);
  }
  function validateProxyAddress(value, label) {
    if (typeof value !== 'string') throw new Error(label + '格式不正确。');
    if (value === '') return;
    try {
      if (value.length > 4096 || /[\r\n\t]/.test(value)) throw new Error();
      const expanded = value.replace(/\{(?:random|sid)\}/g, '123456');
      if (/[{}]/.test(expanded)) throw new Error();
      const url = new URL(expanded);
      if (!['http:', 'https:', 'socks5:', 'socks5h:'].includes(url.protocol) || !url.hostname || url.search || url.hash || (url.pathname && url.pathname !== '/')) throw new Error();
    } catch (_) {
      throw new Error(label + '须为完整的 HTTP(S) 或 SOCKS5(H) 地址。');
    }
  }
  function validateGeneratorURL(value) {
    if (typeof value !== 'string') throw new Error('代理生成器地址格式不正确。');
    if (value === '') return;
    try {
      if (value.length > 8192 || /[\r\n\t]/.test(value)) throw new Error();
      const url = new URL(value);
      if (!['http:', 'https:'].includes(url.protocol) || !url.hostname || url.hash || url.username || url.password) throw new Error();
    } catch (_) {
      throw new Error('代理生成器须为不含认证信息或锚点的 HTTP(S) 地址。');
    }
  }
  function validateTimezone(value, label) {
    if (typeof value !== 'string') throw new Error(label + '格式不正确。');
    const timezone = value.trim();
    if (!timezone || Array.from(timezone).length > ACCOUNT_TEXT_LIMITS.request_timezone || /[\u0000-\u001f\u007f]/.test(timezone)) {
      throw new Error(label + '过长或包含不支持的控制字符。');
    }
    try {
      new Intl.DateTimeFormat('en-US', { timeZone: timezone }).format(new Date());
    } catch (_) {
      throw new Error(label + '不是有效的 IANA 时区。');
    }
    if (RESTRICTED_TIMEZONES.has(timezone)) throw new Error(label + '不支持当前地区。');
    return timezone;
  }
  function normalizeBlockedCountries(value) {
    if (!Array.isArray(value) || value.length > 32) throw new Error('阻止国家或地区最多填写 32 个 ISO 两位代码。');
    const seen = new Set();
    const countries = [];
    value.forEach(function (entry) {
      if (typeof entry !== 'string') throw new Error('阻止国家或地区须使用 ISO 两位代码。');
      const code = entry.trim().toUpperCase();
      if (!code) return;
      if (!/^[A-Z]{2}$/.test(code)) throw new Error('阻止国家或地区须使用 ISO 两位代码。');
      if (!seen.has(code)) { seen.add(code); countries.push(code); }
    });
    if (!seen.has('HK')) countries.push('HK');
    return countries.sort();
  }
  function normalizeConfig(input) {
    const source = input && typeof input === 'object' && !Array.isArray(input) ? input : {};
    const config = Object.assign({}, DEFAULT_CONFIG, { proxy_generator_blocked_countries: DEFAULT_CONFIG.proxy_generator_blocked_countries.slice() });
    if (source.refresh_before_seconds === undefined && Number.isInteger(source.refresh_before_minutes)) {
      config.refresh_before_seconds = source.refresh_before_minutes * 60;
    }
    Object.keys(DEFAULT_CONFIG).forEach(function (key) {
      if (source[key] !== undefined) {
        config[key] = key === 'proxy_generator_blocked_countries' && Array.isArray(source[key]) ? source[key].slice() : source[key];
      }
    });
    config.accounts = Array.isArray(source.accounts) ? source.accounts.map(function (account) {
      return { account_id: account.account_id, name: accountText(account.name, 'name'),
        email: accountText(account.email, 'email'), expires_at: accountText(account.expires_at, 'expires_at'),
        quota: accountText(account.quota, 'quota'), enabled: account.enabled === true,
        egress_mode: account.egress_mode || 'sub2', sticky_proxy_url: accountText(account.sticky_proxy_url, 'sticky_proxy_url'),
        plan: account.plan || 'pro', request_timezone: accountText(account.request_timezone, 'request_timezone') || config.default_request_timezone,
        models: Array.isArray(account.models) ? account.models.slice() : ['gpt-6-astra'] };
    }) : [];
    return config;
  }
  function validateConfig(config) {
    if (typeof config.enabled !== 'boolean') throw new Error('总开关格式不正确。');
    validateProxyAddress(config.upstream_proxy_url, '第一层代理');
    validateProxyAddress(config.dynamic_proxy_url, '动态代理');
    validateProxyAddress(config.cookie_capture_proxy_url, 'Cookie 模式采集代理');
    validateProxyAddress(config.cookie_business_proxy_url, 'Cookie 模式业务固定代理');
    config.proxy_generator_url = typeof config.proxy_generator_url === 'string' ? config.proxy_generator_url.trim() : '';
    validateGeneratorURL(config.proxy_generator_url);
    config.proxy_generator_blocked_countries = normalizeBlockedCountries(config.proxy_generator_blocked_countries);
    if (typeof config.prefer_previous_ip !== 'boolean') throw new Error('上一轮可用出口复用开关格式不正确。');
    if (typeof config.allow_state_780 !== 'boolean') throw new Error('780 状态兼容开关格式不正确。');
    if (typeof config.request_rewrite_enabled !== 'boolean') throw new Error('请求环境替换开关格式不正确。');
    config.default_request_timezone = validateTimezone(config.default_request_timezone, '默认请求时区');
    if (!['legacy', 'cookie'].includes(config.ticket_mode)) throw new Error('运行方式须选择稳定同出口或 Cookie 分流。');
    if (!['generator', 'socks5'].includes(config.cookie_capture_mode)) throw new Error('Cookie 模式打票方式须选择代理生成器或 S5/HTTP 固定代理。');
    if (typeof config.standby_ticket_enabled !== 'boolean') throw new Error('备用票队列开关格式不正确。');
    Object.keys(NUMBERS).forEach(function (key) {
      const bounds = NUMBERS[key];
      if (!Number.isInteger(config[key]) || config[key] < bounds[0] || config[key] > bounds[1]) {
        throw new Error(bounds[2] + '须为 ' + bounds[0] + '–' + bounds[1] + ' 之间的整数。');
      }
    });
    if (config.refresh_before_seconds >= config.ttl_minutes * 60) throw new Error('提前续期必须小于票据有效期。');
    if (config.ticket_mode === 'cookie' && config.standby_lead_seconds >= config.cookie_ticket_ttl_seconds) {
      throw new Error('备用票提前时间必须小于 Cookie 票据持续期。');
    }
    if (config.ticket_mode === 'legacy' && config.standby_lead_seconds >= config.ttl_minutes * 60) {
      throw new Error('备用票提前时间必须小于旧模式票据有效期。');
    }
    if (!Array.isArray(config.accounts) || config.accounts.length > 256) throw new Error('最多配置 256 个账号。');
    const ids = new Set();
    let totalModels = 0;
    config.accounts.forEach(function (account) {
      if (accountID(account.account_id) === null) throw new Error('账号 ID 须为正整数。');
      if (ids.has(account.account_id)) throw new Error('账号 ID ' + account.account_id + ' 重复。');
      ids.add(account.account_id);
      account.name = validateAccountText(account.name, '账号名称', 'name');
      account.email = validateAccountText(account.email, '账号邮箱', 'email');
      account.expires_at = validateAccountText(account.expires_at, '账号到期时间', 'expires_at');
      account.quota = validateAccountText(account.quota, '账号额度', 'quota');
      if (typeof account.enabled !== 'boolean') throw new Error('账号开关格式不正确。');
      if (!['sub2', 'plugin', 'generator'].includes(account.egress_mode)) throw new Error('请选择 Sub2 原有代理、账号粘性代理或代理生成器出口。');
      validateProxyAddress(account.sticky_proxy_url, '账号粘性代理');
      if (account.egress_mode === 'plugin') {
        if (!account.sticky_proxy_url) throw new Error('插件固定出口模式必须填写账号粘性代理。');
        if (/\{(?:random|sid)\}/i.test(account.sticky_proxy_url)) throw new Error('账号粘性代理必须使用服务商固定 session，不能使用 {random} 或 {sid}。');
      }
      if (!['pro', 'team'].includes(account.plan)) throw new Error('请选择 Pro 或 Team 套餐。');
      account.request_timezone = validateTimezone(account.request_timezone || config.default_request_timezone, '账号请求时区');
      if (!Array.isArray(account.models) || !account.models.length || account.models.length > 16) throw new Error('每个账号须填写 1–16 个模型。');
      totalModels += account.models.length;
      const models = new Set();
      account.models.forEach(function (model) {
        if (typeof model !== 'string' || !MODEL_PATTERN.test(model)) throw new Error('模型须以 gpt- 开头，只能包含字母、数字、点、下划线和连字符，最长 99 个字符。');
        if (models.has(model)) throw new Error('同一账号的模型名称不能重复。');
        models.add(model);
      });
    });
    if (totalModels > 1024) throw new Error('最多配置 1024 个账号与模型组合。');
    if (config.enabled && config.ticket_mode === 'legacy' && config.accounts.some(function (account) { return account.enabled && account.egress_mode === 'sub2'; }) && !config.dynamic_proxy_url) {
      throw new Error('启用 Sub2 原有代理模式的账号前，请填写动态代理地址。');
    }
    if (config.enabled && config.ticket_mode === 'legacy' && config.accounts.some(function (account) { return account.enabled && account.egress_mode === 'generator'; }) && !config.proxy_generator_url) {
      throw new Error('启用代理生成器出口模式的账号前，请填写代理生成器地址。');
    }
    if (config.enabled && config.ticket_mode === 'cookie' && config.accounts.some(function (account) { return account.enabled; })) {
      if (config.cookie_capture_mode === 'socks5' && !config.cookie_capture_proxy_url) throw new Error('Cookie 模式选择 S5/HTTP 固定采集代理时，请填写采集代理。');
      if (config.cookie_capture_mode === 'generator' && !config.proxy_generator_url) throw new Error('Cookie 模式选择代理生成器打票时，请填写代理生成器地址。');
    }
    return config;
  }
  function stateLabel(state) { return Object.prototype.hasOwnProperty.call(STATES, state) ? STATES[state] : ['未知状态', 'warning']; }
  function errorLabel(code) { return Object.prototype.hasOwnProperty.call(ERRORS, code) ? ERRORS[code] : '操作未完成，请检查账号与插件设置。'; }
  function diagnosticStageLabel(stage) {
    return ({ capture: '采集票据', fixed_validation: '同出口复验', restore: '恢复复验',
      ticket_lookup: '票据查询', business: '业务请求', business_result: '业务结果' })[stage] || '未知阶段';
  }
  function diagnosticOutcomeLabel(outcome) {
    return ({ accepted: '已接受', probe_failed: '探测失败', unexpected_state: '状态长度不符',
      validation_failed: '固定代理复验失败', state_312: '收到 312', unavailable: '票据未准备好',
      invalid_proxy: '代理配置无效', headers_received: '已收到响应头', model_match: '模型一致',
      model_mismatch: '模型不一致', incomplete: '响应未完成', upstream_read_error: '响应中断',
      upstream_transport: '传输失败', upstream_transport_dns: 'DNS 失败', upstream_transport_connect: '连接失败',
      upstream_transport_tls: 'TLS 失败', upstream_transport_timeout: '传输超时', upstream_transport_reset: '连接重置',
      generator_failed: '生成器调用失败', egress_unavailable: '出口不可用', region_blocked: '地区被阻止',
      egress_changed: '出口 IP 不一致' })[outcome] || '未知结果';
  }
  function safeDiagnosticText(value, max) {
    if (typeof value !== 'string') return '';
    return redactError(value.trim()).slice(0, max);
  }
  function normalizeDiagnostic(event) {
    if (!event || typeof event !== 'object' || Array.isArray(event)) return null;
    const stateClasses = new Set(['292', '312', '332', '780', 'empty', 'other']);
    const responseModel = typeof event.response_model === 'string' && MODEL_PATTERN.test(event.response_model) ? event.response_model : '';
    return {
      seq: Number.isSafeInteger(event.seq) && event.seq >= 0 ? event.seq : 0,
      time: typeof event.time === 'string' && Number.isFinite(Date.parse(event.time)) ? event.time : '',
      account_id: event.account_id === undefined ? null : accountID(event.account_id),
      model: typeof event.model === 'string' && MODEL_PATTERN.test(event.model) ? event.model : '',
      stage: safeDiagnosticText(event.stage, 40),
      attempt: Number.isSafeInteger(event.attempt) && event.attempt > 0 ? event.attempt : 0,
      upstream_proxy: safeDiagnosticText(event.upstream_proxy, 160),
      target_proxy: safeDiagnosticText(event.target_proxy, 160),
      capture_egress: safeDiagnosticText(event.capture_egress, 64),
      fixed_egress: safeDiagnosticText(event.fixed_egress, 64),
      egress_match: typeof event.egress_match === 'boolean' ? event.egress_match : null,
      state_length: Number.isSafeInteger(event.state_length) && event.state_length >= 0 && event.state_length <= 8192 ? event.state_length : 0,
      state_class: stateClasses.has(event.state_class) ? event.state_class : '',
      response_model: responseModel,
      http_status: Number.isSafeInteger(event.http_status) && event.http_status >= 100 && event.http_status <= 599 ? event.http_status : 0,
      outcome: safeDiagnosticText(event.outcome, 64),
      error: safeDiagnosticText(event.error, 80),
      duration_ms: Number.isSafeInteger(event.duration_ms) && event.duration_ms >= 0 ? event.duration_ms : 0
    };
  }
  function redactError(value) {
    return String(value || '')
      .replace(/(?:https?|socks5h?):\/\/[^\s/]*@/gi, '[代理凭据已隐藏]@')
      .replace(/(?:x-codex-turn-state|authorization|access_token|refresh_token|api_key|password)\s*[:=]\s*[^\s,;]+/gi, '[敏感字段已隐藏]')
      .replace(/\beyJ[A-Za-z0-9_-]{15,}(?:\.[A-Za-z0-9_-]+){0,2}/g, '[票据已隐藏]')
      .slice(0, 400);
  }
  function parseStatus(result) {
    let status = result && result.status_json;
    if (typeof status === 'string') {
      try { status = JSON.parse(status); } catch (_) { throw new Error('宿主返回的状态格式不正确。'); }
    }
    if (!status || typeof status !== 'object' || Array.isArray(status)) status = {};
    const catalog = Array.isArray(status.account_catalog) ? status.account_catalog.map(function (account) {
      const id = accountID(account && account.account_id);
      if (id === null) return null;
      return { account_id: id, name: safeAccountText(account.name, 'name'), email: safeAccountText(account.email, 'email'),
        expires_at: safeAccountText(account.expires_at, 'expires_at'), quota: safeAccountText(account.quota, 'quota') };
    }).filter(Boolean).slice(0, 256) : [];
    return { host_ready: status.host_ready === true,
      account_ids: Array.isArray(status.account_ids) ? Array.from(new Set(status.account_ids.map(accountID).filter(function (id) { return id !== null; }))).sort(function (a, b) { return a - b; }) : [],
      account_catalog: catalog,
      tickets: Array.isArray(status.tickets) ? status.tickets.filter(function (ticket) { return ticket && accountID(ticket.account_id) !== null; }).slice(0, 4096) : [],
      diagnostics_enabled: status.diagnostics_enabled === true,
      diagnostics_listening: status.diagnostics_listening === true || status.diagnostics_enabled === true,
      diagnostics: Array.isArray(status.diagnostics) ? status.diagnostics.map(normalizeDiagnostic).filter(Boolean).slice(-240) : [],
      message: redactError(MESSAGES[status.message] || status.message || result && result.message || '') };
  }
  function remainingText(seconds) {
    const value = Number(seconds);
    if (!Number.isFinite(value) || value <= 0) return '—';
    const minutes = Math.floor(value / 60);
    return minutes ? minutes + ' 分 ' + Math.floor(value % 60) + ' 秒' : Math.floor(value) + ' 秒';
  }

  function start(global) {
    const document = global.document;
    const bridge = global.Sub2APIPluginBridge;
    const byID = function (id) { return document.getElementById(id); };
    let loaded = false;
    let busy = false;
    let dirty = false;
    let statusBusy = false;
    let diagnosticsBusy = false;
    let diagnosticsOpen = false;
    let diagnosticsListening = false;
    let diagnosticsTimer;
    let closed = false;
    let pollTimer;
    let resizeObserver;
    let accounts = [];
    let statusAccountCatalog = [];
    let lastDiagnostics = [];
    const numberIDs = { ttl_minutes: 'ttl-minutes', refresh_before_seconds: 'refresh-before-seconds',
      proxy_generator_ttl_minutes: 'proxy-generator-ttl-minutes',
      cookie_ticket_ttl_seconds: 'cookie-ticket-ttl-seconds', standby_lead_seconds: 'standby-lead-seconds',
      max_attempts: 'max-attempts', attempt_interval_seconds: 'attempt-interval-seconds', cooldown_seconds: 'cooldown-seconds' };
    function element(tag, text, className) {
      const node = document.createElement(tag);
      if (text !== undefined) node.textContent = String(text);
      if (className) node.className = className;
      return node;
    }
    function notice(message, kind) {
      const node = byID('notice');
      node.textContent = redactError(message);
      node.className = 'notice' + (kind ? ' ' + kind : '');
      node.hidden = !message;
    }
    function updateSaveState(text) {
      byID('save-state').textContent = text || (dirty ? '有未保存修改' : '配置已加载');
      byID('save-state').className = dirty ? 'dirty' : 'muted';
    }
    function markDirty() { if (loaded) { dirty = true; updateSaveState(); } }
    function renderTicketMode() {
      const cookieMode = byID('ticket-mode').value === 'cookie';
      const generatorCapture = !cookieMode || byID('cookie-capture-mode').value === 'generator';
      byID('legacy-mode-fields').hidden = cookieMode;
      byID('cookie-mode-fields').hidden = !cookieMode;
      byID('cookie-socks5-fields').hidden = !cookieMode || generatorCapture;
      byID('generator-fields').hidden = !generatorCapture;
      byID('prefer-previous-fields').hidden = !generatorCapture;
      byID('cookie-capture-proxy-url').disabled = !cookieMode || byID('cookie-capture-mode').value !== 'socks5';
      byID('cookie-business-proxy-url').disabled = !cookieMode;
      byID('standby-ticket-enabled').disabled = false;
      byID('standby-lead-seconds').disabled = !byID('standby-ticket-enabled').checked;
    }
    function setBusy(value) {
      busy = value;
      byID('config-fields').disabled = !loaded || busy;
      byID('save-config').disabled = !loaded || busy;
      byID('test-config').disabled = !loaded || busy;
      byID('open-diagnostics').disabled = !loaded;
    }
    function renderAccounts() {
      const body = byID('accounts-body');
      body.replaceChildren();
      accounts.forEach(function (account, index) {
        const row = element('tr');
        row.appendChild(element('td', account.account_id, 'account-id'));

        const metadataCell = element('td');
        const metadata = element('div', undefined, 'account-metadata');
        [
          ['名称', 'name', '账号名称'],
          ['邮箱', 'email', '账号邮箱'],
          ['到期', 'expires_at', '例如 2026-12-31 23:59'],
          ['额度', 'quota', '例如 $12.50 / $20.00'],
          ['时区', 'request_timezone', 'Asia/Singapore']
        ].forEach(function (entry) {
          const field = element('label', undefined, 'metadata-field');
          field.appendChild(element('span', entry[0], 'metadata-label'));
          const input = element('input');
          input.type = 'text';
          input.autocomplete = 'off';
          input.spellcheck = false;
          input.placeholder = entry[2];
          input.value = account[entry[1]] || '';
          input.setAttribute('aria-label', '账号 ' + account.account_id + ' 的' + entry[0]);
          input.addEventListener('input', function () {
            account[entry[1]] = input.value;
            markDirty();
          });
          field.appendChild(input);
          metadata.appendChild(field);
        });
        metadataCell.appendChild(metadata);
        row.appendChild(metadataCell);

        const enabledCell = element('td');
        const enabled = element('input');
        enabled.type = 'checkbox'; enabled.checked = account.enabled === true;
        enabled.setAttribute('aria-label', '启用账号 ' + account.account_id);
        enabled.addEventListener('change', function () { account.enabled = enabled.checked; markDirty(); });
        enabledCell.appendChild(enabled); row.appendChild(enabledCell);

        const egressCell = element('td');
        const egressMode = element('select');
        egressMode.setAttribute('aria-label', '账号 ' + account.account_id + ' 的出口模式');
        [['sub2', 'Sub2 原有代理'], ['plugin', '账号粘性代理'], ['generator', '代理生成器']].forEach(function (entry) {
          const option = element('option', entry[1]); option.value = entry[0]; egressMode.appendChild(option);
        });
        egressMode.value = account.egress_mode;
        egressCell.appendChild(egressMode); row.appendChild(egressCell);

        const stickyCell = element('td');
        const sticky = element('input');
        sticky.type = 'password';
        sticky.value = account.sticky_proxy_url || '';
        sticky.placeholder = 'socks5h://user-session-...:pass@host:port';
        sticky.autocomplete = 'new-password';
        sticky.spellcheck = false;
        sticky.disabled = account.egress_mode !== 'plugin';
        sticky.setAttribute('aria-label', '账号 ' + account.account_id + ' 的账号粘性代理');
        sticky.addEventListener('input', function () { account.sticky_proxy_url = sticky.value.trim(); markDirty(); });
        egressMode.addEventListener('change', function () {
          account.egress_mode = egressMode.value;
          sticky.disabled = account.egress_mode !== 'plugin';
          markDirty();
        });
        stickyCell.appendChild(sticky); row.appendChild(stickyCell);

        const planCell = element('td');
        const plan = element('select'); plan.setAttribute('aria-label', '账号 ' + account.account_id + ' 的套餐');
        [['pro', 'Pro · 292'], ['team', 'Team · 332']].forEach(function (entry) {
          const option = element('option', entry[1]); option.value = entry[0]; plan.appendChild(option);
        });
        plan.value = account.plan;
        plan.addEventListener('change', function () { account.plan = plan.value; markDirty(); });
        planCell.appendChild(plan); row.appendChild(planCell);
        const modelsCell = element('td');
        const models = element('input'); models.type = 'text'; models.value = account.models.join(', '); models.spellcheck = false;
        models.autocomplete = 'off'; models.setAttribute('aria-label', '账号 ' + account.account_id + ' 的模型');
        models.setAttribute('list', 'model-options');
        models.addEventListener('input', function () { account.models = models.value.split(',').map(function (v) { return v.trim(); }).filter(Boolean); markDirty(); });
        modelsCell.appendChild(models); row.appendChild(modelsCell);
        const deleteCell = element('td'); const remove = element('button', '删除', 'delete-button'); remove.type = 'button';
        remove.setAttribute('aria-label', '删除账号 ' + account.account_id + ' 的插件配置');
        remove.addEventListener('click', function () { accounts.splice(index, 1); renderAccounts(); markDirty(); });
        deleteCell.appendChild(remove); row.appendChild(deleteCell); body.appendChild(row);
      });
      byID('accounts-empty').hidden = accounts.length !== 0;
      byID('account-count').textContent = accounts.length + ' 个账号';
    }
    function accountMetadataByID(id) {
      const local = accounts.find(function (account) { return account.account_id === id; });
      const remote = statusAccountCatalog.find(function (account) { return account.account_id === id; });
      return {
        account_id: id,
        name: accountName(local) || accountName(remote),
        email: accountEmail(local) || accountEmail(remote),
        expires_at: accountText(local && local.expires_at, 'expires_at') || accountText(remote && remote.expires_at, 'expires_at'),
        quota: accountText(local && local.quota, 'quota') || accountText(remote && remote.quota, 'quota')
      };
    }
    function renderDetectedAccounts(ids) {
      const select = byID('new-account-id');
      select.replaceChildren();
      const placeholder = element('option', '选择已发现账号');
      placeholder.value = '';
      select.appendChild(placeholder);
      ids.forEach(function (id) {
        const option = element('option', accountOptionLabel(id, accountMetadataByID(id)));
        option.value = String(id);
        select.appendChild(option);
      });
    }
    function applyConfig(input) {
      const config = normalizeConfig(input);
      byID('enabled').checked = config.enabled === true;
      byID('upstream-proxy-url').value = config.upstream_proxy_url;
      byID('dynamic-proxy-url').value = config.dynamic_proxy_url;
      byID('ticket-mode').value = config.ticket_mode;
      byID('cookie-capture-mode').value = config.cookie_capture_mode;
      byID('cookie-capture-proxy-url').value = config.cookie_capture_proxy_url;
      byID('cookie-business-proxy-url').value = config.cookie_business_proxy_url;
      byID('standby-ticket-enabled').checked = config.standby_ticket_enabled === true;
      byID('proxy-generator-url').value = config.proxy_generator_url;
      byID('proxy-generator-blocked-countries').value = config.proxy_generator_blocked_countries.join(', ');
      byID('prefer-previous-ip').checked = config.prefer_previous_ip === true;
      byID('allow-state-780').checked = config.allow_state_780 === true;
      byID('request-rewrite-enabled').checked = config.request_rewrite_enabled === true;
      byID('default-request-timezone').value = config.default_request_timezone;
      Object.keys(numberIDs).forEach(function (key) { byID(numberIDs[key]).value = config[key]; });
      accounts = config.accounts;
      renderAccounts();
      renderTicketMode();
      dirty = false;
      updateSaveState();
    }
    function formConfig() {
      const config = {
        enabled: byID('enabled').checked,
        upstream_proxy_id: 0,
        upstream_proxy_url: byID('upstream-proxy-url').value.trim(),
        dynamic_proxy_url: byID('dynamic-proxy-url').value.trim(),
        ticket_mode: byID('ticket-mode').value,
        cookie_capture_mode: byID('cookie-capture-mode').value,
        cookie_capture_proxy_url: byID('cookie-capture-proxy-url').value.trim(),
        cookie_business_proxy_url: byID('cookie-business-proxy-url').value.trim(),
        standby_ticket_enabled: byID('standby-ticket-enabled').checked,
        proxy_generator_url: byID('proxy-generator-url').value.trim(),
        proxy_generator_blocked_countries: byID('proxy-generator-blocked-countries').value.split(',').map(function (value) { return value.trim(); }).filter(Boolean),
        prefer_previous_ip: byID('prefer-previous-ip').checked,
        allow_state_780: byID('allow-state-780').checked,
        request_rewrite_enabled: byID('request-rewrite-enabled').checked,
        default_request_timezone: byID('default-request-timezone').value.trim()
      };
      Object.keys(numberIDs).forEach(function (key) {
        const raw = byID(numberIDs[key]).value.trim();
        config[key] = raw === '' ? NaN : Number(raw);
      });
      config.accounts = accounts.map(function (account) { return {
        account_id: account.account_id, name: accountText(account.name, 'name'), email: accountText(account.email, 'email'),
        expires_at: accountText(account.expires_at, 'expires_at'), quota: accountText(account.quota, 'quota'),
        enabled: account.enabled, egress_mode: account.egress_mode, sticky_proxy_url: account.sticky_proxy_url,
        plan: account.plan, request_timezone: accountText(account.request_timezone, 'request_timezone'), models: account.models.slice()
      }; });
      return validateConfig(config);
    }
    function renderStatus(status) {
      const connection = byID('connection-status');
      connection.textContent = status.host_ready ? '宿主已连接' : '等待宿主初始化';
      connection.className = 'badge ' + (status.host_ready ? 'success' : 'warning');
      byID('status-summary').textContent = status.message || (status.host_ready ? '状态已更新' : '等待宿主提供账号信息；可先保存配置。');
      statusAccountCatalog = status.account_catalog.slice();
      renderDetectedAccounts(status.account_ids);
      byID('account-discovery').textContent = status.account_ids.length ? '发现 ' + status.account_ids.length + ' 个账号。下拉只提供账号 ID；名称、邮箱、到期时间和额度请在添加后填写。' : '暂未发现账号，也可以手动填写 ID。宿主不会向此页面提供账号 Token。';
      const body = byID('tickets-body'); body.replaceChildren();
      status.tickets.forEach(function (ticket) {
        const id = accountID(ticket.account_id);
        const info = accountMetadataByID(id);
        const row = element('tr'); const account = element('td');
        const model = typeof ticket.model === 'string' && MODEL_PATTERN.test(ticket.model) ? ticket.model : '未知模型';
        account.appendChild(element('span', '账号：' + id + (info.name ? ' · ' + info.name : ''), 'status-account'));
        account.appendChild(element('span', '模型：' + model, 'status-model')); row.appendChild(account);
        const plan = ticket.plan === 'team' ? 'Team · 332' : ticket.plan === 'pro' ? 'Pro · 292' : '—';
        row.appendChild(element('td', plan + ' · ' + (ticket.ticket_mode === 'cookie' ? 'Cookie 分流' : '旧模式')));
        const state = stateLabel(ticket.state); const stateCell = element('td');
        stateCell.appendChild(element('span', state[0], 'badge ' + state[1])); row.appendChild(stateCell);
        const remaining = element('td', remainingText(ticket.remaining_seconds));
        if (typeof ticket.expires_at === 'string' && Number.isFinite(Date.parse(ticket.expires_at))) remaining.title = '到期时间：' + new Date(ticket.expires_at).toLocaleString('zh-CN');
        if (ticket.standby_ready === true) remaining.appendChild(element('div', '备用票：' + remainingText(ticket.standby_remaining_seconds), 'help'));
        row.appendChild(remaining);
        const attempts = Number.isSafeInteger(ticket.attempts) && ticket.attempts > 0 ? '本轮尝试 ' + ticket.attempts + ' 次' : '—';
        const detail = element('td', attempts, 'error-detail');
        if (ticket.last_error) detail.appendChild(element('div', errorLabel(ticket.last_error)));
        row.appendChild(detail); body.appendChild(row);
      });
      byID('tickets-empty').hidden = status.tickets.length !== 0;
      lastDiagnostics = status.diagnostics.slice();
      diagnosticsListening = status.diagnostics_listening;
      renderDiagnostics();
    }
    function renderDiagnostics() {
      const body = byID('diagnostics-body');
      body.replaceChildren();
      lastDiagnostics.slice().reverse().forEach(function (event) {
        const row = element('tr');
        row.appendChild(element('td', event.time ? new Date(event.time).toLocaleString('zh-CN') : '—'));
        const accountModel = element('td');
        accountModel.appendChild(element('span', event.account_id ? '#' + event.account_id : '—', 'status-account'));
        accountModel.appendChild(element('span', event.model || '—', 'status-model'));
        row.appendChild(accountModel);
        const stage = element('td', diagnosticStageLabel(event.stage));
        if (event.attempt) stage.appendChild(element('div', '第 ' + event.attempt + ' 次', 'diagnostic-sub'));
        row.appendChild(stage);
        row.appendChild(element('td', [event.upstream_proxy, event.target_proxy].filter(Boolean).join(' → ') || '—', 'diagnostic-mono'));
        const egress = element('td', '采集：' + (event.capture_egress || '未知') + '\n固定：' + (event.fixed_egress || '未知'), 'diagnostic-mono');
        if (event.egress_match === true) egress.appendChild(element('span', '出口一致', 'badge success'));
        else if (event.egress_match === false) egress.appendChild(element('span', '出口不一致', 'badge warning'));
        row.appendChild(egress);
        row.appendChild(element('td', (event.state_class || '—') + (event.state_length ? ' · ' + event.state_length : '')));
        row.appendChild(element('td', (event.http_status ? event.http_status + ' · ' : '') + (event.response_model || '—'), 'diagnostic-mono'));
        const outcome = element('td');
        outcome.appendChild(element('span', diagnosticOutcomeLabel(event.outcome), event.outcome === 'accepted' || event.outcome === 'model_match' ? 'badge success' : event.outcome === 'model_mismatch' || event.outcome === 'state_312' || event.outcome === 'validation_failed' ? 'badge warning' : 'badge'));
        if (event.error) outcome.appendChild(element('div', event.error, 'diagnostic-sub'));
        row.appendChild(outcome);
        row.appendChild(element('td', event.duration_ms ? event.duration_ms + ' ms' : '—'));
        body.appendChild(row);
      });
      byID('diagnostics-empty').hidden = lastDiagnostics.length !== 0;
      if (!diagnosticsOpen) {
        byID('diagnostics-summary').textContent = '打开面板后开始实时监听，不保存日志。';
      } else if (!diagnosticsListening) {
        byID('diagnostics-summary').textContent = '正在建立实时监听…';
      } else if (lastDiagnostics.length) {
        byID('diagnostics-summary').textContent = '实时监听中 · 最近 ' + lastDiagnostics.length + ' 条，新事件置顶，不保存。';
      } else {
        byID('diagnostics-summary').textContent = '正在监听，等待新事件；关闭面板即停止。';
      }
    }
    async function refreshStatus() {
      if (closed || statusBusy || !bridge) return;
      statusBusy = true; byID('refresh-status').disabled = true;
      try {
        const response = await bridge.status();
        if (!closed) renderStatus(parseStatus(response.result));
      } catch (error) {
        if (!closed) {
          byID('connection-status').textContent = '状态暂不可用';
          byID('connection-status').className = 'badge warning';
          byID('status-summary').textContent = redactError(error.message);
        }
      } finally { statusBusy = false; if (!closed) byID('refresh-status').disabled = false; }
    }
    byID('config-form').addEventListener('input', markDirty);
    byID('config-form').addEventListener('change', markDirty);
    byID('ticket-mode').addEventListener('change', function () { renderTicketMode(); markDirty(); });
    byID('cookie-capture-mode').addEventListener('change', function () { renderTicketMode(); markDirty(); });
    byID('standby-ticket-enabled').addEventListener('change', function () { renderTicketMode(); markDirty(); });
    async function saveConfig(event) {
      event.preventDefault(); if (busy || !loaded) return;
      let config;
      try { config = formConfig(); } catch (error) { notice(error.message, 'error'); return; }
      setBusy(true); updateSaveState('正在保存…');
      try {
        const response = await bridge.save(config);
        if (closed) return;
        applyConfig(response.config); updateSaveState('已保存');
        notice(config.enabled ? '设置已保存。仅开启的账号会参与票据获取与注入。' : '设置已保存。STATE Kit 已关闭，正常请求继续转发。', 'success');
        await refreshStatus();
      } catch (error) { if (!closed) { notice(error.message, 'error'); updateSaveState('保存未确认；重新打开配置页可核对宿主结果。'); } }
      finally { if (!closed) setBusy(false); }
    }
    // The host iframe does not grant allow-forms: save via Bridge on an explicit
    // button click instead of relying on sandbox-blocked native form submission.
    byID('save-config').addEventListener('click', saveConfig);
    byID('config-form').addEventListener('submit', saveConfig);
    byID('add-account').addEventListener('click', function () {
      const manual = byID('manual-account-id').value.trim();
      const id = accountID(manual || byID('new-account-id').value);
      if (id === null) { notice('请选择已发现账号，或手动输入有效的正整数账号 ID。', 'error'); return; }
      if (accounts.some(function (account) { return account.account_id === id; })) { notice('此账号已在列表中。', 'error'); return; }
      if (accounts.length >= 256) { notice('最多配置 256 个账号。', 'error'); return; }
      const metadata = accountMetadataByID(id);
      accounts.push({ account_id: id, name: metadata.name, email: metadata.email, expires_at: metadata.expires_at,
        quota: metadata.quota, enabled: false, egress_mode: 'sub2', sticky_proxy_url: '', plan: 'pro',
        request_timezone: byID('default-request-timezone').value.trim() || DEFAULT_CONFIG.default_request_timezone,
        models: ['gpt-6-astra'] });
      renderAccounts(); markDirty(); byID('new-account-id').value = ''; byID('manual-account-id').value = '';
      notice('已添加账号 ' + id + '，默认关闭。补全账号资料、选择套餐和模型后，可手动开启并保存。');
    });
    byID('new-account-id').addEventListener('keydown', function (event) {
      if (event.key === 'Enter') { event.preventDefault(); byID('add-account').click(); }
    });
    byID('manual-account-id').addEventListener('keydown', function (event) {
      if (event.key === 'Enter') { event.preventDefault(); byID('add-account').click(); }
    });
    byID('toggle-proxy').addEventListener('click', function () {
      const input = byID('dynamic-proxy-url'); const reveal = input.type === 'password';
      input.type = reveal ? 'text' : 'password'; byID('toggle-proxy').textContent = reveal ? '隐藏' : '显示';
      byID('toggle-proxy').setAttribute('aria-pressed', String(reveal));
    });
    byID('toggle-upstream-proxy').addEventListener('click', function () {
      const input = byID('upstream-proxy-url'); const reveal = input.type === 'password';
      input.type = reveal ? 'text' : 'password'; byID('toggle-upstream-proxy').textContent = reveal ? '隐藏' : '显示';
      byID('toggle-upstream-proxy').setAttribute('aria-pressed', String(reveal));
    });
    byID('test-config').addEventListener('click', async function () {
      if (busy || !loaded) return;
      setBusy(true); notice('正在检查已保存配置；未保存修改不参与检查。');
      try {
        const response = await bridge.test();
        if (!closed) notice((response.result && response.result.message || '已保存配置检查完成。') + (dirty ? ' 当前表单还有未保存修改。' : ''), 'success');
      } catch (error) { if (!closed) notice(error.message, 'error'); }
      finally { if (!closed) setBusy(false); }
    });
    byID('refresh-status').addEventListener('click', refreshStatus);
    async function signalDiagnosticsListener() {
      // The host bridge exposes no subscription call. Send the close status
      // polls in parallel so their arrival gap stays small even when one host
      // status request is slow.
      const results = await Promise.allSettled([bridge.status(), bridge.status(), bridge.status()]);
      const fulfilled = results.filter(function (entry) { return entry.status === 'fulfilled'; });
      if (!fulfilled.length) {
        const first = results.find(function (entry) { return entry.status === 'rejected'; });
        throw first && first.reason || new Error('宿主状态请求失败。');
      }
      if (!closed) renderStatus(parseStatus(fulfilled[fulfilled.length - 1].value.result));
    }
    async function startDiagnosticsListening() {
      diagnosticsOpen = true;
      lastDiagnostics = [];
      byID('diagnostics-summary').textContent = '正在建立实时监听…';
      renderDiagnostics();
      try {
        await signalDiagnosticsListener();
      } catch (error) {
        if (!closed) {
          byID('diagnostics-summary').textContent = redactError(error.message);
          notice(redactError(error.message), 'error');
        }
        return;
      }
      if (!diagnosticsOpen || closed) return;
      global.clearInterval(diagnosticsTimer);
      diagnosticsTimer = global.setInterval(refreshStatus, 500);
    }
    function stopDiagnosticsListening() {
      diagnosticsOpen = false;
      diagnosticsListening = false;
      global.clearInterval(diagnosticsTimer);
      diagnosticsTimer = undefined;
      lastDiagnostics = [];
      renderDiagnostics();
    }
    byID('open-diagnostics').addEventListener('click', async function () {
      const dialog = byID('diagnostics-dialog');
      if (typeof dialog.showModal === 'function') dialog.showModal();
      else dialog.hidden = false;
      await startDiagnosticsListening();
    });
    byID('diagnostics-refresh').addEventListener('click', refreshStatus);
    byID('diagnostics-close').addEventListener('click', function () {
      const dialog = byID('diagnostics-dialog');
      if (typeof dialog.close === 'function') dialog.close();
      else dialog.hidden = true;
    });
    byID('diagnostics-dialog').addEventListener('close', stopDiagnosticsListening);
    byID('diagnostics-copy').addEventListener('click', async function () {
      if (diagnosticsBusy) return;
      diagnosticsBusy = true;
      try {
        const text = lastDiagnostics.slice().reverse().map(function (event) {
          return [event.time, event.account_id ? '#' + event.account_id : '', event.model, diagnosticStageLabel(event.stage),
            [event.upstream_proxy, event.target_proxy].filter(Boolean).join(' -> '), 'capture=' + (event.capture_egress || 'unknown'),
            'fixed=' + (event.fixed_egress || 'unknown'), 'state=' + (event.state_class || 'unknown'), 'response=' + (event.response_model || 'unknown'),
            'outcome=' + event.outcome, event.error].filter(Boolean).join('\t');
        }).join('\n');
        if (global.navigator && global.navigator.clipboard && global.navigator.clipboard.writeText) await global.navigator.clipboard.writeText(text);
        notice(text ? '诊断日志已复制。' : '当前没有可复制的诊断日志。', text ? 'success' : undefined);
      } catch (error) {
        notice('复制诊断日志失败：' + error.message, 'error');
      } finally { diagnosticsBusy = false; }
    });
    function resize() { try { bridge.resize(document.documentElement.scrollHeight); } catch (_) { /* Context may already be closed. */ } }
    function stop() {
      if (closed) return;
      closed = true; global.clearInterval(pollTimer);
      stopDiagnosticsListening();
      if (resizeObserver) resizeObserver.disconnect();
      if (bridge) bridge.dispose();
      global.removeEventListener('pagehide', stop);
    }
    global.addEventListener('pagehide', stop);
    (async function () {
      try {
        if (!bridge) throw new Error('配置桥接未加载，请重新打开插件配置页。');
        bridge.ready();
        const response = await bridge.load();
        if (closed) return;
        applyConfig(response.config); loaded = true; setBusy(false); resize();
        if (global.ResizeObserver) { resizeObserver = new global.ResizeObserver(resize); resizeObserver.observe(document.body); }
        await refreshStatus();
        if (!closed) pollTimer = global.setInterval(function () { if (document.visibilityState !== 'hidden') refreshStatus(); }, 10000);
      } catch (error) { if (!closed) { notice(error.message, 'error'); updateSaveState('配置未加载'); byID('connection-status').textContent = '连接失败'; } }
    })();
    return { stop: stop, refreshStatus: refreshStatus };
  }
  return { DEFAULT_CONFIG: DEFAULT_CONFIG, normalizeConfig: normalizeConfig, validateConfig: validateConfig,
    validateTimezone: validateTimezone,
    accountID: accountID, accountOptionLabel: accountOptionLabel, normalizeAccountCatalog: normalizeAccountCatalog,
    parseStatus: parseStatus, stateLabel: stateLabel, errorLabel: errorLabel, redactError: redactError, remainingText: remainingText,
    diagnosticStageLabel: diagnosticStageLabel, diagnosticOutcomeLabel: diagnosticOutcomeLabel, start: start };
});
