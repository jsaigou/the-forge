// talk-studio: talk to a speech-to-speech model, record the conversation, keep voices and cards.
// The page builds its controls from /api/config (the model registry), so a new model is a registry entry, not new code (ADR-0029).
import {
  SAMPLE_RATE, FRAME_SAMPLES, FRAME_MS, LinearResampler, FrameAssembler, FramePacer, Playout, DuplexSession, Percentiles,
} from './duplex-client.js';

const $ = (id) => document.getElementById(id);

// Easter egg: switching from dark to light flashes "MY EYES !!!" for a moment (style.css .me-eyes).
function flashMeEyes() {
  if (document.querySelector('.me-eyes')) return;
  const el = document.createElement('div');
  el.className = 'me-eyes';
  el.setAttribute('aria-hidden', 'true');
  const span = document.createElement('span');
  span.textContent = 'MY EYES !!!';
  el.appendChild(span);
  document.body.appendChild(el);
  // 100 ms at full white, a 100 ms fade, gone at 200 ms. Timers, not a CSS animation: an animation can sit "pending"
  // in a slow or unfocused tab and would leave the overlay up past the 200 ms limit.
  setTimeout(() => el.classList.add('out'), 100);
  setTimeout(() => el.remove(), 200);
}
const LANG_NAMES = { ja: '日本語', en: 'English', zh: '中文', ko: '한국어', es: 'Español', fr: 'Français', de: 'Deutsch', pt: 'Português', ru: 'Русский' };
const langName = (l) => LANG_NAMES[l] || l;
const modelLangs = () => (S.model && S.model.languages && S.model.languages.length ? S.model.languages : ['en']);
function fillLangSelect(sel, placeholder, keep) {
  const have = keep && !modelLangs().includes(keep) ? [keep] : [];
  sel.replaceChildren(el('option', { value: '', text: placeholder }), ...[...modelLangs(), ...have].map((l) => el('option', { value: l, text: langName(l) })));
}
const S = {
  config: null, voices: [], cards: [], model: null,
  vs: [], sampleEdited: false,
  ctx: null, session: null, pacer: null, assembler: null, playout: null, mic: null, rec: null,
  timer: null, uiTimer: null, sentAt: [], lat: new Percentiles(512), micLevel: 0, modelLevel: 0, muted: false,
  startedAt: 0, engineWait: 0, ended: false, stopRequested: false,
};

// ---- small helpers ----------------------------------------------------------------------------------------------------------------------

async function api(method, path, body) {
  const init = { method, headers: {} };
  if (body !== undefined) { init.headers['Content-Type'] = 'application/json'; init.body = JSON.stringify(body); }
  const r = await fetch(path, init);
  if (r.status === 204) return null;
  const data = await r.json().catch(() => ({}));
  if (!r.ok) { const e = new Error(data.error || `HTTP ${r.status}`); e.status = r.status; e.code = data.code; e.data = data; throw e; }
  return data;
}

