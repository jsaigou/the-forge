export const SAMPLE_RATE = 24000;
export const FRAME_SAMPLES = 3200;
export const FRAME_MS = (FRAME_SAMPLES * 1000) / SAMPLE_RATE;
export const FRAME_BYTES = 5 + FRAME_SAMPLES * 2;
export const PROTOCOL = 'forge-duplex.v1';

export class ProtocolError extends Error {
  constructor(code, message) {
    super(message);
    this.name = 'ProtocolError';
    this.code = code;
  }
}

export function floatToInt16(sample) {
  const clamped = Math.max(-1, Math.min(1, Number.isFinite(sample) ? sample : 0));
  return clamped < 0 ? Math.round(clamped * 0x8000) : Math.round(clamped * 0x7fff);
}

export function encodeAudioFrame(seq, samples) {
  if (!samples || samples.length !== FRAME_SAMPLES) {
    throw new RangeError(`frame must have ${FRAME_SAMPLES} samples, got ${samples ? samples.length : 0}`);
  }
  const buffer = new ArrayBuffer(FRAME_BYTES);
  const view = new DataView(buffer);
  view.setUint8(0, 0x01);
  view.setUint32(1, seq >>> 0, true);
  for (let i = 0; i < FRAME_SAMPLES; i++) {
    view.setInt16(5 + i * 2, floatToInt16(samples[i]), true);
  }
  return buffer;
}

export function decodeAudioFrame(data) {
  if (!(data instanceof ArrayBuffer)) {
    throw new ProtocolError('bad_frame', 'binary frame is not an ArrayBuffer');
  }
  if (data.byteLength !== FRAME_BYTES) {
    throw new ProtocolError('bad_frame', `frame is ${data.byteLength} bytes, expected ${FRAME_BYTES}`);
  }
  const view = new DataView(data);
  const kind = view.getUint8(0);
  if (kind !== 0x01) {
    throw new ProtocolError('bad_frame', `unsupported frame kind 0x${kind.toString(16)}`);
  }
  const seq = view.getUint32(1, true);
  const samples = new Float32Array(FRAME_SAMPLES);
  for (let i = 0; i < FRAME_SAMPLES; i++) {
    samples[i] = view.getInt16(5 + i * 2, true) / 32768;
  }
  return { seq, samples };
}

export class LinearResampler {
  constructor(inRate, outRate = SAMPLE_RATE) {
    if (!(inRate > 0) || !(outRate > 0)) throw new RangeError('sample rates must be positive');
    this.inRate = inRate;
    this.outRate = outRate;
    this.ratio = inRate / outRate;
    this.passthrough = inRate === outRate;
    this.tail = new Float32Array(0);
    this.pos = 0;
  }

  reset() {
    this.tail = new Float32Array(0);
    this.pos = 0;
  }

  process(input) {
    if (!input || input.length === 0) return new Float32Array(0);
    if (this.passthrough) return input;
    const buffer = new Float32Array(this.tail.length + input.length);
    buffer.set(this.tail, 0);
    buffer.set(input, this.tail.length);
    const out = new Float32Array(Math.ceil(buffer.length / this.ratio) + 1);
    let count = 0;
    let pos = this.pos;
    while (pos + 1 < buffer.length) {
      const i = Math.floor(pos);
      const frac = pos - i;
      out[count++] = buffer[i] + (buffer[i + 1] - buffer[i]) * frac;
      pos += this.ratio;
    }
    const keepFrom = Math.min(Math.floor(pos), buffer.length - 1);
    this.tail = buffer.slice(keepFrom);
    this.pos = pos - keepFrom;
    return out.subarray(0, count);
  }
}

export class FrameAssembler {
  constructor({ frameSamples = FRAME_SAMPLES, capacityFrames = 4 } = {}) {
    if (capacityFrames < 1) throw new RangeError('capacityFrames must be at least 1');
    this.frameSamples = frameSamples;
    this.capacity = frameSamples * capacityFrames;
    this.buffer = new Float32Array(this.capacity);
    this.read = 0;
    this.size = 0;
    this.dropped = 0;
  }

