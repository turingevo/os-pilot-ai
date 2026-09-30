#!/usr/bin/env python3
"""本地 mock OpenAI 兼容服务，用于离线端到端测试 os-pilot-ai。

按固定剧本响应 /v1/chat/completions（stream=true 时走 SSE 流式，否则整包 JSON）：
  用户第 1 轮 -> system_probe -> 文本回复（列出镜像）
  用户第 2 轮 -> system_probe(自动) -> schedule_boot -> 文本回复（配置完成）
  用户第 3 轮 -> ask_user -> 文本回复
  用户第 4 轮 -> run_command -> 文本回复

环境变量:
  VTOY_MOCK_CMD       第 4 轮要执行的命令（默认 "blkid"，可写成 "cat /proc/cmdline" 这类带参数的形式）
  VTOY_MOCK_SCENARIO  设为 disk 时走“磁盘工具”剧本：按顺序驱动
                      list_disks → partition(/dev/vdb) → format(/dev/vdb1) → mkdir → mount
                      → mkdir → backup → umount → list_disks → gen_autoinstall → fs_read(回读模板)，
                      用于端到端验证受守卫工具与自动安装模板的生成
  VTOY_MOCK_LOG       把每个请求体追加写入该文件（端到端测试核对 guest 发送内容用）
"""

import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

EXTRACT_IMAGE = "/ubuntu-24.04-desktop-amd64.iso"
EXTRACT_TEMPLATE = "/scripts/ubuntu.seed"

# VTOY_MOCK_LOG=<文件> 时把每个请求体追加写入该文件（端到端测试核对 guest 发送内容用）
LOG_BODIES = os.environ.get("VTOY_MOCK_LOG")
SCENARIO = os.environ.get("VTOY_MOCK_SCENARIO", "")

# 磁盘工具剧本：靶盘固定 /dev/vdb（verify_disk_tools.sh 挂的第二块 virtio 盘）。
# 每收到一条 tool 结果就下发下一条，最后回文字收尾。
DISK_PLAN = [
    ("list_disks", {}),
    # 先故意对承载 payload 的 vda 动手：两处都必须被硬守卫拒绝（预期报错）
    ("format", {"device": "/dev/vda", "fstype": "ext4"}),
    ("partition", {"disk": "/dev/vda", "partitions": [{"size": "rest"}]}),
    ("partition", {
        "disk": "/dev/vdb",
        "table": "gpt",
        "partitions": [
            {"size": "200MiB", "fs": "ext4", "name": "backup"},
            {"size": "rest"},
        ],
    }),
    ("format", {"device": "/dev/vdb1", "fstype": "ext4", "label": "BK"}),
    ("run_command", {"cmd": "mkdir", "args": ["-p", "/mnt/bk"]}),
    ("run_command", {"cmd": "mount", "args": ["/dev/vdb1", "/mnt/bk"]}),
    ("run_command", {"cmd": "mkdir", "args": ["-p", "/mnt/bk/ventoy-backup"]}),
    ("backup", {"dst": "/mnt/bk/ventoy-backup"}),
    ("run_command", {"cmd": "umount", "args": ["/mnt/bk"]}),
    ("list_disks", {}),
    # 生成 autoinstall 模板：必须按磁盘序列号选盘、挂载用卷标（不硬编码 /dev/vdX）。
    # 靶盘由 verify_disk_tools.sh 以 serial=aitarget 挂载；分区大小要放得下 256M 靶盘。
    ("gen_autoinstall", {
        "image": "/ubuntu-24.04-desktop-amd64.iso",
        "disk": "/dev/vdb",
        "output": "autoinstall-e2e.yaml",
        "partitions": [
            {"size": "64M", "label": "boot-efi", "mount": "/boot/efi", "boot": True},
            {"size": "rest", "label": "root", "mount": "/"},
        ],
    }),
    # 回读模板文件：证明它真写进了数据分区（fs_read 返回原文，便于断言内容）
    ("fs_read", {"path": "autoinstall-e2e.yaml"}),
]

_call_seq = 0


def _tool_call(name, arguments):
    global _call_seq
    _call_seq += 1
    return {
        "id": "call_%d" % _call_seq,
        "type": "function",
        "function": {"name": name, "arguments": json.dumps(arguments, ensure_ascii=False)},
    }


def _text(content):
    return {"role": "assistant", "content": content}