function banner(msg) { const b = $('banner'); $('bannerText').textContent = msg || ''; b.hidden = !msg; }
function status(text, kind) { const p = $('status'); p.textContent = text; p.dataset.s = kind || ''; }
function remember(k, v) { try { localStorage.setItem('talk-studio.' + k, v); } catch { /* storage may be blocked */ } }
function recall(k) { try { return localStorage.getItem('talk-studio.' + k) || ''; } catch { return ''; } }
// Deleting cannot be undone, so the first click arms the button ("Really delete?") and a second click within 4 s does it. No browser dialog.
function twoStep(btn, run) {
  const label = btn.textContent; let timer = null;
  const disarm = () => { clearTimeout(timer); timer = null; btn.textContent = label; btn.classList.remove('armed'); };
  btn.addEventListener('click', () => {
    if (!timer) { btn.textContent = 'Really delete?'; btn.classList.add('armed'); timer = setTimeout(disarm, 4000); return; }
    disarm(); run();
  });
}
function clock(sec) { const s = Math.max(0, Math.floor(sec)); return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`; }
function ms(v) { return v == null || !Number.isFinite(v) ? '—' : `${v.toFixed(0)} ms`; }
function rms(f) { let a = 0; for (let i = 0; i < f.length; i++) a += f[i] * f[i]; return Math.sqrt(a / f.length); }
function el(tag, props = {}, ...kids) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) { if (k === 'class') n.className = v; else if (k === 'text') n.textContent = v; else n.setAttribute(k, v); }
  for (const k of kids) n.append(k);
  return n;
}

function i16(f32) {
  const o = new Int16Array(f32.length);
  for (let i = 0; i < f32.length; i++) { const v = Math.max(-1, Math.min(1, f32[i])); o[i] = v < 0 ? Math.round(v * 0x8000) : Math.round(v * 0x7fff); }
  return o;
}
function bytesOf(i16arr) { return new Uint8Array(i16arr.buffer, i16arr.byteOffset, i16arr.byteLength); }
function b64(bytes) { let s = ''; for (let i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode(...bytes.subarray(i, i + 0x8000)); return btoa(s); }
function wav(i16arr) {
  const n = i16arr.byteLength, h = new DataView(new ArrayBuffer(44));
  const w = (o, s) => { for (let i = 0; i < s.length; i++) h.setUint8(o + i, s.charCodeAt(i)); };
  w(0, 'RIFF'); h.setUint32(4, 36 + n, true); w(8, 'WAVEfmt '); h.setUint32(16, 16, true); h.setUint16(20, 1, true); h.setUint16(22, 1, true);
  h.setUint32(24, SAMPLE_RATE, true); h.setUint32(28, SAMPLE_RATE * 2, true); h.setUint16(32, 2, true); h.setUint16(34, 16, true);
  w(36, 'data'); h.setUint32(40, n, true);
  return new Blob([h, bytesOf(i16arr)], { type: 'audio/wav' });
}

// ---- audio: context and microphone -------------------------------------------------------------------------------------------------------

const CAPTURE = `class Cap extends AudioWorkletProcessor { process(i) { const c = i[0] && i[0][0]; if (c && c.length) { const a = new Float32Array(c.length); a.set(c); this.port.postMessage(a, [a.buffer]); } return true; } } registerProcessor('ts-capture', Cap);`;

async function ensureCtx() {
  if (!S.ctx) {
    const C = window.AudioContext || window.webkitAudioContext;
    if (!C) throw new Error('Web Audio is not available in this browser');
    try { S.ctx = new C({ sampleRate: SAMPLE_RATE, latencyHint: 'interactive' }); } catch { S.ctx = new C(); }
    const url = URL.createObjectURL(new Blob([CAPTURE], { type: 'application/javascript' }));
    await S.ctx.audioWorklet.addModule(url);
    URL.revokeObjectURL(url);
  }
  await S.ctx.resume?.();
  return S.ctx;
}

// openMic delivers 24 kHz mono float chunks to onChunk. processing=true asks the browser for echo cancellation and noise suppression (a
// conversation); false leaves the voice untouched (recording a reference voice).
async function openMic(processing, onChunk) {
  const ctx = await ensureCtx();
  const stream = await navigator.mediaDevices.getUserMedia({
    audio: { channelCount: 1, echoCancellation: processing, noiseSuppression: processing, autoGainControl: processing },
  });
  const src = ctx.createMediaStreamSource(stream);
  const node = new AudioWorkletNode(ctx, 'ts-capture', { numberOfInputs: 1, numberOfOutputs: 1, channelCount: 1, channelCountMode: 'explicit' });
  const rs = new LinearResampler(ctx.sampleRate, SAMPLE_RATE);
  node.port.onmessage = (e) => { const c = rs.process(e.data); if (c.length) onChunk(c); };
  const mute = ctx.createGain(); mute.gain.value = 0;
  src.connect(node); node.connect(mute); mute.connect(ctx.destination);
  return {
    stop() {
      node.port.onmessage = null;
      for (const t of stream.getTracks()) t.stop();
      try { src.disconnect(); node.disconnect(); mute.disconnect(); } catch { /* already gone */ }
    },
  };
}

// ---- models -----------------------------------------------------------------------------------------------------------------------------

function renderModels() {
  const sel = $('model');
  sel.replaceChildren(...S.config.models.map((m) => el('option', { value: m.id, text: m.label })));
  const last = recall('model');
  if (S.config.models.some((m) => m.id === last)) sel.value = last;
  onModel();
}

function onModel() {
  S.model = S.config.models.find((m) => m.id === $('model').value) || S.config.models[0];
  remember('model', S.model.id);
  $('modelNotes').textContent = S.model.notes || '';
  $('modelRow').hidden = S.config.models.length < 2; // one model needs no choosing
  $('cardBox').hidden = S.model.card === 'none';
  for (const e of document.querySelectorAll('.haruOnly')) e.hidden = S.model.card !== 'haru';
  for (const e of document.querySelectorAll('.refOnly')) e.hidden = !S.model.reference;
  $('textCard').hidden = S.model.card !== 'text';
  $('cLanguage').replaceChildren(...modelLangs().map((l) => el('option', { value: l, text: langName(l) })));
  // The conversation starts in the browser's language when the model has it (the model's own first language otherwise).
  const browser = (navigator.language || '').slice(0, 2);
  if (modelLangs().includes(browser)) $('cLanguage').value = browser;
  fillLangSelect($('voiceNewLang'), 'Language of this clip…');
  renderPresets();
  fillDefaultCard();
  renderStarters();
  onVoice();
}

// Starting points for the scene. Each fills who they are, how you know each other and what is going on; the name and what they call you stay. They are
// plain editable text, not hidden behaviour. A blank card makes the model fall back on a monologue of its own (a recorded Japanese session did exactly that), and
// a generic one ("chatting, just the two of us") was echoed back as a podcast title (docs/duplex/haru-ref-eval-2026-11-24.md), so the page always starts from a
// concrete scene: "evening" is the one that was measured; the others are untested wording.
const STARTERS = {
  en: [
    { key: 'evening', label: 'Evening phone call', name: 'Haru', persona: 'Warm, curious and a little playful. Speaks in short, natural sentences.', relationship: 'friends who talk most evenings', situation: 'An evening phone call. One of them has just got home and is making tea.' },
    { key: 'coffee', label: 'Coffee with a friend', name: 'Haru', persona: 'Relaxed and friendly. Likes to tell small stories and ask about yours.', relationship: 'old friends catching up', situation: 'Two friends at a quiet café on a Saturday morning, catching up over coffee.' },
    { key: 'practice', label: 'Language practice', name: 'Haru', persona: 'Patient and encouraging. Speaks simply and clearly, and gently repeats words back.', relationship: 'a friendly tutor and a learner', situation: 'A relaxed practice chat about everyday things: food, the weather, plans for the weekend.' },
    { key: 'blank', label: 'Blank', name: '', persona: '', relationship: '', situation: '' },
  ],
  ja: [
    { key: 'evening', label: '夜の電話', name: 'ハル', persona: '穏やかで好奇心旺盛。短い自然な文で話す。', relationship: '毎晩のように電話で話す友達', situation: '夜の電話。ひとりは帰宅したばかりで、お茶を入れている。' },
    { key: 'coffee', label: 'カフェで友達と', name: 'ハル', persona: '気さくで親しみやすい。ちょっとした話をするのが好き。', relationship: '久しぶりに会った友達', situation: '土曜の朝、静かなカフェでコーヒーを飲みながら近況を話している。' },
    { key: 'practice', label: '会話の練習', name: 'ハル', persona: '辛抱強く励ましてくれる。やさしい言葉ではっきり話す。', relationship: '親切な先生と生徒', situation: '食べ物や天気、週末の予定など、身近な話題でのんびり練習している。' },
    { key: 'blank', label: '白紙', name: '', persona: '', relationship: '', situation: '' },
  ],
};
const startersFor = (lang) => STARTERS[lang] || STARTERS.en;
const DEFAULT_CARDS = Object.fromEntries(Object.entries(STARTERS).map(([l, list]) => [l, list[0]]));
const isStarterText = () => Object.values(STARTERS).some((list) => list.some((st) => st.persona && $('cPersona').value === st.persona));

function renderStarters() {
  const box = $('starters');
  box.replaceChildren(...startersFor($('cLanguage').value || 'en').map((st) => {
    const b = el('button', { type: 'button', 'aria-pressed': 'false', text: st.label });
    b.addEventListener('click', () => applyStarter(st, b));
    return b;
  }));
  markStarter();
}
function applyStarter(st, btn) {
  $('cPersona').value = st.persona; $('cRelationship').value = st.relationship; $('cSituation').value = st.situation;
  if (st.name && !$('cName').value.trim()) $('cName').value = st.name;
  markStarter(btn);
  refreshSampleText();
}
// The pressed chip shows which starter the scene text still matches; editing the text leaves none pressed.
function markStarter(forced) {
  const list = startersFor($('cLanguage').value || 'en');
  for (const b of $('starters').children) {
    const st = list.find((x) => x.label === b.textContent);
    b.setAttribute('aria-pressed', String(b === forced || !!(st && st.situation && $('cSituation').value === st.situation && $('cPersona').value === st.persona)));
  }
}
function fillDefaultCard() {
  if (S.model.card !== 'haru') return;
  if (Object.keys(readCard()).some((k) => k !== 'language')) return;
  const d = DEFAULT_CARDS[$('cLanguage').value];
  if (d) for (const [id, key] of Object.entries(CARD_FIELDS)) if (d[key]) $(id).value = d[key];
  markStarter();
}

// ---- voices -----------------------------------------------------------------------------------------------------------------------------

async function loadVoices() {
  S.voices = await api('GET', '/api/voices');
  const sel = $('voice');
  const groups = [];
  const vsOpt = (v) => el('option', { value: 'vs:' + v.id, text: `${v.name}${v.language ? ' · ' + v.language : ''}` });
  const mine = S.vs.filter((v) => !v.builtin), builtin = S.vs.filter((v) => v.builtin);
  if (mine.length) groups.push(el('optgroup', { label: 'Your voices in voice-studio (a sample is made for each conversation)' }, ...mine.map(vsOpt)));
  if (builtin.length) groups.push(el('optgroup', { label: 'Built-in voices in voice-studio' }, ...builtin.map(vsOpt)));
  if (S.voices.length) groups.push(el('optgroup', { label: 'Saved recordings (used as they are)' }, ...S.voices.map((v) => el('option', { value: v.id, text: `${v.name} · ${v.seconds.toFixed(0)} s · ${v.language ? langName(v.language) : 'language not set'}` }))));
  sel.replaceChildren(el('option', { value: '', text: S.voices.length || S.vs.length ? '(no reference voice)' : '(none saved yet)' }), ...groups);
  const last = recall('voice');
  if (S.voices.some((v) => v.id === last) || S.vs.some((v) => 'vs:' + v.id === last)) sel.value = last;
  onVoice();
}

// The chosen voice is a saved recording ("rec", used as it is) or a voice-studio voice ("vs", a sample is made for the conversation) or nothing.
function chosenVoice() {
  const val = $('voice').value;
  if (!val) return null;
  if (val.startsWith('vs:')) { const v = S.vs.find((x) => 'vs:' + x.id === val); return v ? { kind: 'vs', v } : null; }
  const v = S.voices.find((x) => x.id === val);
  return v ? { kind: 'rec', v } : null;
}

// voice-studio names a voice's language in words ("English", "Japanese"); the card uses codes.
function vsLangCode(l) { const x = (l || '').toLowerCase(); return x.startsWith('english') ? 'en' : x.startsWith('japanese') ? 'ja' : ''; }

function onVoice() {
  const val = $('voice').value;
  remember('voice', val);
  const cv = chosenVoice();
  const rec = cv && cv.kind === 'rec' ? cv.v : null;
  const p = $('voicePreview');
  p.hidden = !rec; $('voiceDelete').hidden = !rec;
  if (rec) p.src = `/api/voices/${rec.id}/audio`; else p.removeAttribute('src');
  $('voiceLangRow').hidden = !rec || !S.model || !S.model.reference;
  $('sampleBox').hidden = !(cv && cv.kind === 'vs' && S.model && S.model.reference);
  if (rec) {
    fillLangSelect($('voiceLang'), '(choose the language)', rec.language);
    $('voiceLang').value = rec.language || '';
    // The card follows the clip: a card in another language than the clip leaves the model silent.
    if (rec.language && modelLangs().includes(rec.language) && $('cLanguage').value !== rec.language) { $('cLanguage').value = rec.language; onCardLanguage(); }
  }
  if (cv && cv.kind === 'vs') {
    // A voice-studio voice is also tied to a language: the card follows it (the sample is spoken in the card's language).
    const code = vsLangCode(cv.v.language);
    if (code && modelLangs().includes(code) && $('cLanguage').value !== code) { $('cLanguage').value = code; onCardLanguage(); }
  }
  $('voiceHint').textContent = !cv ? 'With no voice the model uses its default (male) voice.'
    : cv.kind === 'vs' ? 'A short sample is made in this voice for each conversation, from the name and what they call you (edit it under More options).'
    : 'This recording is played to the model as it is, and words in it can come back in the conversation. A voice from voice-studio is safer.';
  refreshSampleText();
  checkStart();
}

// The sample text is written from the card: who they are and whom they are talking to. It follows the card until it is edited by hand.
const SAMPLE_TEMPLATES = {
  en: (n, o) => `${o ? `Hey ${o}, it's ${n}.` : `Hi, it's ${n}.`} It's good to talk to you again. So, how has your day been?`,
  ja: (n, o) => `${o ? `ねえ、${o}。${n}だよ。` : `もしもし、${n}です。`}また話せてうれしいな。今日はどんな一日だった？`,
};
function sampleTemplate() {
  const lang = $('cLanguage').value || 'en';
  const make = SAMPLE_TEMPLATES[lang] || SAMPLE_TEMPLATES.en;
  const name = $('cName').value.trim() || (DEFAULT_CARDS[lang] || DEFAULT_CARDS.en).name;
  return make(name, $('cOther').value.trim());
}
function refreshSampleText() {
  const cv = chosenVoice();
  if (!cv || cv.kind !== 'vs') return;
  if (!S.sampleEdited) $('sampleText').value = sampleTemplate();
  const code = vsLangCode(cv.v.language), card = $('cLanguage').value;
  const note = $('sampleLangNote');
  note.hidden = !(code && card && code !== card);
  if (!note.hidden) note.textContent = `This voice was made from ${langName(code)} speech, so ${langName(card)} speech in it will sound less like the original.`;
  $('sampleInfo').textContent = '';
}