  get available() {
    return this.size;
  }

  get framesAvailable() {
    return Math.floor(this.size / this.frameSamples);
  }

  clear() {
    this.read = 0;
    this.size = 0;
  }

  push(input) {
    let offset = 0;
    while (offset < input.length) {
      if (this.size === this.capacity) {
        this.read = (this.read + this.frameSamples) % this.capacity;
        this.size -= this.frameSamples;
        this.dropped++;
      }
      const write = (this.read + this.size) % this.capacity;
      const chunk = Math.min(input.length - offset, this.capacity - write);
      this.buffer.set(input.subarray(offset, offset + chunk), write);
      this.size += chunk;
      offset += chunk;
    }
  }

  popFrame() {
    if (this.size < this.frameSamples) return null;
    const out = new Float32Array(this.frameSamples);
    const first = Math.min(this.frameSamples, this.capacity - this.read);
    out.set(this.buffer.subarray(this.read, this.read + first), 0);
    if (first < this.frameSamples) {
      out.set(this.buffer.subarray(0, this.frameSamples - first), first);
    }
    this.read = (this.read + this.frameSamples) % this.capacity;
    this.size -= this.frameSamples;
    return out;
  }
}

export class Percentiles {
  constructor(capacity = 512) {
    this.capacity = capacity;
    this.values = [];
  }

  add(value) {
    if (!Number.isFinite(value)) return;
    this.values.push(value);
    if (this.values.length > this.capacity) this.values.shift();
  }

  clear() {
    this.values = [];
  }

  quantile(q) {
    if (this.values.length === 0) return null;
    const sorted = [...this.values].sort((a, b) => a - b);
    const index = Math.min(sorted.length - 1, Math.max(0, Math.ceil(q * sorted.length) - 1));
    return sorted[index];
  }

  get p50() { return this.quantile(0.5); }
  get p95() { return this.quantile(0.95); }
  get max() { return this.quantile(1); }
  get count() { return this.values.length; }
}

export class FramePacer {
  constructor({ assembler = new FrameAssembler(), frameMs = FRAME_MS, clock = () => performance.now() } = {}) {
    this.assembler = assembler;
    this.frameMs = frameMs;
    this.clock = clock;
    this.nextAt = null;
    this.lateFrames = 0;
    this.lateMs = 0;
  }

  reset() {
    this.nextAt = null;
  }

  push(samples) {
    this.assembler.push(samples);
  }

  nextFrame(now = this.clock()) {
    if (this.nextAt === null) this.nextAt = now;
    if (now < this.nextAt) return null;
    const frame = this.assembler.popFrame();
    if (!frame) return null;
    const lateMs = now - this.nextAt;
    if (lateMs > this.frameMs * 1.5) {
      this.lateFrames++;
      this.lateMs += lateMs;
      this.nextAt = now + this.frameMs;
    } else {
      this.nextAt += this.frameMs;
    }
    return { frame, lateMs };
  }
}

export class Playout {
  constructor({ ctx, targetFrames = 1, frameSamples = FRAME_SAMPLES, sampleRate = SAMPLE_RATE } = {}) {
    if (!ctx) throw new Error('Playout needs an AudioContext');
    this.ctx = ctx;
    this.targetFrames = targetFrames;
    this.frameSamples = frameSamples;
    this.sampleRate = sampleRate;
    this.queue = [];
    this.sources = new Set();
    this.playhead = 0;
    this.started = false;
    this.underruns = 0;
    this.underrunMs = 0;
    this.dropped = 0;
    this.scheduledFrames = 0;
    this.latency = new Percentiles(256);
    this.maxQueue = targetFrames + 6;
  }

  get frameDuration() {
    return this.frameSamples / this.sampleRate;
  }

  get queued() {
    return this.queue.length;
  }

  setTargetFrames(frames) {
    this.targetFrames = Math.max(1, Math.min(2, frames));
    this.maxQueue = this.targetFrames + 6;
    this.stopAll();
  }