def decide(messages):
    # 磁盘工具剧本：已收到的 tool 结果条数即剧本进度
    if SCENARIO == "disk":
        n = len([m for m in messages if m.get("role") == "tool"])
        if n < len(DISK_PLAN):
            name, args = DISK_PLAN[n]
            return {"role": "assistant", "content": "", "tool_calls": [_tool_call(name, args)]}
        return _text(
            "磁盘准备工作完成：/dev/vdb 已建 GPT 并分出 /dev/vdb1，已格式化为 ext4（卷标 BK），"
            "数据分区内容已 rsync 到 /mnt/bk/ventoy-backup，最后已卸载；"
            "并已按磁盘序列号生成 autoinstall 模板 autoinstall-e2e.yaml（挂载用卷标，不含设备名）。"
        )

    users = [
        m for m in messages
        if m.get("role") == "user" and not m.get("content", "").startswith("【系统探测结果")
    ]
    tools = [m for m in messages if m.get("role") == "tool"]
    n_user = len(users)

    if not tools:
        return {"role": "assistant", "content": "", "tool_calls": [_tool_call("system_probe", {})]}
    last = tools[-1].get("name")

    if n_user <= 1:
        return _text(
            "U 盘里检测到这些镜像：ubuntu-24.04-desktop-amd64.iso、debian-12-netinst-amd64.iso。"
            "需要我给哪个配置自动安装？"
        )
    if n_user == 2:
        if last == "schedule_boot":
            return _text("配置已写入并备份，重启后将自动开始无人值守安装。")
        return {
            "role": "assistant",
            "content": "",
            "tool_calls": [_tool_call(
                "schedule_boot",
                {"image": EXTRACT_IMAGE, "template": EXTRACT_TEMPLATE, "autosel": 1, "timeout": -1},
            )],
        }
    if n_user == 3:
        if last == "ask_user":
            return _text("好的，按默认选项继续。")
        return {
            "role": "assistant",
            "content": "",
            "tool_calls": [_tool_call(
                "ask_user",
                {"question": "是否现在写入配置并重启？", "options": ["是", "否"]},
            )],
        }
    if n_user == 4:
        if last == "run_command":
            return _text("命令输出正常，环境检查完毕，可以开始安装了。")
        parts = os.environ.get("VTOY_MOCK_CMD", "blkid").split()
        return {
            "role": "assistant",
            "content": "",
            "tool_calls": [_tool_call("run_command", {"cmd": parts[0], "args": parts[1:]})],
        }
    return _text("好的。")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _send_sse(self, model, msg):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True

        def emit(delta, finish=None):
            obj = {
                "id": "chatcmpl-mock",
                "object": "chat.completion.chunk",
                "model": model,
                "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
            }
            data = json.dumps(obj, ensure_ascii=False).encode()
            self.wfile.write(b"data: " + data + b"\n\n")
            self.wfile.flush()

        def emit_text(field, text, step=12):
            for i in range(0, len(text), step):
                emit({field: text[i:i + step]})

        emit({"role": "assistant", "content": ""})
        tcs = msg.get("tool_calls")
        if tcs:
            tc = tcs[0]
            fn = tc["function"]
            emit({"tool_calls": [{"index": 0, "id": tc["id"], "type": "function",
                                  "function": {"name": fn["name"], "arguments": ""}}]})
            emit_text_arg = fn["arguments"]
            for i in range(0, len(emit_text_arg), 12):
                emit({"tool_calls": [{"index": 0, "function": {"arguments": emit_text_arg[i:i + 12]}}]})
        else:
            content = msg.get("content", "")
            if content:
                emit_text("reasoning_content", "（mock 推理：按轮次选择动作）")
                emit_text("content", content)
        emit({}, finish="stop")
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length)
        if LOG_BODIES:
            # 端到端测试用：把请求体落盘，便于核对 guest 实际发送的内容
            with open(LOG_BODIES, "ab") as f:
                f.write(raw + b"\n")
        try:
            req = json.loads(raw)
        except json.JSONDecodeError:
            self.send_error(400, "bad json")
            return

        msgs = req.get("messages", [])
        has_key = "Authorization" in self.headers
        n_tools = len(req.get("tools", []))
        stream = bool(req.get("stream"))
        print("[mock] model=%s messages=%d tools=%d auth=%s stream=%s"
              % (req.get("model"), len(msgs), n_tools, has_key, stream), flush=True)

        model = req.get("model", "mock")
        msg = decide(msgs)
        if stream:
            self._send_sse(model, msg)
            return

        body = json.dumps({
            "id": "chatcmpl-mock",
            "object": "chat.completion",
            "model": model,
            "choices": [{"index": 0, "message": msg, "finish_reason": "stop"}],
        }, ensure_ascii=False).encode()

        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path in ("/health", "/v1/models"):
            body = b'{"object":"list","data":[{"id":"mock"}]}'
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_error(404)

    def log_message(self, fmt, *args):
        print("[mock] " + fmt % args, flush=True)


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8000
    print("[mock] listening on 0.0.0.0:%d" % port, flush=True)
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