// Make (or fetch from the cache) the sample for the chosen voice and the current text. Returns {id, seconds}.
async function makeSample() {
  const cv = chosenVoice();
  const text = $('sampleText').value.trim() || sampleTemplate();
  return api('POST', '/api/sample', { voice: cv.v.id, text });
}

async function previewSample() {
  const btn = $('samplePreview'); btn.disabled = true; $('sampleInfo').textContent = 'Making the sample…'; banner('');
  try {
    const r = await makeSample();
    const a = $('samplePlayer'); a.src = `/api/samples/${r.id}/audio`; a.hidden = false;
    $('sampleInfo').textContent = `${r.seconds.toFixed(1)} s${r.cached ? ' (already made)' : ''}`;
    if (r.cleanup_warning) banner(r.cleanup_warning);
    a.play().catch(() => { /* autoplay may be blocked: the player is there */ });
  } catch (e) { $('sampleInfo').textContent = ''; banner(e.message); } finally { btn.disabled = false; }
}

async function setVoiceLanguage() {
  const id = $('voice').value; if (!id) return;
  try { await api('PATCH', '/api/voices/' + id, { language: $('voiceLang').value }); await loadVoices(); } catch (e) { banner(e.message); }
}

// Start is blocked, with the reason next to the button, while the clip's language is unknown or differs from the card's language.
function checkStart() {
  let why = '';
  const cv = S.model && S.model.reference ? chosenVoice() : null;
  const v = cv && cv.kind === 'rec' ? cv.v : null;
  if (v) {
    if (!v.language) why = 'Say which language this clip is spoken in (under the voice) before starting.';
    else if (S.model.card === 'haru' && $('cLanguage').value && v.language !== $('cLanguage').value) why = `This clip is ${langName(v.language)} but the card is ${langName($('cLanguage').value)}. The model usually stays silent in that case: change the card language or pick another clip.`;
  }
  $('startBlock').textContent = why; $('startBlock').hidden = !why;
  $('start').disabled = !!why;
}

