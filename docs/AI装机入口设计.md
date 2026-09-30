# AI 装机入口设计（设计稿）

> 路径与文档说明：文中 `AI/xxx` 即本仓根目录下的 `xxx`；`02-整体架构.md`、`07-插件系统.md` 等
> Ventoy 本体文档不在本仓。
> 下面的"尚未实现"是设计稿当时的状态，实际落地进度以仓库根 [README.md](../README.md) 为准，
> 验证结论与实测流程见 [测试.md](测试.md)。

> 状态：**设计草案，尚未实现**。本文不含代码改动，只定义方案、接口与落地步骤。
> 分析基线：`master` 分支提交 `6568972`；文中"现有机制"的行号均已核对到该基线。
> 前置阅读：`02-整体架构.md`（三阶段模型）、`07-插件系统.md`（ventoy.json / control / auto_install）。

---

## 1. 目标与非目标

### 1.1 目标

在 Ventoy 主菜单顶部增加一个 **AI 装机入口**：用户以自然语言问答的方式表达装机需求，由大模型驱动的 agent 完成"理解需求 → 探测硬件 → 生成无人值守配置 → 重启并执行安装"的编排。大模型的接入信息（URL / API Key / Model）通过 U 盘上的配置文件预置。

### 1.2 关键判断：AI 只做编排，执行交给 Ventoy 现有能力

**不做**"AI 直接对目标磁盘分区、格式化、解包安装"的默认路径。原因是：

- Ventoy 已经支持 100+ 发行版的无人值守安装（`auto_install` 插件 + kickstart / preseed / autoinstall / autounattend 模板），这套能力是现成的、被验证过的；
- 生成答录文件（文本）正是大模型最可靠的能力，且**失败代价低**——模板写错只是装不进去，不会毁数据；
- 而让 LLM 直接执行 `parted` / `mkfs` / `dd`，一旦幻觉就是数据灾难，且几乎无法在发布版中承担这个风险。

因此本设计把 AI 的产出**收敛为两类可审计的文本产物**：答录文件 + `ventoy.json` 编排条目，然后调用既有引导链执行。AI 直接操作磁盘仅作为默认关闭的专家模式（见 §9）。

### 1.3 非目标（本期不做）

- 自制发行版 / 自制桌面环境（见 §10 的取舍说明）；
- 多轮安装中实时监控安装进度（安装阶段由发行版自己的界面负责）；
- 云端服务、账号体系、遥测（AI 入口是纯本地工具，只与用户配置的模型端点通信）。

---

## 2. 设计原则

| 原则 | 落地方式 |
|------|----------|
| 复用优先，对核心零或最小侵入 | GRUB 侧只用现有 `linux`/`initrd`/`menuentry` 能力；C 代码零改动；`grub.cfg` 最多加一个固定菜单项（T3，见 §4） |
| AI 与执行分离 | AI 只写配置/模板文件，安装由 Ventoy 既有流程完成 |
| 一切写操作可审计、可回滚 | 写前 diff 预览 + 备份 `ventoy.json.bak`；全量日志落盘 |
| 默认安全 | 默认 `orchestrate` 模式（不碰目标盘）；高危操作需键入确认（typed confirm） |
| 与现有插件体系一致 | 配置放 `/ventoy/ai.json`（与 `ventoy.json` 同级、同盘、同目录约定）；复用 `control` / `auto_install` / `menu_alias` / `password` 插件语义 |

---

## 3. 总体架构

### 3.1 组件图

