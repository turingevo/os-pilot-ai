#!/usr/bin/env python3
"""本地屏交互端到端回归：Tab 模式切换 / ↑↓ 命令历史 / 固定输入行（QEMU + 点阵字体 OCR）。

流程（与 pack/run_qemu_ime.sh 同一套路，只是改用 QMP 取屏并按构建期字体做确定性 OCR）：
  1. 造交互式 payload 盘（VTOY_AI_WITH_SCRIPT=0，ai.json 指向宿主机 10.0.2.2:18800）
  2. 端口空闲时起 mock LLM（18800）；已在跑则直接复用
  3. 起 QEMU：本地 VGA 作 console（console=tty0 在最后）+ QMP unix socket
  4. QMP send-key 驱动键盘，screendump 抓帧 → 用 $BUILD/screen/font.bin（与 guest 同源）逆查字形做 OCR 断言
  5. 断言：状态栏/提示符随 Tab 切换、内容滚动不影响输入行与状态栏、↑↓ 历史回放、
     历史回车执行、find 放行、AI 与命令历史互不串

用法: run_qemu_ui.py [截图目录]
环境变量:
  VTOY_AI_BUILD_DIR  构建目录（默认见 pack/defaults.sh）
  VTOY_UI_BOOT_WAIT  等界面出现的秒数上限（默认 120）
退出码: 0 = 全部断言通过；1 = 有失败或超时
"""
import hashlib
import json
import os
import shutil
import socket
import struct
import subprocess
import sys
import time
from pathlib import Path

SH = Path(__file__).resolve().parent
AI_DIR = SH.parent
CELL_W, CELL_H = 8, 16
INK = 120  # 三通道之和阈值：白765/青510/绿510/灰474 算墨；状态栏底(0,0,96)不算
STATUS_BG = (0, 0, 96)  # screen.statusBg = 0x000060
# 字符 → QKeyCode（避免需要 shift 的符号，只用字母/数字/空格/斜杠/短横/点）
QC = {' ': 'spc', '/': 'slash', '-': 'minus', '.': 'dot'}
for _c in 'abcdefghijklmnopqrstuvwxyz0123456789':
    QC[_c] = _c


def build_dir() -> Path:
    # VTOY_AI_PACK_DIR：sh -c 里 $0 不带路径，defaults.sh 无法自查所在目录（用户配置在同目录）
    env = {**os.environ, 'VTOY_AI_PACK_DIR': str(SH)}
    out = subprocess.run(['sh', '-c', f'. "{SH}/defaults.sh"; printf %s "$VTOY_AI_BUILD_DIR"'],
                         capture_output=True, text=True, check=True, env=env).stdout.strip()
    if not out:
        raise SystemExit('无法解析 VTOY_AI_BUILD_DIR（检查 pack/defaults.sh）')
    return Path(out)


def qemu_args() -> list:
    """QEMU 加速与资源参数（-accel/-m/-smp），取自 defaults.sh，与 sh 脚本同源。"""
    cmd = ('. "{}/defaults.sh"; '
           'printf "%s\\n" "$VTOY_AI_QEMU_ACCEL" "$VTOY_AI_QEMU_MEM" "$VTOY_AI_QEMU_SMP"').format(SH)
    env = {**os.environ, 'VTOY_AI_PACK_DIR': str(SH)}
    out = subprocess.run(['sh', '-c', cmd],
                         capture_output=True, text=True, check=True, env=env).stdout.splitlines()
    if len(out) != 3:
        raise SystemExit('无法解析 QEMU 资源默认值（检查 pack/defaults.sh）')
    accel, mem, smp = out
    return accel.split() + ['-m', mem, '-smp', smp]