  reset() {
    this.stopAll();
    this.underruns = 0;
    this.underrunMs = 0;
    this.dropped = 0;
    this.scheduledFrames = 0;
    this.latency.clear();
  }

  stopAll() {
    for (const source of this.sources) {
      try { source.stop(); } catch { /* already stopped */ }
    }
    this.sources.clear();
    this.queue = [];
    this.playhead = 0;
    this.started = false;
  }

  push(samples) {
    if (samples.length !== this.frameSamples) return;
    if (this.queue.length >= this.maxQueue) {
      this.queue.shift();
      this.dropped++;
    }
    this.queue.push(samples);
    this.pump();
  }

  pump() {
    const now = this.ctx.currentTime;
    if (!this.started) {
      if (this.queue.length < this.targetFrames) return;
      this.started = true;
      this.playhead = now + this.targetFrames * this.frameDuration;
    }
    while (this.queue.length > 0) {
      if (this.playhead < now) {
        this.underruns++;
        this.underrunMs += (now - this.playhead) * 1000;
        this.playhead = now + 0.02;
      }
      const samples = this.queue.shift();
      this.schedule(samples);
    }
  }

  schedule(samples) {
    const buffer = this.ctx.createBuffer(1, samples.length, this.sampleRate);
    if (typeof buffer.copyToChannel === 'function') buffer.copyToChannel(samples, 0);
    const source = this.ctx.createBufferSource();
    source.buffer = buffer;
    source.connect(this.ctx.destination);
    const startAt = this.playhead;
    this.latency.add((startAt - this.ctx.currentTime) * 1000);
    try {
      source.start(startAt);
    } catch {
      source.start();
    }
    this.playhead += samples.length / this.sampleRate;
    this.scheduledFrames++;
    this.sources.add(source);
    source.onended = () => this.sources.delete(source);
  }
}

const RUNNING_STATES = new Set(['starting', 'running']);

export class DuplexSession {
  constructor({
    url,
    token = '',
    tenant = 'loopback',
    text = true,
    maxMinutes = 30,
    socketFactory = (socketUrl, protocols) => new WebSocket(socketUrl, protocols),
    onEvent = () => {},
  }) {
    this.url = url;
    this.token = token;
    this.tenant = tenant;
    this.text = text;
    this.maxMinutes = maxMinutes;
    this.socketFactory = socketFactory;
    this.emit = onEvent;
    this.socket = null;
    this.state = 'idle';
    this.outSeq = 0;
    this.inSeq = 0;
    this.sentFrames = 0;
    this.receivedFrames = 0;
    this.gaps = 0;
    this.bytesSent = 0;
    this.bytesReceived = 0;
    this.info = null;
    this.startedAt = 0;
    this.closeTimer = null;
  }

  setState(state, detail = {}) {
    this.state = state;
    this.emit({ type: 'state', state, ...detail });
  }

  start() {
    if (this.socket) return;
    this.setState('connecting');
    const protocols = [PROTOCOL];
    if (this.token) protocols.push(`bearer.${this.token}`);
    let socket;
    try {
      socket = this.socketFactory(this.url, protocols);
    } catch (error) {
      this.fail('connect', error.message);
      return;
    }
    this.socket = socket;
    socket.binaryType = 'arraybuffer';
    socket.onopen = () => {
      this.startedAt = Date.now();
      this.setState('starting');
      this.sendControl({
        type: 'session.start',
        version: 1,
        tenant: this.tenant,
        sample_rate: SAMPLE_RATE,
        max_minutes: this.maxMinutes,
        text: this.text,
        client: { name: 'forge-duplex-web', version: '0.1' },
      });
    };
    socket.onmessage = (event) => {
      this.onMessage(event.data).catch((error) => this.emit({ type: 'log', level: 'error', message: error.message }));
    };
    socket.onerror = () => this.emit({ type: 'log', level: 'error', message: 'WebSocket transport error' });
    socket.onclose = (event) => this.onClose(event);
  }

