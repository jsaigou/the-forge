// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/store"
)

func TestRotateSpeechKey(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	auth := authz.New(db)
	ctx := context.Background()
	dir := t.TempDir()
	env, studio := path.Join(dir, "tts.env"), path.Join(dir, "studio.env")
	if err := os.WriteFile(studio, []byte("# mine\nVOICE_STUDIO_FORGE_URL=http://127.0.0.1:5000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	expires, rotated, err := rotateSpeechKey(ctx, auth, env, studio, true, time.Now())
	if err != nil || !rotated {
		t.Fatal(err)
	}
	if d := time.Until(expires); d < 364*24*time.Hour || d > authz.MaxSpeechKeyTTL {
		t.Errorf("expires in %s", d)
	}
	first, err := readEnvFile(env)
	if err != nil {
		t.Fatal(err)
	}
	if first["FORGE_TTS_DAEMON_KEY"] == "" || len(first["FORGE_TTS_INTERNAL_TOKEN"]) != 64 || first["FORGE_TTS_DAEMON_KEY_EXPIRES"] != expires.Format(time.DateOnly) {
		t.Fatal("the env file is missing a setting") // never print the values
	}
	for _, f := range []string{env, studio} {
		if st, err := os.Stat(f); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, st.Mode(), err)
		}
	}
	raw, err := os.ReadFile(studio)
	if err != nil || !strings.HasPrefix(string(raw), "# mine\nVOICE_STUDIO_FORGE_URL=http://127.0.0.1:5000\n") || !strings.Contains(string(raw), "VOICE_STUDIO_NODE_TOKEN="+first["FORGE_TTS_INTERNAL_TOKEN"]) {
		t.Fatal("Voice Studio's other settings must stay, with the token added")
	}

	// Rotating revokes the old key, keeps the token and the operator's own lines.
	if err := updateEnvFile(env, map[string]string{"FORGE_TTS_DEFAULT_VOICE": "af_heart"}); err != nil {
		t.Fatal(err)
	}
	if _, rotated, err := rotateSpeechKey(ctx, auth, env, "", false, time.Now()); err != nil || !rotated {
		t.Fatal(err)
	}
	second, err := readEnvFile(env)
	if err != nil {
		t.Fatal(err)
	}
	if second["FORGE_TTS_DAEMON_KEY"] == first["FORGE_TTS_DAEMON_KEY"] || second["FORGE_TTS_INTERNAL_TOKEN"] != first["FORGE_TTS_INTERNAL_TOKEN"] || second["FORGE_TTS_DEFAULT_VOICE"] != "af_heart" {
		t.Fatal("rotation must change the key only")
	}
	if _, err := auth.VerifyBearer(first["FORGE_TTS_DAEMON_KEY"], authz.KindForge); err == nil {
		t.Error("the previous key still works")
	}

	// A file that cannot be written leaves the key in use alone.
	if _, _, err := rotateSpeechKey(ctx, auth, env, path.Join(dir, "no", "such", "dir.env"), false, time.Now()); err == nil {
		t.Fatal("expected a refusal")
	}
	third, err := readEnvFile(env)
	if err != nil || third["FORGE_TTS_DAEMON_KEY"] != second["FORGE_TTS_DAEMON_KEY"] {
		t.Fatal("the key changed although a target could not be written")
	}

	// "If needed" keeps a key that works, still hands the token on, and replaces one that is revoked or about to expire.
	if err := os.Remove(studio); err != nil {
		t.Fatal(err)
	}
	if _, rotated, err := rotateSpeechKey(ctx, auth, env, studio, true, time.Now()); err != nil || rotated {
		t.Fatalf("a good key stays: %v %v", rotated, err)
	}
	if got, err := readEnvFile(studio); err != nil || got["VOICE_STUDIO_NODE_TOKEN"] != second["FORGE_TTS_INTERNAL_TOKEN"] {
		t.Fatal("the token was not handed to Voice Studio")
	}
	if err := updateEnvFile(env, map[string]string{"FORGE_TTS_DAEMON_KEY_EXPIRES": time.Now().Add(10 * 24 * time.Hour).Format(time.DateOnly)}); err != nil {
		t.Fatal(err)
	}
	if _, rotated, err := rotateSpeechKey(ctx, auth, env, "", true, time.Now()); err != nil || !rotated {
		t.Fatalf("a key about to expire is replaced: %v %v", rotated, err)
	}
	if err := updateEnvFile(env, map[string]string{"FORGE_TTS_DAEMON_KEY": third["FORGE_TTS_DAEMON_KEY"]}); err != nil {
		t.Fatal(err)
	}
	if _, rotated, err := rotateSpeechKey(ctx, auth, env, "", true, time.Now()); err != nil || !rotated {
		t.Fatalf("a revoked key is replaced: %v %v", rotated, err)
	}
}

func TestAddStudioLink(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if _, _, err := addStudioLink(ctx, db, "javascript:alert(1)"); err == nil {
		t.Fatal("not a web address")
	}
	if err := db.Settings().Set(ctx, externalToolsKey, []byte(`[{"name":"Talk","url":"https://talk.example/"}]`)); err != nil {
		t.Fatal(err)
	}
	added, at, err := addStudioLink(ctx, db, "http://127.0.0.1:8090")
	if err != nil || !added || at != "http://127.0.0.1:8090" {
		t.Fatalf("add: %v %q %v", added, at, err)
	}
	added, at, err = addStudioLink(ctx, db, "http://elsewhere:1")
	if err != nil || added || at != "http://127.0.0.1:8090" {
		t.Fatalf("an existing one stays: %v %q %v", added, at, err)
	}
	raw, err := db.Settings().Get(ctx, externalToolsKey)
	if err != nil || !strings.Contains(string(raw), "talk.example") || strings.Contains(string(raw), "elsewhere") {
		t.Fatalf("stored: %s %v", raw, err)
	}
}
