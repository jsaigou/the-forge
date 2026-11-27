# Japanese glossary

Reference for every Japanese catalog in `web/src/locales/ja/`. Every translated string is checked
against this list before it ships, so the same English term always becomes the same Japanese term.
Add to this file first when a new domain term needs translating — don't invent a rendering
ad hoc inside a catalog file.

Column 3 is the katakana transliteration where one is idiomatic in Japanese technical writing;
column 4 is a native Japanese term where one reads more naturally. Pick one per row — don't mix
both for the same English term across catalogs.

| English | Stays as-is | Katakana | Native term | Notes |
|---|---|---|---|---|
| slot (A1–A4) | ✓ (identifier) | | | Slot IDs are identifiers, never translated. |
| config | | コンフィグ | 設定 | Use 設定 in prose ("この設定"); コンフィグ only where "config" names a catalog object, to avoid colliding with 設定 = Settings (the page). |
| model | | | モデル | |
| catalog | | | カタログ | |
| a0 (router) | ✓ (identifier) | | | Product name, not translated. |
| smith | ✓ (identifier) | | | Product name, not translated. |
| compressor | | | 圧縮 / コンプレッサー | 圧縮 for the action/concept, コンプレッサー when naming the proxy component. |
| GTT | ✓ (identifier) | | | Kernel/hardware term, left as-is with a short gloss on first use per page if space allows. |
| KV cache | ✓ (identifier) | | | Left as-is; widely used untranslated in Japanese ML writing. |
| context (window) | | コンテキスト | | |
| prefill | | プリフィル | | |
| decode | | デコード | | |
| output modality | | モダリティ | 出力モダリティ | ADR-0021: what a model can *produce* (text / speech / decisions), the counterpart of the input modalities (vision, audio) the catalog already had. Label the section 出力モダリティ and the detail row 出力. |
| speech (output) | | | 音声 | The `audio` output value: the model speaks. Same word as the *input* audio toggle (音声), disambiguated by the surrounding 出力モダリティ label. |
| decisions (output) | | | 判定 | The `decisions` output value: a native decision model (d1) answering typed questions (choice / score / yes-no) with probabilities and no generated text, served on `/v1/systemone`. Not 決定 (reads as "determination/resolution"). |
| tier (capability tier) | | ティア | 階層 | ティア for the named catalog concept ("capability tier" = 能力ティア); 階層 only in generic prose about hierarchy. |
| step-up (auth) | | ステップアップ | | As in "ステップアップ認証". |
| load / unload (a model) | | | 読み込み / 解放 | |
| dismiss (a notice) | | | 閉じる | Same word the catalogs already use for "close" (common.json); a dismiss button clears a remembered failure, it does not delete anything. |
| slot busy | | | スロットが使用中 | |
| offering | | | 提供モデル / オファリング | オファリング only in Settings → Catalog where "Offering" is a named object; 提供モデル in plain prose. |
| Model catalog | | | モデルカタログ | The Settings section (design feedback 2026-11-27; was "Catalog" / カタログ). |
| Profile tonight | | | 今夜プロファイル | The overnight profiling run; "outdated" measurement = 要再測定, "never profiled" = 未測定. |
| benchmark | | ベンチマーク | | |
| profiling | | プロファイリング | | |
| scheduler | | スケジューラー | | |
| reservation | | | 予約 | |
| virtual model | | | 仮想モデル | |
| Headroom / compressor savings | | | 圧縮による削減 | |
| onboarding tour | | | 案内ツアー | |
| settings | | | 設定 | Also the page name; disambiguate from "config" (see above). |
| dashboard | | ダッシュボード | | |
| console | | コンソール | | Also the page name. |
| restart required | | | 再起動が必要 | |
| capability tiers / virtual models UI copy | | | (see "tier" row) | |
| autonomy (smith standing autonomy) | | | 自律実行 | "autonomous" as adjective → 自律的に. |
| retention (smith findings/cache pruning) | | | 保持 | As in 保持期間 (retention period). |
| self-review (smith sweep) | | | 自己点検 | |
| finding (smith deterministic check result) | | | 検出結果 | |
| reasoning effort | | | 推論の強さ | Matches this app's own concept name, not a generic MT phrase. |
| alias (model alias) | | | エイリアス | |
| GGUF | ✓ (identifier) | | | File-format name, never translated. |
| watchlist (build-refresh keyword) | | | 監視リスト | |
| speculative decoding | | 投機的デコーディング | | Includes MTP/draft-model strategies. |
| draft model | | ドラフトモデル | | The smaller model used by speculative decoding. |
| batch size (prompt/physical) | | バッチサイズ | | "logical"→論理, "physical"→物理 as a qualifier. |
| flash attention | | フラッシュアテンション | | Named technique, not translated further. |
| quantize / quantization (KV cache, weights) | | 量子化 | | |
| data residency (provider) | | | データ所在地 | As in "データ所在地: 日本". |
| investigation (smith) | | | 調査 | |
| procedure (smith automated remediation) | | プロシージャ | | |
| checkpoint (procedure pause) | | チェックポイント | | |
| precondition | | | 前提条件 | |
| unattended completion | | | 無人完了 | |
| post-verify | | | 事後検証 | |
| downtime (procedure estimate) | | ダウンタイム | | |
| confidence (finding) | | | 確信度 | |
| sweep (Ask-the-smith quick/deep checks run) | | スイープ | | Distinct from "self-review (smith sweep)" above (自己点検, an internal maintenance concept) — this is the operator-triggered check run in the chat UI. |
| conversation (smith chat) | | | 会話 | |
| suggestion (pending smith action tray) | | | 提案 | |
| check (diagnostic, smith) | | | チェック | Matches `diagnostics.investigations.run_more_checks`'s existing 追加のチェックを実行. |
| knowledge base / KB reference | | | ナレッジベース参照 | |
| handoff (smith self-eviction) | | ハンドオフ | | Matches `fields.smith.handoff_offerings`'s existing ハンドオフ/自己退避 usage. |
| runbook (handoff/self-review) | | ランブック | | |
| brain (smith's reasoning-tier model) | | ブレイン | | |
| evidence (chat/tool-call detail) | | | 根拠 | |
| source (web-research citation) | | | 情報源 | |
| externally blocked work | | | ブロック中の作業 | As in "外部要因でブロック中の作業". |
| retired (compressor) | | | 廃止 / 廃止されました | Permanent end state of the compressor (ADR-0017), not "disabled"/無効 — nothing re-enables it from the UI. |
| append-only (context) | | | 追記のみ | As in "コンテキストは追記のみ". |
| passthrough / bypass | | | バイパス | Already used for the global passthrough toggle; kept as バイパス. |
| context creation (ledger) | | | コンテキストの生成量 | Dashboard panel on how much context each request adds; "生成" = created, never "圧縮". |
| consumer (a0 client) | | コンシューマー | | The API-key holder calling a0 (OpenCode, LibreChat, ...). |
| cache-bust suspect | | | キャッシュ破壊の疑い | A client that rewrote earlier messages instead of appending. |
| cache reuse | | | キャッシュ再利用 | Share of the prompt prefix served from the KV cache. |
| tool schema | | ツールスキーマ | | The `tools[]` definitions sent on every request. |
| tool output / result | | | ツール出力 / ツールの結果 | "大きな結果" = a single tool output over 8K/24K characters. |

## Style rules

- Use polite plain form (丁寧語, です/ます), not casual (だ/である) — this is operator-facing tooling
  copy, not conversational chat.
- No `。` at the end of short UI labels (buttons, chips, table headers) — only in full sentences
  (help text, descriptions, error messages).
- Numbers, units (GB, tokens, W, ¥, $), ISO dates fed through `Intl`, and code/paths/flags are
  never translated or transliterated.
- When a term isn't in this table yet, add a row here in the same PR that first uses it — don't
  guess inline in a JSON catalog.
| build (llama-server binary) | | | ビルド | The executable a config runs on; not a "build" in the CI sense. |
| retire (a build) | | | 廃止 | Same word as the compressor's 廃止: a permanent end state, refused while a config still uses the build. |
| artifact (model file) | | | アーティファクト | One weight, mmproj or tokenizer file a variant ships as. |
| engine (what runs in a dedicated slot) | | | エンジン | A small, purpose-built model or service kept loaded in a dedicated slot (ADR-0022), e.g. Kokoro, Qwen3-ASR, d1-2b. Replaced the earlier user-facing word "specialist", which read as a person or a separate thing; "specialist" survives only in code names (`specialist` package, `/api/v1/specialists`, `specialist_*` tables and error codes). A voice engine (Kokoro, Base / clone) is an engine too; it runs in a dedicated slot or as its own process. |
| runtime (llama.cpp / vLLM / Radiance) | | | ランタイム | The inference software that serves a config, chosen by the engine row's kind (`llamacpp`, `vllm`, `radiance`), not by the build's backend (rocm / vulkan, the GPU path, which stays バックエンド). Operator-approved word because "engine" is already used for dedicated-slot engines above. Product names are never translated; an unknown kind shows its raw kind. |
| dedicated slot | | | 専用スロット | One of the small slots S1, S2… kept apart from the LLM slots A1–A4, which stay スロット. Do not translate the slot ids. |
| always on / on demand | | | 常時オン / オンデマンド | Two of the three values of an engine's Behavior. |
| Behavior (Always on / On demand / Disabled) | | | 動作設定（常時オン / オンデマンド / 無効） | The one setting every engine in a dedicated slot has, identical on the Engines tab, the Models page ("Dedicated slot: …") and Voice & Speech. Disabled = unloaded, slot freed, still registered. |
| speech engine | | | 音声エンジン | An engine that makes speech (Kokoro, Qwen3-TTS base / voicedesign / customvoice). One row each on Voice & Speech. |
| kind of voice (Kokoro / Custom / Designed / Cloned voices) | | | Kokoro の音声 / カスタム音声 / デザインした音声 / クローンした音声 | The four kinds a speech engine can make: Kokoro's own voices, a custom voice's built-in speakers, a voice made by Describe (designed), a voice made by Clone (cloned). Each kind has one default engine. |
| default engine (for a kind of voice) | | | 既定エンジン | The engine that makes voices of a kind that do not name an engine. A voice that names an engine always uses it and is never moved to another. |
| default engine (for a function) | | | 既定エンジン | The engine that answers a function (embeddings, speech, transcription, alignment, decisions, speech-to-speech) when a request names none. It must be an Always on engine (ADR-0024), so the UI refuses another Behavior for it while it is the default. Same word as the per-kind voice default above; say which function or kind. |
| engine missing / engine off (voice status) | | | エンジンが見つかりません / エンジンがオフ | A voice cannot be played because the engine it names does not exist or is Disabled. |
| set up (a speech engine) | | | 設定（する） | Registering an engine from files already on the server (Voice & Speech, "What works"). Never "install": nothing is downloaded or built. "Not installed" (未インストール) means the files are not there. The code word "recipe" is not shown to users. |
| preset voices / voice cloning / voice design | | | プリセット音声 / 音声クローン / 音声デザイン | The three capabilities of the speech readiness report: built-in voices, a voice from a recording, a voice from a description. |
| embeddings (function) | | | 埋め込み | |
| speech (function, TTS) | | | 音声合成 | |
| transcription (function, STT) | | | 文字起こし | |
| alignment (function) | | アライメント | | Forced alignment / timing. |
| speech-to-speech (function, duplex) | | | 音声対話 | A model that hears and speaks at the same time over a continuous audio session (ADR-0029). The function is named `duplex` in code and on the wire. Not the chained speech-to-text, model, text-to-speech loop. |
| external tool (link-only Console entry) | | | 外部ツール | A tool that runs somewhere else (voice-studio, talk-studio on Core) shown on the Console as a chip with a ↗ link. The Console does not check it or control it, so it has no status dot and no start or stop. Set in Settings → General → External tools (`infra.external_tools`). |
| decisions (function) | | | 判定 | Typed decisions from a decision model. |
| passkey (WebAuthn) | | | パスキー | A registered authenticator (platform security key or roaming key). |
| recovery code | | | リカバリーコード | One-time codes shown once at enrollment; the last-resort factor. |
| authenticator app (TOTP) | | | 認証アプリ | The TOTP factor, e.g. "TOTP · 認証アプリ". |
| identity link / unlink | | | ID連携 / 連携解除 | Binding/unbinding an external identity (GitHub, …) to the account. |
| revoke (key) | | | 失効 | Revoking an API key or passkey; not 取り消し (reserved for UI cancel). |
| forward auth | | | フォワード認証 | The embedded forward-auth endpoint/headers mode (security section). |
| credential | | | 資格情報 | Generic: a key/token/passkey being managed. |
| factor (auth) | | | 認証要素 | password / totp / passkey in the access-policy matrix. |
| principal | | | プリンシパル | The authenticated entity a policy applies to. |
| concurrency (of requests) | | | 同時実行数 | Dashboard card: how many a0-proxied requests a model served at the same moment. |
| in flight (request) | | | 処理中 | A request that has started and not yet finished; "in flight now" = 現在の処理中. |
| overlap (fraction) | | | 重なり率 | Share of requests that ran together with at least one other. |
| busy time | | | 稼働時間 | Time with at least one request in flight. |
| aborted (request outcome) | | | 中断 | The client went away before the answer finished; ok = 成功, error = エラー. |
| launch generation | | | 起動世代 | Counter of how many times the slot's runtime was launched; a new generation means a fresh process. |
| model budget (memory) | | | モデル予算 | The part of host RAM left for LLM slots after the declared reserve: host total minus the OS floor, minus the declared peak of every always-on engine, minus the growth margin (ADR-0027). Free memory for a new load is the smallest of this, MemAvailable minus the host floor, and the GTT ceiling. |
| reserved (memory region) | | | 予約 | Host RAM set aside up front (OS, always-on engines, growth margin) so a model load can never take it. Shown hatched grey on the meter. |
| held back (memory) | | | 確保外 | Free in the model budget but not claimable now because a tighter bound (the host floor or the GTT ceiling) is lower. The bound is named. |
| recommended (config) | | | 推奨 | A chip on a config card: the config runs on the Radiance runtime and the operator has verified it. Derived from runtime and status; it is information, not a ranking or a speed claim. |
