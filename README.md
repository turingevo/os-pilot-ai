**中文** | [English](README.en.md)

# AI 装机助手（os-pilot-ai）

![](demo.png)

AI 装机的 AI 运行环境与 agent：initramfs + 静态 Go agent + 自绘本地屏 + 内置拼音输入法，打成
普通 mini-ISO 由 Ventoy 菜单启动。

依赖关系：**构建期不依赖 Ventoy 源码**（只需宿主的 grub-mkstandalone/genisoimage/mtools 等工具与
下载缓存）；**运行期依赖数据分区上的两条约定**——含 `/ventoy` 目录的数据分区（挂载到 `/iso`）和
`ventoy.json`（`schedule_boot` 工具写它，下一轮开机的无人值守安装由 Ventoy 的 grub 插件执行）。

## 文档索引

| 文档 | 内容 |
|------|------|
| [`docs/AI装机入口设计.md`](docs/AI装机入口设计.md) | 入口设计：自绘屏幕、T1 入口、输入法、里程碑与验收标准、T1 实测记录 |
| [`docs/部署.md`](docs/部署.md) | T1 入口部署（mini-ISO 放上数据分区）与数据分区配置（`ai.json` / `ventoy.json`） |
| [`docs/测试.md`](docs/测试.md) | 测试与验证：宿主机 mock、QEMU 端到端、Ventoy 菜单、真实 U 盘、工具链、重装闭环 |
| [`docs/本地屏.md`](docs/本地屏.md) | 本地屏交互（模式切换 / 历史 / 补全）与内置拼音输入法（按键与环境变量） |
| [`docs/工具与安全.md`](docs/工具与安全.md) | 11 个 agent 工具、内置工具链、磁盘工具三层防护 |

## 目录结构

| 路径 | 说明 |
|------|------|
| `agent/` | Go 静态 agent（stdlib-only，`CGO_ENABLED=0`）。入口 `main.go`：`--config/--payload/--script/--base-url/--model/--api-key/--mode/--log-dir/--no-reboot` |
| `agent/internal/{config,llm,session}/` | `ai.json` 配置与校验；OpenAI 兼容客户端（function calling、代理）；会话循环、确认闸门、JSONL+MD 日志 |
| `agent/internal/tools/` | 11 个工具：`system_probe` `fs_read` `fs_write` `run_command` `ask_user` `schedule_boot` `list_disks` `partition` `format` `backup` `gen_autoinstall` |
| `agent/internal/disk/` | 磁盘业务层（供 agent 工具与将来的 TUI/GTK/WebUI 复用）：块设备枚举、受保护设备判定、parted/mke2fs/rsync 命令拼装与参数白名单，返回结构化 JSON |
| `agent/internal/screen/` | 用户态自绘屏幕：VTF1 点阵字体（GNU Unifont 生成）+ 网格终端仿真 + fb 画布 + raw 行编辑 + 拼音输入法客户端 |
| `ime/` | 拼音输入法 helper：`pinyin-ime.cpp`（libgooglepinyin 静态封装）、`build.sh`、`LICENSE`（Apache-2.0） |
| `init/` | initramfs PID 1：`init`（挂载 → 加载模块 → 挂数据分区 → DHCP → 运行 agent → 关机）、`mount_payload.sh`（挂载含 `/ventoy` 的分区到 `/iso`）、`udhcpc.script` |
| `config/ai.json.example` | 配置示例（部署为数据分区 `/ventoy/ai.json`） |
| `tools/make_font.py` | unifont.hex → font.bin（VTF1：索引二分 + 32B/字形，混宽位图） |
| `tools/mock_openai_server.py` | 离线 mock LLM（剧本化 tool_calls）；`VTOY_MOCK_LOG` 落请求体、`VTOY_MOCK_SCENARIO=disk` 磁盘剧本 |
| `docs/` | 文档目录（索引见上方「文档索引」） |

`pack/` 脚本（全部免 root；路径经 `defaults.sh` 解析）：

