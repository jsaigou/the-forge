# Kokoro's preset voices over HTTP for the Forge's engine pool (docs/speech-setup-plan.md section 5.5). `forge speech install kokoro` writes this file
# next to its virtual environment and its models/ folder. It listens on 127.0.0.1:8880; KOKORO_PORT moves it for the installer's trial run only.
import io
import logging
import os

import numpy as np
import soundfile as sf
from fastapi import FastAPI, Request
from fastapi.responses import Response
from kokoro_onnx import Kokoro, SAMPLE_RATE

HERE = os.path.dirname(os.path.abspath(__file__))
MODEL = os.path.join(HERE, "models", "kokoro-v1.0.onnx")
VOICES = os.path.join(HERE, "models", "voices-v1.0.bin")
VOICE = "af_heart"

app = FastAPI(title="kokoro-fastapi", version="0.1.0")
_k = None

def kokoro():
    global _k
    if _k is None:
        _k = Kokoro(MODEL, VOICES)
    return _k

_LANG_ALIASES = {
    "en": "en", "eng": "en", "english": "en", "en-us": "en", "en-gb": "en",
    "es": "es", "spa": "es", "spanish": "es",
    "fr": "fr", "fra": "fr", "fre": "fr", "french": "fr",
    "hi": "hi", "hin": "hi", "hindi": "hi",
    "it": "it", "ita": "it", "italian": "it",
    "ja": "ja", "jpn": "ja", "japanese": "ja",
    "pt": "pt", "por": "pt", "portuguese": "pt", "pt-br": "pt",
    "zh": "zh", "zho": "zh", "chi": "zh", "chinese": "zh", "cmn": "zh",
}

_ESPEAK = {
    "en": "en-us", "es": "es", "fr": "fr-fr", "hi": "hi",
    "it": "it", "pt": "pt-br",
}

def lang_key(raw):
    key = str(raw or "").strip().lower()
    return _LANG_ALIASES.get(key, "en")

def synthesize(k, text, voice, speed, key):
    if key == "ja":
        try:
            from misaki import ja as misaki_ja
            g2p = misaki_ja.JAG2P(version="cutlet")
            phonemes, _ = g2p(text)
            return k.create(phonemes, voice=voice, speed=speed, is_phonemes=True)
        except Exception:
            # The fallback reads Japanese badly: say so, do not hide it.
            logging.getLogger("uvicorn.error").exception("the Japanese front end failed; using espeak")
            return k.create(text, voice=voice, speed=speed, lang="ja")
    if key == "zh":
        return k.create(text, voice=voice, speed=speed, lang="zh")
    return k.create(text, voice=voice, speed=speed, lang=_ESPEAK.get(key, "en-us"))

@app.get("/health")
def health():
    return {"status": "ok", "model": "kokoro-v1.0", "engine": "cpu"}

@app.get("/v1/audio/voices")
def list_voices():
    k = kokoro()
    voices = k.get_voices()
    return {
        "object": "list",
        "data": [
            {"id": v, "name": v, "tier": "fast", "engine": "kokoro"}
            for v in voices
        ],
    }

@app.post("/v1/audio/speech")
async def speech(req: Request):
    body = await req.json()
    voice = body.get("voice", VOICE)
    text = body.get("input", "")
    speed = float(body.get("speed", 1.0))
    key = lang_key(body.get("language"))
    k = kokoro()
    audio, sr = synthesize(k, text, voice, speed, key)
    buf = io.BytesIO()
    sf.write(buf, audio, sr, format="WAV")
    buf.seek(0)
    return Response(content=buf.read(), media_type="audio/wav")

if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="127.0.0.1", port=int(os.environ.get("KOKORO_PORT", "8880")), log_level="info")