```
┌──────────────── 引导期（现有 GRUB2 + ventoy 模块，零 C 改动）────────────────┐
│  主菜单                                                                     │
│   ├─ [★ AI 装机助手]     ← 新增固定入口（T3，grub.cfg 插入 ~20 行）          │
│   ├─ ubuntu-24.04.iso    ← 既有动态列表（vt_list_img / vt_dynamic_menu）     │
│   └─ ...                                                                    │
└──────────────────────────────────┬──────────────────────────────────────────┘
        选择 AI 入口 → linux $vtoy_iso_part/ventoy/ai/vmlinuz + initrd
┌──────────────────────────────────▼──────────────────────────────────────────┐
│ AI 环境（精简 Linux，内存运行，不写目标盘）                                    │
│   init → 扫描 Ventoy 签名盘 → 挂载 part1 于 /iso → DHCP → 启动 agent          │
│   agent：OpenAI 兼容 API（读 /iso/ventoy/ai.json）                            │
│   工具集：system_probe / fs_read / fs_write / run_command / ask_user /        │
│           schedule_boot                                                     │
│   产物：/iso/ventoy/script/*.cfg|.xml（答录文件）                              │
│         /iso/ventoy/ventoy.json（control + auto_install 编排条目）            │
└──────────────────────────────────┬──────────────────────────────────────────┘
        schedule_boot：写 VTOY_DEFAULT_IMAGE + 备份 + 确认 → reboot
┌──────────────────────────────────▼──────────────────────────────────────────┐
│ 执行期（零新代码，全部复用现有能力）                                           │
│   GRUB 默认选中目标 ISO（VTOY_DEFAULT_IMAGE，ventoy_cmd.c:2572-2640）         │
│   → VTOY_MENU_TIMEOUT 倒计时后自动进入（grub.cfg:2554-2556）                  │
│   → auto_install 插件直取模板（autosel + timeout=-1，ventoy_cmd.c:3858-3874） │
│   → 发行版安装器无人值守执行                                                  │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 3.2 为什么 AI 不直接跑在 GRUB 里

GRUB2 没有 TLS/HTTPS POST、没有进程模型与 shell，而 agent 循环（多轮 tool-call → 执行 → 观察 → 再推理）必须跑在 Linux 用户态；硬件探测（`lsblk`/`lspci`/网络）也只有 Linux 能做。所以 **"AI 装机入口" = 一个精简 Linux 启动项 + 一个 agent 可执行文件**，GRUB 菜单项只是引导入口。

进而推出本设计的核心结构：**两阶段装机**

| 阶段 | 运行环境 | 职责 | 产出 |
|------|----------|------|------|
| 阶段一：AI 编排 | AI 环境（精简 Linux） | 问答澄清、探测硬件、选镜像、生成答录文件、写编排配置 | 文本产物 + 重启 |
| 阶段二：无人值守安装 | 现有 Ventoy 引导链 + 发行版安装器 | 按答录文件自动安装 | 装好的系统 |

阶段一是本设计要新建的全部内容；阶段二**不需要写任何新代码**。

---

## 4. 引导入口（顶置）设计

三种实现档位，按侵入性递增；建议 M1 用 T1 验证，M3 落 T3。

| 档 | 实现方式 | 改动 | 顶置效果 | 适用 |
|----|----------|------|----------|------|
| **T1** | AI 环境打包成普通 mini-ISO，拷贝到 part1，作为普通镜像启动 | 零代码 | 菜单按镜像名排序（`ventoy_cmd.c:3002`），文件名加前缀（如 `0-OS-PILOT-AI.iso`）+ `menu_alias` 改名 | M1 快速验证（**已实测通过**，见 4.4） |
| **T2** | 载荷放 `/ventoy/ai/` 目录，配一个 sidecar `.vcfg` 走 custom_boot 机制 | 零代码（配置级） | 菜单项名 = 占位文件名；占位文件须 > 32KB（`VTOY_FILT_MIN_FILE_SIZE`，`ventoy_def.h:30`） | M2 |
| **T3** | `grub.cfg` 在 `vt_list_img` 之后、主菜单生成之前插入固定 `menuentry` | `grub.cfg` +约 20 行 | **真正的固定顶置项**，支持多语言、按载荷存在性显示 | M3 推荐落地 |

### 4.1 T2 的现有机制依据

- `.vcfg` 旁挂脚本：镜像列表扫描时发现 `xxx.iso.vcfg` 会注册 custom_boot（`ventoy_cmd.c:2053-2061`）；选中该镜像时 `ventoy_vcfg_proc` 用 `configfile` 执行 vcfg 脚本代替默认引导（`grub.cfg:65-78`）；
- 显式映射：插件 `custom_boot` 的 `{"file"|"dir", "vcfg"}` 形式（`ventoy_plugin.c:1950-2021`、`:2928-2952`）。

即：**不需要改一行 C 代码，就能让一个自定义脚本接管某个菜单项的引导行为**（vcfg 里可以写 `linux` / `initrd` / `boot`）。

### 4.2 T3 的 grub.cfg 草案

插入位置：`INSTALL/grub/grub.cfg:2703`（`vt_list_img`）之后、`:2705`（主菜单生成）之前。

```
# ===== AI 装机助手入口（载荷存在时才显示）=====
if [ -f $vtoy_iso_part/ventoy/ai/vmlinuz -a -f $vtoy_iso_part/ventoy/ai/initrd ]; then
    menuentry "$VTLANG_AI_ASSISTANT" --class ventoy --class ai_entry {
        linux  $vtoy_iso_part/ventoy/ai/vmlinuz rdinit=/init vtoy_ai=1 loglevel=3
        initrd $vtoy_iso_part/ventoy/ai/initrd
    }