| 脚本 | 说明 |
|------|------|
| `defaults.sh` | 构建路径解析层（其余脚本 source）：位置参数 > `VTOY_AI_*` 环境变量 > `~/.config/ventoy-ai/defaults.sh` > 仓库外内置默认；并给出 QEMU 资源默认值（KVM/内存/CPU） |
| `build_busybox.sh` | 编译 x86_64 静态 busybox（默认下载官方源码包 + SHA256 校验） |
| `fetch_kernel.sh` | 下载 Ubuntu 26.04 发行版内核（linux-image + linux-modules，SHA256 固定），按 `kernel-modules.list` 裁剪模块（`.ko.zst` → `.ko`） |
| `kernel-modules.list` | 随 initramfs 携带的模块清单（存储/文件系统/网卡/输入/显示，依赖闭包 59 个） |
| `build_tools.sh` | 静态编译分区/格式化/备份工具链：e2fsprogs 1.47.0 + parted 3.6 + rsync 3.2.7（源码 SHA256 固定） |
| `make_font.sh` | unifont.hex → `$BUILD/screen/font.bin`（随 initramfs 分发；缺字体时 pack_env.sh 自动调用） |
| `pack_env.sh` | 打包 initramfs：init + agent + 裁剪模块树 + 点阵字体 + 输入法 + 工具链（字体缺失时自动生成；输入法/工具链缺失只警告） |
| `make_test_disk.sh` | 造测试 payload 盘（ext4，含假 ISO / 应答脚本 / ai.json，`mke2fs -d` 免 root） |
| `make_iso.sh` | 生成 AI 环境 mini-ISO（T1 入口） |
| `get_mtools.sh` / `find_mtools.sh` | 免 root 获取 / 定位 mtools（`mcopy`/`mmd`，`make_ventoy_testdisk.sh` 需要） |
| `make_ventoy_testdisk.sh` | 组装完整 Ventoy 测试盘镜像（官方引导资产 + 自建数据分区） |
| `make_blank_disk.sh` | 造/重置一块空白虚拟靶盘（全零、无分区表）；**已存在的同名文件会被覆盖清空** |
| `run_qemu.sh` | QEMU 直启（串口）；`VTOY_AI_INTERACTIVE=1` 交互对话 |
| `run_qemu_ventoy.sh` | QEMU 启 Ventoy 测试盘（走真实菜单） |
| `run_qemu_usb.sh` | QEMU 直挂真实 U 盘（需 root），拒绝非 USB/已挂载设备；可选加挂靶盘 |
| `run_qemu_target.sh` | QEMU 启动一块**已安装**的目标盘镜像（免 root），验证装完能否启动 |
| `run_qemu_ime.sh` | 本地屏输入法端到端回归（sendkey 驱动 + 校验发给模型的中文） |
| `run_qemu_ui.py` | 本地屏交互端到端回归（QMP send-key + 点阵字体 OCR，34 项断言） |
| `verify_tools.sh` | guest 内实测内置工具链（纯 `!命令`，16 项断言，不需要 LLM） |
| `verify_disk_tools.sh` | guest 内实测受守卫磁盘工具（mock LLM 剧本，30 项断言） |
| `verify_install_loop.sh` | 「AI 建房 → 目标发行版读盘 → 真实引导」闭环（修前/修后对照） |
| `verify_target_image.sh` | 已安装目标盘的「能启动」静态判据（只读取证） |

## 构建（rootless，全部在仓库外构建）

构建目录由 `pack/defaults.sh` 统一解析，优先级：**位置参数 > 环境变量 `VTOY_AI_BUILD_DIR` >
机器本地 `~/.config/ventoy-ai/defaults.sh` > 内置默认 `${XDG_CACHE_HOME:-$HOME/.cache}/ventoy-ai`**。
本机私有路径写进本机文件，不要提交：

```sh
# ~/.config/ventoy-ai/defaults.sh
export VTOY_AI_BUILD_DIR=/path/to/ventoy-ai-build
export VTOY_AI_BUSYBOX_SRC=/path/to/busybox-1.36.1    # 可选，不给则自动下载官方源码包
export VTOY_AI_VENTOY_RELEASE=/path/to/ventoy-1.1.05  # 仅 make_ventoy_testdisk.sh 需要
```

```sh
AI=$(pwd)                        # 仓库根；下文示例假定已在仓库根目录
BUILD=/path/to/ventoy-ai-build   # 不给则走上面的解析顺序；下文 $BUILD 指构建目录

# 1) agent（产物在仓库内 agent/os-pilot-ai，已被 .gitignore 忽略；点阵字体不在二进制内，
#    由第 6 步 pack_env.sh 生成/打包）
$AI/agent/build.sh

# 2) x86_64 静态 busybox（默认下载官方 busybox-1.36.1 源码包 + SHA256 校验；
#    有现成源码树时用 VTOY_AI_BUSYBOX_SRC 指过去）
$AI/pack/build_busybox.sh

# 3) 发行版内核（Ubuntu 26.04 LTS，7.0.0-34-generic；默认清华镜像 + SHA256 固定）
#    产物 $BUILD/kernel/vmlinuz 与按 kernel-modules.list 裁剪的模块树（打包期 .ko.zst 解压为 .ko）
$AI/pack/fetch_kernel.sh

# 4) 内置拼音输入法（可选；产物 $BUILD/ime/{pinyin-ime,dict_pinyin.dat}）
#    libgooglepinyin（Apache-2.0）全静态编译；缺失时 pack_env.sh 只警告，agent 降级为无输入法
sh $AI/ime/build.sh

# 5) 分区/格式化/备份工具链（可选，但推荐；产物 $BUILD/tools/{mke2fs,e2fsck,resize2fs,tune2fs,dumpe2fs,parted,rsync}）
#    源码包下载进 $BUILD/dl/ 缓存，重跑不重复下载
sh $AI/pack/build_tools.sh

# 6) 打包 initramfs（init + agent + 裁剪模块树 + 点阵字体 + 输入法 + 工具链）
#    点阵字体缺失时自动调 pack/make_font.sh 生成（apt 下载 GNU Unifont；
#    离线环境先跑 VTOY_FONT_HEX=<unifont.hex> sh pack/make_font.sh）
$AI/pack/pack_env.sh

# 7) 生成测试盘（ext4 payload，含假 ISO / 应答脚本 / ventoy.json / AI 环境文件）
$AI/pack/make_test_disk.sh

# 8) 生成 AI 环境 mini-ISO（T1 入口；产物 $BUILD/test/0-OS-PILOT-AI.iso，约 46MB）
$AI/pack/make_iso.sh

# 9) 免 root 获取 mtools（仅第 10 步需要 mcopy/mmd；宿主 PATH 里已有 mtools 则跳过）
sh $AI/pack/get_mtools.sh

# 10) 组装完整 Ventoy 测试盘镜像（可选；供“走真实 Ventoy 菜单”的端到端测试）
sh $AI/pack/make_ventoy_testdisk.sh
```

