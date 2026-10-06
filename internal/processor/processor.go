package processor

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"

	"golang.org/x/image/draw"
)

const (
	MaxEdge = 1920
	// DefaultQuality 是缺省 JPEG 编码质量（design v0.1 的定版数值）；
	// v0.2 起任务可携带自定义质量（handler 钳位 50~95），processor 只做 1~100 兜底。
	DefaultQuality = 80
)

// Result 是单文件处理结果：MD5 为原始输入字节的摘要（判重依据），
// OutBytes 为重编码后的 JPEG 字节流。
type Result struct {
	MD5      string
	OutBytes []byte
	Width    int
	Height   int
}

// Process 是纯函数：解码 → 最长边超限则等比缩放 → 指定质量 JPEG 重编码。
// 解码失败（非图片 / 损坏文件）返回错误，由调用方做单文件隔离。
func Process(data []byte, quality int) (*Result, error) {
	if quality < 1 {
		quality = 1
	}
	if quality > 100 {
		quality = 100
	}
	sum := MD5Hex(data)

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("无法解码图片: %w", err)
	}

	dst := resizeIfNeeded(src)

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("JPEG 编码失败: %w", err)
	}

	b := dst.Bounds()
	return &Result{
		MD5:      sum,
		OutBytes: out.Bytes(),
		Width:    b.Dx(),
		Height:   b.Dy(),
	}, nil
}

// MD5Hex 返回输入字节的 MD5 hex，供 worker 在压缩前判重（重复文件直接跳过压缩）。
func MD5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

func resizeIfNeeded(src image.Image) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= MaxEdge && h <= MaxEdge {
		return src
	}

	var nw, nh int
	if w >= h {
		nw = MaxEdge
		nh = int(float64(h) * float64(MaxEdge) / float64(w))
	} else {
		nh = MaxEdge
		nw = int(float64(w) * float64(MaxEdge) / float64(h))
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}

// OutName 生成统一重命名，n 从 1 起：photo_001.jpg。
func OutName(n int) string {
	return fmt.Sprintf("photo_%03d.jpg", n)
}
