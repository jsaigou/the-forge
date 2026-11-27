// duplex-client.js: the protocol client talk-studio runs in the browser (forge-duplex.v1: docs/duplex/protocol.md).
// Taken from go/cmd/forge-duplex/static/app.js (the WS3 client) up to, not including, its page glue. Changes from that file: DuplexSession takes
// `card` and `reference` and sends them in session.start, and identifies itself as talk-studio. Keep it in step with the original when the protocol changes.
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
    card = null,
    reference = null,
    socketFactory = (socketUrl, protocols) => new WebSocket(socketUrl, protocols),
    onEvent = () => {},
  }) {
    this.url = url;
    this.token = token;
    this.tenant = tenant;
    this.text = text;
    this.maxMinutes = maxMinutes;
    this.card = card;
    this.reference = reference;
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
        ...(this.card != null ? { card: this.card } : {}),
        ...(this.reference ? { reference: this.reference } : {}),
        client: { name: 'talk-studio', version: '0.1' },
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