async function fileToWav(file) {
  const ctx = await ensureCtx();
  const audio = await ctx.decodeAudioData(await file.arrayBuffer());
  const n = Math.ceil(audio.duration * SAMPLE_RATE);
  const off = new OfflineAudioContext(1, n, SAMPLE_RATE);
  const mono = off.createBuffer(1, audio.length, audio.sampleRate);
  const out = mono.getChannelData(0);
  for (let c = 0; c < audio.numberOfChannels; c++) { const d = audio.getChannelData(c); for (let i = 0; i < d.length; i++) out[i] += d[i] / audio.numberOfChannels; }
  const src = off.createBufferSource(); src.buffer = mono; src.connect(off.destination); src.start();
  const rendered = (await off.startRendering()).getChannelData(0);
  const max = (S.config.limits.reference_bytes - 44) / 2; // the 44-byte WAV header counts against the limit
  return { pcm: i16(rendered.length > max ? rendered.subarray(0, max) : rendered), trimmed: rendered.length > max };
}

let pendingVoice = null;
function offerVoice(pcm, name, note) {
  pendingVoice = pcm;
  const blob = wav(pcm);
  const a = $('voiceNew'); a.src = URL.createObjectURL(blob); a.hidden = false;
  $('voiceName').value = name || '';
  $('voiceSave').hidden = false;
  $('voiceRecInfo').textContent = note || `${(pcm.length / SAMPLE_RATE).toFixed(1)} s ready. Listen, name it, save it.`;
}
function discardVoice() { pendingVoice = null; $('voiceNewLang').value = ''; $('voiceSave').hidden = true; $('voiceNew').hidden = true; $('voiceRecInfo').textContent = ''; }

async function saveVoice() {
  const name = $('voiceName').value.trim();
  if (!name || !pendingVoice) { banner('Give the voice a name first.'); return; }
  if (!$('voiceNewLang').value) { banner('Say which language is spoken in this clip.'); $('voiceNewLang').focus(); return; }
  const fd = new FormData(); fd.append('name', name); fd.append('language', $('voiceNewLang').value); fd.append('file', wav(pendingVoice), 'voice.wav');
  const r = await fetch('/api/voices', { method: 'POST', body: fd });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) { banner(data.error || `Could not save the voice (HTTP ${r.status}).`); return; }
  banner(''); discardVoice();
  remember('voice', data.id);
  await loadVoices();
}

let voiceRec = null;
async function toggleVoiceRecord() {
  const btn = $('voiceRec');
  if (voiceRec) { await finishVoiceRecord(); return; }
  banner('');
  const chunks = [];
  try { voiceRec = { mic: await openMic(false, (c) => chunks.push(c)), chunks, t0: performance.now() }; } catch (e) { banner('Microphone unavailable: ' + e.message); return; }
  btn.textContent = 'Stop recording';
  const max = S.config.limits.reference_bytes / 2 / SAMPLE_RATE;
  voiceRec.timer = setInterval(() => {
    const s = (performance.now() - voiceRec.t0) / 1000;
    $('voiceRecInfo').textContent = `Recording… ${s.toFixed(0)} s`;
    if (s >= max) finishVoiceRecord();
  }, 250);
}
async function finishVoiceRecord() {
  if (!voiceRec) return;
  const v = voiceRec; voiceRec = null;
  clearInterval(v.timer); v.mic.stop(); $('voiceRec').textContent = 'Record my voice';
  const total = v.chunks.reduce((n, c) => n + c.length, 0);
  const all = new Float32Array(total); let o = 0; for (const c of v.chunks) { all.set(c, o); o += c.length; }
  if (total < SAMPLE_RATE * 3) { $('voiceRecInfo').textContent = 'That was too short. Record at least 3 seconds.'; return; }
  offerVoice(i16(all), '');
}

