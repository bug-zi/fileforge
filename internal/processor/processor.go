package processor

import (
	"bytes"        // 提供`Buffer` 和`NewReader` ，把`[]byte` 当成`io.Reader` /`io.Writer` 用
	"crypto/md5"   // MD5 哈希算法
	"encoding/hex" // 提供 Base16 编码解码函数
	"fmt"          // 提供 fmt.Errorf 等格式化输出函数
	"image"        // 图片编解码的核心包，定义了`image.Image` 接口、`Decode` 函数
	"image/jpeg"   // 提供 JPEG 编解码器
	_ "image/png"  // 提供 PNG 编解码器（空导入：导入这个包，不是为了用它里面的函数，只是为了让它自动跑一遍初始化。）；Go 语言要求导入的包必须被使用，否则编译报错。但有些包的价值不在“被调用”，而在“被加载”

	"golang.org/x/image/draw" // 全项目唯一的第三方依赖：提供 CatmullRom 高质量插值缩放。
)

const (
	MaxEdge        = 1920 // 图片最长边上限，超过就缩放。
	DefaultQuality = 80   // JPEG 默认质量
)

// Result 是单文件处理结果：MD5 为原始输入字节的摘要（判重依据），
// OutBytes 为重编码后的 JPEG 字节流。
type Result struct {
	MD5      string // MD5（原始文件的哈希，用于判重）
	OutBytes []byte // 压缩后的 JPEG 字节
	Width    int    // 压缩后的 JPEG 图片宽度
	Height   int    // 压缩后的 JPEG 图片高度
}

// Process 是纯函数：解码 → 最长边超限则等比缩放 → 指定质量 JPEG 重编码。
// 解码失败（非图片 / 损坏文件）返回错误，由调用方做单文件隔离。
func Process(data []byte, quality int) (*Result, error) {
	// 分层校验：handler 层已钳位过 quality，但最底层仍不信任输入，做 1~100 兜底。
	// 这是「纵深防御」思想——每一层都假设上层可能出错。
	if quality < 1 {
		quality = 1
	}
	if quality > 100 {
		quality = 100
	}

	sum := MD5Hex(data) // 计算原始文件的 MD5 哈希，用于判重。

	src, _, err := image.Decode(bytes.NewReader(data)) // 解码图片： bytes.NewReader 把 []byte 转成 io.Reader，再解码 image.Reader。
	if err != nil {
		return nil, fmt.Errorf("无法解码图片: %w", err) // 解码失败，返回错误。
	}

	dst := resizeIfNeeded(src) // 调用内部函数`resizeIfNeeded` ，如果最长边超过 1920 就等比缩放。

	var out bytes.Buffer // 缓存压缩后的 JPEG 字节流。
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("JPEG 编码失败: %w", err)
	}

	b := dst.Bounds() // 获取压缩后的 JPEG 图片的边界框。
	return &Result{
		MD5:      sum,
		OutBytes: out.Bytes(),
		Width:    b.Dx(),
		Height:   b.Dy(),
	}, nil
}

// MD5Hex 返回输入字节的 MD5 hex，供 worker 在压缩前判重（重复文件直接跳过压缩）。
func MD5Hex(data []byte) string {
	sum := md5.Sum(data)              // md5.Sum 返回定长数组 [16]byte（不是 slice）；
	return hex.EncodeToString(sum[:]) // hex.EncodeToString 需要 []byte，所以用 sum[:] 把数组转成切片。
}

func resizeIfNeeded(src image.Image) image.Image {
	b := src.Bounds() // 获取原始图片的边界框。
	w, h := b.Dx(), b.Dy()
	if w <= MaxEdge && h <= MaxEdge {
		return src // 最长边不超过上限，直接返回原始图片。
	}

	// 等比缩放：把较长边压到 MaxEdge，另一边按比例缩小。
	var nw, nh int
	if w >= h {
		nw = MaxEdge
		nh = int(float64(h) * float64(MaxEdge) / float64(w))
	} else {
		nh = MaxEdge
		nw = int(float64(w) * float64(MaxEdge) / float64(h))
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))                   // 创建新的 RGBA 图片，大小为 nw * nh。
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil) // 缩放原始图片到新的 RGBA 图片。
	return dst
}

// OutName 生成统一重命名，n 从 1 起：photo_001.jpg。
func OutName(n int) string {
	return fmt.Sprintf("photo_%03d.jpg", n)
}
