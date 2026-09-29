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
| tier (capability tier) | | ティア | 階層 | ティア for the named catalog concept ("capability tier" = 能力ティア); 階層 only in generic prose about hierarchy. |
| step-up (auth) | | ステップアップ | | As in "ステップアップ認証". |
| load / unload (a model) | | | 読み込み / 解放 | |
| slot busy | | | スロットが使用中 | |
| offering | | | 提供モデル / オファリング | オファリング only in Settings → Catalog where "Offering" is a named object; 提供モデル in plain prose. |
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

## Style rules

- Use polite plain form (丁寧語, です/ます), not casual (だ/である) — this is operator-facing tooling
  copy, not conversational chat.
- No `。` at the end of short UI labels (buttons, chips, table headers) — only in full sentences
  (help text, descriptions, error messages).
- Numbers, units (GB, tokens, W, ¥, $), ISO dates fed through `Intl`, and code/paths/flags are
  never translated or transliterated.
- When a term isn't in this table yet, add a row here in the same PR that first uses it — don't
  guess inline in a JSON catalog.
