package screen

import (
	"fmt"
	"os"
)

// memCanvas 是测试用内存画布（0x00RRGGBB 每像素 4 字节）。
type memCanvas struct {
	w, h   int
	stride int
	buf    []byte
}

func newMemCanvas(w, h int) *memCanvas {
	return &memCanvas{w: w, h: h, stride: w * 4, buf: make([]byte, w*h*4)}
}

func (c *memCanvas) Size() (int, int) { return c.w, c.h }

func (c *memCanvas) SetPixel(x, y int, rgb uint32) {
	if x < 0 || y < 0 || x >= c.w || y >= c.h {
		return
	}
	off := y*c.stride + x*4
	c.buf[off] = byte(rgb >> 16)
	c.buf[off+1] = byte(rgb >> 8)
	c.buf[off+2] = byte(rgb)
	c.buf[off+3] = 0xFF
}

func (c *memCanvas) Fill(x, y, w, h int, rgb uint32) {
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			c.SetPixel(xx, yy, rgb)
		}
	}
}

func (c *memCanvas) ScrollUp(px int) {
	if px <= 0 || px > c.h {
		return
	}
	n := copy(c.buf, c.buf[px*c.stride:])
	for i := n; i < len(c.buf); i++ {
		c.buf[i] = 0
	}
}

func (c *memCanvas) ScrollUpRegion(y0, y1, px int) {
	if px <= 0 || y0 < 0 || y1 > c.h || y0+px >= y1 {
		return
	}
	top, mid, end := y0*c.stride, (y0+px)*c.stride, y1*c.stride
	n := copy(c.buf[top:end], c.buf[mid:end])
	for i := top + n; i < end; i++ {
		c.buf[i] = 0
	}
}

// Pixel 返回像素颜色（0xRRGGBB），供测试断言。
func (c *memCanvas) Pixel(x, y int) uint32 {
	off := y*c.stride + x*4
	return uint32(c.buf[off])<<16 | uint32(c.buf[off+1])<<8 | uint32(c.buf[off+2])
}

// WritePPM 把画面写成 PPM（P6）文件，便于人工查看渲染结果。
func (c *memCanvas) WritePPM(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "P6\n%d %d\n255\n", c.w, c.h); err != nil {
		return err
	}
	row := make([]byte, c.w*3)
	for y := 0; y < c.h; y++ {
		for x := 0; x < c.w; x++ {
			p := c.Pixel(x, y)
			row[x*3] = byte(p >> 16)
			row[x*3+1] = byte(p >> 8)
			row[x*3+2] = byte(p)
		}
		if _, err := f.Write(row); err != nil {
			return err
		}
	}
	return nil
}
