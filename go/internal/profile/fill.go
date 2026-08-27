// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strings"
)

// fillRNG is a splitmix64 PRNG. The corpus generator needs a SEEDED,
// deterministic source (same seed → same corpus, for benchmark
// comparability), which is exactly what math/rand provided — but math/rand
// also reads as a weak-crypto choice to SAST. A tiny explicit splitmix64
// keeps the determinism contract without the security-adjacent import;
// this randomness feeds benchmark filler text, never anything sensitive.
type fillRNG uint64

func (r *fillRNG) next() uint64 {
	*r += 0x9e3779b97f4a7c15
	z := uint64(*r)
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *fillRNG) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n)) //nolint:gosec // result < n, always fits in int
}

// fillSeed is a ~4 KB corpus of mixed natural-language prose, Python code,
// JSON, and a markdown table. Embedded in the binary; a seeded PRNG shuffles
// + repeats it to reach ~n_ctx tokens (~4 chars/token heuristic). Deterministic
// across runs for comparability. The variety exercises the KV cache + decode
// path realistically (no repeated-token shortcuts).
const fillSeed = `The quick brown fox jumps over the lazy dog. In machine learning, a transformer is a deep learning architecture that relies on the self-attention mechanism. It was introduced in the 2017 paper "Attention Is All You Need" and has since become the dominant architecture for natural language processing tasks.

Transformers process input sequences in parallel rather than sequentially, which allows for significantly faster training compared to recurrent neural networks. The core innovation is the scaled dot-product attention, which computes attention weights as softmax(QK^T / sqrt(d_k)) V, where Q, K, and V are query, key, and value matrices derived from the input embeddings.

The multi-head attention mechanism projects the input into multiple representation subspaces, allowing the model to attend to different parts of the sequence simultaneously. Each head produces its own attention pattern, and the results are concatenated and linearly transformed.

Positional encoding is added to the input embeddings to inject positional information, since the self-attention mechanism itself is permutation-invariant. The original transformer uses sinusoidal positional encodings, though learned positional embeddings are also common in practice.

Layer normalization and residual connections are applied after each sub-layer, stabilizing training of the deep network. The feed-forward network within each transformer layer consists of two linear transformations with a ReLU activation in between.

In reinforcement learning, an agent learns to make decisions by interacting with an environment to maximize cumulative reward. The agent observes the current state, takes an action, receives a reward, and transitions to a new state. The goal is to learn a policy that maps states to actions optimizing the expected return.

Q-learning is a model-free reinforcement learning algorithm that learns the value of an action in a particular state. The Q-value is updated using the Bellman equation: Q(s,a) <- Q(s,a) + alpha[r + gamma * max Q(s',a') - Q(s,a)].

Gradient descent is an optimization algorithm that iteratively adjusts parameters in the direction of the negative gradient of the loss function. Stochastic gradient descent updates parameters using a single training example, while mini-batch gradient descent uses a small subset.
`

const fillCode = `
def attention(Q, K, V, mask=None):
    d_k = K.shape[-1]
    scores = torch.matmul(Q, K.transpose(-2, -1)) / math.sqrt(d_k)
    if mask is not None:
        scores = scores.masked_fill(mask == 0, float('-inf'))
    weights = torch.softmax(scores, dim=-1)
    return torch.matmul(weights, V)

class TransformerBlock(nn.Module):
    def __init__(self, d_model, n_heads, d_ff, dropout=0.1):
        super().__init__()
        self.attention = nn.MultiheadAttention(d_model, n_heads)
        self.norm1 = nn.LayerNorm(d_model)
        self.norm2 = nn.LayerNorm(d_model)
        self.ffn = nn.Sequential(
            nn.Linear(d_model, d_ff),
            nn.ReLU(),
            nn.Linear(d_ff, d_model),
        )
        self.dropout = nn.Dropout(dropout)

    def forward(self, x):
        attn_out, _ = self.attention(x, x, x)
        x = self.norm1(x + self.dropout(attn_out))
        ff_out = self.ffn(x)
        x = self.norm2(x + self.dropout(ff_out))
        return x
`

const fillJSON = `{
  "model": "qwen3-35b",
  "architecture": "qwen2",
  "parameters": 3500000000,
  "quantization": "Q4_K_M",
  "context_length": 32768,
  "layers": [
    {"id": 0, "type": "attention", "heads": 32, "dim": 4096},
    {"id": 1, "type": "feed_forward", "dim": 11008, "activation": "silu"},
    {"id": 2, "type": "attention", "heads": 32, "dim": 4096},
    {"id": 3, "type": "feed_forward", "dim": 11008, "activation": "silu"}
  ],
  "tokenizer": {"type": "BPE", "vocab_size": 32000, "merges": 50000},
  "training": {"data_tokens": 3000000000, "epochs": 1, "lr": 0.0003}
}`

const fillTable = `| Layer | Type         | Heads | Dim    | Parameters |
|-------|-------------|-------|--------|-----------|
| 0     | attention   | 32    | 4096   | 50331648  |
| 1     | feed_forward| -     | 11008  | 90177536  |
| 2     | attention   | 32    | 4096   | 50331648  |
| 3     | feed_forward| -     | 11008  | 90177536  |
| 4     | attention   | 32    | 4096   | 50331648  |
| 5     | feed_forward| -     | 11008  | 90177536  |
| 6     | attention   | 32    | 4096   | 50331648  |
| 7     | feed_forward| -     | 11008  | 90177536  |
| 8     | attention   | 32    | 4096   | 50331648  |
| 9     | feed_forward| -     | 11008  | 90177536  |`

// generateFill produces a deterministic heterogeneous text corpus of ~nCtx
// tokens. The char-to-token ratio is conservatively 3.5 (code/JSON tokenize
// denser than prose), so the generated text is shorter than nCtx*4 to avoid
// overflowing the context window. The corpus is a seeded shuffle-repeat of
// bundled prose + code + JSON + table content, so nothing is artificially
// cache/compression-friendly. Deterministic across runs for comparability.
//
// This is generateFillSeeded with the original fixed seed (42) — kept as
// its own function so existing callers/tests asserting on that exact
// output are untouched.
func generateFill(nCtx int) string {
	return generateFillSeeded(nCtx, 42)
}

// generateFillSeeded is generateFill parameterized by seed. The depth-sweep
// benchmark (profile.go's runDepthSweep, product/QA sprint 2026-07-29) uses
// a different seed per fill/probe call so that successive chunks appended
// to the same growing conversation are genuinely different content, not
// the same corpus restarting from paragraph 0 each time — with a fixed
// seed, two calls at different depths would begin with an identical
// sequence of paragraphs, which is exactly the kind of accidental
// repetition the corpus's own heterogeneity is designed to avoid.
func generateFillSeeded(nCtx int, seed int64) string {
	if nCtx <= 0 {
		nCtx = 4096
	}
	targetChars := int(float64(nCtx) * 3.5)

	corpus := fillSeed + "\n" + fillCode + "\n" + fillJSON + "\n" + fillTable + "\n"
	paragraphs := strings.Split(corpus, "\n\n")
	if len(paragraphs) == 0 {
		paragraphs = []string{corpus}
	}

	rng := fillRNG(uint64(seed)) //nolint:gosec // seed is always non-negative

	var b strings.Builder
	b.Grow(targetChars + 1024)
	for b.Len() < targetChars {
		idx := rng.intn(len(paragraphs))
		b.WriteString(paragraphs[idx])
		b.WriteString("\n\n")
	}
	return b.String()
}