  sendControl(message) {
    if (!this.socket || this.socket.readyState !== 1) return false;
    const payload = JSON.stringify(message);
    this.socket.send(payload);
    this.bytesSent += payload.length;
    return true;
  }

  sendFrame(samples) {
    if (this.state !== 'running' || !this.socket || this.socket.readyState !== 1) return false;
    let buffer;
    try {
      buffer = encodeAudioFrame(this.outSeq, samples);
    } catch (error) {
      this.emit({ type: 'log', level: 'error', message: error.message });
      return false;
    }
    this.socket.send(buffer);
    this.outSeq = (this.outSeq + 1) >>> 0;
    this.sentFrames++;
    this.bytesSent += buffer.byteLength;
    return true;
  }

  close(reason = 'client_done') {
    if (!this.socket) return;
    if (RUNNING_STATES.has(this.state)) {
      this.setState('draining', { reason });
      this.sendControl({ type: 'session.close', reason });
    }
    this.armCloseTimer();
  }

  cancel(reason = 'user_hangup') {
    if (!this.socket) return;
    if (RUNNING_STATES.has(this.state)) {
      this.setState('draining', { reason });
      this.sendControl({ type: 'session.cancel', reason });
    }
    this.armCloseTimer();
  }

  armCloseTimer() {
    clearTimeout(this.closeTimer);
    this.closeTimer = setTimeout(() => this.teardown(), 3000);
  }

  teardown() {
    clearTimeout(this.closeTimer);
    this.closeTimer = null;
    const socket = this.socket;
    this.socket = null;
    if (!socket) return;
    try { socket.close(1000, 'client closed'); } catch { /* already closing */ }
    if (this.state !== 'closed' && this.state !== 'error') {
      this.setState('closed', { reason: 'client_closed' });
      this.emit({ type: 'done', reason: 'client_closed', stats: null });
    }
  }

  fail(code, message) {
    this.emit({ type: 'log', level: 'error', message: `${code}: ${message}` });
    this.emit({ type: 'error', code, message, fatal: true });
    this.setState('error', { code, message });
    this.teardown();
  }

  async onMessage(data) {
    if (typeof data === 'string') {
      this.onControl(data);
      return;
    }
    let buffer = data;
    if (typeof Blob !== 'undefined' && data instanceof Blob) buffer = await data.arrayBuffer();
    else if (ArrayBuffer.isView(data)) buffer = data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength);
    if (buffer instanceof ArrayBuffer) this.onBinary(buffer);
  }

  onBinary(buffer) {
    let frame;
    try {
      frame = decodeAudioFrame(buffer);
    } catch (error) {
      this.fail(error.code || 'bad_frame', error.message);
      return;
    }
    this.bytesReceived += buffer.byteLength;
    if (this.receivedFrames > 0 && frame.seq !== this.inSeq) {
      this.gaps++;
      this.emit({ type: 'gap', expected: this.inSeq, got: frame.seq });
    }
    this.inSeq = (frame.seq + 1) >>> 0;
    this.receivedFrames++;
    this.emit({ type: 'frame', seq: frame.seq, samples: frame.samples });
  }

  onControl(raw) {
    let message;
    try {
      message = JSON.parse(raw);
    } catch {
      this.fail('bad_request', 'server sent invalid JSON');
      return;
    }
    switch (message.type) {
      case 'session.ready':
        this.info = message;
        this.setState('running', { info: message });
        break;
      case 'text.delta':
        this.emit({ type: 'text', seq: message.seq, text: message.text });
        break;
      case 'stats':
        this.emit({ type: 'stats', stats: message });
        break;
      case 'session.done':
        clearTimeout(this.closeTimer);
        this.setState('closed', { reason: message.reason });
        this.emit({ type: 'done', reason: message.reason, stats: message.stats });
        this.teardown();
        break;
      case 'error':
        this.fail(message.code || 'internal', message.message || 'server error');
        break;
      case 'pong':
        this.emit({ type: 'pong', t: message.t });
        break;
      default:
        this.emit({ type: 'log', level: 'warn', message: `unknown server message: ${message.type}` });
    }
  }

  onClose(event) {
    this.socket = null;
    clearTimeout(this.closeTimer);
    if (this.state === 'closed' || this.state === 'error') return;
    if (this.state === 'draining') {
      this.setState('closed', { reason: event?.reason || 'socket_closed' });
      this.emit({ type: 'done', reason: event?.reason || 'socket_closed', stats: null });
      return;
    }
    this.setState('disconnected', { code: event?.code, reason: event?.reason });
    this.emit({ type: 'disconnected', code: event?.code, reason: event?.reason });
  }

  snapshot() {
    return {
      state: this.state,
      sessionId: this.info?.session_id || null,
      tenant: this.info?.tenant || this.tenant,
      sentFrames: this.sentFrames,
      receivedFrames: this.receivedFrames,
      outSeq: this.outSeq,
      inSeq: this.inSeq,
      gaps: this.gaps,
      bytesSent: this.bytesSent,
      bytesReceived: this.bytesReceived,
      uptimeMs: this.startedAt ? Date.now() - this.startedAt : 0,
    };
  }
}

