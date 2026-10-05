-- SPDX-License-Identifier: Apache-2.0
-- Schema v90 (context-creation plan WS-N1, docs/compression-redesign/CONTRACTS.md C2/C4).
--
-- Hourly rollups of the a0 creation ledger: how much context each consumer/
-- model adds per request and which tool outputs dominate. SIZES AND NAMES
-- ONLY — no column ever holds message content, tool arguments or tool output.
-- Additive: one new table + index, nothing existing is touched. 90-day
-- retention is enforced by the ledger's prune job, not by this schema.
--
-- tool = '' is the request-level row for (hour, consumer, model); tool = '<name>'
-- rows carry that tool's output stats (requests = requests with >=1 new output
-- of that tool). Histograms are sparse "bucket:count,..." text (log-scale chars
-- buckets for result/schema sizes, 5%-wide buckets for reuse) so percentiles
-- can be merged across hours without storing raw samples.
CREATE TABLE IF NOT EXISTS context_creation_hourly (
    hour                      INTEGER NOT NULL,           -- unix seconds, UTC hour start
    consumer                  TEXT    NOT NULL,
    model                     TEXT    NOT NULL,
    tool                      TEXT    NOT NULL DEFAULT '',
    requests                  INTEGER NOT NULL DEFAULT 0,
    new_msgs                  INTEGER NOT NULL DEFAULT 0,
    user_chars                INTEGER NOT NULL DEFAULT 0,
    assistant_text_chars      INTEGER NOT NULL DEFAULT 0,
    assistant_reasoning_chars INTEGER NOT NULL DEFAULT 0,
    tool_args_chars           INTEGER NOT NULL DEFAULT 0,
    other_chars               INTEGER NOT NULL DEFAULT 0, -- system/developer/other roles
    tool_results              INTEGER NOT NULL DEFAULT 0,
    tool_output_chars         INTEGER NOT NULL DEFAULT 0,
    big_8k                    INTEGER NOT NULL DEFAULT 0,
    big_24k                   INTEGER NOT NULL DEFAULT 0,
    max_result                INTEGER NOT NULL DEFAULT 0,
    result_hist               TEXT    NOT NULL DEFAULT '',
    schema_requests           INTEGER NOT NULL DEFAULT 0,
    schema_chars_sum          INTEGER NOT NULL DEFAULT 0,
    schema_hist               TEXT    NOT NULL DEFAULT '',
    reuse_n                   INTEGER NOT NULL DEFAULT 0,
    reuse_hist                TEXT    NOT NULL DEFAULT '',
    cache_bust_suspects       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (hour, consumer, model, tool)
);
CREATE INDEX IF NOT EXISTS idx_context_creation_hourly_hour ON context_creation_hourly(hour);
