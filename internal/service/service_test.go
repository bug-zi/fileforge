package service

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"mime/multipart"
	"os"
	"path/filepath"
	"regexp"
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

func TestCreateTaskIsReady(t *testing.T) {
	svc, outDir := newSvc(t)

	view, err := svc.CreateTask(headers(t, map[string][]byte{
		"a.jpg": gradientJPEG(t, 400, 300),
		"b.jpg": gradientJPEG(t, 500, 350),
	}))
	if err != nil {
		t.Fatal(err)
	}

	if view.Status != store.TaskReady {
		t.Errorf("status = %q, want %q (upload must not auto-start)", view.Status, store.TaskReady)
	}
	if view.DirName == "" {
		t.Fatal("DirName empty")
	}
	if matched, _ := regexp.MatchString(`^\d{6}-\d{4}$`, view.DirName); !matched {
		t.Errorf("DirName = %q, want YYMMDD-HHMM format", view.DirName)
	}

	got, _ := svc.Task(view.ID)
	if got.Done != 0 {
		t.Errorf("done = %d, want 0 (no processing before start)", got.Done)
	}

	// 输出目录已预留（同分钟防冲突），但没有任何结果文件
	entries, err := os.ReadDir(filepath.Join(outDir, view.DirName))
	if err != nil {
		t.Fatalf("output dir not reserved: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("output dir has %d entries, want 0 before start", len(entries))
	}
}

func TestDirNameCollisionSuffix(t *testing.T) {
	svc, _ := newSvc(t)

	v1, err := svc.CreateTask(headers(t, map[string][]byte{"a.jpg": gradientJPEG(t, 100, 80)}))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := svc.CreateTask(headers(t, map[string][]byte{"b.jpg": gradientJPEG(t, 100, 80)}))
	if err != nil {
		t.Fatal(err)
	}
	v3, err := svc.CreateTask(headers(t, map[string][]byte{"c.jpg": gradientJPEG(t, 100, 80)}))
	if err != nil {
		t.Fatal(err)
	}

	if v2.DirName != v1.DirName+"-2" {
		t.Errorf("second DirName = %q, want %q-2", v2.DirName, v1.DirName)
	}
	if v3.DirName != v1.DirName+"-3" {
		t.Errorf("third DirName = %q, want %q-3", v3.DirName, v1.DirName)
	}
}

func TestStartRunsBatch(t *testing.T) {
	svc, outDir := newSvc(t)

	imgA := gradientJPEG(t, 400, 300)
	imgB := gradientJPEG(t, 500, 350)
	view, err := svc.CreateTask(headers(t, map[string][]byte{
		"a.jpg":      imgA,
		"b.jpg":      imgB,
		"a-copy.jpg": imgA, // 与 a.jpg 内容相同 → 判重
		"broken.txt": []byte("not an image"),
	}))
	if err != nil {
		t.Fatal(err)
	}

	updated, err := svc.Reserve(view.ID, 4, 80)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != store.TaskProcessing {
		t.Errorf("status after Reserve = %q, want %q", updated.Status, store.TaskProcessing)
	}
	if updated.Concurrency != 4 || updated.Quality != 80 {
		t.Errorf("params = conc %d / quality %d, want 4 / 80", updated.Concurrency, updated.Quality)
	}
	if err := svc.RunByID(view.ID); err != nil {
		t.Fatal(err)
	}

	got, ok := svc.Task(view.ID)
	if !ok {
		t.Fatal("task missing after run")
	}
	if got.Status != store.TaskDone {
		t.Fatalf("status = %q, want %q", got.Status, store.TaskDone)
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
	for _, f := range got.Files {
		if f.Status == store.StatusFailed && f.Reason == "" {
			t.Errorf("failed file %q missing reason", f.OrigName)
		}
	}

	entries, err := os.ReadDir(filepath.Join(outDir, got.DirName))
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
		data, err := os.ReadFile(filepath.Join(outDir, got.DirName, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
			t.Errorf("output %q not decodable: %v", e.Name(), err)
		}
	}
}

func TestStartGuards(t *testing.T) {
	svc, _ := newSvc(t)

	if _, err := svc.Start("nope", 1, 80); err == nil {
		t.Error("start unknown task: expected error, got nil")
	}

	view, err := svc.CreateTask(headers(t, map[string][]byte{"a.jpg": gradientJPEG(t, 100, 80)}))
	if err != nil {
		t.Fatal(err)
	}
	// Start 是阻塞全流程（Reserve + Run）的便捷入口
	if _, err := svc.Start(view.ID, 1, 80); err != nil {
		t.Fatal(err)
	}
	if okHelper(svc, view.ID) != true {
		t.Fatal("task missing")
	}
	// 已开始/已完成后再 Start 必须被拒绝
	if _, err := svc.Start(view.ID, 2, 90); err == nil {
		t.Error("second Start: expected error, got nil")
	}
}

func okHelper(svc *Service, id string) bool {
	_, ok := svc.Task(id)
	return ok
}

func TestEstimate(t *testing.T) {
	svc, _ := newSvc(t)

	imgA := gradientJPEG(t, 800, 600)
	imgB := gradientJPEG(t, 1000, 700)
	view, err := svc.CreateTask(headers(t, map[string][]byte{
		"a.jpg":      imgA,
		"b.jpg":      imgB,
		"broken.txt": []byte("not an image"),
	}))
	if err != nil {
		t.Fatal(err)
	}

	files, total, err := svc.Estimate(view.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("estimate files = %d, want 3", len(files))
	}

	byName := map[string]FileEstimate{}
	for _, f := range files {
		byName[f.Name] = f
	}
	a := byName["a.jpg"]
	if a.Err != "" || a.Est <= 0 || a.Est >= int64(len(imgA)) {
		t.Errorf("a.jpg estimate = %+v, want 0 < est < %d", a, len(imgA))
	}
	b := byName["b.jpg"]
	if b.Err != "" || b.Est <= 0 || b.Est >= int64(len(imgB)) {
		t.Errorf("b.jpg estimate = %+v, want 0 < est < %d", b, len(imgB))
	}
	if byName["broken.txt"].Err == "" {
		t.Error("broken.txt estimate missing error note")
	}
	if total != a.Est+b.Est {
		t.Errorf("total = %d, want sum of successful estimates %d", total, a.Est+b.Est)
	}

	// 质量越低预估越小
	files90, _, err := svc.Estimate(view.ID, 90)
	if err != nil {
		t.Fatal(err)
	}
	var estA50, estA90 int64
	for _, f := range files {
		if f.Name == "a.jpg" {
			estA50 = f.Est
		}
	}
	for _, f := range files90 {
		if f.Name == "a.jpg" {
			estA90 = f.Est
		}
	}
	if estA50 >= estA90 {
		t.Errorf("q50 estimate %d should be smaller than q90 %d", estA50, estA90)
	}

	// 开始之后 Estimate 必须拒绝
	if _, err := svc.Start(view.ID, 1, 80); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Estimate(view.ID, 80); err == nil {
		t.Error("estimate after start: expected error, got nil")
	}
}

func TestEstimateUnknownTask(t *testing.T) {
	svc, _ := newSvc(t)
	if _, _, err := svc.Estimate("nope", 80); err == nil {
		t.Error("estimate unknown task: expected error, got nil")
	}
}
