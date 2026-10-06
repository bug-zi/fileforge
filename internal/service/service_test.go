package service

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fileforge/internal/store"
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

func headers(t *testing.T, files map[string][]byte) []*multipart.FileHeader {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, data := range files {
		fw, err := mw.CreateFormFile("files[]", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	form, err := multipart.NewReader(&buf, mw.Boundary()).ReadForm(32 << 20)
	if err != nil {
		t.Fatal(err)
	}
	return form.File["files[]"]
}

func newSvc(t *testing.T) (*Service, string) {
	t.Helper()
	uploads := t.TempDir()
	outputs := t.TempDir()
	return New(store.New(), uploads, outputs), outputs
}

func TestRunBatchEndToEnd(t *testing.T) {
	svc, outDir := newSvc(t)

	imgA := gradientJPEG(t, 400, 300)
	imgB := gradientJPEG(t, 500, 350)
	view, err := svc.CreateTask(headers(t, map[string][]byte{
		"a.jpg":      imgA,
		"b.jpg":      imgB,
		"a-copy.jpg": imgA, // 与 a.jpg 内容相同 → 判重
		"broken.txt": []byte("not an image"),
	}), 4, 80)
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.RunByID(view.ID); err != nil {
		t.Fatal(err)
	}

	got, ok := svc.Task(view.ID)
	if !ok {
		t.Fatal("task missing after run")
	}
	if got.Status != store.TaskDone {
		t.Errorf("status = %q, want %q", got.Status, store.TaskDone)
	}
	if got.Done != 4 {
		t.Errorf("done = %d, want 4", got.Done)
	}
	if got.Elapsed <= 0 {
		t.Errorf("elapsed = %v, want > 0", got.Elapsed)
	}

	counts := map[string]int{}
	for _, f := range got.Files {
		counts[f.Status]++
	}
	if counts[store.StatusDone] != 2 {
		t.Errorf("done count = %d, want 2; files: %+v", counts[store.StatusDone], got.Files)
	}
	if counts[store.StatusDuplicate] != 1 {
		t.Errorf("duplicate count = %d, want 1", counts[store.StatusDuplicate])
	}
	if counts[store.StatusFailed] != 1 {
		t.Errorf("failed count = %d, want 1", counts[store.StatusFailed])
	}

	// 失败文件要带原因
	for _, f := range got.Files {
		if f.Status == store.StatusFailed && f.Reason == "" {
			t.Errorf("failed file %q missing reason", f.OrigName)
		}
	}

	// outputs 恰 2 个文件，photo_NNN.jpg 命名且可解码
	entries, err := os.ReadDir(filepath.Join(outDir, view.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("outputs has %d files, want 2", len(entries))
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "photo_") || !strings.HasSuffix(e.Name(), ".jpg") {
			t.Errorf("output name %q not photo_NNN.jpg format", e.Name())
		}
		data, err := os.ReadFile(filepath.Join(outDir, view.ID, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
			t.Errorf("output %q not decodable: %v", e.Name(), err)
		}
	}
}

func TestRunByIDUnknownTask(t *testing.T) {
	svc, _ := newSvc(t)
	if err := svc.RunByID("nope"); err == nil {
		t.Error("expected error for unknown task, got nil")
	}
}
