# Radiance

[Radiance](https://codeberg.org/StillDeadcode/radiance) is an inference engine by Deadcode. The Forge runs it beside llama.cpp, through an
**unofficial fork** for AMD Strix Halo: [jsaigou/radiance-strix](https://github.com/jsaigou/radiance-strix). The fork is not affiliated with
the original project.

It runs `.rad` model files, which can store different parts of a model at different precisions. llama.cpp cannot load them, and Radiance does
not load GGUF.

## What you need

- An AMD Strix Halo machine (gfx1151, for example a Ryzen AI Max+ 395). Nothing else has been tested.
- Linux with **ROCm 10.x**.
- The kernel parameter `amdgpu.cwsr_enable=1`. The engine does not start without it, and changing it needs a reboot. The installer does not
  edit your boot settings: it prints the line to add.
- `git`, `cmake`, `ninja` and a C++ compiler.

## 1. Install the engine

On Strix Halo the installer builds Radiance by default:

```bash
sudo ./install.sh --target=public
```

If your ROCm is not in a standard place such as `/opt/rocm*`, say where it is:

```bash
sudo ROCM_ROOT=/path/to/rocm-10.1 ./install.sh --target=public
```

The step fetches a pinned release of the fork (`strix-halo-r2`), checks the commit, builds it (about 13 minutes) and registers it in the model
catalog. A failure here does not stop the rest of the install. `--no-radiance` skips the step; `--radiance` builds on hardware the installer
does not recognise.

## 2. Download a model

| Model | File | Memory in use* | Download id |
|---|---:|---|---|
| [Qwen3.8-Flash-Next](https://huggingface.co/jsaigou/Qwen3.8-Flash-Next-UD-Size-radiance) | 84 GiB | about 93 GiB (3 requests, 131K context each) | `qwen38-flash-next-ud-size-rad` |
| [Qwen3.6-35B-A3B, round-to-nearest](https://huggingface.co/jsaigou/Qwen3.6-35B-A3B-W4W5-RTN-radiance) | 21 GiB | about 34 GiB (4 requests, 262K context each) | `qwen36-35b-a3b-w4w5-rtn-rad` |
| [Qwen3.6-35B-A3B, GPTQ-calibrated](https://huggingface.co/jsaigou/Qwen3.6-35B-A3B-W4W5-GPTQ-radiance) | 21 GiB | similar to the line above | `qwen36-35b-a3b-w4w5-gptq-rad` |

\* As measured on a 128 GB machine with the settings on each model card. Smaller settings use less.

```bash
sudo installer/provision.py --only qwen36-35b-a3b-w4w5-rtn-rad --root /var/lib/forge
```

The GPTQ model's Hugging Face page now leads with a newer file for upstream Radiance 2.x. The download id above fetches the earlier file,
which is the one this fork runs.

## 3. Add it to The Forge

In **Settings → Model catalog**, add the downloaded file and a config that uses the Radiance build. Each model card lists the launch settings it
was measured with. After that it behaves like any other model: load it from the Console, or let the router load it on first request.

Check the context you got after the first load. `max_model_len` is the limit for each request:

```bash
curl -s http://localhost:8080/v1/models
```

## Speed

Measured by us on one Ryzen AI Max+ 395 with 128 GB, using the settings on each model card. Your results will differ.

| Model | Measured |
|---|---|
| Qwen3.8-Flash-Next | 91 tokens/s in total across 3 simultaneous requests |
| Qwen3.6-35B-A3B, round-to-nearest | 64 tokens/s for one request (an early single measurement; 59 for the FP8 version on the same test) |

Quality comparisons, and what was **not** measured, are on the model cards.

## Speech-to-speech

The Haru speech model also runs on Radiance, but on a different branch of the fork that the installer does not build. See
[speech-to-speech.md](speech-to-speech.md).