async function onVoiceFile(e) {
  const f = e.target.files[0]; e.target.value = '';
  if (!f) return;
  banner('');
  try {
    const { pcm, trimmed } = await fileToWav(f);
    offerVoice(pcm, f.name.replace(/\.[^.]+$/, ''), trimmed ? 'Trimmed to the longest voice the service accepts. Listen, name it, save it.' : undefined);
  } catch (err) { banner('That file could not be read as audio: ' + err.message); }
}

// Switching language replaces a card that is still one of the built-in defaults; anything the user typed is left alone.
function onCardLanguage() {
  const was = isStarterText();
  if (was) { $('cName').value = ''; $('cPersona').value = ''; $('cRelationship').value = ''; $('cSituation').value = ''; }
  fillDefaultCard();
  renderStarters();
  refreshSampleText();
  checkStart();
}

// ---- cards ------------------------------------------------------------------------------------------------------------------------------

const CARD_FIELDS = { cLanguage: 'language', cName: 'name', cGender: 'gender', cBackchannel: 'backchannel', cPersona: 'persona', cOther: 'other_name', cRelationship: 'relationship', cSituation: 'situation' };

function readCard() {
  if (S.model.card === 'haru') {
    const o = {};
    for (const [id, key] of Object.entries(CARD_FIELDS)) { const v = $(id).value.trim(); if (v) o[key] = v; }
    return o;
  }
  if (S.model.card === 'text') { const t = $('cText').value.trim(); return t || undefined; }
  return undefined;
}
function writeCard(c) {
  if (S.model.card === 'haru') { for (const [id, key] of Object.entries(CARD_FIELDS)) $(id).value = (c && c[key]) || ''; if (!$('cLanguage').value && $('cLanguage').options.length) $('cLanguage').selectedIndex = 0; }
  else if (S.model.card === 'text') $('cText').value = typeof c === 'string' ? c : '';
}

async function loadCards() { S.cards = await api('GET', '/api/cards'); renderPresets(); }
function renderPresets() {
  const mine = S.cards.filter((c) => c.model === S.model.id);
  $('preset').replaceChildren(el('option', { value: '', text: mine.length ? '(new scenario)' : '(new scenario, none saved yet)' }), ...mine.map((c) => el('option', { value: c.id, text: c.name })));
  const last = recall('card.' + S.model.id);
  if (mine.some((c) => c.id === last)) { $('preset').value = last; onPreset(); } else $('presetDelete').hidden = true;
}
function onPreset() {
  const id = $('preset').value;
  remember('card.' + S.model.id, id);
  $('presetDelete').hidden = !id;
  const c = S.cards.find((x) => x.id === id);
  if (!c) return;
  // A scenario is the character, the scene and the voice together.
  writeCard(c.card);
  renderStarters();
  if (c.voice && [...$('voice').options].some((o) => o.value === c.voice)) $('voice').value = c.voice;
  S.sampleEdited = !!c.sample_text;
  if (c.sample_text) $('sampleText').value = c.sample_text;
  onVoice();
}
function askPresetName() {
  if (readCard() === undefined) { banner('Fill in the scenario first.'); return; }
  const cur = S.cards.find((x) => x.id === $('preset').value);
  $('presetName').value = cur ? cur.name : '';
  $('presetNameRow').hidden = false; $('presetName').focus(); $('presetName').select();
}
function closePresetName() { $('presetNameRow').hidden = true; $('presetSave').focus(); }
async function savePreset() {
  const card = readCard();
  if (card === undefined) { banner('Fill in the scenario first.'); return; }
  const cur = S.cards.find((x) => x.id === $('preset').value);
  const name = $('presetName').value.trim();
  if (!name) { banner('Give the scenario a name first.'); $('presetName').focus(); return; }
  closePresetName();
  try {
    const saved = await api('POST', '/api/cards', { id: cur && cur.name === name ? cur.id : undefined, name, model: S.model.id, card, voice: $('voice').value, sample_text: S.sampleEdited ? $('sampleText').value.trim() : '' });
    remember('card.' + S.model.id, saved.id);
    await loadCards(); banner('');
  } catch (e) { banner(e.message); }
}
async function deletePreset() {
  const id = $('preset').value; if (!id) return;
  try { await api('DELETE', '/api/cards/' + id); remember('card.' + S.model.id, ''); await loadCards(); } catch (e) { banner(e.message); }
}

// ---- recording a conversation -------------------------------------------------------------------------------------------------------------

