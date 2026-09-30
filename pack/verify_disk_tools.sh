#!/bin/sh
# guest 内实测「受守卫磁盘工具」+ 自动安装模板生成：list_disks / partition / format / backup / gen_autoinstall。
#
# 原理：mock LLM 走 disk 剧本，按顺序下发工具调用；agent 在 guest 内真正执行
#       内置的 parted/mke2fs/rsync。脚本模式下确认自动通过，因此无需人工交互。
# 靶盘：额外挂一块空白 virtio 盘（payload 是 /dev/vda，靶盘是 /dev/vdb）——
#       两盘都用 -device 挂载，靶盘带 serial=aitarget，供 gen_autoinstall 按序列号选盘。
#
# 用法: verify_disk_tools.sh [超时秒数]
# 环境变量: VTOY_AI_BUILD_DIR（默认见 defaults.sh）
# 退出码: 0 = 全部断言命中；1 = 有缺失或超时
set -e

. "$(dirname "$0")/defaults.sh"

BUILD="$VTOY_AI_BUILD_DIR"
PACK_DIR="$(cd "$(dirname "$0")" && pwd)"
TEST_DIR="$BUILD/test"
PAYLOAD_IMG="$TEST_DIR/payload.img"
# 靶盘用本脚本私有的文件名：本脚本每次运行都会 rm + truncate 它，而 test/scratch-disk.img 是
# run_qemu_usb.sh 的交互靶盘默认路径（AI 刚分好区的待测盘也在那里），同名会把待测靶盘清掉。
SCRATCH="$TEST_DIR/verify-scratch.img"
SCRIPT="$TEST_DIR/disk-script.txt"
LOG="$TEST_DIR/qemu-disk.log"
REQS="$TEST_DIR/disk-requests.log"
TIMEOUT="${1:-300}"
PORT="${VTOY_AI_MOCK_PORT:-18811}"

mkdir -p "$TEST_DIR"

# ---------- 1. 一句用户输入即可驱动整条剧本 ----------
cat > "$SCRIPT" <<'EOF'
请把这块空盘分区格式化，然后帮我把U盘里的数据备份过去
EOF

# ---------- 2. mock LLM（disk 剧本，请求体落盘便于核对工具结果）----------
if command -v ss >/dev/null 2>&1 && ss -ltn 2>/dev/null | grep -q ":$PORT "; then
    echo "[verify] 端口 $PORT 已被占用，请先停掉旧 mock" >&2
    exit 1
fi
rm -f "$REQS"
echo "[verify] 启动 mock LLM（disk 剧本，$PORT）..."
VTOY_MOCK_SCENARIO=disk VTOY_MOCK_LOG="$REQS" \
    python3 "$PACK_DIR/../tools/mock_openai_server.py" "$PORT" >"$TEST_DIR/disk-mock.log" 2>&1 &
MOCK_PID=$!
trap 'kill "$MOCK_PID" 2>/dev/null || true' EXIT INT TERM
sleep 1

# ---------- 3. payload 盘（脚本模式）+ 靶盘 ----------
echo "[verify] 造 payload 盘（ai.json 指向宿主 10.0.2.2:$PORT）..."
VTOY_AI_WITH_SCRIPT=1 VTOY_AI_SCRIPT_FILE="$SCRIPT" \
    VTOY_AI_BASE_URL="http://10.0.2.2:$PORT/v1" VTOY_AI_MODEL=mock-model \
    sh "$PACK_DIR/make_test_disk.sh" >/dev/null

rm -f "$SCRATCH"
# 靶盘要放得下 payload 的内容（vmlinuz+initrd 约 32MB），给 256M
truncate -s 256M "$SCRATCH"

# ---------- 4. 非交互启动 ----------
rm -f "$LOG"
echo "[verify] 启动 QEMU（payload=/dev/vda 靶盘=/dev/vdb，超时 ${TIMEOUT}s）..."
# 两个盘都用 -device：① 靶盘才能带 serial（gen_autoinstall 按它选盘，见 serial=aitarget）；
# ② 统一机制才能保证盘序 = 命令行顺序（混用 -drive if=virtio 与 -device 会颠倒盘序）。
# shellcheck disable=SC2086
timeout --foreground "$TIMEOUT" qemu-system-x86_64 \
    $VTOY_AI_QEMU_ACCEL \
    -m "$VTOY_AI_QEMU_MEM" -smp "$VTOY_AI_QEMU_SMP" \
    -kernel "$BUILD/kernel/vmlinuz" \
    -initrd "$BUILD/initrd-ai.cpio.gz" \
    -append "console=ttyS0 loglevel=3 rdinit=/init vtoy_ai=1" \
    -drive file="$PAYLOAD_IMG",format=raw,if=none,id=payload \
    -device virtio-blk-pci,drive=payload \
    -drive file="$SCRATCH",format=raw,if=none,id=scratch \
    -device virtio-blk-pci,drive=scratch,serial=aitarget \
    -netdev user,id=n0 -device virtio-net-pci,netdev=n0 \
    -display none -serial file:"$LOG" -monitor none -no-reboot \
    || true

# ---------- 5. 断言 ----------
FAILED=0
ok() { printf '  PASS  %s\n' "$1"; }
no() { printf '  FAIL  %s\n' "$1"; FAILED=$((FAILED + 1)); }

echo "[verify] 核对串口日志 $LOG（工具调用轨迹）:"
for t in list_disks partition format backup gen_autoinstall; do
    if grep -aq "→ $t" "$LOG"; then ok "调用了 $t"; else no "未调用 $t"; fi
done

echo "[verify] 结构化核对请求日志 $REQS（guest 回传给模型的工具结果）:"
python3 - "$REQS" <<'PY'
import json, sys