class Font:
    """点阵字体（$BUILD/screen/font.bin，与 guest 内 /ventoy/ai/screen/font.bin 同源）的只读视图，用于把屏幕像素反查成文字。"""

    def __init__(self, path: Path):
        data = path.read_bytes()
        if data[:4] != b'VTF1':
            raise SystemExit(f'字体格式不正确: {path}')
        count = struct.unpack_from('<I', data, 4)[0]
        base = 8 + count * 12
        self.narrow, self.wide = {}, {}
        for i in range(count):
            cp, off, flags = struct.unpack_from('<III', data, 8 + i * 12)
            bits = data[base + off: base + off + 32]
            rows = [(bits[r * 2] << 8) | bits[r * 2 + 1] for r in range(16)]
            if flags & 1:
                self.wide.setdefault(tuple(rows), chr(cp))
            else:
                self.narrow.setdefault(tuple(v >> 8 for v in rows), chr(cp))

    def decode(self, im):
        px = im.load()
        w, h = im.size
        rows = []
        for y in range(h // CELL_H):
            line, x = [], 0
            cols = w // CELL_W
            while x < cols:
                if x + 1 < cols:
                    key = tuple(self._bits(px, x, y, 16, r) for r in range(CELL_H))
                    ch = self.wide.get(key)
                    if ch:
                        line.append(ch)
                        x += 2
                        continue
                line.append(self.narrow.get(
                    tuple(self._bits(px, x, y, 8, r) for r in range(CELL_H)), '?'))
                x += 1
            rows.append(''.join(line).rstrip())
        return rows

    @staticmethod
    def _bits(px, x, y, width, r):
        v = 0
        for c in range(width):
            R, G, B = px[x * CELL_W + c, y * CELL_H + r]
            v = (v << 1) | (1 if R + G + B > INK else 0)
        return v


class Qmp:
    def __init__(self, path: Path, timeout=30):
        self.s = socket.socket(socket.AF_UNIX)
        t0 = time.time()
        while True:
            try:
                self.s.connect(str(path))
                break
            except OSError:
                if time.time() - t0 > timeout:
                    raise SystemExit('QMP 连接超时')
                time.sleep(0.3)
        self.buf = b''
        self._read()
        self.cmd({'execute': 'qmp_capabilities'})

    def _read(self):
        while b'\n' not in self.buf:
            d = self.s.recv(65536)
            if not d:
                raise EOFError('QMP 关闭')
            self.buf += d
        ln, self.buf = self.buf.split(b'\n', 1)
        return json.loads(ln)

    def cmd(self, obj):
        self.s.sendall((json.dumps(obj) + '\n').encode())
        while True:
            m = self._read()
            if 'return' in m or 'error' in m:
                return m

    def key(self, name):
        self.cmd({'execute': 'send-key', 'arguments': {'keys': [{'type': 'qcode', 'data': name}]}})

    def key_with(self, mod, name):
        """带修饰键（ctrl/shift/alt）的组合，例如 Ctrl+T、Ctrl-U。"""
        self.cmd({'execute': 'send-key',
                  'arguments': {'keys': [{'type': 'qcode', 'data': mod},
                                         {'type': 'qcode', 'data': name}]}})

    def type(self, text):
        for ch in text:
            self.key(QC[ch])
            time.sleep(0.06)

    def shot(self, ppm: Path):
        r = self.cmd({'execute': 'screendump', 'arguments': {'filename': str(ppm)}})
        if 'error' in r:
            raise SystemExit(f'screendump 失败: {r}')

    def wait_stable(self, ppm: Path, timeout=120, gap=1.5):
        """等画面连续两次抓帧一致（流式输出结束）。"""
        last, stable, t0 = None, 0, time.time()
        while time.time() - t0 < timeout:
            self.shot(ppm)
            md5 = hashlib.md5(ppm.read_bytes()).hexdigest()
            if md5 == last:
                stable += 1
                if stable >= 2:
                    return
            else:
                stable, last = 0, md5
            time.sleep(gap)


class Ui:
    def __init__(self, qmp: Qmp, shots: Path, font: Font):
        self.q = qmp
        self.shots = shots
        self.font = font
        self.fails = []

    def capture(self, tag: str, note=''):
        ppm = self.shots / f'{tag}.ppm'
        self.q.shot(ppm)
        im = self._image(ppm)
        (self.shots / f'{tag}.png').unlink(missing_ok=True)
        im.save(self.shots / f'{tag}.png')
        rows = self.font.decode(im)
        self.rows = rows
        self.status = rows[-1] if rows else ''
        self.input_row = rows[-2] if len(rows) > 1 else ''
        self.content = '\n'.join(rows[:-2])
        if note:
            print(f'  [{tag}] 输入行={self.input_row!r} 状态栏={self.status.strip()!r} {note}')
        return self

    @staticmethod
    def _image(ppm: Path):
        from PIL import Image
        return Image.open(ppm).convert('RGB')

    def has_status_bg(self, ppm: Path) -> bool:
        from PIL import Image
        im = Image.open(ppm).convert('RGB')
        w, h = im.size
        px = im.load()
        for y in range(h - CELL_H - 1, h):
            for x in range(0, w, 2):
                if px[x, y] == STATUS_BG:
                    return True
        return False

    def check(self, cond: bool, msg: str):
        print(f'  {"PASS" if cond else "FAIL"}: {msg}')
        if not cond:
            self.fails.append(msg)

    @property
    def input_text(self) -> str:
        """输入行去掉光标块（反白格 OCR 成全角实心块）后的文本。"""
        return self.input_row.replace('█', '').strip()


def main():
    build = build_dir()
    shots = Path(sys.argv[1]) if len(sys.argv) > 1 else build / 'test' / 'ui-shots'
    payload = build / 'test' / 'payload.img'
    kernel = build / 'kernel' / 'vmlinuz'
    initrd = build / 'initrd-ai.cpio.gz'
    font_bin = build / 'screen' / 'font.bin'
    qmp_sock = shots / 'qmp.sock'
    qemu_log = shots / 'qemu.log'
    boot_wait = int(os.environ.get('VTOY_UI_BOOT_WAIT', '120'))
    qemu_pid = mock_pid = None

    shutil.rmtree(shots, ignore_errors=True)
    shots.mkdir(parents=True, exist_ok=True)

    for f in (kernel, initrd):
        if not f.is_file():
            raise SystemExit(f'缺少 {f}（先跑 agent/build.sh + pack/pack_env.sh）')
    if not font_bin.is_file():
        raise SystemExit(f'缺少 {font_bin}（先跑 pack/pack_env.sh 生成点阵字体）')
    if not shutil.which('qemu-system-x86_64'):
        raise SystemExit('需要 qemu-system-x86_64')

    def cleanup(*_):
        for p in (qemu_pid, mock_pid):
            if p:
                try:
                    p.terminate()
                except OSError:
                    pass

    try:
        print('[ui-test] 造交互式 payload 盘（无 test_script.txt）…')
        subprocess.run(['sh', f'{SH}/make_test_disk.sh'], check=True,
                       env=dict(os.environ, VTOY_AI_WITH_SCRIPT='0'),
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

        # mock LLM：端口被占（上次遗留）就直接复用，不阻断 UI 断言
        with socket.socket() as probe:
            busy = probe.connect_ex(('127.0.0.1', 18800)) == 0
        if busy:
            print('[ui-test] 18800 已在监听，复用现有 mock')
        else:
            print('[ui-test] 启动 mock LLM (18800)…')
            mock_pid = subprocess.Popen(
                [sys.executable, str(AI_DIR / 'tools' / 'mock_openai_server.py'), '18800'],
                env=dict(os.environ, VTOY_MOCK_LOG=str(shots / 'requests.log')),
                stdout=(shots / 'mock.log').open('wb'), stderr=subprocess.STDOUT)
            time.sleep(1)

        print('[ui-test] 启动 QEMU（本地屏 console=tty0，QMP: %s）…' % qmp_sock)
        qemu = subprocess.Popen([
            'qemu-system-x86_64', *qemu_args(),
            '-kernel', str(kernel), '-initrd', str(initrd),
            '-append', 'rdinit=/init vtoy_ai=1 vtoy_ai_interactive=1 loglevel=3 console=tty0',
            '-drive', f'file={payload},format=raw,if=virtio',
            '-netdev', 'user,id=n0', '-device', 'virtio-net-pci,netdev=n0',
            '-vga', 'std', '-display', 'none', '-serial', 'none',
            '-monitor', 'none', '-no-reboot',
            '-qmp', f'unix:{qmp_sock},server,nowait',
        ], stdout=qemu_log.open('wb'), stderr=subprocess.STDOUT)
        qemu_pid = qemu

        qmp = Qmp(qmp_sock)
        ui = Ui(qmp, shots, Font(font_bin))

        print(f'[ui-test] 等待界面出现（上限 {boot_wait}s）…')
        probe = shots / '_probe.ppm'
        ready, t0 = False, time.time()
        while time.time() - t0 < boot_wait:
            if qemu.poll() is not None:
                raise SystemExit(f'QEMU 提前退出，见 {qemu_log}')
            qmp.shot(probe)
            if ui.has_status_bg(probe):
                ready = True
                break
            time.sleep(2)
        if not ready:
            ui.capture('00-timeout')
            raise SystemExit(f'超时：未检测到状态栏底色（检查 {qemu_log} 与 {shots}/00-timeout.png）')
        print(f'[ui-test] 界面就绪（{time.time() - t0:.1f}s）')

        # --- 1. AI 模式基线 ---
        qmp.wait_stable(shots / '_idle.ppm')
        ui.capture('01-ai-mode')
        ui.check(ui.input_row.startswith('你>'), 'AI 模式提示符应为「你> 」')
        ui.check('[AI]' in ui.status, '状态栏左侧应显示 [AI]')
        ui.check('Ctrl+Space' in ui.status, '状态栏应提示 Ctrl+Space 中/英')
        ui.check('Ctrl+T' in ui.status, '状态栏应提示 Ctrl+T 切换模式')

        # --- AI 历史留一条（mock 不可用时也只是报错，不影响历史记录） ---
        qmp.type('hello')
        qmp.key('ret')
        qmp.wait_stable(shots / '_idle.ppm')
        ui.capture('02-ai-turn', 'AI 一轮结束后输入行应复位')
        ui.check(ui.input_row.startswith('你>'), 'AI 回复后输入行应复位为「你> 」')
        # 回显必须左对齐：固定输入行有自己的光标列，不能把内容光标带偏
        ui.check(any(r.startswith('你> hello') for r in ui.content.split('\n')),
                 '回车回显应从内容区第 0 列开始（左对齐）')

        # --- 2. Ctrl+T 切命令模式（本地屏 Shift+Tab 与 Tab 同效，不可用）---
        qmp.key_with('ctrl', 't')
        qmp.wait_stable(shots / '_idle.ppm')
        ui.capture('03-cmd-mode')
        ui.check(ui.input_row.startswith('$'), 'Ctrl+T 后提示符应变为「$ 」')
        ui.check('[命令]' in ui.status, '状态栏左侧应变为 [命令]')

        # --- 3. 命令模式执行 ---
        for cmd in ('free', 'uname'):
            qmp.type(cmd)
            qmp.key('ret')
            qmp.wait_stable(shots / '_idle.ppm')
        ui.capture('04-cmd-run')
        ui.check('exit_code=0' in ui.content, '本地命令应正常执行（输出含 exit_code=0）')
        ui.check(ui.input_row.startswith('$'), '执行后输入行应仍是「$ 」')
        # 命令模式的回显同样必须左对齐
        ui.check(any(r.startswith('$ free') for r in ui.content.split('\n')),
                 '命令模式回显应从第 0 列开始（左对齐）')

        # --- 4. 大量输出：内容滚动，输入行/状态栏不动（固定输入行的核心断言）---
        qmp.type('ls /bin')
        qmp.key('ret')
        qmp.wait_stable(shots / '_idle.ppm')
        ui.capture('05-scroll')
        ui.check('zcip' in ui.content, '长输出尾部应可见（内容已滚动）')
        ui.check('AI 装机助手' not in ui.content, '横幅应已滚出屏幕')
        ui.check(ui.input_row.startswith('$'), '内容滚动不应冲掉输入行')
        ui.check('[命令]' in ui.status, '内容滚动不应冲掉状态栏')

        # --- 5. ↑↓ 历史 ---
        qmp.key('up')
        time.sleep(0.6)
        ui.capture('06-hist-up1')
        ui.check('ls /bin' in ui.input_row, '↑ 应回放上一条（ls /bin）')
        qmp.key('up')
        time.sleep(0.6)
        ui.capture('07-hist-up2')
        ui.check('uname' in ui.input_row, '再 ↑ 应回放更早一条（uname）')
        ui.check('ls /bin' not in ui.input_row, '历史回放不应残留上一条的像素')
        qmp.key('down')
        time.sleep(0.6)
        ui.capture('08-hist-down')
        ui.check('ls /bin' in ui.input_row, '↓ 应回到较新一条（ls /bin）')

        # --- 6. 历史项直接回车执行 ---
        qmp.key('ret')
        qmp.wait_stable(shots / '_idle.ppm')
        ui.capture('09-exec-history')
        ui.check(ui.input_row.startswith('$'), '回车执行后输入行应清空')
        ui.check('zcip' in ui.content, '历史项应被重新执行（输出再次出现）')

        # --- 7. find 放行（A+C 回归）---
        qmp.type('find /iso')
        qmp.key('ret')
        qmp.wait_stable(shots / '_idle.ppm')
        ui.capture('10-find')
        ui.check('不允许执行 shell 类命令' not in ui.content, 'find 不应被 shell 类命令拦截')
        ui.check('/iso/ventoy/ai.json' in ui.content, 'find /iso 应列出数据分区内容')

        # --- 8. Tab 补全：命令名 + 路径；多候选再按一次才列出 ---
        qmp.type('sysc')
        qmp.key('tab')
        time.sleep(0.6)
        ui.capture('10-complete-cmd')
        ui.check('sysctl' in ui.input_row, 'Tab 应把 sysc 补成命令名（sysctl）')
        qmp.key_with('ctrl', 'u')

        qmp.type('ls /i')
        qmp.key('tab')
        time.sleep(0.6)
        ui.capture('11-complete-prefix')
        ui.check(ui.input_text.endswith('ls /i'), '多候选（/init、/iso/）第一次 Tab 只补公共前缀')
        qmp.key('tab')
        time.sleep(0.6)
        ui.capture('12-complete-list')
        ui.check('/iso/' in ui.content and '/init' in ui.content, '再按一次 Tab 应把候选列到内容区')
        qmp.key_with('ctrl', 'u')

        # --- 9. cd：工作目录跟着变，ls 列出当前目录（含文件）---
        qmp.type('cd /iso')
        qmp.key('ret')
        qmp.wait_stable(shots / '_idle.ppm')
        ui.capture('13-cd')
        ui.check('/iso' in ui.status, 'cd 后状态栏应显示当前目录 /iso')
        qmp.type('ls ven')
        qmp.key('tab')
        time.sleep(0.6)
        ui.capture('14-complete-rel')
        ui.check('ls ventoy/' in ui.input_text, '相对路径补全应基于 cd 后的目录')
        qmp.key_with('ctrl', 'u')
        qmp.type('ven')          # 行首无同名命令 ⇒ 退回路径补全（bash 习惯）
        qmp.key('tab')
        time.sleep(0.6)
        ui.capture('14b-complete-fallback')
        ui.check('ventoy/' in ui.input_text, '行首无同名命令时应退回路径补全')
        qmp.key_with('ctrl', 'u')
        qmp.type('ls')
        qmp.key('ret')
        qmp.wait_stable(shots / '_idle.ppm')
        ui.capture('15-ls-cwd')
        ui.check('ubuntu-24.04-desktop-amd64.iso' in ui.content, 'cd 后 ls 应列出 /iso 下的文件（不只目录）')

        # --- 10. Ctrl+T 回 AI：历史互不串 ---
        qmp.key_with('ctrl', 't')
        qmp.wait_stable(shots / '_idle.ppm')
        ui.capture('16-back-ai')
        ui.check(ui.input_row.startswith('你>'), 'Ctrl+T 应切回 AI 模式提示符')
        ui.check('[AI]' in ui.status, '状态栏应切回 [AI]')
        qmp.key('up')
        time.sleep(0.6)
        ui.capture('17-ai-history')
        ui.check('hello' in ui.input_row, 'AI 模式 ↑ 应取 AI 侧历史（hello）')
        ui.check('ls /bin' not in ui.input_row, 'AI 历史不应串到命令历史')

        print(f'[ui-test] 产物: {shots}（截图 PNG/PPM、requests.log、mock.log、qemu.log）')
        if ui.fails:
            print(f'[ui-test] 失败 {len(ui.fails)} 项:', file=sys.stderr)
            for f in ui.fails:
                print('  - ' + f, file=sys.stderr)
            return 1
        print('[ui-test] 通过: 全部断言成立')
        return 0
    finally:
        cleanup()
        if qemu_pid is not None:
            try:
                qemu_pid.wait(timeout=5)
            except subprocess.TimeoutExpired:
                qemu_pid.kill()
        if mock_pid is not None:
            try:
                mock_pid.wait(timeout=5)
            except subprocess.TimeoutExpired:
                mock_pid.kill()


if __name__ == '__main__':
    raise SystemExit(main())
