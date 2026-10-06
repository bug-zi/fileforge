package processor

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func gradientJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), uint8((x + y) % 256), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodeCfg(t *testing.T, data []byte) image.Config {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("无法解码图片数据: %v", err)
	}
	return cfg
}

func TestProcessShrinksLargeImage(t *testing.T) {
	orig := gradientJPEG(t, 2560, 1920)

	res, err := Process(orig, DefaultQuality)
	if err != nil {
		t.Fatal(err)
	}

	if got := decodeCfg(t, res.OutBytes); got.Width != 1920 || got.Height != 1440 {
		t.Errorf("output size = %dx%d, want 1920x1440", got.Width, got.Height)
	}
	if len(res.OutBytes) >= len(orig) {
		t.Errorf("output %d bytes not smaller than original %d bytes", len(res.OutBytes), len(orig))
	}
	if ratio := float64(len(res.OutBytes)) / float64(len(orig)); ratio >= 0.6 {
		t.Errorf("shrink ratio = %.2f, want < 0.6", ratio)
	}
}

func TestProcessQualityAffectsSize(t *testing.T) {
	data := gradientJPEG(t, 1600, 1200)

	hi, err := Process(data, 90)
	if err != nil {
		t.Fatal(err)
	}
	lo, err := Process(data, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(lo.OutBytes) >= len(hi.OutBytes) {
		t.Errorf("quality 50 output (%d bytes) should be smaller than quality 90 (%d bytes)",
			len(lo.OutBytes), len(hi.OutBytes))
	}
}

func TestProcessConvertsPNGToJPEG(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 800, 600))
	for y := 0; y < 600; y++ {
		for x := 0; x < 800; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}

	res, err := Process(pngBuf.Bytes(), DefaultQuality)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.OutBytes) < 2 || res.OutBytes[0] != 0xFF || res.OutBytes[1] != 0xD8 {
		t.Error("output is not a JPEG stream (missing SOI marker)")
	}
	if res.Width != 800 || res.Height != 600 {
		t.Errorf("size = %dx%d, want 800x600 (small image unchanged)", res.Width, res.Height)
	}
}

func TestProcessKeepsSmallImageSize(t *testing.T) {
	orig := gradientJPEG(t, 1200, 900)

	res, err := Process(orig, DefaultQuality)
	if err != nil {
		t.Fatal(err)
	}
	cfg := decodeCfg(t, res.OutBytes)
	if cfg.Width != 1200 || cfg.Height != 900 {
		t.Errorf("size = %dx%d, want 1200x900 (small image must not upscale)", cfg.Width, cfg.Height)
	}
}

func TestMD5IdenticalForSameContent(t *testing.T) {
	data := gradientJPEG(t, 640, 480)

	r1, err := Process(data, DefaultQuality)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Process(append([]byte(nil), data...), DefaultQuality)
	if err != nil {
		t.Fatal(err)
	}
	if r1.MD5 == "" || r1.MD5 != r2.MD5 {
		t.Errorf("MD5 mismatch for identical content: %q vs %q", r1.MD5, r2.MD5)
	}
	if len(r1.MD5) != 32 {
		t.Errorf("MD5 length = %d, want 32 (hex)", len(r1.MD5))
	}
}

func TestProcessRejectsCorruptData(t *testing.T) {
	for name, data := range map[string][]byte{
		"plain text": []byte("this is not an image at all"),
		"truncated":  {0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'},
		"empty":      {},
	} {
		if _, err := Process(data, DefaultQuality); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestMD5HexMatchesResultMD5(t *testing.T) {
	data := gradientJPEG(t, 640, 480)
	res, err := Process(data, DefaultQuality)
	if err != nil {
		t.Fatal(err)
	}
	if got := MD5Hex(data); got != res.MD5 {
		t.Errorf("MD5Hex = %q, want %q (same digest as Process)", got, res.MD5)
	}
}

func TestOutNameFormat(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{1, "photo_001.jpg"},
		{42, "photo_042.jpg"},
		{999, "photo_999.jpg"},
		{1000, "photo_1000.jpg"},
	} {
		if got := OutName(tc.n); got != tc.want {
			t.Errorf("OutName(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
