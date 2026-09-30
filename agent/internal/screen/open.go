package screen

import (
	"fmt"
	"os"
	"strings"
)

// 内核 VT ioctl（linux/vt.h）
const (
	kdText     = 0
	kdGraphics = 1
	kdSetMode  = 0x4B3A // KDSETMODE
)

// Open 尝试接管本地显示器进行自绘。返回 (nil, nil) 表示继续用原文本控制台
// （串口输出、无帧缓冲或显式禁用），调用方无需处理错误。
//
// 接管条件（全部满足）：
//   - 运行在 AI initramfs 内（VTOY_AI_INITRAMFS=1，由 init/init 设置）；
//   - /dev/console 绑定到本地显示器（最后一个 console= 是 tty0）；
//   - /dev/fb0 可用且分辨率足够；
//   - tty0 能切到 KD_GRAPHICS（fbcon 停止绘制，屏幕交给本进程）。
//
// 环境变量 VTOY_AI_SCREEN=fb 强制启用（失败返回错误），=tty 强制禁用。
func Open() (*Screen, error) {
	mode := os.Getenv("VTOY_AI_SCREEN")
	if mode == "tty" || mode == "0" {
		return nil, nil
	}
	forced := mode == "fb"
	if !forced {
		if os.Getenv("VTOY_AI_INITRAMFS") != "1" || !consoleIsDisplay() {
			return nil, nil
		}
	}

	font, err := LoadFont()
	if err != nil {
		return nil, err // 字体文件缺失/损坏属于打包问题；调用方会降级为文本控制台
	}
	cv, err := openFBCanvas("/dev/fb0")
	if err != nil {
		if forced {
			return nil, err
		}
		return nil, nil
	}
	if w, h := cv.Size(); w < 480 || h < 240 {
		cv.close()
		if forced {
			return nil, fmt.Errorf("帧缓冲分辨率过小: %dx%d", w, h)
		}
		return nil, nil
	}

	tty, err := os.OpenFile("/dev/tty0", os.O_RDWR, 0)
	if err != nil {
		cv.close()
		if forced {
			return nil, err
		}
		return nil, nil
	}
	if err := ioctlUint(int(tty.Fd()), kdSetMode, kdGraphics); err != nil {
		tty.Close()
		cv.close()
		if forced {
			return nil, fmt.Errorf("tty0 切换图形模式失败: %w", err)
		}
		return nil, nil
	}
	orig, err := makeRaw(int(tty.Fd()))
	if err != nil {
		_ = ioctlUint(int(tty.Fd()), kdSetMode, kdText)
		tty.Close()
		cv.close()
		return nil, err
	}

	s := newScreen(cv, font)
	s.SetStatusRows(statusRows) // 底部留两行：固定输入行 + 状态栏（模式/快捷键提示）
	s.in = tty
	s.tty = tty
	s.orig = orig
	s.startKeyReader() // 后台读键，使流式输出期间也能翻页
	return s, nil
}

// Close 恢复文本模式与终端设置（幂等）。成功退出时可保留画面不清屏，
// 由调用方决定是否调用。
func (s *Screen) Close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
	if s.ime != nil {
		s.ime.Close()
		s.ime = nil
	}
	if s.tty != nil {
		restoreTermios(int(s.tty.Fd()), s.orig)
		_ = ioctlUint(int(s.tty.Fd()), kdSetMode, kdText)
		s.tty.Close()
		s.tty = nil
	}
	if fb, ok := s.cv.(*fbCanvas); ok {
		fb.close()
	}
}

// consoleIsDisplay 判断 /dev/console 是否绑定到本地显示器（tty0）：
// 优先读 /proc/consoles 中带 C（CON_CONSDEV，即 /dev/console 目标）标记的项，
// 退化到 /sys/class/tty/console/active 的最后一项。
func consoleIsDisplay() bool {
	if data, err := os.ReadFile("/proc/consoles"); err == nil {
		for _, ln := range strings.Split(string(data), "\n") {
			i, j := strings.IndexByte(ln, '('), strings.IndexByte(ln, ')')
			if i < 0 || j <= i || !strings.Contains(ln[i:j], "C") {
				continue
			}
			return strings.Fields(ln)[0] == "tty0"
		}
	}
	if data, err := os.ReadFile("/sys/class/tty/console/active"); err == nil {
		parts := strings.Fields(string(data))
		if len(parts) > 0 {
			return parts[len(parts)-1] == "tty0"
		}
	}
	return false
}