class Recorder {
  constructor() { this.id = null; this.pending = { user: [], model: [] }; this.sent = { user: 0, model: 0 }; this.events = []; this.t0 = performance.now(); this.timer = null; this.retrying = false; this.busy = false; }
  async open(meta) { const r = await api('POST', '/api/recordings', meta); this.id = r.id; this.timer = setInterval(() => this.flush(), 5000); }
  add(stream, f32) { this.pending[stream].push(bytesOf(i16(f32))); }
  event(e) { this.events.push(JSON.stringify({ t: +((performance.now() - this.t0) / 1000).toFixed(3), ...e })); }
  savedSeconds() { return Math.min(this.sent.user, this.sent.model || this.sent.user) / (SAMPLE_RATE * 2); }
  async flush() {
    if (!this.id || this.busy) return;
    this.busy = true;
    try {
      for (const stream of ['user', 'model']) {
        const parts = this.pending[stream];
        if (!parts.length) continue;
        const all = new Uint8Array(parts.reduce((n, p) => n + p.length, 0)); let o = 0; for (const p of parts) { all.set(p, o); o += p.length; }
        for (let at = 0; at < all.length; at += 1 << 20) {
          const chunk = all.subarray(at, Math.min(all.length, at + (1 << 20)));
          const r = await fetch(`/api/recordings/${this.id}/audio/${stream}?offset=${this.sent[stream]}`, { method: 'PUT', body: chunk });
          if (r.status === 409) {
            const d = await r.json().catch(() => ({}));
            // The server already holds some of this (a reply was lost): carry on from where it really is.
            const have = Number(d.size) || 0, skip = have - this.sent[stream];
            this.sent[stream] = have;
            if (skip > 0 && skip < chunk.length) { const rest = chunk.subarray(skip); const r2 = await fetch(`/api/recordings/${this.id}/audio/${stream}?offset=${have}`, { method: 'PUT', body: rest }); if (!r2.ok) throw new Error('resume failed'); this.sent[stream] = have + rest.length; }
            else if (skip < 0) throw new Error('the server holds less than was sent');
          } else if (!r.ok) throw new Error('HTTP ' + r.status);
          else this.sent[stream] += chunk.length;
        }
        this.pending[stream] = [];
      }
      if (this.events.length) { const body = this.events.join('\n'); const r = await fetch(`/api/recordings/${this.id}/events`, { method: 'POST', body }); if (!r.ok) throw new Error('events HTTP ' + r.status); this.events = []; }
      this.retrying = false;
    } catch { this.retrying = true; } finally { this.busy = false; }
  }
  async close(stats) {
    clearInterval(this.timer);
    for (let i = 0; i < 3 && (this.busy || this.pending.user.length || this.pending.model.length || this.events.length); i++) { await this.flush(); if (this.retrying) await new Promise((r) => setTimeout(r, 500)); }
    // A conversation that never got going (the engine refused, the microphone was missing) has nothing worth keeping.
    if (!this.sent.user && !this.sent.model && !this.pending.user.length && !this.pending.model.length) {
      try { await api('DELETE', `/api/recordings/${this.id}`); } catch { /* an empty entry is harmless */ }
      return;
    }
    try { await api('POST', `/api/recordings/${this.id}/finish`, { stats }); } catch { /* the audio is already saved */ }
  }
}

// ---- the conversation -------------------------------------------------------------------------------------------------------------------

function busy(on) {
  $('start').hidden = on; $('stop').hidden = !on; if (!on) checkStart(); $('mute').disabled = !on;
  document.body.classList.toggle('is-live', on);
  $('setupSummary').hidden = !on;
  if (on) {
    // One line that says what is running: who, in which voice, in what scene.
    const cv = S.model.reference ? chosenVoice() : null;
    const who = (S.model.card === 'haru' && $('cName').value.trim()) || S.model.label;
    const scene = S.model.card === 'haru' ? $('cSituation').value.trim() : '';
    const parts = [who, cv ? `voice: ${cv.v.name}` : '', scene.length > 70 ? scene.slice(0, 70) + '…' : scene].filter(Boolean);
    $('setupSummary').textContent = parts.join(' · ');
  }
  for (const id of ['model', 'voice', 'preset', 'voiceRec', 'presetSave', 'presetDelete', 'voiceDelete', 'samplePreview', 'sampleText']) $(id).disabled = on;
  S.muted = false; $('mute').setAttribute('aria-pressed', 'false'); $('mute').textContent = 'Mute my microphone'; $('muteNote').textContent = '';
  $('live').hidden = !on && !S.ended;
}

async function start() {
  if (!S.config.duplex_configured) { banner('No speech service is configured for this server, so a conversation cannot start.'); return; }
  banner(''); S.ended = false; S.stopRequested = false;
  const gen = S.gen = (S.gen || 0) + 1; // a start that was ended while waiting must not resume after a newer start began
  const stale = () => S.ended || gen !== S.gen;
  $('transcript').replaceChildren(); $('engineMsg').textContent = '';
  S.lat.clear(); S.sentAt = []; S.micLevel = S.modelLevel = 0;
  busy(true); status('Getting ready…', 'working');
  try {
    // The microphone comes first: a denied permission or a missing device fails now, not after a cold engine start of a minute or more.
    // Frames from the microphone wait in the assembler (tick sends nothing until the session is running; the running state clears them).
    S.assembler = new FrameAssembler(); S.pacer = new FramePacer({ assembler: S.assembler });
    try {
      S.mic = await openMic(true, (c) => { if (S.muted) c = new Float32Array(c.length); if (S.pacer) { S.pacer.push(c); tick(); } });
    } catch (e) {
      banner('Microphone unavailable: ' + e.message + '. The conversation cannot start without it. Allow microphone access for this page and try again.');
      if (!stale()) { busy(false); status('Ready'); }
      S.pacer = S.assembler = null;
      return;
    }
    // End was pressed while the browser's permission prompt was open: end() found no microphone to stop, so stop the one that just arrived.
    if (stale()) { S.mic.stop(); S.mic = null; return; }
    const card = readCard();
    let reference = null;
    const cv = S.model.reference ? chosenVoice() : null;
    let vid = '', sampleText = '';
    if (cv && cv.kind === 'rec') {
      vid = cv.v.id;
      const bytes = new Uint8Array(await (await fetch(`/api/voices/${vid}/audio`)).arrayBuffer());
      // The whole WAV file, header included: the engine reads a container, not bare samples.
      reference = { id: 'voice-' + vid, audio_b64: b64(bytes) };
    } else if (cv && cv.kind === 'vs') {
      // The reference is a sample voice-studio speaks for this conversation (the clip is context the model hears as its own speech).
      status('Making the voice sample…', 'working');
      const made = await makeSample();
      if (stale()) return;
      const r = await fetch(`/api/samples/${made.id}/audio`);
      if (!r.ok) throw new Error('could not read the voice sample back');
      reference = { id: 'sample-' + made.id, audio_b64: b64(new Uint8Array(await r.arrayBuffer())) };
      vid = 'vs:' + cv.v.id; sampleText = $('sampleText').value.trim();
      if (made.cleanup_warning) banner(made.cleanup_warning);
    }
    if (stale()) return;
    const sess = await api('POST', '/api/session', { model: S.model.id });
    if (stale()) return;
    const ctx = await ensureCtx();
    S.playout = new Playout({ ctx, targetFrames: 1 });
    S.rec = new Recorder();
    await S.rec.open({ model: S.model.id, voice: vid, card: card === undefined ? undefined : card });
    if (sampleText) S.rec.event({ type: 'sample', voice: vid, text: sampleText });
    const url = new URL(sess.ws_url); url.searchParams.set('ticket', sess.ticket);
    S.session = new DuplexSession({ url: url.toString(), tenant: sess.tenant, text: S.model.text, card, reference, onEvent });
    S.startedAt = performance.now();
    S.session.start();
    S.timer = setInterval(tick, 10);
    S.uiTimer = setInterval(render, 200);
  } catch (e) {
    banner(e.code === 'duplex_unreachable' || e.code === 'duplex_refused' ? `The speech service did not answer: ${e.message}` : e.message);
    end('could not start', true);
  }
}

