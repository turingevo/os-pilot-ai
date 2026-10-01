[中文](README.md) | **English**

# AI Installation Assistant (os-pilot-ai)

![](demo.png)

The AI runtime environment and agent for AI-assisted OS installation: initramfs + static Go agent +
a built-in llama-server local inference backend (zero-config, just drop a model on the data
partition) + a userspace-drawn local screen + a built-in Pinyin IME, packaged as a regular mini-ISO
that boots from the Ventoy menu.

Dependencies: **build time needs no Ventoy source** (only host tools such as
grub-mkstandalone/genisoimage/mtools plus a download cache). **Runtime relies on two conventions on
the data partition** — a data partition containing a `/ventoy` directory (mounted at `/iso`), and
`ventoy.json` (written by the `schedule_boot` tool; the unattended installation on the next boot is
executed by Ventoy's GRUB plugin). `ai.json` and the local model are both optional: with a remote
endpoint configured it uses the remote one, with only a model file present it starts a local
llama-server automatically.

## Documentation index

> The detailed documentation under `docs/` is currently written in Chinese.

| Document | Content |
|----------|---------|
| [`docs/AI装机入口设计.md`](docs/AI装机入口设计.md) | Entry design: custom screen, T1 entry, IME, milestones & acceptance criteria, T1 test record |
| [`docs/部署.md`](docs/部署.md) | T1 entry deployment (put the mini-ISO on the data partition), local-model placement (`fetch_local_llm.sh`) and data-partition configuration (`ai.json` / `ventoy.json`) |
| [`docs/测试.md`](docs/测试.md) | Tests & verification: host mock, QEMU end-to-end, Ventoy menu, real USB drive, toolchain, reinstall loop, zero-config local-model loop |
| [`docs/本地屏.md`](docs/本地屏.md) | Local-screen interaction (mode switching / history / completion) and the built-in Pinyin IME (keys & environment variables) |
| [`docs/工具与安全.md`](docs/工具与安全.md) | The 11 agent tools, the bundled toolchain, and the 3-layer protection for the disk tools |

## Repository layout

| Path | Description |
|------|-------------|
| `agent/` | Static Go agent (stdlib-only, `CGO_ENABLED=0`). Entry `main.go`: `--config/--payload/--script/--base-url/--model/--api-key/--mode/--log-dir/--no-reboot` |
| `agent/internal/{config,llm,session}/` | `ai.json` config & validation; OpenAI-compatible client (function calling, proxy); session loop, confirmation gate, JSONL+MD logs |
| `agent/internal/tools/` | 11 tools: `system_probe` `fs_read` `fs_write` `run_command` `ask_user` `schedule_boot` `list_disks` `partition` `format` `backup` `gen_autoinstall` |
| `agent/internal/disk/` | Disk domain layer (reusable by the agent tools and a future TUI/GTK/WebUI): block-device enumeration, protected-device detection, parted/mke2fs/rsync command building & argument whitelisting, structured JSON results |
| `agent/internal/screen/` | Userspace-drawn screen: VTF1 bitmap font (generated from GNU Unifont) + grid terminal emulation + fb canvas + raw line editor + Pinyin IME client |
| `ime/` | Pinyin IME helper: `pinyin-ime.cpp` (static libgooglepinyin wrapper), `build.sh`, `LICENSE` (Apache-2.0) |
| `init/` | initramfs PID 1: `init` (mount → load modules → mount data partition → local model (optional) → DHCP → run agent → poweroff), `mount_payload.sh` (mount the partition containing `/ventoy` at `/iso`), `udhcpc.script` |
| `config/ai.json.example` | Config example (deploy as `/ventoy/ai.json` on the data partition; optional — a local model is used automatically when absent) |
| `tools/make_font.py` | unifont.hex → font.bin (VTF1: binary-search index + 32 B/glyph, mixed-width bitmaps) |
| `tools/mock_openai_server.py` | Offline mock LLM (scripted tool_calls); `VTOY_MOCK_LOG` dumps request bodies, `VTOY_MOCK_SCENARIO=disk` for the disk scenario |
| `tools/fetch_local_llm.sh` | One-shot placement of llama-server + a GGUF model onto the USB data partition (SHA256 checks, resume support; zero-config local inference once done) |
| `dev.env.sh.example` | Template for the developer's machine-local config (build-time paths and machine-specific values; copy to `dev.env.sh` and source it — the local file is gitignored) |
| `docs/` | Documentation (see the index above) |

`pack/` scripts (all rootless; paths resolved via `dev.env.sh` + `defaults.sh`):

| Script | Description |
|--------|-------------|
| `defaults.sh` | Fallback default layer for build paths (sourced by the other scripts): positional args > `VTOY_AI_*` env vars > built-in fallback outside the repo; it reads no user config file at all (machine-local config lives in `dev.env.sh` at the repo root); when `VTOY_AI_BUILD_DIR` is unset it prints a hint naming the build directory actually in effect; also provides the QEMU resource defaults (KVM/memory/CPUs) |
| `build_busybox.sh` | Build a static x86_64 busybox (downloads the official source tarball + SHA256 verification by default) |
| `fetch_kernel.sh` | Download the Ubuntu 26.04 distro kernel (linux-image + linux-modules, pinned SHA256) and trim modules per `kernel-modules.list` (`.ko.zst` → `.ko`) |
| `kernel-modules.list` | Module list shipped in the initramfs (storage/filesystems/NICs/input/display; dependency closure of 63 modules) |
| `build_tools.sh` | Statically build the partitioning/formatting/backup toolchain, 13 binaries: e2fsprogs 1.47.0 + parted 3.6 + rsync 3.2.7 + exfatprogs 1.4.3 + f2fs-tools 1.16.0 + ntfs-3g 2021.8.22 (pinned source SHA256) |
| `build_llama.sh` | Statically build the llama-server local inference backend (`v3` = x86-64-v3/AVX2 default, `v2` = SSE4.2 for older CPUs; pinned llama.cpp commit + tarball SHA256) |
| `make_font.sh` | unifont.hex → `$BUILD/screen/font.bin` (shipped with the initramfs; pack_env.sh invokes it automatically when the font is missing) |
| `pack_env.sh` | Pack the initramfs: init + agent + trimmed module tree + bitmap font + IME + toolchain + local inference backend (auto-generates the font when missing; only warns if the rest are missing) |
| `make_test_disk.sh` | Create a test payload disk (ext4 with fake ISOs / answer file / ai.json, rootless via `mke2fs -d`) |
| `make_iso.sh` | Build the AI-environment mini-ISO (T1 entry) |
| `get_mtools.sh` / `find_mtools.sh` | Fetch / locate mtools without root (`mcopy`/`mmd`, needed by `make_ventoy_testdisk.sh`) |
| `make_ventoy_testdisk.sh` | Assemble a complete Ventoy test-disk image (official boot assets + self-built data partition) |
| `make_blank_disk.sh` | Create/reset a blank virtual target disk (all-zero, no partition table); **an existing file with the same name is overwritten** |
| `run_qemu.sh` | Direct QEMU boot (serial console); `VTOY_AI_INTERACTIVE=1` for interactive chat |
| `run_qemu_ventoy.sh` | Boot the Ventoy test disk in QEMU (through the real menu) |
| `run_qemu_usb.sh` | Attach a real USB drive to QEMU (needs root); refuses non-USB/mounted devices; optionally attaches a target disk |
| `run_qemu_target.sh` | Boot an **installed** target-disk image in QEMU (rootless) to verify it boots |
| `run_qemu_ime.sh` | Local-screen IME end-to-end regression (sendkey-driven; verifies the Chinese text sent to the model) |
| `run_qemu_ui.py` | Local-screen interaction end-to-end regression (QMP send-key + bitmap-font OCR, 34 assertions) |
| `verify_tools.sh` | In-guest test of the bundled toolchain (pure `!command`, 35 assertions, no LLM needed) |
| `verify_disk_tools.sh` | In-guest test of the guarded disk tools (mock LLM scenario, 30 assertions) |
| `verify_install_loop.sh` | "AI builds the disk → target distro reads it → real boot" loop (before/after contrast) |
| `verify_target_image.sh` | Read-only static criteria for an installed target disk ("can it boot") |

## Building (rootless, entirely outside the repo)

Variables split into three layers by **ownership**, each written in a different place:

| Owner | Where it lives | Typical variables |
|-------|----------------|-------------------|
| Developer machine config (build time, set up once) | `dev.env.sh` at the repo root (gitignored; template `dev.env.sh.example`) — `. ./dev.env.sh` turns it into environment variables | `VTOY_AI_BUILD_DIR` `VTOY_AI_BUSYBOX_SRC` `VTOY_AI_VENTOY_RELEASE` `VTOY_AI_BUILD_JOBS` `VTOY_AI_QEMU_MEM` |
| Single-run knobs (this invocation only) | Command-line prefix — **never written into a file** | `VTOY_AI_INTERACTIVE=1` `VTOY_AI_TOOLS_FORCE=1` `VTOY_AI_DISK=/dev/sdb` |
| Product runtime config (inside the guest) | `ai.json` / `ventoy.json` on the USB data partition | `api_key` `base_url` `screen` `local_llm` |

Resolution therefore has just two sources: **positional args > environment variables > the built-in
fallback in `pack/defaults.sh` (`${XDG_CACHE_HOME:-$HOME/.cache}/ventoy-ai`)**. `defaults.sh` is a
pure fallback layer (every other script sources it) and reads no user config file; when
`VTOY_AI_BUILD_DIR` is unset, sourcing it prints a hint naming the build directory actually in
effect and pointing at `dev.env.sh`.

```sh
# Configure the dev machine once; afterwards every pack/ ime/ tools/ script runs as-is
cp dev.env.sh.example dev.env.sh    # then edit the paths for your machine
. ./dev.env.sh                      # bare exports only, no path self-discovery — sourcing it by absolute path from anywhere works too

# Building without dev.env.sh also works: pass values per invocation, or take the built-in
# fallback (which prints the hint)
VTOY_AI_BUILD_DIR=/path/to/ventoy-ai-build sh pack/pack_env.sh
```

```sh
AI=$(pwd)                        # repo root; examples assume you are in the repo root and have run `. ./dev.env.sh`
BUILD=$VTOY_AI_BUILD_DIR         # $BUILD below is the build directory (built-in fallback when not sourced)

# 1) agent (output: agent/os-pilot-ai inside the repo, gitignored; the bitmap font is NOT embedded
#    in the binary — it is generated/packed by pack_env.sh in step 7)
$AI/agent/build.sh

# 2) static x86_64 busybox (downloads the official busybox-1.36.1 tarball + SHA256 check;
#    point VTOY_AI_BUSYBOX_SRC at an existing source tree to skip the download)
$AI/pack/build_busybox.sh

# 3) distro kernel (Ubuntu 26.04 LTS, 7.0.0-34-generic; Tsinghua mirror + pinned SHA256 by default)
#    outputs $BUILD/kernel/vmlinuz and a module tree trimmed per kernel-modules.list
#    (.ko.zst decompressed to .ko at pack time)
$AI/pack/fetch_kernel.sh

# 4) built-in Pinyin IME (optional; outputs $BUILD/ime/{pinyin-ime,dict_pinyin.dat})
#    libgooglepinyin (Apache-2.0) fully static; if missing, pack_env.sh only warns and the
#    agent degrades to no-IME
sh $AI/ime/build.sh

# 5) partitioning/formatting/backup toolchain (optional but recommended; 13 static binaries in
#    $BUILD/tools/: mke2fs e2fsck resize2fs tune2fs dumpe2fs parted rsync
#    mkfs.exfat fsck.exfat mkfs.f2fs fsck.f2fs mkntfs ntfsfix)
#    source tarballs are cached in $BUILD/dl/, reruns do not re-download
sh $AI/pack/build_tools.sh

# 6) local inference backend llama-server (optional but recommended; output $BUILD/llama-server.v3, ~15 MB)
#    pinned llama.cpp commit, fully static; older CPUs (no AVX2) use `sh $AI/pack/build_llama.sh v2`
sh $AI/pack/build_llama.sh

# 7) pack the initramfs (init + agent + trimmed module tree + bitmap font + IME + toolchain + local inference backend)
#    generates the font via pack/make_font.sh when missing (apt-downloads GNU Unifont;
#    offline: VTOY_FONT_HEX=<unifont.hex> sh pack/make_font.sh first)
$AI/pack/pack_env.sh

# 8) create the test disk (ext4 payload with fake ISOs / answer file / ventoy.json / AI env files)
$AI/pack/make_test_disk.sh

# 9) build the AI-environment mini-ISO (T1 entry; output $BUILD/test/0-OS-PILOT-AI.iso, ~52 MB)
$AI/pack/make_iso.sh

# 10) fetch mtools without root (mcopy/mmd, only needed by step 11; skip if the host PATH has mtools)
sh $AI/pack/get_mtools.sh

# 11) assemble the full Ventoy test-disk image (optional; for "real Ventoy menu" end-to-end tests)
sh $AI/pack/make_ventoy_testdisk.sh
```

See [`docs/测试.md`](docs/测试.md) for the test & verification workflow, and
[`docs/部署.md`](docs/部署.md) for deploying to a real USB drive.

## Status & known limitations

Verified on QEMU (x86_64) and a real USB drive: boot → mount data partition → DHCP → multi-turn Q&A
with tool calls → write `ventoy.json` → persist logs → poweroff; the T1 menu loop (zero Ventoy code
changes); userspace screen on real UEFI GOP plus Pinyin Chinese input; the bundled toolchain and the
guarded disk tools pass in-guest tests; **the real-machine reinstall loop works end-to-end** (Ubuntu
22.04 installed onto an AI-built disk; `verify_target_image.sh` 8/8 and `run_qemu_target.sh` boots
to gdm3); **the zero-config local-model loop works end-to-end** (exFAT data partition + built-in
llama-server + a GGUF model → service starts automatically, ready in 2 s, the agent talks to the
local model in Chinese over multiple turns). Per-round test records are in [`docs/测试.md`](docs/测试.md); milestones, acceptance
criteria and the T1 test record are in [`docs/AI装机入口设计.md`](docs/AI装机入口设计.md) §13 / §4.4.

Known limitations:

- Automated regressions mostly use a mock LLM; a real model endpoint (Qwen family) has been verified on real hardware, but function-calling compatibility with other vendors' endpoints has not been checked one by one
- Verified on x86_64 only (QEMU + real USB); ARM64 is untested
- The T3 menu entry is not yet wired into `INSTALL/grub/grub.cfg` or the build pipeline (`INSTALL` / `GRUB2` packaging)
- Disk tools can format ext2/3/4, vfat, exfat, ntfs and f2fs; HFS+ and XFS are read/write-mount only
  (no usable Linux-side creation tool / xfsprogs not bundled), APFS and ReFS are unsupported — see the
  [filesystem support matrix](docs/工具与安全.md#文件系统支持矩阵). partition/format/backup end-to-end
  against **a real USB drive itself** (including hot-plug) is untested
- `direct` mode (skip confirmations), encrypted key storage, and rollback strategies beyond `/undo` lack end-to-end verification
- Local model: the GGUF is **not** packed into the ISO (it lives on the data partition under
  `/ventoy/ai/models/`; a 4B-Q4-class model needs ~4.5 GB of RAM); the bundled binary is v3 (AVX2),
  older CPUs without AVX2 need the v2 variant (shipped with the Release or built from source);
  inference speed depends on the machine (CPU-only, no GPU/CUDA)

## License

The code authored in this repository is **Apache-2.0** as of 2026-09-30 (full text in
[LICENSE](LICENSE)); earlier commits (up to `97c31d8`) were released under GPL-3.0-only and remain
valid and irrevocable for those who obtained them. This repository neither links against nor
contains Ventoy source code; it cooperates only through public conventions — reading/writing
`ventoy.json` on the data partition, whose configuration is executed by Ventoy's GRUB plugin on the
next boot.

**External contributions**: the repository has a single author and no CLA yet; before accepting
external PRs, agree on a "contributions are relicensable" term to preserve the ability to switch
the license (including to closed source) in the future.

Third-party components pulled in at build time (none are committed to the repository; artifacts are
distributed with the initramfs/ISO under their own terms, unaffected by this repository's license):

| Component | License | How it is used |
|-----------|---------|----------------|
| GNU Unifont | GPL-2.0-or-later (with font embedding exception) | `pack/make_font.sh` generates `$BUILD/screen/font.bin`, shipped as a **standalone data file** with the initramfs (not embedded in the agent binary; source & license in `/ventoy/ai/screen/LICENSE.unifont` inside the package) |
| busybox 1.36.1 | GPL-2.0-only | `pack/build_busybox.sh` downloads and builds the official source |
| Linux kernel & modules (Ubuntu distro packages) | GPL-2.0-only | `pack/fetch_kernel.sh` downloads and trims per the module list |
| libgooglepinyin | Apache-2.0 | `ime/build.sh` builds the helper statically; full terms in [ime/LICENSE](ime/LICENSE) |
| e2fsprogs 1.47.0 | GPL-2.0-or-later / LGPL-2.1 (libuuid) | `pack/build_tools.sh` (kernel.org sources, SHA256-pinned) |
| GNU parted 3.6 | GPL-3.0-or-later | Same as above (libuuid from the e2fsprogs tree; libblkid statically linked from the host) |
| rsync 3.2.7 | GPL-3.0-only | Same as above (bundled popt/zlib source trees) |
| exfatprogs 1.4.3 | GPL-2.0-only | Same as above (`mkfs.exfat` / `fsck.exfat`; all optional external dependencies disabled) |
| f2fs-tools 1.16.0 | GPL-2.0-only (`lib/`, `libf2fs*`, `f2fs_fs.h` under a LGPL-2.1 dual license) | Same as above (`mkfs.f2fs` / `fsck.f2fs`; the tarball ships no `configure`, needs host `autoreconf -fi`) |
| ntfs-3g 2021.8.22 | GPL-2.0-or-later (NTFS components and libntfs-3g; fuse-lite under LGPL-2.0) | Same as above, **only `mkntfs` / `ntfsfix`** (`--disable-ntfs-3g`); read/write mounting uses the kernel ntfs3 driver, no FUSE in the artifacts |
| llama.cpp (llama-server) | MIT | `pack/build_llama.sh` builds a pinned commit statically (v3 bundled in the initramfs, v2 for older CPUs) |
| GRUB (standalone bootloader) | GPL-3.0-or-later | `pack/make_iso.sh` invokes the host `grub-mkstandalone` |

All of the above form a **mere aggregation** with the first-party code: packaged and loaded
independently, none is a derivative work of the others. The font data in particular has been
**externalized** from the agent binary (`/ventoy/ai/screen/font.bin`, loaded from disk at runtime);
shipping copyleft components on the distribution medium does not bring first-party code under their
licenses.