fi
```

要点与约束：

- **GRUB 能读 part1**：part1 支持的 FS（exFAT 由改造过的 `fat.c` 支持、NTFS、ext2/3/4、XFS、UDF 等，见 `GRUB2/MOD_SRC/grub-2.04/grub-core/fs/`）都能直接 `linux` 加载内核；
- **菜单顺序**：静态 `menuentry` 按脚本出现顺序排列，位于所有动态项之前；GUI 树状模式（`VTOY_DEFAULT_MENU_MODE=1`）下该静态项位于顶层；
- **多语言**：`$VTLANG_AI_ASSISTANT` 需要新增到 `LANGUAGES/` 与各 `menulang.cfg`（M3 工作）；MVP 可先硬编码中英文字符串；
- **Secure Boot**：内核由已签名的 `grubx64_real.efi` 加载，与普通 ISO 的加载路径一致，**不引入新的签名问题**（`05-引导原理-UEFI.md` §Secure Boot）；内核模块需过签名校验——已改用 Ubuntu 发行版内核 + 带 PKCS#7 签名的发行版模块（2026-09-28），lockdown 下应可正常校验（真机 Secure Boot 场景待实测）；屏幕帧缓冲不依赖 GPU 模块（内建 `simpledrm` 走 UEFI GOP）；
- **密码插件**：`password` 插件按镜像路径生效，覆盖不到固定入口。若配置了菜单密码，需要在入口内复用同样的校验逻辑（M3 细化，设计上留钩子）。

### 4.3 载荷存放位置的选择

| 位置 | 容量 | 可行性 |
|------|------|--------|
| part2（VTOYEFI，32MB，`VENTOY_SECTOR_NUM=65536`） | 固定 32MB，已被 GRUB/EFI 文件占据大半 | **不可行**（见 `03-磁盘布局与安装原理.md`） |
| **part1 `/ventoy/ai/`（推荐）** | 用户数据区，无容量约束 | 可行。但注意 part1 会被安装器格式化，**载荷只能由用户/发布包拷贝，不能由安装器预置**；发布包中附 `ventoy/ai/` 目录与说明即可 |

### 4.4 T1 实测记录（2026-09-28，x86_64 QEMU/OVMF，Ventoy 1.1.05）

实现产物：`pack/make_iso.sh`（GRUB standalone 引导器 + `/ventoy.dat` 标记，生成的 mini-ISO
约 32MB）。验证走**真实 Ventoy 菜单**（`pack/make_ventoy_testdisk.sh` 免 root 组装的完整
Ventoy 测试盘 + `pack/run_qemu_ventoy.sh`），两次启动：

| 轮次 | 观察点 | 结果 |
|------|--------|------|
| boot1 | 菜单顺序与倒计时 | `0-OS-PILOT-AI.iso` 排最前且为默认项，10s 倒计时后 `Booting '0-OS-PILOT-AI.iso'` |
| boot1 | agent 写 `ventoy.json` | 保留原 `theme` 键；`control` 合并为数组+字符串形式（旧键不丢）；`auto_install` 追加新项；原文件备份 `.bak` |
| boot2 | agent 写的默认项是否生效 | 菜单默认项变为 `ubuntu-24.04-desktop-amd64.iso`；guest `/proc/cmdline` 出现该副本的标记 `vtoy_ai_tag=ubuntu-copy` |

落地时必须带的两条配置约束（均为实测踩坑）：

1. **`control` 必须显式设 `VTOY_SECONDARY_BOOT_MENU="0"`**：1.1.05 默认值为 `"1"`
   （`ventoy.c:575` `g_ctrl_vars`），二级动作菜单（Normal/grub2/memdisk/…）**无倒计时**，
   会把无人值守启动卡住。替代：`VTOY_SECONDARY_TIMEOUT`（正常模式倒计时后继续）或
   文件名加 `_vtnormal` 后缀（`ventoy_check_mode_by_name`，仅对该镜像跳过动作菜单）。
2. **`ventoy.json` 不得出现负数**：`ventoy_json.c` 的 `vtoy_json_parse_value` 把 `-` 交给
   `grub_strtoul`，解析失败会使**整份配置失效**（GRUB 报 `unrecognized number.`，菜单都不出）。
   因此「不显示模板菜单」（原 `timeout: -1` 语义）改为**省略 `timeout` 键**——`ventoy_plugin.c`
   的 auto_install 插件默认值即 `-1`，`ventoy_cmd.c:3868` 依此自动选中模板项，语义等价。

---

## 5. AI 环境（精简 Linux）设计

### 5.1 组成与体积预算

| 组件 | 说明 | 体积量级 |
|------|------|----------|
| 内核 | Ubuntu 26.04 LTS 发行版 `vmlinuz`（7.0.0-34-generic，Canonical 签名） | 17 MB（bzImage） |
| 内核模块 | 按 `kernel-modules.list` 裁剪的 59 个模块（.ko 解压态，随 initramfs） | 18 MB（gzip 后约 4.3 MB） |
| initramfs | busybox（静态）+ init 脚本 + CA 证书 + 模块集 + 拼音输入法（helper + 词典） | 10 MB |
| agent | Go 静态二进制（含 TLS/JSON、自绘屏幕字体） | 8–15 MB |
| **合计** | | **约 35–45 MB** |

实装数据（2026-09-28）：vmlinuz 17 MB + initramfs 10.0 MB（含输入法组件 2.2 MB）⇒ mini-ISO 43.9 MB。

### 5.2 initramfs 内容树

```
/init                        # busybox sh：环境准备 + 加载内核模块 + 起 agent（见 5.3）
/bin /sbin /usr/bin          # busybox 静态（applet 软链）
/lib/modules/<kver>/         # 发行版内核裁剪模块集（.ko 解压态 + modules.dep 等索引）
/ventoy/ai/                  # mount_payload.sh / udhcpc.script / kernel-modules.list / ai.json.example
/ventoy/ai/ime/              # 拼音输入法：pinyin-ime（libgooglepinyin 静态封装）+ dict_pinyin.dat（1.02 MB）
/ventoy/ai/os-pilot-ai       # agent 静态二进制（点阵字体已外置，见下）
/ventoy/ai/screen/           # font.bin（GNU Unifont 点阵，独立数据文件）+ LICENSE.unifont
/etc/ssl/certs/ca-certificates.crt  # 模型端点 TLS 校验用
```

构建方式复用仓库内既有先例：cpio newc + gzip（`LiveCD/livecd.sh:42-47`；运行期 cpio 同样方式，见 `06-运行时环境与工具链.md`）。

### 5.3 init 流程

1. 挂载 `proc` / `sys` / `dev`，起 `udev` 等价物（busybox `mdev`）等待设备就绪；
2. **加载内核模块**：按随 initramfs 装入的 `/ventoy/ai/kernel-modules.list` 逐个 `modprobe`（存储/文件系统/网卡/输入/显示），随后等待块设备枚举完成；
3. **扫描 Ventoy 签名盘**：遍历块设备，按 MBR 偏移 `0x180` 的磁盘 UUID + `0x1B8` 签名识别目标 U 盘（同 Ventoy2Disk 的判定方式，见 `03-磁盘布局与安装原理.md`§磁盘身份识别）；找不到则给出提示并落到 shell（实装按"挂载后含 `/ventoy` 目录"判定，语义等价且更宽松）；
4. 挂载 part1 到 `/iso`（读写；exFAT/NTFS3 等文件系统由随包模块集提供，第 2 步已加载）；
5. 网络：`udhcpc` 自动获取；失败则进入 TUI 让用户选择"静态 IP / 跳过网络 / 退出"；**无网络时 AI 模式不可用**，需明确提示并可退回传统菜单路径；
6. 读取 `/iso/ventoy/ai.json`；若配置要求口令解密 Key，则在此弹出口令输入；
7. 启动 `ai-agent`（前台 TUI）；agent 正常退出或崩溃后回到"重启 / 关机 / shell"菜单。

### 5.4 内核来源

| 方案 | 优点 | 代价 |
|------|------|------|
| **已采用：Ubuntu 26.04 LTS 发行版内核**（`linux-image` + `linux-modules`，7.0.0-34-generic） | 新硬件覆盖好（7.0 内核）、安全更新可跟随发行版、Canonical 签名（模块带 PKCS#7 签名）、无需自维护配置 | 需在 initramfs 内携带裁剪模块集（59 个，解压后 18 MB）；legacy BIOS 下无 fb0 时回退文本控制台（真机图形依赖内建 simpledrm） |
| 备选：自编内核（M1–M3 原型曾用 6.6.157 全内建） | 完全可控、可裁剪、全内建 | 需自维护配置与升级；无签名（Secure Boot 受限）——2026-09-28 弃用，构建脚本已删除 |

### 5.5 网络与 TLS

- DHCP：busybox `udhcpc`；DNS 写 `/etc/resolv.conf`；
- TLS：由 Go 运行时的 `crypto/tls` + 内置 CA 完成，**不依赖 busybox wget/openssl**；
- 代理：支持 `ai.json` 中 `http_proxy` 字段（企业网络/本地网关场景）。

### 5.6 本地屏中文输入（内置拼音输入法，2026-09-28 实装）

自绘屏幕解决了中文**显示**，本地键盘仍只能送 ASCII ⇒ 真机（显示器 + 键盘）无法输入中文提问。
实装方案：把候选检索下沉到 initramfs 内的一个静态 helper，供自绘屏幕的行编辑器调用。

- **选型**：libgooglepinyin（Apache-2.0，库 251 KB + 词典 1.02 MB，全静态 helper 1.15 MB）。
  备选中 zhcon 可用但老旧（GPL-2.0+，6.9 MB）；libpinyin/sunpinyin/librime 依赖与体积过大；
  fcitx5/ibus 需要图形栈与 D-Bus，均不适配 initramfs。许可证全文随包（`ime/LICENSE`）。
- **协议**（helper `ime/pinyin-ime.cpp`，stdio 一行一请求 / TSV 应答）：
  `S\t<拼音>` → `C\t<n>\t<候选…>`（n≤9）；`A\t<i>` 选字（触发引擎侧学习）；`R` 复位；`Q` 退出；
  握手 `R\tready`；拼音字符集限 `[a-z']`（helper 侧过滤，防注入与脏输入）。
- **Go 侧集成**（`agent/internal/screen/ime.go`）：懒启动（首次 Ctrl-Space 才起进程）、
  握手 1s / 请求 500ms 超时（防 helper 卡死拖住 UI）、`imeEngine` 接口便于单测注入 fake；
  `endLine()` 在回车/Ctrl-C/Ctrl-D 时清组合与候选后缀，**后缀不落历史行**。
- **按键**：Ctrl-Space 切中/英（跨行保持）；组合态 a-z/`'` 进拼音，后缀显示"灰色拼音串 + 白色候选条"；
  `1-9` 选字、空格提交首选（无候选按原样上屏）、回车提交首选并送行、退格退拼音（退空退出组合）、
  Ctrl-U 连组合一起清、大写字母直通不参与组合。
- **降级**：`VTOY_AI_IME=0` 禁用；helper/词典缺失或握手失败 ⇒ 提示一行"输入法不可用"后永久降级
  （不再重试），其余功能与串口/脚本路径不受影响（横幅的输入法提示行只在自绘屏幕路径显示）。
- **边界**：用户词典写在 tmpfs（重启不保留）；>9 候选不翻页；无模糊音/云输入。

验证（2026-09-28）：QEMU 直启本地 VGA 控制台 + `sendkey` 模拟键盘，全链路通过——Ctrl-Space →
`bangwozhuangxitong` 实时候选条 → 数字选字 / 空格提交「帮我装系统」→ 回车送行 → 工具调用 +
mock 回复 → 请求体落盘确认中文进入模型请求 → 关输入法后 `exit` 正常关机；回归脚本
`pack/run_qemu_ime.sh`（含断言与截图）。同轮串口脚本模式回归 rc=0、Ventoy 菜单链路（新 ISO）rc=0。

---

## 6. Agent 设计

### 6.1 会话循环

```
用户输入 ──► LLM（chat/completions + tools）
                │
                ├─ 返回 tool_calls ──► 本地执行（确认闸门） ──► tool 结果回填 ──┐
                │                                                              │
                └─ 返回文本 ──► 展示给用户 ──► 等待下一轮输入 ◄────────────────┘
```

- 防失控上限为 `session.go` 内建常量（不是 ai.json 配置项）：单轮工具调用 `maxToolItersPerTurn = 20`
  超限停止本轮并报错；历史消息超过 `maxHistoryMessages = 80` 时按 `historyKeepTail = 60` 裁剪；
  会话轮数本身不设上限（每轮由用户输入驱动，不会自旋）；
- 用户随时可 `Ctrl-C` 退出到 shell；退出不丢现场（日志已落盘）。

### 6.2 模型接入（OpenAI 兼容协议）

只实现一套协议：`POST {base_url}/chat/completions`，带 `tools`（function calling）。可覆盖 DeepSeek、Qwen（DashScope 兼容模式）、Ollama、vLLM、OpenRouter、one-api 等网关。

- MVP 用非流式（实现简单、错误好处理）；流式作为后续优化；
- 严格校验响应中的 `tool_calls` 结构，非法 JSON 直接判错重试（最多 N 次），不猜测。

### 6.3 工具集

核心工具刻意保持窄（更安全、提示词更稳）；初版 6 个，后续扩充到 11 个（受守卫磁盘工具 4 个 + `gen_autoinstall`）：

| 工具 | 参数 | 风险级 | 约束 |
|------|------|--------|------|
| `system_probe` | 无 | 只读 | 返回 JSON：块设备（`lsblk`）、总线（`lspci`）、CPU/内存、网络状态、**U 盘镜像清单**（扫描 `/iso` 下的 iso/img/vhd 及大小） |
| `fs_read` | `path` | 只读 | 路径白名单前缀 `/iso/`；单文件读取上限（如 256KB） |
| `fs_write` | `path, content` | 写 | 仅允许 `/iso/ventoy/` 下；写前显示 diff；临时文件 + rename 原子写 |
| `run_command` | `cmd, timeout` | 分级 | 只读白名单（`lsblk/blkid/lspci/df/ip/...`）直接执行；其余需确认；黑名单命令（`mkfs*`/`dd`/`parted`写操作/`rm -rf /` 等）在非 direct 模式下直接拒绝 |
| `ask_user` | `question, options?` | 无 | 结构化提问（单选/多选/自由文本），返回用户选择 |
| `schedule_boot` | `image, template?, timeout_sec?` | 高危 | 组合动作：备份 `ventoy.json` → 写入 `control.VTOY_DEFAULT_IMAGE` 与 `auto_install` 条目 → typed confirm → `reboot` |
| `list_disks` | 无 | 只读 | 枚举整盘/分区（容量/型号/总线/分区表/文件系统/卷标/UUID/挂载点）；`protected=true` 表示承载运行环境或已挂载，禁止改动 |
| `partition` | `disk, table?, partitions[]` | 高危 | 只接受整盘；受保护盘拒绝；parted 命令由代码拼装（参数白名单）；orchestrate 下逐字输入盘名确认 |
| `format` | `device, fstype, label?` | 高危 | ext2/3/4（内置 e2fsprogs，显式排除 orphan_file/metadata_csum_seed）与 vfat（内置 `mkfs.vfat`，供 ESP）；已挂载/敏感盘拒绝；逐字输入设备名确认 |
| `backup` | `src?, dst, delete?, dry_run?` | 写 | rsync 同步；拒写承载 payload/根的盘（含同一物理盘）；`--delete` 要求 `dst` 是挂载点；确认后执行 |
| `gen_autoinstall` | `image, disk, partitions[], output?, username?, hostname?, password_hash?, locale?, timezone?` | 写 | 按 curtin schema 确定性拼装（选盘用 serial、挂载用卷标）；只写 payload_dir 内；镜像须存在、目标盘须非 `protected` |

设计说明：

- **答录文件**：Ubuntu autoinstall 由 `gen_autoinstall` 确定性生成（选盘用序列号、挂载用卷标；手写模板易出现硬编码设备名与非法字段，已被提示词禁止）；其它发行版的 kickstart/preseed 仍可用 `fs_write` 写到 payload_dir 内（人类可读、可手工修改的产物）；
- `schedule_boot` 是唯一触碰"重启"的工具，且必须二次确认（§6.4），保证 AI 不可能静默重启机器。

### 6.4 确认闸门与安全策略

三档运行模式，配置在 `ai.json.mode`：

| 模式 | fs_write | run_command | schedule_boot | 适用 |
|------|----------|-------------|---------------|------|
| `readonly` | 拒绝 | 只读白名单 | 拒绝 | 只咨询/规划（如"我这台机器能装什么"） |
| **`orchestrate`（默认）** | 允许（限 `/iso/ventoy/`，需确认） | 只读白名单；其他需确认 | 允许（需 typed confirm） | 正常 AI 装机 |
| `direct` | 允许 | 全部允许（黑名单需 typed confirm） | 允许 | 专家直操（§9，默认关闭） |

强制规则（所有模式）：

1. **写前 diff**：任何 `fs_write` 先展示目标路径与 diff，用户确认后才落盘；
2. **typed confirm**：高危操作要求用户键入指定词（如目标设备名或 `ERASE`），不接受 `y` 连打；
3. **可回滚**：改 `ventoy.json` 前自动备份为 `ventoy.json.bak`；agent 提供一个"撤销上次编排"操作（恢复备份）；
4. **不猜设备**：向用户确认目标盘时，必须列出探测到的设备及容量，由用户选择，禁止模型自行推断设备名。

### 6.5 日志与可追溯

- 原始日志：`/iso/ventoy/ai/logs/session-<时间戳>.jsonl`（每轮请求、工具调用、结果、耗时）；
- 可读摘要：同目录 `.md`（对话 + 关键动作 + 最终编排内容）；
- 日志只落 U 盘，不上传任何第三方（模型端点通信除外）；
- **日志中出现的 API Key 需打码**。

### 6.6 系统提示词要点

1. 语言跟随 `ai.json.language`（默认 `zh-CN`）；
2. 先探测后结论：涉及硬件/镜像的事实必须来自 `system_probe` 结果，不得臆造；
3. 澄清优先：一次性提出最关键的 1–3 个问题（用 `ask_user`），不要连环追问；
4. 模板必须正确：生成的答录文件必须符合对应发行版文档的 schema（如 Ubuntu autoinstall、RHEL kickstart）；
5. 破坏性操作必须显式说明后果并等待确认；
6. 绝不自行调用 `schedule_boot`（必须先获得用户对该次重启的明确同意）；
7. 失败重试上限：同一工具连续失败 2 次即停止并汇报，不得反复试探。

---

## 7. 配置文件 `/ventoy/ai.json`

与 `ventoy.json` 同级、同盘（`/ventoy/` 目录，必须在 part1——错误放置的提示逻辑与现有文件一致，见 `grub.cfg:2659-2667`）。**独立文件而非塞进 ventoy.json**：不污染插件配置、便于单独备份/分享（分享时可去掉 Key 字段）。

### 7.1 Schema

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `provider` | string | `"openai"` | 协议类型，当前仅 OpenAI 兼容 |
| `base_url` | string | — | 如 `https://api.deepseek.com/v1`、`http://192.168.1.10:11434/v1`（Ollama） |
| `model` | string | — | 模型名，如 `deepseek-chat`、`qwen2.5:14b` |
| `api_key` | string | `""` | 空则启动时提示输入（不落盘） |
| `api_key_enc` | string | — | 口令加密的 Key（与 `api_key` 二选一，见 7.3） |
| `temperature` | number | `0.2` | 低温度更适合工程任务 |
| `max_tokens` | int | `262144` | 输出上限（**含思考**）；思考型模型下过小会让正文被截断。本地 llama.cpp 可设很大，云端端点须按其上限（超限会被拒）|
| `language` | string | `zh-CN` | 交互语言 |
| `mode` | string | `orchestrate` | `readonly` / `orchestrate` / `direct` |
| `http_proxy` | string | — | 可选代理 |
| `request_timeout` | int | `60` | 秒 |
| `log_dir` | string | `/iso/ventoy/ai/logs` | 日志目录 |

### 7.2 示例

```json
{
    "provider": "openai",
    "base_url": "https://api.deepseek.com/v1",
    "api_key": "sk-xxxxxxxxxxxxxxxx",
    "model": "deepseek-chat",
    "temperature": 0.2,
    "max_tokens": 262144,
    "language": "zh-CN",
    "mode": "orchestrate"
}
```

### 7.3 API Key 的三档策略（U 盘是可移动介质，必须向用户说明风险）

| 档 | 做法 | 安全性 | 便利性 |
|----|------|--------|--------|
| A（默认） | 明文写在 `ai.json` | 低（拿到 U 盘即拿到 Key） | 最高 |
| B | `api_key_enc`：口令 AES 加密，启动时输口令解密 | 中 | 中 |
| C | 不写 Key，启动时输入，仅存内存 | 高 | 每次输入 |

补充建议：**优先使用局域网自建端点**（Ollama / vLLM / one-api），此类 Key 泄露后果有限；文档中明确提示不要把手握高权限的云端 Key 长期放在 U 盘。

---

## 8. 编排层：与 ventoy.json 的联动（本设计的落地关键）

AI 的最终产物全部体现在 `ventoy.json` 中，完全复用现有插件语义，**GRUB 侧零改动即可实现"重启后自动进入无人值守安装"**。

### 8.1 答录文件 + auto_install

AI 把答录文件写到 `/ventoy/script/`，然后向 `ventoy.json` 增加：

```json
{
    "auto_install": [
        {
            "image": "/ISO/ubuntu-24.04.iso",
            "template": "/ventoy/script/ubuntu-autoinstall.yaml",
            "autosel": 1,
            "timeout": -1
        }
    ]
}
```

- `autosel`：1 起、指向 `template` 数组第 N 项；
- `timeout: -1`：**不显示模板选择菜单**，直接使用 `autosel` 项（`ventoy_cmd.c:3858-3874`：`node->timeout < 0` 时直接 `goto load`；参数校验见 `ventoy_plugin.c:662-680`）。
- 落地前需实测确认 `autosel`/`timeout` 的边界语义（`autosel=0` 的行为按代码存在歧义），M2 首个实验项。

#### 8.1.1 模板内容：按序列号选盘、按卷标挂载（不要硬编码设备名）

`/dev/vdX`、`/dev/sdX` 都不稳定：QEMU 里换个 `-drive` 接口就变，真机上受接口/端口/BIOS 顺序/U 盘在位与否影响，
不同内核命名空间还不一样（`sd`/`vd`/`nvme`/`mmcblk`）。因此**答录文件里不写设备名**：

- **选盘用磁盘序列号**：curtin 的 `type: disk` 支持 `serial` 或 `path`（`match` 亦可用 `model`/`path`/`serial`/
  `ssd`/`size`/`install-media`）——**没有按 UUID 选盘**，因为空白盘还没有文件系统、也就没有 UUID。
  序列号读自 `/sys/block/<dev>/serial`（virtio-blk 由 QEMU `serial=` 提供）或 `<dev>/device/wwid`（SATA/SCSI，
  新内核已移除 `device/serial`）。
- **挂载用卷标（label）**：`/etc/fstab` 里用 `UUID=`/`LABEL=` 才是标准做法（这属于"装完之后"，与"选盘"是两件事）。
- **字段名必须符合 curtin schema**：`disk`（用 serial/path）→ `partition`（`device: <盘 id>`）→
  `format`（**`volume: <分区 id>`**）→ `mount`（`device: <format id>` + `path`）。常见错法是把 `format` 写成
  `type: fs` + `device:`，或臆造 `fstab_id` 之类的键——都会让安装器报错或行为未定义。

**实现方式：生成必须走确定性工具 `gen_autoinstall`**（`agent/internal/disk/autoinstall.go` + 工具薄封装），
由代码按上面的 schema 拼装 storage 段；模型只提供镜像路径、目标盘（用来取序列号）与分区方案（size/label/mount/fs）。
不让模型手写 YAML，是因为手写必然重新引入硬编码设备名与幻觉字段。生成示例：

```yaml
#cloud-config
autoinstall:
  version: 1
  storage:
    version: 1
    config:
      - {id: disk0, type: disk, ptable: gpt, serial: "aitarget", wipe: superblock, grub_device: true}
      - {id: part1, type: partition, device: disk0, number: 1, size: 512M, flag: boot}
      - {id: part1-fmt, type: format, volume: part1, fstype: vfat, label: "boot-efi"}
      - {id: part1-mnt, type: mount, device: part1-fmt, path: /boot/efi}
      - {id: part2, type: partition, device: disk0, number: 2, size: -1}
      - {id: part2-fmt, type: format, volume: part2, fstype: ext4, label: "root"}
      - {id: part2-mnt, type: mount, device: part2-fmt, path: /}
```

- 取不到序列号时退回 `path:` 并在结果里给 warning（安全但不够稳）；真机上建议核对安装器是否认该盘的 serial。
- 测试靶盘带 `serial=aitarget`（`VTOY_AI_SCRATCH_SERIAL`），所以 QEMU 与真机走同一条"按序列号"的路径。
- **测试环境注意**：`autoinstall` 只被 Subiquity 系安装器支持；`ubuntu-22.04.*-desktop` 用的是 Ubiquity
  （GRUB 里是 `maybe-ubiquity`/`only-ubiquity`），**不会执行 autoinstall**——需换成 Ubuntu Server 或 24.04+
  的 Desktop ISO 才能验证这条链路。

### 8.2 重启后自动进入安装

`control` 插件两条配置组合（均为现有能力）：

```json
{
    "control": [
        { "VTOY_DEFAULT_IMAGE": "/ISO/ubuntu-24.04.iso" },
        { "VTOY_MENU_TIMEOUT": 10 }
    ]
}
```

- `VTOY_DEFAULT_IMAGE`：菜单默认选中该镜像（列表/树两种模式都支持，`ventoy_cmd.c:2572-2640` `ventoy_set_default_menu`）；
- `VTOY_MENU_TIMEOUT`：主菜单倒计时秒数（`grub.cfg:2554-2556`）；**设 10 秒是刻意留出的"中止窗口"**——用户看到倒计时仍可取消，符合"AI 不得静默操作"原则；不设置则 `timeout=0` 立即进入。
- 补充：`VTOY_DEFAULT_IMAGE` 还支持 `F6>/path` 修辞直接把某个热键动作当作默认（`grub.cfg:2728-2758`），本设计不用，仅记录。

### 8.3 完整时序（一次 AI 装机）

```
1. 用户开机 → GRUB 主菜单 → 选择 [★ AI 装机助手]
2. AI 环境启动（约 5–10 秒）→ 扫描并挂载 Ventoy 盘 → DHCP
3. agent 启动，读取 ai.json，显示欢迎语
4. 用户："帮我把 Ubuntu 24.04 装到这台机器的 NVMe 上，全盘、中文、用户名 foo"
5. agent → system_probe（探测磁盘/内存/镜像清单）
6. agent → ask_user："确认目标盘为 /dev/nvme0n1（1TB）？会清空全部数据"
7. agent → fs_write：生成 /ventoy/script/ubuntu-autoinstall.yaml（展示 diff + 确认）
8. agent → fs_write：更新 ventoy.json（备份 .bak；展示 diff + 确认）
9. agent → schedule_boot：展示"将重启并开始安装"摘要 → typed confirm
10. 重启 → GRUB 默认选中 Ubuntu ISO → 10 秒倒计时 → 自动进入
11. auto_install 直取模板 → Ubuntu 无人值守安装开始
12. （安装完成）系统重启进入已安装的 Ubuntu
```

### 8.4 撤销

用户后悔时：把 `/iso/ventoy/ventoy.json.bak` 覆盖回 `ventoy.json` 即可（U 盘插到任意电脑上操作都行）；agent 内的"撤销上次编排"按钮做同一件事。

---

## 9. 专家直操模式（`direct`，默认关闭）

允许 agent 通过 `run_command` 直接对目标盘操作（分区、格式化、`debootstrap`、`dd` 写盘等）。

- 开启方式：`ai.json.mode = "direct"`（显式修改配置，不是 TUI 里点一下就能开）；
- 强制规则：黑名单命令逐条 typed confirm；执行前输出"将要发生什么"的解释性摘要；全程日志；
- 定位：面向高级用户的逃生通道（例如无人值守模板覆盖不到的场景），**不是主路径**，文档需显著标注风险。

---

## 10. 备选形态：持久化 AI 工作台（vDisk）——独立产品线，M4 可选

用户设想的"精简版桌面/命令行系统，内置 AI CLI"属于另一个目标：**便携 AI 工作台**（聊天记录、配置持久化），而不是装机助手。建议做法：

| 方案 | 说明 | 代价 |
|------|------|------|
| 推荐 | 直接用现成轻量发行版（Alpine / Debian minimal + 预装 agent），做成 **vDisk**，由 Ventoy 引导（`vtoyboot` 持久化） | 镜像 1–2 GB、启动慢；需要跟随上游维护 |
| 不推荐 | 自制精简桌面发行版 | 驱动/固件/升级维护成本极高，远超收益 |

结论：先把 M1–M3 的"装机助手"做完；工作台作为独立里程碑评估。

---

## 11. 目录骨架与改动清单

### 11.1 新增目录

```
AI/
├── README.md                 # 本环境的使用与构建说明
├── agent/                    # Go 源码（静态二进制）
│   ├── main.go               # TUI 会话循环
│   ├── llm/client.go         # OpenAI 兼容客户端（含 tools/function calling）
│   ├── tools/                # system_probe / fs_read / fs_write / run_command /
│   │                         # ask_user / schedule_boot 实现
│   ├── session/              # 会话状态、确认闸门、日志
│   └── build.sh              # CGO_ENABLED=0 交叉编译
├── init/                     # initramfs 骨架（init 脚本 + busybox 清单 + CA）
├── pack/
│   └── pack_ai_env.sh        # 内核 + initrd + agent → ISO / ventoy/ai 目录
└── config/
    ├── ai.json.example
    └── prompts/system.zh-CN.md
```

### 11.2 对现有文件的改动清单

| 文件 | 改动 | 阶段 |
|------|------|------|
| `AI/**` | 新增（上表） | M1 |
| `INSTALL/grub/grub.cfg` | 约 2704 行后插入固定 menuentry（T3，§4.2） | M3 |
| `LANGUAGES/**`、`INSTALL/grub/menulang.cfg` 等 | 新增 `VTLANG_AI_ASSISTANT` 文案 | M3 |
| `INSTALL/ventoy_pack.sh`、`.github/workflows/ci.yml` | 发布包内附带 `AI/` 产物（`ventoy-ai-<ver>.iso` 或 `ventoy/ai/` 目录） | M3 |
| `INSTALL/plugin/ventoy/ventoy.json`（示例） | 增加 `menu_alias` 示例（可选） | M3 |
| GRUB 模块 C 代码 | **无改动** | — |

---

## 12. 构建与打包集成

| 步骤 | 做法 | 仓库内同类先例 |
|------|------|----------------|
| agent 编译 | `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w"`（arm64 同理） | — |
| initramfs 打包 | cpio newc + gzip | `LiveCD/livecd.sh:42-47`；运行期 cpio 见 `06-运行时环境与工具链.md` |
| ISO 打包（T1） | `xorriso -as mkisofs ...`（可裁剪掉 hybrid MBR 部分） | `LiveCD/livecd.sh:74-80` |
| 发布集成 | `AI/pack/pack_ai_env.sh` 产出物加入发布包；CI 增加对应 artifact | `INSTALL/ventoy_pack.sh`、`.github/workflows/ci.yml` |

注意：**不改动 Ventoy2Disk 的磁盘布局**——part1 是用户数据区，AI 载荷以普通文件形式存在，与 ISO 文件同级。

---

## 13. 里程碑与验收标准

| 里程碑 | 交付物 | 验收标准 | 量级估计 |
|--------|--------|----------|----------|
| **M1 环境可跑** | AI 环境 ISO（内核+busybox+agent）+ `ai.json` + T1 菜单呈现 | QEMU 中：启动 → DHCP → 与真实模型端点完成一轮问答；`system_probe` 正确输出块设备与镜像清单；日志落盘 | 1–2 周 |
| **M2 编排闭环** | `fs_write`/`ask_user`/`schedule_boot`、确认闸门、diff、备份/撤销 | QEMU 全流程：AI 生成 Ubuntu autoinstall → 重启自动进入 → 无人值守装完；期间所有写操作有 diff 与确认记录 | 2–3 周 |
| **M3 产品化** | T3 顶置入口、多语言、发布包集成、`readonly`/`direct` 模式、Key 加密档、arm64（可选） | 真实 U 盘 + 真实机器完成一次端到端装机；发布包中带 AI 产物；文档齐全 | 2 周 |

（工作量是单人开发的量级估计，供排期参考。）

---

## 14. 风险与对策

| 风险 | 影响 | 对策 |
|------|------|------|
| LLM 幻觉：答录文件内容错误 | 安装失败 | 模板 schema 校验；diff 确认；失败只损失一次重启 |
| LLM 幻觉：选错目标盘 | 数据丢失 | 默认不碰目标盘；目标盘必须由用户确认；typed confirm |
| API Key 落在可移动介质 | 泄露 | §7.3 三档策略；优先局域网端点；文档警告 |
| 无网络环境 | AI 不可用 | 明确提示并保留传统菜单路径；支持局域网端点 |
| 模型/端点行为漂移（不同模型 function calling 差异） | 会话卡住 | 严格结构校验 + 重试上限；日志可复盘；提示词收敛工具使用 |
| 成本与延迟 | 体验差 | `max_tokens` 上限 + 单轮工具调用内建上限（20）；日志记录 token 用量（如端点返回） |
| 内核 GPL 源码义务 | 合规 | 发布时附内核来源与获取方式（Ubuntu 26.04 LTS 官方内核包归档地址）；模块裁剪清单 `kernel-modules.list` 随仓库发布 |
| 第三方 CLI 许可证 | 合规 | **不打包任何第三方 AI CLI**，agent 自研（薄客户端，见 §6） |
| 维护面扩大 | 长期成本 | GRUB C 代码零改动；新增内容集中在 `AI/` 目录，可独立迭代/移除 |

---

## 15. 未决问题（待拍板）

| # | 问题 | 选项 | 建议 |
|---|------|------|------|
| 1 | agent 实现语言 | Go / Rust | **Go**：交叉编译单静态二进制最省事，TLS/JSON 标准库齐备 |
| 2 | 入口形态与节奏 | T1→T3 / 直接 T3 | **M1 用 T1 验证，M3 落 T3** |
| 3 | 内核来源 | 发行版预编译 / 自编 | **发行版预编译**（Ubuntu 26.04 LTS 7.0.0-34-generic；2026-09-28 由自编 6.6.157 切换，自编脚本已删除） |
| 4 | Key 默认档 | A 明文 / B 加密 / C 每次输入 | **A + 文档警告**，B/C 作为选项 |
| 5 | 是否一并做 arm64 | 是 / 否 | M3 可选（x86_64 先行） |
| 6 | vDisk 持久化工作台 | 做 / 不做 | 独立产品线，M4 再评估（§10） |
| 7 | 交付形态 | 并入 Ventoy 发行包 / 独立发行 | 独立目录 + 发布包附带，便于剥离 |

---

## 16. 参考（仓库内文件:行号，基线 6568972）

| 机制 | 位置 |
|------|------|
| 插件总表 / control / auto_install / custom_boot | `ventoy_plugin.c:2401`、`:113`、`:632-680`、`:1950-2021`、`:2928-2952` |
| 默认镜像选中（VTOY_DEFAULT_IMAGE） | `ventoy_cmd.c:2572-2640` |
| auto_install 直取模板（autosel + timeout<0） | `ventoy_cmd.c:3858-3874` |
| 镜像排序（按名） | `ventoy_cmd.c:3002` |
| `.vcfg` 旁挂注册与执行 | `ventoy_cmd.c:2053-2061`、`grub.cfg:65-78` |
| 菜单超时 | `grub.cfg:2554-2556`、`:2728-2758` |
| 主菜单构建 / 固定项插入点 | `grub.cfg:2701-2711` |
| 用户自定义菜单（ventoy_grub.cfg） | `grub.cfg:93-103` |
| 文件大小过滤（32KB） | `ventoy_def.h:30` |
| cpio / ISO 打包先例 | `LiveCD/livecd.sh:42-47`、`:74-80`；`LiveCD/README.txt:28-33` |
| 磁盘签名判定 | `Ventoy2Disk/Ventoy2Disk/PhyDrive.c`、`INSTALL/tool/ventoy_lib.sh`（详见 `03-磁盘布局与安装原理.md`） |
