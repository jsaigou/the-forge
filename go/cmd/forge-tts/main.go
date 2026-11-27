// Command forge-tts is the pure-Go speech front door. It serves the OpenAI-compatible speech + voices contract (what voice-studio and the a0 router
// call) and keeps the voice registry; it does NOT run or address any engine itself. For every request it asks the Forge daemon where an engine is
// (docs/speech-routing-plan.md): the daemon loads the engine on demand, never evicts it while the request runs (a lease) and says why when it cannot.
// An engine that cannot be resolved is a clear 503, never a slower path.
//
// Env:
//
//	FORGE_TTS_LISTEN              listen addr                       (default 127.0.0.1:8082)
//	FORGE_TTS_REGISTRY            voices registry dir               (default /opt/forge/tts_voices)
//	FORGE_TTS_INTERNAL_TOKEN      shared secret for /v1/voices*     (default "")
//	FORGE_TTS_DEFAULT_VOICE       voice for a request naming none   (default "": such a request is refused)
//	FORGE_TTS_DEFAULT_FAST        default fast voice id             (default af_heart)
//	FORGE_TTS_KOKORO_ENABLED      fast-tier (Kokoro) on/off         (default true; false = Kokoro voices are not served or listed at all)
//	FORGE_TTS_KOKORO_TOKEN        optional Bearer token sent to Kokoro
//	FORGE_TTS_DAEMON_KEY          REQUIRED: a speech-scoped key: forge mint-key -kind forge -role operator -scope speech -bind-ip 127.0.0.1 -ttl 8760h
//	FORGE_TTS_DAEMON_URL          the daemon                        (default http://127.0.0.1:5000)
//	FORGE_TTS_DAEMON_KEY_EXPIRES  when the key expires (RFC3339 or YYYY-MM-DD); only for /health and the startup log, so a lapse is not silent
//	FORGE_TTS_DAEMON_TIMEOUT_S    one resolve call, an engine start included   (default 60)
//	FORGE_TTS_DAEMON_CACHE_S      how long the last known address is used if the daemon does not answer (default 120)
//
// forge-tts REFUSES TO START without a key (the 2026-09 incident was a token variable that was silently unset for three weeks). Put the variables in
// /opt/forge/tts_voices/.internal_token_env (loaded by the unit).
//
// Retired (P5): the qwen-tts command-line path, FORGE_TTS_RESOLVER, FORGE_TTS_INFERENCE, FORGE_TTS_SERVER_CUSTOM/_DESIGN/_BASE, FORGE_TTS_KOKORO_URL,
// FORGE_TTS_DISABLED_MODES and FORGE_TTS_BIN_DIR/_MODELS_DIR/_SIZE/_BACKEND. Which engines exist, where they run and whether they are on is the
// daemon's (Settings -> Voice & Speech). A leftover variable is ignored and named once at startup.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/tts"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		log.Fatalf("forge-tts: %s=%q is not a positive whole number", key, v)
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
		log.Fatalf("forge-tts: %s=%q is not a number of zero or more", key, v)
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "0", "no", "off":
		return false
	}
	return true
}

// retiredVars are the variables forge-tts used to read for engine addresses, the command-line path and the per-mode switch. They no longer do anything.
var retiredVars = []string{
	"FORGE_TTS_RESOLVER", "FORGE_TTS_INFERENCE",
	"FORGE_TTS_SERVER_CUSTOM", "FORGE_TTS_SERVER_DESIGN", "FORGE_TTS_SERVER_BASE",
	"FORGE_TTS_KOKORO_URL", "FORGE_TTS_DISABLED_MODES",
	"FORGE_TTS_BIN_DIR", "FORGE_TTS_MODELS_DIR", "FORGE_TTS_SIZE", "FORGE_TTS_BACKEND",
}