// Frames are sent the moment the microphone completes them, not from a timer: the microphone callback is not throttled when the tab loses focus,
// a 10 ms timer is (to once a second), and a server that sees no input for a few frame periods ends the session. The timer stays as a fallback for
// frames the callback has not flushed. After the page itself stalls, the microphone callbacks it missed all fire at once; the server's input ring
// holds 8 frames and treats overflow as fatal, so sending is a token bucket (a burst of at most SEND_BURST frames, refilled at the real-time rate)
// and stale frames beyond it are dropped rather than sent.
const SEND_BURST = 4;
function tick() {
  const s = S.session;
  if (!s || s.state !== 'running' || !S.pacer) return;
  const now = performance.now();
  S.tokens = Math.min(SEND_BURST, (S.tokens === undefined ? SEND_BURST : S.tokens) + (now - (S.tokenAt || now)) / FRAME_MS);
  S.tokenAt = now;
  for (let n = S.pacer.assembler.popFrame(); n; n = S.pacer.assembler.popFrame()) {
    if (S.tokens < 1) { S.droppedMic = (S.droppedMic || 0) + 1; continue; }
    S.tokens -= 1;
    S.sentAt.push(now);
    s.sendFrame(n);
    S.micLevel = rms(n);
    S.rec.add('user', n);
  }
}

function onEvent(e) {
  switch (e.type) {
    case 'state': stateChanged(e.state, e); break;
    case 'frame': {
      S.playout.push(e.samples);
      S.rec.add('model', e.samples);
      S.modelLevel = rms(e.samples);
      const t = S.sentAt.shift(); if (t !== undefined) S.lat.add(performance.now() - t);
      break;
    }
    case 'text': {
      S.rec.event({ type: 'text', text: e.text });
      const line = el('span', {}, el('span', { class: 't', text: clock((performance.now() - S.startedAt) / 1000) }), document.createTextNode(e.text));
      const box = $('transcript'); const stick = box.scrollTop + box.clientHeight >= box.scrollHeight - 8;
      box.append(line, ' '); if (stick) box.scrollTop = box.scrollHeight;
      break;
    }
    case 'stats': S.serverStats = e.stats; break;
    case 'error': banner(`The conversation ended: ${e.message || e.code}`); S.rec && S.rec.event({ type: 'error', code: e.code, message: e.message }); end('error', true); break;
    case 'done': end('finished'); break;
    default: break;
  }
}

function stateChanged(state) {
  S.rec && S.rec.event({ type: 'state', state });
  if (state === 'connecting' || state === 'starting') { S.engineWait = performance.now(); status('Starting…', 'working'); }
  else if (state === 'running') { S.engineWait = 0; $('engineMsg').textContent = ''; status(S.muted ? 'Live, muted' : 'Live', 'live'); if (S.assembler) S.assembler.clear(); if (S.pacer) S.pacer.reset(); S.tokens = SEND_BURST; S.tokenAt = performance.now(); S.droppedMic = 0; }
  else if (state === 'draining') status('Finishing…', 'working');
}

async function end(why, isError) {
  if (S.ended) return;
  S.ended = true;
  clearInterval(S.timer); clearInterval(S.uiTimer); S.timer = S.uiTimer = null;
  if (S.mic) { S.mic.stop(); S.mic = null; }
  const snap = S.session ? S.session.snapshot() : null;
  try { if (S.session && S.session.state !== 'closed') S.session.close('client_done'); } catch { /* already closed */ }
  S.session = null;
  if (S.playout) { try { S.playout.stopAll(); } catch { /* ignore */ } }
  status(isError ? 'Ended with a problem' : 'Ended', isError ? 'error' : '');
  $('engineMsg').textContent = '';
  busy(false);
  $('live').hidden = !snap; // a start that failed before any session has nothing to show
  if (S.rec) {
    const r = S.rec; S.rec = null;
    $('saved').textContent = 'saving…';
    await r.close({ why, frames: snap && snap.sentFrames, received: snap && snap.receivedFrames, gaps: snap && snap.gaps, latency_ms: { p50: S.lat.p50, p95: S.lat.p95, max: S.lat.max } });
    $('saved').textContent = r.retrying ? 'some audio could not be saved' : 'saved';
    loadRecordings();
  }
}

function render() {
  const s = S.session; if (!s) return;
  const snap = s.snapshot();
  $('micBar').style.width = Math.min(100, S.micLevel * 400) + '%';
  $('modelBar').style.width = Math.min(100, S.modelLevel * 400) + '%';
  $('lat').textContent = `${ms(S.lat.p50)} typical · ${ms(S.lat.p95)} slow`;
  $('frames').textContent = `${snap.sentFrames} out · ${snap.receivedFrames} in`;
  $('gaps').textContent = `${snap.gaps} / ${S.playout ? S.playout.underruns : 0}`;
  $('uptime').textContent = clock(snap.uptimeMs / 1000);
  if (S.rec) $('saved').textContent = S.rec.retrying ? 'retrying…' : `${clock(S.rec.savedSeconds())} on the server`;
  if (S.engineWait) {
    const w = (performance.now() - S.engineWait) / 1000;
    if (w > 3) $('engineMsg').textContent = `Starting the engine and loading the voice. A cold start takes about a minute (${w.toFixed(0)} s so far).`;
  }
}

