package screen

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Canvas 是像素级绘制后端：/dev/fb0 与测试用内存画布都实现它。
type Canvas interface {
	Size() (w, h int)
	SetPixel(x, y int, rgb uint32)
	Fill(x, y, w, h int, rgb uint32)
	ScrollUp(px int)
	// ScrollUpRegion 把 [y0,y1) 像素行区间整体上移 px 行，区间底部补黑。
	ScrollUpRegion(y0, y1, px int)
}

const (
	fbioGetVScreenInfo = 0x4600 // struct fb_var_screeninfo
	fbioGetFScreenInfo = 0x4602 // struct fb_fix_screeninfo
)

// fb_var_screeninfo 字段偏移（x86_64，内核 UAPI 固定布局）
const (
	voXres  = 0
	voYres  = 4
	voBpp   = 24
	voROff  = 32
	voRLen  = 36
	voGOff  = 44
	voGLen  = 48
	voBOff  = 56
	voBLen  = 60
	voSizeS = 160
)

// fb_fix_screeninfo 中 line_length 的偏移
const (
	foLineLength = 48
	foSizeS      = 80
)

type fbCanvas struct {
	fd     int
	mem    []byte
	w, h   int
	stride int
	psz    int // 字节/像素
	rOff   uint32
	rLen   uint32
	gOff   uint32
	gLen   uint32
	bOff   uint32
	bLen   uint32
}

func u32at(b []byte, off int) uint32 {
	return uint32(b[off]) | uint32(b[off+1])<<8 | uint32(b[off+2])<<16 | uint32(b[off+3])<<24
}

// openFBCanvas 打开并映射帧缓冲设备（仅支持 16/32bpp）。
func openFBCanvas(path string) (*fbCanvas, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("打开 %s 失败: %w", path, err)
	}
	var vin [voSizeS]byte
	if err := ioctlPtr(fd, fbioGetVScreenInfo, unsafe.Pointer(&vin[0])); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("读取 %s 屏幕信息失败: %w", path, err)
	}
	var fin [foSizeS]byte
	if err := ioctlPtr(fd, fbioGetFScreenInfo, unsafe.Pointer(&fin[0])); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("读取 %s 固定信息失败: %w", path, err)
	}
	w := int(u32at(vin[:], voXres))
	h := int(u32at(vin[:], voYres))
	bpp := int(u32at(vin[:], voBpp))
	stride := int(u32at(fin[:], foLineLength))
	if stride == 0 {
		stride = w * bpp / 8
	}
	if w <= 0 || h <= 0 || (bpp != 16 && bpp != 32) {
		syscall.Close(fd)
		return nil, fmt.Errorf("不支持的帧缓冲格式: %dx%d %dbpp", w, h, bpp)
	}
	c := &fbCanvas{
		fd: fd, w: w, h: h, stride: stride, psz: bpp / 8,
		rOff: u32at(vin[:], voROff), rLen: u32at(vin[:], voRLen),
		gOff: u32at(vin[:], voGOff), gLen: u32at(vin[:], voGLen),
		bOff: u32at(vin[:], voBOff), bLen: u32at(vin[:], voBLen),
	}
	if c.rLen == 0 && c.gLen == 0 && c.bLen == 0 { // 个别驱动不给位域，按常规布局兜底
		if bpp == 32 {
			c.rOff, c.rLen, c.gOff, c.gLen, c.bOff, c.bLen = 16, 8, 8, 8, 0, 8
		} else {
			c.rOff, c.rLen, c.gOff, c.gLen, c.bOff, c.bLen = 11, 5, 5, 6, 0, 5
		}
	}
	mem, err := syscall.Mmap(fd, 0, stride*h, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("映射帧缓冲失败: %w", err)
	}
	c.mem = mem
	return c, nil
}

func (c *fbCanvas) close() {
	if c.mem != nil {
		syscall.Munmap(c.mem)
		c.mem = nil
	}
	if c.fd >= 0 {
		syscall.Close(c.fd)
		c.fd = -1
	}
}

func (c *fbCanvas) Size() (int, int) { return c.w, c.h }

// pack 把 0xRRGGBB 打包成帧缓冲像素（按 vinfo 位域）。
func (c *fbCanvas) pack(rgb uint32) uint32 {
	r := (rgb >> 16) & 0xFF
	g := (rgb >> 8) & 0xFF
	b := rgb & 0xFF
	scale := func(v, l uint32) uint32 {
		if l == 0 {
			return 0
		}
		if l > 8 {
			l = 8
		}
		return v >> (8 - l)
	}
	return scale(r, c.rLen)<<c.rOff | scale(g, c.gLen)<<c.gOff | scale(b, c.bLen)<<c.bOff
}

func (c *fbCanvas) SetPixel(x, y int, rgb uint32) {
	if x < 0 || y < 0 || x >= c.w || y >= c.h {
		return
	}
	v := c.pack(rgb)
	off := y*c.stride + x*c.psz
	switch c.psz {
	case 4:
		c.mem[off] = byte(v)
		c.mem[off+1] = byte(v >> 8)
		c.mem[off+2] = byte(v >> 16)
		c.mem[off+3] = byte(v >> 24)
	case 2:
		c.mem[off] = byte(v)
		c.mem[off+1] = byte(v >> 8)
	}
}

func (c *fbCanvas) Fill(x, y, w, h int, rgb uint32) {
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			c.SetPixel(xx, yy, rgb)
		}
	}
}

// ScrollUp 把画面整体上移 px 像素行，底部补黑。
func (c *fbCanvas) ScrollUp(px int) {
	if px <= 0 || px > c.h {
		return
	}
	n := copy(c.mem, c.mem[px*c.stride:])
	for i := n; i < len(c.mem); i++ {
		c.mem[i] = 0
	}
}

// ScrollUpRegion 只滚动 [y0,y1) 的像素行区间（内容区独立滚动用），区间底部补黑。
func (c *fbCanvas) ScrollUpRegion(y0, y1, px int) {
	if px <= 0 || y0 < 0 || y1 > c.h || y0+px >= y1 {
		return
	}
	top, mid, end := y0*c.stride, (y0+px)*c.stride, y1*c.stride
	n := copy(c.mem[top:end], c.mem[mid:end])
	for i := top + n; i < end; i++ {
		c.mem[i] = 0
	}
}

func ioctlPtr(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if e != 0 {
		return e
	}
	return nil
}

func ioctlUint(fd int, req uintptr, arg uintptr) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, arg)
	if e != 0 {
		return e
	}
	return nil
}