// ignoredRetiredVars returns the retired variables that are set (sorted), so startup can name them in one line.
func ignoredRetiredVars() []string {
	var set []string
	for _, k := range retiredVars {
		if os.Getenv(k) != "" {
			set = append(set, k)
		}
	}
	sort.Strings(set)
	return set
}

// cloneWarmer is the part of the server backend the warm-up needs.
type cloneWarmer interface {
	WarmClones(ctx context.Context, specs []tts.SynthSpec, logf func(format string, args ...any)) (int, error)
}

// warmClones keeps the clone engine holding every cloned voice, registered and primed, so no request pays the seconds that costs: once at start (the daemon
// may still be coming up, so it retries) and then every interval, which also catches an engine that restarted and lost its cache. It never gives up and
// never runs a request's work for it: a request that finds a voice missing registers it itself.
func warmClones(b cloneWarmer, q *tts.QwenTTS, every time.Duration) {
	last := -1
	for {
		specs, err := q.CloneSpecs()
		if err != nil {
			log.Printf("forge-tts: warm-up: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		n, werr := b.WarmClones(ctx, specs, log.Printf)
		cancel()
		switch {
		case werr != nil:
			log.Printf("forge-tts: warm-up: the clone engine is not reachable yet (%v); trying again", werr)
			time.Sleep(15 * time.Second)
			continue
		case n != last:
			log.Printf("forge-tts: warm-up: %d of %d cloned voices are ready on the clone engine", n, len(specs))
			last = n
		}
		time.Sleep(every)
	}
}

func main() {
	reg := tts.NewRegistry(env("FORGE_TTS_REGISTRY", "/opt/forge/tts_voices"))
	if err := reg.Load(); err != nil {
		log.Printf("warn: registry load: %v", err)
	}

	var expires time.Time
	if v := os.Getenv("FORGE_TTS_DAEMON_KEY_EXPIRES"); v != "" {
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if tm, err := time.Parse(layout, v); err == nil {
				expires = tm
				break
			}
		}
		if expires.IsZero() {
			log.Fatalf("forge-tts: FORGE_TTS_DAEMON_KEY_EXPIRES=%q is not RFC3339 or YYYY-MM-DD", v)
		}
	}
	daemonResolver, err := tts.NewDaemonResolver(tts.DaemonResolverConfig{
		BaseURL:    env("FORGE_TTS_DAEMON_URL", "http://127.0.0.1:5000"),
		Key:        os.Getenv("FORGE_TTS_DAEMON_KEY"),
		Timeout:    time.Duration(envInt("FORGE_TTS_DAEMON_TIMEOUT_S", 60)) * time.Second,
		CacheFor:   time.Duration(envInt("FORGE_TTS_DAEMON_CACHE_S", 120)) * time.Second,
		KeyExpires: expires,
		Logf:       log.Printf,
	})
	if err != nil {
		// Refuse to start rather than run with a resolver that can never reach the daemon.
		log.Fatalf("forge-tts: %v", err)
	}
	if !expires.IsZero() {
		log.Printf("forge-tts: the daemon key expires %s (%d days)", expires.Format("2006-01-02"), int(time.Until(expires).Hours()/24))
	}
	if ignored := ignoredRetiredVars(); len(ignored) > 0 {
		log.Printf("forge-tts: ignoring retired settings %s: engines and their on/off state now come from the Forge daemon (Settings -> Voice & Speech); remove them from the unit and env files", strings.Join(ignored, ", "))
	}

	zonos := tts.DefaultZonosOptions()
	zonos.Reference.SpeakingRate = envFloat("FORGE_TTS_ZONOS_RATE", zonos.Reference.SpeakingRate) // bytes of text per second; 0 leaves the pace to the model
	zonos.Embedding.SpeakingRate = envFloat("FORGE_TTS_ZONOS_EMBEDDING_RATE", zonos.Embedding.SpeakingRate)
	zonos.Embedding.SpeakerCFG = envFloat("FORGE_TTS_ZONOS_EMBEDDING_CFG", zonos.Embedding.SpeakerCFG)
	zonos.Reference.SpeakerCFG = envFloat("FORGE_TTS_ZONOS_REFERENCE_CFG", zonos.Reference.SpeakerCFG)
	if v := os.Getenv("FORGE_TTS_ZONOS_EMBEDDING_LANGUAGES"); v != "" { // "en,ja"; "none" turns embedding mode off
		zonos.EmbeddingFamilies = nil
		for _, f := range strings.Split(v, ",") {
			if f = strings.TrimSpace(f); f != "" && f != "none" {
				zonos.EmbeddingFamilies = append(zonos.EmbeddingFamilies, f)
			}
		}
	}
	zonos.SilencePrefillFrames = int(envFloat("FORGE_TTS_ZONOS_SILENCE_FRAMES", float64(zonos.SilencePrefillFrames)))
	zonos.FadeOutMS = int(envFloat("FORGE_TTS_ZONOS_FADE_OUT_MS", float64(zonos.FadeOutMS)))
	zonos.Edge.LeadMS = int(envFloat("FORGE_TTS_ZONOS_LEAD_MS", float64(zonos.Edge.LeadMS)))
	zonos.Edge.TailMS = int(envFloat("FORGE_TTS_ZONOS_TAIL_MS", float64(zonos.Edge.TailMS)))
	zonos.TargetLevelDB = -envFloat("FORGE_TTS_ZONOS_TARGET_DB", -zonos.TargetLevelDB) // given as a positive number of dB below full scale
	zonos.MaxGainDB = envFloat("FORGE_TTS_ZONOS_MAX_GAIN_DB", zonos.MaxGainDB)
	zonos.Logf = log.Printf
	backend := tts.NewServerBackend(daemonResolver).WithZonos(zonos)
	engine := tts.NewQwenTTS(backend, reg)
	go warmClones(backend, engine, time.Duration(envInt("FORGE_TTS_WARM_INTERVAL_S", 30))*time.Second)

	kokoroEnabled := envBool("FORGE_TTS_KOKORO_ENABLED", true)
	defaultVoice := env("FORGE_TTS_DEFAULT_VOICE", "") // none built in: a request that names no voice is refused until the node sets one
	defaultFast := env("FORGE_TTS_DEFAULT_FAST", "af_heart")

	// dualEngine's concrete type is unexported, so kokoro's construction (also unexported) has to happen entirely inside each branch and be assigned to the
	// exported tts.Engine interface here, rather than declared as a typed nil-or-not variable beforehand.
	var dual tts.Engine
	if kokoroEnabled {
		dual = tts.NewDualEngine(engine, tts.NewKokoroBackend(daemonResolver, os.Getenv("FORGE_TTS_KOKORO_TOKEN")), defaultVoice, defaultFast)
	} else {
		dual = tts.NewDualEngine(engine, nil, defaultVoice, defaultFast)
	}

	srv := tts.NewServer(dual, os.Getenv("FORGE_TTS_INTERNAL_TOKEN"), reg.AudioDir())
	srv.SetEngineLister(daemonResolver)      // GET /v1/engines
	srv.SetReadinessReporter(daemonResolver) // GET /v1/readiness

	listen := env("FORGE_TTS_LISTEN", "127.0.0.1:8082")
	fastDesc := "Kokoro via the daemon"
	if !kokoroEnabled {
		fastDesc = "disabled"
	}
	log.Printf("forge-tts listening on %s (engines resolved per request by the daemon at %s; fast=%s)", listen, env("FORGE_TTS_DAEMON_URL", "http://127.0.0.1:5000"), fastDesc)
	// Explicit Server instead of http.ListenAndServe so the listener carries header-read/idle timeouts (slowloris posture; synthesis itself can be slow, so no
	// WriteTimeout).
	server := &http.Server{
		Addr:              listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("forge-tts: %v", err)
	}
}