const CAPTURE_WORKLET = `
class DuplexCapture extends AudioWorkletProcessor {
  process(inputs) {
    const channel = inputs[0] && inputs[0][0];
    if (channel && channel.length) {
      const copy = new Float32Array(channel.length);
      copy.set(channel);
      this.port.postMessage(copy, [copy.buffer]);
    }
    return true;
  }
}
registerProcessor('duplex-capture', DuplexCapture);
`;

function qs(id) {
  return document.getElementById(id);
}

function setText(id, value) {
  const el = qs(id);
  if (!el) return;
  const next = value == null ? '—' : String(value);
  if (el.textContent !== next) el.textContent = next;
}

function fmtMs(value) {
  return value == null || !Number.isFinite(value) ? '—' : `${value.toFixed(1)} ms`;
}

function fmtBytes(bytes) {
  if (!bytes) return '0 B';
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
  return `${(bytes / (1024 * 1024)).toFixed(2)} MiB`;
}

function defaultWsUrl() {
  const params = new URLSearchParams(location.search);
  const explicit = params.get('ws');
  if (explicit) return explicit;
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
  if (location.host) return `${scheme}//${location.host}/v1/audio/duplex`;
  return 'ws://localhost:8787/v1/audio/duplex';
}

function withTicket(url, ticket) {
  if (!ticket) return url;
  try {
    const parsed = new URL(url);
    parsed.searchParams.set('ticket', ticket);
    return parsed.toString();
  } catch {
    return url;
  }
}