// ---- recordings -------------------------------------------------------------------------------------------------------------------------

async function loadRecordings() {
  let list;
  try { list = await api('GET', '/api/recordings'); } catch (e) { $('recordings').textContent = e.message; return; }
  const box = $('recordings');
  if (!list.length) { box.replaceChildren(el('p', { class: 'hint', text: 'No conversations yet.' })); return; }
  box.replaceChildren(...list.map((r) => {
    const when = new Date(r.started).toLocaleString();
    const voice = r.voice && r.voice.startsWith('vs:') ? S.vs.find((v) => v.id === r.voice.slice(3)) : S.voices.find((v) => v.id === r.voice);
    const d = el('details', { class: 'rec' }, el('summary', { text: `${when} · ${r.model} · ${clock(r.seconds)}${voice ? ' · ' + voice.name : ''}${r.finished ? '' : ' · unfinished'}` }));
    let loaded = false;
    d.addEventListener('toggle', async () => {
      if (!d.open || loaded) return; loaded = true;
      const lines = el('div', { class: 'lines', text: 'Loading…' });
      const mk = (label, stream) => el('div', {}, el('span', { class: 'k', text: label }), (() => { const a = el('audio', { controls: '', preload: 'none', src: `/api/recordings/${r.id}/audio/${stream}` }); return a; })(),
        el('a', { href: `/api/recordings/${r.id}/audio/${stream}`, download: `${r.id}-${stream}.wav`, text: ' download' }));
      const del = el('button', { type: 'button', class: 'small danger', text: 'Delete this recording' });
      twoStep(del, async () => { try { await api('DELETE', '/api/recordings/' + r.id); loadRecordings(); } catch (e) { banner(e.message); } });
      d.append(mk('You', 'user'), mk('Model', 'model'), el('h3', { text: 'What the model said' }), lines, el('div', { class: 'row' }, del));
      try {
        const full = await api('GET', '/api/recordings/' + r.id);
        const texts = full.events.filter((e) => e.type === 'text');
        lines.textContent = '';
        for (const e of texts) { lines.append(el('span', { class: 't', text: clock(e.t) }), document.createTextNode(e.text + ' ')); }
        if (!texts.length) lines.textContent = '(no text)';
        const err = full.events.find((e) => e.type === 'error');
        if (err) lines.append(el('p', { class: 'hint', text: `Ended with an error: ${err.message || err.code}` }));
      } catch (e) { lines.textContent = e.message; }
    });
    return d;
  }));
}

// ---- boot -------------------------------------------------------------------------------------------------------------------------------

async function boot() {
  $('model').addEventListener('change', onModel);
  $('voice').addEventListener('change', onVoice);
  $('voiceFile').addEventListener('change', onVoiceFile);
  $('voiceRec').addEventListener('click', toggleVoiceRecord);
  $('voiceSaveBtn').addEventListener('click', saveVoice);
  $('voiceDiscard').addEventListener('click', discardVoice);
  twoStep($('voiceDelete'), async () => {
    const v = S.voices.find((x) => x.id === $('voice').value); if (!v) return;
    try { await api('DELETE', '/api/voices/' + v.id); remember('voice', ''); await loadVoices(); } catch (e) { banner(e.message); }
  });
  $('bannerClose').addEventListener('click', () => banner(''));
  $('themeBtn').addEventListener('click', () => {
    const cur = document.documentElement.dataset.theme || (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');
    const next = cur === 'light' ? 'dark' : 'light';
    if (next === 'light') flashMeEyes();
    document.documentElement.dataset.theme = next;
    remember('theme', next);
  });
  $('preset').addEventListener('change', onPreset);
  $('presetSave').addEventListener('click', askPresetName);
  $('presetNameOk').addEventListener('click', savePreset);
  $('presetNameCancel').addEventListener('click', closePresetName);
  $('presetName').addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); savePreset(); } else if (e.key === 'Escape') closePresetName(); });
  twoStep($('presetDelete'), deletePreset);
  $('start').addEventListener('click', start);
  $('cLanguage').addEventListener('change', onCardLanguage);
  $('voiceLang').addEventListener('change', setVoiceLanguage);
  $('samplePreview').addEventListener('click', previewSample);
  $('sampleText').addEventListener('input', () => { S.sampleEdited = true; });
  for (const id of ['cName', 'cOther', 'cRelationship']) $(id).addEventListener('input', refreshSampleText);
  for (const id of ['cSituation', 'cPersona']) $(id).addEventListener('input', () => markStarter());
  $('stop').addEventListener('click', () => { S.stopRequested = true; end('stopped by you'); });
  $('mute').addEventListener('click', () => {
    S.muted = !S.muted;
    $('mute').setAttribute('aria-pressed', String(S.muted));
    $('mute').textContent = S.muted ? 'Unmute my microphone' : 'Mute my microphone';
    $('muteNote').textContent = S.muted ? 'Muted: the model cannot hear you.' : '';
    status(S.muted ? 'Live, muted' : 'Live', 'live');
  });
  window.addEventListener('pagehide', () => { if (S.rec) S.rec.flush(); });
  try {
    S.config = await api('GET', '/api/config');
    if (S.config.voice_studio_url) { $('vsLink').href = S.config.voice_studio_url; $('vsLink').hidden = false; }
    if (!S.config.duplex_configured) banner('No speech service is configured for this server (TALK_STUDIO_DUPLEX_URL). You can manage voices and recordings, but not start a conversation.');
    S.voices = await api('GET', '/api/voices'); S.cards = await api('GET', '/api/cards');
    if (S.config.voice_studio_configured) { try { S.vs = await api('GET', '/api/vs/voices'); } catch (e) { S.vs = []; banner('voice-studio voices are not listed: ' + e.message); } }
    renderModels(); await loadVoices(); await loadCards(); await loadRecordings();
    status('idle');
  } catch (e) { banner('talk-studio could not load: ' + e.message); status('error', 'error'); }
}
boot();