测试与验证流程见 [`docs/测试.md`](docs/测试.md)；部署到真实 U 盘见 [`docs/部署.md`](docs/部署.md)。

## 现状与已知限制

QEMU（x86_64）与真机 U 盘均已验证：启动 → 挂载数据分区 → DHCP → 多轮问答与工具调用 → 写 `ventoy.json`
→ 日志落盘 → 关机；T1 菜单闭环（零 Ventoy 代码改动）；真机 UEFI GOP 自绘屏 + 拼音中文输入；内置工具链
与受守卫磁盘工具 guest 内实测通过；**真机重装闭环已跑通**（Ubuntu 22.04 装到 AI 建的盘，
`verify_target_image.sh` 8/8 + `run_qemu_target.sh` 引导到 gdm3）。逐轮实测记录见
[`docs/测试.md`](docs/测试.md)；里程碑与验收标准、T1 实测记录见
[`docs/AI装机入口设计.md`](docs/AI装机入口设计.md) §13 / §4.4。

已知限制：

- 自动化回归多用 mock LLM；真实模型端点（Qwen 系列）已在真机跑通，其它厂商端点的 function calling 兼容性未逐一验证
- 仅在 x86_64 上验证（QEMU + 真机 U 盘）；ARM64 平台未测
- 尚未把 T3 菜单项接入 `INSTALL/grub/grub.cfg` 与构建流程（`INSTALL` / `GRUB2` 打包）
- 磁盘工具目前只格式化 ext2/3/4/vfat（未内置 exFAT/NTFS 工具）；对**真实 U 盘本身**的 partition/format/backup 端到端（含拔插）未测
- `direct` 模式（跳过确认）与密钥加密存储、`/undo` 之外的回滚策略未做端到端验证

## 许可证

本仓自研代码自 2026-09-30 起为 **Apache-2.0**（全文见 [LICENSE](LICENSE)）；此前提交（至 `97c31d8`）
按 GPL-3.0-only 发布，对已获取者继续有效、不可撤回。本仓不链接、也不包含 Ventoy 源码；
只按公开约定协作——读写数据分区上的 `ventoy.json`，并由 Ventoy 的 grub 插件在下一轮开机执行配置。

**外部贡献**：仓库目前只有单一作者、未引入 CLA；接收外部 PR 前请先约定“贡献可再许可”条款，
以保留将来整体切换许可（含闭源）的能力。

构建期引入的第三方组件（都不进仓库，产物按各自条款随 initramfs/ISO 分发；其许可不随本仓协议切换而变）：

| 组件 | 许可证 | 引入方式 |
|------|--------|----------|
| GNU Unifont | GPL-2.0-or-later（含字体嵌入例外） | `pack/make_font.sh` 生成 `$BUILD/screen/font.bin`，**作为独立数据文件**随 initramfs 分发（不嵌入 agent 二进制；来源与许可见包内 `/ventoy/ai/screen/LICENSE.unifont`） |
| busybox 1.36.1 | GPL-2.0-only | `pack/build_busybox.sh` 下载官方源码编译 |
| Linux 内核与模块（Ubuntu 发行版包） | GPL-2.0-only | `pack/fetch_kernel.sh` 下载并按清单裁剪 |
| libgooglepinyin | Apache-2.0 | `ime/build.sh` 静态编译 helper；条款全文见 [ime/LICENSE](ime/LICENSE) |
| e2fsprogs 1.47.0 | GPL-2.0-or-later / LGPL-2.1（libuuid） | `pack/build_tools.sh`（kernel.org 源码，SHA256 校验） |
| GNU parted 3.6 | GPL-3.0-or-later | 同上（libuuid 取自 e2fsprogs 源树，libblkid 静态链接宿主库） |
| rsync 3.2.7 | GPL-3.0-only | 同上（内置 popt/zlib 源码树） |
| GRUB（standalone 引导器） | GPL-3.0-or-later | `pack/make_iso.sh` 调宿主 `grub-mkstandalone` |

上述第三方组件与自研代码仅构成**聚合分发**（mere aggregation）：各自独立打包、独立加载，互不构成
衍生作品。其中字体数据已从 agent 二进制**外置**（`/ventoy/ai/screen/font.bin`，运行时按文件加载），
分发介质包含 copyleft 组件不会使自研代码被其许可覆盖。