function startApp() {
  const params = new URLSearchParams(location.search);
  const token = params.get('token') || '';
  const ticket = params.get('ticket') || '';
  const tenant = params.get('tenant') || 'loopback';
  const state = {
    ctx: null,
    session: null,
    pacer: null,
    assembler: null,
    playout: null,
    mic: null,
    muted: false,
    autoReconnect: true,
    reconnectAttempts: 0,
    reconnectTimer: null,
    playoutTarget: 1,
    serverStats: null,
    logs: [],
    transcript: '',
    timer: null,
    renderTimer: null,
  };

  function log(level, message) {
    state.logs.push({ at: new Date(), level, message });
    if (state.logs.length > 200) state.logs.shift();
    renderLog();
  }

  function renderLog() {
    const list = qs('log');
    if (!list) return;
    list.replaceChildren(...state.logs.slice(-60).map((entry) => {
      const item = document.createElement('li');
      item.className = `log-${entry.level}`;
      const time = document.createElement('span');
      time.className = 'log-time';
      time.textContent = entry.at.toLocaleTimeString();
      item.append(time, document.createTextNode(entry.message));
      return item;
    }));
    list.scrollTop = list.scrollHeight;
  }

  function appendTranscript(text) {
    state.transcript += text;
    if (state.transcript.length > 8000) state.transcript = state.transcript.slice(-8000);
    const pane = qs('transcript');
    if (pane) pane.textContent = state.transcript;
  }

  function setBadge(name) {
    const badge = qs('stateBadge');
    if (!badge) return;
    badge.textContent = name;
    badge.className = `badge state-${name}`;
  }

  function showError(message) {
    const banner = qs('errorBanner');
    if (!banner) return;
    banner.textContent = message;
    banner.hidden = !message;
  }

  function setMicState(name, detail = '') {
    const el = qs('micState');
    if (el) {
      el.textContent = detail ? `${name} — ${detail}` : name;
      el.className = `mic mic-${name.split(' ')[0]}`;
    }
    const retry = qs('retryMicBtn');
    if (retry) retry.hidden = name !== 'denied' && name !== 'error';
    const mute = qs('micMuted');
    if (mute) mute.disabled = name !== 'on';
  }

  function updateButtons() {
    const running = state.session && ['starting', 'running', 'draining'].includes(state.session.state);
    if (qs('startBtn')) qs('startBtn').disabled = Boolean(state.session);
    if (qs('stopBtn')) qs('stopBtn').disabled = !running;
    if (qs('cancelBtn')) qs('cancelBtn').disabled = !state.session || state.session.state !== 'running';
    if (qs('wsUrl')) qs('wsUrl').disabled = Boolean(state.session);
  }

  async function ensureAudioContext() {
    if (state.ctx) return state.ctx;
    const Ctor = window.AudioContext || window.webkitAudioContext;
    if (!Ctor) throw new Error('Web Audio is not available in this browser');
    let ctx;
    try {
      ctx = new Ctor({ sampleRate: SAMPLE_RATE, latencyHint: 'interactive' });
    } catch {
      ctx = new Ctor();
    }
    if (ctx.sampleRate !== SAMPLE_RATE) {
      log('warn', `AudioContext runs at ${ctx.sampleRate} Hz; the mic is resampled to ${SAMPLE_RATE} Hz`);
    }
    state.ctx = ctx;
    return ctx;
  }

  async function startMic() {
    if (!state.ctx) return;
    setMicState('requesting');
    try {
      const stream = await navigator.mediaDevices.getUserMedia({
        audio: {
          channelCount: 1,
          sampleRate: SAMPLE_RATE,
          echoCancellation: true,
          noiseSuppression: true,
          autoGainControl: true,
        },
      });
      if (state.mic) stopMic();
      const source = state.ctx.createMediaStreamSource(stream);
      const workletUrl = URL.createObjectURL(new Blob([CAPTURE_WORKLET], { type: 'application/javascript' }));
      await state.ctx.audioWorklet.addModule(workletUrl);
      URL.revokeObjectURL(workletUrl);
      const node = new AudioWorkletNode(state.ctx, 'duplex-capture', {
        numberOfInputs: 1,
        numberOfOutputs: 1,
        channelCount: 1,
        channelCountMode: 'explicit',
      });
      const resampler = new LinearResampler(state.ctx.sampleRate, SAMPLE_RATE);
      node.port.onmessage = (event) => {
        if (state.muted || !state.pacer) return;
        const chunk = resampler.process(event.data);
        if (chunk.length) state.pacer.push(chunk);
      };
      const silent = state.ctx.createGain();
      silent.gain.value = 0;
      source.connect(node);
      node.connect(silent);
      silent.connect(state.ctx.destination);
      state.mic = { stream, source, node, silent, resampler };
      setMicState('on', `${(state.ctx.sampleRate / 1000).toFixed(1)} kHz in`);
      log('info', 'microphone capturing');
    } catch (error) {
      const denied = error && (error.name === 'NotAllowedError' || error.name === 'SecurityError');
      setMicState(denied ? 'denied' : 'error', error.message);
      showError(denied
        ? 'Microphone permission denied. Allow access for this origin, then press Retry mic. No audio is being sent.'
        : `Microphone unavailable: ${error.message}`);
      log('error', `microphone: ${error.message}`);
    }
  }

  function stopMic() {
    if (!state.mic) return;
    try { state.mic.node.port.onmessage = null; } catch { /* ignore */ }
    for (const track of state.mic.stream.getTracks()) track.stop();
    try { state.mic.source.disconnect(); } catch { /* ignore */ }
    try { state.mic.node.disconnect(); } catch { /* ignore */ }
    state.mic = null;
    setMicState('off');
  }

  function handleSessionEvent(event) {
    switch (event.type) {
      case 'state':
        setBadge(event.state);
        if (event.state === 'running') {
          state.reconnectAttempts = 0;
          showError('');
          if (state.assembler) state.assembler.clear();
          if (state.pacer) state.pacer.reset();
          log('info', `session ready: ${event.info?.session_id || 'unknown'}`);
        }
        if (event.state === 'draining') log('info', `draining (${event.reason || 'client'})`);
        updateButtons();
        render();
        break;
      case 'frame':
        if (state.playout) state.playout.push(event.samples);
        break;
      case 'text':
        appendTranscript(event.text);
        break;
      case 'stats':
        state.serverStats = event.stats;
        break;
      case 'gap':
        log('warn', `output sequence gap: expected ${event.expected}, got ${event.got}`);
        break;
      case 'log':
        log(event.level, event.message);
        break;
      case 'error':
        showError(`Session error ${event.code}: ${event.message}`);
        log('error', `server error ${event.code}: ${event.message}`);
        endSession();
        break;
      case 'done':
        log('info', `session done (${event.reason})`);
        endSession();
        break;
      case 'disconnected':
        log('warn', `disconnected (${event.reason || 'socket closed'}, code ${event.code ?? 'n/a'})`);
        endSession();
        scheduleReconnect();
        break;
      default:
        break;
    }
  }

  function endSession() {
    stopMic();
    if (state.playout) state.playout.stopAll();
    if (state.session) {
      state.lastSnapshot = state.session.snapshot();
      state.session.socket = null;
      state.session = null;
    }
    updateButtons();
    render();
  }

  function scheduleReconnect() {
    if (!state.autoReconnect || state.reconnectAttempts >= 5) {
      showError('Disconnected. Press Start to open a new session.');
      return;
    }
    state.reconnectAttempts++;
    const delay = 2000;
    setBadge('reconnecting');
    showError(`Connection lost. Reconnecting in ${delay / 1000} s (attempt ${state.reconnectAttempts}/5, new session).`);
    clearTimeout(state.reconnectTimer);
    state.reconnectTimer = setTimeout(() => {
      state.reconnectTimer = null;
      startSession().catch((error) => {
        showError(`Reconnect failed: ${error.message}`);
        setBadge('disconnected');
      });
    }, delay);
  }

  async function startSession() {
    clearTimeout(state.reconnectTimer);
    state.reconnectTimer = null;
    showError('');
    if (state.session) return;
    const ctx = await ensureAudioContext();
    await ctx.resume?.();
    const url = withTicket((qs('wsUrl')?.value || '').trim() || defaultWsUrl(), ticket);
    state.transcript = '';
    if (qs('transcript')) qs('transcript').textContent = '';
    state.serverStats = null;
    state.lastSnapshot = null;
    state.assembler = new FrameAssembler();
    state.pacer = new FramePacer({ assembler: state.assembler });
    state.playout = new Playout({ ctx, targetFrames: state.playoutTarget });
    state.session = new DuplexSession({
      url,
      token,
      tenant,
      onEvent: handleSessionEvent,
    });
    log('info', `connecting to ${url}`);
    setBadge('connecting');
    updateButtons();
    state.session.start();
    startMic();
    if (!state.timer) state.timer = setInterval(tick, 10);
    if (!state.renderTimer) state.renderTimer = setInterval(render, 250);
    render();
  }

  function tick() {
    if (state.session && state.session.state === 'running' && state.pacer && !state.muted) {
      const next = state.pacer.nextFrame(performance.now());
      if (next) state.session.sendFrame(next.frame);
    }
  }

  function render() {
    const snapshot = state.session ? state.session.snapshot() : state.lastSnapshot || null;
    const playout = state.playout;
    const stats = state.serverStats;
    setText('sessionId', snapshot?.sessionId);
    setText('tenant', snapshot?.tenant || tenant);
    setText('sentFrames', snapshot?.sentFrames ?? 0);
    setText('recvFrames', snapshot?.receivedFrames ?? 0);
    setText('outSeq', snapshot?.outSeq ?? 0);
    setText('inSeq', snapshot?.inSeq ?? 0);
    setText('gaps', snapshot?.gaps ?? 0);
    setText('lateFrames', state.pacer?.lateFrames ?? 0);
    setText('localDrops', state.assembler?.dropped ?? 0);
    setText('underruns', playout?.underruns ?? 0);
    setText('underrunMs', fmtMs(playout?.underrunMs ?? 0));
    setText('queuedFrames', playout?.queued ?? 0);
    setText('playoutDrops', playout?.dropped ?? 0);
    setText('playoutP50', fmtMs(playout?.latency.p50));
    setText('playoutP95', fmtMs(playout?.latency.p95));
    setText('playoutMax', fmtMs(playout?.latency.max));
    setText('serverStep', stats ? `${fmtMs(stats.step_ms?.p50)} / ${fmtMs(stats.step_ms?.p95)}` : '—');
    setText('serverE2E', stats ? `${fmtMs(stats.end_to_end_ms?.p50)} / ${fmtMs(stats.end_to_end_ms?.p95)}` : '—');
    setText('uptime', snapshot ? `${(snapshot.uptimeMs / 1000).toFixed(1)} s` : '—');
    setText('bytes', snapshot ? `${fmtBytes(snapshot.bytesSent)} / ${fmtBytes(snapshot.bytesReceived)}` : '—');
    setText('serverQueue', stats ? `${stats.queue_in ?? '—'} / ${stats.queue_out ?? '—'}` : '—');
    setText('serverDrops', stats ? stats.dropped ?? 0 : '—');
  }

  qs('startBtn')?.addEventListener('click', () => {
    state.reconnectAttempts = 0;
    startSession().catch((error) => {
      showError(error.message);
      setBadge('error');
      log('error', error.message);
    });
  });

  qs('stopBtn')?.addEventListener('click', () => {
    if (!state.session) return;
    log('info', 'closing session');
    state.session.close('client_done');
    updateButtons();
  });

  qs('cancelBtn')?.addEventListener('click', () => {
    if (!state.session) return;
    log('info', 'cancelling session');
    state.session.cancel('user_hangup');
    updateButtons();
  });

  qs('retryMicBtn')?.addEventListener('click', () => { startMic(); });

  qs('micMuted')?.addEventListener('change', (event) => {
    state.muted = event.target.checked;
    if (state.pacer) state.pacer.reset();
    if (state.muted && state.assembler) state.assembler.clear();
    log('info', state.muted ? 'microphone muted' : 'microphone unmuted');
  });

  qs('playoutTarget')?.addEventListener('change', (event) => {
    state.playoutTarget = Number(event.target.value) === 2 ? 2 : 1;
    if (state.playout) state.playout.setTargetFrames(state.playoutTarget);
    log('info', `playout buffer set to ${state.playoutTarget} frame(s); takes effect on the next audio`);
  });

  qs('autoReconnect')?.addEventListener('change', (event) => {
    state.autoReconnect = event.target.checked;
  });

  const wsInput = qs('wsUrl');
  if (wsInput) wsInput.value = defaultWsUrl();
  if (qs('autoReconnect')) qs('autoReconnect').checked = true;
  setMicState('off');
  showError('');
  log('info', 'ready. Start a session; dev: run docs/duplex/dev/mock-duplex.mjs');
  render();
  updateButtons();

  window.addEventListener('beforeunload', () => {
    clearInterval(state.timer);
    clearInterval(state.renderTimer);
    if (state.session) state.session.teardown();
    stopMic();
    try { state.ctx?.close(); } catch { /* ignore */ }
  });
}

if (typeof window !== 'undefined' && typeof document !== 'undefined') {
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', startApp, { once: true });
  } else {
    startApp();
  }
}