req = None
for ln in open(sys.argv[1], 'rb'):
    ln = ln.strip()
    if not ln:
        continue
    try:
        req = json.loads(ln)
    except Exception:
        pass
tools = [m for m in (req or {}).get('messages', []) if m.get('role') == 'tool']

fails = 0
def ok(cond, msg):
    global fails
    print(("  PASS  " if cond else "  FAIL  ") + msg)
    if not cond:
        fails += 1

def results(name):
    out = []
    for m in tools:
        if m.get('name') != name:
            continue
        try:
            out.append(json.loads(m.get('content', '')))
        except Exception:
            out.append({'raw': m.get('content', '')})
    return out

def first_ok(name):
    for r in results(name):
        if r.get('ok'):
            return r
    return {}

ld = results('list_disks')
ok(len(ld) >= 2, "list_disks 前后各执行一次")
first = ld[0].get('disks', []) if ld else []
ok(any(d.get('protected') for d in first), "list_disks 标出受保护设备")
ok(any('payload' in (d.get('protect_reason') or '') for d in first), "受保护原因指出 payload")
ok(any(d.get('name') == 'vdb' and not d.get('protected') for d in first), "空白靶盘 vdb 可操作")

part = first_ok('partition')
ok(part.get('action') == 'partition', "partition 返回 ok 结构化结果")
ok(any('/dev/vdb1' in d for d in part.get('devices', [])), "partition 报告新建 /dev/vdb1")
ok(any('mklabel' in s for s in part.get('steps', [])), "partition 展示将执行的 parted 命令")

fmt = first_ok('format')
ok(fmt.get('action') == 'format', "format 返回 ok 结构化结果")
ok('BK' in fmt.get('detail', ''), "format 报告卷标 BK")
ok(bool(fmt.get('output')), "format 带回 mke2fs 真实输出")
# 目标发行版的 e2fsck/GRUB（Ubuntu 22.04 = 1.46.5 / GRUB 2.06）读不了 orphan_file/metadata_csum_seed，
# 必须显式排除，否则装完的盘 grub-install 报 unknown filesystem、开机 fsck 失败进紧急模式。
ok(any('^orphan_file' in s and '^metadata_csum_seed' in s for s in fmt.get('steps', [])),
   "format 命令排除 orphan_file/metadata_csum_seed（目标发行版可读）")

bk = first_ok('backup')
ok(bool(bk.get('ok')), "backup 返回 ok 结构化结果")
botxt = '\n'.join((bk.get('output') or []) + [bk.get('detail', '')])
ok('files transferred' in botxt or 'Number of regular files transferred' in botxt,
   "rsync 带回传输统计")

last = ld[-1].get('disks', []) if ld else []
vdb1 = [p for d in last for p in d.get('partitions', []) if p.get('path') == '/dev/vdb1']
ok(bool(vdb1), "收尾 list_disks 能看到 /dev/vdb1")
ok(bool(vdb1) and vdb1[0].get('filesystem') == 'ext4', "vdb1 文件系统为 ext4")
ok(bool(vdb1) and vdb1[0].get('label') == 'BK', "vdb1 卷标为 BK")

# 硬守卫：剧本里对承载 payload 的 /dev/vda 的两次写操作都必须被拒绝
refused = [m for m in tools if '拒绝' in (m.get('content') or '')]
ok(len(refused) == 2, "对受保护设备 /dev/vda 的两次写操作都被拒绝（实际 %d）" % len(refused))
ok(all('/dev/vda' in (m.get('content') or '') for m in refused), "被拒的确实是 vda")
ok(any(d.get('protected') for d in last if d.get('name') == 'vda'), "vda 事后仍标记受保护")

# gen_autoinstall：模板必须按磁盘序列号选盘、且全文没有设备名（不硬编码 /dev/vdX）
ga = first_ok('gen_autoinstall')
ok(ga.get('action') == 'gen_autoinstall', "gen_autoinstall 返回 ok 结构化结果")
ok(ga.get('disk_ident_kind') == 'serial', "模板按磁盘序列号选盘（disk_ident_kind=serial）")
ok('serial: "aitarget"' in ga.get('template', ''), '模板含 serial: "aitarget"')
ok('/dev/' not in ga.get('template', ''), "模板全文不含设备名 /dev/")
ok('type: format, volume:' in ga.get('template', ''),
   "模板用 curtin 的 format.volume（不是 type: fs/device）")

# 回读模板文件（guest 端 fs_read 返回原文）：证明它真的落到了数据分区
reads = [m.get('content') or '' for m in tools if m.get('name') == 'fs_read']
ok(bool(reads), "回读了模板文件 autoinstall-e2e.yaml")
ok(bool(reads) and 'serial: "aitarget"' in reads[-1], "回读到按序列号选盘")
ok(bool(reads) and '/dev/' not in reads[-1], "回读内容不含设备名")

# 除上述预期拒绝外，不应有其它错误
unexpected = [m.get('name') for m in tools
              if '拒绝' not in (m.get('content') or '')
              and ('错误' in (m.get('content') or '') or 'error' in (m.get('content') or '').lower())]
ok(not unexpected, "无预期外的错误（实际: %s）" % (unexpected or '无'))

sys.exit(1 if fails else 0)
PY
CHECK_RC=$?
if [ "$CHECK_RC" -ne 0 ]; then
    FAILED=$((FAILED + 1))
fi

echo "[verify] 完成，日志: $LOG / $REQS"
if [ "$FAILED" -gt 0 ]; then
    echo "[verify] 失败 $FAILED 项" >&2
    exit 1
fi
echo "[verify] 全部通过"
