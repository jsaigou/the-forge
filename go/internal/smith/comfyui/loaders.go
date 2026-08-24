// SPDX-License-Identifier: Apache-2.0

package comfyui

import "regexp"

// LoaderSpec is one loader node class_type's file-reference shape, in both
// projections a real installation's data can take (docs/v5-smith.md §4.9's
// mapping table): the API/prompt format's named input field, and the saved
// UI-workflow format's positional widgets_values index. FolderType is the
// ComfyUI models/<subfolder> this loader reads from (folder_paths.py's
// convention) — also the key BuildMap groups referenced names and disk
// candidates by.
type LoaderSpec struct {
	FolderType  string
	APIField    string
	WidgetIndex int
}

// Loaders is the mapping table — one entry per loader class_type this
// package understands. Covers every class_type actually seen live in
// ForgeHost's two real workflows (UNETLoader, CLIPLoader, VAELoader,
// LoraLoaderModelOnly) plus the other standard single-filename ComfyUI
// loaders §4.9 names. A class_type NOT in this table is exactly what
// BuildMap's guardrail (c) — "unknown loader class_type" — exists to catch:
// this table is deliberately not treated as exhaustive by the code that
// reads it.
var Loaders = map[string]LoaderSpec{
	"CheckpointLoaderSimple": {FolderType: "checkpoints", APIField: "ckpt_name", WidgetIndex: 0},
	"unCLIPCheckpointLoader": {FolderType: "checkpoints", APIField: "ckpt_name", WidgetIndex: 0},
	"UNETLoader":             {FolderType: "diffusion_models", APIField: "unet_name", WidgetIndex: 0},
	"UnetLoaderGGUF":         {FolderType: "diffusion_models", APIField: "unet_name", WidgetIndex: 0},
	"CLIPLoader":             {FolderType: "text_encoders", APIField: "clip_name", WidgetIndex: 0},
	"DualCLIPLoader":         {FolderType: "text_encoders", APIField: "clip_name1", WidgetIndex: 0},
	"CLIPVisionLoader":       {FolderType: "clip_vision", APIField: "clip_name", WidgetIndex: 0},
	"VAELoader":              {FolderType: "vae", APIField: "vae_name", WidgetIndex: 0},
	"LoraLoader":             {FolderType: "loras", APIField: "lora_name", WidgetIndex: 0},
	"LoraLoaderModelOnly":    {FolderType: "loras", APIField: "lora_name", WidgetIndex: 0},
	"ControlNetLoader":       {FolderType: "controlnet", APIField: "control_net_name", WidgetIndex: 0},
	"DiffControlNetLoader":   {FolderType: "controlnet", APIField: "control_net_name", WidgetIndex: 0},
	"UpscaleModelLoader":     {FolderType: "upscale_models", APIField: "model_name", WidgetIndex: 0},
	"StyleModelLoader":       {FolderType: "style_models", APIField: "style_model_name", WidgetIndex: 0},
	"GLIGENLoader":           {FolderType: "gligen", APIField: "gligen_name", WidgetIndex: 0},
	"HypernetworkLoader":     {FolderType: "hypernetworks", APIField: "hypernetwork_name", WidgetIndex: 0},
	"PhotoMakerLoader":       {FolderType: "photomaker", APIField: "photomaker_model_name", WidgetIndex: 0},
}

// KnownFolderTypes returns the distinct FolderType values Loaders names, for
// disk-inventory walking and root-coverage checking.
func KnownFolderTypes() []string {
	seen := map[string]bool{}
	var out []string
	for _, spec := range Loaders {
		if !seen[spec.FolderType] {
			seen[spec.FolderType] = true
			out = append(out, spec.FolderType)
		}
	}
	return out
}

// syntheticComboValues are combo-list entries ComfyUI's own node code
// injects that are NOT real files — found live on ForgeHost 2026-08-11 running
// the P6 guardrail (d) check for real: `object_info/VAELoader`'s
// `vae_name` combo genuinely returns `["qwen_image_vae.safetensors",
// "pixel_space"]`, where "pixel_space" is VAELoader's built-in "no VAE /
// passthrough" sentinel, hardcoded in ComfyUI's own nodes.py — never a
// filename under any model root. Without this exclusion, guardrail (d)
// permanently refuses to build a map on any install using VAELoader,
// mistaking a real, legitimate ComfyUI quirk for a missing root. A small,
// explicit, maintained table (same posture as Loaders itself) rather than
// a heuristic like "values with no file extension" — the failure mode of
// guessing wrong here is silently widening what counts as "found", which
// is exactly the direction guardrail (d) exists to prevent.
var syntheticComboValues = map[string]bool{
	"pixel_space": true,
}

// embeddingTokenRe matches ComfyUI's `embedding:<name>` prompt-text token
// syntax — the one file reference that lives inside a free-text widget
// (CLIPTextEncode) rather than a dedicated loader node.
var embeddingTokenRe = regexp.MustCompile(`embedding:([A-Za-z0-9_\-./]+)`)

// embeddingTokens extracts every embedding name referenced in a text
// widget's raw string content.
func embeddingTokens(text string) []string {
	m := embeddingTokenRe.FindAllStringSubmatch(text, -1)
	out := make([]string, 0, len(m))
	for _, g := range m {
		out = append(out, g[1])
	}
	return out
}
