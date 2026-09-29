// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"strings"
	"sync"
)

// kb_ja_terms.go — Phase 3 of the multilanguage plan
// (docs/adr/0016-localization.md): a ja→en domain-term map so a Japanese
// question against the (deliberately English-only, see reasoning.go's
// languageDirective doc comment and the plan's §"KB stays English-source")
// KB corpus actually retrieves something. tokenizeKBQuery (kb.go) already
// keeps embedded Latin/ASCII substrings — a Japanese question that quotes
// "GTT" or a slot ID verbatim already tokenizes those correctly — this map
// only covers the domain nouns/verbs that would otherwise be pure CJK and
// invisible to kbTokenRe.
//
// Deliberately NOT exhaustive: every entry here is a term that already has
// a settled rendering in web/src/locales/GLOSSARY.md, reversed back to its
// English source term(s). A query word not in this map simply contributes
// no extra English tokens — degrading to "the embedded Latin terms in the
// query still work," never a hard failure. Add a row here (and to the
// glossary, if it's a new concept) rather than guessing a translation
// inline.
var kbJaTermMap = map[string][]string{
	// memory / GTT / disk
	"メモリ":     {"memory", "gtt"},
	"ジーティーティー": {"gtt"},
	"ディスク":    {"disk"},
	"容量":      {"capacity"},

	// context / KV cache / prefill / decode
	"コンテキスト": {"context"},
	"文脈":     {"context"},
	"プリフィル":  {"prefill"},
	"デコード":   {"decode"},
	"kvキャッシュ": {"kv", "cache"},

	// load / unload / restart
	"読み込み":  {"load"},
	"読み込む":  {"load"},
	"解放":    {"unload"},
	"アンロード": {"unload"},
	"再起動":   {"restart"},

	// slot / config / model / catalog
	"スロット":     {"slot"},
	"使用中":      {"busy"},
	"コンフィグ":    {"config"},
	"設定":       {"config", "settings"},
	"モデル":      {"model"},
	"カタログ":     {"catalog"},
	"提供モデル":    {"offering"},
	"オファリング":   {"offering"},
	"仮想モデル":    {"virtual", "model"},
	"エイリアス":    {"alias"},
	"ティア":      {"tier"},
	"能力ティア":    {"capability", "tier"},
	"階層":       {"tier"},
	"ベンチマーク":   {"benchmark"},
	"プロファイリング": {"profile", "profiling"},

	// compressor / headroom / a0
	"圧縮":      {"compress", "compressor"},
	"コンプレッサー": {"compressor"},
	"削減":      {"savings", "saved"},

	// scheduler / reservation
	"スケジューラー": {"scheduler"},
	"予約":      {"reservation"},

	// brain / smith
	"ブレイン": {"brain"},
	"推論":   {"reasoning"},
	"自律実行": {"autonomy"},
	"自律的":  {"autonomous"},

	// auth / step-up
	"ステップアップ": {"step-up", "stepup"},
	"認証":      {"auth"},
	"パスキー":    {"passkey", "webauthn"},

	// GPU / hardware / temperature
	"温度": {"temperature"},
	"故障": {"failure", "hang"},
	"ハング": {"hang"},

	// findings / checks / diagnostics
	"検出結果": {"finding"},
	"チェック":  {"check"},
	"確信度":   {"confidence"},
	"根拠":    {"evidence"},
	"情報源":   {"source"},

	// investigation / procedure / handoff
	"調査":      {"investigation"},
	"プロシージャ":  {"procedure"},
	"チェックポイント": {"checkpoint"},
	"前提条件":    {"precondition"},
	"無人完了":    {"unattended", "completion"},
	"事後検証":    {"post-verify"},
	"ダウンタイム":  {"downtime"},
	"ハンドオフ":   {"handoff"},
	"ランブック":   {"runbook"},
	"提案":      {"suggestion", "action"},

	// speculative decoding / quantization
	"投機的デコーディング": {"speculative", "decode"},
	"ドラフトモデル":     {"draft", "model"},
	"量子化":         {"quantize", "quantization"},
	"バッチサイズ":      {"batch"},
	"フラッシュアテンション": {"flash", "attention"},

	// misc operational vocabulary
	"保持":    {"retention"},
	"自己点検":  {"self-review"},
	"会話":    {"conversation"},
	"ナレッジベース": {"kb", "knowledge"},
	"データ所在地": {"data", "residency"},
	"外部要因":  {"blocked"},
	"再起動が必要": {"restart", "required"},
}

// expandJapaneseTerms scans q for known ja domain terms (longest-substring
// match first, so multi-character compounds like "自律実行" match before a
// shorter overlapping entry could) and returns the English terms they map
// to, deduplicated. Called unconditionally by tokenizeKBQuery — a no-op
// (returns nil) for a query with no recognized Japanese vocabulary.
func expandJapaneseTerms(q string) []string {
	kbJaTermsOnce.Do(initKBJaTermsBySortedLen)
	var out []string
	seen := map[string]bool{}
	remaining := q
	for _, term := range kbJaTermsBySortedLen {
		if !strings.Contains(remaining, term) {
			continue
		}
		for _, en := range kbJaTermMap[term] {
			if !seen[en] {
				seen[en] = true
				out = append(out, en)
			}
		}
	}
	return out
}

// kbJaTermsBySortedLen is kbJaTermMap's keys sorted longest-first, computed
// once on first use (guarded by kbJaTermsOnce — expandJapaneseTerms can run
// concurrently across chat turns) — matching order matters (a longer
// compound term should contribute its own mapping even if a shorter
// substring of it is also a key), but the map itself never changes at
// runtime.
var (
	kbJaTermsBySortedLen []string
	kbJaTermsOnce        sync.Once
)

func initKBJaTermsBySortedLen() {
	terms := make([]string, 0, len(kbJaTermMap))
	for t := range kbJaTermMap {
		terms = append(terms, t)
	}
	// Simple insertion sort by descending rune length — the table is small
	// (under 100 entries) and this only ever runs once per process.
	for i := 1; i < len(terms); i++ {
		for j := i; j > 0 && len([]rune(terms[j-1])) < len([]rune(terms[j])); j-- {
			terms[j-1], terms[j] = terms[j], terms[j-1]
		}
	}
	kbJaTermsBySortedLen = terms
}
